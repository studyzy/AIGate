GO ?= go
PNPM ?= pnpm
BIN := bin/aigate

.PHONY: build build-webui run test test-webui test-e2e test-e2e-webui vet fmt tidy clean

# 先构建前端再编译二进制：go:embed 打包的是 internal/webui/dist 里的真实产物。
build: build-webui
	$(GO) build -o $(BIN) ./cmd/aigate

build-webui:
	$(PNPM) --dir webui install --frozen-lockfile
	$(PNPM) --dir webui build

run: build
	$(BIN) -db-type sqlite -db-dsn aigate.db -init-admin-token dev-admin-token

test:
	$(GO) test ./...

test-webui:
	$(PNPM) --dir webui test

test-e2e:
	bash test/e2e/run.sh

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
