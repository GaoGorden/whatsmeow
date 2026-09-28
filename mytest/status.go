package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
)

// Status（WhatsApp 动态）采集。
//
// 把 status@broadcast 上「监听集合内联系人」发布的动态（文字/图片/视频/语音）下载 → 传 S3
// → 以 statusFile proto 交给 Java Server。对应 doc/Status动态追踪功能实现方案.md §3。
//
// 三条纪律：
//
//  1. **过滤必须全部发生在下载之前**，且整个 Status 分支要在 events.Message 的通用处理
//     （ProtoOutput(MsgReceivedMessage)）**之前** return。否则 skdm-only / REVOKE / 自己发的
//     动态会被当普通消息上报 Java，把 observer 误标 online 并写一条脏 History
//     （可行性验证文档 §3.3 记录的现存 bug，本次顺带修掉）。
//  2. **纯文字动态没有文件**，不走下载 / S3 / 并发信号量 / 队列背压那一整套——
//     媒体路径写顺手了很容易拿一个空的 media message 去下载（方案 §3.2 点名的最易漏点）。
//  3. **上报走 outbox**：先落盘 status-outbox/{statusId}.json，收到 Java 的 statusack 才删。
//     配合 presenceCache.Notify，覆盖「Java 掉线」「Go 进程重启」「两者都重启」三种断线场景
//     （方案 §3.6）。媒体本体在 S3 已落地，outbox 只存几十字节的元数据。

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

const (
	// statusDownloadConcurrency: 下载/上传并发上限（worker 数即信号量）。
	// 现状**完全没有**并发控制，多条大视频会同时打满内存与带宽（方案 §3.7）。
	statusDownloadConcurrency = 2

	// statusQueueCap: 待处理队列容量。队满丢弃**最旧**并记 ERROR——丢弃即该动态永久丢失。
	statusQueueCap = 64

	// statusDedupMax: 进程内已处理 statusId 的 LRU 上限。
	statusDedupMax = 5000

	// statusDownloadTimeout / statusUploadTimeout: 动态视频远大于 view-once（后者 60/30s）。
	statusDownloadTimeout = 120 * time.Second
	statusUploadTimeout   = 120 * time.Second

	// statusDiskWatermark: 临时目录所在磁盘剩余低于此值时暂停下载。
	// 同机有 ~93 个 Go daemon，磁盘是共享资源。
	statusDiskWatermark = 200 << 20

	// statusDiskPauseLimit: 磁盘持续不足时最多暂停多久，超时降级为「只上报元数据」——
	// 避免一个饿死的 worker 长期占着并发位。
	statusDiskPauseLimit = 5 * time.Minute

	// statusReplayMinGap: 同一条 outbox 记录的最小重放间隔。正常路径下 Java 会立刻 ack、
	// 记录随之消失；此间隔只为兜「Java 不 ack（旧版本）时每 60s 全量重发」这一情形。
	statusReplayMinGap = 5 * time.Minute

	// ⚠️ statusKindText **已不再产出**（2026-09-28）：纯文本动态在入口就被丢弃，
	// 不再上报给 Java。常量保留只为记录协议取值（存量 status_file 里仍有 kind='text' 的行，
	// Proxy 侧查询已按 `kind <> 'text'` 过滤，见 StatusFileRepository）。
	statusKindText  = "text"
	statusKindImage = "image"
	statusKindVideo = "video"
	statusKindAudio = "audio"

	statusThumbSuffix = "_thumb.jpg"
)

// statusMaxFileBytes: 单条媒体上限（100MB），超出只上报元数据 + 缩略图、不下载媒体。
// ⚠️ 这是**成本/磁盘口径**而非技术必需——主链路（downloadToTemp + uploadFileToS3）已流式化，
// 峰值内存与文件大小无关（方案 §3.7.2④）。用 var 而非 const 便于单测调小。
var statusMaxFileBytes int64 = 100 << 20

// statusOutboxMax: outbox 条数上限，超出丢最旧并记 ERROR。用 var 而非 const 便于单测调小。
var statusOutboxMax = 5000

// ---------------------------------------------------------------------------
// 采集状态
// ---------------------------------------------------------------------------

