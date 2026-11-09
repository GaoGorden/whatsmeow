package main

// 消息恢复（Recover）采集。
//
// 与 Status 的差别：
//   - 不按 observer 过滤，**所有一对一单聊**都采集（群聊 / status@broadcast / newsletter 跳过）；
//   - 普通消息只进 3 天缓冲（Java 侧不可见）；撤回 / 编辑 / view-once 才升级为 permanent；
//   - 必须处理 ProtocolMessage(REVOKE / MESSAGE_EDIT)，且要在通用 receivedMessage 之前分流。
//
// 三条纪律：
//  1. ProtocolMessage 的 REVOKE / EDIT 必须在通用消息处理之前 return，否则编辑事件会被
//     whatsmeow 规范化成「原消息 ID + 新内容」并当普通消息归档，**原文被静默覆盖**。
//  2. 媒体全部下载并上传（产品定案不设上限），但只对「点过开始」的用户开启；
//     每用户 1 个 worker、队列 256、磁盘水位兜底，避免全量单聊把节点打爆。
//  3. 上报走 outbox（`recover-outbox/{ackId}.json`），收到 Java 的 `recoverack <ackId>` 才删。
//     同一 messageId 可能有 edit/revoke 多个事件，**ackId 必须带 eventType + eventAt**，
//     不能只用 messageId（否则后一个事件会覆盖前一个的 outbox 记录）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const (
	recoverWorkerConcurrency = 1
	recoverQueueCap          = 256
	recoverDedupMax          = 5000
	recoverDownloadTimeout   = 120 * time.Second
	recoverUploadTimeout     = 120 * time.Second
	recoverReplayMinGap      = 5 * time.Minute
)

var (
	// recoverEnabled 是「用户点过开始 + 服务端下发 enable-recover」的运行时开关。
	// 与 enableViewOnce 的裸 bool 不同：必须 atomic（命令 goroutine 写、事件 goroutine 读）。
	recoverEnabled atomic.Bool

	recoverSeen = newStatusSeenSet(recoverDedupMax)

	// recoverQueue: 待下载/上传的媒体任务；文字与撤回/编辑直接发布，不入队。
	recoverQueue = make(chan *recoverJob, recoverQueueCap)

	recoverDropped atomic.Int64

	// recoverPnFallbackLogged: PN 解析失败的告警只打前 20 条，避免 LID 联系人刷屏。
	recoverPnFallbackLogged atomic.Int32
)

var recoverOutboxDir = "recover-outbox"

// recoverOutboxMax: outbox 条数上限，超出丢最旧并记 ERROR。用 var 便于单测调小。
var recoverOutboxMax = 5000

var recoverReplayMu sync.Mutex

var recoverReplayLast = struct {
	mu   sync.Mutex
	last map[string]time.Time
}{last: make(map[string]time.Time)}

// ---------------------------------------------------------------------------
// 事件入口
// ---------------------------------------------------------------------------

