# 临时文件传输项目

一个自用的轻量级临时文件访问与传输工具，用于在外网、局域网、临时设备或他人电脑上安全地浏览、下载、上传指定目录中的文件。当前根目录是唯一 Git 仓库，`backend/` 与 `frontend/` 不再分别维护子仓库。

## 功能概览

- 普通用户使用 TOTP 动态验证码登录，只能浏览、上传、下载已开放目录。
- 管理员使用独立账号密码登录，可管理临时令牌、查看访问记录和配置概览。
- 支持受控目录浏览、子目录进入、文件大小和修改时间展示。
- 支持登录用户拖拽上传、队列上传、失败重试和下载文件。
- 支持创建临时下载/上传令牌，并按有效期、使用次数和上传累计容量自动失效。
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
├── README.md         # 项目总说明
└── 部署说明.md       # 简要部署说明
```

运行数据、本地配置、依赖目录和构建产物不纳入 Git：`backend/config.yaml`、`backend/data/`、`backend/uploads/`、`frontend/node_modules/`、`frontend/dist/` 等均由根 `.gitignore` 排除。

## 快速开始

### 1. 后端

```bash
cd backend
cp config.example.yaml config.yaml
```

至少修改：

```yaml
auth:
  totp_secret: "你的 Base32 TOTP Secret"
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
```

生成 TOTP Secret：

```bash
python3 - <<'PY'
import base64, os
print(base64.b32encode(os.urandom(20)).decode().rstrip('='))
PY
```

生成管理员密码摘要：

```bash
printf '%s' 'your-password' | sha256sum | awk '{print $1}'
```

启动后端：

```bash
go mod tidy
go run ./cmd/server -config config.yaml
```

### 2. 前端

```bash
cd frontend
bun install
bun run dev
```

Vite 会把 `/api` 和 `/t` 代理到 `http://localhost:8080`。打开 Vite 输出的地址后，普通用户使用 TOTP 登录；管理员切换到“管理员账号”模式登录。

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

### 前后端分容器

```bash
cd backend
cp config.example.yaml config.yaml
# 修改 auth.totp_secret、auth.admin、storage.dirs 等配置
docker compose -f docker-compose.example.yml up -d --build
```

前端 nginx 容器会代理 `/api` 和 `/t` 到后端容器。

### 后端托管前端静态文件

```bash
cd frontend
bun install --frozen-lockfile
bun run build

cd ../backend
cp config.example.yaml config.yaml
# 确保 web.static_dir 指向 ../frontend/dist
go run ./cmd/server -config config.yaml
```

## 安全设计

- TOTP Secret 只保存在后端配置中，不会发送到前端。
- 普通 TOTP 用户和管理员权限分离；令牌管理与审计日志仅管理员可访问。
- 会话使用 HttpOnly Cookie，支持过期、退出和过期记录清理。
- 临时令牌明文只在创建时返回一次，数据库中只保存 SHA-256 哈希。
- 文件路径拒绝绝对路径、NUL 字符和任何 `..` 段，并做符号链接逃逸防护。
- 登录下载和公开下载都会显式检查目标存在且不是目录。
- 公开令牌信息接口只返回有效令牌的有限元数据；过期、撤销、耗尽、上传容量耗尽的令牌只返回失效原因。
- 上传采用原子创建避免并发同名覆盖，并受数量、大小、扩展名和令牌累计容量限制。
- 审计和登录限速默认使用连接 IP；部署在可信反向代理后可启用 `server.trust_proxy_headers` 获取真实客户端 IP。

## 常用命令

后端：

```bash
cd backend
go test ./...
go vet ./...
go build ./cmd/server
go run ./cmd/server -config config.yaml
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
