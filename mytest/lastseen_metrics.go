package main

import "sync/atomic"

// LastSeen 健康指标（2026-11-23），随 60s 心跳上报 Java（/inner/lastseenHealth 汇总）。
//
// 🔴 只统计「实时到达」的 presence 事件：Go 掉线期间 PresenceCache 的缓存重放不经过
// events.Presence 分支，因此不会刷新 lastPresenceMs —— 这正好让 Java/Proxy 能区分
// 「真的有新事件」与「只是把旧事件重放了一遍」。
var (
	presenceEvents atomic.Int64 // 进程内累计收到的实时 presence 事件数（Go 重启归零）
	lastPresenceMs atomic.Int64 // 最近一次实时 presence 事件时刻（epoch ms，0 = 尚未收到）
)