var (
	// statusTrackEnabled: 总开关（对应 SubUser.enableStatusTrack）。
	statusTrackEnabled atomic.Bool

	// statusObservers: 过滤集合（只用于过滤收到的动态，Status 不需要订阅协议）。
	statusObservers = &statusObserverSet{ids: make(map[string]struct{})}

	// statusSeen: 进程内已处理 statusId，避免重放/重复派发触发重复下载。
	// 跨进程去重由 Server 的 uk_status_msg 唯一键兜底。
	statusSeen = newStatusSeenSet(statusDedupMax)

	// statusQueue: 媒体任务队列（文字动态不入队）。
	statusQueue = make(chan *statusJob, statusQueueCap)

	// statusDropped: 因队列背压 / outbox 溢出被丢弃的总条数（心跳上报）。
	statusDropped atomic.Int64
)

// statusObserverSet 是带锁的过滤集合。元素统一为 normalizeStatusJid 之后的形式
// （{phone}@s.whatsapp.net），与事件侧 searchPhoneNum 的输出同口径。
type statusObserverSet struct {
	mu  sync.RWMutex
	ids map[string]struct{}
}

// replace 全量覆盖（Java 侧唯一的同步方式，空集合 = 清空）。
func (s *statusObserverSet) replace(raw []string) {
	next := make(map[string]struct{}, len(raw))
	for _, r := range raw {
		if jid := normalizeStatusJid(r); jid != "" {
			next[jid] = struct{}{}
		}
	}
	s.mu.Lock()
	s.ids = next
	s.mu.Unlock()
}

func (s *statusObserverSet) add(jid string) {
	if jid == "" {
		return
	}
	s.mu.Lock()
	s.ids[jid] = struct{}{}
	s.mu.Unlock()
}

func (s *statusObserverSet) remove(jid string) {
	if jid == "" {
		return
	}
	s.mu.Lock()
	delete(s.ids, jid)
	s.mu.Unlock()
}

func (s *statusObserverSet) contains(jid string) bool {
	s.mu.RLock()
	_, ok := s.ids[jid]
	s.mu.RUnlock()
	return ok
}

func (s *statusObserverSet) count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.ids)
}

// normalizeStatusJid 把任意形态的号码统一成 JID 字面量。
// 两侧都过一遍，避免「Java 发纯号码 / Go 拿完整 JID」时集合**静默**不匹配
// （表现为「某些联系人永远收不到动态」，且没有任何报错）。
func normalizeStatusJid(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "+")
	if s == "" {
		return ""
	}
	at := strings.Index(s, "@")
	if at < 0 {
		return s + "@s.whatsapp.net"
	}
	// 去掉设备号后缀（1234:12@s.whatsapp.net → 1234@s.whatsapp.net）
	if colon := strings.Index(s[:at], ":"); colon >= 0 {
		s = s[:colon] + s[at:]
	}
	return s
}

// statusSeenSet 是固定容量的 FIFO 去重集合（环形淘汰，内存不随时间增长）。
type statusSeenSet struct {
	mu    sync.Mutex
	ids   map[string]struct{}
	order []string
	head  int
	max   int
}

func newStatusSeenSet(max int) *statusSeenSet {
	return &statusSeenSet{ids: make(map[string]struct{}, max), order: make([]string, 0, max), max: max}
}

// mark 返回 true 表示首次出现（应继续处理），false 表示重复（应丢弃）。
func (s *statusSeenSet) mark(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ids[id]; ok {
		return false
	}
	if len(s.order) < s.max {
		s.order = append(s.order, id)
	} else {
		delete(s.ids, s.order[s.head])
		s.order[s.head] = id
		s.head = (s.head + 1) % s.max
	}
	s.ids[id] = struct{}{}
	return true
}

// ---------------------------------------------------------------------------
// 事件入口
// ---------------------------------------------------------------------------

