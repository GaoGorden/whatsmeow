package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
)

// pairFailureOf：events.PairError.Error → 结构化原因码 + status。
// 这些码由 Java 的 LinkFailureClassifier 映射成 link_attempts.reason。
func TestPairFailureOf(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantReason string
		wantStatus int
	}{
		{
			name:       "invalid device identity HMAC",
			err:        whatsmeow.ErrPairInvalidDeviceIdentityHMAC,
			wantReason: "pair_hmac_mismatch",
			wantStatus: 401,
		},
		{
			name:       "invalid device signature",
			err:        whatsmeow.ErrPairInvalidDeviceSignature,
			wantReason: "pair_signature_mismatch",
			wantStatus: 401,
		},
		{
			name:       "wrapped HMAC error",
			err:        fmt.Errorf("pair failed: %w", whatsmeow.ErrPairInvalidDeviceIdentityHMAC),
			wantReason: "pair_hmac_mismatch",
			wantStatus: 401,
		},
		{
			name:       "proto error",
			err:        &whatsmeow.PairProtoError{Message: "failed to parse device identity container", ProtoErr: errors.New("bad wire")},
			wantReason: "pair_proto_error",
			wantStatus: 500,
		},
		{
			name:       "database error",
			err:        &whatsmeow.PairDatabaseError{Message: "failed to store main device identity", DBErr: errors.New("disk full")},
			wantReason: "pair_db_error",
			wantStatus: 500,
		},
		{
			name:       "rejected locally",
			err:        whatsmeow.ErrPairRejectedLocally,
			wantReason: "pair_rejected_locally",
			wantStatus: 500,
		},
		{
			name:       "unknown error",
			err:        errors.New("something new from WhatsApp"),
			wantReason: "pair_completion_failed",
			wantStatus: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason, status := pairFailureOf(c.err)
			if reason != c.wantReason {
				t.Fatalf("reason = %q, want %q", reason, c.wantReason)
			}
			if status != c.wantStatus {
				t.Fatalf("status = %d, want %d", status, c.wantStatus)
			}
		})
	}
}

// elapsedFor：未出码按 startedAt、已出码按 codeIssuedAt 计算，负值归零。
// Java 用它区分「配对码过期」与「出码后网络中断」。
func TestElapsedFor(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	started := now.Add(-10 * time.Second)
	codeAt := now.Add(-2 * time.Second)

	cases := []struct {
		name       string
		startedAt  time.Time
		codeAt     time.Time
		codeIssued bool
		wantMs     int64
		wantIssued bool
	}{
		{
			name:       "before code, use startedAt",
			startedAt:  started,
			codeAt:     time.Time{},
			codeIssued: false,
			wantMs:     10000,
			wantIssued: false,
		},
		{
			name:       "after code, use codeIssuedAt",
			startedAt:  started,
			codeAt:     codeAt,
			codeIssued: true,
			wantMs:     2000,
			wantIssued: true,
		},
		{
			name:       "zero base",
			startedAt:  time.Time{},
			codeAt:     time.Time{},
			codeIssued: false,
			wantMs:     0,
			wantIssued: false,
		},
		{
			name:       "clock skew never returns negative",
			startedAt:  now.Add(time.Second),
			codeAt:     time.Time{},
			codeIssued: false,
			wantMs:     0,
			wantIssued: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ms, issued := elapsedFor(c.startedAt, c.codeAt, c.codeIssued, now)
			if ms != c.wantMs {
				t.Fatalf("ms = %d, want %d", ms, c.wantMs)
			}
			if issued != c.wantIssued {
				t.Fatalf("issued = %v, want %v", issued, c.wantIssued)
			}
		})
	}
}
