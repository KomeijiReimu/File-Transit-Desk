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

脚本默认读取 `backend/config.yaml`，后端端口仍以该配置文件中的 `server.port` 为准，并显式传入 `-dev` 与 `-dev-frontend-port`。直接运行后端默认为生产模式，固定验证码及开发 CORS/CSRF 例外均不会启用。
如果后端启动失败，一键脚本会显示中文错误、后端日志尾部和日志文件路径。若看到 YAML 格式错误，请优先检查提示行附近的缩进；`file_picker` 是顶层配置，应该与 `storage` 同级，不能写到 `storage.shares` 下面。

如只运行后端：

```bash
cp config.example.yaml config.yaml
# 修改 config.yaml 中的 auth.totp_secret、auth.admin、storage.dirs/storage.shares 等配置
go run ./cmd/server -config config.yaml
```

仓库提交了 `go.sum` 用于依赖校验；如调整依赖，请重新执行 `go mod tidy`。

## 安全默认值

- `config.example.yaml` 默认使用不可启动的占位 TOTP secret 和管理员账号占位符，必须替换后才能启动。
- `auth.dev_allow_fixed_code` 默认是 `false`。只有显式设置为 `true` 且 `auth.totp_secret` 为空时，才允许开发验证码 `000000`。
- 启动时会校验 `auth.totp_secret` 是否为有效且长度足够的 Base32 Secret，并拒绝占位符、空密钥和危险 CORS 通配符。
- 登录接口带有内存级失败限速：同一来源短时间内多次失败会被临时拒绝，降低 TOTP 在线猜测风险。
- 普通 TOTP 登录用户角色为 `user`，只能浏览目录/文件、上传下载、退出和查看自身登录状态；临时令牌管理与审计日志只允许 `admin` 角色访问。
- 管理员通过独立接口登录，优先使用 Argon2id PHC；旧 SHA-256 配置仅保留迁移兼容。用户名使用固定长度常量时间比较，错误用户名也会执行密码验证。
- Cookie 使用 `HttpOnly`、`SameSite=Lax`、`Path=/`，`Secure` 由 `auth.cookie_secure` 控制；HTTPS 部署时应启用。服务端数据库只保存会话 ID 哈希，避免数据库只读泄露时直接复用 Cookie 原值。
- 登录会话同时受绝对有效期和空闲有效期约束。前端只在用户活跃时调用心跳接口刷新空闲时间，页面隐藏或离开后不会持续保活。
- 文件下载使用短期下载票据。票据绑定具体目录、路径、文件大小、修改时间，并会按 `downloads.content_hash_max_mb` 对文件内容写入 SHA-256 哈希；页面会话空闲过期后，已兑换票据的长下载和 HTTP Range 续传仍可继续，但文件被替换后旧票据会失效。
- 管理员撤销或删除公开下载令牌时，会同步清理该令牌已兑换但尚未过期的下载票据，用于应急止血。共享资源路径、类型、权限变更或删除时，会同步撤销相关公开令牌并清理对应目录 ID 的下载票据和上传票据，避免旧授权指向新资源。
- 对 `/api` 下会改变状态的请求，后端会校验非空 `Origin`，降低 Cookie 凭据接口的跨站请求风险。一键开发模式会额外允许同一主机名的前端开发端口来源，用于默认直连传输；生产或不同域名跨域访问仍必须写入 `cors.allow_origins`。
- 路径会拒绝绝对路径、NUL、任何 `..` 段；已存在目标会通过 `filepath.EvalSymlinks` 校验真实路径仍在配置目录内；上传创建目录前会校验最近存在父目录没有通过符号链接逃逸。
- 临时令牌数据库只保存 SHA-256 哈希，明文只在创建响应中返回一次。
- 上传默认不覆盖同名文件，会使用原子创建方式自动追加 `-1`、`-2` 等后缀，避免并发同名上传互相覆盖；登录态和公开分享前端默认走原始字节流上传，后端从连接开始直接读取请求体、写临时文件并更新传输进度。旧的 `multipart/form-data` 上传接口仍兼容保留，但不再是前端默认路径；上传还会校验单次文件数量、单文件大小、请求总量、扩展名白/黑名单，以及上传令牌累计容量。
- 登录态可先创建短期上传票据，再通过不依赖 Cookie 的 `upload-raw-by-lease` 传输。票据只保存哈希，绑定用户、目录、路径、文件名、文件大小、资源授权指纹、过期时间且一次性使用；即使页面会话随后空闲过期或被删除，已创建且未使用的票据仍可完成本次上传。资源路径、类型或权限变化后，旧票据会因指纹不匹配而失效，不能写入同 ID 的新资源路径。上传票据是单次 bearer 授权，泄露即等同本次上传权限，因此前端通过 `Authorization: Bearer` 发送，不放入分享链接或页面 URL。所有 `/t/...` capability 响应以及 token/lease 创建与使用响应都会设置 `no-store`、`no-referrer` 和 `noindex` 等安全头。内置 nginx 会脱敏 `/t/<token>` 并忽略查询参数，但部署在它之前的外部 LB、CDN、WAF 和代理日志仍需独立禁止记录 capability、Referer 与 Authorization。
- 服务维护内存传输注册表，管理员可查看活跃上传和下载。原始字节流上传的进度、速度与取消是精确能力；下载继续使用 Fiber `c.Download` 极速路径，只做 best-effort 登记，不承诺精确速度或可靠取消。若部署在反向代理后，应关闭上传请求体缓冲，例如 Nginx `proxy_request_buffering off;`，否则代理缓冲会让后端观测滞后。
- 上传临时文件写在目标目录内，文件名形如 `.upload-*.tmp`；成功提交最终文件后会立即删除临时文件，失败、超限、客户端断开或管理员取消也会尽量删除。服务启动和定时任务会按配置清理超过保留期且不在活跃注册表中的崩溃残留临时文件。

