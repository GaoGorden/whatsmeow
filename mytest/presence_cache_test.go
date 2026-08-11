package main

import (
	"bufio"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// TestMain 初始化包级 log（Notify 溢出路径会 log.Errorf；main() 不执行所以 log 默认 nil）。
func TestMain(m *testing.M) {
	log = waLog.Stdout("test", "INFO", true)
	os.Exit(m.Run())
}

// setupCapturedClient 切换到 socket backend 并安装一个可捕获输出的「Java 客户端」。
// 返回 server 端连接（用于读取 Go 写出的 ##PROTO## 行）和行通道。
// net.Pipe 为同步无缓冲通道：写会阻塞直到 reader 读取，故必须先启动读取 goroutine。
func setupCapturedClient() (net.Conn, chan string) {
	clearProtoConn()
	setDaemonBackend()
	server, client := net.Pipe()
	lines := make(chan string, 1024)
	go func() {
		scanner := bufio.NewScanner(server)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	setProtoConn(client)
	return server, lines
}

// recvLine 带超时地从行通道读取一行，避免测试卡死。
func recvLine(t *testing.T, lines chan string) string {
	t.Helper()
	select {
	case line := <-lines:
		return line
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for output line")
		return ""
	}
}

// expectNoLine 断言短时间内不应有新输出。
func expectNoLine(t *testing.T, lines chan string) {
	t.Helper()
	select {
	case line := <-lines:
		t.Fatalf("unexpected output line: %s", line)
	case <-time.After(100 * time.Millisecond):
	}
}

// 断连时缓冲，重连后按序重放且携带 ts。
func TestDisconnectedBufferAndReplay(t *testing.T) {
	clearProtoConn()
	setDaemonBackend()
	pc := NewPresenceCache(100)

	pc.Handle(&presenceEvent{state: "online", jid: "A@s.whatsapp.net", ts: "2026/08/07 10:01:00"})
	pc.Handle(&presenceEvent{state: "offline", jid: "A@s.whatsapp.net", lastSeen: "2026/08/07 10:02:00", ts: "2026/08/07 10:02:00"})
	pc.Handle(&presenceEvent{state: "online", jid: "B@s.whatsapp.net", ts: "2026/08/07 10:03:00"})
	if len(pc.buffer) != 3 {
		t.Fatalf("expected 3 buffered events, got %d", len(pc.buffer))
	}

	server, lines := setupCapturedClient()
	defer server.Close()

	pc.Replay()

	line1 := recvLine(t, lines)
	line2 := recvLine(t, lines)
	line3 := recvLine(t, lines)

	if !strings.HasPrefix(line1, "##PROTO##") {
		t.Fatalf("expected PROTO line, got: %s", line1)
	}
	if !strings.Contains(line1, `"state":"online"`) || !strings.Contains(line1, `"jid":"A@s.whatsapp.net"`) {
		t.Fatalf("line1 mismatch: %s", line1)
	}
	if !strings.Contains(line1, `"ts":"2026/08/07 10:01:00"`) {
		t.Fatalf("line1 missing ts: %s", line1)
	}
	if !strings.Contains(line2, `"state":"offline"`) || !strings.Contains(line2, `"lastSeen":"2026/08/07 10:02:00"`) {
		t.Fatalf("line2 mismatch: %s", line2)
	}
	if !strings.Contains(line3, `"state":"online"`) || !strings.Contains(line3, `"jid":"B@s.whatsapp.net"`) {
		t.Fatalf("line3 mismatch: %s", line3)
	}
	if len(pc.buffer) != 0 {
		t.Fatalf("buffer not drained after replay, len=%d", len(pc.buffer))
	}
	// latest 应保留每 JID 最新状态（供后续 overflow 兜底）
	if pc.latest["A@s.whatsapp.net"].state != "offline" {
		t.Fatalf("latest[A] should be offline")
	}
}

// 在线时立即发送且 buffer 保持为空；随后的 Replay 不再输出。
func TestConnectedDeliversImmediately(t *testing.T) {
	server, lines := setupCapturedClient()
	defer server.Close()

	pc := NewPresenceCache(100)
	pc.Handle(&presenceEvent{state: "online", jid: "C@s.whatsapp.net", ts: "2026/08/07 11:00:00"})

	line := recvLine(t, lines)
	if !strings.Contains(line, `"state":"online"`) || !strings.Contains(line, `"jid":"C@s.whatsapp.net"`) {
		t.Fatalf("unexpected line: %s", line)
	}
	if len(pc.buffer) != 0 {
		t.Fatalf("expected empty buffer when connected, got %d", len(pc.buffer))
	}

	pc.Replay()
	expectNoLine(t, lines)
}

// 溢出丢最旧；被完全挤出的 JID 由 latest 兜底补发当前状态。
func TestOverflowDropAndLatestFallback(t *testing.T) {
	clearProtoConn()
	setDaemonBackend()
	pc := NewPresenceCache(3)

	// X 的事件最先到达，随后超过 maxBuf 的其他事件将其挤出 buffer
	pc.Handle(&presenceEvent{state: "online", jid: "X@s.whatsapp.net", ts: "2026/08/07 12:00:00"})
	pc.Handle(&presenceEvent{state: "online", jid: "A@s.whatsapp.net", ts: "2026/08/07 12:01:00"})
	pc.Handle(&presenceEvent{state: "online", jid: "B@s.whatsapp.net", ts: "2026/08/07 12:02:00"})
	pc.Handle(&presenceEvent{state: "online", jid: "C@s.whatsapp.net", ts: "2026/08/07 12:03:00"})
	if len(pc.buffer) != 3 {
		t.Fatalf("expected 3 buffered events, got %d", len(pc.buffer))
	}
	for _, ev := range pc.buffer {
		if ev.jid == "X@s.whatsapp.net" {
			t.Fatalf("X should have been dropped from buffer")
		}
	}

	server, lines := setupCapturedClient()
	defer server.Close()
	pc.Replay()

	var output []string
	for i := 0; i < 4; i++ {
		output = append(output, recvLine(t, lines))
	}
	joined := strings.Join(output, "\n")
	if !strings.Contains(joined, `"jid":"X@s.whatsapp.net"`) {
		t.Fatalf("expected X current state via latest fallback, got: %s", joined)
	}
	if !strings.Contains(joined, `"jid":"A@s.whatsapp.net"`) ||
		!strings.Contains(joined, `"jid":"B@s.whatsapp.net"`) ||
		!strings.Contains(joined, `"jid":"C@s.whatsapp.net"`) {
		t.Fatalf("expected A/B/C buffered events, got: %s", joined)
	}
}

// 同 JID 连续 online→offline 重放顺序正确。
func TestSameJidOrdering(t *testing.T) {
	clearProtoConn()
	setDaemonBackend()
	pc := NewPresenceCache(100)
	pc.Handle(&presenceEvent{state: "online", jid: "D@s.whatsapp.net", ts: "2026/08/07 13:00:00"})
	pc.Handle(&presenceEvent{state: "offline", jid: "D@s.whatsapp.net", lastSeen: "2026/08/07 13:01:00", ts: "2026/08/07 13:01:00"})

	server, lines := setupCapturedClient()
	defer server.Close()
	pc.Replay()

	l1 := recvLine(t, lines)
	l2 := recvLine(t, lines)
	if !strings.Contains(l1, `"state":"online"`) {
		t.Fatalf("expected online first: %s", l1)
	}
	if !strings.Contains(l2, `"state":"offline"`) {
		t.Fatalf("expected offline second: %s", l2)
	}
}

// 断连时 view-once 通知缓冲，重连后按序重放（含完整载荷）。
func TestNotifyBuffersAndReplays(t *testing.T) {
	clearProtoConn()
	setDaemonBackend()
	pc := NewPresenceCache(100)

	pc.Notify("viewOnceFile", map[string]any{"observerId": "1", "objectKey": "whatsapp/view-once/A/f1.jpg"})
	pc.Notify("viewOnceFile", map[string]any{"observerId": "1", "objectKey": "whatsapp/view-once/A/f2.jpg"})
	if len(pc.notifyEvents) != 2 {
		t.Fatalf("expected 2 buffered notify events, got %d", len(pc.notifyEvents))
	}

	server, lines := setupCapturedClient()
	defer server.Close()
	pc.Replay()

	l1 := recvLine(t, lines)
	l2 := recvLine(t, lines)
	if !strings.Contains(l1, "viewOnceFile") || !strings.Contains(l1, "f1.jpg") {
		t.Fatalf("line1 mismatch: %s", l1)
	}
	if !strings.Contains(l2, "viewOnceFile") || !strings.Contains(l2, "f2.jpg") {
		t.Fatalf("line2 mismatch: %s", l2)
	}
	if len(pc.notifyEvents) != 0 {
		t.Fatalf("notify buffer not drained after replay, len=%d", len(pc.notifyEvents))
	}
}

// 在线时 view-once 通知立即送达，notify 缓冲保持为空。
func TestNotifyConnectedDeliversImmediately(t *testing.T) {
	server, lines := setupCapturedClient()
	defer server.Close()
	pc := NewPresenceCache(100)

	pc.Notify("viewOnceFile", map[string]any{"objectKey": "whatsapp/view-once/B/f.jpg"})

	l := recvLine(t, lines)
	if !strings.Contains(l, "viewOnceFile") || !strings.Contains(l, "f.jpg") {
		t.Fatalf("unexpected line: %s", l)
	}
	if len(pc.notifyEvents) != 0 {
		t.Fatalf("notify buffer should be empty when connected, got %d", len(pc.notifyEvents))
	}
}

// Remove 删除某 JID 的全部缓存条目（latest/buffer），其余 JID 不受影响。
func TestCacheRemove(t *testing.T) {
	clearProtoConn()
	setDaemonBackend()
	pc := NewPresenceCache(100)

	pc.Handle(&presenceEvent{state: "online", jid: "A@s.whatsapp.net", ts: "2026/08/07 14:00:00"})
	pc.Handle(&presenceEvent{state: "offline", jid: "A@s.whatsapp.net", lastSeen: "2026/08/07 14:01:00", ts: "2026/08/07 14:01:00"})
	pc.Handle(&presenceEvent{state: "online", jid: "B@s.whatsapp.net", ts: "2026/08/07 14:02:00"})
	if len(pc.buffer) != 3 {
		t.Fatalf("expected 3 buffered events, got %d", len(pc.buffer))
	}

	pc.Remove("A@s.whatsapp.net")

	// buffer 中 A 的事件被清掉，B 保留
	if len(pc.buffer) != 1 || pc.buffer[0].jid != "B@s.whatsapp.net" {
		t.Fatalf("buffer after remove mismatch: %+v", pc.buffer)
	}
	// latest 中 A 被移除
	if _, ok := pc.latest["A@s.whatsapp.net"]; ok {
		t.Fatalf("latest[A] should be removed")
	}
	if pc.latest["B@s.whatsapp.net"] == nil {
		t.Fatalf("latest[B] should remain")
	}

	// 重放只输出 B，不再输出 A
	server, lines := setupCapturedClient()
	defer server.Close()
	pc.Replay()
	l := recvLine(t, lines)
	if !strings.Contains(l, `"jid":"B@s.whatsapp.net"`) {
		t.Fatalf("expected only B after remove, got: %s", l)
	}
	expectNoLine(t, lines)
}

// Remove 同时清理 overflow 兜底集合 droppedJIDs 中的该 JID，重放不再补发。
func TestCacheRemoveClearsDroppedJIDs(t *testing.T) {
	clearProtoConn()
	setDaemonBackend()
	pc := NewPresenceCache(2)

	// X 被挤出 buffer，进入 droppedJIDs
	pc.Handle(&presenceEvent{state: "online", jid: "X@s.whatsapp.net", ts: "2026/08/07 15:00:00"})
	pc.Handle(&presenceEvent{state: "online", jid: "A@s.whatsapp.net", ts: "2026/08/07 15:01:00"})
	pc.Handle(&presenceEvent{state: "online", jid: "B@s.whatsapp.net", ts: "2026/08/07 15:02:00"})
	if !pc.droppedJIDs["X@s.whatsapp.net"] {
		t.Fatalf("expected X in droppedJIDs")
	}

	pc.Remove("X@s.whatsapp.net")
	if pc.droppedJIDs["X@s.whatsapp.net"] {
		t.Fatalf("expected X removed from droppedJIDs")
	}
	if _, ok := pc.latest["X@s.whatsapp.net"]; ok {
		t.Fatalf("expected X removed from latest")
	}

	// 重放不再补发 X
	server, lines := setupCapturedClient()
	defer server.Close()
	pc.Replay()
	l1 := recvLine(t, lines)
	l2 := recvLine(t, lines)
	joined := l1 + "\n" + l2
	if strings.Contains(joined, `"jid":"X@s.whatsapp.net"`) {
		t.Fatalf("X should not be replayed after remove, got: %s", joined)
	}
}

// notify 缓冲溢出丢最旧（并走 log.Errorf，TestMain 已初始化 log）。
func TestNotifyOverflowDropsOldest(t *testing.T) {
	clearProtoConn()
	setDaemonBackend()
	pc := NewPresenceCache(100)
	pc.maxNotify = 2

	pc.Notify("viewOnceFile", map[string]any{"objectKey": "whatsapp/view-once/A/f1.jpg"})
	pc.Notify("viewOnceFile", map[string]any{"objectKey": "whatsapp/view-once/A/f2.jpg"})
	pc.Notify("viewOnceFile", map[string]any{"objectKey": "whatsapp/view-once/A/f3.jpg"})
	if len(pc.notifyEvents) != 2 {
		t.Fatalf("expected 2 after overflow, got %d", len(pc.notifyEvents))
	}
	if pc.notifyEvents[0].data["objectKey"] != "whatsapp/view-once/A/f2.jpg" {
		t.Fatalf("expected oldest dropped, first is: %v", pc.notifyEvents[0].data["objectKey"])
	}
}
