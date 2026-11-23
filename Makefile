GO ?= go
PNPM ?= pnpm
BIN := bin/aigate
CLI_BIN := bin/aigatectl

.PHONY: build build-cli build-webui run test test-webui test-e2e test-e2e-sqlite test-e2e-mysql test-e2e-webui vet fmt tidy clean

# 先构建前端再编译二进制：go:embed 打包的是 internal/webui/dist 里的真实产物。
build: build-webui
	$(GO) build -o $(BIN) ./cmd/aigate

build-cli:
	$(GO) build -o $(CLI_BIN) ./cmd/aigatectl

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