## 配置说明

见 `config.example.yaml`：

- `server.host/port`：监听地址。
- `server.trust_proxy_headers`：是否启用可信代理头；直接运行必须保持 `false`。
- `server.trusted_proxy_cidrs`：可信代理 socket 来源 CIDR。启用后必填、最多 64 项；只剥离链右侧可信代理，非法链回退 `X-Real-IP`，再失败回退 socket remote IP。
- `server.keepalive_idle_timeout_seconds`：HTTP keep-alive 请求之间的空闲超时，默认 120 秒。安全配置摘要只读展示，修改后需要重启；它不是上传、下载或单个请求的总时长，服务端不会设置固定 ReadTimeout/WriteTimeout。
- `database.path`：SQLite 文件路径，启动自动建表 `sessions`、`tokens`、`download_leases`、`upload_leases`、`audit_logs` 并创建索引。
- `auth.totp_secret`：TOTP Base32 secret。
- `auth.dev_allow_fixed_code`：本地开发固定码开关，默认关闭。
- `auth.session_ttl_seconds`：会话绝对最长有效期。
- `auth.idle_timeout_seconds`：空闲过期时间，默认 1800 秒；前端不活跃或页面隐藏超过该时间后，后续依赖 Cookie 的 API 会返回 401。已兑换的下载票据和已创建的上传票据不依赖 Cookie。
- `auth.idle_grace_seconds`：心跳恢复宽限期；普通业务请求不会使用宽限期，只有 `/api/auth/heartbeat` 可在短暂超时后恢复会话。
- `auth.upload_lease_ttl_seconds`：登录态上传票据有效期，默认 1800 秒，必须小于等于 `auth.session_ttl_seconds`。票据只允许使用一次，过期或用过后会被清理。
- `auth.cookie_secure`：HTTPS 下启用安全 Cookie。
- `downloads.lease_ttl_seconds`：下载票据默认有效期，点击下载或公开分享下载时兑换。
- `downloads.lease_max_ttl_seconds`：下载票据最大有效期上限，防止误配置过长。
- `downloads.content_hash_max_mb`：下载票据内容哈希阈值。小于等于该大小的文件在创建票据时记录 SHA-256；`0` 表示所有文件都记录内容哈希。
- `downloads.max_concurrent_hashes`：全局并发内容哈希上限，默认 2，范围 1–16。满载时立即返回 503，不占用下载传输连接等待。
- `downloads.verify_hash_on_every_request`：默认 `false`，有内容哈希的 lease 仅在首次使用时复核一次，后续 Range 只复核普通文件身份、大小和 mtime，以避免每段续传都读取完整文件。若服务器目录可能被不可信本地写者修改，可设为 `true`，让每次请求都复核内容哈希。

