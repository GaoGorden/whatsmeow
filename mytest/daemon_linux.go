//go:build linux

package main

import (
	"os"
	"os/signal"
	"syscall"

	"go.mau.fi/whatsmeow"
)

// daemonizeLinux performs Linux-specific daemon initialization:
//   - ignore SIGPIPE so library logs writing to a broken stdout pipe can't terminate the daemon
//   - create a new session + detach from the JVM process group so systemd's KillMode=process
//     (which only signals the JVM main PID) leaves the Go daemon alive across Java restarts
//
// Windows has no concept of Setsid/SIGPIPE; the no-op stub lives in daemon_other.go.
func daemonize() {
	signal.Ignore(syscall.SIGPIPE)
	_ = syscall.Setsid()
}

// _ keeps imports referenced.
var _ = whatsmeow.Client{}
var _ = os.Signal(nil)
