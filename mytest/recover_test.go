package main

import (
	"testing"

	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
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