// handleRecoverMessage 处理一条普通（非 status@broadcast）消息。
//
// 返回值 = 是否已被本函数「消费」：
//   - true：这是 REVOKE / MESSAGE_EDIT 等协议消息，调用方必须 **直接 return**，
//     不能再走通用 receivedMessage（否则编辑会被当普通消息、原文被覆盖）；
//   - false：普通/view-once 消息，调用方继续走通用处理（在线活动推断仍需要）。
func handleRecoverMessage(evt *events.Message) bool {
	if !recoverEnabled.Load() || evt == nil || evt.Message == nil {
		return false
	}
	// 群聊/自己发的：不采集，但也不能消费——通用 receivedMessage 仍要跑
	if evt.Info.IsGroup || evt.Info.IsFromMe {
		return false
	}
	// 动态（status@broadcast）已在上层分支处理；newsletter/channel 不是单聊
	if evt.Info.Chat == types.StatusBroadcastJID ||
		strings.Contains(strings.ToLower(evt.Info.Chat.String()), "newsletter") {
		return false
	}

	ctx := context.Background()
	eventAt := evt.Info.Timestamp.UnixMilli()
	if eventAt <= 0 {
		eventAt = time.Now().UnixMilli()
	}

	// ① ProtocolMessage 必须最先分流（RawMessage 才是未被规范化前的原文）
	if pm := evt.RawMessage.GetProtocolMessage(); pm != nil {
		switch pm.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			originalID := pm.GetKey().GetID()
			if originalID != "" {
				ackID := recoverAckID(originalID, "revoke", eventAt)
				if recoverSeen.mark(ackID) {
					publishRecover(map[string]any{
						"ackId":     ackID,
						"messageId": originalID,
						"chatId":    recoverChatID(ctx, evt),
						"chatName":  recoverChatName(ctx, evt),
						"senderJid": evt.Info.Sender.String(),
						"eventType": "revoke",
						"kind":      "",
						"text":      "",
						"postedAt":  int64(0),
						"eventAt":   eventAt,
					})
				}
			}
			return true

		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			originalID := pm.GetKey().GetID()
			if originalID != "" {
				text, kind := recoverEditedTextAndKind(pm.GetEditedMessage())
				logRecoverEdit("protocol", originalID, kind, text)
				ackID := recoverAckID(originalID, "edit", eventAt)
				if recoverSeen.mark(ackID) {
					publishRecover(map[string]any{
						"ackId":        ackID,
						"messageId":    originalID,
						"chatId":       recoverChatID(ctx, evt),
						"chatName":     recoverChatName(ctx, evt),
						"senderJid":    evt.Info.Sender.String(),
						"eventType":    "edit",
						"kind":         kind,
						"text":         text,
						"originalText": "",
						"postedAt":     int64(0),
						"eventAt":      eventAt,
					})
				}
			}
			return true

		default:
			// 其它协议消息（临时设置、历史同步等）没有可归档内容；
			// 消费掉，避免落到通用 receivedMessage 被当成一条真实消息。
			return true
		}
	}

	// ② 新加密编辑路径：WhatsApp 新版客户端把编辑放在 SecretEncryptedMessage 里。
	//    原先只打 WARN 就放行，编辑会落到 normal → Server 写 buffered → 列表不可见
	//    （2026-11-05 本地联调报障）。这里用 whatsmeow 自带的 `DecryptSecretEncryptedMessage`
	//    解密（依赖原消息的 message secret，`storeMessageSecret` 在收原消息/历史同步时已落库），
	//    再按 `edit` 事件上报；`messageId` 取 `targetMessageKey.id`（原消息 ID）。
	if enc := evt.Message.GetSecretEncryptedMessage(); enc != nil {
		if enc.GetSecretEncType() != waE2E.SecretEncryptedMessage_MESSAGE_EDIT {
			log.Warnf("recover: SecretEncryptedMessage type %s observed (message %s), not handled",
				enc.GetSecretEncType().String(), evt.Info.ID)
			return false
		}
		targetID := enc.GetTargetMessageKey().GetID()
		if targetID == "" {
			log.Warnf("recover: secret edit missing target key (message %s)", evt.Info.ID)
			return true
		}
		if cli == nil {
			log.Warnf("recover: secret edit without cli (message %s)", evt.Info.ID)
			return true
		}
		decrypted, err := cli.DecryptSecretEncryptedMessage(ctx, evt)
		if err != nil {
			// 原消息 secret 缺失/解密失败：消费掉，避免落成一条 normal buffered；
			// 需要用户重新收一条新消息后再编辑才可能拿到 secret。
			log.Warnf("recover: secret edit decrypt failed (message %s, target %s): %v",
				evt.Info.ID, targetID, err)
			return true
		}
		text, kind := recoverEditedTextAndKind(decrypted)
		logRecoverEdit("secret", targetID, kind, text)
		if text == "" && kind == "unknown" {
			// 只打结构、不打内容：若仍出现，说明解密后的 payload 还有未覆盖的包装。
			log.Warnf("recover: secret edit content unknown (message %s, target %s): "+
				"conv=%d ext=%v image=%v video=%v doc=%v",
				evt.Info.ID, targetID,
				len(decrypted.GetConversation()),
				decrypted.GetExtendedTextMessage() != nil,
				decrypted.GetImageMessage() != nil,
				decrypted.GetVideoMessage() != nil,
				decrypted.GetDocumentMessage() != nil)
		}
		ackID := recoverAckID(targetID, "edit", eventAt)
		if recoverSeen.mark(ackID) {
			publishRecover(map[string]any{
				"ackId":        ackID,
				"messageId":    targetID,
				"chatId":       recoverChatID(ctx, evt),
				"chatName":     recoverChatName(ctx, evt),
				"senderJid":    evt.Info.Sender.String(),
				"eventType":    "edit",
				"kind":         kind,
				"text":         text,
				"originalText": "",
				"postedAt":     int64(0),
				"eventAt":      eventAt,
			})
		}
		return true
	}

	// ③ EditedMessage 包裹形态（新客户端）：`UnwrapRaw` 已把 evt.Message 解到编辑后的内容，
	//    并置 `IsEdit=true`；这种事件的 RawMessage 里**没有 ProtocolMessage**，
	//    上面的 REVOKE/MESSAGE_EDIT 分支抓不到。若不在这里处理，会落到下面的 normal 分支，
	//    Server 写成 `status='buffered'` → 列表不可见（2026-11-05 本地联调报障）。
	if evt.IsEdit {
		if evt.Info.ID != "" {
			text, kind := recoverEditedTextAndKind(evt.Message)
			logRecoverEdit("isEdit", evt.Info.ID, kind, text)
			if text == "" && kind == "unknown" {
				log.Warnf("recover: IsEdit content unknown (message %s)", evt.Info.ID)
			}
			ackID := recoverAckID(evt.Info.ID, "edit", eventAt)
			if recoverSeen.mark(ackID) {
				publishRecover(map[string]any{
					"ackId":        ackID,
					"messageId":    evt.Info.ID,
					"chatId":       recoverChatID(ctx, evt),
					"chatName":     recoverChatName(ctx, evt),
					"senderJid":    evt.Info.Sender.String(),
					"eventType":    "edit",
					"kind":         kind,
					"text":         text,
					"originalText": "",
					"postedAt":     int64(0),
					"eventAt":      eventAt,
				})
			}
		}
		return true
	}

	if evt.Info.ID == "" {
		return false
	}

	isViewOnce := recoverIsViewOnce(evt)
	eventType := "normal"
	if isViewOnce {
		eventType = "view_once"
	}

	postedAt := evt.Info.Timestamp.UnixMilli()
	if postedAt <= 0 {
		postedAt = time.Now().UnixMilli()
	}

	// 普通/view-once 的 ackId 用 postedAt（稳定），重复投递会被同一 key 吃掉
	ackID := recoverAckID(evt.Info.ID, eventType, postedAt)
	if !recoverSeen.mark(ackID) {
		return false
	}

	kind, media, mime, fileName, size, seconds, text := recoverClassify(evt.Message)
	if kind == "" {
		// 无内容/未知类型：不归档；通用 receivedMessage 仍要跑
		return false
	}

	job := &recoverJob{
		messageID: evt.Info.ID,
		ackID:     ackID,
		chatID:    recoverChatID(ctx, evt),
		chatName:  recoverChatName(ctx, evt),
		senderJid: evt.Info.Sender.String(),
		eventType: eventType,
		kind:      kind,
		text:      text,
		mime:      mime,
		fileName:  fileName,
		size:      size,
		seconds:   seconds,
		postedAt:  postedAt,
		eventAt:   eventAt,
		media:     media,
	}
	if job.chatID == "" {
		// 解析不出号码就不归档：分组键为空会把所有未知联系人串成一行
		log.Warnf("recover %s: blank chatId, skip (sender %s)", evt.Info.ID, evt.Info.Sender.String())
		return false
	}
	if media != nil {
		job.objectKey = recoverObjectKey(evt.Info.ID, mime, fileName)
	}
	enqueueRecoverJob(job)
	return false
}

