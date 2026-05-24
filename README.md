# 临时文件传输项目

一个自用的轻量级临时文件访问与传输工具，用于在外网、局域网、临时设备或他人电脑上安全地浏览、下载、上传指定目录中的文件。当前根目录是唯一 Git 仓库，`backend/` 与 `frontend/` 不再分别维护子仓库。

## 功能概览

- 普通用户使用 TOTP 动态验证码登录，只能浏览、上传、下载已开放目录。
- 管理员使用独立账号密码登录，可管理临时令牌、查看访问记录和配置概览。
- 支持受控目录浏览、子目录进入、文件大小和修改时间展示。
- 文件浏览与文件上传分为独立页面：浏览页专注目录和文件列表，上传页支持登录用户拖拽上传、队列上传和失败重试。
- 支持创建临时下载/上传令牌，并按有效期、使用次数和上传累计容量自动失效。
- 支持空闲会话过期：页面离开或长时间无操作后需要重新登录，同时已开始的长下载不会被页面会话失效打断。
- 下载使用短期下载票据，支持 HTTP Range 断点续传；公开下载令牌只在兑换票据时消耗一次使用次数，续传不重复扣次。
- 外部访客可打开前端 `/share/{token}` 分享页完成下载或拖拽上传，不再依赖后端原生 HTML 页面。
- 审计日志记录登录、目录访问、文件列表、上传、下载、令牌创建/撤销/使用等事件，并返回中文 `actionLabel`。
- 支持可信反向代理后的真实 IP 识别，避免访问记录长期显示 `127.0.0.1`。
- 支持上传细粒度策略：单次请求总量、单文件大小、单次文件数量、扩展名白名单/黑名单、单上传令牌累计容量。

## 技术栈

| 部分 | 技术 |
| --- | --- |
| 后端 | Go、Fiber、SQLite、YAML、TOTP |
| 前端 | Vue 3、Vite、TypeScript、Vue Router、Bun |
| 部署 | Docker、Docker Compose、nginx 静态前端代理 |
| 鉴权 | TOTP 普通用户 + 管理员账号 + HttpOnly Cookie 会话 |
| 存储 | SQLite 保存会话、令牌、令牌上传累计量和审计日志 |

## 目录结构

```text
.
├── backend/          # Go 后端
├── frontend/         # Vue 前端（Bun）
├── scripts/          # 本地开发辅助脚本
├── LICENSE           # AGPL-3.0-or-later 许可证
└── README.md         # 项目总说明、开发、部署和维护文档
```

运行数据、本地配置、依赖目录和构建产物不纳入 Git：`backend/config.yaml`、`backend/data/`、`backend/uploads/`、`frontend/node_modules/`、`frontend/dist/` 等均由根 `.gitignore` 排除。

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
  idle_timeout_seconds: 180
  admin:
    username: "admin"
    password_sha256: "管理员密码的 SHA-256 十六进制摘要"

storage:
  dirs:
    - id: "default"
      name: "Default"
      path: "./uploads"
      allow_download: true
      allow_upload: true

downloads:
  lease_ttl_seconds: 7200
  lease_max_ttl_seconds: 21600
  content_hash_max_mb: 64
```

`session_ttl_seconds` 是登录态绝对最长有效期，`idle_timeout_seconds` 是用户无活动后的空闲过期时间。前端只在页面可见且检测到点击、键盘、滚动、触摸等操作时发送心跳，因此用户离开页面后会在较短时间内变成未登录。下载不直接依赖长期页面会话；点击下载会先创建短期 `download lease`，已开始的下载和同一文件的 Range 续传在票据有效期内继续可用。`content_hash_max_mb` 默认让 64 MiB 及以下文件做 SHA-256 内容绑定；如需所有文件都做内容级校验，可设为 `0`，但大文件下载和续传前会增加完整读盘成本。

生成 TOTP Secret：

```bash
python3 scripts/generate-totp-secret.py
```

脚本默认生成 20 字节随机数并输出无填充 Base32，可直接写入 `auth.totp_secret`。如需调整随机字节数，可使用 `--bytes`，但不建议低于默认值。

生成管理员密码摘要：

```bash
python3 scripts/hash-admin-password.py
```

脚本会隐藏输入并要求二次确认，输出结果写入 `auth.admin.password_sha256`。如果需要在非交互环境生成，也可以使用管道：

```bash
printf '%s' 'your-password' | python3 scripts/hash-admin-password.py
```

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

默认访问地址：

```text
前端：http://localhost:5173
后端：http://localhost:17878
```

首次运行如果发现 `backend/config.yaml` 不存在，脚本会从示例配置复制一份并退出。请先替换 TOTP Secret、管理员账号和目录配置后再运行。

### 3. 手动分别启动

后端：

```bash
cd backend
go mod tidy
go run ./cmd/server -config config.yaml
```

前端：

```bash
cd frontend
bun install
bun run dev
```

Vite 会把 `/api` 和 `/t` 代理到 `http://localhost:17878`。打开 Vite 输出的地址后，普通用户使用 TOTP 登录；管理员切换到“管理员账号”模式登录。

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

