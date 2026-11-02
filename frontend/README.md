# 文件传输台前端

使用 Vue 3、Vite、TypeScript 实现，依赖管理与脚本运行使用 [Bun](https://bun.sh/)。

## 功能页面

- **登录页**：支持普通账号、TOTP 临时码和管理员登录，并提供注册入口。
  - 普通账号调用 `/api/auth/account-login`；注册调用 `/api/auth/register`，按策略等待审批或直接启用。
  - 普通 TOTP 登录：调用 `/api/auth/login`，进入受限的文件浏览视图。
  - 管理员账号密码登录：调用 `/api/auth/admin-login`，前端在 `authState.role` 中标记 `admin`，并显示令牌管理、访问记录、配置管理等管理入口。
- **用户管理 `/users`**：管理员查询、创建、审批、启停、重置密码和删除普通账号。
- **我的账户 `/account`**：账号信息与修改密码；改密成功后退出当前会话。
- **文件浏览页**：调用 `/api/dirs` 与 `/api/files/list`，支持共享位置切换、面包屑、返回上级、权限提示和下载，也可进入当前目录的上传页。管理员可从文件行创建下载分享，或从当前目录创建上传分享。
- **文件上传页**：调用 `/api/dirs`、`/api/upload-policy`、`/api/files/upload-lease` 与上传票据地址，只展示允许上传的目录资源，支持目录和目标路径选择、拖拽区域、文件队列、上传进度、上传速度、预计剩余时间、随时取消、失败重试以及上传前大小/扩展名提示。上传开始前会签发短期上传票据；新后端会返回 `rawUploadUrl`，前端默认直接把 `File` 作为原始字节流发送，便于管理员页实时看到后端进度并取消。页面登录状态过期不会中断已授权的当前上传。
- **全局聊天室 `/chat`**：普通账号、TOTP 访客和管理员登录后均可使用，公开分享访客不可使用。消息是纯文本，不渲染 Markdown/HTML，也不支持附件。所有登录用户可见来源 IP；普通用户可在配置的时限内撤回自己的消息，默认 5 分钟。普通响应不含撤回原文及内部归属字段；管理员可查看撤回原文、单删、批删 1–100 条及确认清空。
- **令牌管理页（管理员）**：调用 `/api/tokens` 和 `/api/share-origins`，支持创建下载/上传令牌、设置资源/路径/有效期/最大使用次数。目录资源的下载令牌路径必须指向已存在的具体文件，单文件资源无需填写相对路径；上传令牌只能选择允许上传的目录资源。生成后会显示 `/share/{token}` 分享路径、当前访问地址、显式公开地址、后端枚举到的本机网卡地址和令牌本身，管理员可自行选择复制哪一个地址；历史令牌列表不会再次显示明文链接。列表中的“撤销”用于立即让链接失效；仍可用令牌显示“删除并失效”，已失效令牌显示“删除记录”，避免两种操作含义混淆。
- **公开分享页 `/share/:token`**（无需登录）：调用 `/t/:token/info` 后，根据令牌类型展示
  - 下载分享：调用 `/t/:token/download-lease` 兑换短期下载授权，再开始下载。
  - 上传令牌：拖拽 / 多选 / 上传队列，先兑换公开上传票据，再把文件原始字节流提交到 `rawUploadUrl`；旧后端缺少公开上传票据接口时才回退 `/t/:token/upload` multipart 兼容路径。
- **访问记录页（管理员）**：调用 `/api/audit/logs?page=&pageSize=`，优先展示 `actionLabel`，支持按关键字（动作 / 路径 / 目录 / IP）模糊搜索、按状态筛选和上一页 / 下一页分页。
- **正在传输页（管理员）**：调用 `/api/transfers/active` 和 `/api/transfers/:id/cancel`，展示活跃上传和下载。原始字节流上传显示后端观测进度、速度并可取消；下载保留快速发送路径，仅显示活跃连接，不能在页面取消。
- **配置管理页（管理员）**：管理共享位置、上传规则、注册审批和临时码登录策略。文件选择器默认支持服务进程可访问的系统目录、Windows 盘符和 UNC；受限模式只允许配置的 roots。绝对地址由后端 `/api/config/file-picker/resolve` 解析。安全配置视图不包含 TOTP 密钥或管理员密码摘要。

会话默认绝对有效期 24 小时，真实活动空闲期限 2 小时，心跳宽限 30 秒。前端只在已登录、页面可见且有真实活动时发送心跳；轮询和上传本身不延长会话。独立传输票据不受普通会话过期影响。已有配置的显式空闲期限保持生效；新格式请在 `settings.yaml` 的 `auth.idle_timeout_seconds` 设置，页面始终使用后端返回的实际期限。

聊天室首版使用短轮询，不使用 WebSocket 或 SSE。页面隐藏时暂停聊天轮询，重新可见后恢复；这不等于后台未读推送，也不承诺页面隐藏期间实时更新徽标。history 返回 `generation` 和 `latestChangeSeq`，changes 携带 generation；generation 改变或接口返回 409 `chat_cursor_reset_required` 时，前端清空本地消息并重新加载 history。mutation 的 `eventSeq` 仅标识本次事件，不能当作全局 cursor，只有 history 的 `latestChangeSeq` 和 changes 的 `nextAfterSeq` 可推进同步。管理员批量删除若返回 `chat_batch_delete_conflict`，界面不会假定部分成功，而会刷新当前消息状态；清空提交当前 generation/high-water，若返回 `chat_clear_conflict` 则要求刷新后重新确认，成功后按新 generation 重建消息流。

聊天默认按 90 天或 50000 条先到者保留，后端每批最多清理 500 条。管理员单删或批删后，前端只展示无正文 tombstone；清空会移除当前消息流并推进同步 generation，但保留后端 SQLite 自增序列。后端会把应用数据库当前消息正文设为 `NULL` 或在 clear 事务中删除聊天表记录并写入无正文审计，但这只表示应用层不可恢复，不代表 SQLite 历史页、WAL、备份或存储快照已完成取证级擦除。

聊天展示的 `sourceIP` 应理解为“后端可信网络边界观察到的客户端地址”，而不是终端设备公网 IP。NAT、VPN、CDN、负载均衡和反向代理都会影响该值；旧 SQLite 消息中已经保存的 `127.0.0.1` 不会由前端或后端反推、改写，新代理配置只影响之后的新消息。

配置管理页的服务端文件选择器是只读辅助控件，不提供删除、重命名、移动、上传或编辑能力。上传扩展名策略编辑器会在前端先做格式校验，并在黑名单清空时使用项目内自定义确认弹窗二次确认。

## 本地开发

推荐从仓库根目录一键启动前后端：

```bash
./scripts/dev.sh
```

Windows PowerShell：

```powershell
pwsh -File scripts/dev.ps1
```

该脚本读取管理员文件 `backend/config.yaml` 和同目录 `settings.yaml`，以显式 `-dev -dev-frontend-port <前端端口>` 启动后端，再启动 Vite。
启动失败时，脚本显示失败阶段和诊断目录。请查看后端日志，并按错误检查管理员配置或 `settings.yaml`。
临时令牌不绑定主机名；令牌页会把当前访问地址、显式公开地址和后端枚举到的网卡地址列为候选项供管理员选择。若需额外固定展示某个局域网或域名地址，可设置 `FRONTEND_PUBLIC_SHARE_ORIGIN` / `VITE_PUBLIC_SHARE_ORIGIN`，例如 `http://192.168.124.9:5173`。不设置时，界面仍会提供 `/share/{token}` 分享路径。
一键开发模式下，大文件传输默认按当前页面主机名直连后端：`localhost:5173` 会使用 `localhost:17878`，`192.168.124.9:5173` 会使用 `192.168.124.9:17878`，从而绕过 Vite 开发代理。只有显式 `-dev` 模式才允许“同一主机名 + 指定开发前端端口”的 CORS/CSRF 例外；生产模式默认没有任何开发端口例外，只接受同源或 `cors.allow_origins` 中的显式来源。若需固定为某个域名、反向代理地址或特殊端口，可设置 `FRONTEND_TRANSFER_ORIGIN` / `VITE_TRANSFER_ORIGIN`，例如 `http://192.168.124.9:17878`。地址写错或不可达时不会自动猜测其他地址，也不会静默回退，前端会直接显示连接失败。

如只运行前端：

```bash
bun install
bun run dev
```

`bun.lock` 已纳入版本控制，CI 与发布场景建议使用：

```bash
bun install --frozen-lockfile
bun run build
```

前端以 Bun 为唯一推荐运行方式；依赖版本在 `package.json` 和 `bun.lock` 中锁定，避免 `latest` 带来的不可复现构建。

Vite 已配置开发代理：

- `/api` → `http://127.0.0.1:17878`
- `/t` → `http://127.0.0.1:17878`

因此开发时后端服务需要运行在本机 `17878` 端口。这里默认使用 IPv4 地址，避免部分 Windows/Node 环境把 `localhost` 解析成 `::1` 后连接失败。每个代理请求都会从 Vite 入站 socket 地址生成规范客户端地址，并无条件覆盖浏览器可能伪造或重复提交的 `X-FileTrans-Dev-Client-IP`、`X-Real-IP` 和 `X-Forwarded-For`；无法取得合法地址时会删除这些头。代理同时把 `Origin` 改写为后端同源。后端仅在显式 `-dev` 且直接 peer 为 loopback 时读取自定义开发头，且该头只影响客户端地址，不参与 forwarded host/proto、Origin 或 CSRF 信任判断。

如果后端端口不是 `17878`，通过环境变量修改代理目标：

```bash
VITE_BACKEND_ORIGIN=http://127.0.0.1:9000 VITE_TRANSFER_PORT=9000 bun run dev
```

如果前端开发端口不是 `5173`，通过 Vite 参数修改：

```bash
bun run dev -- --port 5174
```

使用根目录一键脚本时，对应写法为：

```bash
BACKEND_PORT=9000 FRONTEND_PORT=5174 ./scripts/dev.sh
```

Windows PowerShell：

```powershell
pwsh -File scripts/dev.ps1 -BackendPort 9000 -FrontendPort 5174
```

一键开发脚本会把当前前端端口同时传给前端和后端。`FILE_TRANS_DEV_FRONTEND_PORT` 已废弃且不再生效。手动分别启动并使用 5174 端口时，应在 `backend/` 中运行：

```bash
go run ./cmd/server -config config.yaml -dev -dev-frontend-port 5174
```

前端继续使用 `VITE_FRONTEND_PORT=5174` 表示当前 Vite 端口，并用 `VITE_TRANSFER_PORT=<后端端口>` 生成同主机直连传输地址；只执行 `bun run dev -- --port 5174` 时则会安全地继续走 Vite 代理。生产启动不得传 `-dev`，因此默认没有开发 CORS/CSRF 例外。

## 构建与部署

```bash
bun run build
```

构建产物位于 `dist/`。如果由后端托管静态文件，建议保持前后端目录并列，并将后端 `web.static_dir` 配置为 `../frontend/dist`；如果复制到其他目录，也需要同步修改后端配置中的 `web.static_dir`，并确保前端路由回退到 `index.html`。

## Docker 静态托管

```bash
docker build -t file-trans-frontend .
```

该镜像需要能访问名为 `backend`、端口为 `17878` 的后端上游。使用[根 README 的 Compose 部署](../README.md#部署)启动前后端及所需网络。

镜像先在 Bun 1.3.14 Debian/Trixie 阶段使用 `bun install --frozen-lockfile` 按锁文件安装依赖，再由真实 Node 22 Trixie 阶段运行完整的 `vue-tsc` 类型检查与 Vite 生产构建，以确保完整的 Vue SFC 类型检查；最终使用 `nginx:1.27-alpine` 托管静态资源，并将 `/api/` 与 `/t/` 反向代理到 `http://backend:17878`。nginx 已将上传请求体上限设为 `10g`，关闭上传代理缓冲，并把请求体、响应发送及代理读写的连续无 I/O 进展空闲超时设为 3600 秒；这些都不是从请求开始计算的总传输时长。内置 access log 只记录脱敏 URI，`/t/<token>` 会显示为 `/t/[redacted]`，查询参数、Referer 和 Authorization 不会写入日志；外部 LB、CDN、WAF 或其他代理仍须单独配置相同的 capability 脱敏策略。如果后端上传上限调得更高，需要同步调整 `nginx.conf`。在 Docker Compose 中建议将后端服务命名为 `backend`；如服务名不同，请修改 `nginx.conf` 中的 `proxy_pass`。镜像使用 `STOPSIGNAL SIGQUIT` 让 nginx 优雅退出，Compose 的 24 小时 grace 是外部强杀上限，不是代理或应用的总传输时长；nginx 配置不设置有限的 `worker_shutdown_timeout`。

## 接口约定

- 前端对常见字段做了兼容：目录权限兼容 `canUpload/allowUpload`、`canDownload/allowDownload`；文件列表兼容 `entries/files`；文件类型兼容 `isDir`、`type: "dir"`、`type: "directory"`。
- `/api/auth/me` 支持返回 `role`，前端据此决定是否展示管理入口。`/api/auth/admin-login` 接收 `{ username, password }`。
- 账号接口见 [`backend/README.md`](../backend/README.md)。`userId` 是稳定账号身份，`sessionBinding` 仍是单次会话绑定；账号相同也不能让旧会话请求跨登录生效。
- `/api/auth/heartbeat` 用于刷新空闲会话，前端只在非公开路由、已登录、页面可见且用户有活动时调用。
- `/api/chat/capabilities` 返回当前正文字符/字节限制、固定原始请求体上限 `maxRequestBytes: 8192`、撤回窗口和分页边界。聊天 JSON 的完整原始请求体（含语法、空白与 escape）最多 8192 bytes；解码正文默认仍受 2000 Unicode code points 和 8192 UTF-8 bytes 双重限制。
- 普通聊天使用 `GET /api/chat/messages`、`GET /api/chat/changes?afterSeq=&generation=`、`POST /api/chat/messages` 和 `POST /api/chat/messages/:id/withdraw`；普通 history、changes、create/withdraw mutation 以及 deleted tombstone 都包含 `sourceIP`，但不含撤回原文、`authorKey` 或 `deletedBy`。管理员另用 `/api/admin/chat/messages`、`/api/admin/chat/changes` 与 `DELETE /api/admin/chat/messages/:id`，管理员 DTO 可显示 withdrawn 原文。
- 管理员勾选批量删除调用 `POST /api/admin/chat/messages/batch-delete`，请求 `{ ids: number[] }`，必须是 1–100 个唯一正整数且完整 JSON 请求体不超过 4096 bytes。成功响应的每个 mutation 各有 `message/eventSeq`；409 `chat_batch_delete_conflict` 表示整批零修改，前端应刷新而不是移除部分选中项。
- 管理员清空调用 `POST /api/admin/chat/messages/clear`，请求 `{ confirm: "CLEAR_ALL_MESSAGES", expectedGeneration, expectedLatestChangeSeq }`，使用当前 history/changes 同步状态做 CAS。成功响应 `{ clearedCount, generation, latestChangeSeq }` 后重建本地 feed；409 `chat_clear_conflict` 表示状态已变化，必须刷新并重新确认。该接口不会把后续消息 ID 或 change seq 重置为旧值。
- history 默认 50 条、最大 100；changes 默认 50、最大 500。generation 缺失/非法是 400，generation 不匹配或 cursor 不可信是 409 reset；`eventSeq` 不用于推进 changes cursor。
- 默认发送限速为 session 20/min、可信网络边界解析地址 60/min、单实例 global 300/min。来源地址是否符合部署预期取决于后端 trusted proxy 配置；它会显示给所有已登录用户，但不承诺是公网 IP。
- `/api/files/download-lease` 与 `/t/:token/download-lease` 会返回短期下载票据 URL；前端下载按钮跳转该 URL，让浏览器和下载器可以使用 HTTP Range 续传。
- `/api/config` 返回管理员安全配置视图；`/api/config/resources` 支持目录和单文件资源的新增、修改、删除，成功后后端会写回配置并热更新。
- `TokenInfo` 支持可选的 `valid`、`reason`、`actionLabel`、`dirName`、`infoUrl`、`uploadedBytes`、`uploadMaxBytes`、`uploadMaxFileBytes`、`uploadRequestMaxBytes` 字段，前端用它们渲染状态文案和上传前限制提示；上传令牌达到累计容量上限时会显示友好的失效原因。
- `/api/audit/logs` 兼容旧的 `?limit=` 查询，也支持 `?page=&pageSize=` 返回分页对象；访问记录页使用分页方式浏览历史记录。
- 接口返回非 2xx 时会统一读取 JSON 中的 `message` 或 `error`，401 会自动跳转登录页（公开页除外）。

## 可用脚本

- `bun run dev`：启动开发服务器（Vite，监听 `0.0.0.0`）。
- `bun run typecheck`：TypeScript / Vue 类型检查（`vue-tsc -b`）。
- `bun run test`：运行 Vitest。
- `bun run build`：类型检查并生产构建。
- `bun run preview`：预览生产构建。