// handleStatusMessage 处理一条 status@broadcast 消息。调用方必须在本函数返回后
// **不再走通用消息处理**（见文件头「纪律 1」）。
//
// 过滤顺序（全部在下载之前）：自己发的 → 纯 SKDM → protocol（含 REVOKE）→ 非监听集合 → 已处理过。
func handleStatusMessage(evt *events.Message) {
	if !statusTrackEnabled.Load() || evt == nil || evt.Message == nil {
		return
	}
	ctx := context.Background()

	// ① 自己发的动态
	if evt.Info.IsFromMe {
		return
	}
	// ② 纯 SKDM：新 sender key 的第一条会以「skdm+内容」与「skdm-only」派发两次，
	//    只丢没有内容的那次即可，**不需要**按 msgId 做时间窗（可行性验证 §2.2）。
	if evt.Message.GetSenderKeyDistributionMessage() != nil && !statusHasContent(evt.Message) {
		return
	}
	// ③ protocol 控制消息：REVOKE（对方撤回动态）不处理，对齐竞品；其余 protocol 也无内容可采。
	if pm := evt.Message.GetProtocolMessage(); pm != nil {
		log.Debugf("status: ignore protocol message %s (type %v) from status@broadcast", evt.Info.ID, pm.GetType())
		return
	}

	// ④ 发送者是否在监听集合内（Status 的过滤集合按号码过滤）
	observerJid := normalizeStatusJid(searchPhoneNum(ctx, evt.Info.Sender))
	if !statusObservers.contains(observerJid) {
		return
	}

	// ⑤ 去重（重放 / 重复派发）
	if evt.Info.ID == "" || !statusSeen.mark(evt.Info.ID) {
		return
	}

	pushName := getNickName(ctx, evt.Info.Sender)
	if pushName == "" {
		pushName = evt.Info.PushName
	}
	postedAt := evt.Info.Timestamp.UnixMilli()
	if postedAt <= 0 {
		postedAt = time.Now().UnixMilli()
	}

	job := &statusJob{
		statusID:    evt.Info.ID,
		observerJid: observerJid,
		pushName:    pushName,
		postedAt:    postedAt,
	}

	switch {
	case evt.Message.GetImageMessage() != nil:
		m := evt.Message.GetImageMessage()
		job.kind = statusKindImage
		job.miniType = m.GetMimetype()
		job.fileLength = m.GetFileLength()
		job.text = m.GetCaption()
		job.thumb = m.GetJPEGThumbnail()
		job.media = m
	case evt.Message.GetVideoMessage() != nil:
		m := evt.Message.GetVideoMessage()
		job.kind = statusKindVideo
		job.miniType = m.GetMimetype()
		job.fileLength = m.GetFileLength()
		job.seconds = m.GetSeconds()
		job.text = m.GetCaption()
		job.thumb = m.GetJPEGThumbnail()
		job.media = m
	case evt.Message.GetAudioMessage() != nil:
		m := evt.Message.GetAudioMessage()
		job.kind = statusKindAudio
		job.miniType = m.GetMimetype()
		job.fileLength = m.GetFileLength()
		job.seconds = m.GetSeconds()
		// AudioMessage 没有 caption 字段，text 恒为空（方案 §2.4）
		job.media = m
	default:
		// 纯文字动态：**不监听、不上报**（2026-09-28 产品要求）。
		//
		// 原先这里会把纯文字动态当成一条 kind=text、无文件的记录 publish 出去
		//（方案 §3.2「无文件 → 不碰下载/上传/并发/背压」）—— 它照样进 status_file、
		// 照样参与未读数与 5 分钟聚合推送，而 App 详情页只有一行文字、没有任何
		// 可看/可下/可分享的内容。现在连"监听"这一层都不再做：落进本分支的一律丢弃
		//（既无媒体也无正文的未知类型同样在此丢弃）。
		//
		// ⚠️ **动态自带的文案不受影响**：图片/视频的 caption 走上面两个分支的
		// `job.text = m.GetCaption()`，语音恒空（AudioMessage 没有 caption 字段）——
		// 本改动只砍掉「纯文本」这一种动态，不会让带图动态丢文案。
		log.Debugf("status: drop text-only status %s from status@broadcast", evt.Info.ID)
		return
	}

	job.objectKey = statusObjectKey(observerJid, job.statusID, statusMediaExt(job.miniType))
	if job.media != nil && job.fileLength > uint64(statusMaxFileBytes) {
		// 超限：只上报元数据 + 缩略图，不下载媒体（方案 §3.7 的 oversize 口径）。
		// App 侧据「kind 是媒体但 objectKey 为空」判定该条媒体不可用。
		log.Errorf("status %s oversize: %d bytes > %d, reporting metadata only (kind %s)",
			job.statusID, job.fileLength, statusMaxFileBytes, job.kind)
		job.media = nil
		job.oversize = true
	}
	enqueueStatusJob(job)
}

// statusHasContent 判定消息是否带实际内容（用于剔除纯 SKDM 事件）。
func statusHasContent(m *waE2E.Message) bool {
	return m.GetImageMessage() != nil || m.GetVideoMessage() != nil ||
		m.GetAudioMessage() != nil || m.GetExtendedTextMessage() != nil ||
		m.GetConversation() != ""
}

