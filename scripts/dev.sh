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

BACKEND_STDOUT_LOG="$(mktemp "${TMPDIR:-/tmp}/file-trans-backend-stdout.XXXXXX.log")"
BACKEND_STDERR_LOG="$(mktemp "${TMPDIR:-/tmp}/file-trans-backend-stderr.XXXXXX.log")"

show_log_tail() {
  local path="$1"
  local title="$2"
  if [[ -s "${path}" ]]; then
    echo "" >&2
    echo "${title}" >&2
    while IFS= read -r line; do
      echo "  ${line}" >&2
    done < <(tail -n 80 "${path}")
  fi
}

show_backend_failure() {
  local message="$1"
  local code="${2:-}"
  echo "" >&2
  echo "${message}" >&2
  if [[ -n "${code}" ]]; then
    echo "退出码：${code}" >&2
  fi
  show_log_tail "${BACKEND_STDERR_LOG}" "后端错误日志："
  show_log_tail "${BACKEND_STDOUT_LOG}" "后端输出日志："
  cat >&2 <<MSG

常见处理方式：
  1. 如果提示 YAML 格式错误，请检查 backend/config.yaml 对应行附近的缩进。
  2. file_picker 应与 storage 同级；不要把 roots/max_page_size/deny_names 缩进到 storage.shares 下面。
  3. 如果提示端口监听失败，请确认 ${BACKEND_ORIGIN} 没有被其他进程占用，或用 BACKEND_PORT 修改端口。
  4. 如果提示数据库无法打开，请确认 backend/data 目录可写。
日志文件：
  stdout: ${BACKEND_STDOUT_LOG}
  stderr: ${BACKEND_STDERR_LOG}
MSG
}

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
      local code=0
      wait "${BACKEND_PID}" || code=$?
      show_backend_failure "后端启动失败。" "${code}"
      exit "${code}"
    fi
    # 通过 bash 内置 TCP 发送真实健康检查请求，避免端口被旧进程占用时误判为本项目后端就绪。
    if (exec 3<>"/dev/tcp/${host}/${port}" && printf 'GET /api/health HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n' "${host}" >&3 && IFS= read -r -t 1 line <&3 && [[ "${line}" == *" 200 "* ]]) >/dev/null 2>&1; then
      exec 3>&- 3<&- || true
      return
    fi
    sleep 0.5
  done
  show_backend_failure "后端未在预期时间内就绪。" ""
  exit 1
}

echo "启动后端：${BACKEND_CONFIG}"
echo "前端代理目标：${BACKEND_ORIGIN}"
(
  cd "${ROOT_DIR}/backend"
  go run ./cmd/server -config "${BACKEND_CONFIG_PATH}"
) >"${BACKEND_STDOUT_LOG}" 2>"${BACKEND_STDERR_LOG}" &
BACKEND_PID=$!
wait_for_backend

echo "启动前端：http://localhost:${FRONTEND_PORT}"
(
  cd "${ROOT_DIR}/frontend"
  BACKEND_ORIGIN="${BACKEND_ORIGIN}" VITE_BACKEND_ORIGIN="${BACKEND_ORIGIN}" \
    bun run dev -- --host "${FRONTEND_HOST}" --port "${FRONTEND_PORT}"
) &
FRONTEND_PID=$!

wait -n "${BACKEND_PID}" "${FRONTEND_PID}" || code=$?
code="${code:-0}"
if ! kill -0 "${BACKEND_PID}" >/dev/null 2>&1; then
  show_backend_failure "后端进程已退出。" "${code}"
fi
exit "${code}"
