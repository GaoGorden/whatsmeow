package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gabriel-vasile/mimetype"
)

// pendingUploadDir: view-once 媒体本地暂存目录（相对 cwd = 用户目录）。
// Java 掉线导致预签名申请/上传失败时，媒体先暂存于此，Java 重连（attach）后自动重传并通知。
var pendingUploadDir = "pending-upload"

// pendingUploadMax: 暂存上限（条）。溢出丢最旧并记 ERROR（丢弃意味着该 view-once 无法恢复）。
const pendingUploadMax = 100

// uploadFlushMu 防止 attach 触发与周期触发并发重传同一批文件（避免重复上传/重复通知）。
var uploadFlushMu sync.Mutex

// pendingUploadMeta 是暂存媒体的元数据（pending-upload/{base}.json；{base}.bin 为原始媒体字节）。
type pendingUploadMeta struct {
	ObserverId string `json:"observerId"`
	PushName   string `json:"pushName"`
	FileName   string `json:"fileName"`
	FileLength uint64 `json:"fileLength"`
	Seconds    uint32 `json:"seconds"`
	MiniType   string `json:"miniType"`
	SavedAt    string `json:"savedAt"`
}

// savePendingUpload 将上传失败（如 Java 掉线导致预签名申请失败）的 view-once 媒体暂存到本地。
// 先写 .bin 再写 .json（.json 作为提交标记：存在即代表 .bin 已完整写入）。
func savePendingUpload(observerId, pushName, fileName string, fileData []byte, fileLength uint64, seconds uint32, miniType string) error {
	dir := pendingUploadDir
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	base := sanitizeFileName(fileName)
	if base == "" {
		base = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	meta := pendingUploadMeta{
		ObserverId: observerId,
		PushName:   pushName,
		FileName:   fileName,
		FileLength: fileLength,
		Seconds:    seconds,
		MiniType:   miniType,
		SavedAt:    time.Now().UTC().Format(presenceTimeLayout),
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	// 先媒体后元数据：.json 作为提交标记，保证重传时读到的媒体是完整的
	if err := os.WriteFile(filepath.Join(dir, base+".bin"), fileData, 0600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, base+".json"), metaJSON, 0600); err != nil {
		return err
	}
	prunePendingUploads()
	return nil
}

// sanitizeFileName 将消息 ID 等名称清理为安全的文件名（防路径穿越/非法字符）。
func sanitizeFileName(name string) string {
	s := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(name)
	s = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return '_'
		}
		return r
	}, s)
	return s
}

// prunePendingUploads 超过上限时按 mtime 丢最旧（记 ERROR：被丢弃的 view-once 无法恢复）。
func prunePendingUploads() {
	pairs := listPendingPairs()
	if len(pairs) <= pendingUploadMax {
		return
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].mtime.Before(pairs[j].mtime) })
	overflow := len(pairs) - pendingUploadMax
	for i := 0; i < overflow; i++ {
		_ = os.Remove(pairs[i].json)
		_ = os.Remove(pairs[i].bin)
	}
	log.Errorf("pending-upload pruned %d oldest items (cap %d)", overflow, pendingUploadMax)
}

// flushPendingUploads 重传所有暂存的 view-once 媒体：上传成功 → 通知 Java → 删除本地；
// 失败保留，待下次（attach 或周期）重试。调用方需保证 Java 客户端在线（预签名依赖 Server）。
func flushPendingUploads() {
	uploadFlushMu.Lock()
	defer uploadFlushMu.Unlock()

	for _, p := range listPendingPairs() {
		metaJSON, err := os.ReadFile(p.json)
		if err != nil {
			continue
		}
		var meta pendingUploadMeta
		if err := json.Unmarshal(metaJSON, &meta); err != nil {
			log.Warnf("pending-upload %s bad json, deleting: %v", p.base, err)
			_ = os.Remove(p.json)
			_ = os.Remove(p.bin)
			continue
		}
		data, err := os.ReadFile(p.bin)
		if err != nil {
			continue // 媒体缺失（异常），保留待下次重试
		}
		mType := mimetype.Detect(data)
		miniType := mType.String()
		objectKey := viewOnceObjectKey(meta.FileName, mType.Extension())
		if err := uploadToS3(objectKey, data, miniType); err != nil {
			log.Warnf("pending-upload %s retry upload failed (%v), keep for next retry", p.base, err)
			continue
		}
		// 上传成功：先通知 Java（Notify 在连接又断开时自动缓冲）再删本地，避免丢通知
		presenceCache.Notify(MsgViewOnceFile, map[string]any{
			"observerId": meta.ObserverId,
			"pushName":   meta.PushName,
			"miniType":   miniType,
			"fileLength": meta.FileLength,
			"seconds":    meta.Seconds,
			"objectKey":  objectKey,
		})
		_ = os.Remove(p.json)
		_ = os.Remove(p.bin)
		log.Infof("pending-upload %s replayed to Java (objectKey %s)", p.base, objectKey)
	}
}

// viewOnceObjectKey 构造 view-once 媒体在 S3 的 objectKey。
// cli 为 nil（如单元测试）时降级为 "unknown"，不影响生产路径。
func viewOnceObjectKey(fileName, ext string) string {
	jid := "unknown"
	if cli != nil && cli.Store != nil {
		jid = cli.Store.GetJID().String()
	}
	return "whatsapp/view-once/" + jid + "/" + fileName + ext
}

// listPendingPairs 列出暂存目录下的待重传项（以 .json 为准，含 .bin 路径与 mtime）。
func listPendingPairs() []pendingPair {
	var pairs []pendingPair
	entries, err := os.ReadDir(pendingUploadDir)
	if err != nil {
		return pairs // 目录不存在 = 无待重传
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".json")
		pairs = append(pairs, pendingPair{
			json:  filepath.Join(pendingUploadDir, e.Name()),
			bin:   filepath.Join(pendingUploadDir, base+".bin"),
			base:  base,
			mtime: info.ModTime(),
		})
	}
	return pairs
}

type pendingPair struct {
	json  string
	bin   string
	base  string
	mtime time.Time
}
