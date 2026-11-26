package main

import "testing"

// 真实 /proc/self/smaps_rollup 的字段顺序与缩进（值为 2026-11-26 线上样本的量级）。
const smapsSample = `55f0c0000000-55f0c1a00000 r-xp 00000000 08:01 1234567    /root/lshome/userFile/XXXXX/linux_go
Size:              16960 kB
KernelPageSize:        4 kB
MMUPageSize:           4 kB
Rss:               16960 kB
Pss:               16940 kB
Shared_Clean:       1816 kB
Shared_Dirty:          0 kB
Private_Clean:     16600 kB
Private_Dirty:       360 kB
Referenced:        12000 kB
Anonymous:           360 kB
LazyFree:              0 kB
AnonHugePages:         0 kB
ShmemPmdMapped:        0 kB
FilePmdMapped:         0 kB
Shared_Hugetlb:        0 kB
Private_Hugetlb:       0 kB
Swap:                  0 kB
SwapPss:               0 kB
Locked:                0 kB
THPeligible:           0
VmFlags: rd ex mr mw me sd
`

func TestParseSmapsRollup(t *testing.T) {
	rss, pss, shared, dirty, ok := parseSmapsRollup(smapsSample)
	if !ok {
		t.Fatal("expected ok for a valid smaps_rollup")
	}
	if rss != 16 { // 16960 kB
		t.Fatalf("rss=%d, want 16", rss)
	}
	if pss != 16 { // 16940 kB
		t.Fatalf("pss=%d, want 16", pss)
	}
	if shared != 1 { // 1816 kB
		t.Fatalf("shared=%d, want 1", shared)
	}
	// 关键：dirty 取 Private_Dirty（360 kB → 0MB），不能误取 Anonymous 之外的行。
	if dirty != 0 {
		t.Fatalf("dirty=%d, want 0", dirty)
	}
}

func TestParseSmapsRollupMissingFields(t *testing.T) {
	// 只有 Rss 时其余为 -1（web 显示「—」，容量估算回落）
	rss, pss, shared, dirty, ok := parseSmapsRollup("Rss: 20480 kB\n")
	if !ok || rss != 20 {
		t.Fatalf("rss=%d ok=%v, want 20/true", rss, ok)
	}
	if pss != -1 || shared != -1 || dirty != -1 {
		t.Fatalf("pss/shared/dirty = %d/%d/%d, want -1/-1/-1", pss, shared, dirty)
	}
}

func TestParseSmapsRollupEmpty(t *testing.T) {
	if _, _, _, _, ok := parseSmapsRollup(""); ok {
		t.Fatal("empty input must not be ok")
	}
}

func TestParseSmapsRollupDirtyValue(t *testing.T) {
	// 典型线上形态：Rss 34MB 里 17MB 是可回收代码页，匿名只有 15MB
	s := "Rss: 34816 kB\nPss: 33000 kB\nShared_Clean: 1816 kB\nShared_Dirty: 0 kB\nPrivate_Dirty: 15360 kB\n"
	rss, _, shared, dirty, ok := parseSmapsRollup(s)
	if !ok || rss != 34 || shared != 1 || dirty != 15 {
		t.Fatalf("got rss=%d shared=%d dirty=%d ok=%v", rss, shared, dirty, ok)
	}
}
