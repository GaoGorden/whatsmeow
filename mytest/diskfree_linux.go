//go:build linux

package main

import "syscall"

// statusFreeDiskBytes 返回 path 所在文件系统的可用字节数（Linux 走 statfs）。
// ok=false 表示探测失败（路径不存在/权限不足），调用方据此放行、不阻塞下载。
func statusFreeDiskBytes(path string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}
