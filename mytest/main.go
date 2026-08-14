// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waBinary "go.mau.fi/whatsmeow/binary"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"github.com/gabriel-vasile/mimetype"

	"C"
)

var cli *whatsmeow.Client
var log waLog.Logger

var logLevel = "INFO"
var debugLogs = flag.Bool("debug", false, "Enable debug logs?")
var dbDialect = flag.String("db-dialect", "sqlite3", "Database dialect (sqlite3 or postgres)")
var dbAddress = flag.String("db-address", "file:mdtest.db?_foreign_keys=on", "Database address")
var requestFullSync = flag.Bool("request-full-sync", false, "Request full (1 year) history sync when logging in?")

// socketPath: Unix domain socket 路径，由 Java Server 通过命令行参数传入。
// Java Server 在 Windows 与 Linux 上统一传入 --socket（Windows 测试 = Linux 生产同一套逻辑）：
// Go 进入 daemon 模式，命令从 socket 接收、ProtoOutput 事件写回 socket，Java 重启时 Go 不受
// 影响、可被重新 attach。留空则回退到 stdin/stdout 管道模式（仅手动终端调试用，Java 不再走该路径）。
var socketPath = flag.String("socket", "", "Unix domain socket path for Java IPC (daemon mode). Empty = legacy stdin/stdout mode (manual debugging only).")

// serverUrl: Java Server 的 HTTP 基地址（http://host:port），用于请求 view-once 上传的 S3 预签名 PUT URL。
// 由 Java Server 启动时通过 --server-url 传入；为空时 view-once 上传不可用（上传前会报错）。
var serverUrl = flag.String("server-url", "", "Java Server HTTP base URL for presigned S3 upload. Empty = presign upload disabled.")

// daemonMode: 是否处于 daemon 模式（socketPath 非空）。启动早期根据 flag 设置。
var daemonMode = false
var pairRejectChan = make(chan bool, 1)

var enableViewOnce = false

var device *store.Device
var lid = ""
var presenceMgr *PresenceManager

// presenceBufferMax: presence 事件缓冲上限（条）。Java 掉线期间的事件存于此，
// Java 重连后按序重放；超限丢最旧，latest 兜底保证每 JID 至少拿到当前状态。
const presenceBufferMax = 1000

// attachHandshakeTimeout: 连接用途握手超时。Java 完整 attach 连接首行发 "attach"，
// Go 才设 ProtoOutput 后端并重放缓冲；命令/探测连接（sendCommand、health checker
// probe）不设后端、不重放，避免提前消耗 PresenceCache 缓冲导致事件丢失。
// 命令连接首行即命令、探测连接不发任何内容即关闭，均远快于 10s，超时仅作兜底。
const attachHandshakeTimeout = 10 * time.Second

// presenceCache: Java 掉线期间的上下线通知缓存，重连时重放。
var presenceCache = NewPresenceCache(presenceBufferMax)

func main() {
	// 内存软上限：限制 Go 堆 + 运行时内存（不含跨进程共享的代码段），防止 view-once 下载/
	// 历史同步等瞬时大分配把整机内存打穿。软上限（可临时小幅超出），过低会引发 GC 频繁，
	// 64MB 对 view-once 媒体（下载 + 上传缓冲）足够。可在多个 Go 进程并行下载时限制节点峰值。
	debug.SetMemoryLimit(64 << 20)

	waBinary.IndentXML = true
	flag.Parse()

	// log 提前初始化：daemon 模式下 startCommandSocket 的 log.Infof 依赖 log 已就绪
	// （socket 前置到 cli.Connect 之前，不能等 log 在后面才初始化，否则 nil logger panic）
	if *debugLogs {
		logLevel = "DEBUG"
	}
	log = waLog.Stdout("Main", logLevel, true)

	// daemon 模式判定：传了 --socket 即进入 daemon 模式
	daemonMode = *socketPath != ""
	// 提前声明，供 daemon 模式 socket 前置启动与主循环共用（非 daemon 模式在后面初始化）
	var c chan os.Signal
	var input chan string
	if daemonMode {
		// daemon 化（Linux: setsid 脱离 JVM 进程组 + 忽略 SIGPIPE；非 Linux: no-op）
		// 配合 systemd KillMode=process 让 Go 在 Java 重启时存活
		daemonize()
		// ProtoOutput 事件从 stdout 改写 Unix socket（Java 重启时 stdout pipe 会断，必须迁通道）
		setDaemonBackend()
		// 【修复】socket 创建前置：在 cli.Connect()（首次连接 WhatsApp 可能数十秒）之前
		// 尽早监听 Unix socket，避免 Java 端 8 秒 attach 等待窗口被 WhatsApp 连接占满而误判
		// 启动失败。accept 循环在 goroutine 内不阻塞主流程；命令经无缓冲 input channel
		// 暂存，主循环启动后消费，不丢失。
		input = make(chan string)
		c = make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		startCommandSocket(input, *socketPath)
	}

	if *requestFullSync {
		store.DeviceProps.RequireFullSync = proto.Bool(true)
		store.DeviceProps.HistorySyncConfig = &waProto.DeviceProps_HistorySyncConfig{
			FullSyncDaysLimit:   proto.Uint32(3650),
			FullSyncSizeMbLimit: proto.Uint32(102400),
			StorageQuotaMb:      proto.Uint32(102400),
		}
	}

	// 启动时打印协议版本与设备指纹，便于排查风控问题
	waVer := store.GetWAVersion()
	log.Infof("=== Startup Info ===")
	log.Infof("whatsmeow WA version: %s (buildHash: %x)", waVer.String(), waVer.Hash())
	log.Infof("DeviceProps: OS=%q Version=%d.%d.%d PlatformType=%v",
		store.DeviceProps.GetOs(),
		store.DeviceProps.Version.GetPrimary(),
		store.DeviceProps.Version.GetSecondary(),
		store.DeviceProps.Version.GetTertiary(),
		store.DeviceProps.GetPlatformType())
	log.Infof("BaseClientPayload: Platform=%v OsVersion=%q Manufacturer=%q Device=%q DeviceType=%v",
		store.BaseClientPayload.UserAgent.GetPlatform(),
		store.BaseClientPayload.UserAgent.GetOsVersion(),
		store.BaseClientPayload.UserAgent.GetManufacturer(),
		store.BaseClientPayload.UserAgent.GetDevice(),
		store.BaseClientPayload.UserAgent.GetDeviceType())
	log.Infof("====================")

	dbLog := waLog.Stdout("Database", logLevel, true)
	ctx := context.Background()
	storeContainer, err := sqlstore.New(ctx, *dbDialect, *dbAddress, dbLog)
	if err != nil {
		log.Errorf("Failed to connect to database: %v", err)
		return
	}
	device, err = storeContainer.GetFirstDevice(ctx)
	if err != nil {
		log.Errorf("Failed to get device: %v", err)
		return
	}

	// Configure iOS device fingerprint for ViewOnce support
	store.SetDeviceFingerprintIOS()

	cli = whatsmeow.NewClient(device, waLog.Stdout("Client", logLevel, true))
	presenceMgr = NewPresenceManager(cli)
	var isWaitingForPair atomic.Bool
	cli.PrePairCallback = func(jid types.JID, platform, businessName string) bool {
		isWaitingForPair.Store(true)
		defer isWaitingForPair.Store(false)
		log.Infof("Pairing %s (platform: %q, business name: %q). Type r within 3 seconds to reject pair", jid, platform, businessName)
		select {
		case reject := <-pairRejectChan:
			if reject {
				log.Infof("Rejecting pair")
				return false
			}
		case <-time.After(3 * time.Second):
		}
		log.Infof("Accepting pair")
		return true
	}
	cli.OnLoginSuccess = func() {
		printUserInfo()
		ProtoOutput(MsgLoginSuccess, map[string]any{})
		parseRealLid()
	}

	//ch, err := cli.GetQRChannel(context.Background())
	//if err != nil {
	//	// This error means that we're already logged in, so ignore it.
	//	if !errors.Is(err, whatsmeow.ErrQRStoreContainsID) {
	//		log.Errorf("Failed to get QR channel: %v", err)
	//	}
	//} else {
	//	go func() {
	//		for evt := range ch {
	//			if evt.Event == "code" {
	//				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
	//				log.Infof("qrcode: $%s$", evt.Code)
	//			} else {
	//				log.Infof("QR channel result: %s", evt.Event)
	//			}
	//		}
	//	}()
	//}

	// Heartbeat: report process health metrics every 60 seconds for Java-side zombie detection
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			ProtoOutput(MsgHeartbeat, map[string]any{
				"goroutines":  runtime.NumGoroutine(),
				"mem_mb":      m.Alloc / 1024 / 1024,
				"subscribers": presenceMgr.Count(),
				"uptime_sec":  time.Since(time.Unix(startupTime, 0)).Seconds(),
			})
		}
	}()

	// 周期重试暂存的 view-once 媒体（attach 触发为主，周期兜底覆盖重传失败/延迟场景；
	// 仅在有 Java 客户端时执行，预签名依赖 Server 在线）
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if HasJavaClient() {
				flushPendingUploads()
			}
		}
	}()

	cli.AddEventHandler(handler)
	if device.ID != nil {
		err = cli.Connect()
		if err != nil {
			log.Errorf("Failed to connect: %v", err)
			return
		}
		log.Infof("Client is ready")
	} else {
		log.Infof("Device not logged in. Use 'require-qrcode' or 'pair-phone' to log in.")
	}

	// daemon 模式下命令由 startCommandSocket 的 accept goroutine 喂入 input channel
	// （socket 已在前方 daemon 初始化块中前置启动）；非 daemon 模式在这里初始化 signal
	// 与 stdin goroutine（仅手动终端调试用）。
	if !daemonMode {
		c = make(chan os.Signal, 1)
		input = make(chan string)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		// 回退模式（仅手动终端调试用，Java 不再走此路径）：从 stdin 读命令
		go func() {
			defer close(input)
			scan := bufio.NewScanner(os.Stdin)
			for scan.Scan() {
				line := strings.TrimSpace(scan.Text())
				if len(line) > 0 {
					input <- line
				}
			}
		}()
	}
	for {
		select {
		case <-c:
			log.Infof("Interrupt received, exiting")
			cli.Disconnect()
			return
		case cmd, ok := <-input:
			// daemon 模式下 input channel 不会关闭（socket accept 循环常驻）；
			// 旧模式下 stdin goroutine defer close(input) 会导致 ok=false → 退出
			if !ok || len(cmd) == 0 {
				if !daemonMode {
					log.Infof("Stdin closed, exiting")
					cli.Disconnect()
					return
				}
				continue
			}
			if isWaitingForPair.Load() {
				if cmd == "r" {
					pairRejectChan <- true
				} else if cmd == "a" {
					pairRejectChan <- false
				}
				continue
			}
			args := strings.Fields(cmd)
			cmd = args[0]
			args = args[1:]
			go handleCmd(strings.ToLower(cmd), args)
		}
	}
}

