//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	procGetProcessMemoryInfo = syscall.NewLazyDLL("psapi.dll").NewProc("GetProcessMemoryInfo")
	procGetCurrentProcess    = syscall.NewLazyDLL("kernel32.dll").NewProc("GetCurrentProcess")
)

// processMemoryCounters 对应 Win32 PROCESS_MEMORY_COUNTERS。SIZE_T 字段用 uintptr，
// DWORD 字段用 uint32。
type processMemoryCounters struct {
	CB                        uint32
	PageFaultCount            uint32
	PeakWorkingSetSize        uintptr
	WorkingSetSize            uintptr
	QuotaPeakPagedPoolUsage   uintptr
	QuotaPagedPoolUsage       uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage    uintptr
	PagefileUsage             uintptr
	PeakPagefileUsage         uintptr
}

// readProcMem 返回进程物理内存（MB）。Windows 用 GetProcessMemoryInfo 取 WorkingSet
// （= RSS 等价口径，含共享代码段，跨进程重复计数）。Windows 无 PSS/共享页均摊概念，
// pss/shared 返回 -1（web 显示「—」）。rss 读失败同样返回 -1。
func readProcMem() (rssMB, pssMB, sharedMB int64) {
	rssMB, pssMB, sharedMB = -1, -1, -1
	h, _, _ := procGetCurrentProcess.Call()
	var c processMemoryCounters
	c.CB = uint32(unsafe.Sizeof(c))
	if ret, _, _ := procGetProcessMemoryInfo.Call(h, uintptr(unsafe.Pointer(&c)), uintptr(c.CB)); ret != 0 {
		rssMB = int64(c.WorkingSetSize) / 1024 / 1024
	}
	return
}
