package main

import (
	"testing"

	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestRecoverClassify(t *testing.T) {
	cases := []struct {
		name      string
		msg       *waE2E.Message
		wantKind  string
		wantMedia bool
	}{
		{
			name:     "conversation is text without media",
			msg:      &waE2E.Message{Conversation: proto.String("hello")},
			wantKind: "text",
		},
		{
			name:     "extended text is text",
			msg:      &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("hi")}},
			wantKind: "text",
		},
		{
			name: "image",
			msg: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
				Mimetype: proto.String("image/jpeg"), FileLength: proto.Uint64(10),
			}},
			wantKind: "image", wantMedia: true,
		},
		{
			name: "video",
			msg: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
				Mimetype: proto.String("video/mp4"), FileLength: proto.Uint64(20), Seconds: proto.Uint32(3),
			}},
			wantKind: "video", wantMedia: true,
		},
		{
			name: "audio",
			msg: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
				Mimetype: proto.String("audio/ogg"), FileLength: proto.Uint64(30),
			}},
			wantKind: "audio", wantMedia: true,
		},
		{
			name: "document",
			msg: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
				Mimetype: proto.String("application/pdf"), FileName: proto.String("a.pdf"),
			}},
			wantKind: "document", wantMedia: true,
		},
		{
			name: "sticker",
			msg: &waE2E.Message{StickerMessage: &waE2E.StickerMessage{
				Mimetype: proto.String("image/webp"),
			}},
			wantKind: "sticker", wantMedia: true,
		},
		{
			name: "ptv uses video message",
			msg: &waE2E.Message{PtvMessage: &waE2E.VideoMessage{
				Mimetype: proto.String("video/mp4"),
			}},
			wantKind: "ptv", wantMedia: true,
		},
		{
			name:     "unknown/empty is dropped",
			msg:      &waE2E.Message{},
			wantKind: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, media, _, _, _, _, _ := recoverClassify(tc.msg)
			if kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", kind, tc.wantKind)
			}
			if (media != nil) != tc.wantMedia {
				t.Fatalf("media present = %v, want %v", media != nil, tc.wantMedia)
			}
		})
	}
}

func TestRecoverEditedTextAndKind(t *testing.T) {
	text, kind := recoverEditedTextAndKind(&waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("edited")},
	})
	if text != "edited" || kind != "text" {
		t.Fatalf("got (%q, %q), want (\"edited\", \"text\")", text, kind)
	}

	text, kind = recoverEditedTextAndKind(&waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{Caption: proto.String("new caption")},
	})
	if text != "new caption" || kind != "image" {
		t.Fatalf("got (%q, %q), want (\"new caption\", \"image\")", text, kind)
	}
}

// Meta AI 等 bot 消息的编辑 payload 包在 botInvokeMessage（FutureProofMessage）里
// （常见形态 `editedMessage{botInvokeMessage{...}}`）。不解这层会返回 ("", "unknown")，
// Server 会把一次空编辑落成永久行 —— 2026-11-25 线上：Meta AI 会话整页「无编辑历史」。
func TestRecoverEditedTextUnwrapsBotInvoke(t *testing.T) {
	answer := func() *waE2E.Message { return &waE2E.Message{Conversation: proto.String("AI answer")} }
	cases := []struct {
		name string
		msg  *waE2E.Message
	}{
		{
			name: "protocol edit -> botInvoke -> conversation",
			msg: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
				EditedMessage: &waE2E.Message{BotInvokeMessage: &waE2E.FutureProofMessage{
					Message: answer(),
				}},
			}},
		},
		{
			name: "editedMessage -> botInvoke -> extendedText",
			msg: &waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{BotInvokeMessage: &waE2E.FutureProofMessage{
					Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
						Text: proto.String("AI answer"),
					}},
				}},
			}},
		},
		{
			name: "botInvoke -> deviceSent -> conversation",
			msg: &waE2E.Message{BotInvokeMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{
					Message: answer(),
				}},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, kind := recoverEditedTextAndKind(tc.msg)
			if text != "AI answer" || kind != "text" {
				t.Fatalf("got (%q, %q), want (\"AI answer\", \"text\")", text, kind)
			}
		})
	}
}

