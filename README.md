# 临时文件传输项目

一个自用的轻量级临时文件访问与传输工具，用于在外网、局域网、临时设备或他人电脑上安全地浏览、下载、上传指定目录中的文件。当前根目录是唯一 Git 仓库，`backend/` 与 `frontend/` 不再分别维护子仓库。

## 功能概览

- 普通用户使用 TOTP 动态验证码登录，只能浏览、上传、下载已开放目录。
- 管理员使用独立账号密码登录，可管理临时令牌、查看访问记录和配置概览。
- 已登录的 TOTP 普通用户和管理员共享一个全局纯文本聊天室；公开分享访客不能进入。
- 支持受控目录浏览、子目录进入、文件大小和修改时间展示。
- 文件浏览与文件上传分为独立页面：浏览页专注目录和文件列表，上传页支持登录用户拖拽上传、队列上传和失败重试。
- 支持创建临时下载/上传令牌，并按有效期、使用次数和上传累计容量自动失效。
- 支持空闲会话过期：页面离开或长时间无操作后需要重新登录，同时已开始的长下载不会被页面会话失效打断。
- 下载使用短期下载票据，支持 HTTP Range 断点续传；公开下载令牌只在兑换票据时消耗一次使用次数，续传不重复扣次。
- 外部访客可打开前端 `/share/{token}` 分享页完成下载或拖拽上传，不再依赖后端原生 HTML 页面。
- 审计日志记录登录、目录访问、文件列表、上传、下载、令牌创建/撤销/使用等事件，并返回中文 `actionLabel`。
- 支持可信反向代理后的真实 IP 识别，避免访问记录长期显示 `127.0.0.1`。
- 管理员配置页提供“服务访问地址”，区分浏览器当前入口、显式公开地址、本机网卡候选和后端 listener 诊断。
- 支持上传细粒度策略：单次请求总量、单文件大小、单次文件数量、扩展名白名单/黑名单、单上传令牌累计容量。

## 技术栈

| 部分 | 技术 |
| --- | --- |
| 后端 | Go、Fiber、SQLite、YAML、TOTP |
| 前端 | Vue 3、Vite、TypeScript、Vue Router、Bun |
| 部署 | Docker、Docker Compose、nginx 静态前端代理 |
| 鉴权 | TOTP 普通用户 + 管理员账号 + HttpOnly Cookie 会话 |
| 存储 | SQLite 保存会话、令牌、令牌上传累计量、聊天消息/同步元数据和审计日志 |

## 目录结构

```text
.
├── backend/          # Go 后端
├── frontend/         # Vue 前端（Bun）
├── scripts/          # 本地开发辅助脚本
├── LICENSE           # AGPL-3.0-or-later 许可证
└── README.md         # 项目总说明、开发、部署和维护文档
```

运行数据、本地配置、依赖目录和构建产物不纳入 Git：`backend/config.yaml`、`backend/config/`、`backend/data/`、`backend/uploads/`、`frontend/node_modules/`、`frontend/dist/` 等均由根 `.gitignore` 排除。

## 快速开始

### 1. 准备后端配置

```bash
cd backend
cp config.example.yaml config.yaml
```

至少修改：

```yaml
auth:
  totp_secret: "你的 Base32 TOTP Secret"
  session_ttl_seconds: 86400
  idle_timeout_seconds: 7200
  idle_grace_seconds: 30
  admin:
    username: "admin"
    password_hash: "脚本生成的 Argon2id PHC"

abuse:
  login:
    max_concurrent_admin_verifications: 2

chat:
  withdraw_window_seconds: 300
  max_message_chars: 2000
  max_message_bytes: 8192
  retention_days: 90
  max_messages: 50000
  cleanup_batch: 500
  session_messages_per_minute: 20
  ip_messages_per_minute: 60
  global_messages_per_minute: 300

storage:
  allowed_extensions: []
  blocked_extensions: []
  dirs:
    - id: "default"
      name: "Default"
      type: "directory"
      path: "./uploads"
      allow_download: true
      allow_upload: true
  shares: [] # 可选：单文件资源写在这里，或由管理员在配置管理页添加

file_picker:
  roots:
    - id: "uploads"
      name: "上传目录"
      path: "./uploads"
      allow_select_files: true
      allow_select_dirs: true

downloads:
  lease_ttl_seconds: 7200
  lease_max_ttl_seconds: 21600
  content_hash_max_mb: 64
```

`storage.dirs` 用于目录资源，`storage.shares` 可用于单文件资源；管理员登录后也可以在“配置管理”页新增、修改、删除目录和单文件资源，并可通过服务端文件选择器从顶层 `file_picker.roots` 明确配置的允许位置中浏览选择路径。`allowed_extensions` 与 `blocked_extensions` 可在 Web 配置页修改；默认黑名单为空，白名单为空表示允许所有未被黑名单阻断的扩展名。配置保存成功后会写回当前启动参数指定的配置文件，并在同目录生成 `.bak` 备份，对新请求立即生效。

