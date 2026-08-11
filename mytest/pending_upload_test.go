package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	if err := savePendingUpload("obs1", "nick", "MSGAABB", payload, uint64(len(payload)), 0, "image/jpeg"); err != nil {
		t.Fatalf("savePendingUpload failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "MSGAABB.bin")); err != nil {
		t.Fatalf("expected pending .bin written: %v", err)
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
	if err := savePendingUpload("obs", "n", "KEEP01", payload, uint64(len(payload)), 0, "image/png"); err != nil {
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
		if err := savePendingUpload("obs", "n", fileName, []byte("d"), 1, 0, "image/png"); err != nil {
			t.Fatalf("save %d failed: %v", i, err)
		}
	}
	if got := len(listPendingPairs()); got > pendingUploadMax {
		t.Fatalf("expected capped at %d, got %d", pendingUploadMax, got)
	}
}
