// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"mime"
	"mytest2"
	"mytest2/bean"
	"mytest2/utils"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"C"
)

var log waLog.Logger

var logLevel = "INFO"
var debugLogs = flag.Bool("debug", false, "Enable debug logs?")
var dbDialect = flag.String("db-dialect", "sqlite3", "Database dialect (sqlite3 or postgres)")
var requestFullSync = flag.Bool("request-full-sync", false, "Request full (1 year) history sync when logging in?")
var pairRejectChan = make(chan bool, 1)
var startupTime = time.Now().Unix()

// --- 客户端特定结构体 ---

// ClientConfig 定义了单个客户端实例所需的配置
type ClientConfig struct {
	UserId     string // 用于日志和识别
	LoginPhone string
	DBAddress  string // 数据库地址，每个客户端必须独立
}

// ClientInstance 封装了一个 whatsmeow 客户端的所有状态
type ClientInstance struct {
	Config        ClientConfig
	Client        *whatsmeow.Client
	Log           waLog.Logger
	HistorySyncID atomic.Int32
}

type LoginRequest struct {
	UserId     string `json:"userId" binding:"required"`
	LoginPhone string `json:"loginPhone" binding:"required"`
	VerifyCode string `json:"verifyCode" binding:"required"`
}

type UserIdRequest struct {
	UserId string `json:"userId" binding:"required"`
}

type SubsObserversRequest struct {
	UserId      string   `json:"userId" binding:"required"`
	ObserverIds []string `json:"observerIds" binding:"required"`
}

var isClientReady = false
var mainLog = waLog.Stdout("Main", logLevel, true)

var waClientInfo bean.WaClientInfo

var globalManager *mytest2.ClientManager

var srv *http.Server

var parentCtx context.Context
var globalCancel context.CancelFunc
var waitGroup sync.WaitGroup

// --- 主要逻辑函数 ---

func main() {
	// 1. 读取配置文件
	dir, err := os.Getwd()
	mainLog.Infof("Current dir: %s", dir)
	content, err := os.ReadFile("WaClientInfo.json")
	if err != nil {
		mainLog.Errorf("Error reading WaClientInfo.json: %v\n", err)
		return
	}
	err = json.Unmarshal(content, &waClientInfo)
	if err != nil {
		mainLog.Errorf("Error unmarshalling WaClientInfo.json: %v\n", err)
		return
	}
	mainLog.Infof("WaClientInfo.json load success, user's file root path: %s.", waClientInfo.UserFile)

	// 2. 初始化 ClientManager 等全局变量
	mytest2.InitManager()
	globalManager = mytest2.GetManager()
	parentCtx, globalCancel = context.WithCancel(context.Background())
	mainLog.Infof("Global Variable init success.")

	// 3. 启动 HTTP API Server
	r := gin.Default()
	r.GET("/checkState", func(c *gin.Context) {
		var clientState string
		if isClientReady {
			clientState = "isReady"
		} else {
			clientState = "notReady"
		}
		c.JSON(http.StatusOK, gin.H{
			"status": clientState,
		})
	})

	r.POST("/login", handleLoginRequest)                           // 接收登录请求
	r.POST("/reLoginUsers", handleReLoginUsersRequest)             // 接收重新登录请求
	r.POST("/reconnect", handleReconnectRequest)                   // 接收重连请求
	r.POST("/disconnect", handleDisconnectRequest)                 // 接收退出请求
	r.POST("/subscribeObservers", handleSubscribeObserversRequest) // 接收订阅lastseen请求

	// 4. 启动服务
	// 将 Gin 放入 http.Server 中
	srv = &http.Server{
		Addr:    ":9090",
		Handler: r,
	}
	go func() {
		mainLog.Infof("Starting HTTP server on :9090")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			mainLog.Errorf("Failed to run server: %v", err)
		}
	}()
	//if err := r.Run(":9090"); err != nil {
	//	mainLog.Errorf("Failed to run server: %v", err)
	//}

	mainLog.Infof("WaClient is ready")
	isClientReady = true

	// 监听系统中断信号
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	// 阻塞直到接收到中断信号
	<-c
	mainLog.Infof("Interrupt received, starting graceful shutdown...")

	if err := srv.Shutdown(context.Background()); err != nil {
		mainLog.Errorf("Server Shutdown forced: %v", err)
	}

	// 通知所有客户端断开连接
	globalCancel()

	// 等待所有客户端 goroutine 退出
	waitGroup.Wait()
	mainLog.Infof("All clients shut down. Exiting.")
}