// startCommandSocket listens on a Unix domain socket for Java commands and feeds them
// into the shared input channel (same as the legacy stdin goroutine). Also installs the
// accepted connection as the ProtoOutput backend so Go→Java events flow back over the
// same socket. A single Java client is expected at a time; a new connection replaces the
// old one (Java re-attach after restart). A client disconnecting does NOT exit the daemon.
func startCommandSocket(input chan<- string, sockPath string) {
	// 清理可能残留的旧 socket 文件（上次异常退出留下）
	_ = os.Remove(sockPath)
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		// 静默 return 会让 main 循环空转阻塞在 input channel 上（僵尸 daemon）；
		// 直接退出由 Java 端 ProcessUtils 检测进程死亡后走重启路径。
		log.Errorf("Failed to listen on socket %s: %v", sockPath, err)
		os.Exit(1)
	}
	// 限制权限：仅属主读写
	_ = os.Chmod(sockPath, 0600)
	log.Infof("Command socket listening at %s", sockPath)

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				// listener closed (daemon exiting)
				return
			}
			// 连接用途握手（标记协议）：完整 attach 连接首行发 "attach" → 设后端 + 重放缓冲；
			// 命令/探测连接（sendCommand、health checker probe）首行是命令或空 → 只喂命令、
			// 不设后端、不重放。避免非 attach 连接提前消耗 PresenceCache 缓冲，导致 Java 掉线
			// 窗口的事件写进无人读取的 socket 而永久丢失。
			buf := bufio.NewReader(conn)
			conn.SetReadDeadline(time.Now().Add(attachHandshakeTimeout))
			first, readErr := buf.ReadString('\n')
			conn.SetReadDeadline(time.Time{}) // 清除握手超时，进入正常读取
			first = strings.TrimSpace(first)
			isAttach := readErr == nil && first == "attach"
			if isAttach {
				// 新 Java attach：设为 ProtoOutput 后端，旧连接会被 setProtoConn 关闭。
				// 先重放 Java 掉线期间缓存的上下线通知，再异步重传暂存的 view-once 媒体。
				setProtoConn(conn)
				presenceCache.Replay()
				go flushPendingUploads()
				log.Infof("Java client attached from %s", conn.RemoteAddr())
			} else if readErr == nil && len(first) > 0 {
				log.Debugf("command-only connection: %s", first)
				input <- first
			}
			go func(c net.Conn, r *bufio.Reader) {
				defer func() {
					// 仅当 c 仍是当前后端连接时才清空：旧连接读 goroutine 可能晚于新连接
					// setProtoConn 执行，无条件 clearProtoConn 会把刚建立的新连接清掉，
					// 导致事件全部转入缓冲且永不重放（与 socketBackend.write 错误路径同模式）。
					globalSocketBackend.mu.Lock()
					if globalSocketBackend.conn == c {
						globalSocketBackend.conn = nil
					}
					globalSocketBackend.mu.Unlock()
					_ = c.Close()
				}()
				scan := bufio.NewScanner(r)
				scan.Buffer(make([]byte, 0, 64*1024), 1024*1024)
				for scan.Scan() {
					line := strings.TrimSpace(scan.Text())
					if len(line) > 0 {
						input <- line
					}
				}
				// client disconnected (Java restart) — daemon stays alive, waits for re-attach
				log.Infof("Java client disconnected, daemon stays alive")
			}(conn, buf)
		}
	}()
}

func parseRealLid() {
	lidStr := device.GetLID().String()
	// 匹配中间带冒号和数字的部分，并将其替换为空
	// :(\d+) 匹配冒号加数字，@ 确保它是 JID 的一部分
	re := regexp.MustCompile(`:\d+@`)
	lid = re.ReplaceAllString(lidStr, "@")

	fmt.Printf("real lid is: %s\n", lid)
}

func printUserInfo() {
	pushName := cli.Store.PushName
	if pushName == "" {
		contact, err := cli.Store.Contacts.GetContact(context.TODO(), *cli.Store.ID)
		if err == nil {
			pushName = contact.PushName
		}
	}
	ProtoOutput(MsgPushName, map[string]any{"name": pushName})
	ProtoOutput(MsgPhoneNumber, map[string]any{"number": cli.Store.ID.ToNonAD().User})
}

func parseJID(arg string) (types.JID, bool) {
	if arg[0] == '+' {
		arg = arg[1:]
	}
	if !strings.ContainsRune(arg, '@') {
		return types.NewJID(arg, types.DefaultUserServer), true
	} else {
		recipient, err := types.ParseJID(arg)
		if err != nil {
			log.Errorf("Invalid JID %s: %v", arg, err)
			return recipient, false
		} else if recipient.User == "" {
			log.Errorf("Invalid JID %s: no server specified", arg)
			return recipient, false
		}
		return recipient, true
	}
}