// recoverIsViewOnce 兼容新旧三种包装与「媒体自身 GetViewOnce()」。
func recoverIsViewOnce(evt *events.Message) bool {
	if evt.IsViewOnce || evt.IsViewOnceV2 || evt.IsViewOnceV2Extension {
		return true
	}
	m := evt.Message
	if i := m.GetImageMessage(); i != nil && i.GetViewOnce() {
		return true
	}
	if v := m.GetVideoMessage(); v != nil && v.GetViewOnce() {
		return true
	}
	if a := m.GetAudioMessage(); a != nil && a.GetViewOnce() {
		return true
	}
	// PTV（圆形视频）的 view-once 形态：GetPtvMessage() 返回 *VideoMessage
	if p := m.GetPtvMessage(); p != nil && p.GetViewOnce() {
		return true
	}
	return false
}

// recoverClassify 返回 (kind, media, mime, fileName, size, seconds, text)。
// kind 为空表示「没有可归档内容」。
func recoverClassify(m *waE2E.Message) (string, whatsmeow.DownloadableMessage, string, string, uint64, uint32, string) {
	if m == nil {
		return "", nil, "", "", 0, 0, ""
	}
	if text := m.GetConversation(); text != "" {
		return "text", nil, "", "", 0, 0, text
	}
	if e := m.GetExtendedTextMessage(); e != nil {
		return "text", nil, "", "", 0, 0, e.GetText()
	}
	if i := m.GetImageMessage(); i != nil {
		return "image", i, i.GetMimetype(), "", i.GetFileLength(), 0, i.GetCaption()
	}
	if v := m.GetVideoMessage(); v != nil {
		return "video", v, v.GetMimetype(), "", v.GetFileLength(), v.GetSeconds(), v.GetCaption()
	}
	if a := m.GetAudioMessage(); a != nil {
		return "audio", a, a.GetMimetype(), "", a.GetFileLength(), a.GetSeconds(), ""
	}
	if d := m.GetDocumentMessage(); d != nil {
		return "document", d, d.GetMimetype(), d.GetFileName(), d.GetFileLength(), 0, d.GetCaption()
	}
	if s := m.GetStickerMessage(); s != nil {
		return "sticker", s, s.GetMimetype(), "", s.GetFileLength(), 0, ""
	}
	if p := m.GetPtvMessage(); p != nil {
		return "ptv", p, p.GetMimetype(), "", p.GetFileLength(), p.GetSeconds(), p.GetCaption()
	}
	return "", nil, "", "", 0, 0, ""
}

