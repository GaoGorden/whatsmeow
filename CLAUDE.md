# whatsmeow — WhatsApp 协议通信（Fork）

> 本项目是 [tulir/whatsmeow](https://github.com/tulir/whatsmeow) 开源库的二次开发 Fork，是 WaTracker/WeSeen 系统的 **WhatsApp 协议通信核心**。
> 请先阅读根目录 `CLAUDE.md` 了解整体架构。

## 基本信息

- **语言**：Go 1.25+
- **上游**：`go.mau.fi/whatsmeow`（fork 自 tulir/whatsmeow）
- **数据库**：SQLite（每个用户一个 `mdtest.db`，通过 sqlstore 管理）
- **构建**：`cd mytest && go build`（单客户端）或 `cd mytest2/main && go build`（多客户端 HTTP 版）
- **部署**：`scp-linux-go-file.ps1` 上传编译好的 Linux 可执行文件到 Server
- **CGO**：需要（`import "C"`，编译时依赖 C 库）

## 两套入口程序

### mytest/ — 单客户端版（当前生产使用）

由 Java Server 的 `ProcessUtils` 启动和管理，通过 **`##PROTO##` JSON 协议**（stdout）与 Java Server 通信。

| 文件 | 说明 |
|------|------|
| `main.go` | 【核心】生产入口，包含完整的事件处理和命令系统 |
| `proto_output.go` | Proto 协议输出封装（`ProtoOutput()` 函数 + `Msg*` 消息类型常量） |
| `presence_manager.go` | 联系人订阅管理器（跟踪已订阅 JID，重连后自动重订阅） |
| `main_new.go` | 原始上游示例代码（函数名改为 `maingg()`，不执行） |
| `go.mod` | 独立 Go module，通过 `replace` 指向本地 whatsmeow 库 |
| `amazon.yaml` | AWS S3 凭证配置（`//go:embed` 嵌入） |

### mytest2/ — 多客户端 HTTP 版（新版/实验）

使用 **Gin 框架** 暴露 HTTP API，支持在同一进程中管理多个 WhatsApp 客户端实例。

| 文件 | 说明 |
|------|------|
| `main/main.go` | 入口，启动 Gin HTTP Server（端口 9090） |
| `client-manager.go` | `ClientManager`（`sync.RWMutex` + `map[string]*ClientEntry`） |
| `bean/wa-client-info.go` | `WaClientInfo` 配置结构体 |
| `utils/string.go` | 字符串工具 |

#### mytest2 HTTP API

| 路径 | 方法 | 说明 |
|------|------|------|
| `/checkState` | GET | 检查客户端是否就绪 |
| `/login` | POST | 登录（验证码方式） |
| `/logout` | POST | 登出 |
| `/reLoginUsers` | POST | 批量重新登录 |
| `/reconnect` | POST | 重连 |
| `/disconnect` | POST | 断开连接 |
| `/subscribeObservers` | POST | 订阅观察者 Last Seen |

## 核心定制功能

### 1. ViewOnce 阅后即焚拦截

**触发条件**：`enableViewOnce == true` 且消息发送者不是自己

**处理流程**（`handler` 函数中 `events.Message` 分支）：
1. 检测 `evt.Message.GetImageMessage().GetViewOnce()` / `GetVideoMessage()` / `GetAudioMessage()`
2. 通过 `searchPhoneNum()` 将 LID 解析为真实电话号码
3. 通过 `getNickName()` 获取联系人昵称（优先 FullName → PushName）
4. 异步调用 `cli.Download()` 下载媒体
5. `uploadAndNotify()` 上传到 S3 并输出 JSON 通知（Java Server 通过 stdout 解析）

**S3 上传路径**：`whatsapp/view-once/{userJID}/{messageID}{extension}`

### 2. 在线状态追踪（Presence）

**事件结构体**（`types/events/events.go`）：
```go
type Presence struct {
    From        types.JID   // 联系人 JID
    Unavailable bool        // true = 离线
    LastSeen    time.Time   // 最后在线时间，可能为零值（用户隐藏了 Last Seen）
}
```

**协议解析**（`presence.go` → `handlePresence()`）：

WhatsApp 服务器推送的 presence XML 节点包含 `type` 和 `last` 两个关键属性：
```go
presenceType := ag.OptionalString("type")       // "unavailable" = 离线
lastSeen := ag.OptionalString("last")           // Unix 时间戳 / "deny" / 空
if lastSeen != "" && lastSeen != "deny" {
    evt.LastSeen = ag.UnixTime("last")
}
```

**`last` 属性与用户隐私设置的关系**（`types/user.go` → `PrivacySettingTypeLastSeen`）：

被监控联系人在 WhatsApp 中设置的 **"Last Seen & Online" 隐私选项** 决定了 `last` 属性的值：

| 用户隐私设置 | 监控者收到 | `evt.LastSeen` | stdout 输出 |
|---|---|---|---|
| Everyone（`all`） | `last="时间戳"` | 有值 | `offline: <jid>%!(EXTRA time.Time=...)` |
| My Contacts（`contacts`），监控者在联系人列表 | `last="时间戳"` | 有值 | 同上 |
| My Contacts，监控者不在联系人列表 | `last="deny"` | 零值 | `offline: <jid>` |
| Nobody（`none`） | `last="deny"` | 零值 | `offline: <jid>` |

**两种离线输出格式的原因**：

Go 端代码（`main.go`）中有一个格式串 bug：
```go
if evt.LastSeen.IsZero() {
    log.Infof("offline: %s", result)                    // 1个 %s，1个参数 → 正常
} else {
    log.Infof("offline: %s", result, evt.LastSeen)       // 1个 %s，2个参数 → bug
}
```
第二行格式串只有 1 个 `%s` 但传了 2 个参数，Go 的 `fmt.Sprintf` 会将多余的 `time.Time` 参数以 `%!(EXTRA time.Time=...)` 格式追加输出。Java 端 `ProcessLineUtils` 已"将错就错"地按此格式解析（匹配 `EXTRA time.Time=` 字符串）。

**stdout 输出格式**（经 `searchPhoneNum()` 解析 LID → 电话号码后）：
- 上线：`online: {phoneNumber}`
- 离线（无 LastSeen）：`offline: {phoneNumber}`
- 离线（有 LastSeen）：`offline: {phoneNumber}%!(EXTRA time.Time={lastSeenTime})`

### 3. LID 解析

WhatsApp 使用 LID（Linked ID）代替真实电话号码，需要反查：
- `searchPhoneNum(ctx, lid)` → 调用 `cli.Store.LIDs.GetPNForLID()` 获取真实号码
- `searchJid(ctx, lid)` → 返回完整的 JID（含设备号）
- `parseRealLid()` → 解析自己的 LID（去掉设备号 `:N@` → `@`）

### 4. 登录流程

支持两种登录方式：
- **QR 码**：`require-qrcode` 命令 → `cli.GetQRChannel()` → `cli.Connect()` → 输出 QR code
- **验证码**：`pair-phone <number>` → `cli.PairPhone()` → 输出 linking code

登录成功后：
- 发送 `PresenceAvailable`
- 输出 `push name` 和 `phone number`（Java Server 解析为登录成功标记）
- 解析自己的 LID

### 5. 命令系统（stdin 交互）

Java Server 通过 stdin 向 Go 进程发送命令：

| 命令 | 说明 |
|------|------|
| `enable-view-once` | 开启 ViewOnce 拦截 |
| `disable-view-once` | 关闭 ViewOnce 拦截 |
| `pair-phone <number>` | 验证码登录 |
| `require-qrcode` | QR 码登录 |
| `reconnect` | 断开并重连 |
| `logout` | 登出 |
| `subscribepresence <jid>` | 订阅联系人在线状态 |
| `checkuser <phones...>` | 检查号码是否注册 WhatsApp |
| `getavatar <jid>` | 获取头像 URL |
| `getgroup <jid>` | 获取群组信息 |
| `send <jid> <text>` | 发送文本消息 |
| `getuser <jids...>` | 获取用户信息 |
| `presence <available/unavailable>` | 设置自身在线状态 |
| `setpushname <name>` | 设置推送昵称 |

## 库层目录结构（上游 + 定制）

```
whatsmeow/
├── client.go              ← 【核心】Client 结构体（连接管理、事件处理、自动重连）
├── send.go                ← 消息发送（GenerateMessageID、SendMessage、BuildPollCreation 等）
├── download.go            ← 媒体下载（Decrypt + Download）
├── upload.go              ← 媒体上传
├── message.go             ← 消息解密/处理
├── presence.go            ← 在线状态（SubscribePresence、SendPresence）
├── pair.go / pair-code.go ← 配对登录（QR 码、验证码）
├── connectionevents.go    ← 连接事件处理
├── keepalive.go           ← 心跳保活
├── prekeys.go             ← 预密钥管理
├── receipt.go             ← 消息回执（已读、已送达）
├── group.go               ← 群组操作
├── user.go                ← 用户信息查询
├── newsletter.go          ← Newsletter 功能
├── privacysettings.go     ← 隐私设置
├── call.go                ← 通话事件
├── notification.go        ← 通知处理
├── retry.go / mediaretry.go ← 重试机制
├── errors.go              ← 错误定义
├── internals.go           ← DangerousInternals（高级 API）
├── request.go             ← 底层请求发送
│
├── argo/                  ← Argo 查询协议（自定义）
│   ├── argo.go            ← Wire Type Store 加载、QueryID 映射
│   ├── argo-wire-type-store.argo  ← 嵌入的协议定义文件
│   └── name-to-queryids.json      ← 嵌入的查询名称映射
│
├── binary/                ← WhatsApp 二进制协议编解码
│   ├── decoder.go         ← XML-like 节点解码器
│   ├── node.go            ← Node 结构体
│   ├── attrs.go           ← 属性处理
│   ├── token/token.go     ← 协议 token 表
│   └── proto/             ← 旧版 protobuf 定义
│
├── proto/                 ← Protobuf 协议定义（自动生成）
│   ├── waE2E/             ← 端到端加密消息
│   ├── waWeb/             ← Web 协议
│   ├── waWa6/             ← WA6 协议
│   ├── waCommon/          ← 公共定义
│   └── ...（20+ 子目录）
│
├── socket/                ← WebSocket + Noise Protocol 通信
│   └── noisehandshake.go  ← Noise 协议握手
│
├── store/                 ← 客户端状态存储
│   ├── store.go           ← Device 结构体（JID、密钥、联系人等）
│   ├── sqlstore/          ← SQLite 持久化（容器、升级）
│   ├── signal.go          ← Signal 协议密钥存储
│   └── sessioncache.go    ← 会话缓存
│
├── types/                 ← 类型定义
│   ├── jid.go             ← JID（WhatsApp 标识符）
│   ├── events/            ← 所有事件类型定义
│   ├── presence.go        ← Presence 枚举
│   ├── message.go         ← 消息类型
│   ├── group.go           ← 群组类型
│   └── ...
│
├── util/                  ← 工具库
│   ├── log/               ← 日志（zerolog 封装）
│   ├── keys/              ← 密钥对
│   ├── gcmutil/           ← GCM 加密
│   └── hkdfutil/          ← HKDF 密钥派生
│
├── appstate/              ← App State 同步（收藏、静音、标签等）
└── mytest/ mytest2/       ← 项目定制入口（见上文）
```

## 与 Java Server 的通信协议

### Java → Go（stdin 命令）

Java Server 通过 `Process.getOutputStream()` 向 Go 进程写入命令字符串（每行一条命令 + `\n`）：
- 登录命令、订阅命令、ViewOnce 开关等
- 每个用户的命令通过 Java 端 per-user 锁保证不交叉

### Go → Java（stdout 输出 — Proto 协议）

Go 进程通过 `ProtoOutput()` 函数输出 **`##PROTO##` 前缀的 JSON 消息**，Java Server 的 `ProcessUtils` 解析 `ProtoMessage`。

**输出格式**：`##PROTO##{"type":"<消息类型>","field1":"value1",...}`

**消息类型常量**（定义在 `proto_output.go`）：

| 常量 | type 值 | 说明 |
|------|--------|------|
| `MsgPresence` | `presence` | 联系人在线/离线状态 |
| `MsgReadReceipt` | `readReceipt` | 消息已读回执 |
| `MsgReceivedMessage` | `receivedMessage` | 收到消息通知 |
| `MsgCheckUser` | `checkUser` | 号码检查结果 |
| `MsgGetAvatar` | `getAvatar` | 头像获取成功 |
| `MsgGetAvatarFail` | `getAvatarFail` | 头像获取失败 |
| `MsgLoginSuccess` | `loginSuccess` | 登录成功 |
| `MsgPushName` | `pushName` | 用户昵称 |
| `MsgPhoneNumber` | `phoneNumber` | 用户手机号 |
| `MsgQrCode` | `qrCode` | QR 码数据 |
| `MsgLinkingCode` | `linkingCode` | 配对码 |
| `MsgQrTimeout` | `qrTimeout` | QR 码超时 |
| `MsgLogoutSuccess` | `logoutSuccess` | 登出确认 |
| `MsgViewOnceFile` | `viewOnceFile` | 阅后即焚文件通知 |
| `MsgViewOnceEnabled` | `viewOnceEnabled` | ViewOnce 开关确认 |
| `MsgPairError` | `pairError` | 配对错误 |
| `MsgHeartbeat` | `heartbeat` | 进程心跳（60s 间隔） |
| `MsgResubscribe` | `resubscribe` | 重连后重订阅结果 |
| `MsgStreamReplaced` | `streamReplaced` | 被顶号（其他设备登录） |
| `MsgLoggedOut` | `loggedOut` | 服务端登出 |

**稳定性机制**：
- **PresenceManager**（`presence_manager.go`）：跟踪所有已订阅的联系人 JID，`events.Connected` 时自动调用 `ResubscribeAll()` 重订阅
- **心跳**：每 60 秒输出 `heartbeat`，上报 goroutine 数、内存使用、订阅数、运行时间
- **StreamReplaced 处理**：发送 Proto 通知 → `cli.Disconnect()` → `time.Sleep(3s)` → `os.Exit(42)`
- **LoggedOut 处理**：发送 Proto 通知 → `cli.Disconnect()` → `time.Sleep(3s)` → `os.Exit(43)`
- **ViewOnce 超时保护**：下载 60 秒超时，上传 30 秒超时（`context.WithTimeout`）

### Go → Java（库级别日志 — 辅助）

whatsmeow 库自身的 `log.Errorf` / `log.Warnf` 输出仍为纯文本行，Java 通过 `line.contains()` / 正则匹配作为回退：
```
[Client/Socket ERROR] reconnecting in background    ← 库自动重连
[Main ERROR] Failed to get avatar for ...           ← handleCmd 命令失败
```

**注意**：`[Main ERROR]` 来自 `handleCmd()` 的命令执行失败（如 getavatar 目标没有头像），Java 端将其视为命令级错误（不触发重启），仅 `[Client*/Database*]` 错误进入重启频率窗口。

## 配置与依赖

### 运行时配置

- **`amazon.yaml`**（嵌入）：AWS S3 凭证（key、secret、region）
- **`WaClientInfo.json`**（mytest2 用）：用户文件目录路径
- **SQLite DB**：每个用户独立 `mdtest.db`，存储密钥、联系人、会话等

### 关键依赖

| 库 | 用途 |
|----|------|
| `go.mau.fi/libsignal` | Signal 协议（端到端加密） |
| `github.com/coder/websocket` | WebSocket 通信 |
| `github.com/rs/zerolog` | 结构化日志 |
| `google.golang.org/protobuf` | Protobuf 序列化 |
| `github.com/mattn/go-sqlite3` | SQLite 驱动（CGO） |
| `github.com/aws/aws-sdk-go-v2` | S3 上传（ViewOnce 媒体） |
| `github.com/gabriel-vasile/mimetype` | MIME 类型检测 |
| `github.com/gin-gonic/gin` | HTTP API（mytest2） |
| `github.com/beeper/argo-go` | Argo 查询协议 |
| `golang.org/x/crypto` | 加密工具 |

## 注意事项

1. **CGO 依赖**：编译时必须启用 CGO（`CGO_ENABLED=1`），需要对应平台的 C 编译器
2. **SQLite 锁定**：每个 Go 进程使用独立的 `mdtest.db`，不存在并发锁问题
3. **LID 解析**：WhatsApp 正在从电话号码迁移到 LID，所有事件中的 sender 可能返回 LID 而非真实号码，必须通过 `searchPhoneNum()` 转换
4. **ViewOnce 下载**：异步进行（`go func()`），不阻塞主事件循环；上传到 S3 后才输出通知
5. **Proto 协议同步**：修改 `##PROTO##` 消息格式或新增消息类型时，需同步修改 Go 的 `proto_output.go`（常量定义）和 Java 的 `TerminalConstants.java`（常量）+ `ProcessUtils.java`（处理逻辑）
6. **上游同步**：Fork 版本需定期与 `tulir/whatsmeow` 同步协议更新，注意解决合并冲突
7. **`mytest2` 是实验性**：多客户端 HTTP 版本尚在开发中，当前生产环境使用 `mytest`（单客户端 + Java Server 管理）
8. **argo 目录**：包含自定义的 Argo 协议查询定义，嵌入了 `.argo` 和 `.json` 文件
9. **history JSON 文件**：根目录有大量 `history-*.json` 文件，是 HistorySync 事件的调试输出，生产环境可忽略或清理
10. **`amazon.yaml` 包含 S3 密钥**：注意不要泄露
