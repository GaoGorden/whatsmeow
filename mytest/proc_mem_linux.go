//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
)

// parseVmRSS 从 /proc/self/status 文本中解析 VmRSS（kB）。
func parseVmRSS(s string) (int64, bool) {
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				if kb, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					return kb, true
				}
			}
		}
	}
	return 0, false
}

// readProcMem 返回进程物理内存（MB）：rss(驻留含共享，跨进程重复计数)、pss(共享页均摊=真实物理成本)、
// shared(共享页物理占用)。一次读 /proc/self/smaps_rollup 取 Rss/Pss/Shared_Clean/Shared_Dirty。
// 不可用（内核<4.14 / 读失败）返回 -1，web 显示「—」。
func readProcMem() (rssMB, pssMB, sharedMB int64) {
	rssMB, pssMB, sharedMB = -1, -1, -1
	data, err := os.ReadFile("/proc/self/smaps_rollup")
	if err != nil {
		// 兜底：smaps_rollup 缺失时退回 status 的 VmRSS；PSS/共享仍 -1
		if st, err := os.ReadFile("/proc/self/status"); err == nil {
			if kb, ok := parseVmRSS(string(st)); ok {
				rssMB = kb / 1024
			}
		}
		return
	}
	var sharedClean, sharedDirty int64
	for _, line := range strings.Split(string(data), "\n") {
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
		case "Pss:":
			pssMB = kb / 1024
		case "Shared_Clean:":
			sharedClean = kb
		case "Shared_Dirty:":
			sharedDirty = kb
		}
	}
	if sharedClean >= 0 && sharedDirty >= 0 {
		sharedMB = (sharedClean + sharedDirty) / 1024
	}
	return
}
