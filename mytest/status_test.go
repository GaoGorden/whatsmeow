package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// 过滤集合 / 去重集
// ---------------------------------------------------------------------------

func TestNormalizeStatusJid(t *testing.T) {
	cases := map[string]string{
		"447751931566":                  "447751931566@s.whatsapp.net",
		"+447751931566":                 "447751931566@s.whatsapp.net",
		"447751931566@s.whatsapp.net":   "447751931566@s.whatsapp.net",
		"447751931566:12@s.whatsapp.net": "447751931566@s.whatsapp.net", // 去设备号
		"  447751931566  ":               "447751931566@s.whatsapp.net",
		"":                               "",
	}
	for in, want := range cases {
		if got := normalizeStatusJid(in); got != want {
			t.Errorf("normalizeStatusJid(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStatusObserverSetReplaceClearsOnEmpty(t *testing.T) {
	s := &statusObserverSet{ids: make(map[string]struct{})}
	s.replace([]string{"447751931566"})
	if !s.contains("447751931566@s.whatsapp.net") {
		t.Fatal("expected member after replace")
	}
	if s.count() != 1 {
		t.Fatalf("count = %d, want 1", s.count())
	}
	// 空集合的语义是「清空」，不是「不改动」
	s.replace(nil)
	if s.count() != 0 {
		t.Fatalf("count = %d after empty replace, want 0", s.count())
	}
	// 全量覆盖：旧成员必须消失（增量漏同步是静默故障，全量覆盖才自愈）
	s.replace([]string{"AAA", "BBB"})
	s.replace([]string{"BBB"})
	if s.contains("AAA@s.whatsapp.net") || !s.contains("BBB@s.whatsapp.net") {
		t.Fatal("replace must fully overwrite the set")
	}
}

func TestStatusSeenSetEvictsOldest(t *testing.T) {
	s := newStatusSeenSet(3)
	for _, id := range []string{"a", "b", "c"} {
		if !s.mark(id) {
			t.Fatalf("first mark(%s) must be true", id)
		}
	}
	if s.mark("a") {
		t.Fatal("repeat mark must be false")
	}
	// 触发淘汰：最旧的 a 被挤出，随后 a 应重新被视为「首次」
	if !s.mark("d") {
		t.Fatal("mark(d) must be true")
	}
	if !s.mark("a") {
		t.Fatal("evicted id must be treated as new again")
	}
	if len(s.ids) > 3 {
		t.Fatalf("set grew beyond capacity: %d", len(s.ids))
	}
}

func TestStatusMediaExt(t *testing.T) {
	cases := map[string]string{
		"image/jpeg": ".jpg",
		"video/mp4":  ".mp4",
		"audio/ogg":  ".ogg",
		"":           ".bin",
	}
	for in, want := range cases {
		if got := statusMediaExt(in); got != want {
			t.Errorf("statusMediaExt(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 事件过滤
// ---------------------------------------------------------------------------

func TestStatusHasContent(t *testing.T) {
	if statusHasContent(&waE2E.Message{}) {
		t.Fatal("empty message must have no content")
	}
	skdmOnly := &waE2E.Message{SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{}}
	if statusHasContent(skdmOnly) {
		t.Fatal("skdm-only message must have no content")
	}
	text := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("hi")}}
	if !statusHasContent(text) {
		t.Fatal("extended text must count as content")
	}
}

// TestHandleStatusMessageEarlyFilters 覆盖**不触碰 cli** 的那几层过滤。
// （后面几层要 searchPhoneNum/getNickName，依赖已登录的 cli.Store，只能在真机验证。）
func TestHandleStatusMessageEarlyFilters(t *testing.T) {
	resetStatusQueue(t)

	// 总开关关闭：什么都不过
	statusTrackEnabled.Store(false)
	handleStatusMessage(statusEvent(false, &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("x")}}))
	if len(statusQueue) != 0 {
		t.Fatal("disabled switch must ignore everything")
	}

	statusTrackEnabled.Store(true)

	// 自己发的
	handleStatusMessage(statusEvent(true, &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("x")}}))
	if len(statusQueue) != 0 {
		t.Fatal("IsFromMe must be filtered")
	}

	// 纯 SKDM（新 sender key 首条的重复派发）
	handleStatusMessage(statusEvent(false, &waE2E.Message{SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{}}))
	if len(statusQueue) != 0 {
		t.Fatal("skdm-only must be filtered")
	}

	// REVOKE / 其它 protocol 控制消息
	handleStatusMessage(statusEvent(false, &waE2E.Message{
		ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum()},
	}))
	if len(statusQueue) != 0 {
		t.Fatal("protocol (REVOKE) must be filtered")
	}

	// nil message
	handleStatusMessage(statusEvent(false, nil))
	if len(statusQueue) != 0 {
		t.Fatal("nil message must be ignored")
	}
}

func statusEvent(fromMe bool, msg *waE2E.Message) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			// Chat/IsFromMe 由内嵌的 MessageSource 提升而来，组合字面量里必须写到内层
			MessageSource: types.MessageSource{Chat: types.StatusBroadcastJID, IsFromMe: fromMe},
			ID:            "MSG-" + time.Now().Format("150405.000000"),
		},
		Message: msg,
	}
}

func resetStatusQueue(t *testing.T) {
	t.Helper()
	for {
		select {
		case <-statusQueue:
			continue
		default:
		}
		break
	}
}

