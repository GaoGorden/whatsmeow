# whatsmeow — Agent Entry

> Claude Code 以 `./CLAUDE.md` 为准。本文件供其它工具使用。

## What this is

whatsmeow 的二次开发 fork（Go 1.25+）。`mytest/` 是生产入口，`mytest2/` 是多客户端实验版。
负责 WhatsApp 协议、presence、ViewOnce、Status、Recover 采集。

## Read first

1. `./CLAUDE.md` — 命令表、Proto 表、presence/status/recover 实现、上游同步
2. 完整 workspace 下：`../.claude/rules/go-protocol.md`、`../CLAUDE.md`、
   `../doc/ai/INDEX.md`

## Build

```bash
cd mytest && go build
```

- 需要 CGO 与对应 C 库
- 部署顺序：**Java 先于 Go**

## Constraints

- 连接首行 `attach` 才设后端并重放 presence 缓存
- `statusFile` / `recoverMessage` / ViewOnce 必须走 `PresenceCache.Notify`
- `ackId = messageId|eventType|eventAt`，不能按 messageId ack
- 纯文本 Status 直接丢弃
- 不要覆盖上游同步脚本的冲突处理约定

## Task context

跨仓任务：`../tasks/active/<ID>/`。
