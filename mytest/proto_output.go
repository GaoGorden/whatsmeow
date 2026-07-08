package main

import (
	"encoding/json"
	"fmt"
)

// PROTO_PREFIX is the unique prefix that separates protocol messages from log output.
// Java Server only parses lines starting with this prefix as protocol messages.
// All other stdout lines are treated as diagnostic logs.
const PROTO_PREFIX = "##PROTO##"

// Protocol message type constants
const (
	MsgPresence         = "presence"
	MsgReadReceipt      = "readReceipt"
	MsgReceivedMessage  = "receivedMessage"
	MsgCheckUser        = "checkUser"
	MsgGetAvatar        = "getAvatar"
	MsgGetAvatarFail    = "getAvatarFail"
	MsgLoginSuccess     = "loginSuccess"
	MsgPushName         = "pushName"
	MsgPhoneNumber      = "phoneNumber"
	MsgQrCode           = "qrCode"
	MsgLinkingCode      = "linkingCode"
	MsgQrTimeout        = "qrTimeout"
	MsgLogoutSuccess    = "logoutSuccess"
	MsgViewOnceFile     = "viewOnceFile"
	MsgViewOnceEnabled  = "viewOnceEnabled"
	MsgPairError        = "pairError"
)

// ProtoOutput writes a structured JSON protocol message to stdout.
// Each message is a single line: ##PROTO##{"type":"...","field":"value",...}
// The output is atomic (single fmt.Printf call) to prevent interleaving with log output.
func ProtoOutput(msgType string, data map[string]any) {
	data["type"] = msgType
	b, err := json.Marshal(data)
	if err != nil {
		// Fallback: output error as diagnostic log, not as proto message
		fmt.Printf("ProtoOutput marshal error for type %s: %v\n", msgType, err)
		return
	}
	fmt.Printf("%s%s\n", PROTO_PREFIX, string(b))
}
