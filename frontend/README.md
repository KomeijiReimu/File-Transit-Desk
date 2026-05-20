# 临时文件传输台前端

这是 `file-trans` 的前端项目，使用 Vue 3、Vite、TypeScript 实现，依赖管理与脚本运行均统一到 [Bun](https://bun.sh/)。当前项目由仓库根目录统一进行 Git 管理。

## 功能页面

- **登录页**：支持两种模式。
  - 普通 TOTP 登录：调用 `/api/auth/login`，进入受限的文件浏览视图。
  - 管理员账号密码登录：调用 `/api/auth/admin-login`，前端在 `authState.role` 中标记 `admin`，并显示令牌管理、访问记录、配置概览等管理入口。
- **文件浏览页**：调用 `/api/dirs` 与 `/api/files/list`，支持目录切换、面包屑、返回上级、权限提示、下载，并提供“上传到此处”入口。上传区域已从浏览页拆出，目录工具栏下方会直接显示文件列表。
- **文件上传页**：调用 `/api/dirs` 与 `/api/files/upload`，支持目录和目标路径选择、拖拽区域、文件队列、上传状态以及失败后的**重试**按钮。“拖拽文件到此上传”和“或点击按钮选择文件，队列支持失败重试”采用分行间距展示，避免说明文案过于紧凑。
- **令牌管理页（管理员）**：调用 `/api/tokens`，支持创建下载/上传令牌、设置目录/路径/有效期/最大使用次数。下载令牌的路径必须指向已存在的具体文件，后端会提前校验并返回友好错误。生成后的链接统一转换成 `/share/{token}` 形式，并仅在创建成功时提供一键**复制链接**按钮与复制成功提示；历史令牌列表不会再次显示明文链接。列表中的“撤销”用于立即让链接失效；仍可用令牌显示“删除并失效”，已失效令牌显示“删除记录”，避免两种操作含义混淆。
- **公开分享页 `/share/:token`**（无需登录）：调用 `/t/:token/info` 后，根据令牌类型展示
  - 下载令牌：漂亮的下载页与“立即下载”按钮，先调用 `/t/:token/download-lease` 兑换短期下载票据，再跳转票据地址。
  - 上传令牌：拖拽 / 多选 / 上传队列，提交到 `/t/:token/upload`，每个文件有状态显示。
- **访问记录页（管理员）**：调用 `/api/audit/logs`，优先展示 `actionLabel`，支持按关键字（动作 / 路径 / 目录 / IP）模糊搜索、按状态（全部 / 成功 / 失败 / 拒绝）筛选、以及“加载更多”（自动递增 `limit`）。
- **配置 / 目录概览页（管理员）**：展示后端开放目录、根路径与上传/下载权限。

前端在登录态页面会监听点击、键盘、滚动、触摸和页面重新可见等事件，只在用户活跃时调用 `/api/auth/heartbeat` 刷新空闲会话；页面隐藏或离开后不会持续保活。文件下载不再直接使用长期会话 URL，而是先兑换下载票据，因此页面会话随后空闲过期也不会中断已授权的长下载或断点续传。

## 本地开发

推荐从仓库根目录一键启动前后端：

```bash
./scripts/dev.sh
```

Windows PowerShell：

```powershell
pwsh -File scripts/dev.ps1
```

该脚本会读取 `backend/config.yaml` 启动后端，并启动 Vite 前端开发服务器。

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

- `/api` → `http://localhost:8080`
- `/t` → `http://localhost:8080`

因此开发时后端服务需要运行在本机 `8080` 端口。

如果后端端口不是 `8080`，通过环境变量修改代理目标：

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

前端端口变化后，需要同步更新后端 `cors.allow_origins`，例如加入 `http://localhost:5174`。

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

镜像基于 `oven/bun:1-alpine` 完成依赖安装与生产构建，再用 `nginx:1.27-alpine` 托管静态资源，并将 `/api/` 与 `/t/` 反向代理到 `http://backend:8080`。nginx 已将上传请求体上限设为 `1g`，避免默认 1MiB 限制影响文件上传。在 Docker Compose 中建议将后端服务命名为 `backend`；如服务名不同，请修改 `nginx.conf` 中的 `proxy_pass`。

## 接口约定

- 前端对常见字段做了兼容：目录权限兼容 `canUpload/allowUpload`、`canDownload/allowDownload`；文件列表兼容 `entries/files`；文件类型兼容 `isDir`、`type: "dir"`、`type: "directory"`。
- `/api/auth/me` 支持返回 `role`，前端据此决定是否展示管理入口。`/api/auth/admin-login` 接收 `{ username, password }`。
- `/api/auth/heartbeat` 用于刷新空闲会话，前端只在非公开路由、已登录、页面可见且用户有活动时调用。
- `/api/files/download-lease` 与 `/t/:token/download-lease` 会返回短期下载票据 URL；前端下载按钮跳转该 URL，让浏览器和下载器可以使用 HTTP Range 续传。
- `TokenInfo` 支持可选的 `valid`、`reason`、`actionLabel`、`dirName`、`infoUrl`、`uploadedBytes`、`uploadMaxBytes` 字段，前端用它们渲染状态文案；上传令牌达到累计容量上限时会显示友好的失效原因。
- `/api/audit/logs` 支持 `?limit=` 查询参数，访问记录页会逐步增大 `limit` 来实现“加载更多”。
- 接口返回非 2xx 时会统一读取 JSON 中的 `message` 或 `error`，401 会自动跳转登录页（公开页除外）。

## 可用脚本

- `bun run dev`：启动开发服务器（Vite，监听 `0.0.0.0`）。
- `bun run typecheck`：TypeScript / Vue 类型检查（`vue-tsc -b`）。
- `bun run build`：类型检查并生产构建。
- `bun run preview`：预览生产构建。
