package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSrcFile 模拟 downloadToTemp 的产物：把内容写成一个临时源文件，供 savePendingUpload 搬入暂存目录。
// 返回路径（savePendingUpload 会把它 rename 走，调用后该路径通常已不存在）。
func writeSrcFile(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(p, content, 0600); err != nil {
		t.Fatalf("write src file: %v", err)
	}
	return p
}

// 保存 → 重连（httptest 假 Server 提供预签名 + PUT）→ flush → 上传成功、本地删除、通知送达。
func TestSaveAndFlushPendingUpload(t *testing.T) {
	dir := t.TempDir()
	pendingUploadDir = dir
	clearProtoConn()
	setDaemonBackend()

	var putCalls int
	var putBody []byte
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/inner/presignViewOnceUpload":
			w.Write([]byte(`{"url":"` + srv.URL + `/upload"}`))
		case "/upload":
			putCalls++
			putBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	*serverUrl = srv.URL

	// 保存一条待重传媒体
	payload := []byte("fake-view-once-media-bytes")
	src := writeSrcFile(t, payload)
	if err := savePendingUpload("obs1", "nick", "MSGAABB", src, uint64(len(payload)), 0, "image/jpeg"); err != nil {
		t.Fatalf("savePendingUpload failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "MSGAABB.bin")); err != nil {
		t.Fatalf("expected pending .bin written: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source temp file should have been moved into pending dir, still exists: %s", src)
	}

	// 重连 Java 客户端并触发 flush
	server, lines := setupCapturedClient()
	defer server.Close()
	flushPendingUploads()

	if putCalls != 1 {
		t.Fatalf("expected 1 PUT, got %d", putCalls)
	}
	if string(putBody) != string(payload) {
		t.Fatalf("PUT body mismatch: got %q", putBody)
	}
	if _, err := os.Stat(filepath.Join(dir, "MSGAABB.json")); !os.IsNotExist(err) {
		t.Fatalf("pending .json should be deleted after successful flush")
	}
	if _, err := os.Stat(filepath.Join(dir, "MSGAABB.bin")); !os.IsNotExist(err) {
		t.Fatalf("pending .bin should be deleted after successful flush")
	}

	// 通知应已送达 Java（含 objectKey 中的文件名）
	l := recvLine(t, lines)
	if !strings.Contains(l, "viewOnceFile") || !strings.Contains(l, "MSGAABB") {
		t.Fatalf("expected viewOnceFile notification, got: %s", l)
	}
}

// 上传失败（无 --server-url，预签名不可用）时暂存文件保留，待下次重试。
func TestFlushKeepsPendingOnUploadFailure(t *testing.T) {
	dir := t.TempDir()
	pendingUploadDir = dir
	*serverUrl = ""

	payload := []byte("x")
	if err := savePendingUpload("obs", "n", "KEEP01", writeSrcFile(t, payload), uint64(len(payload)), 0, "image/png"); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	clearProtoConn()
	setDaemonBackend()
	flushPendingUploads()

	if _, err := os.Stat(filepath.Join(dir, "KEEP01.json")); err != nil {
		t.Fatalf("pending .json should be kept on upload failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "KEEP01.bin")); err != nil {
		t.Fatalf("pending .bin should be kept: %v", err)
	}
}

// 暂存超过上限时裁剪到上限。
func TestPrunePendingUploads(t *testing.T) {
	dir := t.TempDir()
	pendingUploadDir = dir

	for i := 0; i < pendingUploadMax+5; i++ {
		fileName := fmt.Sprintf("MSG%04d", i)
		if err := savePendingUpload("obs", "n", fileName, writeSrcFile(t, []byte("d")), 1, 0, "image/png"); err != nil {
			t.Fatalf("save %d failed: %v", i, err)
		}
	}
	if got := len(listPendingPairs()); got > pendingUploadMax {
		t.Fatalf("expected capped at %d, got %d", pendingUploadMax, got)
	}
}

// 条数远未到上限、但总字节超标时同样裁剪（大视频场景）。
func TestPrunePendingUploadsByBytes(t *testing.T) {
	dir := t.TempDir()
	pendingUploadDir = dir
	origBytes := pendingUploadMaxBytes
	pendingUploadMaxBytes = 300 // 每条 100 字节 → 只留得下 3 条
	defer func() { pendingUploadMaxBytes = origBytes }()

	payload := make([]byte, 100)
	base := time.Now()
	for i := 0; i < 5; i++ {
		fileName := fmt.Sprintf("BIG%04d", i)
		src := writeSrcFile(t, payload)
		if err := savePendingUpload("obs", "n", fileName, src, uint64(len(payload)), 0, "video/mp4"); err != nil {
			t.Fatalf("save %d failed: %v", i, err)
		}
		// 落盘 mtime 可能同毫秒；显式拉成递增，让「丢最旧」的顺序可预期
		ts := base.Add(time.Duration(i) * time.Second)
		_ = os.Chtimes(filepath.Join(dir, fileName+".json"), ts, ts)
	}

	pairs := listPendingPairs()
	total := int64(0)
	for _, p := range pairs {
		total += p.size
	}
	if total > pendingUploadMaxBytes {
		t.Fatalf("expected total bytes <= %d, got %d (%d items)", pendingUploadMaxBytes, total, len(pairs))
	}
	if len(pairs) == 0 {
		t.Fatalf("prune should keep the newest items, got none")
	}
}

// writePendingFixture 直接落一条暂存记录（不经过 save 路径，便于构造指定的 source / mtime）。
func writePendingFixture(t *testing.T, dir, base string, meta pendingUploadMeta, size int, mtime time.Time) {
	t.Helper()
	b, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta %s: %v", base, err)
	}
	jsonPath := filepath.Join(dir, base+".json")
	binPath := filepath.Join(dir, base+".bin")
	if err := os.WriteFile(jsonPath, b, 0600); err != nil {
		t.Fatalf("write json %s: %v", base, err)
	}
	if err := os.WriteFile(binPath, make([]byte, size), 0600); err != nil {
		t.Fatalf("write bin %s: %v", base, err)
	}
	if err := os.Chtimes(jsonPath, mtime, mtime); err != nil {
		t.Fatalf("chtimes json %s: %v", base, err)
	}
	if err := os.Chtimes(binPath, mtime, mtime); err != nil {
		t.Fatalf("chtimes bin %s: %v", base, err)
	}
}

func pendingExists(dir, base string) bool {
	_, err := os.Stat(filepath.Join(dir, base+".json"))
	return err == nil
}

// 淘汰顺序必须是「价值最低先丢」，而不是「最旧先丢」：
// 最老的 ViewOnce 必须活下来，优先牺牲 Recover 的缓冲消息媒体。
func TestPrunePendingUploadsByPriority(t *testing.T) {
	dir := t.TempDir()
	pendingUploadDir = dir
	origMax, origBytes := pendingUploadMax, pendingUploadMaxBytes
	pendingUploadMax, pendingUploadMaxBytes = 4, int64(1<<30)
	defer func() { pendingUploadMax, pendingUploadMaxBytes = origMax, origBytes }()

	now := time.Now()
	// ViewOnce 是最老的一条：若只看 mtime 必被淘汰，本用例证明优先级胜过 mtime
	writePendingFixture(t, dir, "VIEWONCE_OLD", pendingUploadMeta{FileName: "VIEWONCE_OLD"}, 10, now.Add(-10*time.Hour))
	writePendingFixture(t, dir, "REC_N1", pendingUploadMeta{
		FileName: "REC_N1", Source: pendingSourceRecover, RecoverEventType: "normal"}, 10, now.Add(-5*time.Hour))
	writePendingFixture(t, dir, "REC_N2", pendingUploadMeta{
		FileName: "REC_N2", Source: pendingSourceRecover, RecoverEventType: "normal"}, 10, now.Add(-4*time.Hour))
	writePendingFixture(t, dir, "REC_N3", pendingUploadMeta{
		FileName: "REC_N3", Source: pendingSourceRecover, RecoverEventType: "normal"}, 10, now.Add(-3*time.Hour))
	writePendingFixture(t, dir, "REC_PERM", pendingUploadMeta{
		FileName: "REC_PERM", Source: pendingSourceRecover, RecoverEventType: "revoke"}, 10, now.Add(-2*time.Hour))
	writePendingFixture(t, dir, "STATUS1", pendingUploadMeta{
		FileName: "STATUS1", Source: pendingSourceStatus}, 10, now.Add(-1*time.Hour))

	prunePendingUploads()

	if !pendingExists(dir, "VIEWONCE_OLD") {
		t.Fatalf("view-once must not be evicted by recover volume")
	}
	for _, gone := range []string{"REC_N1", "REC_N2"} {
		if pendingExists(dir, gone) {
			t.Fatalf("%s (oldest recover-buffer) should be pruned first", gone)
		}
	}
	for _, keep := range []string{"REC_N3", "REC_PERM", "STATUS1"} {
		if !pendingExists(dir, keep) {
			t.Fatalf("%s should be kept", keep)
		}
	}
	if got := len(listPendingPairs()); got != 4 {
		t.Fatalf("expected 4 items after prune, got %d", got)
	}
}

// 只有 ViewOnce 且超过上限时，退回「丢最旧」的既有行为。
func TestPrunePendingUploadsViewOnceOnlyDropsOldest(t *testing.T) {
	dir := t.TempDir()
	pendingUploadDir = dir
	origMax, origBytes := pendingUploadMax, pendingUploadMaxBytes
	pendingUploadMax, pendingUploadMaxBytes = 2, int64(1<<30)
	defer func() { pendingUploadMax, pendingUploadMaxBytes = origMax, origBytes }()

	now := time.Now()
	for i := 0; i < 4; i++ {
		base := fmt.Sprintf("VIEW%04d", i)
		writePendingFixture(t, dir, base, pendingUploadMeta{FileName: base}, 10, now.Add(time.Duration(i)*time.Minute))
	}

	prunePendingUploads()

	for _, gone := range []string{"VIEW0000", "VIEW0001"} {
		if pendingExists(dir, gone) {
			t.Fatalf("%s should be pruned (oldest first)", gone)
		}
	}
	for _, keep := range []string{"VIEW0002", "VIEW0003"} {
		if !pendingExists(dir, keep) {
			t.Fatalf("%s should be kept", keep)
		}
	}
}

// 元数据损坏的记录无法重传，应最先被淘汰，且不影响正常记录。
func TestPrunePendingUploadsDropsCorruptFirst(t *testing.T) {
	dir := t.TempDir()
	pendingUploadDir = dir
	origMax, origBytes := pendingUploadMax, pendingUploadMaxBytes
	pendingUploadMax, pendingUploadMaxBytes = 2, int64(1<<30)
	defer func() { pendingUploadMax, pendingUploadMaxBytes = origMax, origBytes }()

	now := time.Now()
	// 损坏记录最旧 + 最低优先级（无法解析）
	if err := os.WriteFile(filepath.Join(dir, "BROKEN.json"), []byte("{not-json"), 0600); err != nil {
		t.Fatalf("write broken json: %v", err)
	}
	_ = os.Chtimes(filepath.Join(dir, "BROKEN.json"), now.Add(-10*time.Hour), now.Add(-10*time.Hour))
	writePendingFixture(t, dir, "VIEWOK1", pendingUploadMeta{FileName: "VIEWOK1"}, 10, now.Add(-2*time.Hour))
	writePendingFixture(t, dir, "VIEWOK2", pendingUploadMeta{FileName: "VIEWOK2"}, 10, now.Add(-1*time.Hour))

	prunePendingUploads()

	if pendingExists(dir, "BROKEN") {
		t.Fatalf("corrupt meta should be pruned first")
	}
	for _, keep := range []string{"VIEWOK1", "VIEWOK2"} {
		if !pendingExists(dir, keep) {
			t.Fatalf("%s should be kept", keep)
		}
	}
}
