#!/usr/bin/env bash
# 本地一次跑齐 CI 门禁。与 CI 口径的差别只在环境：Go 构建缓存落在仓库内 .cache/，
# 浏览器二进制读用户缓存。门禁内容与 .github/workflows/ci.yml 保持一致。
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd); cd "$ROOT"
export PATH="/usr/local/go/bin:$PATH"
export GOCACHE="$ROOT/.cache/gocache" GOMODCACHE="$HOME/go/pkg/mod"
export PLAYWRIGHT_BROWSERS_PATH="${HOME}/.cache/ms-playwright"

# go job
test -z "$(gofmt -l cmd internal test)"
go build ./...
go vet ./...
go test -race ./... -count=1

# e2e job（matrix: sqlite / mysql）
AIGATE_E2E_DB_TYPE=sqlite bash test/e2e/run.sh
AIGATE_E2E_DB_TYPE=mysql bash test/e2e/run.sh

# webui job：pnpm build 内含 tsc -b（类型检查），单测单独一步
pnpm --dir webui build
pnpm --dir webui test

# 浏览器 e2e，放在 pnpm build 之后：run.sh 自己构建二进制（嵌入 dist）并起服务
bash webui/e2e/run.sh