package main

import "sync"

// presenceTimeLayout 与 Java 端 Constants.LAST_SEEN_TIME_FORMAT（"yyyy/MM/dd HH:mm:ss"）一致，
// 使 ts / lastSeen 可直接被 Java 的 TimeUtils.lastSeenTimeToMillis 解析。
const presenceTimeLayout = "2006/01/02 15:04:05"

// presenceEvent 是一条 online/offline 通知的缓存条目。
// ts 记录事件发生时刻（与 lastSeen 同格式）；重放时 online 事件用 ts 精确还原历史时间。
type presenceEvent struct {
	state    string // "online" | "offline"
	jid      string // 已解析为手机号的 JID（searchPhoneNum 输出）
	lastSeen string // "2006/01/02 15:04:05" 或 ""（联系人隐藏 Last Seen）
	ts       string // 事件发生时刻，同上格式
}

// notifyEvent 是一条 Java 掉线期间需要重放的运行时通知（如 viewOnceFile），携带完整 proto 载荷。
// 通知丢失意味着 view-once 文件成孤儿（已传 S3 但 Server 无记录），故溢出时会记 ERROR。
type notifyEvent struct {
	msgType string
	data    map[string]any
}

// PresenceCache 在 Java 客户端断开期间缓存事件，并在 Java 重连后按序重放。
// 覆盖两类事件：
//   - presence（Handle）：上下线通知，含 latest 状态与溢出 JID 兜底；
//   - 运行时通知（Notify，view-once 等）：纯 FIFO，无状态折叠，每条都必须送达。
//
// 设计要点：所有事件统一先入 buffer，能送达则立即排空发送，不能送达则留存，
// Java attach（setProtoConn）后由 Replay 整体排空 —— buffer 是唯一真相源，避免
// 「事件既直发又滞留」的竞态。
//
// 线程安全：Handle/Notify 来自 whatsmeow 事件 handler goroutine，Replay 来自 socket accept
// goroutine，全部状态在锁内访问，发送在锁外进行。
type PresenceCache struct {
	mu     sync.RWMutex
	latest map[string]*presenceEvent // 每 JID 最新状态（key: jid）
	buffer []*presenceEvent          // FIFO 未送达 presence 事件
	maxBuf int
	// droppedJIDs: 因 buffer 溢出被挤出、且未被后续事件覆盖的 JID。
	// 用于 Replay 时对这些 JID 补发当前状态，避免「已送达的 JID」被 latest 误补发造成重复。
	droppedJIDs map[string]bool
	// notifyEvents: FIFO 未送达的运行时通知（view-once 等），重连时按序重放。
	notifyEvents []*notifyEvent
	maxNotify    int
}

func NewPresenceCache(maxBuf int) *PresenceCache {
	return &PresenceCache{
		latest:       make(map[string]*presenceEvent),
		buffer:       make([]*presenceEvent, 0, 16),
		maxBuf:       maxBuf,
		droppedJIDs:  make(map[string]bool),
		notifyEvents: make([]*notifyEvent, 0, 8),
		maxNotify:    1000,
	}
}

// Handle 记录一条 presence 事件并尝试送达：
//   - 总是更新 latest 并追加 buffer（同一事件先入 buffer，保证顺序与唯一真相源）
//   - 超限丢弃最旧事件（溢出兜底见 Replay）
//   - 若当前有 Java 客户端连接，立即排空 buffer 发送；否则事件留存待重放
func (pc *PresenceCache) Handle(evt *presenceEvent) {
	pc.mu.Lock()
	pc.latest[evt.jid] = evt
	pc.buffer = append(pc.buffer, evt)
	if pc.maxBuf > 0 && len(pc.buffer) > pc.maxBuf {
		// 丢弃最旧事件，并记录该 JID 供 Replay 的 latest 兜底（防当前状态缺失）
		dropped := pc.buffer[0]
		pc.buffer = pc.buffer[1:]
		pc.droppedJIDs[dropped.jid] = true
	}
	if HasJavaClient() {
		pending := pc.buffer
		pc.buffer = nil
		pc.mu.Unlock()
		for _, pe := range pending {
			emitPresence(pe)
		}
		return
	}
	pc.mu.Unlock()
}

// Notify 记录一条运行时通知（view-once 等），与 Handle 相同的「先入缓冲，在线即排空」策略。
// 通知丢失意味着 view-once 文件成孤儿，故溢出丢弃最旧时记 ERROR（便于运维介入）。
func (pc *PresenceCache) Notify(msgType string, data map[string]any) {
	pc.mu.Lock()
	pc.notifyEvents = append(pc.notifyEvents, &notifyEvent{msgType: msgType, data: data})
	if pc.maxNotify > 0 && len(pc.notifyEvents) > pc.maxNotify {
		dropped := pc.notifyEvents[0]
		pc.notifyEvents = pc.notifyEvents[1:]
		log.Errorf("presenceCache notify buffer overflow, dropped %s: %v", dropped.msgType, dropped.data)
	}
	if HasJavaClient() {
		pending := pc.notifyEvents
		pc.notifyEvents = nil
		pc.mu.Unlock()
		for _, ev := range pending {
			ProtoOutput(ev.msgType, ev.data)
		}
		return
	}
	pc.mu.Unlock()
}

// Remove 删除某 JID 的全部缓存条目（latest / buffer / droppedJIDs）。
// 用于删除/改号 observer 后的退订：清掉该 JID 的滞留事件，避免后续重放
// 被 Java 端 handlePresence 以 observer==null 过滤（纯浪费），也避免 latest
// 兜底补发已删除联系人。入参 jid 为已解析的完整 JID 字符串（同 presenceEvent.jid）。
func (pc *PresenceCache) Remove(jid string) {
	pc.mu.Lock()
	delete(pc.latest, jid)
	delete(pc.droppedJIDs, jid)
	kept := pc.buffer[:0]
	for _, pe := range pc.buffer {
		if pe.jid != jid {
			kept = append(kept, pe)
		}
	}
	pc.buffer = kept
	pc.mu.Unlock()
}

// Replay 在 Java 重连（setProtoConn 之后）调用：按 FIFO 序排空重放缓冲事件；
// 对 presence 溢出被挤出的 JID 补发最新状态，随后按序重放未送达的运行时通知。
func (pc *PresenceCache) Replay() {
	pc.mu.Lock()
	pending := pc.buffer
	pc.buffer = nil
	covered := make(map[string]bool, len(pending))
	for _, pe := range pending {
		covered[pe.jid] = true
	}
	var final []*presenceEvent
	for jid := range pc.droppedJIDs {
		if !covered[jid] {
			if ev, ok := pc.latest[jid]; ok {
				final = append(final, ev)
			}
		}
	}
	pc.droppedJIDs = make(map[string]bool)
	pendingNotify := pc.notifyEvents
	pc.notifyEvents = nil
	pc.mu.Unlock()

	for _, pe := range pending {
		emitPresence(pe)
	}
	for _, ev := range final {
		emitPresence(ev)
	}
	for _, ne := range pendingNotify {
		ProtoOutput(ne.msgType, ne.data)
	}
}

// emitPresence 将缓存事件以标准 presence proto 消息输出。
// 所有输出都带 ts（实时事件 ts≈now，重放事件 ts=真实时刻），旧 Java 忽略未知字段，向后兼容。
func emitPresence(pe *presenceEvent) {
	data := map[string]any{
		"state": pe.state,
		"jid":   pe.jid,
		"ts":    pe.ts,
	}
	if pe.lastSeen != "" {
		data["lastSeen"] = pe.lastSeen
	}
	ProtoOutput(MsgPresence, data)
}