func handleCmd(cmd string, args []string) {
	ctx := context.Background()
	switch cmd {
	// Set device display name shown in WhatsApp "Linked Devices" list.
	// Must be sent before pair-phone / require-qrcode for new registrations;
	// already-paired devices retain their name server-side and are unaffected.
	case "set-logintype":
		if len(args) < 1 {
			log.Errorf("Usage: set-logintype <waTracker_android|wastgo_win>")
			return
		}
		switch args[0] {
		case "waTracker_android":
			store.DeviceProps.Os = proto.String("WeSeen")
		case "wastgo_win":
			store.DeviceProps.Os = proto.String("WastGo")
		default:
			log.Errorf("Unknown login type: %s", args[0])
			return
		}
		log.Infof("Device display name set to: %s", *store.DeviceProps.Os)

	case "enable-view-once":
		enableViewOnce = true
		ProtoOutput(MsgViewOnceEnabled, map[string]any{"enabled": true})
	case "disable-view-once":
		enableViewOnce = false
		ProtoOutput(MsgViewOnceEnabled, map[string]any{"enabled": false})
	case "pair-phone":
		if len(args) < 1 {
			log.Errorf("Usage: pair-phone <number>")
			return
		}
		if !cli.IsConnected() {
			err := cli.Connect()
			if err != nil {
				log.Errorf("Failed to connect: %v", err)
				return
			}
			log.Infof("Client is ready")
			//time.Sleep(2 * time.Second)
		}
		// PairPhone 必须使用浏览器类型（code pairing 是 WhatsApp 浏览器端功能）
		// 服务器校验 companion_platform_display 必须为 "Browser (OS)" 格式，否则返回 400
		// 注意：这里的浏览器身份 ≠ DeviceProps.PlatformType（后者控制 view-once 等设备能力，独立于配对阶段）
		linkingCode, err := cli.PairPhone(ctx, args[0], true, whatsmeow.PairClientChrome, "Chrome (Linux)")
		if err != nil {
			ProtoOutput(MsgPairError, map[string]any{"error": err.Error()})
			return
		}
		ProtoOutput(MsgLinkingCode, map[string]any{"code": linkingCode})
	case "require-qrcode":
		if cli.IsConnected() {
			log.Errorf("Already connected, can't start QR login")
			return
		}
		qrChan, err := cli.GetQRChannel(context.Background())
		if err != nil {
			log.Errorf("Failed to get QR channel: %v", err)
			return
		}
		go func() {
			for evt := range qrChan {
				if evt.Event == whatsmeow.QRChannelEventCode {
					ProtoOutput(MsgQrCode, map[string]any{"code": evt.Code})
					printQRCode(evt.Code)
				} else {
					log.Infof("QR channel result: %s", evt.Event)
					if evt.Event == whatsmeow.QRChannelTimeout.Event {
						ProtoOutput(MsgQrTimeout, map[string]any{})
					}
				}
			}
		}()
		err = cli.Connect()
		if err != nil {
			log.Errorf("Failed to connect: %v", err)
		} else {
			log.Infof("Client is ready")
		}
	case "reconnect":
		cli.Disconnect()
		err := cli.Connect()
		if err != nil {
			log.Errorf("Failed to connect: %v", err)
		}
	case "logout":
		err := cli.Logout(ctx)
		if err != nil {
			log.Errorf("Error logging out: %v", err)
		} else {
			ProtoOutput(MsgLogoutSuccess, map[string]any{})
		}
	case "appstate":
		if len(args) < 1 {
			log.Errorf("Usage: appstate <types...>")
			return
		}
		names := []appstate.WAPatchName{appstate.WAPatchName(args[0])}
		if args[0] == "all" {
			names = []appstate.WAPatchName{appstate.WAPatchRegular, appstate.WAPatchRegularHigh, appstate.WAPatchRegularLow, appstate.WAPatchCriticalUnblockLow, appstate.WAPatchCriticalBlock}
		}
		resync := len(args) > 1 && args[1] == "resync"
		for _, name := range names {
			err := cli.FetchAppState(ctx, name, resync, false)
			if err != nil {
				log.Errorf("Failed to sync app state: %v", err)
			}
		}
	case "request-appstate-key":
		if len(args) < 1 {
			log.Errorf("Usage: request-appstate-key <ids...>")
			return
		}
		var keyIDs = make([][]byte, len(args))
		for i, id := range args {
			decoded, err := hex.DecodeString(id)
			if err != nil {
				log.Errorf("Failed to decode %s as hex: %v", id, err)
				return
			}
			keyIDs[i] = decoded
		}
		cli.DangerousInternals().RequestAppStateKeys(context.Background(), keyIDs)
	case "unavailable-request":
		if len(args) < 3 {
			log.Errorf("Usage: unavailable-request <chat JID> <sender JID> <message ID>")
			return
		}
		chat, ok := parseJID(args[0])
		if !ok {
			return
		}
		sender, ok := parseJID(args[1])
		if !ok {
			return
		}
		resp, err := cli.SendMessage(
			context.Background(),
			cli.Store.ID.ToNonAD(),
			cli.BuildUnavailableMessageRequest(chat, sender, args[2]),
			whatsmeow.SendRequestExtra{Peer: true},
		)
		fmt.Println(resp)
		fmt.Println(err)
	case "checkuser":
		if len(args) < 1 {
			log.Errorf("Usage: checkuser <phone numbers...>")
			return
		}
		resp, err := cli.IsOnWhatsApp(ctx, args)
		if err != nil {
			log.Errorf("Failed to check if users are on WhatsApp: %v", err)
		} else {
			// 记录已响应的号码（去掉 + 前缀），用于补发未返回的号码
			respondedPhones := make(map[string]bool, len(resp))
			for _, item := range resp {
				data := map[string]any{
					"query": item.Query,
					"isIn":  item.IsIn,
					"jid":   searchPhoneNum(ctx, item.JID),
				}
				if item.VerifiedName != nil {
					data["businessName"] = item.VerifiedName.Details.GetVerifiedName()
				}
				ProtoOutput(MsgCheckUser, data)
				respondedPhones[strings.TrimPrefix(item.Query, "+")] = true
			}
			// 对于 WhatsApp 服务器未返回的号码（未注册等情况），补发 isIn=false
			for _, phone := range args {
				cleanPhone := strings.TrimPrefix(phone, "+")
				if !respondedPhones[cleanPhone] {
					log.Infof("checkuser: phone %s not in response, emitting isIn=false", cleanPhone)
					ProtoOutput(MsgCheckUser, map[string]any{
						"query": cleanPhone,
						"isIn":  false,
						"jid":   cleanPhone + "@s.whatsapp.net",
					})
				}
			}
		}
	//case "checkupdate":
	//	resp, err := cli.CheckUpdate()
	//	if err != nil {
	//		log.Errorf("Failed to check for updates: %v", err)
	//	} else {
	//		log.Debugf("Version data: %#v", resp)
	//		if resp.ParsedVersion == store.GetWAVersion() {
	//			log.Infof("Client is up to date")
	//		} else if store.GetWAVersion().LessThan(resp.ParsedVersion) {
	//			log.Warnf("Client is outdated")
	//		} else {
	//			log.Infof("Client is newer than latest")
	//		}
	//	}
	case "subscribepresence":
		if len(args) < 1 {
			log.Errorf("Usage: subscribepresence <jid>")
			return
		}
		if err := presenceMgr.Subscribe(args[0]); err != nil {
			log.Errorf("Failed to subscribe presence for %s: %v", args[0], err)
		} else {
			// 重新订阅 = 取消退订标记（删除后重新添加的 observer 恢复事件处理）
			presenceMgr.ClearUnsubscribed(args[0])
		}
	case "unsubscribepresence":
		// 软退订（whatsmeow 无协议级 UnsubscribePresence，服务器侧订阅到下次连接周期
		// ResubscribeAll 不再重订时自动停止）：
		//   1. 移出 subscribedJIDs —— 重连后不再重订；
		//   2. 标记退订 —— presence handler 跳过该 JID，不再缓存/重放；
		//   3. 清空 PresenceCache —— 抹掉已滞留的该 JID 事件。
		if len(args) < 1 {
			log.Errorf("Usage: unsubscribepresence <jid>")
			return
		}
		jid := args[0]
		presenceMgr.Unsubscribe(jid)
		presenceMgr.MarkUnsubscribed(jid)
		presenceCache.Remove(jid)
		log.Infof("Unsubscribed presence for %s", jid)
	case "presence":
		if len(args) == 0 {
			log.Errorf("Usage: presence <available/unavailable>")
			return
		}
		fmt.Println(cli.SendPresence(ctx, types.Presence(args[0])))
	case "chatpresence":
		if len(args) == 2 {
			args = append(args, "")
		} else if len(args) < 2 {
			log.Errorf("Usage: chatpresence <jid> <composing/paused> [audio]")
			return
		}
		jid, _ := types.ParseJID(args[0])
		fmt.Println(cli.SendChatPresence(ctx, jid, types.ChatPresence(args[1]), types.ChatPresenceMedia(args[2])))
	case "privacysettings":
		resp, err := cli.TryFetchPrivacySettings(ctx, false)
		if err != nil {
			fmt.Println(err)
		} else {
			fmt.Printf("%+v\n", resp)
		}
	case "setprivacysetting":
		if len(args) < 2 {
			log.Errorf("Usage: setprivacysetting <setting> <value>")
			return
		}
		setting := types.PrivacySettingType(args[0])
		value := types.PrivacySetting(args[1])
		resp, err := cli.SetPrivacySetting(ctx, setting, value)
		if err != nil {
			fmt.Println(err)
		} else {
			fmt.Printf("%+v\n", resp)
		}
	case "getuser":
		if len(args) < 1 {
			log.Errorf("Usage: getuser <jids...>")
			return
		}
		var jids []types.JID
		for _, arg := range args {
			jid, ok := parseJID(arg)
			if !ok {
				return
			}
			jids = append(jids, jid)
		}
		resp, err := cli.GetUserInfo(ctx, jids)
		if err != nil {
			log.Errorf("Failed to get user info: %v", err)
		} else {
			for jid, info := range resp {
				log.Infof("%s: %+v", jid, info)
			}
		}
	case "mediaconn":
		conn, err := cli.DangerousInternals().RefreshMediaConn(ctx, false)
		if err != nil {
			log.Errorf("Failed to get media connection: %v", err)
		} else {
			log.Infof("Media connection: %+v", conn)
		}
	case "raw":
		var node waBinary.Node
		if err := json.Unmarshal([]byte(strings.Join(args, " ")), &node); err != nil {
			log.Errorf("Failed to parse args as JSON into XML node: %v", err)
		} else if err = cli.DangerousInternals().SendNode(ctx, node); err != nil {
			log.Errorf("Error sending node: %v", err)
		} else {
			log.Infof("Node sent")
		}
	case "listnewsletters":
		newsletters, err := cli.GetSubscribedNewsletters(ctx)
		if err != nil {
			log.Errorf("Failed to get subscribed newsletters: %v", err)
			return
		}
		for _, newsletter := range newsletters {
			log.Infof("* %s: %s", newsletter.ID, newsletter.ThreadMeta.Name.Text)
		}
	case "getnewsletter":
		jid, ok := parseJID(args[0])
		if !ok {
			return
		}
		meta, err := cli.GetNewsletterInfo(ctx, jid)
		if err != nil {
			log.Errorf("Failed to get info: %v", err)
		} else {
			log.Infof("Got info: %+v", meta)
		}
	case "getnewsletterinvite":
		meta, err := cli.GetNewsletterInfoWithInvite(ctx, args[0])
		if err != nil {
			log.Errorf("Failed to get info: %v", err)
		} else {
			log.Infof("Got info: %+v", meta)
		}
	case "livesubscribenewsletter":
		if len(args) < 1 {
			log.Errorf("Usage: livesubscribenewsletter <jid>")
			return
		}
		jid, ok := parseJID(args[0])
		if !ok {
			return
		}
		dur, err := cli.NewsletterSubscribeLiveUpdates(context.TODO(), jid)
		if err != nil {
			log.Errorf("Failed to subscribe to live updates: %v", err)
		} else {
			log.Infof("Subscribed to live updates for %s for %s", jid, dur)
		}
	case "getnewslettermessages":
		if len(args) < 1 {
			log.Errorf("Usage: getnewslettermessages <jid> [count] [before id]")
			return
		}
		jid, ok := parseJID(args[0])
		if !ok {
			return
		}
		count := 100
		var err error
		if len(args) > 1 {
			count, err = strconv.Atoi(args[1])
			if err != nil {
				log.Errorf("Invalid count: %v", err)
				return
			}
		}
		var before types.MessageServerID
		if len(args) > 2 {
			before, err = strconv.Atoi(args[2])
			if err != nil {
				log.Errorf("Invalid message ID: %v", err)
				return
			}
		}
		messages, err := cli.GetNewsletterMessages(ctx, jid, &whatsmeow.GetNewsletterMessagesParams{Count: count, Before: before})
		if err != nil {
			log.Errorf("Failed to get messages: %v", err)
		} else {
			for _, msg := range messages {
				log.Infof("%d: %+v (viewed %d times)", msg.MessageServerID, msg.Message, msg.ViewsCount)
			}
		}
	case "createnewsletter":
		if len(args) < 1 {
			log.Errorf("Usage: createnewsletter <name>")
			return
		}
		resp, err := cli.CreateNewsletter(ctx, whatsmeow.CreateNewsletterParams{
			Name: strings.Join(args, " "),
		})
		if err != nil {
			log.Errorf("Failed to create newsletter: %v", err)
		} else {
			log.Infof("Created newsletter %+v", resp)
		}
	case "getavatar":
		if len(args) < 1 {
			log.Errorf("Usage: getavatar <jid> [existing ID] [--preview] [--community]")
			return
		}
		jid, ok := parseJID(args[0])
		if !ok {
			return
		}
		existingID := ""
		if len(args) > 2 {
			existingID = args[2]
		}
		var preview, isCommunity bool
		for _, arg := range args {
			if arg == "--preview" {
				preview = true
			} else if arg == "--community" {
				isCommunity = true
			}
		}
		pic, err := cli.GetProfilePictureInfo(ctx, jid, &whatsmeow.GetProfilePictureParams{
			Preview:     preview,
			IsCommunity: isCommunity,
			ExistingID:  existingID,
		})
		if err != nil {
			//log.Errorf("Failed to get avatar for %s: %v", jid, err)
			ProtoOutput(MsgGetAvatarFail, map[string]any{"jid": searchPhoneNum(ctx, jid)})
		} else if pic != nil {
			ProtoOutput(MsgGetAvatar, map[string]any{"jid": searchPhoneNum(ctx, jid), "url": pic.URL})
		} else {
			ProtoOutput(MsgGetAvatarFail, map[string]any{"jid": searchPhoneNum(ctx, jid)})
		}
	case "getgroup":
		if len(args) < 1 {
			log.Errorf("Usage: getgroup <jid>")
			return
		}
		group, ok := parseJID(args[0])
		if !ok {
			return
		} else if group.Server != types.GroupServer {
			log.Errorf("Input must be a group JID (@%s)", types.GroupServer)
			return
		}
		resp, err := cli.GetGroupInfo(ctx, group)
		if err != nil {
			log.Errorf("Failed to get group info: %v", err)
		} else {
			log.Infof("Group info: %+v", resp)
		}
	case "subgroups":
		if len(args) < 1 {
			log.Errorf("Usage: subgroups <jid>")
			return
		}
		group, ok := parseJID(args[0])
		if !ok {
			return
		} else if group.Server != types.GroupServer {
			log.Errorf("Input must be a group JID (@%s)", types.GroupServer)
			return
		}
		resp, err := cli.GetSubGroups(ctx, group)
		if err != nil {
			log.Errorf("Failed to get subgroups: %v", err)
		} else {
			for _, sub := range resp {
				log.Infof("Subgroup: %+v", sub)
			}
		}
	case "communityparticipants":
		if len(args) < 1 {
			log.Errorf("Usage: communityparticipants <jid>")
			return
		}
		group, ok := parseJID(args[0])
		if !ok {
			return
		} else if group.Server != types.GroupServer {
			log.Errorf("Input must be a group JID (@%s)", types.GroupServer)
			return
		}
		resp, err := cli.GetLinkedGroupsParticipants(ctx, group)
		if err != nil {
			log.Errorf("Failed to get community participants: %v", err)
		} else {
			log.Infof("Community participants: %+v", resp)
		}
	case "listgroups":
		groups, err := cli.GetJoinedGroups(ctx)
		if err != nil {
			log.Errorf("Failed to get group list: %v", err)
		} else {
			for _, group := range groups {
				log.Infof("%+v", group)
			}
		}
	case "getinvitelink":
		if len(args) < 1 {
			log.Errorf("Usage: getinvitelink <jid> [--reset]")
			return
		}
		group, ok := parseJID(args[0])
		if !ok {
			return
		} else if group.Server != types.GroupServer {
			log.Errorf("Input must be a group JID (@%s)", types.GroupServer)
			return
		}
		resp, err := cli.GetGroupInviteLink(ctx, group, len(args) > 1 && args[1] == "--reset")
		if err != nil {
			log.Errorf("Failed to get group invite link: %v", err)
		} else {
			log.Infof("Group invite link: %s", resp)
		}
	case "queryinvitelink":
		if len(args) < 1 {
			log.Errorf("Usage: queryinvitelink <link>")
			return
		}
		resp, err := cli.GetGroupInfoFromLink(ctx, args[0])
		if err != nil {
			log.Errorf("Failed to resolve group invite link: %v", err)
		} else {
			log.Infof("Group info: %+v", resp)
		}
	case "querybusinesslink":
		if len(args) < 1 {
			log.Errorf("Usage: querybusinesslink <link>")
			return
		}
		resp, err := cli.ResolveBusinessMessageLink(ctx, args[0])
		if err != nil {
			log.Errorf("Failed to resolve business message link: %v", err)
		} else {
			log.Infof("Business info: %+v", resp)
		}
	case "joininvitelink":
		if len(args) < 1 {
			log.Errorf("Usage: acceptinvitelink <link>")
			return
		}
		groupID, err := cli.JoinGroupWithLink(ctx, args[0])
		if err != nil {
			log.Errorf("Failed to join group via invite link: %v", err)
		} else {
			log.Infof("Joined %s", groupID)
		}
	//case "updateparticipant":
	//	if len(args) < 3 {
	//		log.Errorf("Usage: updateparticipant <jid> <action> <numbers...>")
	//		return
	//	}
	//	jid, ok := parseJID(args[0])
	//	if !ok {
	//		return
	//	}
	//	action := whatsmeow.ParticipantChange(args[1])
	//	switch action {
	//	case whatsmeow.ParticipantChangeAdd, whatsmeow.ParticipantChangeRemove, whatsmeow.ParticipantChangePromote, whatsmeow.ParticipantChangeDemote:
	//	default:
	//		log.Errorf("Valid actions: add, remove, promote, demote")
	//		return
	//	}
	//	users := make([]types.JID, len(args)-2)
	//	for i, arg := range args[2:] {
	//		users[i], ok = parseJID(arg)
	//		if !ok {
	//			return
	//		}
	//	}
	//	resp, err := cli.UpdateGroupParticipants(jid, users, action)
	//	if err != nil {
	//		log.Errorf("Failed to add participant: %v", err)
	//		return
	//	}
	//	for _, item := range resp {
	//		if action == whatsmeow.ParticipantChangeAdd && item.Error == 403 && item.AddRequest != nil {
	//			log.Infof("Participant is private: %d %s %s %v", item.Error, item.JID, item.AddRequest.Code, item.AddRequest.Expiration)
	//			cli.SendMessage(context.TODO(), item.JID, &waProto.Message{
	//				GroupInviteMessage: &waProto.GroupInviteMessage{
	//					InviteCode:       proto.String(item.AddRequest.Code),
	//					InviteExpiration: proto.Int64(item.AddRequest.Expiration.Unix()),
	//					GroupJid:         proto.String(jid.String()),
	//					GroupName:        proto.String("Test group"),
	//					Caption:          proto.String("This is a test group"),
	//				},
	//			})
	//		} else if item.Error == 409 {
	//			log.Infof("Participant already in group: %d %s %+v", item.Error, item.JID)
	//		} else if item.Error == 0 {
	//			log.Infof("Added participant: %d %s %+v", item.Error, item.JID)
	//		} else {
	//			log.Infof("Unknown status: %d %s %+v", item.Error, item.JID)
	//		}
	//	}
	case "getrequestparticipant":
		if len(args) < 1 {
			log.Errorf("Usage: getrequestparticipant <jid>")
			return
		}
		group, ok := parseJID(args[0])
		if !ok {
			log.Errorf("Invalid JID")
			return
		}
		resp, err := cli.GetGroupRequestParticipants(ctx, group)
		if err != nil {
			log.Errorf("Failed to get request participants: %v", err)
		} else {
			log.Infof("Request participants: %+v", resp)
		}
	case "getstatusprivacy":
		resp, err := cli.GetStatusPrivacy(ctx)
		fmt.Println(err)
		fmt.Println(resp)
	case "setdisappeartimer":
		if len(args) < 2 {
			log.Errorf("Usage: setdisappeartimer <jid> <days>")
			return
		}
		days, err := strconv.Atoi(args[1])
		if err != nil {
			log.Errorf("Invalid duration: %v", err)
			return
		}
		recipient, ok := parseJID(args[0])
		if !ok {
			return
		}
		err = cli.SetDisappearingTimer(ctx, recipient, time.Duration(days)*24*time.Hour, time.Now())
		if err != nil {
			log.Errorf("Failed to set disappearing timer: %v", err)
		}
	case "setdefaultdisappeartimer":
		if len(args) < 1 {
			log.Errorf("Usage: setdefaultdisappeartimer <days>")
			return
		}
		days, err := strconv.Atoi(args[0])
		if err != nil {
			log.Errorf("Invalid duration: %v", err)
			return
		}
		err = cli.SetDefaultDisappearingTimer(ctx, time.Duration(days)*24*time.Hour)
		if err != nil {
			log.Errorf("Failed to set default disappearing timer: %v", err)
		}
	case "send":
		if len(args) < 2 {
			log.Errorf("Usage: send <jid> <text>")
			return
		}
		recipient, ok := parseJID(args[0])
		if !ok {
			return
		}
		msg := &waProto.Message{Conversation: proto.String(strings.Join(args[1:], " "))}
		resp, err := cli.SendMessage(context.Background(), recipient, msg)
		if err != nil {
			log.Errorf("Error sending message: %v", err)
		} else {
			log.Infof("Message sent (server timestamp: %s)", resp.Timestamp)
		}
	case "sendpoll":
		if len(args) < 7 {
			log.Errorf("Usage: sendpoll <jid> <max answers> <question> -- <option 1> / <option 2> / ...")
			return
		}
		recipient, ok := parseJID(args[0])
		if !ok {
			return
		}
		maxAnswers, err := strconv.Atoi(args[1])
		if err != nil {
			log.Errorf("Number of max answers must be an integer")
			return
		}
		remainingArgs := strings.Join(args[2:], " ")
		question, optionsStr, _ := strings.Cut(remainingArgs, "--")
		question = strings.TrimSpace(question)
		options := strings.Split(optionsStr, "/")
		for i, opt := range options {
			options[i] = strings.TrimSpace(opt)
		}
		resp, err := cli.SendMessage(context.Background(), recipient, cli.BuildPollCreation(question, options, maxAnswers))
		if err != nil {
			log.Errorf("Error sending message: %v", err)
		} else {
			log.Infof("Message sent (server timestamp: %s)", resp.Timestamp)
		}
	//case "react":
	//	if len(args) < 3 {
	//		log.Errorf("Usage: react <jid> <message ID> <reaction>")
	//		return
	//	}
	//	recipient, ok := parseJID(args[0])
	//	if !ok {
	//		return
	//	}
	//	messageID := args[1]
	//	fromMe := false
	//	if strings.HasPrefix(messageID, "me:") {
	//		fromMe = true
	//		messageID = messageID[len("me:"):]
	//	}
	//	reaction := args[2]
	//	if reaction == "remove" {
	//		reaction = ""
	//	}
	//	msg := &waProto.Message{
	//		ReactionMessage: &waProto.ReactionMessage{
	//			Key: &waProto.MessageKey{
	//				RemoteJid: proto.String(recipient.String()),
	//				FromMe:    proto.Bool(fromMe),
	//				Id:        proto.String(messageID),
	//			},
	//			Text:              proto.String(reaction),
	//			SenderTimestampMs: proto.Int64(time.Now().UnixMilli()),
	//		},
	//	}
	//	resp, err := cli.SendMessage(context.Background(), recipient, msg)
	//	if err != nil {
	//		log.Errorf("Error sending reaction: %v", err)
	//	} else {
	//		log.Infof("Reaction sent (server timestamp: %s)", resp.Timestamp)
	//	}
	case "revoke":
		if len(args) < 2 {
			log.Errorf("Usage: revoke <jid> <message ID>")
			return
		}
		recipient, ok := parseJID(args[0])
		if !ok {
			return
		}
		messageID := args[1]
		resp, err := cli.SendMessage(context.Background(), recipient, cli.BuildRevoke(recipient, types.EmptyJID, messageID))
		if err != nil {
			log.Errorf("Error sending revocation: %v", err)
		} else {
			log.Infof("Revocation sent (server timestamp: %s)", resp.Timestamp)
		}
	//case "sendimg":
	//	if len(args) < 2 {
	//		log.Errorf("Usage: sendimg <jid> <image path> [caption]")
	//		return
	//	}
	//	recipient, ok := parseJID(args[0])
	//	if !ok {
	//		return
	//	}
	//	data, err := os.ReadFile(args[1])
	//	if err != nil {
	//		log.Errorf("Failed to read %s: %v", args[0], err)
	//		return
	//	}
	//	var uploaded whatsmeow.UploadResponse
	//	if recipient.Server == types.NewsletterServer {
	//		uploaded, err = cli.UploadNewsletter(context.Background(), data, whatsmeow.MediaImage)
	//	} else {
	//		uploaded, err = cli.Upload(context.Background(), data, whatsmeow.MediaImage)
	//	}
	//	if err != nil {
	//		log.Errorf("Failed to upload file: %v", err)
	//		return
	//	}
	//	msg := &waProto.Message{ImageMessage: &waProto.ImageMessage{
	//		Caption:       proto.String(strings.Join(args[2:], " ")),
	//		Url:           proto.String(uploaded.URL),
	//		DirectPath:    proto.String(uploaded.DirectPath),
	//		MediaKey:      uploaded.MediaKey,
	//		Mimetype:      proto.String(http.DetectContentType(data)),
	//		FileEncSha256: uploaded.FileEncSHA256,
	//		FileSha256:    uploaded.FileSHA256,
	//		FileLength:    proto.Uint64(uint64(len(data))),
	//	}}
	//	resp, err := cli.SendMessage(context.Background(), recipient, msg, whatsmeow.SendRequestExtra{
	//		MediaHandle: uploaded.Handle,
	//	})
	//	if err != nil {
	//		log.Errorf("Error sending image message: %v", err)
	//	} else {
	//		log.Infof("Image message sent (server timestamp: %s)", resp.Timestamp)
	//	}
	case "setpushname":
		if len(args) == 0 {
			log.Errorf("Usage: setpushname <name>")
			return
		}
		err := cli.SendAppState(ctx, appstate.BuildSettingPushName(strings.Join(args, " ")))
		if err != nil {
			log.Errorf("Error setting push name: %v", err)
		} else {
			log.Infof("Push name updated")
		}
	case "setstatus":
		if len(args) == 0 {
			log.Errorf("Usage: setstatus <message>")
			return
		}
		err := cli.SetStatusMessage(ctx, strings.Join(args, " "))
		if err != nil {
			log.Errorf("Error setting status message: %v", err)
		} else {
			log.Infof("Status updated")
		}
	case "archive":
		if len(args) < 2 {
			log.Errorf("Usage: archive <jid> <action>")
			return
		}
		target, ok := parseJID(args[0])
		if !ok {
			return
		}
		action, err := strconv.ParseBool(args[1])
		if err != nil {
			log.Errorf("invalid second argument: %v", err)
			return
		}

		err = cli.SendAppState(ctx, appstate.BuildArchive(target, action, time.Time{}, nil))
		if err != nil {
			log.Errorf("Error changing chat's archive state: %v", err)
		}
	case "mute":
		if len(args) < 2 {
			log.Errorf("Usage: mute <jid> <action>")
			return
		}
		target, ok := parseJID(args[0])
		if !ok {
			return
		}
		action, err := strconv.ParseBool(args[1])
		if err != nil {
			log.Errorf("invalid second argument: %v", err)
			return
		}

		err = cli.SendAppState(ctx, appstate.BuildMute(target, action, 1*time.Hour))
		if err != nil {
			log.Errorf("Error changing chat's mute state: %v", err)
		}
	case "pin":
		if len(args) < 2 {
			log.Errorf("Usage: pin <jid> <action>")
			return
		}
		target, ok := parseJID(args[0])
		if !ok {
			return
		}
		action, err := strconv.ParseBool(args[1])
		if err != nil {
			log.Errorf("invalid second argument: %v", err)
			return
		}

		err = cli.SendAppState(ctx, appstate.BuildPin(target, action))
		if err != nil {
			log.Errorf("Error changing chat's pin state: %v", err)
		}
	case "getblocklist":
		blocklist, err := cli.GetBlocklist(ctx)
		if err != nil {
			log.Errorf("Failed to get blocked contacts list: %v", err)
		} else {
			log.Infof("Blocklist: %+v", blocklist)
		}
	case "block":
		if len(args) < 1 {
			log.Errorf("Usage: block <jid>")
			return
		}
		jid, ok := parseJID(args[0])
		if !ok {
			return
		}
		resp, err := cli.UpdateBlocklist(ctx, jid, events.BlocklistChangeActionBlock)
		if err != nil {
			log.Errorf("Error updating blocklist: %v", err)
		} else {
			log.Infof("Blocklist updated: %+v", resp)
		}
	case "unblock":
		if len(args) < 1 {
			log.Errorf("Usage: unblock <jid>")
			return
		}
		jid, ok := parseJID(args[0])
		if !ok {
			return
		}
		resp, err := cli.UpdateBlocklist(ctx, jid, events.BlocklistChangeActionUnblock)
		if err != nil {
			log.Errorf("Error updating blocklist: %v", err)
		} else {
			log.Infof("Blocklist updated: %+v", resp)
		}
	case "labelchat":
		if len(args) < 3 {
			log.Errorf("Usage: labelchat <jid> <labelID> <action>")
			return
		}
		jid, ok := parseJID(args[0])
		if !ok {
			return
		}
		labelID := args[1]
		action, err := strconv.ParseBool(args[2])
		if err != nil {
			log.Errorf("invalid third argument: %v", err)
			return
		}

		err = cli.SendAppState(ctx, appstate.BuildLabelChat(jid, labelID, action))
		if err != nil {
			log.Errorf("Error changing chat's label state: %v", err)
		}
	case "labelmessage":
		if len(args) < 4 {
			log.Errorf("Usage: labelmessage <jid> <labelID> <messageID> <action>")
			return
		}
		jid, ok := parseJID(args[0])
		if !ok {
			return
		}
		labelID := args[1]
		messageID := args[2]
		action, err := strconv.ParseBool(args[3])
		if err != nil {
			log.Errorf("invalid fourth argument: %v", err)
			return
		}

		err = cli.SendAppState(ctx, appstate.BuildLabelMessage(jid, labelID, messageID, action))
		if err != nil {
			log.Errorf("Error changing message's label state: %v", err)
		}
	case "editlabel":
		if len(args) < 4 {
			log.Errorf("Usage: editlabel <labelID> <name> <color> <action>")
			return
		}
		labelID := args[0]
		name := args[1]
		color, err := strconv.Atoi(args[2])
		if err != nil {
			log.Errorf("invalid third argument: %v", err)
			return
		}
		action, err := strconv.ParseBool(args[3])
		if err != nil {
			log.Errorf("invalid fourth argument: %v", err)
			return
		}

		err = cli.SendAppState(ctx, appstate.BuildLabelEdit(labelID, name, int32(color), action))
		if err != nil {
			log.Errorf("Error editing label: %v", err)
		}
	}
}