如果前端端口不是 `5173`，请同步修改 `backend/config.yaml` 的 `cors.allow_origins`，否则 Cookie 登录请求会被浏览器 CORS 策略拦截：

```yaml
cors:
  allow_origins:
    - "http://localhost:5174"
```

## 日常维护：清除数据库和记录

后端运行数据默认保存在 `backend/data/`，上传文件默认按 `storage.dirs` 指向的目录保存。清理前请先停止后端服务，并确认已经备份需要保留的文件。

### 清空全部数据库记录

如果只想清除登录会话、令牌、下载票据和审计日志等数据库记录，保留已上传文件，可删除 SQLite 数据库文件后重启服务，后端会自动重新建表：

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

### 清除会话、令牌和下载票据

如果希望让所有登录态、分享链接和已兑换下载票据立即失效，但保留审计日志：

```bash
cd backend
sqlite3 data/filetrans.db "DELETE FROM sessions; DELETE FROM download_leases; DELETE FROM tokens; VACUUM;"
```

### 清除上传文件

上传文件不一定都在 `backend/uploads/`，以 `storage.dirs[].path` 为准。确认目录后再删除，例如默认配置：

```bash
rm -rf backend/uploads/*
```

不要删除 `backend/config.yaml`，除非你希望重新配置服务。

## 关键配置

| 配置项 | 说明 |
| --- | --- |
| `server.host` / `server.port` | 服务监听地址和端口 |
| `server.trust_proxy_headers` | 可信反向代理后读取 `X-Forwarded-For` / `X-Real-IP`，供审计和限速使用 |
| `database.path` | SQLite 数据库路径 |
| `auth.totp_secret` | 普通用户 TOTP Base32 密钥 |
| `auth.admin.username` / `auth.admin.password_sha256` | 管理员账号与密码 SHA-256 摘要 |
| `auth.session_ttl_seconds` | 登录会话有效期 |
| `auth.cookie_secure` | HTTPS 部署时建议设为 `true` |
| `web.static_dir` | 前端构建产物目录 |
| `cors.allow_origins` | 允许的前端来源，不能使用 `*` |
| `storage.upload_max_mb` | 单次上传请求总大小限制 |
| `storage.upload_max_file_mb` | 单个文件大小限制 |
| `storage.upload_max_files` | 单次请求最多文件数量 |
| `storage.allowed_extensions` / `storage.blocked_extensions` | 上传扩展名白名单与黑名单 |
| `storage.dirs` | 开放目录列表及上传/下载权限 |
| `tokens.default_ttl_seconds` | 临时令牌默认有效期 |
| `tokens.upload_max_mb` | 单个上传令牌累计上传容量，`0` 表示不限制 |
| `audit.retain` | 审计日志保留条数 |

## 部署方式

### 方式一：前后端分容器

在 `backend/` 中准备 `config.yaml` 后运行：

```bash
cd backend
cp config.example.yaml config.yaml
# 必须修改 auth.totp_secret、auth.admin，并确认 storage.dirs 指向需要开放的目录
docker compose -f docker-compose.example.yml up -d --build
```

浏览器默认访问：

```text
http://服务器地址:17878
```

