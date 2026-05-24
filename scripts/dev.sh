#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BACKEND_CONFIG="${BACKEND_CONFIG:-backend/config.yaml}"
BACKEND_PORT="${BACKEND_PORT:-17878}"
# 使用 127.0.0.1 避免部分系统把 localhost 解析到 IPv6 ::1，导致前端代理连不上只监听 IPv4 的后端。
BACKEND_ORIGIN="${BACKEND_ORIGIN:-http://127.0.0.1:${BACKEND_PORT}}"
FRONTEND_HOST="${FRONTEND_HOST:-0.0.0.0}"
FRONTEND_PORT="${FRONTEND_PORT:-5173}"

if [[ "${BACKEND_CONFIG}" = /* ]]; then
  BACKEND_CONFIG_PATH="${BACKEND_CONFIG}"
else
  BACKEND_CONFIG_PATH="${ROOT_DIR}/${BACKEND_CONFIG}"
fi

if ! command -v go >/dev/null 2>&1; then
  echo "未找到 go，请先安装 Go。" >&2
  exit 1
fi

if ! command -v bun >/dev/null 2>&1; then
  echo "未找到 bun，请先安装 Bun。" >&2
  exit 1
fi

if [[ ! -f "${BACKEND_CONFIG_PATH}" ]]; then
  mkdir -p "$(dirname "${BACKEND_CONFIG_PATH}")"
  cp "${ROOT_DIR}/backend/config.example.yaml" "${BACKEND_CONFIG_PATH}"
  cat >&2 <<MSG
已复制 backend/config.example.yaml 到 ${BACKEND_CONFIG}。
请先编辑配置文件，至少替换：
  - auth.totp_secret
  - auth.admin.username
  - auth.admin.password_sha256
  - storage.dirs

配置完成后再次运行：./scripts/dev.sh
MSG
  exit 1
fi

if [[ ! -d "${ROOT_DIR}/frontend/node_modules" ]]; then
  echo "首次运行：安装前端依赖..."
  (cd "${ROOT_DIR}/frontend" && bun install)
fi

cleanup() {
  local code=$?
  if [[ -n "${BACKEND_PID:-}" ]]; then
    kill "${BACKEND_PID}" >/dev/null 2>&1 || true
  fi
  if [[ -n "${FRONTEND_PID:-}" ]]; then
    kill "${FRONTEND_PID}" >/dev/null 2>&1 || true
  fi
  wait >/dev/null 2>&1 || true
  exit "${code}"
}
trap cleanup INT TERM EXIT

wait_for_backend() {
  local origin="${BACKEND_ORIGIN#http://}"
  origin="${origin#https://}"
  local host="${origin%%[:/]*}"
  local rest="${origin#${host}}"
  local port="${rest#:}"
  port="${port%%/*}"
  if [[ -z "${host}" || -z "${port}" || "${port}" = "${rest}" ]]; then
    sleep 2
    return
  fi
  echo "等待后端就绪：${BACKEND_ORIGIN}/api/health"
  for _ in {1..60}; do
    if ! kill -0 "${BACKEND_PID}" >/dev/null 2>&1; then
      wait "${BACKEND_PID}"
    fi
    # 这里只检查 TCP 端口是否可连接，避免额外依赖 curl；后端监听后 /api/health 会立即可用。
    if (exec 3<>"/dev/tcp/${host}/${port}") >/dev/null 2>&1; then
      exec 3>&- 3<&- || true
      return
    fi
    sleep 0.5
  done
  echo "后端未在预期时间内就绪，请检查后端启动日志。" >&2
  exit 1
}

echo "启动后端：${BACKEND_CONFIG}"
echo "前端代理目标：${BACKEND_ORIGIN}"
(
  cd "${ROOT_DIR}/backend"
  go run ./cmd/server -config "${BACKEND_CONFIG_PATH}"
) &
BACKEND_PID=$!
wait_for_backend

echo "启动前端：http://localhost:${FRONTEND_PORT}"
(
  cd "${ROOT_DIR}/frontend"
  BACKEND_ORIGIN="${BACKEND_ORIGIN}" VITE_BACKEND_ORIGIN="${BACKEND_ORIGIN}" \
    bun run dev -- --host "${FRONTEND_HOST}" --port "${FRONTEND_PORT}"
) &
FRONTEND_PID=$!

wait -n "${BACKEND_PID}" "${FRONTEND_PID}"
