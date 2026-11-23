GO ?= go
PNPM ?= pnpm
BIN := bin/aigate
CLI_BIN := bin/aigatectl

# 构建期信息：版本号默认取 git tag（无 tag 时退化为 v0.0.0），由链接器注入；
# commit 与构建时间由 Go 工具链自动写入 VCS 元数据，无需额外传参。
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.0.0)
LDFLAGS := -X 'aigate/internal/version.Version=$(VERSION)'

.PHONY: build build-cli build-webui run test test-webui test-e2e test-e2e-sqlite test-e2e-mysql test-e2e-webui vet fmt tidy clean

# 先构建前端再编译二进制：go:embed 打包的是 internal/webui/dist 里的真实产物。
build: build-webui
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/aigate

build-cli:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(CLI_BIN) ./cmd/aigatectl

build-webui:
	$(PNPM) --dir webui install --frozen-lockfile
	$(PNPM) --dir webui build

run: build
	$(BIN) -db-type sqlite -db-dsn aigate.db -init-admin-token dev-admin-token

test:
	$(GO) test ./...

test-webui:
	$(PNPM) --dir webui test

# 集成测试默认走 SQLite：临时库 + 网关 AutoMigrate 建表，无需外部依赖，跑完即弃。
test-e2e: test-e2e-sqlite

test-e2e-sqlite:
	AIGATE_E2E_DB_TYPE=sqlite bash test/e2e/run.sh

# 需要本地 MySQL（root 免密）与 mysql 客户端：每次重置 aigate_e2e 库并导入 sql/init.sql。
test-e2e-mysql:
	AIGATE_E2E_DB_TYPE=mysql bash test/e2e/run.sh

# 浏览器 e2e：临时 SQLite + 内置 mock 上游，跑完自动清理进程与端口。
test-e2e-webui:
	bash webui/e2e/run.sh

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

tidy:
	$(GO) mod tidy

clean:
	rm -rf bin
