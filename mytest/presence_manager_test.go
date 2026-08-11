package main

import "testing"

// 退订标记集：MarkUnsubscribed / ClearUnsubscribed / IsUnsubscribed。
// cli 传 nil 即可（这些方法不触网；Subscribe 才会调 cli，测试不走到）。
func TestPresenceManagerUnsubscribedSet(t *testing.T) {
	pm := NewPresenceManager(nil)

	pm.MarkUnsubscribed("4477@s.whatsapp.net")
	if !pm.IsUnsubscribed("4477@s.whatsapp.net") {
		t.Fatalf("expected marked as unsubscribed")
	}
	// 其他 JID 不受影响
	if pm.IsUnsubscribed("4478@s.whatsapp.net") {
		t.Fatalf("other jid should not be unsubscribed")
	}

	pm.ClearUnsubscribed("4477@s.whatsapp.net")
	if pm.IsUnsubscribed("4477@s.whatsapp.net") {
		t.Fatalf("expected unsubscribed mark cleared")
	}
}

// 软退订语义：Unsubscribe 移出 subscribedJIDs（重连后不再重订），
// 配合 MarkUnsubscribed 实现过滤。这里直接验证集合操作。
func TestPresenceManagerUnsubscribeRemovesFromTracking(t *testing.T) {
	pm := NewPresenceManager(nil)
	pm.subscribedJIDs["4477@s.whatsapp.net"] = true

	pm.Unsubscribe("4477@s.whatsapp.net")
	if pm.subscribedJIDs["4477@s.whatsapp.net"] {
		t.Fatalf("expected removed from subscribedJIDs")
	}
	// Count 反映订阅集合收缩
	if pm.Count() != 0 {
		t.Fatalf("expected 0 subscribed after unsubscribe, got %d", pm.Count())
	}
}