// recoverEditedTextAndKind 从 MESSAGE_EDIT 的新内容里取文本与类型。
//
// ⚠️ 编辑内容可能被包在多层 Message 里（ProtocolMessage.EditedMessage /
// EditedMessage / DeviceSentMessage / Ephemeral / DocumentWithCaption ...）。
// 2026-11-05 实测 `SecretEncryptedMessage(MESSAGE_EDIT)` 解密后的 payload 就不是
// 直接的 Conversation，必须逐层解开，否则会返回 `("", "unknown")`，
// Server 把 text_content 覆盖成空串，列表摘要变空白。
// logRecoverEdit 打一条编辑事件摘要（三个分支共用）。
//
// 只打**长度**不打正文：正文属于用户隐私，日志里有长度+kind 就足以判断
// 「是解析失败（len=0 + kind=unknown）还是正常取到」。
// 排查「编辑没有推送」时，这三条 INFO 与 Server 的 `edit applied` / `edit redelivered`
// 是同一事件在两端的一对锚点。
func logRecoverEdit(form, targetID, kind, text string) {
	log.Infof("recover: edit event form=%s target=%s kind=%s textLen=%d",
		form, targetID, kind, len(text))
}

func recoverEditedTextAndKind(m *waE2E.Message) (string, string) {
	m = recoverUnwrapMessage(m)
	if m == nil {
		return "", "text"
	}
	if text := m.GetConversation(); text != "" {
		return text, "text"
	}
	if e := m.GetExtendedTextMessage(); e != nil {
		if text := e.GetText(); text != "" {
			return text, "text"
		}
	}
	if i := m.GetImageMessage(); i != nil {
		return i.GetCaption(), "image"
	}
	if v := m.GetVideoMessage(); v != nil {
		return v.GetCaption(), "video"
	}
	if d := m.GetDocumentMessage(); d != nil {
		return d.GetCaption(), "document"
	}
	if a := m.GetAudioMessage(); a != nil {
		return "", "audio"
	}
	return "", "unknown"
}

