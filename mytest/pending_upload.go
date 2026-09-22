package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gabriel-vasile/mimetype"
)

// pendingUploadDir: view-once 媒体本地暂存目录（相对 cwd = 用户目录）。
// Java 掉线导致预签名申请/上传失败时，媒体先暂存于此，Java 重连（attach）后自动重传并通知。
var pendingUploadDir = "pending-upload"

// pendingUploadMax: 暂存上限（条）。溢出丢最旧并记 ERROR（丢弃意味着该 view-once 无法恢复）。
const pendingUploadMax = 100

// pendingUploadMaxBytes: 暂存上限（字节，2GiB）。只看条数挡不住「100 个大视频」——100 × 100MB
// 就是一个用户的磁盘占用上到 10GB。条数与字节任一超标都触发按 mtime 丢最旧。
// 用 var 而非 const：单测需要把它调小才能在不开几 GB 临时文件的前提下验证裁剪逻辑。
var pendingUploadMaxBytes int64 = 2 << 30

// uploadFlushMu 防止 attach 触发与周期触发并发重传同一批文件（避免重复上传/重复通知）。
var uploadFlushMu sync.Mutex

// pendingSourceStatus 标记一条暂存记录来自 Status 动态（Source 为空 = view-once，兼容旧记录）。
const pendingSourceStatus = "status"

// pendingUploadMeta 是暂存媒体的元数据（pending-upload/{base}.json；{base}.bin 为原始媒体字节）。
//
// Source 之后的字段是 **Status 动态专用**：view-once 生成的记录没有这些字段，反序列化后为空，
// flushPendingUploads 据此走各自的补通知分支。两套语义共存是刻意的——复用同一份暂存目录，
// 就同时复用了条数/字节双上限、rename 搬移、attach 与周期两条触发路径。
type pendingUploadMeta struct {
	ObserverId string `json:"observerId"`
	PushName   string `json:"pushName"`
	FileName   string `json:"fileName"`
	FileLength uint64 `json:"fileLength"`
	Seconds    uint32 `json:"seconds"`
	MiniType   string `json:"miniType"`
	SavedAt    string `json:"savedAt"`

	// ---- Status 动态专用 ----
	Source    string `json:"source,omitempty"`
	StatusID  string `json:"statusId,omitempty"`
	MediaKind string `json:"mediaKind,omitempty"`
	Text      string `json:"text,omitempty"`
	ObjectKey string `json:"objectKey,omitempty"`
	ThumbKey  string `json:"thumbKey,omitempty"`
	PostedAt  int64  `json:"postedAt,omitempty"`
}

// savePendingUpload 将上传失败（如 Java 掉线导致预签名申请失败）的 view-once 媒体暂存到本地。
func savePendingUpload(observerId, pushName, fileName, srcPath string, fileLength uint64, seconds uint32, miniType string) error {
	return savePendingUploadWithMeta(pendingUploadMeta{
		ObserverId: observerId,
		PushName:   pushName,
		FileName:   fileName,
		FileLength: fileLength,
		Seconds:    seconds,
		MiniType:   miniType,
	}, srcPath)
}

// savePendingUploadWithMeta 是暂存的完整入口（Status 动态走这里，带 Source/对象键等字段）。
// srcPath 是 downloadToTemp 落盘的临时文件，本函数负责把它「搬」进 pending-upload/
// （rename 优先，跨设备时才退化为 copy）；搬动后 srcPath 不再存在，调用方按惯例照常删除即可。
// 先落 .bin 再写 .json（.json 作为提交标记：存在即代表 .bin 已完整写入）。
func savePendingUploadWithMeta(meta pendingUploadMeta, srcPath string) error {
	dir := pendingUploadDir
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	base := sanitizeFileName(meta.FileName)
	if base == "" {
		base = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	meta.SavedAt = time.Now().UTC().Format(presenceTimeLayout)
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	// 先媒体后元数据：.json 作为提交标记，保证重传时读到的媒体是完整的
	binPath := filepath.Join(dir, base+".bin")
	if err := moveFile(srcPath, binPath); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, base+".json"), metaJSON, 0600); err != nil {
		return err
	}
	prunePendingUploads()
	return nil
}

// moveFile 优先 rename（同设备下 O(1) 且不占内存），跨设备（EXDEV）时退化为流式拷贝 + 删除源文件。
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return os.Remove(src)
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