var historySyncID int32
var startupTime = time.Now().Unix()

func handler(rawEvt interface{}) {
	ctx := context.Background()
	switch evt := rawEvt.(type) {
	case *events.AppStateSyncComplete:
		if len(cli.Store.PushName) > 0 && evt.Name == appstate.WAPatchCriticalBlock {
			err := cli.SendPresence(ctx, types.PresenceAvailable)
			if err != nil {
				log.Warnf("Failed to send available presence: %v", err)
			}
		}
	case *events.Connected:
		if len(cli.Store.PushName) == 0 {
			return
		}
		// Send presence available when connecting.
		// This makes sure that outgoing messages always have the right pushname.
		// MsgLoginSuccess is now sent earlier via OnLoginSuccess callback in handleConnectSuccess.
		err := cli.SendPresence(ctx, types.PresenceAvailable)
		if err != nil {
			log.Warnf("Failed to send available presence: %v", err)
		}
		// Re-subscribe all tracked contacts after reconnect (subscriptions are lost on disconnect)
		go presenceMgr.ResubscribeAll()
	case *events.PushNameSetting:
		// Pushname changed mid-session: re-send presence to update server,
		// notify Java of new nickname. Not a login event.
		if len(cli.Store.PushName) == 0 {
			return
		}
		err := cli.SendPresence(ctx, types.PresenceAvailable)
		if err != nil {
			log.Warnf("Failed to send available presence: %v", err)
		}
		printUserInfo()
		parseRealLid()
	case *events.StreamReplaced:
		log.Warnf("Stream replaced (logged in elsewhere), notifying Java and exiting")
		ProtoOutput(MsgStreamReplaced, map[string]any{
			"reason": "logged_in_elsewhere",
		})
		cli.Disconnect()
		time.Sleep(3 * time.Second) // Give Java time to process the proto message
		os.Exit(42)
	case *events.LoggedOut:
		log.Warnf("Logged out event received (reason: %v), notifying Java and exiting", evt.Reason)
		ProtoOutput(MsgLoggedOut, map[string]any{
			"reason": evt.Reason.String(),
		})
		cli.Disconnect()
		time.Sleep(3 * time.Second)
		os.Exit(43)
	case *events.Message:
		metaParts := []string{fmt.Sprintf("pushname: %s", evt.Info.PushName), fmt.Sprintf("timestamp: %s", evt.Info.Timestamp)}
		if evt.Info.Type != "" {
			metaParts = append(metaParts, fmt.Sprintf("type: %s", evt.Info.Type))
		}
		if evt.Info.Category != "" {
			metaParts = append(metaParts, fmt.Sprintf("category: %s", evt.Info.Category))
		}
		if evt.IsViewOnce {
			metaParts = append(metaParts, "view once")
		}
		if evt.IsViewOnce {
			metaParts = append(metaParts, "ephemeral")
		}
		if evt.IsViewOnceV2 {
			metaParts = append(metaParts, "ephemeral (v2)")
		}
		if evt.IsDocumentWithCaption {
			metaParts = append(metaParts, "document with caption")
		}
		if evt.IsEdit {
			metaParts = append(metaParts, "edit")
		}

		// Debug log without JID to prevent keyword false-matching in Java parser
		log.Debugf("Received message %s (%s)", evt.Info.ID, strings.Join(metaParts, ", "))
		// Protocol output for Java Server (only useful metadata, no message body)
		ProtoOutput(MsgReceivedMessage, map[string]any{
			"msgId": evt.Info.ID,
			"jid":   searchPhoneNum(ctx, evt.Info.Sender),
		})

		if evt.Message.GetPollUpdateMessage() != nil {
			decrypted, err := cli.DecryptPollVote(ctx, evt)
			if err != nil {
				log.Errorf("Failed to decrypt vote: %v", err)
			} else {
				log.Infof("Selected options in decrypted vote:")
				for _, option := range decrypted.SelectedOptions {
					log.Infof("- %X", option)
				}
			}
		} else if evt.Message.GetEncReactionMessage() != nil {
			decrypted, err := cli.DecryptReaction(ctx, evt)
			if err != nil {
				log.Errorf("Failed to decrypt encrypted reaction: %v", err)
			} else {
				log.Infof("Decrypted reaction: %+v", decrypted)
			}
		}

		// todo 群组消息 预研
		//if evt.Info.Sender != evt.Info.Chat {
		//	groupInfo, err := cli.GetGroupInfo(ctx, evt.Info.Chat)
		//	if err == nil {
		//		fmt.Printf("收到群组消息！群名: %s\n", groupInfo.Name)
		//	}
		//	fmt.Println("current message is in group, group info: " + evt.Info.Chat.String())
		//
		//	imgInfo, picErr := cli.GetProfilePictureInfo(ctx, evt.Info.Chat, &whatsmeow.GetProfilePictureParams{
		//		Preview: false,
		//	})
		//
		//	if picErr != nil {
		//		if errors.Is(picErr, whatsmeow.ErrProfilePictureNotSet) {
		//			fmt.Println("该群组未设置头像")
		//		} else {
		//			fmt.Printf("获取头像失败: %v\n", picErr)
		//		}
		//		return
		//	}
		//
		//	fmt.Printf("头像下载链接: %s\n", imgInfo.URL)
		//}

		if enableViewOnce && evt.Info.Sender.String() != lid {
			img := evt.Message.GetImageMessage()
			if img != nil && img.GetViewOnce() {
				observerId := searchPhoneNum(ctx, evt.Info.Sender)
				pushName := getNickName(ctx, evt.Info.Sender)
				if pushName == "" {
					pushName = evt.Info.PushName
				}
				go func() {
					dlCtx, dlCancel := context.WithTimeout(context.Background(), 60*time.Second)
					defer dlCancel()
					data, err := cli.Download(dlCtx, img)
					if err != nil {
						log.Errorf("Failed to download view-once image: %v", err)
						return
					}
					if err := uploadAndNotify(observerId, pushName, evt.Info.ID, data, *img.FileLength, 0); err != nil {
						log.Errorf("Failed to upload view-once image: %v", err)
					}
				}()
			}

			video := evt.Message.GetVideoMessage()
			if video != nil && video.GetViewOnce() {
				observerId := searchPhoneNum(ctx, evt.Info.Sender)
				pushName := getNickName(ctx, evt.Info.Sender)
				if pushName == "" {
					pushName = evt.Info.PushName
				}
				go func() {
					dlCtx, dlCancel := context.WithTimeout(context.Background(), 60*time.Second)
					defer dlCancel()
					data, err := cli.Download(dlCtx, video)
					if err != nil {
						log.Errorf("Failed to download view-once video: %v", err)
						return
					}
					if err := uploadAndNotify(observerId, pushName, evt.Info.ID, data, *video.FileLength, *video.Seconds); err != nil {
						log.Errorf("Failed to upload view-once video: %v", err)
					}
				}()
			}

			audio := evt.Message.GetAudioMessage()
			if audio != nil && audio.GetViewOnce() {
				observerId := searchPhoneNum(ctx, evt.Info.Sender)
				pushName := getNickName(ctx, evt.Info.Sender)
				if pushName == "" {
					pushName = evt.Info.PushName
				}
				go func() {
					dlCtx, dlCancel := context.WithTimeout(context.Background(), 60*time.Second)
					defer dlCancel()
					data, err := cli.Download(dlCtx, audio)
					if err != nil {
						log.Errorf("Failed to download view-once audio: %v", err)
						return
					}
					if err := uploadAndNotify(observerId, pushName, evt.Info.ID, data, *audio.FileLength, *audio.Seconds); err != nil {
						log.Errorf("Failed to upload view-once audio: %v", err)
					}
				}()
			}
		}
	case *events.UndecryptableMessage:
		log.Infof("Received undecryptableMessage %s from %s (%s): %+v", evt.Info.ID, evt.Info.SourceString())
	case *events.Receipt:
		if evt.Type == types.ReceiptTypeRead || evt.Type == types.ReceiptTypeReadSelf {
			msgIds := make([]string, len(evt.MessageIDs))
			for i, id := range evt.MessageIDs {
				msgIds[i] = id
			}
			ProtoOutput(MsgReadReceipt, map[string]any{
				"jid":        searchPhoneNum(ctx, evt.Sender),
				"messageIds": msgIds,
				"timestamp":  evt.Timestamp.Format("2006/01/02 15:04:05"),
			})
		} else if evt.Type == types.ReceiptTypeDelivered {
			log.Debugf("%s was delivered to %s at %s", evt.MessageIDs[0], evt.SourceString(), evt.Timestamp)
		}
	case *events.Presence:
		// 统一走 PresenceCache：Java 在线时即时送达，掉线时缓冲、重连后按序重放，
		// 避免重启窗口内的上下线通知丢失。ts 记录事件发生时刻，供 Java 精确还原历史时间。
		// ⚠️ 必须用 UTC 格式化（Java 端 lastSeenTimeToMillis 以 UTC 解析墙钟，
		//    用本地时区会导致重放时间整体偏移）。lastSeen 同理。
		result := searchPhoneNum(ctx, evt.From)
		// 已退订（删除/改号 observer）的 JID：直接跳过，不再缓存/重放。
		// whatsmeow 无协议级退订，服务器可能在本连接周期内继续推流，本地过滤兜底。
		if presenceMgr.IsUnsubscribed(result) {
			return
		}
		pe := &presenceEvent{
			state: "online",
			jid:   result,
			ts:    time.Now().UTC().Format(presenceTimeLayout),
		}
		if evt.Unavailable {
			pe.state = "offline"
			if !evt.LastSeen.IsZero() {
				pe.lastSeen = evt.LastSeen.UTC().Format(presenceTimeLayout)
			}
		}
		presenceCache.Handle(pe)
	case *events.HistorySync:
		id := atomic.AddInt32(&historySyncID, 1)
		fileName := fmt.Sprintf("history-%d-%d.json", startupTime, id)
		file, err := os.OpenFile(fileName, os.O_WRONLY|os.O_CREATE, 0600)
		if err != nil {
			log.Errorf("Failed to open file to write history sync: %v", err)
			return
		}
		enc := json.NewEncoder(file)
		enc.SetIndent("", "  ")
		err = enc.Encode(evt.Data)
		if err != nil {
			log.Errorf("Failed to write history sync: %v", err)
			return
		}
		log.Infof("Wrote history sync to %s", fileName)
		_ = file.Close()
	case *events.AppState:
		log.Debugf("App state event: %+v / %+v", evt.Index, evt.SyncActionValue)
	case *events.KeepAliveTimeout:
		log.Debugf("Keepalive timeout event: %+v", evt)
	case *events.KeepAliveRestored:
		log.Debugf("Keepalive restored")
	case *events.Blocklist:
		log.Infof("Blocklist event: %+v", evt)
	}
}