上述两个下载哈希配置修改后需要重启。首次使用状态持久化在数据库中；并发首次 Range 会合并为一次实际哈希和一次首次使用审计。超过 `content_hash_max_mb` 而未记录内容哈希的大文件仍会在首次成功复核后标记为已使用。
- `auth.admin.username`：管理员用户名。
- `auth.admin.password_hash`：管理员密码的 Argon2id PHC。
- `auth.admin.password_sha256`：deprecated 旧 SHA-256 摘要；仅在 `password_hash` 为空时使用。
- `abuse.login.max_concurrent_admin_verifications`：Argon2id 验证并发槽，默认 2；`0` 表示不限制。
- `abuse.login.global_per_minute` 与 IP 失败窗口参数：分别限制 user/admin 单实例登录总速率，并按可信客户端 IP 隔离失败封禁。
- `abuse.creation.*`：限制 token/lease 创建速率、活跃 token 数量和未使用/未过期 lease 数量；所有字段设为 `0` 可单独关闭对应限制。
- `audit.unauthorized_sample_seconds` / `unauthorized_global_per_minute`：仅对认证噪声中的未授权和非管理员 forbidden 按规范客户端 IP 与路由模板采样，默认 60 秒一次并按 action 分别限制为 120 次/分钟；`0` 表示不限制审计写入，而不是关闭审计。CSRF 使用独立 action 全局桶。路径 allowlist、资源策略、登录、配置、token 和 capability 等关键事件始终完整审计。
- `audit.prune_every_writes`：审计 INSERT 累计到该次数后批量按主键阈值清理，默认 100；`0` 表示只在启动或维护任务中显式清理。若自动 prune 失败，触发它的审计事件已经写入，但调用会返回错误并输出不含敏感 detail 的高优先级诊断。
- `abuse.uploads.*`：单实例上传并发准入，分别限制全局、资源、session 和公开 token；达到上限立即拒绝，不等待，也不影响下载。
- `web.static_dir`：前端构建产物目录，存在时自动托管并回退到 `index.html`；不会吞掉 `/api` 与 `/t` 路由。
- `cors.allow_origins`：生产仅允许同源或显式列出的来源，空列表不会自动允许 localhost；配置中禁止使用 `*`。只有通过 `-dev` 显式启动时，才额外允许同一主机名的 `-dev-frontend-port` 来源。
- `storage.upload_max_mb`：单次上传请求总大小限制，同时作为 Fiber 请求体上限；示例默认 5120 MB，可覆盖常见 2G 文件上传。
- `storage.upload_max_file_mb`：单个文件大小限制，必须小于等于 `storage.upload_max_mb`；示例默认 5120 MB。
- `storage.min_free_mb` / `storage.min_free_percent`：上传开始前的磁盘保留阈值，两者取较大值；默认 1024 MB / 5%，单项 `0` 表示关闭该阈值。该检查是准入时的磁盘空间快照，不是长期空间 reservation 账本。
- `storage.upload_max_files`：单次 multipart 请求最多允许的文件数量。
- `storage.upload_temp_retention_seconds`：`.upload-*.tmp` 临时文件保留时间，默认 86400 秒。超过该时间且不在活跃上传注册表中的临时文件会被清理。
- `storage.upload_temp_cleanup_max_entries` / `upload_temp_cleanup_max_duration_seconds`：单次后台清理最多扫描的原始目录项和执行秒数，默认 50000 项/5 秒，修改后需重启。启动与资源配置发布只触发后台任务，不同步等待目录扫描；达到预算会记录 `truncated=true`，审计和固定日志不包含真实路径。
- `storage.upload_temp_cleanup_interval_seconds`：临时文件定时清理间隔，默认 3600 秒；启动时会异步触发一次相同的有界清理。
- `storage.directory_list_scan_limit` / `directory_list_max_page_size`：普通目录单请求扫描窗口与显式分页上限，默认 5000/200。超过扫描窗口时返回 `truncated=true`，客户端不会自动拉取目录剩余部分。
- `file_picker.max_scan_entries` / `max_page_size`：管理员 picker 的扫描窗口与分页上限，默认 5000/200。