// bot 会话整类排除：Meta AI 等会以「反复编辑同一条消息」流式输出，每个 chunk 都是一次 edit。
// Sender 与 Chat 任一命中都要算 —— bot 消息既可能用 @bot，也可能映射到 PN JID。
func TestRecoverIsBotChat(t *testing.T) {
	human := types.NewJID("6281234567890", types.DefaultUserServer)
	botPN := types.NewJID("13135550002", types.DefaultUserServer)
	botJID := types.NewJID("867051314767696", types.BotServer)
	cases := []struct {
		name string
		evt  *events.Message
		want bool
	}{
		{"human chat", &events.Message{Info: types.MessageInfo{
			MessageSource: types.MessageSource{Sender: human, Chat: human},
		}}, false},
		{"bot PN as sender", &events.Message{Info: types.MessageInfo{
			MessageSource: types.MessageSource{Sender: botPN, Chat: botPN},
		}}, true},
		{"bot server JID as chat", &events.Message{Info: types.MessageInfo{
			MessageSource: types.MessageSource{Sender: human, Chat: botJID},
		}}, true},
		{"nil event", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := recoverIsBotChat(tc.evt); got != tc.want {
				t.Fatalf("recoverIsBotChat = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRecoverIsViewOnce(t *testing.T) {
	if !recoverIsViewOnce(&events.Message{IsViewOnce: true}) {
		t.Fatal("IsViewOnce=true should be detected")
	}
	if !recoverIsViewOnce(&events.Message{Message: &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{ViewOnce: proto.Bool(true)},
	}}) {
		t.Fatal("media-level GetViewOnce()=true should be detected")
	}
	if recoverIsViewOnce(&events.Message{Message: &waE2E.Message{
		Conversation: proto.String("hi"),
	}}) {
		t.Fatal("plain text must not be view-once")
	}
}

func TestRecoverAckIDIsPerEvent(t *testing.T) {
	a := recoverAckID("msg1", "edit", 100)
	b := recoverAckID("msg1", "edit", 101)
	c := recoverAckID("msg1", "revoke", 100)
	if a == b || a == c || b == c {
		t.Fatalf("ackIds must differ per event: %q %q %q", a, b, c)
	}
	if recoverAckID("msg1", "edit", 100) != a {
		t.Fatal("ackId must be stable for the same event")
	}
}

func TestRecoverExt(t *testing.T) {
	cases := []struct {
		mime string
		file string
		want string
	}{
		{"image/jpeg", "", ".jpg"},
		{"video/mp4", "", ".mp4"},
		{"audio/ogg", "", ".ogg"},
		{"application/pdf", "report.PDF", ".pdf"},
		{"application/octet-stream", "", ".bin"},
	}
	for _, tc := range cases {
		if got := recoverExt(tc.mime, tc.file); got != tc.want {
			t.Fatalf("recoverExt(%q, %q) = %q, want %q", tc.mime, tc.file, got, tc.want)
		}
	}
}

// 单文件磁盘保护：最多占用一半剩余空间；大小未知/磁盘不可探测时放行。
func TestMediaFitsDisk(t *testing.T) {
	cases := []struct {
		name string
		free uint64
		ok   bool
		size uint64
		want bool
	}{
		{"unknown size passes", 1000, true, 0, true},
		{"undetectable disk passes", 0, false, 1 << 30, true},
		{"exactly half passes", 1000, true, 500, true},
		{"over half rejected", 1000, true, 501, false},
		{"empty disk rejects any file", 0, true, 1, false},
	}
	for _, tc := range cases {
		if got := mediaFitsDisk(tc.free, tc.ok, tc.size); got != tc.want {
			t.Fatalf("%s: mediaFitsDisk(%d, %v, %d) = %v, want %v",
				tc.name, tc.free, tc.ok, tc.size, got, tc.want)
		}
	}
}
