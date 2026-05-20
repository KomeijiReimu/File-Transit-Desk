# File Trans Backend

自用临时文件传输后端，基于 Go、Fiber、SQLite。功能包括 TOTP 登录、HttpOnly Cookie 会话、受控目录浏览/上传/下载、临时令牌、审计日志、静态前端托管。

## 快速运行

从仓库根目录一键启动前后端：

```bash
./scripts/dev.sh
```

Windows PowerShell：

```powershell
pwsh -File scripts/dev.ps1
```

脚本默认读取 `backend/config.yaml`，后端端口仍以该配置文件中的 `server.port` 为准。

如只运行后端：

```bash
cp config.example.yaml config.yaml
# 修改 config.yaml 中的 auth.totp_secret、auth.admin、storage.dirs 等配置
go mod tidy
go run ./cmd/server -config config.yaml
```

仓库提交了 `go.sum` 用于依赖校验；如调整依赖，请重新执行 `go mod tidy`。

## 安全默认值

- `config.example.yaml` 默认使用不可启动的占位 TOTP secret 和管理员账号占位符，必须替换后才能启动。
- `auth.dev_allow_fixed_code` 默认是 `false`。只有显式设置为 `true` 且 `auth.totp_secret` 为空时，才允许开发验证码 `000000`。
- 启动时会校验 `auth.totp_secret` 是否为有效且长度足够的 Base32 Secret，并拒绝占位符、空密钥和危险 CORS 通配符。
- 登录接口带有内存级失败限速：同一来源短时间内多次失败会被临时拒绝，降低 TOTP 在线猜测风险。
- 普通 TOTP 登录用户角色为 `user`，只能浏览目录/文件、上传下载、退出和查看自身登录状态；临时令牌管理与审计日志只允许 `admin` 角色访问。
- 管理员通过独立接口登录，配置中只保存管理员密码的 SHA-256 十六进制摘要，后端使用常量时间比较校验。
- Cookie 使用 `HttpOnly`、`SameSite=Lax`、`Path=/`，`Secure` 由 `auth.cookie_secure` 控制；HTTPS 部署时应启用。服务端数据库只保存会话 ID 哈希，避免数据库只读泄露时直接复用 Cookie 原值。
- 登录会话同时受绝对有效期和空闲有效期约束。前端只在用户活跃时调用心跳接口刷新空闲时间，页面隐藏或离开后不会持续保活。
- 文件下载使用短期下载票据。票据绑定具体目录、路径、文件大小和修改时间；页面会话空闲过期后，已兑换票据的长下载和 HTTP Range 续传仍可继续，但文件被替换后旧票据会失效。
- 管理员撤销或删除公开下载令牌时，会同步清理该令牌已兑换但尚未过期的下载票据，用于应急止血。
- 对 `/api` 下会改变状态的请求，后端会校验非空 `Origin` 必须出现在 `cors.allow_origins` 中，降低 Cookie 凭据接口的跨站请求风险。
- 路径会拒绝绝对路径、NUL、任何 `..` 段；已存在目标会通过 `filepath.EvalSymlinks` 校验真实路径仍在配置目录内；上传创建目录前会校验最近存在父目录没有通过符号链接逃逸。
- 临时令牌数据库只保存 SHA-256 哈希，明文只在创建响应中返回一次。
- 上传默认不覆盖同名文件，会使用原子创建方式自动追加 `-1`、`-2` 等后缀，避免并发同名上传互相覆盖；上传还会校验单次文件数量、单文件大小、请求总量、扩展名白/黑名单，以及上传令牌累计容量。

## 配置说明

见 `config.example.yaml`：