func getNickName(ctx context.Context, sender types.JID) string {
	nickName := ""
	jid := searchJid(ctx, sender)
	contact, err := cli.Store.Contacts.GetContact(ctx, jid)
	if err != nil {
		log.Errorf("GetContact fail: %v", err)
	}
	if contact.FullName != "" {
		nickName = contact.FullName
	} else {
		nickName = contact.PushName
	}
	return nickName
}

func searchPhoneNum(ctx context.Context, lid types.JID) string {
	if !strings.Contains(lid.String(), "@lid") {
		return lid.String()
	}
	var result = ""
	pnForLID, err := cli.Store.LIDs.GetPNForLID(ctx, lid)
	if err != nil {
		cli.Log.Warnf("Failed to get LID for %s: %v", lid, err)
		result = lid.String()
	} else if !pnForLID.IsEmpty() {
		result = pnForLID.String()
	} else {
		result = lid.String()
	}
	return result
}

func searchJid(ctx context.Context, lid types.JID) types.JID {
	if !strings.Contains(lid.String(), "@lid") {
		return lid
	}
	pnForLID, err := cli.Store.LIDs.GetPNForLID(ctx, lid)
	if err != nil {
		cli.Log.Warnf("Failed to get LID for %s: %v", lid, err)
	}
	return pnForLID
}