上述列表边界属于启动配置，修改后需重启。客户端提交的合法正 `pageSize` 超过配置上限时，服务端会安全夹紧，并在响应 `pageSize` 中返回实际值；页 offset 始终按该实际值计算。列表响应通过 `hasMore` 表示当前已扫描集合还有下一页，通过 `truncated` 表示目录在扫描窗口之外仍可能存在条目；`truncated=true` 并不意味着客户端应自动扫描全目录。`scannedEntries` 表示实际纳入处理的原始扫描窗口，始终不超过 `scanLimit`；被过滤的 `.upload-*.tmp` 同样消耗该窗口预算，第 N+1 项仅用于判断是否截断，不计入 `scannedEntries`。
- `storage.allowed_extensions` / `storage.blocked_extensions`：上传扩展名白名单与黑名单；白名单非空时只允许列出的扩展名，黑名单优先拒绝。默认黑名单为空，可由管理员配置管理页按需维护。
- `storage.dirs`：开放目录资源，含 `type: directory`、`allow_download/allow_upload`，可由管理员“配置管理”页维护。
- `storage.shares`：单文件共享资源，含 `type: file`，只允许下载；也可由管理员“配置管理”页维护。
- `file_picker.roots`：新增或修改资源路径的唯一允许范围；未配置时不会自动提供系统入口。选择器仅辅助填写共享资源路径，不提供删除、重命名、移动、上传或编辑能力。
- `file_picker.max_page_size` / `file_picker.deny_names` / `file_picker.deny_patterns`：文件选择器分页上限和可选隐藏规则。
- `tokens.default_ttl_seconds`：令牌默认有效期。
- `tokens.max_ttl_seconds`：令牌最长有效期，管理员传入更长的 `expiresAt` 或 `ttlSeconds` 会被夹紧到该上限。
- `tokens.upload_max_mb`：单个上传令牌的累计上传容量；示例默认 5120 MB，`0` 表示不限制。
- `audit.retain`：审计日志保留条数。

### 修改端口

后端监听地址由 `config.yaml` 控制：

```yaml
server:
  host: "0.0.0.0"
  port: 17878
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
cd ..
python3 scripts/generate-totp-secret.py
```

脚本默认生成 20 字节随机数并输出无填充 Base32，可直接写入 `auth.totp_secret`。也可以使用任意 TOTP 工具生成后写入配置，并导入手机验证器。

## 生成管理员密码摘要

运行脚本后按提示隐藏输入管理员密码。默认 YAML 同时输出 Argon2id PHC 和旧二进制短期回滚所需的 SHA-256，两个摘要来自同一次输入：

```bash
cd ..
python3 scripts/hash-admin-password.py
```

脚本内部以 `shell=False` 调用 `go run ./cmd/hash-admin-password`。自动化环境只能从 Secret Manager 或受保护的标准输入提供密码，不能把密码放入命令行、脚本或日志。`--format phc` 仅输出 PHC，不提供旧二进制回滚字段；`--format legacy-sha256` 仅供单独生成旧格式。迁移时可暂时同时保留 PHC 和旧 SHA-256，但 PHC 非空时绝不降级；回滚必须显式移除 `password_hash` 并重启，稳定后应删除旧值。