会话默认受 24 小时绝对期限、7200 秒（2 小时）真实活动空闲期限和 30 秒 heartbeat grace 共同约束。前端只有在页面可见且检测到点击、键盘、滚动、触摸等真实活动时才发送心跳；页面隐藏、静置、聊天轮询或长上传本身都不会无限续期会话。已授权的 download/upload transfer lease 使用独立有效期，页面会话随后过期不会中断已经授权的传输。升级不会自动改写现有 `backend/config.yaml`：如果文件中显式保留 `idle_timeout_seconds: 1800`，仍会继续按 1800 秒生效；管理员需要自行改为 `7200`，并按该配置项现有的加载/重启规则使其生效。

`content_hash_max_mb` 默认让 64 MiB 及以下文件做 SHA-256 内容绑定；如需所有文件都做内容级校验，可设为 `0`，但创建票据和首次使用时会增加完整读盘成本。只有启用 `verify_hash_on_every_request` 时，每次 Range 才会重复读取完整文件。

生成 TOTP Secret：

```bash
python3 scripts/generate-totp-secret.py
```

脚本默认生成 20 字节随机数并输出无填充 Base32，可直接写入 `auth.totp_secret`。如需调整随机字节数，可使用 `--bytes`，但不建议低于默认值。

生成管理员密码摘要：

```bash
python3 scripts/hash-admin-password.py
```

脚本会隐藏输入并要求二次确认，默认 YAML 同时包含 `auth.admin.password_hash` 和短期回滚所需的 `auth.admin.password_sha256`，两者来自同一次输入。脚本通过后端 Go CLI 生成摘要，不会打印明文。自动化环境只能从 Secret Manager 或受保护的标准输入提供密码，不应把密码写入命令行、脚本或日志。`--format phc` 只输出 PHC，不包含旧二进制回滚字段。

旧版 `auth.admin.password_sha256` 仍可在迁移窗口内使用。可暂时同时保留两个字段用于回滚，但只要 `password_hash` 非空，登录就只验证 Argon2id，失败后绝不会降级到 SHA-256。需要回滚时必须显式移除 `password_hash` 并重启；确认迁移稳定后应删除旧 SHA-256 值。

### 2. 一键启动前后端

回到仓库根目录后运行：

```bash
./scripts/dev.sh
```

Windows PowerShell：

```powershell
pwsh -File scripts/dev.ps1
```

如果系统只有 Windows PowerShell，也可以使用：

```powershell
powershell -ExecutionPolicy Bypass -File scripts/dev.ps1
```

脚本会：

- 检查 `go` 与 `bun` 是否可用；
- 如果 `frontend/node_modules/` 不存在，自动执行 `bun install`；
- 以 `backend/config.yaml` 启动 Go 后端；
- 启动 Vite 前端开发服务器；
- 将前端开发代理 `/api` 和 `/t` 指向后端。

脚本会显式向后端传入 `-dev -dev-frontend-port <端口>`。`FILE_TRANS_DEV_FRONTEND_PORT` 已废弃且不再生效；直接运行后端默认始终是生产模式，没有开发 CORS/CSRF 例外。

如果后端启动失败，脚本会输出中文原因、后端日志尾部和日志文件路径。常见问题是 `backend/config.yaml` 的 YAML 缩进错误，例如 `file_picker` 应与 `storage` 同级，不能把 `roots/max_page_size/deny_names` 缩进到 `storage.shares` 下面。

默认访问地址：

```text
前端：http://localhost:5173
后端：http://localhost:17878
```

首次运行如果发现 `backend/config.yaml` 不存在，脚本会从示例配置复制一份并退出。请先替换 TOTP Secret、管理员账号和目录配置后再运行。

临时分享令牌不绑定主机名。令牌页会显示 `/share/{token}` 分享路径、当前访问地址、显式公开地址、后端枚举到的本机网卡地址和令牌本身；管理员可以自己选择复制哪一个地址。只要访问者能打开同一套前端地址，把该路径放在对应地址后即可访问。多网卡或局域网部署时，项目不会自动替用户决定唯一 IP；如需额外固定展示某个地址，可显式设置公开分享地址：

管理员“配置管理”页的“服务访问地址”使用同一组信息，但各项含义不同：**当前访问地址**是此浏览器实际打开页面的 origin；**公开访问地址**来自显式前端公开 origin 配置；**网络接口候选**只是后端根据本机网卡和 listener 范围枚举出的可能入口；**后端监听诊断**描述 Go 进程实际绑定的 socket。候选地址只说明地址形态和监听匹配关系，不保证防火墙、NAT、端口映射、反向代理或 TLS 已配置为外部可达。listener socket 可能是 `0.0.0.0:17878`、`[::]:17878` 或代理后的内部端点，它不是可直接复制发布的公开 URL，因此只用于诊断，不混入上方复制列表。

