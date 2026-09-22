//go:build !linux

package main

// statusFreeDiskBytes 在非 Linux（Windows 本地联调 / darwin）无法用 statfs 探测，
// 返回 ok=false —— 调用方 statusDiskLow 据此放行，磁盘水位保护只在生产 Linux 生效。
func statusFreeDiskBytes(path string) (uint64, bool) {
	return 0, false
}