// ---------------------------------------------------------------------------
// outbox
// ---------------------------------------------------------------------------

func TestStatusOutboxLifecycle(t *testing.T) {
	oldDir := statusOutboxDir
	statusOutboxDir = filepath.Join(t.TempDir(), "status-outbox")
	defer func() { statusOutboxDir = oldDir }()
	statusReplayLast.mu.Lock()
	statusReplayLast.last = make(map[string]time.Time)
	statusReplayLast.mu.Unlock()

	if err := statusOutboxWrite("S1", map[string]any{"statusId": "S1", "kind": "text"}); err != nil {
		t.Fatalf("write S1: %v", err)
	}
	if err := statusOutboxWrite("S2", map[string]any{"statusId": "S2", "kind": "image"}); err != nil {
		t.Fatalf("write S2: %v", err)
	}
	if got := statusOutboxCount(); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}

	// ack 幂等：删了再删一次不该报错（重放与 ack 天然会重复投递）
	statusOutboxAck("S1")
	statusOutboxAck("S1")
	if got := statusOutboxCount(); got != 1 {
		t.Fatalf("count = %d after ack, want 1", got)
	}

	// 无 Java 客户端时不重放（否则会把记录灌进内存缓冲，白占 notifyEvents 容量）
	replayStatusOutbox()
	if statusReplayMarked("S2") {
		t.Fatal("replay must be a no-op without a Java client")
	}

	// 装上「Java 客户端」后应重放，并记下重放时刻（用于抑制 60s 周期内的重复重发）
	c1, c2 := net.Pipe()
	defer c2.Close()
	setProtoConn(c1)
	defer func() {
		clearProtoConn()
		_ = c1.Close()
	}()
	replayStatusOutbox()
	if !statusReplayMarked("S2") {
		t.Fatal("expected S2 to be replayed once a Java client is attached")
	}

	// statusReplayMinGap 内不重复重放
	statusReplayLast.mu.Lock()
	marked := statusReplayLast.last["S2"]
	statusReplayLast.mu.Unlock()
	if time.Since(marked) > time.Minute {
		t.Fatal("replay timestamp looks wrong")
	}

	// 重放不会删除记录：只有 ack 才删（否则 Java 落库失败就永久丢了）
	if got := statusOutboxCount(); got != 1 {
		t.Fatalf("replay must not delete entries, count = %d", got)
	}
}

func TestStatusOutboxPrunesOldest(t *testing.T) {
	oldDir := statusOutboxDir
	statusOutboxDir = filepath.Join(t.TempDir(), "status-outbox")
	defer func() { statusOutboxDir = oldDir }()

	oldMax := statusOutboxMax
	statusOutboxMax = 2
	defer func() { statusOutboxMax = oldMax }()

	before := statusDropped.Load()
	for _, id := range []string{"A", "B", "C", "D"} {
		if err := statusOutboxWrite(id, map[string]any{"statusId": id}); err != nil {
			t.Fatalf("write %s: %v", id, err)
		}
		// 让 mtime 拉开，否则同一毫秒内的排序不确定
		time.Sleep(10 * time.Millisecond)
	}
	if got := statusOutboxCount(); got != 2 {
		t.Fatalf("count = %d after prune, want 2", got)
	}
	if statusDropped.Load() != before+2 {
		t.Fatalf("dropped counter not incremented (got %d, want %d)", statusDropped.Load(), before+2)
	}
	// 最旧的两条被淘汰、最新的保留
	if statusOutboxExists("A") || statusOutboxExists("B") {
		t.Fatal("oldest entries must be the ones pruned")
	}
	if !statusOutboxExists("C") || !statusOutboxExists("D") {
		t.Fatal("newest entries must survive")
	}
}

func TestStatusPayloadFromPending(t *testing.T) {
	meta := pendingUploadMeta{
		Source:     pendingSourceStatus,
		ObserverId: "447751931566@s.whatsapp.net",
		PushName:   "nick",
		FileLength: 1234,
		Seconds:    9,
		MiniType:   "video/mp4",
		StatusID:   "S9",
		MediaKind:  statusKindVideo,
		Text:       "capt",
		ThumbKey:   "whatsapp/status/owner/obs/S9_thumb.jpg",
		PostedAt:   1700000000000,
	}
	p := statusPayloadFromPending(meta, "whatsapp/status/owner/obs/S9.mp4")
	want := map[string]any{
		"statusId": "S9", "observerId": meta.ObserverId, "pushName": "nick",
		"kind": statusKindVideo, "miniType": "video/mp4", "fileLength": uint64(1234),
		"seconds": uint32(9), "text": "capt",
		"objectKey": "whatsapp/status/owner/obs/S9.mp4", "thumbKey": meta.ThumbKey,
		"postedAt": int64(1700000000000),
	}
	if len(p) != len(want) {
		t.Fatalf("payload has %d fields, want %d: %v", len(p), len(want), p)
	}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("payload[%q] = %v, want %v", k, p[k], v)
		}
	}
}

func statusReplayMarked(statusID string) bool {
	statusReplayLast.mu.Lock()
	defer statusReplayLast.mu.Unlock()
	_, ok := statusReplayLast.last[statusID]
	return ok
}

func statusOutboxExists(statusID string) bool {
	_, err := os.Stat(statusOutboxPath(statusID))
	return err == nil
}