```bash
FRONTEND_PUBLIC_SHARE_ORIGIN=http://192.168.124.9:5173 ./scripts/dev.sh
```

Windows PowerShell：

```powershell
pwsh -File scripts/dev.ps1 -FrontendPublicShareOrigin http://192.168.124.9:5173
```

如果直接使用一键启动脚本传大文件，前端会默认按当前页面主机名直连 Go 后端：打开 `http://localhost:5173` 时使用 `http://localhost:17878`，打开 `http://192.168.124.9:5173` 时使用 `http://192.168.124.9:17878`。这会绕过 Vite 开发代理，减少一跳传输开销。

如需固定为某个域名、反向代理地址或特殊端口，也可以显式指定浏览器能访问到的后端地址：

```bash
FRONTEND_TRANSFER_ORIGIN=http://192.168.124.9:17878 ./scripts/dev.sh
```

Windows PowerShell：

```powershell
pwsh -File scripts/dev.ps1 -FrontendTransferOrigin http://192.168.124.9:17878
```

无论使用默认直连还是显式指定，页面仍从前端端口打开，但上传票据、下载票据对应的大文件传输会直接访问后端地址。一键开发模式会自动允许“同一主机名 + 前端开发端口”的直连来源，例如 `http://192.168.124.9:5173` 直连 `http://192.168.124.9:17878` 不需要额外写 CORS；如果你显式指定了不同域名、不同主机或生产跨域地址，则仍需把该前端来源加入 `backend/config.yaml` 的 `cors.allow_origins`。地址写错、后端不可达或 CORS 未允许时不会悄悄改走代理，前端会直接提示连接失败，便于管理员修正配置。

### 3. 手动分别启动

后端：

```bash
cd backend
go run ./cmd/server -config config.yaml -dev -dev-frontend-port 5173
```

前端：

```bash
cd frontend
bun install
bun run dev
```

Vite 会把 `/api` 和 `/t` 代理到 `http://127.0.0.1:17878`。打开 Vite 输出的地址后，普通用户使用 TOTP 登录；管理员切换到“管理员账号”模式登录。代理目标默认使用 IPv4 地址，避免部分 Windows/Node 环境把 `localhost` 解析成 `::1` 后连接失败；代理转发时会把 `Origin` 改写为后端同源，避免通过局域网 IP 访问 Vite 时被后端 CSRF Origin 防护误拒绝。

手动分别启动时，如果只执行 `bun run dev -- --port 5174`，前端会安全地继续走 Vite 代理。若手动启动也想启用开发端口直连例外，需要以 `-dev -dev-frontend-port 5174` 启动后端，并给前端设置 `VITE_FRONTEND_PORT=5174`、`VITE_TRANSFER_PORT=<后端端口>`；更推荐直接使用根目录一键脚本。

## 修改绑定端口

本地开发涉及两个端口：后端监听端口和前端 Vite 开发服务器端口。

### 后端端口

后端端口由 `backend/config.yaml` 中的 `server.port` 控制：

```yaml
server:
  host: "0.0.0.0"
  port: 17878
```

例如要把后端改到 `9000`：

```yaml
server:
  host: "0.0.0.0"
  port: 9000
```

同时需要让前端代理也指向新端口。使用一键脚本时：

```bash
BACKEND_PORT=9000 ./scripts/dev.sh
```

Windows PowerShell：

```powershell
pwsh -File scripts/dev.ps1 -BackendPort 9000
```

也可以直接传完整后端地址：

```bash
BACKEND_ORIGIN=http://127.0.0.1:9000 ./scripts/dev.sh
```

Windows PowerShell：

```powershell
pwsh -File scripts/dev.ps1 -BackendOrigin http://127.0.0.1:9000
```

手动启动前端时：

```bash
cd frontend
VITE_BACKEND_ORIGIN=http://127.0.0.1:9000 bun run dev
```

### 前端开发端口

一键脚本默认让 Vite 监听 `5173`。如需改为 `5174`：

```bash
FRONTEND_PORT=5174 ./scripts/dev.sh
```

Windows PowerShell：

```powershell
pwsh -File scripts/dev.ps1 -FrontendPort 5174
```

手动启动前端时：

```bash
cd frontend
bun run dev -- --port 5174
```

一键开发脚本会把当前前端端口同时传给前端和后端，默认直连传输仍会自动允许“同一主机名 + 当前前端端口”的来源。只有显式指定不同域名、不同主机或生产跨域地址时，才需要同步修改 `backend/config.yaml` 的 `cors.allow_origins`：

```yaml
cors:
  allow_origins:
    - "http://localhost:5174"
```

## 全局聊天室

