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
	// priorityJIDs: 下次 ResubscribeAll 优先重订阅的 JID（重加 observer 场景）。重连后
	// subscribedJIDs 的遍历顺序来自 Go map（随机），若重加的 JID 被排到最后，其 fresh
	// presence 要等 N×300ms 才到，onlineState 恢复被拖慢。标记后 ResubscribeAll 先发它。
	// 一次性消费：ResubscribeAll 发送后清空。
	priorityJIDs []string
	// unsubscribedJIDs: 显式退订的 JID（删除/改号 observer）。whatsmeow 无协议级
	// UnsubscribePresence，采用软退订：移出 subscribedJIDs（下个连接周期 ResubscribeAll
	// 不再重订，服务器自动停推）+ 标记过滤（main.go presence handler 跳过该 JID，不再
	// 缓存/重放）。重新 subscribepresence 时 ClearUnsubscribed 恢复。
	unsubscribedJIDs map[string]bool
	cli              *whatsmeow.Client
}

func NewPresenceManager(cli *whatsmeow.Client) *PresenceManager {
	return &PresenceManager{
		subscribedJIDs:   make(map[string]bool),
		unsubscribedJIDs: make(map[string]bool),
		cli:              cli,
	}
}

// Subscribe sends a presence subscription request and tracks the JID.
//
// 粘性订阅：无论本次 send 是否成功（WebSocket 可能暂时断开），都先把 JID 记录到
// subscribedJIDs。发送失败时下次重连 ResubscribeAll 会补发；否则若此处不记录，
// 断线窗口内新增/重加的 observer 会永久丢失订阅——Java 端已不再依赖重连后的
// loginSuccess→checkObservers 补订（登录阶段 proto 只在首次连接上报），因此这里
// 必须粘性兜底，保证「订阅意图」在重连后仍被兑现。
func (pm *PresenceManager) Subscribe(jidStr string) error {
	jid, ok := parseJID(jidStr)
	if !ok {
		return fmt.Errorf("invalid JID: %s", jidStr)
	}
	pm.mu.Lock()
	pm.subscribedJIDs[jidStr] = true
	pm.mu.Unlock()
	return pm.cli.SubscribePresence(context.Background(), jid)
}

// Unsubscribe removes a JID from the tracking set.
func (pm *PresenceManager) Unsubscribe(jidStr string) {
	pm.mu.Lock()
	delete(pm.subscribedJIDs, jidStr)
	pm.mu.Unlock()
}

// MarkUnsubscribed 标记某 JID 已退订：main.go 的 presence handler 将跳过该 JID 的事件
// （不再缓存/重放）。配合 Unsubscribe 使用（退订 = 移出重订阅集合 + 标记过滤）。
func (pm *PresenceManager) MarkUnsubscribed(jidStr string) {
	pm.mu.Lock()
	pm.unsubscribedJIDs[jidStr] = true
	pm.mu.Unlock()
}

// ClearUnsubscribed 取消退订标记（重新 subscribepresence 时调用，恢复事件处理）。
func (pm *PresenceManager) ClearUnsubscribed(jidStr string) {
	pm.mu.Lock()
	delete(pm.unsubscribedJIDs, jidStr)
	pm.mu.Unlock()
}

// MarkPriority 标记某 JID 为下次 ResubscribeAll 的优先重订阅对象（重加 observer 用）。
// 重连后 ResubscribeAll 先发它，使 fresh presence 尽快到达、onlineState 尽快恢复。
func (pm *PresenceManager) MarkPriority(jidStr string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for _, j := range pm.priorityJIDs {
		if j == jidStr {
			return
		}
	}
	pm.priorityJIDs = append(pm.priorityJIDs, jidStr)
}

// IsUnsubscribed 返回该 JID 是否已显式退订。
func (pm *PresenceManager) IsUnsubscribed(jidStr string) bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.unsubscribedJIDs[jidStr]
}

// ResubscribeAll re-subscribes to all tracked JIDs with a 300ms delay between each
// to avoid triggering WhatsApp rate limits. Called after automatic reconnect.
//
// 优先重订阅 priorityJIDs（重加 observer，见 MarkPriority）：先发重加的 JID，其 fresh
// presence 尽快到达；其余 JID 按 map 遍历顺序补订。priority 一次性消费（发送后清空），
// 且只在仍属于 subscribedJIDs 时生效（已被删除的 observer 不因残留 priority 被重订）。
func (pm *PresenceManager) ResubscribeAll() {
	pm.mu.Lock()
	priority := pm.priorityJIDs
	pm.priorityJIDs = nil
	jids := make([]string, 0, len(pm.subscribedJIDs)+len(priority))
	seen := make(map[string]bool, len(priority))
	for _, j := range priority {
		if pm.subscribedJIDs[j] && !seen[j] {
			seen[j] = true
			jids = append(jids, j)
		}
	}
	for jid := range pm.subscribedJIDs {
		if !seen[jid] {
			jids = append(jids, jid)
		}
	}
	pm.mu.Unlock()

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

