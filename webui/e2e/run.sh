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
# 先重建前端再编译二进制：go:embed 打包的是 internal/webui/dist 里的**已有产物**，
# 少了这一步，改了 webui/src 却只跑 go build，测到的仍是上一次构建的界面。
pnpm --dir webui build >"$WORK/webui-build.log" 2>&1 || {
  echo "webui build failed:" >&2
  tail -20 "$WORK/webui-build.log" >&2
  exit 1
}
go build -o "$WORK/aigate" ./cmd/aigate

node webui/e2e/mock-upstream.mjs >"$WORK/mock.log" 2>&1 &
MOCK_PID=$!

"$WORK/aigate" -db-type sqlite -db-dsn "$WORK/e2e.db" -init-admin-token e2e-admin \
  -allow-private-upstreams -addr "$APP_ADDR" \
  -request-log=sqlite \
  -request-log-config "{\"path\":\"$WORK/request-logs.db\",\"retention_days\":0}" \
  >"$WORK/aigate.log" 2>&1 &
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

# 浏览器必须与网关共用同一时区：网关默认 -timezone=Asia/Shanghai（internal/config），
# 而用量页的 todayISO() 取浏览器本地日期。CI runner 默认 UTC，16:00–24:00 UTC 时浏览器
# 日期比业务时区晚一天，usage.spec.ts 会查不到刚产生的用量行（"No usage in this range"）。
# 固定 TZ 让浏览器 e2e 在任意时刻、任意宿主时区下都确定性通过；若将来改网关默认时区，
# 这里需同步调整。
cd "$ROOT/webui"
TZ=Asia/Shanghai WEBUI_BASE_URL="http://$APP_ADDR" pnpm exec playwright test "$@"