登录后的 `/chat` 是一个单一全局聊天室，TOTP 普通用户与管理员看到同一条消息流；`/share/{token}` 等公开分享访客不能使用聊天。消息只接受纯文本 JSON，不渲染 Markdown 或 HTML，也不支持附件。普通用户只能在发送后 300 秒（5 分钟）内撤回自己的普通用户消息；普通端不会看到撤回原文、可信客户端 IP 或内部所有权标识，管理员可在独立管理视图中查看撤回原文和后端按 trusted proxy 规则解析出的来源 IP。

管理员删除任意消息时，应用会在同一事务中把 SQLite 当前记录的正文设为 `NULL`，保留 tombstone、状态变更和不含正文的审计记录。这里描述的是应用层数据库语义，不承诺对 SQLite 历史页、WAL、文件系统快照、备份或外部日志做取证级擦除；需要这类保证时应同时执行专门的存储介质与备份销毁流程。

默认消息边界和速率如下：

- 正文最多 2000 个 Unicode code points，解码后的 UTF-8 正文最多 8192 bytes；包含 JSON 语法、空白和 escape 在内的原始 HTTP 请求体也固定最多 8192 bytes，原始请求限制可能先触发。
- 发送速率为每 session 20 条/分钟、每可信客户端 IP 60 条/分钟、单实例全局 300 条/分钟，任一层达到上限都会返回 `Retry-After`。
- 初始/history 默认 50 条、单页最多 100 条；changes 默认 50 条、单页最多 500 条。
- 消息按 90 天或 50000 条两项中先达到者保留，后台每个短事务最多清理 500 条。

首版同步使用短轮询，不使用 WebSocket 或 SSE。页面隐藏时前端暂停聊天轮询，重新可见后继续；这不是后台未读推送。history 会返回 `generation` 和 `latestChangeSeq`，changes 请求必须携带 generation。generation 变化、changes 返回 HTTP 409 / `chat_cursor_reset_required`，或前端发现游标不可继续时，应清空本地聊天缓存并重新加载 history。mutation 返回的 `eventSeq` 只标识该次事件，不能当作全局 changes cursor；同步只能从 history 的 `latestChangeSeq` 或 changes 的 `nextAfterSeq` 推进。

## 日常维护：清除数据库和记录

后端运行数据默认保存在 `backend/data/`，上传文件默认按 `storage.dirs` 指向的目录保存。清理前请先停止后端服务，并确认已经备份需要保留的文件。

### 清空全部数据库记录

如果只想清除登录会话、令牌、传输票据、聊天消息和审计日志等全部数据库记录，保留已上传文件，可删除 SQLite 数据库文件后重启服务，后端会自动重新建表：

```bash
cd backend
rm -f data/filetrans.db
go run ./cmd/server -config config.yaml
```

如果 `backend/config.yaml` 中 `database.path` 改过，请删除实际配置的数据库文件，而不是上面的默认路径。

### 只清除访问记录

审计日志在 `audit.retain` 中配置保留条数。若需要立即清空访问记录，可在停止后端后直接操作 SQLite：

```bash
cd backend
sqlite3 data/filetrans.db "DELETE FROM audit_logs; VACUUM;"
```

### 清除会话、令牌和传输票据

如果希望让所有登录态、分享链接、已签发上传票据和已兑换下载票据立即失效，但保留审计日志：

```bash
cd backend
sqlite3 data/filetrans.db "DELETE FROM sessions; DELETE FROM upload_leases; DELETE FROM download_leases; DELETE FROM tokens; VACUUM;"
```

### 清除上传文件

上传文件不一定都在 `backend/uploads/`，以 `storage.dirs[].path` 为准。确认目录后再删除，例如默认配置：

```bash
rm -rf backend/uploads/*
```

不要删除 `backend/config.yaml`，除非你希望重新配置服务。

## 关键配置

> 升级提示：本版本为公开 token 和 download lease 增加资源授权指纹。旧数据库中没有指纹的临时下载/上传 token 与 download lease 会安全失效，需要重新创建；审计记录仍会保留。