## API 契约摘要

所有错误统一返回 JSON：`{"error":"..."}`；需要稳定机器判断时会额外返回 `code`。

### 认证

- `POST /api/auth/login`：普通用户 TOTP 登录，JSON `{ "code": "123456" }`，成功返回 `{ "authenticated": true, "role": "user", "expiresAt": "...", "idleExpiresAt": "..." }` 并写入 Cookie。
- `POST /api/auth/admin-login`：管理员账号登录，JSON `{ "username": "admin", "password": "..." }`，成功后角色为 `admin`。
- 登录失败过多时返回 `429 Too Many Requests`。
- Argon2id 管理员验证并发槽已满时返回 `503`、`code=auth_capacity_exhausted` 和 `Retry-After: 1`，不会排队占用请求。
- `GET /api/auth/me`：返回 `{ "authenticated": true, "role": "user", "expiresAt": "...", "idleExpiresAt": "..." }` 或管理员信息。
- `POST /api/auth/heartbeat`：登录后由前端在页面可见且用户活跃时调用，刷新 `idleExpiresAt`；空闲过期后返回 401。
- `POST /api/auth/logout`：清理服务端会话和 Cookie，返回 `{ "ok": true }`。

### 目录与文件

- `GET /api/dirs`：返回共享资源数组，普通用户字段为 `id/name/type/allowDownload/allowUpload/canDownload/canUpload`；管理员响应额外包含 `root` 便于配置管理展示。普通用户不会收到服务端真实路径。
- `GET /api/upload-policy`：返回登录态上传页需要的大小和扩展名限制，前端会在真正传输前提示明显的限制错误，避免大文件传到一半才失败。
- `GET /api/share-origins?currentOrigin=http://localhost:5173`：仅管理员可访问，返回后端枚举到的本机网卡候选地址，令牌页用它生成可选分享链接；它不会改变令牌授权，也不会自动决定应该复制哪个 IP。
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

  如果 `path` 不存在会返回 `404` 和中文提示，例如 `{"error":"路径不存在，请检查路径或返回上级目录。"}`；如果路径包含绝对路径或 `..` 等非法片段，会返回 `400` 和 `路径不合法` 提示。

  对 `type: file` 的单文件资源，`GET /api/files/list?dirId=manual` 会返回一个文件条目；下载票据可使用空路径或返回条目的 `path`。

- `POST /api/files/download-lease`：登录态下载前兑换短期票据，请求 `{ "dirId": "default", "path": "a.txt" }`，返回 `{ "url": "/api/files/download-by-lease?lease=...", "expiresAt": "..." }`。
- `GET /api/files/download-by-lease?lease=...`：使用下载票据下载文件，支持 HTTP Range 断点续传；不要求页面会话仍然有效，但会校验票据未过期、目录仍允许下载、文件大小、修改时间和可用内容哈希未变化。
- `GET /api/files/download?dirId=default&path=a.txt`：兼容保留的直接下载接口，仍要求请求开始时有有效会话。
- `POST /api/files/upload-lease`：登录态创建上传票据，请求 `{ "dirId": "default", "path": "subdir", "fileName": "a.bin", "fileSize": 123 }`，返回 `{ "lease": "...", "uploadUrl": "/api/files/upload-by-lease", "rawUploadUrl": "/api/files/upload-raw-by-lease", "expiresAt": "..." }`。票据一次性使用且不依赖 Cookie，适合直连后端或长时间上传。前端或直连客户端应通过 `Authorization: Bearer <lease>` 发送票据，不要把票据放入 URL 查询参数。
- `POST /api/files/upload-raw-by-lease`：推荐路径。使用上传票据提交原始文件字节流，必须带 `Authorization: Bearer <lease>`，请求体就是文件内容，`Content-Length` 必须等于创建票据时的 `fileSize`，允许 `0` 字节文件。后端使用票据绑定的目录、路径、文件名、大小和资源授权指纹，不信任 Cookie、查询参数或请求体里的目标字段；同名文件仍不覆盖。该入口会从连接开始登记传输记录，管理员页可实时看到进度并取消。
- `POST /api/files/upload-by-lease`：兼容保留路径。仅接受登录态上传票据，使用 `multipart/form-data` 文件，必须带 `Authorization: Bearer <lease>`；新前端默认不再使用它。公开上传票据不能调用该接口，公开 raw 上传必须走 `/t/upload-raw-by-lease`，公开旧客户端继续使用 `/t/:token/upload`。
- `POST /api/files/upload`：兼容保留的登录态 `multipart/form-data` 上传接口，字段 `dirId`、`path`、`file` 或 `files`，兼容多文件。推荐把 `dirId/path` 放在查询参数中；如果放在表单字段中，必须出现在文件字段之前。该接口会流式落盘并执行上传数量、单文件大小、请求总量、扩展名策略校验。返回：

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

