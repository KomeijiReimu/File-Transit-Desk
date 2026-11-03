# 文件传输台 · File Transit Desk

通过浏览器访问服务端指定的文件和目录，支持上传、下载、临时分享、在线交流及用户与资源管理。

## 功能

- 普通用户使用账号密码或 TOTP 临时码登录。
- 支持自助注册、管理员审批，以及用户启停、密码重置和删除。
- 目录浏览、拖拽上传、队列重试、HTTP Range 下载续传。
- 临时上传和下载链接，可限制有效期、使用次数及上传容量。
- 管理员维护共享位置、上传规则，查看传输和访问记录。
- 全局纯文本聊天室，支持撤回及管理员删除。

后端使用 Go、Fiber 和 SQLite；前端使用 Vue 3、TypeScript、Vite 和 Bun。

## 本地运行

环境：Go、Bun、Python 3；Unix 一键启动还需要 curl，Windows 一键启动需要 PowerShell 7。版本要求见 `backend/go.mod` 和 `frontend/package.json`。

### 1. 准备配置

```bash
cp backend/config.example.yaml backend/config.yaml
cp backend/settings.example.yaml backend/settings.yaml
```

`config.yaml` 只保存管理员身份：

```yaml
admin:
  username: admin
  password_hash: "填写生成的 Argon2id 摘要"
```

从仓库根目录生成管理员密码摘要：

```bash
python3 scripts/hash-admin-password.py --format bootstrap
```

工具隐藏输入并要求确认。将输出填入 `backend/config.yaml`。自动化调用通过受保护的标准输入提供密码，不要将密码写进命令行参数。

`settings.yaml` 保存服务、共享资源、注册策略和传输限制。新模板默认使用账号登录，管理员登录后可维护共享位置和常用策略。需要临时码登录时，先生成密钥填写 `auth.totp_secret`，再启用 `accounts.totp_enabled`；已有配置的临时码设置保持不变：

```bash
python3 scripts/generate-totp-secret.py
```

默认单文件、单次上传请求及每个上传分享的累计额度均为 100 GiB（102400 MiB）；已有显式限制保持不变。

### 2. 启动

Linux / macOS：

```bash
./scripts/dev.sh
```

Windows PowerShell：

```powershell
pwsh -File scripts/dev.ps1
```

也可以从其他工作目录通过脚本的完整路径调用。首次缺少配置时，脚本生成模板并退出；填写管理员凭据后再次运行，其他设置可按需调整。

默认入口：

| 服务 | 地址 |
| --- | --- |
| 前端 | `http://localhost:5173` |
| 后端 | `http://localhost:17878` |

脚本会安装缺失的前端依赖、构建并启动后端，再启动 Vite。启动失败会返回非零状态并显示日志位置。

### 修改开发端口

后端监听端口在 `settings.yaml` 的 `server.port` 中设置；脚本参数同时指定前端代理和传输端口：

```bash
BACKEND_PORT=9000 FRONTEND_PORT=5174 ./scripts/dev.sh
```

```powershell
pwsh -File scripts/dev.ps1 -BackendPort 9000 -FrontendPort 5174
```

相对 `BACKEND_CONFIG` 路径以仓库根目录为基准：

```bash
BACKEND_CONFIG=backend/config.local.yaml ./scripts/dev.sh
```

新格式配置内的相对路径以管理员配置文件所在目录为基准。配套的 `settings.yaml` 放在同一目录。

### 分别启动

```bash
cd backend
go run ./cmd/server -config config.yaml -dev -dev-frontend-port 5173
```

```bash
cd frontend
bun install --frozen-lockfile
bun run dev
```

开发模式通过显式 `-dev` 启用。Vite 代理 `/api` 和 `/t`；一键脚本默认让大文件传输按页面主机名直连后端端口。固定传输地址可通过 `FRONTEND_TRANSFER_ORIGIN` 设置，配置错误会直接显示连接失败。

## 账号与共享位置

注册默认需要管理员批准；管理员可在配置页调整注册和临时码登录策略。普通账号使用同一组已开放资源，管理员拥有用户、资源、分享、传输与审计管理权限。

管理员文件选择器默认浏览服务进程可访问的位置，Windows 支持盘符和输入 UNC 路径。使用 `file_picker.mode: restricted` 时，浏览和选择仅限 `file_picker.roots`。普通用户和公开分享始终受已发布资源的根目录与权限限制。

