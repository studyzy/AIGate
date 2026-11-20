#!/usr/bin/env bash
# 起真实二进制 + mock 上游，跑浏览器 e2e；退出时清理进程与临时库。
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
WORK=$(mktemp -d)
APP_ADDR=127.0.0.1:18080
MOCK_ADDR=127.0.0.1:19090

cleanup() {
  [[ -n "${APP_PID:-}" ]] && kill "$APP_PID" 2>/dev/null || true
  [[ -n "${MOCK_PID:-}" ]] && kill "$MOCK_PID" 2>/dev/null || true
  wait 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

if curl -sf "http://$APP_ADDR/healthz" >/dev/null 2>&1; then
  echo "port $APP_ADDR already in use" >&2
  exit 1
fi
if curl -sf "http://$MOCK_ADDR/" >/dev/null 2>&1; then
  echo "port $MOCK_ADDR already in use" >&2
  exit 1
fi

cd "$ROOT"
command -v go >/dev/null 2>&1 || { echo "go toolchain not found (needed to build the binary under test)" >&2; exit 1; }
command -v pnpm >/dev/null 2>&1 || { echo "pnpm not found (run: corepack enable)" >&2; exit 1; }
go build -o "$WORK/aigate" ./cmd/aigate

node webui/e2e/mock-upstream.mjs >"$WORK/mock.log" 2>&1 &
MOCK_PID=$!

"$WORK/aigate" -db-type sqlite -db-dsn "$WORK/e2e.db" -init-admin-token e2e-admin \
  -allow-private-upstreams -addr "$APP_ADDR" >"$WORK/aigate.log" 2>&1 &
APP_PID=$!

for _ in $(seq 1 100); do
  curl -sf "http://$APP_ADDR/healthz" >/dev/null && break
  sleep 0.2
done
if ! curl -sf "http://$APP_ADDR/healthz" >/dev/null; then
  echo "aigate did not become healthy:" >&2
  cat "$WORK/aigate.log" >&2
  exit 1
fi

cd "$ROOT/webui"
WEBUI_BASE_URL="http://$APP_ADDR" pnpm exec playwright test "$@"