// recoverUnwrapMessage 逐层解开编辑 payload 的外层包裹，返回最内层的内容 Message。
func recoverUnwrapMessage(m *waE2E.Message) *waE2E.Message {
	for i := 0; i < 6 && m != nil; i++ {
		switch {
		case m.GetProtocolMessage().GetEditedMessage() != nil:
			m = m.GetProtocolMessage().GetEditedMessage()
		case m.GetEditedMessage().GetMessage() != nil:
			m = m.GetEditedMessage().GetMessage()
		case m.GetDeviceSentMessage().GetMessage() != nil:
			m = m.GetDeviceSentMessage().GetMessage()
		case m.GetEphemeralMessage().GetMessage() != nil:
			m = m.GetEphemeralMessage().GetMessage()
		case m.GetDocumentWithCaptionMessage().GetMessage() != nil:
			m = m.GetDocumentWithCaptionMessage().GetMessage()
		case m.GetViewOnceMessage().GetMessage() != nil:
			m = m.GetViewOnceMessage().GetMessage()
		case m.GetViewOnceMessageV2().GetMessage() != nil:
			m = m.GetViewOnceMessageV2().GetMessage()
		case m.GetViewOnceMessageV2Extension().GetMessage() != nil:
			m = m.GetViewOnceMessageV2Extension().GetMessage()
		default:
			return m
		}
	}
	return m
}

func recoverChatID(ctx context.Context, evt *events.Message) string {
	id := searchPhoneNum(ctx, evt.Info.Sender)
	if id == "" {
		// LID 尚未映射到 PN：退回 JID 的 user 部分。同一个人的 LID 与 PN 可能被拆成两行，
		// 属于计划里已知的 LID 风险；先限流打 WARN 观测占比。
		id = evt.Info.Sender.User
		if n := recoverPnFallbackLogged.Add(1); n <= 20 {
			log.Warnf("recover: failed to resolve PN for %s, fallback to %s (%d so far)",
				evt.Info.Sender.String(), id, n)
		}
	}
	return id
}

func recoverChatName(ctx context.Context, evt *events.Message) string {
	// 先取事件自带的 push name（零成本），为空才去查通讯录/昵称表。
	// 全量单聊下每条消息都查一次 getNickName 会明显放大开销。
	name := evt.Info.PushName
	if name == "" {
		name = getNickName(ctx, evt.Info.Sender)
	}
	if name == "" {
		name = recoverChatID(ctx, evt)
	}
	return name
}

// ---------------------------------------------------------------------------
// 任务 / 队列 / worker
// ---------------------------------------------------------------------------

type recoverJob struct {
	messageID    string
	ackID        string
	chatID       string
	chatName     string
	senderJid    string
	eventType    string
	kind         string
	text         string
	mime         string
	fileName     string
	size         uint64
	seconds      uint32
	postedAt     int64
	eventAt      int64
	objectKey    string
	media        whatsmeow.DownloadableMessage
	mediaSkipped bool
}

func (j *recoverJob) payload(objectKey string) map[string]any {
	return map[string]any{
		"ackId":        j.ackID,
		"messageId":    j.messageID,
		"chatId":       j.chatID,
		"chatName":     j.chatName,
		"senderJid":    j.senderJid,
		"eventType":    j.eventType,
		"kind":         j.kind,
		"text":         j.text,
		"originalText": "",
		"objectKey":    objectKey,
		"mime":         j.mime,
		"size":         j.size,
		"seconds":      j.seconds,
		"fileName":     j.fileName,
		"mediaSkipped": j.mediaSkipped,
		"postedAt":     j.postedAt,
		"eventAt":      j.eventAt,
	}
}

