package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
)

// PresenceManager tracks which contacts have been subscribed for presence updates.
// On automatic reconnect, the WhatsApp server does not preserve presence subscriptions,
// so we must re-subscribe all tracked JIDs after each reconnect event.
type PresenceManager struct {
	mu             sync.RWMutex
	subscribedJIDs map[string]bool // key: full JID string (e.g. "447743009263@s.whatsapp.net")
	cli            *whatsmeow.Client
}

func NewPresenceManager(cli *whatsmeow.Client) *PresenceManager {
	return &PresenceManager{
		subscribedJIDs: make(map[string]bool),
		cli:            cli,
	}
}

// Subscribe sends a presence subscription request and tracks the JID on success.
func (pm *PresenceManager) Subscribe(jidStr string) error {
	jid, ok := parseJID(jidStr)
	if !ok {
		return fmt.Errorf("invalid JID: %s", jidStr)
	}
	err := pm.cli.SubscribePresence(context.Background(), jid)
	if err == nil {
		pm.mu.Lock()
		pm.subscribedJIDs[jidStr] = true
		pm.mu.Unlock()
	}
	return err
}

// Unsubscribe removes a JID from the tracking set.
func (pm *PresenceManager) Unsubscribe(jidStr string) {
	pm.mu.Lock()
	delete(pm.subscribedJIDs, jidStr)
	pm.mu.Unlock()
}

// ResubscribeAll re-subscribes to all tracked JIDs with a 300ms delay between each
// to avoid triggering WhatsApp rate limits. Called after automatic reconnect.
func (pm *PresenceManager) ResubscribeAll() {
	pm.mu.RLock()
	jids := make([]string, 0, len(pm.subscribedJIDs))
	for jid := range pm.subscribedJIDs {
		jids = append(jids, jid)
	}
	pm.mu.RUnlock()

	if len(jids) == 0 {
		return
	}

	successCount := 0
	for _, jidStr := range jids {
		time.Sleep(300 * time.Millisecond)
		jid, ok := parseJID(jidStr)
		if !ok {
			continue
		}
		if err := pm.cli.SubscribePresence(context.Background(), jid); err == nil {
			successCount++
		} else {
			log.Warnf("Resubscribe failed for %s: %v", jidStr, err)
		}
	}
	log.Infof("Re-subscribed %d/%d contacts after reconnect", successCount, len(jids))
	ProtoOutput(MsgResubscribe, map[string]any{
		"total":   len(jids),
		"success": successCount,
	})
}

// Count returns the number of currently tracked subscriptions.
func (pm *PresenceManager) Count() int {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return len(pm.subscribedJIDs)
}

// JIDs returns a copy of all tracked JID strings.
func (pm *PresenceManager) JIDs() []string {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	result := make([]string, 0, len(pm.subscribedJIDs))
	for jid := range pm.subscribedJIDs {
		result = append(result, jid)
	}
	return result
}

