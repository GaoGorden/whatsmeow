// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
	_ "github.com/mattn/go-sqlite3"
	//"github.com/mdp/qrterminal/v3"
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
	"gopkg.in/yaml.v3"

	// amazon s3
	"bytes"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/gabriel-vasile/mimetype"

	"C"
)

var cli *whatsmeow.Client
var log waLog.Logger
var s3Client *s3.Client

var logLevel = "INFO"
var debugLogs = flag.Bool("debug", false, "Enable debug logs?")
var dbDialect = flag.String("db-dialect", "sqlite3", "Database dialect (sqlite3 or postgres)")
var dbAddress = flag.String("db-address", "file:mdtest.db?_foreign_keys=on", "Database address")
var requestFullSync = flag.Bool("request-full-sync", false, "Request full (1 year) history sync when logging in?")
var pairRejectChan = make(chan bool, 1)

type Amazon struct {
	KEY    string `yaml:"key"`
	SECRET string `yaml:"secret"`
	REGION string `yaml:"region"`
}

//go:embed amazon.yaml
var configFile embed.FS
var amazon Amazon

var enableViewOnce = false

var device *store.Device
var lid = ""
var presenceMgr *PresenceManager

func main() {

	data, readErr := configFile.ReadFile("amazon.yaml")
	if readErr != nil {
		log.Errorf("can not read amazon.yaml: %v", readErr)
	}

	marshalErr := yaml.Unmarshal(data, &amazon)
	if marshalErr != nil {
		log.Errorf("can not marshal amazon.yaml: %v", marshalErr)
	}

	staticProvider := credentials.NewStaticCredentialsProvider(
		amazon.KEY,
		amazon.SECRET,
		"",
	)
	cfg, _ := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(amazon.REGION),
		config.WithCredentialsProvider(staticProvider),
	)
	s3Client = s3.NewFromConfig(cfg)

	waBinary.IndentXML = true
	//flag.Parse()

	if *debugLogs {
		logLevel = "DEBUG"
	}
	if *requestFullSync {
		store.DeviceProps.RequireFullSync = proto.Bool(true)
		store.DeviceProps.HistorySyncConfig = &waProto.DeviceProps_HistorySyncConfig{
			FullSyncDaysLimit:   proto.Uint32(3650),
			FullSyncSizeMbLimit: proto.Uint32(102400),
			StorageQuotaMb:      proto.Uint32(102400),
		}
	}
	log = waLog.Stdout("Main", logLevel, true)

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

	c := make(chan os.Signal, 1)
	input := make(chan string)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
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
	for {
		select {
		case <-c:
			log.Infof("Interrupt received, exiting")
			cli.Disconnect()
			return
		case cmd := <-input:
			if len(cmd) == 0 {
				log.Infof("Stdin closed, exiting")
				cli.Disconnect()
				return
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
		}
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
		result := searchPhoneNum(ctx, evt.From)

		if evt.Unavailable {
			data := map[string]any{
				"state": "offline",
				"jid":   result,
			}
			if !evt.LastSeen.IsZero() {
				data["lastSeen"] = evt.LastSeen.Format("2006/01/02 15:04:05")
			}
			ProtoOutput(MsgPresence, data)
		} else {
			ProtoOutput(MsgPresence, map[string]any{
				"state": "online",
				"jid":   result,
			})
		}
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mType := mimetype.Detect(fileData)
	miniType := mType.String()
	objectKey := "whatsapp/view-once/" + cli.Store.GetJID().String() + "/" + fileName + mType.Extension()
	bucket := "view-once"

	_, err := s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &bucket,
		Key:    &objectKey,
		Body:   bytes.NewReader(fileData),
	})
	if err != nil {
		return err
	}

	// 3. 通过协议输出通知 Java Server
	ProtoOutput(MsgViewOnceFile, map[string]any{
		"observerId": observerId,
		"pushName":   pushName,
		"miniType":   miniType,
		"fileLength": fileLength,
		"seconds":    seconds,
		"objectKey":  objectKey,
	})

	return nil
}