下载令牌的 `path` 必须指向已存在的具体普通文件，不能指向不存在的路径、目录、FIFO、socket 或设备；符号链接解析后的目标也必须是普通文件。若下载路径不存在或不可安全共享，会返回不包含系统文件类型和真实路径的稳定错误。

也兼容 `dir_id`、`ttl_seconds`、`expires_at`、`max_uses`。响应中的明文 token 只出现一次：

```json
{ "id": 1, "token": "...", "url": "/t/.../download", "infoUrl": "/t/.../info" }
```

上传令牌的 `url` 为 `/t/{token}/upload`。

- `POST /api/tokens/:id/revoke`：撤销令牌，让尚未过期且未用尽的链接立即失效，并同步清理该令牌已兑换的下载票据。用于应急止血或提前结束分享。
- `DELETE /api/tokens/:id`：删除令牌记录。删除也会让令牌失效并清理相关下载票据，但主要语义是移除管理列表中的历史记录。
- `GET /t/:token/info`：公开令牌信息。无效时返回 `{ "valid": false, "reason": "expired|revoked|exhausted|upload_quota_exhausted|resource_unavailable|permission_disabled|not_found" }`；有效时返回 `{ "valid": true, "type": "download", "path": "a.txt", "expiresAt": "...", "maxUses": 1, "uses": 0, "uploadedBytes": 0, "uploadMaxBytes": 5368709120, "uploadMaxFileBytes": 5368709120, "uploadRequestMaxBytes": 5368709120 }`，不会暴露目录 ID。
- `POST /t/:token/download-lease`：公开下载令牌兑换短期下载票据，兑换时原子消耗一次使用次数。
- `GET /t/download-by-lease?lease=...`：公开票据下载，支持 Range 续传且不重复消耗令牌次数。
- `GET /t/:token/download`：兼容保留，会显示确认下载页；用户主动点击后才 `POST` 兑换票据，避免链接预览或安全扫描提前消耗一次性下载次数。
- `GET /t/:token/upload`：后端兼容保留的简易上传页；推荐对外分享前端 `/share/:token` 页面。
- `POST /t/:token/upload-lease`：公开上传令牌兑换短期上传票据，请求 `{ "fileName": "a.bin", "fileSize": 123 }`，返回 `{ "lease": "...", "uploadUrl": "/t/{token}/upload", "rawUploadUrl": "/t/upload-raw-by-lease", "expiresAt": "..." }`。创建票据时不会最终消耗公开令牌次数和容量；真正开始 raw 上传时才预占。
- `POST /t/upload-raw-by-lease`：推荐路径。使用公开上传票据提交原始文件字节流，必须带 `Authorization: Bearer <lease>`，`Content-Length` 必须等于票据文件大小。成功上传会按实际落盘大小校正公开令牌累计容量；失败、取消、断线或超限会清理临时/已保存文件并回滚预占。
- `POST /t/:token/upload`：兼容保留的 `multipart/form-data` 上传接口，字段 `file` 或 `files`。旧客户端可继续使用；新前端默认先兑换公开上传票据并走 raw 上传。