- `server.host/port`：监听地址。
- `server.trust_proxy_headers`：是否信任反向代理头。为 `true` 时审计日志和登录限速优先使用 `X-Forwarded-For` 第一段，其次 `X-Real-IP`；未部署在可信代理后时必须保持 `false`。
- `database.path`：SQLite 文件路径，启动自动建表 `sessions`、`tokens`、`download_leases`、`audit_logs` 并创建索引。
- `auth.totp_secret`：TOTP Base32 secret。
- `auth.dev_allow_fixed_code`：本地开发固定码开关，默认关闭。
- `auth.session_ttl_seconds`：会话绝对最长有效期。
- `auth.idle_timeout_seconds`：空闲过期时间；前端不活跃或页面隐藏超过该时间后，后续 API 会返回 401。
- `auth.idle_grace_seconds`：空闲策略宽限配置，用于部署策略说明和后续兼容。
- `auth.cookie_secure`：HTTPS 下启用安全 Cookie。
- `downloads.lease_ttl_seconds`：下载票据默认有效期，点击下载或公开分享下载时兑换。
- `downloads.lease_max_ttl_seconds`：下载票据最大有效期上限，防止误配置过长。
- `auth.admin.username`：管理员用户名。
- `auth.admin.password_sha256`：管理员密码的 SHA-256 十六进制摘要。
- `web.static_dir`：前端构建产物目录，存在时自动托管并回退到 `index.html`；不会吞掉 `/api` 与 `/t` 路由。
- `cors.allow_origins`：允许来源，本地默认 `http://localhost:5173`；由于接口使用 Cookie 凭据，配置中禁止使用 `*`。
- `storage.upload_max_mb`：单次上传请求总大小限制，同时作为 Fiber 请求体上限。
- `storage.upload_max_file_mb`：单个文件大小限制，必须小于等于 `storage.upload_max_mb`。
- `storage.upload_max_files`：单次 multipart 请求最多允许的文件数量。
- `storage.allowed_extensions` / `storage.blocked_extensions`：上传扩展名白名单与黑名单；白名单非空时只允许列出的扩展名，黑名单优先拒绝危险类型。
- `storage.dirs`：开放目录，含 `allow_download/allow_upload`。
- `tokens.default_ttl_seconds`：令牌默认有效期。
- `tokens.upload_max_mb`：单个上传令牌的累计上传容量；`0` 表示不限制。
- `audit.retain`：审计日志保留条数。

### 修改端口

后端监听地址由 `config.yaml` 控制：

```yaml
server:
  host: "0.0.0.0"
  port: 8080
```

把 `port` 改成目标端口后重启服务即可。如果同时使用前端开发服务器，需要让前端代理指向相同端口，例如：

```bash
cd ../frontend
VITE_BACKEND_ORIGIN=http://127.0.0.1:9000 bun run dev
```

Windows 一键脚本可直接指定端口：

```powershell
pwsh -File scripts/dev.ps1 -BackendPort 9000
```

## 生成 TOTP Secret

推荐使用 Base32 secret，例如：

```bash
python3 - <<'PY'
import base64, os
print(base64.b32encode(os.urandom(20)).decode().rstrip('='))
PY
```

也可以使用任意 TOTP 工具生成后写入 `auth.totp_secret`，并导入手机验证器。

## 生成管理员密码摘要

将下面的 `your-password` 替换为管理员密码，输出写入 `auth.admin.password_sha256`：

```bash
printf '%s' 'your-password' | sha256sum | awk '{print $1}'
```

## API 契约摘要

所有错误统一返回 JSON：`{"error":"..."}`。

### 认证

- `POST /api/auth/login`：普通用户 TOTP 登录，JSON `{ "code": "123456" }`，成功返回 `{ "authenticated": true, "role": "user", "expiresAt": "...", "idleExpiresAt": "..." }` 并写入 Cookie。
- `POST /api/auth/admin-login`：管理员账号登录，JSON `{ "username": "admin", "password": "..." }`，成功后角色为 `admin`。
- 登录失败过多时返回 `429 Too Many Requests`。
- `GET /api/auth/me`：返回 `{ "authenticated": true, "role": "user", "expiresAt": "...", "idleExpiresAt": "..." }` 或管理员信息。
- `POST /api/auth/heartbeat`：登录后由前端在页面可见且用户活跃时调用，刷新 `idleExpiresAt`；空闲过期后返回 401。
- `POST /api/auth/logout`：清理服务端会话和 Cookie，返回 `{ "ok": true }`。

### 目录与文件

- `GET /api/dirs`：返回目录数组，普通用户字段为 `id/name/allowDownload/allowUpload/canDownload/canUpload`；管理员响应额外包含 `root` 便于配置概览展示。普通用户不会收到服务端真实目录路径。
- `GET /api/files/list?dirId=default&path=subdir`：返回：

```json
{
  "dir": "default",
  "path": "subdir",
  "entries": [
    { "name": "a.txt", "isDir": false, "size": 12, "modifiedAt": "2026-05-18T10:00:00Z", "path": "subdir/a.txt" }
  ],
  "canUpload": true,
  "canDownload": true
}
```

- `POST /api/files/download-lease`：登录态下载前兑换短期票据，请求 `{ "dirId": "default", "path": "a.txt" }`，返回 `{ "url": "/api/files/download-by-lease?lease=...", "expiresAt": "..." }`。
- `GET /api/files/download-by-lease?lease=...`：使用下载票据下载文件，支持 HTTP Range 断点续传；不要求页面会话仍然有效，但会校验票据未过期、目录仍允许下载、文件大小和修改时间未变化。
- `GET /api/files/download?dirId=default&path=a.txt`：兼容保留的直接下载接口，仍要求请求开始时有有效会话。
- `POST /api/files/upload`：`multipart/form-data` 字段 `dirId`、`path`、`file` 或 `files`，兼容多文件。该接口会执行上传数量、单文件大小、请求总量、扩展名策略校验。返回：