func startRecoverWorkers() {
	for i := 0; i < recoverWorkerConcurrency; i++ {
		go func() {
			for job := range recoverQueue {
				processRecoverJob(job)
			}
		}()
	}
}

func enqueueRecoverJob(job *recoverJob) {
	for {
		select {
		case recoverQueue <- job:
			return
		default:
		}
		select {
		case dropped := <-recoverQueue:
			recoverDropped.Add(1)
			log.Errorf("recover queue full, dropped oldest job %s (event %s)", dropped.messageID, dropped.eventType)
		default:
		}
	}
}

func processRecoverJob(job *recoverJob) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("recover job %s panicked: %v", job.messageID, r)
		}
	}()

	if job.media == nil {
		publishRecover(job.payload(""))
		return
	}
	if !recoverWaitForDisk(job.messageID) {
		log.Errorf("recover %s: disk still below watermark after %s, metadata only", job.messageID, statusDiskPauseLimit)
		job.mediaSkipped = true
		publishRecover(job.payload(""))
		return
	}

	dlCtx, cancel := context.WithTimeout(context.Background(), recoverDownloadTimeout)
	tmpPath, err := downloadToTemp(dlCtx, job.media, job.messageID)
	cancel()
	if err != nil {
		// 媒体拿不到不影响「这是一次撤回/编辑/view-once」的事实：仍上报元数据
		log.Errorf("recover %s: download failed: %v", job.messageID, err)
		job.mediaSkipped = true
		publishRecover(job.payload(""))
		return
	}
	defer os.Remove(tmpPath)

	if err := uploadFileToS3(presignRecoverUploadPath, job.objectKey, tmpPath, job.mime, recoverUploadTimeout); err != nil {
		// 媒体已落盘、只是没传上去（多为 Java 掉线）→ 暂存，重连后补传并上报
		if saveErr := savePendingRecover(job, tmpPath); saveErr != nil {
			log.Errorf("recover %s: upload failed (%v) and pending save failed: %v", job.messageID, err, saveErr)
			job.mediaSkipped = true
			publishRecover(job.payload(""))
			return
		}
		log.Warnf("recover %s: upload failed (%v), buffered to pending-upload for retry", job.messageID, err)
		return
	}

	publishRecover(job.payload(job.objectKey))
}

func recoverWaitForDisk(messageID string) bool {
	if !statusDiskLow() {
		return true
	}
	log.Warnf("recover %s: disk below watermark, pausing download", messageID)
	pausedAt := time.Now()
	for statusDiskLow() {
		if time.Since(pausedAt) > statusDiskPauseLimit {
			return false
		}
		time.Sleep(15 * time.Second)
	}
	return true
}

// ---------------------------------------------------------------------------
// 对象键 / ackId
// ---------------------------------------------------------------------------

const presignRecoverUploadPath = "/inner/presignRecoverUpload"

// recoverObjectKey 必须与 Java 侧 RecoverController 的前缀校验一致（"recover/"）。
func recoverObjectKey(messageID, mime, fileName string) string {
	owner := "unknown"
	if cli != nil && cli.Store != nil {
		owner = cli.Store.GetJID().String()
	}
	return "recover/" + sanitizeFileName(owner) + "/" + sanitizeFileName(messageID) + recoverExt(mime, fileName)
}

func recoverExt(mime, fileName string) string {
	if fileName != "" {
		if ext := strings.ToLower(filepath.Ext(fileName)); ext != "" {
			return ext
		}
	}
	return statusMediaExt(mime)
}

// recoverAckID 是 outbox 文件名与 Java 回执的唯一键。
// 同一 messageId 可能有 edit/revoke 多个事件，必须带 eventType + eventAt。
func recoverAckID(messageID, eventType string, eventAt int64) string {
	return messageID + "|" + eventType + "|" + strconv.FormatInt(eventAt, 10)
}

// ---------------------------------------------------------------------------
// outbox
// ---------------------------------------------------------------------------

func publishRecover(payload map[string]any) {
	ackID, _ := payload["ackId"].(string)
	if ackID != "" {
		if err := recoverOutboxWrite(ackID, payload); err != nil {
			log.Errorf("recover %s: outbox write failed: %v", ackID, err)
		}
	}
	presenceCache.Notify(MsgRecoverMessage, payload)
}

