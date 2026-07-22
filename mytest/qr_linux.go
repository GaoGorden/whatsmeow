//go:build linux

package main

// printQRCode 在 Linux 环境下不执行任何操作
// 因为 Linux 服务器上通常没有终端，也不需要显示 QR 码
// QR 码数据已通过 ProtoOutput 输出给 Java Server 处理
func printQRCode(code string) {
	// Linux 环境：不做任何操作
	// QR 码数据已通过 ##PROTO## 协议发送给 Java Server
}
