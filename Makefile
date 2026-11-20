GO ?= go
BIN := bin/aigate
CLI_BIN := bin/aigatectl

.PHONY: build build-cli run test test-e2e vet fmt tidy clean

build:
	$(GO) build -o $(BIN) ./cmd/aigate

build-cli:
	$(GO) build -o $(CLI_BIN) ./cmd/aigatectl

run: build
	$(BIN) -db-type sqlite -db-dsn aigate.db -init-admin-token dev-admin-token

test:
	$(GO) test ./...

test-e2e:
	bash test/e2e/run.sh

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

tidy:
	$(GO) mod tidy

clean:
	rm -rf bin
