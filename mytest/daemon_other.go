//go:build !linux

package main

// daemonize is a no-op on non-Linux (Windows local debugging / macOS dev).
// The legacy stdin/stdout pipe mode is used there; no Setsid/SIGPIPE handling needed.
func daemonize() {}