令牌使用次数与上传累计容量通过 SQLite 条件更新原子预占，避免并发绕过 `maxUses` 或累计容量限制。下载令牌会先确认目标文件存在后再预占次数并创建下载票据；后续同一票据的普通下载或 Range 续传不会重复增加 `uses`。上传令牌在保存失败时会释放预占次数和预占容量。

### 审计与健康检查

- `GET /api/audit/logs?page=1&pageSize=50`：仅管理员可访问，分页返回 `{ logs, page, pageSize, total, totalPages }`，`pageSize` 最大 200。旧的 `?limit=100` 数组响应仍兼容保留。
- `GET /api/transfers/active`：仅管理员可访问，返回 `{ "transfers": [...] }`。上传记录包含 `id/type/status/source/dirId/path/fileName/totalBytes/transferredBytes/currentSpeedBps/averageSpeedBps/startedAt/updatedAt/clientIP/cancelable`；公开分享上传的 `source` 为 `public_token`。下载记录带 `bestEffort: true`，速度字段可能为空或不精确。
- `POST /api/transfers/:id/cancel`：仅管理员可访问。上传会触发取消、打断请求读取、停止写入并清理临时文件；下载保持极速发送路径，不保证可靠取消，通常返回 409 表示不可可靠取消或任务已结束。
- `GET /api/health/live`：仅表示进程仍可响应，不查询数据库；draining 期间仍返回 200。
- `GET /api/health/ready`：检查初始化、draining 状态和最多 2 秒的数据库 Ping；不可接收新流量时返回 503。
- `GET /api/health`：保留为 readiness 兼容别名。

### 配置管理

以下 `/api/config` 接口仅管理员可访问，返回和接收的都是安全配置视图，不包含 TOTP Secret、管理员密码摘要、数据库路径等敏感字段。

- `GET /api/config`：返回共享资源、上传策略、令牌策略和下载票据策略摘要。
- `PUT /api/config/upload-policy`：修改 `storage.allowed_extensions` 与 `storage.blocked_extensions`；后端会统一小写化、补前导点、去重，拒绝 `*`、重叠项和异常字符。
- `GET /api/config/file-picker/roots`：返回文件选择器入口。未配置 `file_picker.roots` 时，Linux/macOS 返回系统根目录，Windows 返回可用盘符。
- `GET /api/config/file-picker/list?rootId=uploads&path=/docs&page=1&pageSize=100&sort=name&order=asc`：列出目录；路径使用 `/` 分隔的相对路径并分页返回，默认目录优先，再按名称排序；`sort` 支持 `name/type/size/modifiedAt`。
- `POST /api/config/file-picker/validate`：最终选择前校验文件或目录，返回可填入现有资源表单的规范化绝对路径。
- `POST /api/config/resources`：新增目录或单文件资源，请求示例：`{ "id": "manual", "name": "说明文档", "type": "file", "path": "/data/manual.pdf", "allowDownload": true, "allowUpload": false }`。
- `PUT /api/config/resources/:id`：修改已有资源。资源 ID 作为配置边界，修改时不允许在表单中变更 ID。
- `DELETE /api/config/resources/:id`：删除资源。已有令牌不会被删除，但后续会因资源不存在而不可继续使用。

保存资源时后端会校验 ID 字符集、路径是否存在、目录/文件类型是否匹配、读写权限以及危险系统目录；即使路径来自文件选择器，保存资源时也会再次校验。成功后原子写回 `config.yaml`，保留 `config.yaml.bak`，并热更新内存中的资源列表。目录资源写入 `storage.dirs`，单文件资源写入 `storage.shares`。在线保存会重新序列化 YAML，原配置文件注释和手工排版不会保留。

`file_picker.roots` 只是快捷入口，不是唯一选择范围。容器部署时，选择器看到的是容器内路径；若要选择宿主机目录，必须先以卷挂载方式暴露给容器。