func handleLoginRequest(ginCtx *gin.Context) {
	var loginRequest LoginRequest

	// 尝试将请求体（通常是 JSON）绑定到结构体
	if err := ginCtx.ShouldBindJSON(&loginRequest); err != nil {
		// 如果绑定失败（例如 JSON 格式错误或缺少 required 字段）
		ginCtx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 绑定成功，现在可以使用 loginRequest.Username 和 loginRequest.Password
	mainLog.Infof("cli-[%s] Received login request", loginRequest.UserId)

	// ... 异步执行登录逻辑 ...
	//startClient(loginRequest)
	go func(req LoginRequest) {
		// 在后台执行，即使 handleLoginRequest 结束了，这里也会继续运行
		startClient(req)
	}(loginRequest)

	ginCtx.JSON(http.StatusOK, gin.H{"message": "Login request receive"})
}

func handleReLoginUsersRequest(ginCtx *gin.Context) {
	var loginRequests []LoginRequest

	// 尝试将请求体（通常是 JSON）绑定到结构体
	if err := ginCtx.ShouldBindJSON(&loginRequests); err != nil {
		// 如果绑定失败（例如 JSON 格式错误或缺少 required 字段）
		ginCtx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 绑定成功，现在可以使用 loginRequest.Username 和 loginRequest.Password
	mainLog.Infof("Received reLoginUsers request for user count: %d", len(loginRequests))

	// ... 异步执行登录逻辑 ...
	for _, req := range loginRequests {
		// 重要：将当前的 req 显式传给匿名函数
		go func(r LoginRequest) {
			startClient(r)
		}(req)
	}

	ginCtx.JSON(http.StatusOK, gin.H{
		"message": "Processing reLoginUsers request",
		"count":   len(loginRequests),
	})
}

func handleReconnectRequest(ginCtx *gin.Context) {
	var userIdRequest UserIdRequest

	// 尝试将请求体（通常是 JSON）绑定到结构体
	if err := ginCtx.ShouldBindJSON(&userIdRequest); err != nil {
		// 如果绑定失败（例如 JSON 格式错误或缺少 required 字段）
		ginCtx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	mainLog.Infof("cli-[%s] Received reconnect request", userIdRequest.UserId)

	// 重启客户端
	clientEntry, getResult := globalManager.GetClient(userIdRequest.UserId)
	client := clientEntry.Client
	clientLog := client.Log

	if !getResult {
		mainLog.Errorf("cli-[%s] handleReconnectRequest Could not get client", userIdRequest.UserId)
	}

	client.Disconnect()
	err := client.Connect()
	if err != nil {
		clientLog.Errorf("failed to connect: %v", err)
	}

	ginCtx.JSON(http.StatusOK, gin.H{"message": "reconnect request receive"})
}

func handleDisconnectRequest(ginCtx *gin.Context) {
	var userIdRequest UserIdRequest

	// 尝试将请求体（通常是 JSON）绑定到结构体
	if err := ginCtx.ShouldBindJSON(&userIdRequest); err != nil {
		// 如果绑定失败（例如 JSON 格式错误或缺少 required 字段）
		ginCtx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	mainLog.Infof("cli-[%s] Received disconnect request", userIdRequest.UserId)

	// 退出客户端
	globalManager.UnregisterAndStop(userIdRequest.UserId)

	ginCtx.JSON(http.StatusOK, gin.H{"message": "Disconnect request receive"})
}

func handleSubscribeObserversRequest(ginCtx *gin.Context) {
	var subsObserversRequest SubsObserversRequest

	// 尝试将请求体（通常是 JSON）绑定到结构体
	if err := ginCtx.ShouldBindJSON(&subsObserversRequest); err != nil {
		// 如果绑定失败（例如 JSON 格式错误或缺少 required 字段）
		ginCtx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	userId := subsObserversRequest.UserId
	mainLog.Infof("Received subscribe observers request for userId: %s\n", userId)

	clientEntry, getResult := globalManager.GetClient(subsObserversRequest.UserId)
	client := clientEntry.Client
	clientLog := client.Log
	if !getResult {
		mainLog.Errorf("cli-[%s] handleSubscribeObserversRequest Could not get client", subsObserversRequest.UserId)
	}
	ctx := context.Background()

	observerIds := subsObserversRequest.ObserverIds
	for _, observerId := range observerIds {

		// 查看 observer 是否存在
		resp, checkErr := client.IsOnWhatsApp(ctx, []string{"+" + observerId})
		if checkErr != nil {
			clientLog.Infof("Failed to check if users are on WhatsApp: %v", checkErr)
		} else {
			for _, item := range resp {
				var message = ""
				if item.VerifiedName != nil {
					message = fmt.Sprintf("%s: on whatsapp: %t, JID: %s, business name: %s", item.Query, item.IsIn, item.JID, item.VerifiedName.Details.GetVerifiedName())
				} else {
					message = fmt.Sprintf("%s: on whatsapp: %t, JID: %s", item.Query, item.IsIn, item.JID)
				}
				clientLog.Infof(message)
			}
		}

		// 订阅 observer 消息
		jid, ok := utils.ParseJID(utils.GetJidString(observerId))
		if !ok {
			return
		}
		subsErr := client.SubscribePresence(ctx, jid)
		if subsErr != nil {
			clientLog.Infof("Failed to SubscribePresence: %v", subsErr)
		}

		// 获取 observer 头像
		pic, err := client.GetProfilePictureInfo(ctx, jid, &whatsmeow.GetProfilePictureParams{
			Preview:     false,
			IsCommunity: false,
			ExistingID:  "",
		})
		var message = ""
		if err != nil {
			message = fmt.Sprintf("Failed to get avatar: %s", jid)
			//log.Errorf("Failed to get avatar: %v", err)
		} else if pic != nil {
			//log.Infof("Got avatar ID %s: %s", pic.ID, pic.URL)
			message = fmt.Sprintf("Got avatar success %s: %sAVATAREND", jid, pic.URL)
		} else {
			//log.Infof("No avatar found")
			message = fmt.Sprintf("Failed to get avatar: %s", jid)
		}

		clientLog.Infof(message)
	}

	ginCtx.JSON(http.StatusOK, gin.H{"message": "Subscribe observers request receive", "count": len(observerIds)})
}

func startClient(request LoginRequest) {
	flag.Parse()

	if *debugLogs {
		logLevel = "DEBUG"
	}

	// 设置全局同步配置（如果你希望所有客户端都应用这个同步策略）
	if *requestFullSync {
		store.DeviceProps.RequireFullSync = proto.Bool(false)
		store.DeviceProps.HistorySyncConfig = &waProto.DeviceProps_HistorySyncConfig{
			FullSyncDaysLimit:   proto.Uint32(3650),
			FullSyncSizeMbLimit: proto.Uint32(102400),
			StorageQuotaMb:      proto.Uint32(102400),
		}
	}

	var dbPath = waClientInfo.UserFile + "/" + request.VerifyCode + ".db"
	mainLog.Infof("cli-[%s] dbPath: %s", request.UserId, dbPath)

	// 定义所有客户端的配置
	// **重要：每个客户端必须有唯一的 DBAddress，以隔离会话数据**
	clientConfig := ClientConfig{
		UserId:     request.UserId,
		LoginPhone: request.LoginPhone,
		DBAddress:  "file:" + dbPath + "?_foreign_keys=on", // 独立的数据库文件
	}

	// 为这个特定的客户端创建一个子 Context
	// 当 parentCtx 取消，或者调用这个 subCancel 时，协程都会停止
	subCtx, subCancel := context.WithCancel(parentCtx)

	waitGroup.Add(1)
	// 为每个客户端启动一个独立的 Go routine
	// cfg, ctx 传参：ctx传参为了注明声明周期
	go func(cfg ClientConfig, ctx context.Context) {
		defer waitGroup.Done()
		err := runClientInstance(ctx, cfg, subCancel)
		if err != nil {
			mainLog.Errorf("cli-[%s] terminated with error: %v", cfg.UserId, err)
		} else {
			mainLog.Infof("cli-[%s] disconnected gracefully.", cfg.UserId)
		}
	}(clientConfig, subCtx)
}

func startAllClients() {
	flag.Parse()

	if *debugLogs {
		logLevel = "DEBUG"
	}

	// 设置全局同步配置（如果你希望所有客户端都应用这个同步策略）
	if *requestFullSync {
		store.DeviceProps.RequireFullSync = proto.Bool(false)
		store.DeviceProps.HistorySyncConfig = &waProto.DeviceProps_HistorySyncConfig{
			FullSyncDaysLimit:   proto.Uint32(3650),
			FullSyncSizeMbLimit: proto.Uint32(102400),
			StorageQuotaMb:      proto.Uint32(102400),
		}
	}

	mainLog := waLog.Stdout("Main", logLevel, true)
	parentCtx, globalCancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	// 为这个特定的客户端创建一个子 Context
	// 当 parentCtx 取消，或者调用这个 subCancel 时，协程都会停止
	subCtx, subCancel := context.WithCancel(parentCtx)

	// 定义所有客户端的配置
	// **重要：每个客户端必须有唯一的 DBAddress，以隔离会话数据**
	clientConfigs := []ClientConfig{
		{
			UserId:    "Client1",
			DBAddress: "file:mdtest1.db?_foreign_keys=on", // 独立的数据库文件
		},
		{
			UserId:    "Client1-1",
			DBAddress: "file:mdtest1-1.db?_foreign_keys=on", // 独立的数据库文件
		},
		{
			UserId:    "Client1-2",
			DBAddress: "file:mdtest1-2.db?_foreign_keys=on", // 独立的数据库文件
		},
		{
			UserId:    "Client1-3",
			DBAddress: "file:mdtest1-3.db?_foreign_keys=on", // 独立的数据库文件
		},
		{
			UserId:    "Client2",
			DBAddress: "file:mdtest2.db?_foreign_keys=on", // 另一个独立的数据库文件
		},
		{
			UserId:    "Client2-1",
			DBAddress: "file:mdtest2-1.db?_foreign_keys=on", // 另一个独立的数据库文件
		},
		{
			UserId:    "Client2-2",
			DBAddress: "file:mdtest2-2.db?_foreign_keys=on", // 另一个独立的数据库文件
		},
		{
			UserId:    "Client2-3",
			DBAddress: "file:mdtest2-3.db?_foreign_keys=on", // 另一个独立的数据库文件
		},
		// 可以在这里添加更多客户端配置
	}

	for _, config := range clientConfigs {
		wg.Add(1)
		// 为每个客户端启动一个独立的 Go routine
		go func(cfg ClientConfig, ctx context.Context) {
			defer wg.Done()
			err := runClientInstance(ctx, cfg, subCancel)
			if err != nil {
				mainLog.Errorf("[%s] Client terminated with error: %v", cfg.UserId, err)
			} else {
				mainLog.Infof("[%s] Client disconnected gracefully.", cfg.UserId)
			}
		}(config, subCtx)
	}

	// 监听系统中断信号
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	// 阻塞直到接收到中断信号
	<-c
	mainLog.Infof("Interrupt received, starting graceful shutdown...")

	// 通知所有客户端断开连接
	globalCancel()

	// 等待所有客户端 goroutine 退出
	wg.Wait()
	mainLog.Infof("All clients shut down. Exiting.")
}

// runClientInstance 初始化并运行单个 whatsmeow 客户端实例
func runClientInstance(ctx context.Context, config ClientConfig, cancel context.CancelFunc) error {
	userId := config.UserId
	clientLog := waLog.Stdout("Client-"+userId, logLevel, true)
	dbLog := waLog.Stdout("DB-"+userId, logLevel, true)

	// 1. 初始化数据库存储
	storeContainer, err := sqlstore.New(ctx, *dbDialect, config.DBAddress, dbLog)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}

	// 2. 获取设备信息
	// 这里使用 GetFirstDevice。如果你需要通过 JID 查找特定设备，需要使用 storeContainer.GetDevice(ctx, config.JID)
	device, err := storeContainer.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("failed to get device: %w", err)
	}

	// 3. 创建客户端实例
	cli := whatsmeow.NewClient(device, clientLog)
	// 将其加入全局管理
	globalManager.RegisterClient(userId, cli, cancel)

	instance := &ClientInstance{
		Config: config,
		Client: cli,
		Log:    clientLog,
	}

	// 4. 注册事件处理器
	cli.AddEventHandler(func(rawEvt interface{}) {
		handleEvent(instance, rawEvt)
	})

	// 5. 连接到 WhatsApp 服务器
	err = cli.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	// 5.1 发送链接码
	linkingCode, err := cli.PairPhone(ctx, config.LoginPhone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		panic(err)
	}
	clientLog.Infof("Linking code: %s", linkingCode)

	// 6. 阻塞，等待连接结束或上下文取消
	<-ctx.Done()

	// 7. 优雅断开连接
	_, timeoutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer timeoutCancel()

	// whatsmeow.Client.Disconnect() 是非阻塞的，它会触发 StreamReplaced 事件
	// 在你的原始 handler 中，StreamReplaced 会导致 os.Exit(0)，这在多客户端中是不对的
	// 我们修改 handler 来适应多客户端环境
	clientLog.Infof("Client [%s] attempting to disconnect...", userId)
	cli.Disconnect()

	// 确保客户端有时间断开连接，或者等待 StreamReplaced 事件被处理
	// 简化的处理是等待一段时间，或者依赖外部 API 来确认状态
	time.Sleep(500 * time.Millisecond) // 给 Disconnect 一点时间

	return nil
}

// handleEvent 是针对特定客户端实例的事件处理器
func handleEvent(inst *ClientInstance, rawEvt interface{}) {
	ctx := context.Background() // 使用新的上下文进行事件内的操作
	log := inst.Log
	cli := inst.Client

	// --- 原始 handler 逻辑被复制并修改以使用 inst.Client 和 inst.Log ---
	switch evt := rawEvt.(type) {
	case *events.AppStateSyncComplete:
		if len(cli.Store.PushName) > 0 && evt.Name == appstate.WAPatchCriticalBlock {
			err := cli.SendPresence(ctx, types.PresenceAvailable)
			if err != nil {
				log.Warnf("Failed to send available presence: %v", err)
			} else {
				log.Infof("Marked self as available")
			}
		}
	case *events.Connected, *events.PushNameSetting:
		if len(cli.Store.PushName) == 0 {
			return
		}
		err := cli.SendPresence(ctx, types.PresenceAvailable)
		if err != nil {
			log.Warnf("Failed to send available presence: %v", err)
		} else {
			log.Infof("Marked self as available")
		}
	case *events.StreamReplaced:
		// 在多客户端环境中，StreamReplaced 仅代表此客户端的连接被替换/关闭
		// 不应调用 os.Exit(0)，而是让 runClientInstance 退出其 goroutine。
		log.Warnf("Client [%s] stream was replaced. Connection closed.", inst.Config.UserId)
		// 我们可以通过取消 runClientInstance 内部的 Context 来实现优雅退出
		// 但在这里依赖 runClientInstance 外部的 Context 取消。
	case *events.Message:
		metaParts := []string{fmt.Sprintf("pushname: %s", evt.Info.PushName), fmt.Sprintf("timestamp: %s", evt.Info.Timestamp)}
		// ... 原始 Message 事件处理逻辑 ...
		log.Infof("Received message %s from %s (%s): %+v", evt.Info.ID, evt.Info.SourceString(), strings.Join(metaParts, ", "), evt.Message)

		// ... 下载图片/解密投票/反应的逻辑 ...
		img := evt.Message.GetImageMessage()
		if img != nil {
			data, err := cli.Download(ctx, img)
			if err != nil {
				log.Errorf("Failed to download image: %v", err)
				return
			}
			exts, _ := mime.ExtensionsByType(img.GetMimetype())
			path := fmt.Sprintf("%s-%s%s", inst.Config.UserId, evt.Info.ID, exts[0])
			err = os.WriteFile(path, data, 0600)
			if err != nil {
				log.Errorf("Failed to save image: %v", err)
				return
			}
			log.Infof("Saved image in message to %s", path)
		}

	case *events.Receipt:
		if evt.Type == types.ReceiptTypeRead || evt.Type == types.ReceiptTypeReadSelf {
			log.Infof("%v was read by %s at %s", evt.MessageIDs, evt.SourceString(), evt.Timestamp)
		}
		//else if evt.Type == types.ReceiptTypeDelivered {
		//	log.Infof("%s was delivered to %s at %s", evt.MessageIDs[0], evt.SourceString(), evt.Timestamp)
		//}
	case *events.Presence:
		// ... 原始 Presence 事件处理逻辑 ...
		var result = ""
		pnForLID, err := cli.Store.LIDs.GetPNForLID(ctx, evt.From)
		if err != nil {
			cli.Log.Warnf("Failed to get LID for %s: %v", evt.From, err)
			result = evt.From.String()
		} else if !pnForLID.IsEmpty() {
			result = pnForLID.String()
		} else {
			result = evt.From.String()
		}

		var message = ""
		if evt.Unavailable {
			if evt.LastSeen.IsZero() {
				message = fmt.Sprintf("offline: %s", result)
			} else {
				message = fmt.Sprintf("offline: %s (last seen: %s)", result, evt.LastSeen)
			}
		} else {
			message = fmt.Sprintf("online: %s", result)
		}
		log.Infof(message)
	case *events.HistorySync:
		id := inst.HistorySyncID.Add(1)
		fileName := fmt.Sprintf("%s-history-%d-%d.json", inst.Config.UserId, startupTime, id)
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