| 配置项 | 说明 |
| --- | --- |
| `server.host` / `server.port` | 服务监听地址和端口 |
| `server.trust_proxy_headers` | 是否启用可信代理解析；直接运行保持 `false` |
| `server.trusted_proxy_cidrs` | 可信代理 socket 来源 CIDR；启用代理解析时必填，最多 64 项 |
| `server.keepalive_idle_timeout_seconds` | keep-alive 请求间空闲超时，默认 120 秒；只读展示且修改后需重启，不是传输总时长 |
| `storage.directory_list_*` | 普通目录扫描窗口与分页上限，默认 5000/200；截断时不自动拉取全量 |
| `file_picker.max_scan_entries` / `max_page_size` | 管理员 picker 扫描窗口与分页上限，默认 5000/200 |
| `downloads.max_concurrent_hashes` | 下载内容哈希并发上限，默认 2；满载立即返回 503 |
| `downloads.verify_hash_on_every_request` | 默认仅首次使用校验内容哈希；不可信本地写者场景可启用每次请求校验 |
| `database.path` | SQLite 数据库路径 |
| `auth.totp_secret` | 普通用户 TOTP Base32 密钥 |
| `auth.admin.username` / `auth.admin.password_hash` | 管理员账号与 Argon2id PHC；`password_sha256` 仅用于旧配置回滚 |
| `abuse.login.max_concurrent_admin_verifications` | Argon2id 管理员验证并发槽，默认 2；0 表示不限制 |
| `abuse.login.global_per_minute` / IP 失败窗口 | user/admin 独立的单实例登录总速率与可信客户端 IP 失败封禁 |
| `abuse.creation.*` | token/lease 创建速率、活跃 token 与 outstanding lease 数量上限；单项 0 表示关闭 |
| `audit.unauthorized_*` / `prune_every_writes` | 高频未授权审计采样、全局上限和批量保留清理；关键安全事件不采样 |
| `abuse.uploads.*` | 单实例上传全局、资源、session 与公开 token 并发上限；单项 0 表示关闭 |
| `storage.min_free_mb` / `min_free_percent` | 上传临时文件创建前的磁盘保留阈值，两者取较大值 |
| `auth.session_ttl_seconds` | 登录会话绝对有效期，默认 86400 秒（24 小时） |
| `auth.idle_timeout_seconds` | 真实用户活动的会话空闲过期时间，默认 7200 秒（2 小时）；现有配置中的显式旧值不会自动迁移 |
| `auth.idle_grace_seconds` | 仅 heartbeat 可使用的恢复宽限，默认 30 秒；普通 API 不使用 |
| `auth.cookie_secure` | HTTPS 部署时建议设为 `true` |
| `chat.withdraw_window_seconds` | 普通用户撤回自己消息的窗口，默认 300 秒 |
| `chat.max_message_chars` | 解码后正文的 Unicode code point 上限，默认 2000 |
| `chat.max_message_bytes` | 解码后正文的 UTF-8 字节上限，默认 8192；聊天原始 JSON 请求体另有固定 8192 bytes 上限 |
| `chat.retention_days` | 聊天按时间保留天数，默认 90 天，与数量上限先到者生效 |
| `chat.max_messages` | 聊天保留消息数量上限，默认 50000，与时间上限先到者生效 |
| `chat.cleanup_batch` | 每个聊天清理短事务最多删除的消息数，默认 500 |
| `chat.session_messages_per_minute` | 每个登录 session 的发送上限，默认 20 条/分钟 |
| `chat.ip_messages_per_minute` | 每个可信解析客户端 IP 的发送上限，默认 60 条/分钟 |
| `chat.global_messages_per_minute` | 单后端实例的聊天发送总上限，默认 300 条/分钟 |
| `web.static_dir` | 前端构建产物目录 |
| `cors.allow_origins` | 显式跨域来源，不能使用 `*`；生产空列表仅允许同源 |
| `storage.upload_max_mb` | 单次上传请求总大小限制，示例默认 5120 MB |
| `storage.upload_max_file_mb` | 单个文件大小限制，示例默认 5120 MB |
| `storage.upload_max_files` | 单次请求最多文件数量 |
| `storage.allowed_extensions` / `storage.blocked_extensions` | 上传扩展名白名单与黑名单，管理员可在 Web 配置页维护 |
| `storage.dirs` | 开放目录资源列表及上传/下载权限，可由管理员配置管理页维护 |
| `storage.shares` | 单文件共享资源列表，单文件资源只允许下载，可由管理员配置管理页维护 |
| `file_picker.roots` | 新增或修改资源路径的唯一允许范围；未配置时不自动提供系统入口 |
| `file_picker.max_page_size` | 文件选择器单页最大返回条目数 |
| `tokens.default_ttl_seconds` | 临时令牌默认有效期 |
| `tokens.max_ttl_seconds` | 临时令牌最长有效期，管理员输入更长时间会被夹紧到该上限 |
| `tokens.upload_max_mb` | 单个上传令牌累计上传容量，示例默认 5120 MB，`0` 表示不限制 |
| `audit.retain` | 审计日志保留条数 |
| `audit.unauthorized_sample_seconds` / `unauthorized_global_per_minute` | 认证噪声采样；单项 0 表示不限制审计写入，并非关闭审计 |
| `audit.prune_every_writes` | 累计写入后的批量清理周期；0 表示仅显式维护清理 |

## 部署方式

### 方式一：前后端分容器

Compose 必须使用目录挂载：宿主机配置文件为 `backend/config/config.yaml`，容器内对应 `/app/config/config.yaml`。不要只挂载单个文件，否则配置管理页无法通过原子替换安全写回配置。

在 `backend/` 中准备配置目录后运行：

```bash
cd backend
mkdir -p config
cp config.example.yaml config/config.yaml
# 必须修改 auth.totp_secret、auth.admin，并确认 storage.dirs / storage.shares 指向需要开放的资源
# Compose 内置 Nginx 使用固定网络 172.28.0.0/24：配置 trust_proxy_headers: true
# 并设置 trusted_proxy_cidrs: ["172.28.0.0/24"]
docker compose -f docker-compose.example.yml up -d --build
```