// prunePendingUploads 超过上限（条数或总字节）时按 mtime 丢最旧
// （记 ERROR：被丢弃的 view-once 无法恢复）。两条上限都要满足才停止淘汰。
func prunePendingUploads() {
	pairs := listPendingPairs()
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].mtime.Before(pairs[j].mtime) })

	total := int64(0)
	for _, p := range pairs {
		total += p.size
	}

	kept := len(pairs)
	bytes := total
	pruned, prunedBytes := 0, int64(0)
	for _, p := range pairs {
		if kept <= pendingUploadMax && bytes <= pendingUploadMaxBytes {
			break
		}
		_ = os.Remove(p.json)
		_ = os.Remove(p.bin)
		kept--
		bytes -= p.size
		pruned++
		prunedBytes += p.size
	}
	if pruned > 0 {
		log.Errorf("pending-upload pruned %d oldest items (%d bytes) to respect caps (count %d, bytes %d); now %d items / %d bytes",
			pruned, prunedBytes, pendingUploadMax, pendingUploadMaxBytes, kept, bytes)
	}
}

// flushPendingUploads 重传所有暂存的媒体：上传成功 → 通知 Java → 删除本地；
// 失败保留，待下次（attach 或周期）重试。调用方需保证 Java 客户端在线（预签名依赖 Server）。
//
// 同时处理两类记录：view-once（Source 为空）与 Status 动态（Source == "status"）。
// 两者的差别只在「对象键怎么算」和「补哪条 proto 通知」，上传/裁剪/触发路径完全共用。
func flushPendingUploads() {
	uploadFlushMu.Lock()
	defer uploadFlushMu.Unlock()

	cleanMediaTmp()

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
		if meta.Source == pendingSourceStatus {
			flushPendingStatus(meta, p)
			continue
		}
		// 流式上传：不再把整份媒体读进堆（100MB 视频 = 一次性 100MB+ 峰值分配）
		mType, err := mimetype.DetectFile(p.bin)
		if err != nil {
			continue // 媒体缺失（异常），保留待下次重试
		}
		miniType := mType.String()
		objectKey := viewOnceObjectKey(meta.FileName, mType.Extension())
		if err := uploadFileToS3(presignViewOnceUploadPath, objectKey, p.bin, miniType, viewOnceUploadTimeout); err != nil {
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

// flushPendingStatus 重传一条暂存的 Status 动态。
//
// 对象键直接取记录里存下的 ObjectKey（首次上传失败前已算好），**不重新推导**——
// 重传与首次上传必须落在同一个键上，否则 S3 会多出一个孤儿、而 DB 里记的是另一个键。
func flushPendingStatus(meta pendingUploadMeta, p pendingPair) {
	if meta.ObjectKey == "" {
		// 没有对象键就无从上传（重试多少次都一样），删掉并记 ERROR 便于追查来源
		log.Errorf("status pending %s has empty objectKey, dropping", p.base)
		_ = os.Remove(p.json)
		_ = os.Remove(p.bin)
		return
	}
	if err := uploadFileToS3(presignStatusUploadPath, meta.ObjectKey, p.bin, meta.MiniType, statusUploadTimeout); err != nil {
		log.Warnf("status pending %s retry upload failed (%v), keep for next retry", p.base, err)
		return
	}
	// 与首次上传走同一条上报路径（outbox 落盘 + Notify），保证重传同样具备跨重启可重放性
	publishStatus(statusPayloadFromPending(meta, meta.ObjectKey))
	_ = os.Remove(p.json)
	_ = os.Remove(p.bin)
	log.Infof("status pending %s replayed to Java (objectKey %s)", p.base, meta.ObjectKey)
}

// mediaTmpStaleAfter: media-tmp 下超过该年龄的文件视为「进程崩在下载中途」的遗留物，直接清掉。
// 正常路径由 uploadAndNotify 的 defer 负责删除，这里的清理对象只有异常退出留下的孤儿。
const mediaTmpStaleAfter = time.Hour

// cleanMediaTmp 清理 media-tmp 下的陈旧遗留文件（目录不存在时直接返回）。
func cleanMediaTmp() {
	entries, err := os.ReadDir(mediaTmpDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-mediaTmpStaleAfter)
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(mediaTmpDir, e.Name())); err == nil {
			removed++
		}
	}
	if removed > 0 {
		log.Infof("media-tmp cleaned %d stale file(s)", removed)
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

// listPendingPairs 列出暂存目录下的待重传项（以 .json 为准，含 .bin 路径、mtime 与字节数）。
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
		binPath := filepath.Join(pendingUploadDir, base+".bin")
		var size int64
		if binInfo, err := os.Stat(binPath); err == nil {
			size = binInfo.Size()
		}
		pairs = append(pairs, pendingPair{
			json:  filepath.Join(pendingUploadDir, e.Name()),
			bin:   binPath,
			base:  base,
			mtime: info.ModTime(),
			size:  size,
		})
	}
	return pairs
}

type pendingPair struct {
	json  string
	bin   string
	base  string
	mtime time.Time
	size  int64
}
