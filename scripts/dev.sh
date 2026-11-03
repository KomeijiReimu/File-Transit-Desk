#!/usr/bin/env bash
set -euo pipefail
# Each launched job owns a process group, including Bun/Go descendants. Never kill by name/port.
set -m

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BACKEND_CONFIG="${BACKEND_CONFIG:-backend/config.yaml}"
BACKEND_PORT="${BACKEND_PORT:-17878}"
BACKEND_ORIGIN="${BACKEND_ORIGIN:-http://127.0.0.1:${BACKEND_PORT}}"
FRONTEND_HOST="${FRONTEND_HOST:-0.0.0.0}"
FRONTEND_PORT="${FRONTEND_PORT:-5173}"
FRONTEND_PUBLIC_SHARE_ORIGIN="${FRONTEND_PUBLIC_SHARE_ORIGIN:-${VITE_PUBLIC_SHARE_ORIGIN:-}}"
FRONTEND_TRANSFER_ORIGIN="${FRONTEND_TRANSFER_ORIGIN:-${VITE_TRANSFER_ORIGIN:-}}"
INSTALL_TIMEOUT_SECONDS="${INSTALL_TIMEOUT_SECONDS:-300}"
BUILD_TIMEOUT_SECONDS="${BUILD_TIMEOUT_SECONDS:-180}"
BACKEND_READY_TIMEOUT_SECONDS="${BACKEND_READY_TIMEOUT_SECONDS:-30}"
FRONTEND_READY_TIMEOUT_SECONDS="${FRONTEND_READY_TIMEOUT_SECONDS:-30}"
for value in "$INSTALL_TIMEOUT_SECONDS" "$BUILD_TIMEOUT_SECONDS" "$BACKEND_READY_TIMEOUT_SECONDS" "$FRONTEND_READY_TIMEOUT_SECONDS"; do
  if [[ ! "$value" =~ ^[1-9][0-9]*$ ]]; then
    echo '启动阶段超时必须为正整数（秒）。' >&2
    exit 2
  fi