浏览器默认访问：

```text
http://服务器地址:17878
```

前端 nginx 容器会代理 `/api` 和 `/t` 到后端容器，默认允许 10G 上传并关闭上传代理缓冲，以便大文件稳定透传到后端。此方式要求 `backend/` 和 `frontend/` 保持同级目录；后端容器使用非 root 用户运行，请确保挂载的 `config/`、`data/` 和上传目录对容器用户可写。配置管理页依赖 `backend/config/` 到 `/app/config/` 的目录挂载来原子写回 `config.yaml`；若希望禁止管理员页面在线写回配置，可把配置目录改成只读挂载。

如需修改 Docker 对外访问端口，调整 `backend/docker-compose.example.yml` 中的左侧端口即可：

```yaml
ports:
  - "9000:80"
```

仅修改宿主机访问端口时，容器内部后端仍可保持 `17878`，因为前端容器通过 Docker 网络访问 `backend:17878`。

### 方式二：后端托管前端静态文件

```bash
cd frontend
bun install --frozen-lockfile
bun run build

cd ../backend
cp config.example.yaml config.yaml
# 确保 web.static_dir 指向 ../frontend/dist
go run ./cmd/server -config config.yaml
```

后端会托管 `web.static_dir` 指向的前端构建产物，并对非 `/api`、非 `/t` 路径回退到 `index.html`，适配 Vue 单页应用路由。

### 反向代理与真实 IP

若部署在 Nginx、Caddy、Traefik 等可信反向代理之后，并希望访问记录显示真实客户端 IP，需要：

1. 在反向代理层覆盖 `X-Forwarded-For`、`X-Real-IP`、`X-Forwarded-Host` 和 `X-Forwarded-Proto`，不要拼接客户端原始转发头；
2. 将 `server.trust_proxy_headers` 设置为 `true`，并在 `server.trusted_proxy_cidrs` 中填写实际代理网络；
3. 后端端口只允许代理访问。直接运行时保持 `false` 和空 CIDR 列表。

示例 Compose 固定内部网络为 `172.28.0.0/24`。使用该 Compose 时配置 `trust_proxy_headers: true` 和 `trusted_proxy_cidrs: ["172.28.0.0/24"]`。如果该 subnet 与现有 Docker 或宿主网络冲突，必须同时修改 Compose subnet 和后端 `trusted_proxy_cidrs`，二者不能只改一处。后端只根据 socket remote address 判断代理是否可信，直连请求中的伪造转发头会被忽略。

后端 `/api/health/live` 是不访问数据库的 liveness，`/api/health/ready` 和兼容别名 `/api/health` 是 readiness。首次停止信号会先摘除 readiness，再无限等待活跃上传、下载和 Range 完成。Compose 的 `stop_grace_period: 24h` 只是容器运行时最终强杀上限，不是应用传输 timeout；可能持续超过 24 小时的部署必须提高它。Kubernetes 同样应设置足够大的 `terminationGracePeriodSeconds`。keep-alive IdleTimeout 仅帮助关闭请求间空闲连接，ReadTimeout/WriteTimeout 保持为 0。

登录失败限速和审计日志都会使用后端解析出的客户端 IP。

大文件下载限速建议放在反向代理层统一处理，例如 Nginx 可按部署场景配置 `limit_rate`、`limit_conn` 或更细的下载 location 策略，避免应用进程自行节流影响 HTTP Range 续传和静态文件发送效率。

### Cookie 与跨站请求防护

- HTTPS 生产部署时应将 `auth.cookie_secure` 设置为 `true`，确保浏览器只通过安全连接发送会话 Cookie。
- `cors.allow_origins` 必须精确列出生产前端来源；后端会对 `/api` 下非空 `Origin` 的状态变更请求做白名单校验。
- 一键开发默认额外允许同一主机名的开发前端端口来源，用于直连后端传输；如果使用不同域名、不同主机或生产访问域名，需要同步更新 `cors.allow_origins`，否则登录、上传、令牌管理等 Cookie 凭据请求会被拒绝。

### 空闲会话与长时间传输部署建议

推荐保留较短空闲时间和较长下载票据时间：

```yaml
auth:
  session_ttl_seconds: 86400
  idle_timeout_seconds: 7200
  idle_grace_seconds: 30

downloads:
  lease_ttl_seconds: 7200
  lease_max_ttl_seconds: 21600
  content_hash_max_mb: 64
```