前端 nginx 容器会代理 `/api` 和 `/t` 到后端容器。此方式要求 `backend/` 和 `frontend/` 保持同级目录；后端容器使用非 root 用户运行，请确保挂载的 `data/` 和上传目录对容器用户可写。

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

1. 在反向代理层传递 `X-Forwarded-For` 或 `X-Real-IP`；
2. 将后端配置 `server.trust_proxy_headers` 设置为 `true`；
3. 不要在直连公网时启用该项，避免客户端伪造 IP。

登录失败限速和审计日志都会使用后端解析出的客户端 IP。

### Cookie 与跨站请求防护

- HTTPS 生产部署时应将 `auth.cookie_secure` 设置为 `true`，确保浏览器只通过安全连接发送会话 Cookie。
- `cors.allow_origins` 必须精确列出前端来源；后端会对 `/api` 下非空 `Origin` 的状态变更请求做白名单校验。
- 如果修改前端开发端口或生产访问域名，需要同步更新 `cors.allow_origins`，否则登录、上传、令牌管理等 Cookie 凭据请求会被拒绝。

### 空闲会话与长下载部署建议

推荐保留较短空闲时间和较长下载票据时间：

```yaml
auth:
  session_ttl_seconds: 86400
  idle_timeout_seconds: 180
  idle_grace_seconds: 30

downloads:
  lease_ttl_seconds: 7200
  lease_max_ttl_seconds: 21600
  content_hash_max_mb: 64
```

- `session_ttl_seconds` 是登录态绝对最长有效期。
- `idle_timeout_seconds` 是页面无活动后的空闲过期时间；前端只在页面可见且用户有操作时发送心跳续期。
- `downloads.lease_ttl_seconds` 是下载票据有效期。用户点击下载时先兑换票据，文件传输和 HTTP Range 断点续传使用票据地址，不依赖页面会话继续在线。
- `downloads.content_hash_max_mb` 控制下载票据内容哈希阈值。默认对 64 MiB 及以下文件记录 SHA-256 并在票据下载前复核；设为 `0` 可对所有文件启用内容哈希，但大文件创建票据和每次 Range 续传前都会读取完整文件。
- 公开下载令牌在兑换票据时消耗一次 `uses`，同一票据的 Range 续传不会重复扣次数。
- 兼容保留的 `/t/:token/download` 只显示确认页，用户主动点击后才兑换票据，避免邮件扫描、聊天软件预览等自动 GET 请求提前消耗一次性令牌。

因此，用户离开页面后会较快变成未登录，但已经开始的大文件下载和同一票据有效期内的断点续传不会被会话空闲过期打断。若下载票据过期或文件被替换，需要回到页面重新获取下载链接。

### 上传安全策略

`backend/config.example.yaml` 中提供以下上传限制：

- `storage.upload_max_mb`：单次上传请求总量；
- `storage.upload_max_file_mb`：单个文件大小；
- `storage.upload_max_files`：单次请求文件数量；
- `storage.allowed_extensions`：扩展名白名单，空数组表示不限制；
- `storage.blocked_extensions`：扩展名黑名单，优先级高于白名单；
- `tokens.upload_max_mb`：单个上传令牌累计上传容量，`0` 表示不限制。

## 安全设计

- TOTP Secret 只保存在后端配置中，不会发送到前端。
- 普通 TOTP 用户和管理员权限分离；令牌管理与审计日志仅管理员可访问。
- 会话使用 HttpOnly Cookie，支持过期、退出和过期记录清理。
- 服务端数据库只保存会话 ID 哈希，不保存可直接复用的 Cookie 明文值。
- `/api` 下的状态变更请求会校验非空 `Origin` 是否在 `cors.allow_origins` 中，降低 Cookie 凭据接口的跨站请求风险。
- 临时令牌明文只在创建时返回一次，数据库中只保存 SHA-256 哈希。
- 文件路径拒绝绝对路径、NUL 字符和任何 `..` 段，并做符号链接逃逸防护。
- 登录下载和公开下载都会显式检查目标存在且不是目录。
- 公开令牌信息接口只返回有效令牌的有限元数据；过期、撤销、耗尽、上传容量耗尽的令牌只返回失效原因。
- 上传采用原子创建避免并发同名覆盖，并受数量、大小、扩展名和令牌累计容量限制。
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