// statusObjectKey 构造动态媒体在 S3 的 objectKey。前缀 whatsapp/status/ 是 Java 侧
// 预签名接口的校验条件（StatusController.STATUS_OBJECT_KEY_PREFIX），前缀不符直接 400。
func statusObjectKey(observerJid, statusID, ext string) string {
	owner := "unknown"
	if cli != nil && cli.Store != nil {
		owner = cli.Store.GetJID().String()
	}
	return "whatsapp/status/" + sanitizeFileName(owner) + "/" + sanitizeFileName(observerJid) + "/" + sanitizeFileName(statusID) + ext
}

// statusMediaExt 由 WhatsApp 声明的 mimetype 推出对象键后缀。
// 刻意不用 mimetype 库反查：状态媒体的类型就这几种，显式表更可预测，也保证
// 「首次上传」与「暂存重传」算出**同一个** key。
func statusMediaExt(miniType string) string {
	switch {
	case strings.Contains(miniType, "jpeg"), strings.Contains(miniType, "jpg"):
		return ".jpg"
	case strings.Contains(miniType, "png"):
		return ".png"
	case strings.Contains(miniType, "webp"):
		return ".webp"
	case strings.Contains(miniType, "mp4"):
		return ".mp4"
	case strings.Contains(miniType, "3gpp"):
		return ".3gp"
	case strings.Contains(miniType, "ogg"):
		return ".ogg"
	case strings.Contains(miniType, "mpeg"):
		return ".mp3"
	case strings.Contains(miniType, "aac"):
		return ".aac"
	}
	return ".bin"
}

// ---------------------------------------------------------------------------
// 任务队列与 worker
// ---------------------------------------------------------------------------

// statusJob 是一条待采集（或已降级为「只上报元数据」）的动态。
type statusJob struct {
	statusID    string
	observerJid string
	pushName    string
	kind        string
	miniType    string
	text        string
	seconds     uint32
	fileLength  uint64
	postedAt    int64
	objectKey   string
	thumb       []byte
	media       whatsmeow.DownloadableMessage
	oversize    bool
}

// payload 构造 statusFile proto 载荷。字段与 Java 侧 StatusFileBean 严格对应。
func (j *statusJob) payload(objectKey, thumbKey string) map[string]any {
	return map[string]any{
		"statusId":   j.statusID,
		"observerId": j.observerJid,
		"pushName":   j.pushName,
		"kind":       j.kind,
		"miniType":   j.miniType,
		"fileLength": j.fileLength,
		"seconds":    j.seconds,
		"text":       j.text,
		"objectKey":  objectKey,
		"thumbKey":   thumbKey,
		"postedAt":   j.postedAt,
	}
}

// startStatusWorkers 启动固定数量的采集 worker（worker 数即下载/上传并发上限）。
func startStatusWorkers() {
	for i := 0; i < statusDownloadConcurrency; i++ {
		go func() {
			for job := range statusQueue {
				processStatusJob(job)
			}
		}()
	}
}

// enqueueStatusJob 入队；队满时丢**最旧**（记 ERROR + 计数，丢弃即永久丢失，必须可观测）。
func enqueueStatusJob(job *statusJob) {
	for {
		select {
		case statusQueue <- job:
			return
		default:
		}
		select {
		case dropped := <-statusQueue:
			statusDropped.Add(1)
			log.Errorf("status queue full, dropped oldest job %s (kind %s)", dropped.statusID, dropped.kind)
		default:
			// 队列刚被 worker 取空，回到循环重试入队
		}
	}
}