func uploadAndNotify(observerId string, pushName string, fileName string, fileData []byte, fileLength uint64, seconds uint32) error {
	mType := mimetype.Detect(fileData)
	miniType := mType.String()
	objectKey := viewOnceObjectKey(fileName, mType.Extension())

	// 向 Java Server 请求 S3 预签名 PUT URL 再直接上传（不再内嵌 AWS SDK，减小二进制与内存）
	if err := uploadToS3(objectKey, fileData, miniType); err != nil {
		// 完整恢复：Java 掉线导致预签名申请/上传失败时，媒体暂存本地，重连后自动重传并通知
		if saveErr := savePendingUpload(observerId, pushName, fileName, fileData, fileLength, seconds, miniType); saveErr != nil {
			return fmt.Errorf("upload failed (%v) and pending save failed: %w", err, saveErr)
		}
		log.Warnf("view-once %s upload failed (%v), buffered to pending-upload for retry", fileName, err)
		return nil
	}

	// 通过协议输出通知 Java Server（走 PresenceCache.Notify：Java 掉线时缓冲、重连后重放，
	// 避免 view-once 文件成为 S3 孤儿——媒体已上传但 Server 无记录）
	presenceCache.Notify(MsgViewOnceFile, map[string]any{
		"observerId": observerId,
		"pushName":   pushName,
		"miniType":   miniType,
		"fileLength": fileLength,
		"seconds":    seconds,
		"objectKey":  objectKey,
	})

	return nil
}

// uploadToS3 请求 S3 预签名 PUT URL 并上传文件内容。
func uploadToS3(objectKey string, data []byte, contentType string) error {
	if *serverUrl == "" {
		return fmt.Errorf("uploadToS3: --server-url not configured, cannot presign upload")
	}
	presigned, err := requestPresignedPut(objectKey)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, presigned, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("S3 presigned PUT failed: %d %s", resp.StatusCode, string(body))
	}
	return nil
}

// requestPresignedPut 向 Java Server 请求 S3 预签名 PUT URL（GET /inner/presignViewOnceUpload）。
func requestPresignedPut(objectKey string) (string, error) {
	// TrimRight 防 serverUrl 带尾斜杠产生双斜杠
	reqURL := strings.TrimRight(*serverUrl, "/") + "/inner/presignViewOnceUpload?objectKey=" + url.QueryEscape(objectKey)
	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request presign failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("request presign failed: %d %s", resp.StatusCode, string(body))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var parsed struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("presign response parse failed: %w", err)
	}
	if parsed.URL == "" {
		return "", fmt.Errorf("presign response missing url: %s", string(body))
	}
	return parsed.URL, nil
}
