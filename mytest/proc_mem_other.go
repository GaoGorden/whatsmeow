//go:build !linux && !windows

package main

// readProcMem 在无 /proc 且无 Windows API 的平台（如 darwin/bsd）返回 -1，web 显示「—」。
func readProcMem() (rssMB, pssMB, sharedMB int64) {
	return -1, -1, -1
}