// processStatusJob 执行一条动态的下载 + 上传 + 上报。
func processStatusJob(job *statusJob) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("status job %s panicked: %v", job.statusID, r)
		}
	}()

	if job.media != nil && !statusWaitForDisk(job.statusID) {
		log.Errorf("status %s: disk still below watermark after %s, reporting metadata only", job.statusID, statusDiskPauseLimit)
		job.media = nil
		job.oversize = true
	}

	// 缩略图先传：它是消息里内嵌的小图（1~4KB），不占磁盘，且列表页靠它渲染。
	thumbKey := ""
	if len(job.thumb) > 0 {
		key := job.objectKey + statusThumbSuffix
		if err := uploadStreamToS3(presignStatusUploadPath, key, bytes.NewReader(job.thumb),
			int64(len(job.thumb)), "image/jpeg", statusUploadTimeout); err != nil {
			// 缩略图失败不该阻断整条动态：列表页少一张图，详情页仍能看到全尺寸媒体
			log.Warnf("status %s: thumbnail upload failed: %v", job.statusID, err)
		} else {
			thumbKey = key
		}
	}

	if job.media == nil {
		// 只上报元数据（oversize / 磁盘降级）。objectKey 留空，App 据此知道媒体不可用。
		publishStatus(job.payload("", thumbKey))
		return
	}

	dlCtx, cancel := context.WithTimeout(context.Background(), statusDownloadTimeout)
	tmpPath, err := downloadToTemp(dlCtx, job.media, job.statusID)
	cancel()
	if err != nil {
		log.Errorf("status %s: download failed: %v", job.statusID, err)
		return
	}
	// 上传成功时删除临时文件；走暂存时 savePendingStatus 已把它 rename 走，这里是 no-op
	defer os.Remove(tmpPath)

	if err := uploadFileToS3(presignStatusUploadPath, job.objectKey, tmpPath, job.miniType, statusUploadTimeout); err != nil {
		// 媒体已落盘、只是没传上去（多为 Java 掉线致预签名失败）→ 走 pending-upload 重传，
		// 不能丢：动态媒体 24h 后就再也拿不到了（方案 §3.6 末尾的提醒）。
		if saveErr := savePendingStatus(job, tmpPath, thumbKey); saveErr != nil {
			log.Errorf("status %s: upload failed (%v) and pending save failed: %v", job.statusID, err, saveErr)
			return
		}
		log.Warnf("status %s: upload failed (%v), buffered to pending-upload for retry", job.statusID, err)
		return
	}

	publishStatus(job.payload(job.objectKey, thumbKey))
}

// statusWaitForDisk 磁盘水位不足时暂停下载；返回 false 表示暂停超时后放弃下载。
func statusWaitForDisk(statusID string) bool {
	if !statusDiskLow() {
		return true
	}
	log.Warnf("status %s: disk below watermark, pausing download", statusID)
	pausedAt := time.Now()
	for statusDiskLow() {
		if time.Since(pausedAt) > statusDiskPauseLimit {
			return false
		}
		time.Sleep(15 * time.Second)
	}
	return true
}

// statusDiskLow 探测当前工作目录所在磁盘是否低于水位；无法探测（Windows 联调等）时放行。
func statusDiskLow() bool {
	free, ok := statusFreeDiskBytes(".")
	if !ok {
		return false
	}
	return free < uint64(statusDiskWatermark)
}

// ---------------------------------------------------------------------------
// outbox（已上传 → 未确认）
// ---------------------------------------------------------------------------

var statusOutboxDir = "status-outbox"

// statusReplayMu 串行化重放：attach 触发与 60s 周期触发可能并发。
var statusReplayMu sync.Mutex

// statusReplayLast 记录每条最近一次重放时刻（纯内存，进程重启即清空——那正是我们希望
// 「重启后全量重放一次」的语义）。
var statusReplayLast = struct {
	mu   sync.Mutex
	last map[string]time.Time
}{last: make(map[string]time.Time)}

// publishStatus 上报一条动态：先落盘 outbox（保证跨重启可重放），再交给 PresenceCache
// （Java 在线即时送达，掉线缓冲、重连重放）。
func publishStatus(payload map[string]any) {
	statusID, _ := payload["statusId"].(string)
	if statusID != "" {
		if err := statusOutboxWrite(statusID, payload); err != nil {
			// 落盘失败仍会上报一次，只是进程重启后无法重放——记 ERROR 因为它意味着丢数据风险
			log.Errorf("status %s: outbox write failed: %v", statusID, err)
		}
	}
	presenceCache.Notify(MsgStatusFile, payload)
}

