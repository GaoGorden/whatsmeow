//go:build !linux

package main

import (
	"os"

	"github.com/mdp/qrterminal/v3"
)

// printQRCode 在非 Linux 环境下（Windows、macOS 等）将 QR 码输出到终端
// 方便开发调试时扫码登录
func printQRCode(code string) {
	qrterminal.GenerateHalfBlock(code, qrterminal.L, os.Stdout)
}