下载带宽限制建议在 Nginx、Caddy、Traefik 等反向代理层统一配置，应用层保持对 HTTP Range 和长连接下载的稳定支持。

## Docker

完整且唯一的部署步骤、Compose 命令、端口与挂载权限说明见[仓库根 README 的“部署方式”](../README.md#部署方式)。

示例 Compose 的前后端固定加入 `172.28.0.0/24`。使用内置 Nginx 时，后端配置必须启用 `trust_proxy_headers` 并将该 CIDR 写入 `trusted_proxy_cidrs`；若 subnet 冲突，必须同时修改 Compose subnet 和可信 CIDR。直接运行后端时保持 `false` 和空列表。

后端镜像不会包含本地配置、数据库、上传文件或构建产物；最终以非 root 用户 `filetrans` 运行，并通过 `wget` 调用 `/api/health/ready` 做 readiness 检查。部署时必须将宿主机 `backend/config/` 目录挂载到 `/app/config/`，使 `backend/config/config.yaml` 对应容器内 `/app/config/config.yaml`，不能只挂载单个配置文件。

首次 SIGTERM/SIGINT 会先将 readiness 置为 503，再无限等待活跃上传、`c.Download`、Range 和其他请求自然完成，然后停止 maintenance 并关闭数据库；不会撤销 lease、删除 session 或主动取消传输。Compose 示例的 `stop_grace_period: 24h` 是容器运行时最终强杀上限，不是应用传输 timeout；可能超过 24 小时的部署必须提高该值。Kubernetes 应相应设置足够大的 `terminationGracePeriodSeconds`，并使用 readiness endpoint 摘除流量。keep-alive IdleTimeout 只帮助关闭请求间空闲连接，ReadTimeout/WriteTimeout 仍为 0。

生产入口 `cmd/server` 使用 `server.Runtime` 统一管理 readiness、maintenance、Fiber 和数据库关闭。旧的 `server.New*` 构造器仅为已有测试和嵌入兼容保留；新的生产集成应使用 `NewRuntimeWithOptions` 并调用 `Runtime.Shutdown`。

上传和下载可以持续数小时。反向代理的传输超时应表示“连续无数据进展的空闲超时”，而不是从请求开始计算的固定总时长；不要用固定总传输超时中断仍在持续传送数据的连接。

## 前端 dist 放置

前端构建后，将产物放在配置的 `web.static_dir`（例如 `../frontend/dist`）。后端会托管静态文件，并对非 `/api`、非 `/t` 路径回退到 `index.html`，适配单页应用路由。

## 清除数据库和运行记录

后端会在启动时自动迁移 SQLite 表结构，因此可以通过删除或清理数据库来重置运行记录。执行前请先停止后端服务，并根据需要备份数据库和上传目录。

- **清空全部数据库记录**：删除 `database.path` 指向的 SQLite 文件后重启服务，默认配置示例：

  ```bash
  cd backend
  rm -f data/filetrans.db
  go run ./cmd/server -config config.yaml
  ```

- **只清空访问记录**：保留会话、令牌和下载票据，只删除审计日志：

  ```bash
  cd backend
  sqlite3 data/filetrans.db "DELETE FROM audit_logs; VACUUM;"
  ```

- **让所有登录态、上传票据和分享链接立即失效**：保留审计日志，清除会话、上传票据、下载票据和令牌：

  ```bash
  cd backend
  sqlite3 data/filetrans.db "DELETE FROM sessions; DELETE FROM upload_leases; DELETE FROM download_leases; DELETE FROM tokens; VACUUM;"
  ```

- **清除上传文件**：上传目录以 `storage.dirs[].path` 为准；默认示例目录可这样清理：

  ```bash
  rm -rf backend/uploads/*
  ```

如果修改过 `database.path` 或开放目录路径，请以实际配置为准，不要直接套用默认路径。

## 测试

```bash
go test ./...
go vet ./...
go build ./cmd/server
```