```json
{ "ok": true, "uploaded": 2, "files": [{ "name": "a-1.txt", "path": "subdir/a-1.txt", "size": 12 }] }
```

### 临时令牌

以下 `/api/tokens` 管理接口仅管理员可访问。

- `GET /api/tokens`：返回数组，字段为 `id/type/dirId/path/expiresAt/maxUses/uses/uploadedBytes/revoked/valid/reason/createdAt`，不会泄露哈希，也不会再次返回明文 token。前端仅在创建响应中显示一次可复制链接。
- `POST /api/tokens`：创建令牌。请求兼容驼峰与旧蛇形字段：

```json
{
  "type": "download",
  "dirId": "default",
  "path": "a.txt",
  "ttlMinutes": 30,
  "maxUses": 1
}
```

也兼容 `dir_id`、`ttl_seconds`、`expires_at`、`max_uses`。响应中的明文 token 只出现一次：

```json
{ "id": 1, "token": "...", "url": "/t/.../download", "infoUrl": "/t/.../info" }
```

上传令牌的 `url` 为 `/t/{token}/upload`。

- `POST /api/tokens/:id/revoke`
- `DELETE /api/tokens/:id`
- `GET /t/:token/info`：公开令牌信息。无效时返回 `{ "valid": false, "reason": "expired|revoked|exhausted|upload_quota_exhausted|not_found" }`；有效时返回 `{ "valid": true, "type": "download", "path": "a.txt", "expiresAt": "...", "maxUses": 1, "uses": 0, "uploadedBytes": 0, "uploadMaxBytes": 1073741824 }`，不会暴露目录 ID。
- `POST /t/:token/download-lease`：公开下载令牌兑换短期下载票据，兑换时原子消耗一次使用次数。
- `GET /t/download-by-lease?lease=...`：公开票据下载，支持 Range 续传且不重复消耗令牌次数。
- `GET /t/:token/download`：兼容保留，会显示确认下载页；用户主动点击后才 `POST` 兑换票据，避免链接预览或安全扫描提前消耗一次性下载次数。
- `GET /t/:token/upload`：后端兼容保留的简易上传页；推荐对外分享前端 `/share/:token` 页面。
- `POST /t/:token/upload`：`multipart/form-data` 字段 `file` 或 `files`，除普通上传策略外，还会按 `tokens.upload_max_mb` 记录并限制该令牌的累计上传容量。

令牌使用次数与上传累计容量通过 SQLite 条件更新原子预占，避免并发绕过 `maxUses` 或累计容量限制。下载令牌会先确认目标文件存在后再预占次数并创建下载票据；后续同一票据的普通下载或 Range 续传不会重复增加 `uses`。上传令牌在保存失败时会释放预占次数和预占容量。

### 审计与健康检查

- `GET /api/audit/logs?limit=100`：仅管理员可访问，`limit` 被限制在 `1..500`，字段为 `id/action/actionLabel/ip/detail/createdAt`。
- `GET /api/health`：返回 `{ "ok": true }`。

## Docker

镜像不会把示例配置复制成生产配置；运行时必须挂载 `/app/config.yaml`。

```bash
docker build -t filetrans-backend .
docker run --rm \
  -p 8080:8080 \
  -v "$PWD/config.yaml:/app/config.yaml:ro" \
  -v "$PWD/data:/app/data" \
  -v "$PWD/uploads:/app/uploads" \
  filetrans-backend
```

镜像内包含 `wget`，Dockerfile 已配置 `HEALTHCHECK` 调用 `/api/health`。
最终镜像使用非 root 用户 `filetrans` 运行。若挂载宿主机目录，请确保容器用户对 `data`、上传目录有读写权限。
Dockerfile 使用受支持的 Go/Alpine 基础镜像，并在依赖下载阶段复制 `go.mod` 与 `go.sum` 进行校验。

如果希望前后端分容器运行，可以先准备 `config.yaml`，再使用仓库内的示例编排文件：

```bash
cp config.example.yaml config.yaml
# 修改 config.yaml 中的 auth.totp_secret、auth.admin、storage.dirs 等配置
docker compose -f docker-compose.example.yml up -d --build
```

该编排文件会构建当前后端目录，并使用相邻的 `../frontend` 目录构建 nginx 前端镜像。浏览器访问 `http://服务器地址:8080`，前端容器会代理 `/api` 和 `/t` 到后端服务。

## 前端 dist 放置

前端构建后，将产物放在配置的 `web.static_dir`（例如 `../frontend/dist`）。后端会托管静态文件，并对非 `/api`、非 `/t` 路径回退到 `index.html`，适配单页应用路由。

## 测试

```bash
go test ./...
go vet ./...
go build ./cmd/server
```