服务账户必须对目标路径具有相应读写权限。Windows 服务看不到的映射盘、未授权的网络共享，以及未挂载进容器的宿主机目录，不能通过网页访问。

禁用、删除账号或修改其密码会撤销登录态及后续传输授权；已通过准入的传输允许完成，新的下载和 Range 请求需要重新授权。临时码是独立的访客入口，关闭账号不会撤销共享临时码。

## 配置与升级

| 文件 | 内容 |
| --- | --- |
| `config.yaml` | 管理员用户名、密码摘要 |
| `settings.yaml` | 服务与数据库路径、共享资源、认证策略、上传下载规则等 |

新版本使用程序默认值补充缺省字段，保留明确配置的零值和布尔值。在线设置只更新所属字段，不将管理员凭据混入服务设置。

### 迁移旧单文件配置

旧格式仍可读取；在线修改前先停止服务并迁移：

```bash
cd backend
go run ./cmd/server -config config.yaml -migrate-config
```

迁移保留旧文件备份，验证服务设置后再发布新的管理员文件。已有 `settings.yaml` 冲突时会停止并保留现场；重复迁移不会覆盖用户修改。只有旧 SHA-256 摘要的配置，需先在旧文件的 `auth.admin.password_hash` 填入有效 Argon2id 摘要。

升级前保存一致的 SQLite 备份及两份配置。回滚需要恢复匹配的程序、数据库和配置；运行中产生的新数据不在升级前备份内。

完整配置项见 [`backend/settings.example.yaml`](backend/settings.example.yaml)。

## 部署

### Docker Compose

```bash
cd backend
mkdir -p config
cp config.example.yaml config/config.yaml
cp settings.example.yaml config/settings.yaml
```

填写管理员凭据，并在 `config/settings.yaml` 中使用容器内路径：数据库为 `/app/data/filetrans.db`，默认上传目录为 `/app/uploads`。

```bash
docker compose -f docker-compose.example.yml up -d --build
```

默认访问 `http://服务器地址:17878`。前端 nginx 代理后端，上传请求上限为 100 GiB，并关闭请求体缓冲。

配置目录整体挂载到 `/app/config`，以支持原子保存。容器以非 root 用户运行，配置、数据库及上传目录需具有相应权限。新增宿主机共享目录时，将该目录挂载进后端容器后再选择容器内路径。

### 后端托管前端

```bash
cd frontend
bun install --frozen-lockfile
bun run build
cd ../backend
go run ./cmd/server -config config.yaml
```

在 `settings.yaml` 中将 `web.static_dir` 指向 `../frontend/dist`，后端会托管前端并处理 SPA 路由回退。

### HTTPS 与反向代理

- HTTPS 部署启用 `auth.cookie_secure`。
- 跨域来源通过 `cors.allow_origins` 精确配置。
- 采用代理地址头时，启用 `server.trust_proxy_headers`，并在 `trusted_proxy_cidrs` 填写实际代理地址范围，优先单个 `/32` 或 `/128`。
- 入口代理覆盖来自客户端的转发头，后端端口限制为仅可信代理可访问。
- 上传代理关闭请求体缓冲；读写超时按持续无数据进展的空闲时间设置。
- `/t/` 链接和下载票据属于访问凭据，外部代理、CDN 和 WAF 日志应脱敏。内置 nginx 已脱敏分享路径并省略查询参数。

## 长时间传输

页面登录会话与传输票据分离。默认会话绝对期限为 24 小时，真实活动空闲期限为 2 小时；后台轮询和上传进度不会延长会话。页面正常过期不打断已授权传输，下载票据有效期内可继续 Range 续传。

公开下载链接在兑换下载票据时消耗一次次数，后续 Range 不重复扣次。下载保留快速发送路径；管理页面展示活跃连接，不将未知下载进度显示为精确百分比。管理员可取消上传；下载不提供可靠的取消能力。

`/api/health/live` 为存活检查，`/api/health/ready` 为就绪检查。停止信号先停止接收新业务，再等待已准入传输。Compose 的 `stop_grace_period: 24h` 是容器最终强杀上限，需覆盖部署允许的最长传输窗口。

## 开发验证

```bash
cd backend
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/server
```

```bash
cd frontend
bun run test
bun run typecheck
bun run build
```

前端测试使用 Vitest。后端接口与配置细节见 [`backend/README.md`](backend/README.md)。

## 许可证

[AGPL-3.0-or-later](LICENSE)。通过网络提供修改后的服务时，须按许可证要求向用户提供对应源码。