- `session_ttl_seconds` 默认 86400 秒，是登录态绝对最长有效期。
- `idle_timeout_seconds` 默认 7200 秒，只由页面可见时的真实用户活动推动心跳续期；隐藏页面、静置页面、聊天/列表轮询和长上传本身不会无限续期。
- `idle_grace_seconds` 默认 30 秒，是心跳恢复宽限期；普通业务请求不会使用宽限期，只有 `/api/auth/heartbeat` 可在短暂超时后恢复会话。
- `downloads.lease_ttl_seconds` 是下载票据有效期。用户点击下载时先兑换票据，文件传输和 HTTP Range 断点续传使用票据地址，不依赖页面会话继续在线。
- `downloads.content_hash_max_mb` 控制下载票据内容哈希阈值。默认对 64 MiB 及以下文件记录 SHA-256，并在票据首次成功使用时复核；设为 `0` 可对所有文件启用内容哈希，因此大文件创建票据和首次使用时都会读取完整文件。只有启用 `verify_hash_on_every_request` 时，每次 Range 续传才重复哈希。
- 公开下载令牌在兑换票据时消耗一次 `uses`，同一票据的 Range 续传不会重复扣次数。
- 兼容保留的 `/t/:token/download` 只显示确认页，用户主动点击后才兑换票据，避免邮件扫描、聊天软件预览等自动 GET 请求提前消耗一次性令牌。

升级不会重写用户已有的真实配置。如果现有 `backend/config.yaml` 显式写着 `idle_timeout_seconds: 1800`，它仍按 1800 秒生效；需要采用新默认值时，管理员必须自行改为 `7200`，并按该项现有生效规则处理。用户离开或静置页面后会在配置的空闲时间内变成未登录，但已经开始的大文件下载、已授权上传和同一票据有效期内的断点续传不会被页面会话空闲过期打断。若票据过期、已使用或资源发生变化，需要回到页面重新授权。

上传和下载允许持续数小时，传输总时长不应被固定的代理总超时截断。反向代理的读写超时应理解并配置为“连接连续没有任何数据进展时的空闲超时”，只在长时间没有上传、下载或响应字节流动时终止连接；不要设置固定的总传输时长上限。下载票据有效期仍需覆盖下载和可能的 Range 续传窗口。

### 上传安全策略

`backend/config.example.yaml` 中提供以下上传限制：

- `storage.upload_max_mb`：单次上传请求总量，示例默认 5120 MB；
- `storage.upload_max_file_mb`：单个文件大小，示例默认 5120 MB；
- `storage.upload_max_files`：单次请求文件数量；
- `storage.allowed_extensions`：扩展名白名单，空数组表示不限制；
- `storage.blocked_extensions`：扩展名黑名单，优先级高于白名单；默认清空，可在管理页按需添加；
- `tokens.max_ttl_seconds`：临时令牌最长有效期，避免误创建长期公开链接。
- `tokens.upload_max_mb`：单个上传令牌累计上传容量，示例默认 5120 MB，`0` 表示不限制。

上传文件名会先规范化再做扩展名判断，尾随空格、尾随点和控制字符不会绕过策略。扩展名策略只是准入规则，不等同于内容安全检测；策略修改后只影响之后的新上传和公开上传令牌。登录态上传和公开分享上传都会先创建短期、单次使用的 bearer 上传票据，前端默认把文件作为原始字节流提交到 `rawUploadUrl`，后端从连接开始直接读取请求体、写入目标目录内的 `.upload-*.tmp`，并实时更新管理员“正在传输”页的进度和速度；完整写入后再提交为最终文件名并立即删除临时文件。上传票据绑定资源授权指纹，资源路径、类型或上传权限变化后旧票据不能写入同 ID 的新资源路径。直连调用示例：`POST /api/files/upload-raw-by-lease`，请求头 `Authorization: Bearer <lease>`，请求体就是文件内容，不要把 `lease` 放入 URL 查询参数。旧的 `multipart/form-data` 上传入口仍兼容保留，但前端默认不再使用它。如果同一次多文件上传中途失败，本次已保存文件会被清理，公开上传令牌的次数和容量也会回滚。上传页面会显示速度和剩余时间，但传输或进度轮询不会替代真实用户活动来无限保活页面会话；即使页面登录状态过期，已获得票据的当前上传仍会继续，完成后再提示重新登录查看文件。

如果生产环境放在反向代理后，上传 location 必须关闭请求体缓冲，例如 Nginx 使用 `proxy_request_buffering off;`，否则代理会先收完整个请求体再转发给后端，管理员页仍只能看到代理到后端阶段的进度。

### 管理员可视化配置

管理员登录后可进入“配置管理”页面维护共享资源：