done
if [[ "$BACKEND_CONFIG" = /* ]]; then
  BACKEND_CONFIG_PATH="$BACKEND_CONFIG"
else
  BACKEND_CONFIG_PATH="$ROOT_DIR/$BACKEND_CONFIG"
fi
SETTINGS_PATH="$(dirname "$BACKEND_CONFIG_PATH")/settings.yaml"
if [[ ! -f "$BACKEND_CONFIG_PATH" ]]; then
  mkdir -p "$(dirname "$BACKEND_CONFIG_PATH")"
  cp "$ROOT_DIR/backend/config.example.yaml" "$BACKEND_CONFIG_PATH"
  chmod 600 "$BACKEND_CONFIG_PATH"
  if [[ ! -e "$SETTINGS_PATH" ]]; then
    cp "$ROOT_DIR/backend/settings.example.yaml" "$SETTINGS_PATH"
    chmod 600 "$SETTINGS_PATH"
  fi
  printf '已生成管理员配置：%s\n服务设置（保留已有文件）：%s\n' "$BACKEND_CONFIG_PATH" "$SETTINGS_PATH" >&2
  printf '请设置 admin.username/password_hash，并检查 settings.yaml。生成密码块：\n  python3 %q --format bootstrap\n' "$ROOT_DIR/scripts/hash-admin-password.py" >&2
  printf '动态验证码登录默认开启；请将生成的密钥填入 settings.yaml 的 auth.totp_secret：\n  python3 %q\n' "$ROOT_DIR/scripts/generate-totp-secret.py" >&2
  echo '配置完成后，请使用本次相同的启动参数重新运行。' >&2
  exit 1
fi
for command in go bun curl; do
  if ! command -v "$command" >/dev/null 2>&1; then
    printf '未找到 %s，请先安装。\n' "$command" >&2
    exit 1
  fi
done

# Do not inherit the system /tmp default. Retain diagnostics/build output for inspection.
ARTIFACT_ROOT="${TMPDIR:-$ROOT_DIR/.opencode/dev}"
mkdir -p "$ARTIFACT_ROOT"
ARTIFACT_ROOT="$(cd "$ARTIFACT_ROOT" && pwd)"
RUN_DIR="$(mktemp -d "$ARTIFACT_ROOT/file-trans-dev.XXXXXX")"
export TMPDIR="$RUN_DIR"
echo "诊断及构建目录：$RUN_DIR"
GROUPS_STARTED=()
RUNTIME_READY=false
cleanup() {
  local code=$? pid backend_code=0
  trap - EXIT
  trap '' INT TERM
  if "$RUNTIME_READY" && [[ -n "${BACKEND_PID:-}" ]]; then
    echo '[停止] 等待当前传输完成后退出，无固定等待上限。' >&2
    # Signal only the server, not its descendants, until its own drain completes.
    kill -TERM "$BACKEND_PID" 2>/dev/null || true
    wait "$BACKEND_PID" 2>/dev/null || backend_code=$?
    if (( code == 0 && backend_code != 0 )); then code=$backend_code; fi
  fi
  for pid in "${GROUPS_STARTED[@]}"; do
    kill -TERM -- "-$pid" 2>/dev/null || true
  done
  # Startup failures, frontend and leftovers after backend drain remain bounded.
  if [[ ${#GROUPS_STARTED[@]} -gt 0 ]]; then sleep 1; fi
  for pid in "${GROUPS_STARTED[@]}"; do
    kill -KILL -- "-$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

run_phase() {
  local label="$1" timeout="$2" directory="$3" pid code=0 started=$SECONDS
  shift 3
  echo "[$label] 开始（最长 ${timeout}s）"
  (cd "$directory" && exec "$@") >"$RUN_DIR/$label.log" 2>&1 &
  pid=$!
  GROUPS_STARTED+=("$pid")
  while kill -0 "$pid" 2>/dev/null; do
    if (( SECONDS - started >= timeout )); then
      echo "[$label] 超时；日志：$RUN_DIR/$label.log" >&2
      tail -n 80 "$RUN_DIR/$label.log" >&2
      exit 124
    fi
    sleep 0.2
  done
  wait "$pid" || code=$?
  # Also reap descendants of a tool that exits before its children.
  kill -TERM -- "-$pid" 2>/dev/null || true
  kill -KILL -- "-$pid" 2>/dev/null || true
  unset 'GROUPS_STARTED[${#GROUPS_STARTED[@]}-1]'
  if (( code != 0 )); then
    echo "[$label] 失败，退出码：$code；日志：$RUN_DIR/$label.log" >&2
    tail -n 80 "$RUN_DIR/$label.log" >&2
    exit "$code"
  fi
  echo "[$label] 完成"
}

check_services() {
  local pid code
  for pid in "${BACKEND_PID:-}" "${FRONTEND_PID:-}"; do
    [[ -z "$pid" ]] && continue
    if ! kill -0 "$pid" 2>/dev/null; then
      code=0
      wait "$pid" || code=$?
      echo "[运行] 服务意外退出（进程 $pid），原退出码：$code；诊断目录：$RUN_DIR" >&2
      tail -n 80 "$RUN_DIR/backend.log" >&2
      if [[ -f "$RUN_DIR/frontend.log" ]]; then tail -n 80 "$RUN_DIR/frontend.log" >&2; fi
      if (( code == 0 )); then code=1; fi
      exit "$code"
    fi
  done
}

wait_ready() {
  local label="$1" url="$2" timeout="$3" started=$SECONDS
  echo "[$label 就绪] 等待 $url（最长 ${timeout}s）"
  while (( SECONDS - started < timeout )); do
    check_services
    if curl --noproxy '*' --silent --fail --connect-timeout 1 --max-time 1 "$url" -o /dev/null; then
      check_services
      echo "[$label 就绪] 完成"
      return
    fi
    sleep 0.2
  done
  echo "[$label 就绪] 超时；诊断目录：$RUN_DIR" >&2
  exit 124
}

if [[ ! -d "$ROOT_DIR/frontend/node_modules" ]]; then
  run_phase install "$INSTALL_TIMEOUT_SECONDS" "$ROOT_DIR/frontend" bun install
else
  echo '[install] 已有 node_modules，跳过安装'
fi
run_phase build "$BUILD_TIMEOUT_SECONDS" "$ROOT_DIR/backend" go build -o "$RUN_DIR/server" ./cmd/server

# Refuse an already-serving endpoint; never stop an unrelated service.
for url in "$BACKEND_ORIGIN/api/health" "http://127.0.0.1:$FRONTEND_PORT/"; do
  if curl --noproxy '*' --silent --connect-timeout 1 --max-time 1 "$url" -o /dev/null; then
    echo "[启动] 地址已被占用：$url；请修改端口或自行停止旧服务。" >&2
    exit 1
  fi
done
echo "[后端启动] 配置：$BACKEND_CONFIG_PATH；代理目标：$BACKEND_ORIGIN"
(cd "$ROOT_DIR/backend" && exec "$RUN_DIR/server" -config "$BACKEND_CONFIG_PATH" -dev -dev-frontend-port "$FRONTEND_PORT") >"$RUN_DIR/backend.log" 2>&1 &
BACKEND_PID=$!
GROUPS_STARTED+=("$BACKEND_PID")
wait_ready 后端 "$BACKEND_ORIGIN/api/health" "$BACKEND_READY_TIMEOUT_SECONDS"

echo "[前端启动] http://localhost:$FRONTEND_PORT"
(
  cd "$ROOT_DIR/frontend"
  export BACKEND_ORIGIN VITE_BACKEND_ORIGIN="$BACKEND_ORIGIN"
  export VITE_PUBLIC_SHARE_ORIGIN="$FRONTEND_PUBLIC_SHARE_ORIGIN" VITE_TRANSFER_ORIGIN="$FRONTEND_TRANSFER_ORIGIN"
  export VITE_FRONTEND_PORT="$FRONTEND_PORT" VITE_TRANSFER_PORT="$BACKEND_PORT"
  exec bun run dev -- --host "$FRONTEND_HOST" --port "$FRONTEND_PORT" --strictPort
) >"$RUN_DIR/frontend.log" 2>&1 &
FRONTEND_PID=$!
GROUPS_STARTED+=("$FRONTEND_PID")
wait_ready 前端 "http://127.0.0.1:$FRONTEND_PORT/" "$FRONTEND_READY_TIMEOUT_SECONDS"
RUNTIME_READY=true
echo '[运行] 前后端已就绪。按一次 Ctrl+C 开始停止，等待当前传输完成。'
while true; do
  check_services
  sleep 0.2
done