func recoverOutboxWrite(ackID string, payload map[string]any) error {
	if err := os.MkdirAll(recoverOutboxDir, 0700); err != nil {
		return err
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := os.WriteFile(recoverOutboxPath(ackID), b, 0600); err != nil {
		return err
	}
	pruneRecoverOutbox()
	return nil
}

func recoverOutboxAck(ackID string) {
	if ackID == "" {
		return
	}
	if err := os.Remove(recoverOutboxPath(ackID)); err != nil && !os.IsNotExist(err) {
		log.Warnf("recover outbox ack %s failed: %v", ackID, err)
	}
	recoverReplayLast.mu.Lock()
	delete(recoverReplayLast.last, ackID)
	recoverReplayLast.mu.Unlock()
}

func replayRecoverOutbox() {
	if !HasJavaClient() {
		return
	}
	recoverReplayMu.Lock()
	defer recoverReplayMu.Unlock()

	entries, err := os.ReadDir(recoverOutboxDir)
	if err != nil {
		return
	}
	now := time.Now()
	sent := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		ackID := strings.TrimSuffix(e.Name(), ".json")
		recoverReplayLast.mu.Lock()
		last, seen := recoverReplayLast.last[ackID]
		recoverReplayLast.mu.Unlock()
		if seen && now.Sub(last) < recoverReplayMinGap {
			continue
		}
		b, err := os.ReadFile(filepath.Join(recoverOutboxDir, e.Name()))
		if err != nil {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(b, &payload); err != nil {
			log.Errorf("recover outbox %s bad json, dropping: %v", ackID, err)
			_ = os.Remove(filepath.Join(recoverOutboxDir, e.Name()))
			continue
		}
		presenceCache.Notify(MsgRecoverMessage, payload)
		recoverReplayLast.mu.Lock()
		recoverReplayLast.last[ackID] = now
		recoverReplayLast.mu.Unlock()
		sent++
	}
	if sent > 0 {
		log.Infof("recover outbox replayed %d pending item(s)", sent)
	}
}

func pruneRecoverOutbox() {
	entries, err := os.ReadDir(recoverOutboxDir)
	if err != nil {
		return
	}
	type item struct {
		name  string
		mtime time.Time
	}
	items := make([]item, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{name: e.Name(), mtime: info.ModTime()})
	}
	if len(items) <= recoverOutboxMax {
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mtime.Before(items[j].mtime) })
	drop := len(items) - recoverOutboxMax
	for i := 0; i < drop; i++ {
		_ = os.Remove(filepath.Join(recoverOutboxDir, items[i].name))
	}
	recoverDropped.Add(int64(drop))
	log.Errorf("recover outbox overflow: dropped %d oldest item(s) (cap %d)", drop, recoverOutboxMax)
}

func recoverOutboxCount() int {
	entries, err := os.ReadDir(recoverOutboxDir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

func recoverOutboxPath(ackID string) string {
	return filepath.Join(recoverOutboxDir, sanitizeFileName(ackID)+".json")
}

// ---------------------------------------------------------------------------
// 暂存重传（复用 pending-upload）
// ---------------------------------------------------------------------------

func savePendingRecover(job *recoverJob, srcPath string) error {
	return savePendingUploadWithMeta(pendingUploadMeta{
		Source:              pendingSourceRecover,
		ObserverId:          job.chatID, // 复用字段：Recover 里存 chatId
		PushName:            job.chatName,
		FileName:            job.messageID,
		FileLength:          job.size,
		Seconds:             job.seconds,
		MiniType:            job.mime,
		ObjectKey:           job.objectKey,
		PostedAt:            job.postedAt,
		RecoverAckID:        job.ackID,
		RecoverEventType:    job.eventType,
		RecoverKind:         job.kind,
		RecoverText:         job.text,
		RecoverFileName:     job.fileName,
		RecoverEventAt:      job.eventAt,
		RecoverMediaSkipped: job.mediaSkipped,
	}, srcPath)
}