- **目录资源**：写入 `storage.dirs`，可分别控制下载和上传权限；允许上传时后端会校验目录可写。
- **单文件资源**：写入 `storage.shares`，只允许下载；文件浏览页会把它显示为一个单文件入口，令牌页可直接为它生成下载分享。
- **上传策略**：可在页面中维护扩展名白名单和黑名单；输入会自动去重、小写化并补齐前导点，黑名单清空时需要二次确认。
- **服务访问地址**：当前 origin 表示此浏览器入口，显式公开 origin 表示管理员预先配置的对外地址，网卡项只是与 listener 范围匹配的本机候选；下方 listener socket 仅用于诊断进程绑定，不是公开 URL。任何候选都不替代防火墙、NAT、反向代理和 TLS 的实际连通性验证。
- **服务端文件选择器**：路径输入旁的“浏览”按钮会打开只读弹窗，只能从 `file_picker.roots` 明确配置的允许位置选择路径；列表默认目录优先并支持按名称、类型、大小、修改时间排序。弹窗不提供删除、重命名、移动、上传或编辑能力，也不会自动暴露系统根目录。容器部署时看到的是容器内路径，宿主机目录必须先挂载到容器内。
- 保存时后端只接收安全字段，不会把 `auth.totp_secret`、`auth.admin.password_hash`、`auth.admin.password_sha256`、`database.path` 等敏感配置返回给前端。
- 后端会校验资源 ID、路径类型、读写权限和危险系统目录；文件选择器只负责选择路径，真正保存资源时仍会再次校验。写入采用同目录临时文件替换，并在当前配置文件旁保留 `.bak` 作为最近一次备份。
- 配置写回成功后会热更新内存中的资源列表，新请求立即可见；监听端口、数据库路径、认证密钥等仍需手动修改配置文件并重启服务。
- 在线保存会由后端重新序列化配置文件，原 YAML 注释和手工排版不会保留；如需保留人工注释，请先备份并在保存后按需整理。

## 安全设计

- TOTP Secret 只保存在后端配置中，不会发送到前端。
- 普通 TOTP 用户和管理员权限分离；令牌管理与审计日志仅管理员可访问。
- 会话使用 HttpOnly Cookie，支持过期、退出和过期记录清理。
- 服务端数据库只保存会话 ID 哈希，不保存可直接复用的 Cookie 明文值。
- `/api` 下的状态变更请求会校验非空 `Origin` 是否在 `cors.allow_origins` 中，降低 Cookie 凭据接口的跨站请求风险。
- 临时令牌明文只在创建时返回一次，数据库中只保存 SHA-256 哈希。
- `/t/...` 分享地址和下载票据 URL 本身属于 bearer capability。应用响应会禁止缓存、引用来源和搜索引擎索引，项目内置 nginx 也会把访问日志中的 `/t/<token>` 改写为 `/t/[redacted]`，且不会记录查询参数。若前方还有云负载均衡、CDN、WAF 或其他反向代理，仍必须单独配置 URL、查询参数、Referer 和 Authorization 脱敏。
- 文件路径拒绝绝对路径、NUL 字符和任何 `..` 段，并做符号链接逃逸防护。
- 登录下载和公开下载都会显式检查目标存在且不是目录。
- 公开令牌信息接口只返回有效令牌的有限元数据；过期、撤销、耗尽、上传容量耗尽的令牌只返回失效原因。
- 上传采用临时文件完整写入后提交，避免半成品以最终文件名可见；前端默认使用原始字节流上传，便于后端从连接开始观测进度和执行取消；同时受数量、大小、扩展名和令牌累计容量限制。
- 管理员“正在传输”页会展示活跃上传和下载。原始字节流上传有后端精确进度、速度和可靠取消；下载保持极速发送路径，只做最佳努力观测，不承诺可靠取消。
- 审计和登录限速默认使用连接 IP；部署在可信反向代理后可启用 `server.trust_proxy_headers` 获取真实客户端 IP。
- 普通用户获取目录列表时不会返回服务端真实目录路径；管理员配置概览仍可看到目录根路径。

## 常用命令

后端：

```bash
cd backend
go test ./...
go vet ./...
go build ./cmd/server
go run ./cmd/server -config config.yaml
```

一键开发：

```bash
./scripts/dev.sh
BACKEND_PORT=9000 FRONTEND_PORT=5174 ./scripts/dev.sh
BACKEND_CONFIG=backend/config.local.yaml ./scripts/dev.sh
```

Windows PowerShell：

```powershell
pwsh -File scripts/dev.ps1
pwsh -File scripts/dev.ps1 -BackendPort 9000 -FrontendPort 5174
pwsh -File scripts/dev.ps1 -BackendConfig backend/config.local.yaml
```

前端：

```bash
cd frontend
bun install --frozen-lockfile
bun run dev
bun run typecheck
bun run build
bun run preview
```

## 验证清单

- 后端：`go test ./...`
- 后端：`go vet ./...`
- 后端：`go build ./cmd/server`
- 前端：`bun install --frozen-lockfile`
- 前端：`bun run typecheck`
- 前端：`bun run build`

## 许可证

本项目以 GNU Affero General Public License v3.0 or later（`AGPL-3.0-or-later`）发布，完整条款见仓库根目录 `LICENSE`。

AGPL 适合网络服务类项目：如果你修改本项目并通过网络向用户提供服务，需要按 AGPL 要求向这些用户提供对应源码。
