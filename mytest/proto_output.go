package main

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
)

// PROTO_PREFIX is the unique prefix that separates protocol messages from log output.
// Java Server only parses lines starting with this prefix as protocol messages.
// All other stdout lines are treated as diagnostic logs.
const PROTO_PREFIX = "##PROTO##"

// Protocol message type constants
const (
	MsgPresence        = "presence"
	MsgReadReceipt     = "readReceipt"
	MsgReceivedMessage = "receivedMessage"
	MsgCheckUser       = "checkUser"
	MsgGetAvatar       = "getAvatar"
	MsgGetAvatarFail   = "getAvatarFail"
	MsgLoginSuccess    = "loginSuccess"
	MsgPushName        = "pushName"
	MsgPhoneNumber     = "phoneNumber"
	MsgQrCode          = "qrCode"
	MsgLinkingCode     = "linkingCode"
	MsgQrTimeout       = "qrTimeout"
	MsgLogoutSuccess   = "logoutSuccess"
	MsgViewOnceFile    = "viewOnceFile"
	MsgViewOnceEnabled = "viewOnceEnabled"
	MsgPairError       = "pairError"

	// Stability monitoring messages
	MsgHeartbeat      = "heartbeat"      // Periodic health report (goroutines, memory, subscribers)
	MsgResubscribe    = "resubscribe"    // Result of auto-resubscribe after reconnect
	MsgStreamReplaced = "streamReplaced" // Session taken over by another device
	MsgLoggedOut      = "loggedOut"      // Logged out event from WhatsApp server
)

// protoBackend abstracts where ProtoOutput writes protocol messages.
// Java 统一走 daemon 模式（--socket）：消息写回当前连接的 Java client；
// 仅手动终端调试（未传 --socket）时回退到 stdout。
//
// 关键约束：Java 重启时 socket 断开，写失败必须静默忽略，绝不 panic 或阻塞事件 handler
// goroutine——Java 重连后 backend 切到新 conn 继续写。
type protoBackend interface {
	write(line string)
}

// stdoutBackend writes to os.Stdout (manual terminal debugging without --socket).
type stdoutBackend struct{}

func (stdoutBackend) write(line string) {
	fmt.Print(line)
}

// socketBackend writes to the current Java client connection.
// Writes are guarded by a mutex to prevent interleaving across event-handler goroutines.
// 注意：不能用 atomic.Value 存连接——Go 禁止 Store/Swap nil 进 Value（会 panic），
// 而 Java 断开连接时需要清空字段，故用普通 net.Conn 字段 + mutex 即可（所有访问已持锁）。
type socketBackend struct {
	mu   sync.Mutex
	conn net.Conn
}

var globalSocketBackend = &socketBackend{}

// setProtoConn installs the connection used by the socket backend (Java attached/re-attached).
func setProtoConn(conn net.Conn) {
	globalSocketBackend.mu.Lock()
	old := globalSocketBackend.conn
	globalSocketBackend.conn = conn
	globalSocketBackend.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// clearProtoConn drops the current connection (Java disconnected).
func clearProtoConn() {
	globalSocketBackend.mu.Lock()
	old := globalSocketBackend.conn
	globalSocketBackend.conn = nil
	globalSocketBackend.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

func (b *socketBackend) write(line string) {
	b.mu.Lock()
	conn := b.conn
	b.mu.Unlock()
	if conn == nil {
		// No Java client connected yet (e.g., during Java restart). Drop silently —
		// Java will re-attach; critical events like loginSuccess are re-driven by
		// PresenceManager resubscribe / cli auto-reconnect after re-attach.
		return
	}
	// Best-effort write; if the pipe is broken, Java side will detect EOF and reconnect.
	_, err := conn.Write([]byte(line))
	if err != nil {
		// Connection broken — drop this conn; Java will reconnect and re-attach.
		b.mu.Lock()
		if b.conn == conn {
			b.conn = nil
		}
		b.mu.Unlock()
		_ = conn.Close()
	}
}

// activeBackend is chosen at startup: socket backend in daemon mode, stdout otherwise.
var activeBackend protoBackend = stdoutBackend{}

// setDaemonBackend switches the active backend to the socket backend (called when daemonMode is set).
func setDaemonBackend() {
	activeBackend = globalSocketBackend
}

// ProtoOutput writes a structured JSON protocol message.
// Each message is a single line: ##PROTO##{"type":"...","field":"value",...}
// The output is atomic (single write call) to prevent interleaving with log output.
func ProtoOutput(msgType string, data map[string]any) {
	data["type"] = msgType
	b, err := json.Marshal(data)
	if err != nil {
		// Fallback: output error as diagnostic log, not as proto message
		fmt.Printf("ProtoOutput marshal error for type %s: %v\n", msgType, err)
		return
	}
	activeBackend.write(PROTO_PREFIX + string(b) + "\n")
}
