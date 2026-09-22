# whatsmeow — WhatsApp 协议通信（Fork）

> 本项目是 [tulir/whatsmeow](https://github.com/tulir/whatsmeow) 开源库的二次开发 Fork，是 WaTracker/WeSeen 系统的 **WhatsApp 协议通信核心**。
> 请先阅读根目录 `CLAUDE.md` 了解整体架构。

## 基本信息

- **语言**：Go 1.25+
- **上游**：`go.mau.fi/whatsmeow`（fork 自 tulir/whatsmeow）
- **数据库**：SQLite（每个用户一个 `mdtest.db`，通过 sqlstore 管理）
- **构建**：`cd mytest && go build`（单客户端）或 `cd mytest2/main && go build`（多客户端 HTTP 版）
- **Go SDK 路径**：`D:\developmentKit\gosdk`
- **部署**：`scp-linux-go-file.ps1` 上传编译好的 Linux 可执行文件到 Server
- **CGO**：需要（`import "C"`，编译时依赖 C 库）

## 两套入口程序

### mytest/ — 单客户端版（当前生产使用）

由 Java Server 的 `ProcessUtils` 启动和管理，与 Java Server 通信统一走 **Unix domain socket**（Windows 测试与 Linux 生产一致）：
- **daemon 模式（Linux 生产）**：传 `--socket={path}` 参数进入。Go 进程 `syscall.Setsid()` 脱离 JVM 进程组成为 daemon，命令从 Unix domain socket 接收、`ProtoOutput` 事件写回 socket。配合 `wa-server.service` 的 `KillMode=process`，Java 重启时 Go 不被杀、可被重新 attach，WhatsApp 连接与 presence 订阅不中断。
- **daemon 模式（Windows 本地联调）**：同样传 `--socket={path}`（`run_windows.bat` 用 `%*` 透传）。`daemonize()` 在非 Linux 为 no-op（无 Setsid），但 Windows 父进程退出后子进程存活，Java 重启时 Go daemon 同样存活、可被重新 attach，验证链路与生产一致。

| 文件 | 说明 |
|------|------|
| `main.go` | 【核心】生产入口，包含完整的事件处理、命令系统、daemon 化（`--socket` flag + `daemonize()` + `startCommandSocket`）。⚠️ `OnLoginSuccess` 回调由 `loginSuccessNotified` atomic.Bool 限定为进程**首次连接**上报（whatsmeow 每次连接成功都触发该回调，重连时重发 pushName/phoneNumber/loginSuccess 会造成 Java 端「假重登」日志；重连后的订阅恢复由 `ResubscribeAll` + 粘性订阅负责） |
| `proto_output.go` | Proto 协议输出封装（`ProtoOutput()` + `Msg*` 常量 + `protoBackend` 抽象：stdout/socket 双 backend，socket 断开静默忽略。⚠️ 连接字段用普通 `net.Conn` + mutex，**不能用 `atomic.Value`**——Go 禁止 Store/Swap nil 进 Value，`clearProtoConn()` 的 `Swap(nil)` 会导致 Java 断开连接时 panic、进程以退出码 2 崩溃，详见 `doc/Go进程daemon模式panic崩溃修复.md`） |
| `daemon_linux.go` | Linux daemon 化（`syscall.Setsid()` + `signal.Ignore(SIGPIPE)`） |
| `daemon_other.go` | 非 Linux daemon 化 no-op（Windows 联调用） |
| `presence_manager.go` | 联系人订阅管理器（跟踪已订阅 JID，重连后自动重订阅；含退订标记集 `unsubscribedJIDs`，软退订 + 协议退订（`UnsubscribePresence`，**实测无效**，best-effort）双层；`Subscribe` 为**粘性订阅**——断线发送失败也记录 JID，重连后 `ResubscribeAll` 补发；`MarkPriority` **优先重订阅**重加 JID（ResubscribeAll 先发它，避免 map 随机顺序把重加 JID 排到最后拖慢恢复）；重加 observer 靠 2s 无推送强制重连兜底） |
| `presence_cache.go` | **Presence 事件缓存与重放**：Java 掉线期间缓存上下线通知，重连后按序重放（含 `ts` 时间戳精确还原）；`Notify` 同时缓冲 view-once 等运行时通知；`Remove(jid)` 清空单 JID 缓存（软退订用）；`lastArrival`/`HasFreshEvent` 供重加订阅后 2s 无推送判定（触发强制重连兜底）。详见 `doc/Go端presence事件缓存与重放方案.md`、`doc/Server重启窗口Observer操作与presence重放风险修复.md` |
| `status.go` | **Status（动态）采集主体**（2026-09-22 新增）：`handleStatusMessage` = 过滤链（总开关 / `IsFromMe` / 纯 SKDM / `ProtocolMessage`(REVOKE) / 号码过滤集 / LRU 去重）→ 类型分派（image/video/audio 走下载上传，text 直发）→ worker 池（并发 2）→ outbox。含 `statusObserverSet`（全量覆盖 `replace`）、`statusSeenSet`（5000 条环形去重）、`normalizeStatusJid`、`statusObjectKey`、磁盘水位暂停。详见 `doc/Status动态追踪_Go端实施记录.md`、方案 `doc/Status动态追踪功能实现方案.md` |
| `diskfree_linux.go` / `diskfree_other.go` | `statusFreeDiskBytes(path)` 平台分叉（`syscall.Statfs`）。**非 Linux 返回 `ok=false`** → 磁盘水位保护只在生产生效，本地 Windows 联调不会被暂停逻辑卡住（对照 `proc_mem_linux.go` / `proc_mem_other.go`） |
| `status_test.go` | Status 单测 9 个（过滤链早退、去重集淘汰、outbox 生命周期/裁剪、payload 字段表）。`statusMaxFileBytes` / `statusOutboxMax` 写成 `var` 就是为了单测能调小 |
| `pending_upload.go` | **View-once 完整恢复**：上传失败（如 Java 掉线致预签名失败）时媒体暂存 `pending-upload/`，attach/周期触发重传 + 补通知，上限 **100 条 + 2GiB**（双上限，任一超标按 mtime 丢最旧）。**2026-09-22 流式改造**：下载走 `downloadToTemp`（`cli.DownloadToFile` 落临时文件）、上传走 `uploadFileToS3`、暂存走 `moveFile`（rename 优先），全链路内存 O(32KB) 与文件大小无关。详见 `doc/pending-upload流式改造实施记录.md`、`doc/Go进程内存优化与预签名上传方案.md`「完整恢复」。**同批新增 `Source` 判别字段**：`flushPendingUploads` 按 `meta.Source == "status"` 分流到 `flushPendingStatus`——Status 复用这套暂存目录，就同时复用了条数/字节双上限、rename 搬移与 attach/周期两条重传触发路径；`Source` 为空（含历史遗留记录）走原 view-once 路径。`flushPendingStatus` 用记录里**存下的 `ObjectKey`**（不重新推导，否则重传会落到与 DB 不同的键上） |
| `main_new.go` | 原始上游示例代码（函数名改为 `maingg()`，不执行） |
| `go.mod` | 独立 Go module，通过 `replace` 指向本地 whatsmeow 库 |
| `amazon.yaml` | ⚠️ **已废弃**：原 S3 凭证嵌入文件，方案B 后 Go 不再嵌入/引用（可删除，密钥改由 Server 侧 S3Config 统一持有） |

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

### 0. iOS 设备指纹配置（运行时）

**位置**：`store/deviceconfig.go`

**功能**：通过 `SetDeviceFingerprintIOS()` 函数在运行时将设备指纹从默认的 WEB 切换为 iOS，以支持 ViewOnce 消息拦截。

**调用时机**：`mytest/main.go` 在创建 `whatsmeow.Client` 之前调用：
```go
store.SetDeviceFingerprintIOS()
cli = whatsmeow.NewClient(device, log)
```

**修改内容**：
- `BaseClientPayload.UserAgent`: Platform=IOS, Manufacturer=Apple, Device=iPhone, OsVersion=18.2
- `BaseClientPayload.WebInfo`: 设为 nil（iOS 不需要 WebInfo）
- `DeviceProps`: Os=iOS, PlatformType=IOS_PHONE, Version=18.2.0

**设计优势**：将设备指纹配置从库代码（`clientpayload.go`）提取到运行时配置，使得 `clientpayload.go` 与上游完全一致，大幅减少同步冲突。

### 1. ViewOnce 阅后即焚拦截

**触发条件**：`enableViewOnce == true` 且消息发送者不是自己

**处理流程**（`handler` 函数中 `events.Message` 分支）：
1. 检测 `evt.Message.GetImageMessage().GetViewOnce()` / `GetVideoMessage()` / `GetAudioMessage()`
2. 通过 `searchPhoneNum()` 将 LID 解析为真实电话号码
3. 通过 `getNickName()` 获取联系人昵称（优先 FullName → PushName）
4. 异步调用 `downloadToTemp()` 把媒体**流式下载**到 `media-tmp/{msgID}.bin`（内部用 `cli.DownloadToFile`）
5. `uploadAndNotify()` 上传到 S3 并输出 JSON 通知（`ProtoOutput(MsgViewOnceFile)`，Java Server 通过 socket/Proto 协议解析），返回前删除临时文件
   - **上传方式（方案B）**：Go 不再内嵌 AWS SDK，而是 `GET {server-url}/inner/presignViewOnceUpload?objectKey=...` 向 Java Server 申请 S3 预签名 PUT URL，再直接 HTTP PUT 上传。`--server-url` 由 Java 启动时传入。详见 `doc/Go进程内存优化与预签名上传方案.md`
   - **完整恢复**：上传失败（如 Java 掉线致预签名失败）时媒体暂存 `pending-upload/`，Java 重连后 `flushPendingUploads` 补传 + `presenceCache.Notify` 补通知（`pending_upload.go`）
   - **内存优化（方案A）**：`main()` 开头 `debug.SetMemoryLimit(64<<20)` 设堆软上限，防 view-once 下载/历史同步瞬时大分配打穿节点
   - **内存优化（方案B，2026-08-26）**：`cli.Download()` 双缓冲（`io.ReadAll` 密文 + `cbcutil.Decrypt` 明文）会把 Go heap arena 撑到峰值，但 GC 后 Go **不归还**扩大的 arena 给 OS（GOMEMLIMIT 是软上限、不触发去提交），形成「当前 RSS 35-43MB / live heap 仅 1-11MB」的峰值残留。在 `uploadAndNotify`（直接上传成功）、`savePendingUpload`（暂存落盘后）、`flushPendingUploads`（每个重传成功项）三处各加 `debug.FreeOSMemory()` 强制归还。详见 `doc/linux_go内存优化方案_viewonce流式与FreeOSMemory.md`
   - **流式改造（2026-09-22）**：上一条的**根因已消除**——改用 `cli.DownloadToFile` + 32KB 分块原地解密（`cbcutil.DecryptFile`）+ `io.Reader` 上传 + `rename` 搬移暂存后，**没有任何路径按文件大小分配内存**，全链路 O(32KB)。三处 `FreeOSMemory()` 随之移除（`uploadAndNotify` 保留一处作廉价兜底）。**单条媒体大小自此不再影响堆占用**，这是 Status 可放开单条上限的前提。`pending-upload` 同时补上 2GiB 字节上限，并新增 `media-tmp` 孤儿回收（`cleanMediaTmp`，1 小时阈值）。详见 `doc/pending-upload流式改造实施记录.md`

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

- **配对码过期（~160s）**：whatsmeow 二维码耗尽后 WhatsApp 服务器会关闭登录 websocket（`pair-code.go`："160-second time limit"）；登录期 `cli.Store.ID==nil` → `autoReconnect`（client.go:618）直接 return **不重连**，进程保持断开。`handler()` 的 `*events.Disconnected` 分支检测到「登录期断开」（`!cli.IsLoggedIn()`，登录成功后运行期重连不命中）即输出 `MsgPairError`（`pairError`）→ Java 走既有 pairError 分支（ProcessUtils:661）下发 loginTimeout → App 立即显示「配对码已过期，重新发送」，**对齐真实配对码过期时刻**（~160s），而非等 Server 5 分钟 emitter 超时。QR 流程不受影响：`qrchan.go` QR 码耗尽走 `cli.Disconnect()`（预期断开，onDisconnect 不派发 Disconnected），QR 超时仍由 `qrTimeout` 处理。详见 `doc/登录超时与配对码过期对齐方案.md`

### 5. 命令系统（socket 交互）

Java Server 通过 Unix socket 向 Go 进程发送命令（Windows 与 Linux 一致）。命令集：

| 命令 | 说明 |
|------|------|
| `enable-view-once` | 开启 ViewOnce 拦截 |
| `disable-view-once` | 关闭 ViewOnce 拦截 |
| `pair-phone <number>` | 验证码登录 |
| `require-qrcode` | QR 码登录 |
| `set-logintype <type> [deviceName]` | 设置登录类型（`waTracker_android`/`wastgo_win`，须在配对命令前发送）。可选第 2 参数 = 按马甲包指定的设备显示名（Server 按 packageName 解析：weseen→"WeSeen"、assist→"WaTracker"）；缺省 `waTracker_android` 回落 "WeSeen"、`wastgo_win` 恒 "WastGo"。仅对新配对生效（DeviceProps.Os 在配对注册时上报） |
| `reconnect` | 断开并重连 |
| `logout` | 登出 |
| `subscribepresence <jid>` | 订阅联系人在线状态（粘性订阅：即使断线发送失败也记录 JID，重连后 `ResubscribeAll` 补发；发送失败不再阻断，`ClearUnsubscribed` 始终执行）。**重加 JID（仍带退订标记）先协议退订（实测无效，best-effort）再订阅**；2s 内无推送则强制重连兜底（重连为唯一可靠重推机制） |
| `unsubscribepresence <jid>` | **协议退订 + 软退订双层**：① 协议退订（`UnsubscribePresence` 发 `<presence type=unsubscribe>`，whatsmeow 上游无此 API，**实测无效**——服务器不响应停推→重推语义，仅 best-effort）；② 软退订（移出重订阅集 + 标记过滤 + 清空 PresenceCache）。删除/改号 observer 时由 Server 下发 |
| `checkuser <phones...>` | 检查号码是否注册 WhatsApp |
| `getavatar <jid>` | 获取头像 URL |
| `getgroup <jid>` | 获取群组信息 |
| `send <jid> <text>` | 发送文本消息 |
| `getuser <jids...>` | 获取用户信息 |
| `presence <available/unavailable>` | 设置自身在线状态 |
| `setpushname <name>` | 设置推送昵称 |
| `enable-status-track` | 开启 Status（动态）采集总开关（`statusTrackEnabled`）。由 Server 在 `loginSuccess→checkObservers` / re-attach 时按 `sub_user.enable_status_track` 下发 |
| `disable-status-track` | 关闭总开关。**只停止采集，不清理已收数据**——套餐有效期内数据保留 |
| `syncstatusobservers <jid...>` | **全量覆盖** Status 过滤集合（`statusObservers.replace`，**空集合 = 清空**）。Java 侧刻意只提供全量、不做增量：增量漏一次会静默地让集合与服务器侧不一致（表现为「某些联系人永远收不到动态」且不报错） |
| `addstatusobserver <jid>` / `removestatusobserver <jid>` | 增/删单个成员。**Java 侧无调用点**，留作 Web 调试控制台的调试命令；业务路径一律走 `syncstatusobservers` 全量覆盖 |
| `statusack <statusId>` | Server 确认某条 Status 已终态落库（或被唯一键判为重复 / 毒报文 / 刻意丢弃）→ Go 删除对应 `status-outbox/{statusId}.json`。**不 ack 的场景只有 `subUser == null`**（用户释放路径上的瞬态，宁重勿丢） |

### 6. Status（动态）采集

> 方案见 `doc/Status动态追踪功能实现方案.md` §3，实施记录见 `doc/Status动态追踪_Go端实施记录.md`，
> 可行性实测见 `doc/State Track动态追踪可行性验证记录.md`。入口 `status.go`。

**与 ViewOnce / Presence 的三个本质区别**：

1. **不需要订阅协议**：Status 发往 `status@broadcast`，服务器会推给**所有联系人**。
   Go 侧只维护一个「哪些号码的动态要留」的**过滤集合**（`statusObservers`），不做任何 subscribe。
2. **过滤按电话号码**（不是 JID 字符串比对）：比对前统一走 `normalizeStatusJid` 归一化
   （剥 `+` 前缀、去设备号后缀、补 `@s.whatsapp.net`）。**这一步不做就会静默失配**——
   表现为「某些联系人永远收不到动态」，且没有任何报错。
3. **纯文字动态也要保存文案**（产品要求）：`kind=text` 走**完全独立的一条早退路径**——
   没有文件就 `publishStatus` 直接上报，**不碰**下载 / S3 / worker 池 / 队列背压那一整套。
   这是实现时最容易漏的地方（拿一个空 media message 去下载）。

**事件分支必须最先 return**（`main.go` 的 `events.Message`）：动态是发往 `status@broadcast` 的**普通消息**，
若不拦在通用逻辑之前，会被当成「收到一条消息」上报 Java → observer 误标 `ONLINE` + 写脏 `History`。
这是可行性验证记录 §3.3 记的**存量问题**（与 Status 功能无关），本次一并修掉，
连带 `skdm-only` / `REVOKE` / 自己发的动态也不再污染 observer 状态。

**过滤链（全部发生在下载之前）**：总开关 → `evt.Info.IsFromMe` → 纯 SKDM（
`GetSenderKeyDistributionMessage() != nil` 且 `!statusHasContent`，新 sender key 首条的重复派发）
→ `GetProtocolMessage() != nil`（REVOKE 不处理，对齐竞品）→ 过滤集合 → LRU 去重。

**两条可靠性机制（互不替代，覆盖不同断线场景）**：

| 机制 | 兜的场景 | 载体 | 删除时机 |
|---|---|---|---|
| `presenceCache.Notify`（既有，Status 复用） | Java 掉线 | 内存 FIFO | 进程存活期内 |
| `status-outbox/{statusId}.json`（**新增**） | Go 进程重启、两者都重启 | 磁盘 | 收到 `statusack` |
| `pending-upload/`（复用，`Source=status`） | 连 S3 都没传上去 | 磁盘（媒体本体） | 重传成功 |

outbox 的三个触发点：① 写入在 `publishStatus` 里**先落盘再 Notify**；② 重放在 attach 时 +
每 60s（且 `HasJavaClient()` 为真，否则会把记录灌进内存缓冲白占容量）；③ 删除**只由 ack 触发**
（重放不删——否则 Java 落库失败就永久丢了）。两条防呆：**重放间隔抑制 5 分钟**（兜「Server 是旧版、
永不 ack」，否则每 60s 会把最多 5000 条全量重发）+ **毒记录直接删**（JSON 反序列化失败，重放多少次都一样）。

⚠️ **部署顺序 Server 先于 Go**：新 Go 配旧 Server 时 outbox 收不到 ack，会涨到 5000 上限后开始丢最旧。
计划顺序（Server → Proxy → Go → App）天然满足。

**并发与背压**（方案 §3.7）：worker 数即信号量（并发 **2**，`startStatusWorkers`）；队列 64 条，
队满**丢最旧** + `log.Errorf` + `statusDropped.Add(1)`；下载/上传各 120s 超时（view-once 仍 60s，
故 `uploadFileToS3`/`uploadStreamToS3` 新增了 `timeout` 参数）；单条 **100MB** 上限
（`statusMaxFileBytes`，超出只报元数据 + 缩略图，**不下载媒体**）。

**磁盘水位**：剩余 < 200MB 时暂停下载（`statusWaitForDisk`），超过 5 分钟仍不足则降级为
「只上报元数据 + 缩略图」——避免一个饿死的 worker 长期占住并发位。为什么是「暂停」而不是「丢弃」：
动态媒体 24h 后就再也拿不到了，丢一条就是永久缺失；暂停的代价是队列积压并触发背压丢弃，那是设计内行为。
非 Linux 上 `statusFreeDiskBytes` 返回 `ok=false` → 水位保护直接放行（本地调试不会被卡）。

**缩略图**：由 **Go 上传**（Server 的 `thumbKey` 直接取自 Go 上报，不自己生成），且**先于媒体上传**——
这样即使媒体上传失败，列表页仍能渲染。代价是「媒体上传失败 → 进 pending-upload → 该条最终被裁剪」
时会留下一个 1~4KB 的孤儿缩略图，**接受该取舍**（详见实施记录 §9）。

**对象键**：`whatsapp/status/{userJID}/{observerJid}/{statusId}{ext}`，**前缀必须是 `whatsapp/status/`**，
否则 Server 的 `/inner/presignStatusUpload` 直接 400（对照 ViewOnce 的 `whatsapp/view-once/`）。
扩展名走显式 mime→ext 表 `statusMediaExt`（**不用 `mimetype` 库**），以保证首次上传与重传算出**同一个键**。

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

### Java → Go（Unix socket 命令）

Java Server 通过 Unix socket 向 Go 进程写入命令字符串（每行一条命令 + `\n`）：
- 登录命令、订阅命令、ViewOnce 开关等
- 每个用户的命令通过 Java 端 per-user 锁保证不交叉

### Go → Java（Unix socket 输出 — Proto 协议）

Go 进程通过 `ProtoOutput()` 函数经 socket 输出 **`##PROTO##` 前缀的 JSON 消息**，Java Server 的 `ProcessUtils` 解析 `ProtoMessage`（socket 断开时静默忽略，Java 重连后继续输出）。

**输出格式**：`##PROTO##{"type":"<消息类型>","field1":"value1",...}`

**消息类型常量**（定义在 `proto_output.go`）：

| 常量 | type 值 | 说明 |
|------|--------|------|
| `MsgPresence` | `presence` | 联系人在线/离线状态（`state`/`jid`/`lastSeen`/可选 `ts`，统一经 `PresenceCache` 输出；掉线期间缓冲、重连重放） |
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
| `MsgStatusFile` | `statusFile` | **Status（动态）条目上报**（2026-09-22 新增）：`statusId` / `observerId` / `pushName` / `kind`(text·image·video·audio) / `miniType` / `fileLength` / `seconds` / `text`（文案，产品要求保存）/ `objectKey` / `thumbKey` / `postedAt`。⚠️ **必须走 `PresenceCache.Notify`，不得裸 `ProtoOutput`**——裸输出拿不到掉线缓冲，Java 掉线期间会直接丢。⚠️ `kind` 为媒体但 `objectKey` 为空 = 该条媒体不可用（超 100MB 只报元数据，或磁盘水位降级），App 侧据此渲染 |
| `MsgHeartbeat` | `heartbeat` | 进程心跳（60s 间隔） |
| `MsgResubscribe` | `resubscribe` | 重连后重订阅结果 |
| `MsgStreamReplaced` | `streamReplaced` | 被顶号（其他设备登录） |
| `MsgLoggedOut` | `loggedOut` | 服务端登出 |

**稳定性机制**：
- **连接用途标记协议**（`startCommandSocket` accept 循环）：首行发 `attach` 的连接才 `setProtoConn + Replay + flushPendingUploads`；命令/探测连接（sendCommand、health checker probe）只喂命令、不设后端、不重放——避免非 attach 连接提前消耗 PresenceCache 缓冲导致事件写进无人读取的 socket 而丢失。⚠️ **部署顺序：Java 先于 Go**（旧 Java + 新 Go 断裂）。见 `doc/Server重启窗口Observer操作与presence重放风险修复.md`
- **PresenceManager**（`presence_manager.go`）：跟踪所有已订阅的联系人 JID，`events.Connected` 时自动调用 `ResubscribeAll()` 重订阅；含退订标记集（`MarkUnsubscribed/ClearUnsubscribed/IsUnsubscribed`），软退订时 presence handler 跳过该 JID；`Subscribe` 为**粘性订阅**（断线发送失败也记录 JID，重连后 `ResubscribeAll` 补发，替代 Java `loginSuccess→checkObservers` 补订）。重加 JID 流程：`UnsubscribePresence`（**实测无效**，best-effort）→ 订阅 → 2s 内无推送则强制重连兜底（**重连是唯一可靠机制**，重连后 `ResubscribeAll` 全量重订必然重推）
- **PresenceCache**（`presence_cache.go`）：Java 掉线期间的事件重放缓存，覆盖两类事件——① presence（`Handle`）：先入缓冲，Java 在线立即排空、掉线留存，attach 后 `Replay()` 按序重放 + 溢出 JID latest 兜底，重放/实时均带 `ts` 还原历史时间；② 运行时通知（`Notify`，view-once 等）：纯 FIFO，`uploadAndNotify` 走 `Notify` 防 S3 孤儿，溢出记 ERROR；`Remove(jid)` 软退订时清空单 JID 滞留事件；`lastArrival`/`HasFreshEvent(jid,since)` 记录每 JID 最近实时事件到达时刻，供重加订阅后的新鲜度校验（2s 无推送 → 强制重连兜底，协议退订实测无效，重连是唯一可靠重推机制）
- **心跳**：每 60 秒输出 `heartbeat`，上报 goroutine 数、内存使用、订阅数、运行时间。内存分四列：`mem_mb`=Go 堆(`runtime.MemStats.Alloc`)、`rss_mb`=物理驻留(含共享,跨进程重复计数)、`pss_mb`=真实物理(共享页均摊)、`shared_mb`=共享页占用。ps. 生产 Linux 读 `/proc/self/smaps_rollup`，Windows 无 `/proc` 时 rss/pss/shared 报 -1（Java/web 显示「—」）。详见 `doc/web页展示Whatsmeow进程真实内存占用方案.md`。
  **Status 指标**（2026-09-22 追加 5 项）：`status_enabled`(总开关)、`status_observers`(过滤集合大小)、`status_pending`(**outbox 待确认条数**)、`status_dropped`(**累计丢弃数**)、`status_queue`(待处理队列长度)。判读：`status_dropped > 0` = **有动态永久丢失，需告警**；`status_pending` 长期不降 = Server 没在 ack（多半是旧版 Server 未部署 `statusack`）
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

- ~~**`amazon.yaml`**~~（已废弃，不再嵌入）：AWS S3 凭证改由 Server 侧持有，Go 通过预签名 URL 上传
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
| ~~`github.com/aws/aws-sdk-go-v2`~~ | ~~S3 上传（ViewOnce 媒体）~~ 已移除（方案B：改用 Server 预签名 URL） |
| `github.com/gabriel-vasile/mimetype` | MIME 类型检测 |
| `github.com/gin-gonic/gin` | HTTP API（mytest2） |
| `github.com/beeper/argo-go` | Argo 查询协议 |
| `golang.org/x/crypto` | 加密工具 |

## 上游同步

### 同步脚本

使用 `sync-upstream.sh` 自动化合并上游更新：

```bash
cd whatsmeow
./sync-upstream.sh
```

脚本会自动：
1. 拉取上游最新代码
2. 创建同步分支
3. 合并主分支的定制代码
4. 验证编译
5. 合并回主分支

### 冲突处理

如果合并时出现冲突，脚本会提示手动处理。当前核心库的定制代码集中在 3 个文件：

| 文件 | 改动内容 | 处理方式 |
|------|---------|---------|
| `client.go` | `OnLoginSuccess` 回调、日志改进、passkey 字段简化 | 接受上游改动，手动加回 `OnLoginSuccess` 回调 |
| `connectionevents.go` | `OnLoginSuccess` 回调调用、日志改进 | 接受上游改动，手动加回回调调用 |
| `pair.go` | `IOS_PHONE → PairClientChrome` 映射 | 接受上游改动，手动加回 case |

以下文件与上游完全一致，无需处理：
- `store/clientpayload.go` ✓（iOS 设备指纹通过 `SetDeviceFingerprintIOS()` 在运行时配置）
- `types/events/events.go` ✓

### 手动同步（不使用脚本）

如果需要手动同步：

```bash
# 1. 拉取上游
git fetch upstream

# 2. 合并
git checkout main
git merge upstream/main --no-commit

# 3. 解决冲突（如有）

# 4. 验证编译
GOROOT=D:\developmentKit\gosdk\go1.26.2 go build ./...

# 5. 提交
git commit -m "sync: merge upstream changes"
```

## 注意事项

1. **CGO 依赖**：编译时必须启用 CGO（`CGO_ENABLED=1`），需要对应平台的 C 编译器
2. **SQLite 锁定**：每个 Go 进程使用独立的 `mdtest.db`，不存在并发锁问题
3. **LID 解析**：WhatsApp 正在从电话号码迁移到 LID，所有事件中的 sender 可能返回 LID 而非真实号码，必须通过 `searchPhoneNum()` 转换
4. **ViewOnce 下载**：异步进行（`go func()`），不阻塞主事件循环；上传到 S3 后才输出通知
5. **Proto 协议同步**：修改 `##PROTO##` 消息格式或新增消息类型时，需同步修改 Go 的 `proto_output.go`（常量定义）和 Java 的 `TerminalConstants.java`（常量）+ `ProcessUtils.java`（处理逻辑）
6. **上游同步**：Fork 版本需定期与 `tulir/whatsmeow` 同步协议更新，注意解决合并冲突
7. **同步脚本**：使用 `sync-upstream.sh` 自动化同步流程
8. **`mytest2` 是实验性**：多客户端 HTTP 版本尚在开发中，当前生产环境使用 `mytest`（单客户端 + Java Server 管理）
9. **argo 目录**：包含自定义的 Argo 协议查询定义，嵌入了 `.argo` 和 `.json` 文件
10. **跨仓库联动**：修改 Proto 消息格式或 Go↔Java 通信协议后，检查 `watracker-server/CLAUDE.md` 是否需要同步更新（根目录 CLAUDE.md 的「文档同步」规则）
11. **history JSON 文件**：根目录有大量 `history-*.json` 文件，是 HistorySync 事件的调试输出，生产环境可忽略或清理
12. ~~**`amazon.yaml` 包含 S3 密钥**~~：方案B 后 Go 不再嵌入/使用，S3 密钥集中在 Server 侧；旧密钥若曾随二进制/仓库分发建议在 AWS 轮换一次
