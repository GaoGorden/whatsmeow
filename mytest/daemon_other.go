//go:build !linux

package main

// daemonize is a no-op on non-Linux (Windows local debugging / macOS dev).
// Socket mode (--socket) works there unchanged; only the process-group detach
// (Setsid) and SIGPIPE ignore are Linux-specific and not needed on Windows.
func daemonize() {}