func statusOutboxWrite(statusID string, payload map[string]any) error {
	if err := os.MkdirAll(statusOutboxDir, 0700); err != nil {
		return err
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := os.WriteFile(statusOutboxPath(statusID), b, 0600); err != nil {
		return err
	}
	pruneStatusOutbox()
	return nil
}

// statusOutboxAck 删除已确认的记录（Java 落库后下发 statusack <statusId>）。
func statusOutboxAck(statusID string) {
	if statusID == "" {
		return
	}
	if err := os.Remove(statusOutboxPath(statusID)); err != nil && !os.IsNotExist(err) {
		log.Warnf("status outbox ack %s failed: %v", statusID, err)
	}
	statusReplayLast.mu.Lock()
	delete(statusReplayLast.last, statusID)
	statusReplayLast.mu.Unlock()
}

// replayStatusOutbox 重放全部未确认的记录（attach 时 + 每 60s，且仅在 Java 在线时）。
func replayStatusOutbox() {
	if !HasJavaClient() {
		return
	}
	statusReplayMu.Lock()
	defer statusReplayMu.Unlock()

	entries, err := os.ReadDir(statusOutboxDir)
	if err != nil {
		return // 目录不存在 = 无待重放
	}
	now := time.Now()
	sent := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		statusID := strings.TrimSuffix(e.Name(), ".json")
		statusReplayLast.mu.Lock()
		last, seen := statusReplayLast.last[statusID]
		statusReplayLast.mu.Unlock()
		if seen && now.Sub(last) < statusReplayMinGap {
			continue
		}
		b, err := os.ReadFile(filepath.Join(statusOutboxDir, e.Name()))
		if err != nil {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(b, &payload); err != nil {
			// 毒记录：重放多少次都是同样结果，删掉并记 ERROR
			log.Errorf("status outbox %s bad json, dropping: %v", statusID, err)
			_ = os.Remove(filepath.Join(statusOutboxDir, e.Name()))
			continue
		}
		presenceCache.Notify(MsgStatusFile, payload)
		statusReplayLast.mu.Lock()
		statusReplayLast.last[statusID] = now
		statusReplayLast.mu.Unlock()
		sent++
	}
	if sent > 0 {
		log.Infof("status outbox replayed %d pending item(s)", sent)
	}
}

// pruneStatusOutbox 超过上限时按 mtime 丢最旧（记 ERROR + 计数：被丢弃的记录意味着
// 该条动态永远不会被 Java 落库）。
func pruneStatusOutbox() {
	entries, err := os.ReadDir(statusOutboxDir)
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
	if len(items) <= statusOutboxMax {
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mtime.Before(items[j].mtime) })
	drop := len(items) - statusOutboxMax
	for i := 0; i < drop; i++ {
		_ = os.Remove(filepath.Join(statusOutboxDir, items[i].name))
	}
	statusDropped.Add(int64(drop))
	log.Errorf("status outbox overflow: dropped %d oldest item(s) (cap %d)", drop, statusOutboxMax)
}

// statusOutboxCount 待确认条数（心跳上报，便于运维判断 Java 侧是否在正常 ack）。
func statusOutboxCount() int {
	entries, err := os.ReadDir(statusOutboxDir)
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

func statusOutboxPath(statusID string) string {
	return filepath.Join(statusOutboxDir, sanitizeFileName(statusID)+".json")
}

// ---------------------------------------------------------------------------
// 暂存重传（复用 pending-upload）
// ---------------------------------------------------------------------------

// savePendingStatus 把上传失败的动态媒体交给 pending-upload 暂存，等待下次重传。
// 复用而非另起一套：裁剪（条数 + 总字节）、rename 搬移、周期/attach 触发都已在那边实现。
func savePendingStatus(job *statusJob, srcPath, thumbKey string) error {
	return savePendingUploadWithMeta(pendingUploadMeta{
		Source:     pendingSourceStatus,
		ObserverId: job.observerJid,
		PushName:   job.pushName,
		FileName:   job.statusID,
		FileLength: job.fileLength,
		Seconds:    job.seconds,
		MiniType:   job.miniType,
		StatusID:   job.statusID,
		MediaKind:  job.kind,
		Text:       job.text,
		ObjectKey:  job.objectKey,
		ThumbKey:   thumbKey,
		PostedAt:   job.postedAt,
	}, srcPath)
}

// statusPayloadFromPending 用暂存元数据重建 statusFile 载荷（重传成功后调用）。
func statusPayloadFromPending(meta pendingUploadMeta, objectKey string) map[string]any {
	return map[string]any{
		"statusId":   meta.StatusID,
		"observerId": meta.ObserverId,
		"pushName":   meta.PushName,
		"kind":       meta.MediaKind,
		"miniType":   meta.MiniType,
		"fileLength": meta.FileLength,
		"seconds":    meta.Seconds,
		"text":       meta.Text,
		"objectKey":  objectKey,
		"thumbKey":   meta.ThumbKey,
		"postedAt":   meta.PostedAt,
	}
}
