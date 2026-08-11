//go:build linux

package main

import (
	"os/signal"
	"syscall"
)

// daemonize performs Linux-specific daemon initialization:
//   - ignore SIGPIPE so library logs writing to a broken stdout pipe can't terminate the daemon
//   - create a new session + detach from the JVM process group so systemd's KillMode=process
//     (which only signals the JVM main PID) leaves the Go daemon alive across Java restarts
//
// Windows has no concept of Setsid/SIGPIPE; the no-op stub lives in daemon_other.go.
func daemonize() {
	signal.Ignore(syscall.SIGPIPE)
	// syscall.Setsid() 返回 (pid, err) 两个值；err 忽略（调用失败时进程仍按原进程组运行，由 systemd 兜底）
	_, _ = syscall.Setsid()
}
