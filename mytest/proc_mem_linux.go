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

// readProcMem 返回进程内存（MB）：rss(驻留含共享，跨进程重复计数且受 page cache 回收影响)、
// pss(共享页均摊)、shared(共享页)、dirty(Private_Dirty 匿名内存，不可回收)。
// 一次读 /proc/self/smaps_rollup；解析逻辑见 parseSmapsRollup（纯函数，有单测）。
// 不可用（内核<4.14 / 读失败）返回 -1，web 显示「—」。
func readProcMem() (rssMB, pssMB, sharedMB, dirtyMB int64) {
	data, err := os.ReadFile("/proc/self/smaps_rollup")
	if err != nil {
		// 兜底：smaps_rollup 缺失时退回 status 的 VmRSS；PSS/共享/匿名仍 -1
		rssMB, pssMB, sharedMB, dirtyMB = -1, -1, -1, -1
		if st, err := os.ReadFile("/proc/self/status"); err == nil {
			if kb, ok := parseVmRSS(string(st)); ok {
				rssMB = kb / 1024
			}
		}
		return
	}
	rssMB, pssMB, sharedMB, dirtyMB, _ = parseSmapsRollup(string(data))
	return
}
