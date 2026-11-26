package main

import (
	"strconv"
	"strings"
)

// parseSmapsRollup 解析 /proc/self/smaps_rollup 文本，返回（MB）：
//
//	rss         = Rss（驻留，含共享代码页；跨进程重复计数，受 page cache 回收影响）
//	pss         = Pss（共享页按比例均摊）
//	shared      = Shared_Clean + Shared_Dirty
//	privateDirty= Private_Dirty（**匿名内存：Go heap arena / cgo+SQLite arena / 栈**）
//
// 为什么要单独报 privateDirty：Rss 里包含文件页（代码段）。每个用户目录各有一份
// linux_go 副本（独立 inode），代码页不能跨进程共享，但在内存紧张的节点上会被内核
// 回收 —— 于是同样的进程在忙碌节点上 Rss 只有 ~18MB、在空闲节点上却有 ~34MB。
// 只有 Private_Dirty（匿名）是不可回收的、真正决定 OOM 的部分，也是容量估算应该用的口径。
//
// 缺失的字段返回 -1；文本为空时 ok=false（调用方回落其它来源）。
func parseSmapsRollup(s string) (rssMB, pssMB, sharedMB, privateDirtyMB int64, ok bool) {
	rssMB, pssMB, sharedMB, privateDirtyMB = -1, -1, -1, -1
	var sharedClean, sharedDirty int64 = -1, -1
	seen := false
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "Rss:":
			rssMB = kb / 1024
			seen = true
		case "Pss:":
			pssMB = kb / 1024
		case "Shared_Clean:":
			sharedClean = kb
		case "Shared_Dirty:":
			sharedDirty = kb
		case "Private_Dirty:":
			privateDirtyMB = kb / 1024
		}
	}
	if sharedClean >= 0 && sharedDirty >= 0 {
		sharedMB = (sharedClean + sharedDirty) / 1024
	}
	return rssMB, pssMB, sharedMB, privateDirtyMB, seen
}
