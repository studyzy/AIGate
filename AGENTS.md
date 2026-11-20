本文件为 AI 在本仓库工作时的指引。仓库自述见 `README.md`（英文）与 `README.zh-CN.md`（中文）。

## 项目概况

**Tencent Cloud AIGate** 是自托管、单二进制的 LLM 网关。Go 1.24，模块名 `aigate`，无 Web 框架（标准库 `net/http` + Go 1.22 路由模式），存储用 GORM（SQLite 本地 / MySQL 生产）。

三面 API，同一进程：

- 数据面 `internal/proxy`：`POST /v1/chat/completions`、`POST /v1/messages`（Anthropic 入口）、`GET /v1/models`，`sk-` 密钥鉴权。
- 管理面 `internal/admin`：`/admin/*`，模型、凭据、密钥、用户、分组、用量、设置、webhook、导入。
- 用户面 `internal/userapi`：`/api/user/*`，本人信息、配额、用量，以及本人密钥的增删改查、rotate 与用量（`/api/user/keys`）。
- `GET /healthz` 健康检查。

## 常用命令

```bash
make build          # go build -o bin/aigate ./cmd/aigate
make run            # 本地 SQLite 启动（dev-admin-token）
make test           # go test ./...
make vet            # go vet ./...
make fmt            # gofmt -l -w .
make test-e2e       # 需要本地 MySQL + mysql 客户端：重置 aigate_e2e 库、导入 sql/init.sql、构建二进制后跑 go test -tags e2e
./demo/quickstart.sh  # 需要 Docker + curl + python3：容器 + mock 上游端到端演示
```

提交前必须满足 CI 口径（`.ci/test.yml`、`.github/workflows/ci.yml`）：
`gofmt -l cmd internal test` 输出为空、`go build ./...`、`go vet ./...`、`go test -race ./... -count=1`。

## 包结构与边界

| 包 | 职责 |
| --- | --- |
| `cmd/aigate` | 单二进制入口：配置加载、加密/SSRF 策略装配、DB 打开与迁移、bootstrap 管理员、路由注册、优雅停机 |
| `internal/proxy` | 数据面管道：鉴权 → 模型解析 → 参数强制 → 上游转发（含流式）→ 用量记账；Anthropic 入口转换与 SSE 事件映射 |
| `internal/provider` | 上游协议适配（openai-completions / anthropic-messages）、出站 HTTP、SSRF 防护 |
| `internal/quota` | 多窗口配额引擎：规则解析/校验/规范化、窗口计算、四维检查（global/model/user/key） |
| `internal/model` | 唯一的数据访问层：GORM 模型与 Store 方法、AutoMigrate（仅 SQLite） |
| `internal/admin` `internal/userapi` | HTTP 层：仅做协议编解码、鉴权编排、参数校验；生产代码**不直接访问数据库、不 import gorm** |
| `internal/auth` | Bearer token 中间件：`RequireAdmin` / `RequireUser`、`UserFromContext` |
| `internal/dto` | 管理面与用户面共用的对外 JSON 结构与组装，保证契约单一来源 |
| `internal/httpx` | 统一 JSON 响应、错误体 `{"error":{...}}`、1 MiB body 上限、路径 ID 解析 |
| `internal/event` | webhook 事件出口：信封 + HMAC 签名 + 进程内异步投递（队列满即丢弃，绝不阻塞数据面） |
| `internal/crypto` | 密钥类字段 AES-256-GCM 加解密 |
| `internal/protocol` | 上游协议类型常量（`ai_models.model_type` 的单一事实源） |
| `internal/queryparams` | 列表接口查询参数解析、用量明细行视图 |
| `internal/config` | flag + `AIGATE_*` 环境变量（flag > env > 默认），非法配置直接拒绝启动 |

## 编码约定

- **每个 `.go` 文件都以 13 行 Tencent Apache-2.0 头注释开头**（新建文件必须照抄，123/123 现有文件均如此）：

```go
//
//
// Tencent is pleased to support the open source community by making Tencent Cloud AIGate available.
//
// Copyright (C) 2026 Tencent.
// All rights reserved.
//
// If you have downloaded a copy of the Tencent Cloud AIGate source code from Tencent,
// please note that Tencent Cloud AIGate source code is licensed under the Apache 2.0 License,
// A copy of the Apache 2.0 License is included in this file.
//
//
```

- **注释与文档用中文**（包注释、行内注释、`docs/*.zh-CN.md`）；**对外 API 的错误文案用英文**（`httpx.WriteError` 的内容）。
- 每个 internal 包有 `// Package xxx ...` 包注释说明职责与边界；`cmd/aigate` 用 Go 惯例的 `// Command aigate ...`。
- 错误判定用 `errors.Is` / `errors.As`，包装用 `fmt.Errorf("...: %w", err)`；`errors.New` 用于哨兵值。
- 日志用 `log/slog` + `key, value` 结构化字段，不拼字符串。
- 导入路径统一 `aigate/internal/...`（模块名 `aigate`）。
- 公开 API / 配置变化要同步 `docs/API.md`、`docs/API.zh-CN.md`、`docs/openapi.yaml`（已覆盖全部路由）与 `CHANGELOG.md`。

## 测试约定

- 黑盒测试放在独立包 `xxx_test`，HTTP 层经该包 `Register` + `httptest` 走真实 HTTP，只依赖导出接口；需要断内部实现时才用同包白盒测试，通常以 `_internal_test.go` 命名（如 `errors_internal_test.go`；例外：`internal/provider/ssrf_test.go` 与 `internal/event/*_test.go` 是同包白盒）。
- 共享夹具通常放 `testing_test.go`（`admin`/`auth`/`model`/`proxy`/`quota`/`userapi`：`newTestStore` / `newTestServer`，临时 SQLite 文件库 + `t.TempDir()` + `t.Cleanup`）。
- 端到端冒烟放 `test/e2e`：以外部黑盒方式驱动构建出的二进制，进程内起 httptest mock 上游、直连 MySQL 断言落库；带 `e2e` build tag，`make test` 不会执行，通过 `test/e2e/run.sh` 驱动。
- 行为变更应带测试；配额、SSRF、密钥生命周期、Anthropic 转换等安全相关路径改动尤其需要。

## 数据库 schema

- SQLite：启动时 `store.AutoMigrate()` 自动建表（仅本地开发）。
- MySQL：**不自动迁移**，生产与 e2e 都导入 `sql/init.sql`。改 GORM 模型时必须同步改 `sql/init.sql`——它是全新安装的单一事实源。

## 安全红线

- 不提交真实 API key、token、客户数据或内部端点（见 `CONTRIBUTING.md`、`SECURITY.md`）。
- 密钥类字段只在写入时加密（`internal/crypto`）、读接口只返回掩码或 `prefix`，明文 token 仅在创建/rotate 的响应中返回一次。
- 出站请求必须经 SSRF 防护（URL 校验、拨号时重解析、重定向校验）；私网/回环默认拒绝，仅靠 `-allow-private-upstreams` 放行。
- 未配置 `AIGATE_MASTER_KEY` 时密钥明文落库，启动会告警——不要在生产接受该状态。

## 版本发布

`cmd/aigate/version.go` 的 `Version` 与 `CHANGELOG.md` 同步递增；CHANGELOG 遵循 Keep a Changelog + SemVer，v0.x 内保持 API/配置向后兼容。
