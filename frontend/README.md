# 临时文件传输台前端

这是 `file-trans` 的前端项目，使用 Vue 3、Vite、TypeScript 实现，依赖管理与脚本运行均统一到 [Bun](https://bun.sh/)。当前项目由仓库根目录统一进行 Git 管理。

## 功能页面

- **登录页**：支持两种模式。
  - 普通 TOTP 登录：调用 `/api/auth/login`，进入受限的文件浏览视图。
  - 管理员账号密码登录：调用 `/api/auth/admin-login`，前端在 `authState.role` 中标记 `admin`，并显示令牌管理、访问记录、配置管理等管理入口。
- **文件浏览页**：调用 `/api/dirs` 与 `/api/files/list`，支持目录/单文件资源切换、面包屑、返回上级、权限提示、下载，并提供“上传到此处”入口。管理员还可以从文件行直接跳到令牌页创建下载分享，或从当前目录创建上传分享。上传区域已从浏览页拆出，目录工具栏下方会直接显示文件列表。
- **文件上传页**：调用 `/api/dirs`、`/api/upload-policy`、`/api/files/upload-lease` 与上传票据地址，只展示允许上传的目录资源，支持目录和目标路径选择、拖拽区域、文件队列、上传进度、上传速度、预计剩余时间、随时取消、失败重试以及上传前大小/扩展名提示。上传开始前会签发短期上传票据；新后端会返回 `rawUploadUrl`，前端默认直接把 `File` 作为原始字节流发送，便于管理员页实时看到后端进度并取消。页面登录状态过期不会中断已授权的当前上传。
- **令牌管理页（管理员）**：调用 `/api/tokens` 和 `/api/share-origins`，支持创建下载/上传令牌、设置资源/路径/有效期/最大使用次数。目录资源的下载令牌路径必须指向已存在的具体文件，单文件资源无需填写相对路径；上传令牌只能选择允许上传的目录资源。生成后会显示 `/share/{token}` 分享路径、当前访问地址、显式公开地址、后端枚举到的本机网卡地址和令牌本身，管理员可自行选择复制哪一个地址；历史令牌列表不会再次显示明文链接。列表中的“撤销”用于立即让链接失效；仍可用令牌显示“删除并失效”，已失效令牌显示“删除记录”，避免两种操作含义混淆。
- **公开分享页 `/share/:token`**（无需登录）：调用 `/t/:token/info` 后，根据令牌类型展示
  - 下载令牌：漂亮的下载页与“立即下载”按钮，先调用 `/t/:token/download-lease` 兑换短期下载票据，再跳转票据地址。
  - 上传令牌：拖拽 / 多选 / 上传队列，先兑换公开上传票据，再把文件原始字节流提交到 `rawUploadUrl`；旧后端缺少公开上传票据接口时才回退 `/t/:token/upload` multipart 兼容路径。
- **访问记录页（管理员）**：调用 `/api/audit/logs?page=&pageSize=`，优先展示 `actionLabel`，支持按关键字（动作 / 路径 / 目录 / IP）模糊搜索、按状态筛选和上一页 / 下一页分页。
- **正在传输页（管理员）**：调用 `/api/transfers/active` 和 `/api/transfers/:id/cancel`，展示活跃上传和下载。原始字节流上传显示后端观测进度、速度并可取消；下载保持极速发送路径，只显示最佳努力状态。
- **配置管理页（管理员）**：调用 `/api/config`、`/api/config/resources`、`/api/config/upload-policy` 和 `/api/config/file-picker/*`，可视化新增、编辑、删除目录资源和单文件资源；路径输入旁提供服务端文件选择器，只能从后端 `file_picker.roots` 明确允许的位置选择目录和单文件，列表默认目录优先并支持排序；上传扩展名白名单和黑名单也可在页面中维护。页面只展示安全配置视图，不显示 TOTP Secret 或管理员密码摘要。

前端在登录态页面会监听点击、键盘、滚动、触摸和页面重新可见等事件，只在用户活跃时调用 `/api/auth/heartbeat` 刷新空闲会话；页面隐藏或离开后不会持续保活。文件下载不再直接使用长期会话 URL，而是先兑换下载票据，因此页面会话随后空闲过期也不会中断已授权的长下载或断点续传。

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

该脚本会读取 `backend/config.yaml`，以显式 `-dev -dev-frontend-port <前端端口>` 启动后端，并启动 Vite 前端开发服务器。
如果后端配置写错，脚本会直接显示中文启动失败原因和日志尾部；常见的 YAML 缩进问题可按提示检查 `backend/config.yaml`。
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

因此开发时后端服务需要运行在本机 `17878` 端口。这里默认使用 IPv4 地址，避免部分 Windows/Node 环境把 `localhost` 解析成 `::1` 后连接失败；代理转发时会把 `Origin` 改写为后端同源，避免通过局域网 IP 访问 Vite 时被后端 CSRF Origin 防护误拒绝。

如果后端端口不是 `17878`，通过环境变量修改代理目标：

```bash
VITE_BACKEND_ORIGIN=http://127.0.0.1:9000 bun run dev
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
docker run --rm -p 8081:80 file-trans-frontend
```

镜像先在 Bun 1.3.14 Debian/Trixie 阶段使用 `bun install --frozen-lockfile` 按锁文件安装依赖，再由真实 Node 22 Trixie 阶段运行完整的 `vue-tsc` 类型检查与 Vite 生产构建，以确保完整的 Vue SFC 类型检查；最终使用 `nginx:1.27-alpine` 托管静态资源，并将 `/api/` 与 `/t/` 反向代理到 `http://backend:17878`。nginx 已将上传请求体上限设为 `10g`，关闭上传代理缓冲，并把请求体、响应发送及代理读写的连续无 I/O 进展空闲超时设为 3600 秒；这些都不是从请求开始计算的总传输时长。内置 access log 只记录脱敏 URI，`/t/<token>` 会显示为 `/t/[redacted]`，查询参数、Referer 和 Authorization 不会写入日志；外部 LB、CDN、WAF 或其他代理仍须单独配置相同的 capability 脱敏策略。如果后端上传上限调得更高，需要同步调整 `nginx.conf`。在 Docker Compose 中建议将后端服务命名为 `backend`；如服务名不同，请修改 `nginx.conf` 中的 `proxy_pass`。镜像使用 `STOPSIGNAL SIGQUIT` 让 nginx 优雅退出，Compose 的 24 小时 grace 是外部强杀上限，不是代理或应用的总传输时长；nginx 配置不设置有限的 `worker_shutdown_timeout`。

## 接口约定

- 前端对常见字段做了兼容：目录权限兼容 `canUpload/allowUpload`、`canDownload/allowDownload`；文件列表兼容 `entries/files`；文件类型兼容 `isDir`、`type: "dir"`、`type: "directory"`。
- `/api/auth/me` 支持返回 `role`，前端据此决定是否展示管理入口。`/api/auth/admin-login` 接收 `{ username, password }`。
- `/api/auth/heartbeat` 用于刷新空闲会话，前端只在非公开路由、已登录、页面可见且用户有活动时调用。
- `/api/files/download-lease` 与 `/t/:token/download-lease` 会返回短期下载票据 URL；前端下载按钮跳转该 URL，让浏览器和下载器可以使用 HTTP Range 续传。
- `/api/config` 返回管理员安全配置视图；`/api/config/resources` 支持目录和单文件资源的新增、修改、删除，成功后后端会写回配置并热更新。
- `TokenInfo` 支持可选的 `valid`、`reason`、`actionLabel`、`dirName`、`infoUrl`、`uploadedBytes`、`uploadMaxBytes`、`uploadMaxFileBytes`、`uploadRequestMaxBytes` 字段，前端用它们渲染状态文案和上传前限制提示；上传令牌达到累计容量上限时会显示友好的失效原因。
- `/api/audit/logs` 兼容旧的 `?limit=` 查询，也支持 `?page=&pageSize=` 返回分页对象；访问记录页使用分页方式浏览历史记录。
- 接口返回非 2xx 时会统一读取 JSON 中的 `message` 或 `error`，401 会自动跳转登录页（公开页除外）。

## 可用脚本

- `bun run dev`：启动开发服务器（Vite，监听 `0.0.0.0`）。
- `bun run typecheck`：TypeScript / Vue 类型检查（`vue-tsc -b`）。
- `bun run build`：类型检查并生产构建。
- `bun run preview`：预览生产构建。
