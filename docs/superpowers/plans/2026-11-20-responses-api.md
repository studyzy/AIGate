# AIGate Responses API 全量矩阵与有状态支持 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 AIGate 支持 OpenAI Responses API，做到「3 个入口 × 3 种上游 = 9 格全量矩阵」全部可用，并支持 `store` / `previous_response_id` / `conversation` / `background` 有状态能力。

**Architecture:** `Provider` 接口（恒吐 OpenAI-chat 形状）是唯一的转换边界。新增两个转换器：`proxy/responses_convert.go`（Responses 入口侧）与 `provider/responses.go`（Responses 上游侧）。`responses` 入口 × `responses` 上游走原生直通。会话状态存 MySQL/SQLite 的 `response_states` 表，不引入 Redis。

**Tech Stack:** Go 1.24 标准库（`net/http`、`encoding/json`、`crypto/rand`、`bufio`）、GORM（SQLite/MySQL）、`httptest`；无新增依赖。

**Spec:** `docs/superpowers/specs/2026-11-20-responses-api-design.md`

## Global Constraints

- 模块名 `aigate`，Go 1.24；`go.mod` 现有 4 个直接依赖全部保留，**不引入任何第三方依赖**（特别是**不引入 Redis 客户端**）。
- 每个新建 `.go` 文件以 13 行 Tencent Apache-2.0 头注释开头；每个 `internal` 包有 `// Package xxx ...` 包注释；导入统一 `aigate/internal/...`。
- 注释与文档用中文，**对外错误文案用英文**；日志用 `log/slog` + 结构化 `key, value` 字段。
- 协议常量值逐字：`TypeOpenAIResponses = "openai-responses"`。
- 上游 URL 拼接：`openai-responses` 的 `url` 含 `/v1`，转发时拼 `/responses`（与 `openai-completions` 拼 `/chat/completions` 对称）。
- **`store` 缺省为 `true`**（OpenAI 语义）；站点开关 `-allow-response-storage` 默认 `true`。
- 转发给上游时**一律强制 `store:false`**，避免对话正文同时留在上游与网关。
- 归属校验按 `owner_user_id`，失配**一律 404**（不区分不存在/不属于你）；`owner_api_key_id` 仅记录不校验。
- `/v1/responses` 的 SSE **不发送 `data: [DONE]`**，终止于 `event: response.completed`。
- `input_tokens` **已包含** cached 部分；`PromptCacheReadTokens` 是 `PromptTokens` 的子集，**配额计算不得相加**。
- 出站必须经 SSRF 防护（复用 `internal/provider/upstream.go` 的 `newUpstreamRequest` / `doUpstream`）。
- MySQL 不自动迁移：改 GORM 模型必须同步 `sql/init.sql`；SQLite 由 `AutoMigrate` 建表（`internal/model/model.go:97-111` 硬编码列表）。
- 验收口径逐字：`test -z "$(gofmt -l cmd internal test)"`、`go build ./...`、`go vet ./...`、`go test -race ./... -count=1`。
- 黑盒测试惯例：独立包 `xxx_test`，经 `httptest` 走真实 HTTP，共享夹具放 `testing_test.go`。
- 每个任务以一次 `git commit` 收尾（语义化前缀）。

## Review Focus

1. **`store` 缺省即落库**：客户端不传 `store` 时会被持久化完整对话正文（与 Chat Completions 缺省 `false` 相反）。站点开关关闭后必须整体拒绝，而不是静默改写 → pinned in **Task 8, Step 1**。
2. **跨用户越权读**：`previous_response_id` 由客户端任意提供，只校验 id 不校验 owner 就会读到别人的对话；按 `user_id` 校验、越权与不存在一律 404 不可区分 → pinned in **Task 6, Step 1**。
3. **用量记成 0**：Responses 用 `input_tokens`/`output_tokens`，现有 `extractOpenAIUsage` 只读 `prompt_tokens`/`completion_tokens`；复用旧提取器会静默记账为 0 → pinned in **Task 2, Step 1**。
4. **静默走错协议**：`GetProvider` 的 `default` 回落 OpenAI，`openai-responses` 不加显式 case 会被当成 Chat Completions 转发 → pinned in **Task 1, Step 1**。
5. **SSE 事件序与 id 一致性**：`response.created` 与 `response.completed` 必须是同一个 `resp_` id，顺序错乱会让 OpenAI SDK 抛错 → pinned in **Task 5, Step 1**。
6. **`cancel` 被终态覆盖**：后台任务完成时若无条件写 `completed`，会把用户刚发出的 `cancelled` 覆盖掉 → pinned in **Task 9, Step 1**。

---

### Task 1: 协议常量与分发

**Files:**
- Modify: `internal/protocol/protocol.go:20-25`
- Modify: `internal/model/ai_model.go:31-34`
- Modify: `internal/provider/provider.go:177-184`
- Modify: `internal/admin/models.go:283`, `internal/admin/models.go:338`, `internal/admin/models.go:357-360`, `internal/admin/import.go:341`
- Test: `internal/provider/provider_test.go`（package `provider_test`，追加）

**Interfaces:**
- Consumes: 无（本任务是所有后续任务的地基）
- Produces（后续任务依赖，命名不得改）:
  - `protocol.TypeOpenAIResponses = "openai-responses"`
  - `model.ModelTypeResponses`（等于上述值）
  - `provider.GetProvider(modelType string) Provider` 对 `"openai-responses"` 返回 `*provider.OpenAIResponsesProvider`
  - `admin.validModelType` 接受三个取值

- [ ] **Step 1: 写失败测试**（`internal/provider/provider_test.go` 追加）

测试名 `TestGetProvider_OpenAIResponses`：断言 `GetProvider(protocol.TypeOpenAIResponses)` 返回非 nil，且类型名为 `*provider.OpenAIResponsesProvider`（用 `%T` 比较字符串）；同时断言 `GetProvider("unknown")` 与 `GetProvider("")` 仍返回 `*provider.OpenAIProvider`（保持历史回落行为）。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/provider/ -run TestGetProvider_OpenAIResponses -v`
Expected: FAIL —— `OpenAIResponsesProvider` 未定义，编译失败。

- [ ] **Step 3: 加常量与分发**

`internal/protocol/protocol.go` 的 const 块内追加：

```go
// TypeOpenAIResponses 是 OpenAI Responses 协议（URL 含 /v1，转发时拼 /responses）。
TypeOpenAIResponses = "openai-responses"
```

`internal/model/ai_model.go` 的常量组追加 `ModelTypeResponses = protocol.TypeOpenAIResponses`。

`internal/provider/provider.go` 的 `GetProvider` 加 `case protocol.TypeOpenAIResponses: return &OpenAIResponsesProvider{}`，`default` 保留不变。

`internal/admin/models.go` 的 `validModelType` 加 `|| t == model.ModelTypeResponses`；三处英文错误文案改为 `"model_type must be openai-completions, anthropic-messages or openai-responses"`（`models.go:283`、`models.go:338`、`import.go:341`）。

本步先只加常量与 case，`OpenAIResponsesProvider` 留到 Task 2——为让本任务可独立编译通过，Step 3 同时在 `internal/provider/responses.go` 建立**带 13 行头注释的最小骨架**：

```go
// OpenAIResponsesProvider 适配 OpenAI Responses API（/responses）。
type OpenAIResponsesProvider struct{}
```

三个接口方法在 Task 2 实现；本任务用 `var _ Provider = (*OpenAIResponsesProvider)(nil)` 会在编译期报缺失方法，因此**本任务先不写该断言**，Task 2 再加。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/provider/ -run TestGetProvider_OpenAIResponses -v && go build ./...`
Expected: PASS，且 `go build ./...` 成功。

- [ ] **Step 5: Commit**

```bash
git add internal/protocol internal/model internal/provider internal/admin
git commit -m "feat(protocol): add openai-responses upstream type"
```

---

### Task 2: Responses 上游适配器（chat ↔ Responses）

**Files:**
- Create: `internal/provider/responses.go`, `internal/provider/responses_internal_test.go`（package `provider`）
- Test: `internal/provider/responses_test.go`（package `provider_test`）

**Interfaces:**
- Consumes: Task 1 的 `OpenAIResponsesProvider` 骨架；`upstream.go` 的 `newUpstreamRequest` / `doUpstream` / `setSSEHeaders` / `forwardUpstreamError` / `ReadUpstreamBody` / `newProbeRequest` / `runConnectivityProbe`。
- Produces:
  - `func (p *OpenAIResponsesProvider) ChatCompletion(ctx, apiKey, apiBase, model string, reqBody []byte, customHeaders map[string]string) (*CompletionResult, int, error)`
  - `func (p *OpenAIResponsesProvider) ChatCompletionStream(...) (*StreamResult, int, error)`
  - `func (p *OpenAIResponsesProvider) CheckConnectivityWithChat(...) (time.Duration, error)`
  - `func buildResponsesRequest(model string, reqBody []byte, stream bool) ([]byte, error)` —— chat 请求体 → Responses 请求体
  - `func responsesToChatResponse(body []byte) ([]byte, error)` —— Responses 响应体 → chat 响应体
  - `func extractResponsesUsage(data string, prev *Usage) *Usage` —— 独立提取器，读 `input_tokens`/`output_tokens`
  - `func responsesStreamToChat(w http.ResponseWriter, flusher http.Flusher, r io.Reader) (*Usage, error)` —— Responses SSE → chat chunk + `[DONE]`

- [ ] **Step 1: 写失败测试**

`responses_test.go`（黑盒，`httptest` mock 上游）：`TestResponsesProvider_ChatCompletion` —— mock 上游返回一条最小 Responses JSON（`{"id":"resp_abc","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`），断言返回的 `result.Body` 解析后 `choices[0].message.content == "hi"`、`choices[0].finish_reason == "stop"`、`result.Usage.PromptTokens == 10`、`CompletionTokens == 5`。

`responses_internal_test.go`（白盒）：`TestExtractResponsesUsage` —— 断言 `input_tokens_details.cached_tokens=4` 与 `output_tokens_details.reasoning_tokens=3` 时，`PromptCacheReadTokens==4` 且 `TotalTokens` 不因 reasoning 重复累加；`TestBuildResponsesRequest` —— 输入 chat 体 `{"model":"x","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`，断言输出含 `max_output_tokens:100`、含 `input`、**不含** `max_tokens`、且 `store` 为 `false`；`TestResponsesToChatResponse_ToolCalls` —— `output` 含 `function_call` 时断言 `tool_calls[0].function.name` 与 `finish_reason=="tool_calls"`。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/provider/ -run 'TestResponsesProvider|TestExtractResponsesUsage|TestBuildResponsesRequest|TestResponsesToChatResponse' -v`
Expected: FAIL —— 函数未定义。

- [ ] **Step 3: 实现 `internal/provider/responses.go`**

按 Spec §5.3.1（请求）与 §5.3.3（响应，**Responses→chat 方向**）实现映射。要点：

- `buildResponsesRequest`：`messages` → `input`（`system` 提到 `instructions`，其余转 item）；`max_tokens`/`max_completion_tokens` → `max_output_tokens`；`response_format` → `text.format`；嵌套 tool → 扁平；**强制 `store:false`**；`stream=true` 时不加 `stream_options`（Responses 不需要）。
- `responsesToChatResponse`：取 `output[]` 中 `type=="message"` 的 `output_text` 拼接为 `content`；`function_call` 转 `tool_calls`；按 `status`/`incomplete_details.reason` 定 `finish_reason`（`completed`+有工具→`tool_calls`，`completed`→`stop`，`incomplete`+`max_output_tokens`→`length`）。
- `extractResponsesUsage`：读 `input_tokens`/`output_tokens`/`total_tokens`，`input_tokens_details.cached_tokens` → `PromptCacheReadTokens`，`input_tokens_details.cache_write_tokens` → `PromptCacheWriteTokens`；**不读 reasoning_tokens**（它是 output 子集）。
- `responsesStreamToChat`：逐 `event:`/`data:` 解析，`response.output_text.delta` → `chat.completion.chunk` 的 `delta.content`；`response.completed` 的 `usage` 作为最后一条带 usage 的 chunk；结束补 `data: [DONE]`（**这条路径的客户端是 chat 协议**）。函数签名里的 `io.Reader` 用于注入测试。
- `CheckConnectivityWithChat`：POST `<base>/responses`，body `{"model":model,"input":"hi","max_output_tokens":1,"store":false}`。
- 非 200：原样返回 status + body（与 `OpenAIProvider` 一致，不解析）。
- 请求超时复用 `doUpstream` 的传参：非流式 5 分钟、流式 30 分钟（与 `anthropic_ingress.go:47-48` 对齐）。

Step 3 同时把 Task 1 留下的 `var _ Provider = (*OpenAIResponsesProvider)(nil)` 断言加上。

- [ ] **Step 4: 运行确认通过**

Run: `go test ./internal/provider/ -race -v -run 'Responses|TestGetProvider' && go vet ./internal/provider/`
Expected: 全部 PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/provider
git commit -m "feat(provider): implement openai-responses upstream adapter"
```

---

### Task 3: `enforceMaxTokens` 支持 `max_output_tokens`

**Files:**
- Modify: `internal/proxy/proxy.go:380-403`
- Test: `internal/proxy/proxy_test.go`（package `proxy_test`，追加）

**Interfaces:**
- Consumes: 无
- Produces: `enforceMaxTokens(reqBody map[string]any, limit int)` 现在同时收敛 `max_output_tokens`，三个入口共用，**Responses 入口无需自行处理密钥上限**。

- [ ] **Step 1: 写失败测试**

`TestEnforceMaxTokens_MaxOutputTokens` 表驱动：

| 输入 | limit | 期望 |
|---|---|---|
| `{"max_output_tokens":9999}` | 100 | `max_output_tokens==100`，**不新增** `max_tokens` |
| `{"max_output_tokens":-1}` | 100 | `max_output_tokens==100` |
| `{"max_output_tokens":50}` | 100 | `max_output_tokens==50`（未超限不动） |
| `{"max_output_tokens":50,"max_tokens":9999}` | 100 | `max_output_tokens==50` 且 `max_tokens==100`（互不干扰） |

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/proxy/ -run TestEnforceMaxTokens_MaxOutputTokens -v`
Expected: FAIL —— 第一行得到 9999（未收敛）。

- [ ] **Step 3: 实现**

把 `enforceMaxTokens` 里现有的两个字段循环抽成对字段列表的遍历，列表加入 `"max_output_tokens"`；「两个字段都没有有效数值时注入 `max_tokens`」的逻辑保持**只针对 chat 字段**——若请求里出现 `max_output_tokens`，则不注入 `max_tokens`（避免 Responses 请求被塞入 chat 专有字段）。

- [ ] **Step 4: 运行确认通过**

Run: `go test ./internal/proxy/ -race -run 'TestEnforceMaxTokens' -v`
Expected: 新旧用例全部 PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/proxy
git commit -m "feat(proxy): enforce max_output_tokens cap alongside max_tokens"
```

---

### Task 4: 存储层 `ResponseState`

**Files:**
- Create: `internal/model/responses.go`, `internal/model/responses_test.go`
- Modify: `internal/model/model.go:97-111`（AutoMigrate）
- Modify: `sql/init.sql`（追加建表）

**Interfaces:**
- Consumes: 无
- Produces（后续任务依赖）:
  - `type ResponseState struct`（字段见 Spec §5.6 表格）
  - `func (s *Store) CreateResponseState(ctx context.Context, st *ResponseState) error`
  - `func (s *Store) GetResponseState(ctx context.Context, id string) (*ResponseState, error)` —— 不存在返回 `model.ErrResponseStateNotFound`
  - `func (s *Store) UpdateResponseStateStatus(ctx context.Context, id, status, errMsg string, items, body []byte) error` —— 带 `WHERE status <> 'cancelled'` 条件
  - `func (s *Store) PruneResponseStates(ctx context.Context, before time.Time) (int64, error)`
  - `func (s *Store) LatestResponseStateInConversation(ctx context.Context, conversationID string) (*ResponseState, error)`
  - `var ErrResponseStateNotFound = errors.New("response state not found")`

- [ ] **Step 1: 写失败测试**

`responses_test.go`（package `model_test`，复用 `testing_test.go` 的 `newTestStore`）：`TestCreateAndGetResponseState` —— 创建后用 `GetResponseState` 读回，断言 `Items`/`ResponseBody` 一致；`TestGetResponseState_NotFound` —— 断言 `errors.Is(err, model.ErrResponseStateNotFound)`；`TestUpdateResponseStateStatus_CancelledNotOverwritten` —— 先把状态置 `cancelled`，再用 `completed` 调用 `UpdateResponseStateStatus`，断言状态**仍是** `cancelled`；`TestPruneResponseStates` —— 插入 `CreatedAt` 为 10 天前与今天的各一行，`Prune(before=now-7d)` 返回 1 且旧行消失；`TestLatestResponseStateInConversation` —— 同会话插两行（时间不同），断言取到**较新**那行。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/model/ -run 'ResponseState' -v`
Expected: FAIL —— 类型与方法未定义。

- [ ] **Step 3: 实现**

`internal/model/responses.go`（13 行头注释）定义 `ResponseState` 与 `ErrResponseStateNotFound`，实现六个方法。GORM tag 按 Spec §5.6：`ID string gorm:"size:64;primaryKey"`、`Items`/`ResponseBody` 用 `gorm:"type:longtext;not null"`（SQLite 下 GORM 自动降级，测试会验证）、`CreatedAt time.Time gorm:"not null;index"`。

`internal/model/model.go` 的 `AutoMigrate` 列表加 `&ResponseState{}`。

`sql/init.sql` 追加 `CREATE TABLE IF NOT EXISTS response_states`（照 `webhook_deliveries`（`:169`）的模板风格，含 Spec §5.6 的四组索引）。

- [ ] **Step 4: 运行确认通过**

Run: `go test ./internal/model/ -race -run 'ResponseState' -v && go vet ./internal/model/`
Expected: 全部 PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/model sql/init.sql
git commit -m "feat(model): add response_states table for Responses session state"
```

---

### Task 5: Responses 入口转换器（Responses ↔ chat）

**Files:**
- Create: `internal/proxy/responses_convert.go`
- Test: `internal/proxy/responses_convert_internal_test.go`（package `proxy`）

**Interfaces:**
- Consumes: 无（纯函数转换）
- Produces（Task 7/8 依赖）:
  - `func responsesRequestToChat(body []byte) (map[string]any, error)`
  - `func chatResponseToResponses(chatBody []byte, requestedModel, respID string) ([]byte, error)`
  - `type responsesStreamTranslator struct` + `func newResponsesStreamTranslator(w http.ResponseWriter, flusher http.Flusher, model, respID string) *responsesStreamTranslator`
  - `func (t *responsesStreamTranslator) Write(p []byte) (int, error)` / `Wrote() bool` / `Finish()`

- [ ] **Step 1: 写失败测试**

`TestResponsesRequestToChat_InputString` —— `{"model":"m","input":"hi"}` → `messages==[{role:user,content:hi}]`；`TestResponsesRequestToChat_ItemsAndInstructions` —— `instructions` 变成首条 `system`，`input` 里的 `function_call`/`function_call_output` 转成 `tool_calls`/`tool` 消息；`TestResponsesRequestToChat_TextFormat` —— `text.format` 转 `response_format`；`TestChatResponseToResponses_RoundTrip` —— chat 响应转 responses 后 `output[0].content[0].text` 正确、`usage.input_tokens` 来自 `prompt_tokens`。

`TestResponsesStreamTranslator_EventOrder`（**Review Focus 5**）—— 喂入三条 chat chunk（`content:"he"`、`content:"llo"`、带 usage 与 `finish_reason:"stop"` 的末块），断言输出事件的 `type` 序列严格等于 `["response.created","response.in_progress","response.output_item.added","response.content_part.added","response.output_text.delta","response.output_text.delta","response.output_text.done","response.content_part.done","response.output_item.done","response.completed"]`；断言 `response.created` 与 `response.completed` 的 `id` **相同**且以 `resp_` 开头；断言输出**不含** `[DONE]`。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/proxy/ -run 'TestResponsesRequestToChat|TestChatResponseToResponses|TestResponsesStreamTranslator' -v`
Expected: FAIL —— 未定义。

- [ ] **Step 3: 实现 `internal/proxy/responses_convert.go`**

请求转换按 Spec §5.3.1；响应转换按 Spec §5.3.2；流式状态机按 Spec §5.5 的事件序列与「`created` 与 `completed` 同 id」约束。SSE 写出格式 `event: <type>\ndata: <json>\n\n`，**不写 `[DONE]`**。

`sanitizeUsage` 的行为（`openai.go:66-97`）不适用于 Responses，本文件独立实现，不改公共函数。

- [ ] **Step 4: 运行确认通过**

Run: `go test ./internal/proxy/ -race -run 'TestResponsesRequestToChat|TestChatResponseToResponses|TestResponsesStreamTranslator' -v`
Expected: 全部 PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/proxy/responses_convert.go internal/proxy/responses_convert_internal_test.go
git commit -m "feat(proxy): add responses<->chat conversion and SSE event synthesis"
```

---

### Task 6: Responses 入口处理器（无状态路径）

**Files:**
- Create: `internal/proxy/responses_ingress.go`
- Modify: `internal/proxy/proxy.go:48-54`（`Register` 加路由）
- Test: `internal/proxy/responses_ingress_test.go`（package `proxy_test`，用 `newEnv`）

**Interfaces:**
- Consumes: Task 1（协议）、Task 3（`max_output_tokens` 压制）、Task 5（转换器）、Task 2（上游适配器）
- Produces: `func (h *Handler) handleResponses(w http.ResponseWriter, r *http.Request)`，路由 `POST /v1/responses`

- [ ] **Step 1: 写失败测试**

`TestResponsesIngress_BasicCompletion` —— mock chat 上游返回标准 chat 响应；`POST /v1/responses` body `{"model":"m","input":"hi"}`；断言 200、响应 `object=="response"`、`output[0].content[0].text` 正确、`id` 以 `resp_` 开头。

`TestResponsesIngress_MissingAPIKey` —— 无 `Authorization` 断言 401（错误体沿用 OpenAI 风格 `{"error":{...}}`）。

`TestResponsesIngress_QuotaExceeded` —— 配一个 0 配额模型，断言 429 且带 `Retry-After`。

`TestResponsesIngress_PassthroughToResponsesUpstream`（**格 7**）—— 上游配成 `openai-responses` 类型，mock 返回 Responses JSON；断言网关**没有**把请求转成 chat（mock 侧检查收到的 body 含 `input` 且不含 `messages`），响应原样透出。

`TestResponsesIngress_CrossUserIsolation`（**Review Focus 2**）—— 用密钥 A 创建一条 response，再用**另一个用户**的密钥 B 带该 `previous_response_id` 请求，断言 404 且响应体**不透露** id 是否存在。

`TestResponsesIngress_Streaming` —— mock chat 上游返回 SSE（两条 `data:` chunk + `[DONE]`）；`POST /v1/responses` body 带 `"stream":true`；断言响应 `Content-Type: text/event-stream`、按序出现 `response.created` 与 `response.completed`、两个事件 `id` 相同、且**不含** `data: [DONE]`。

`TestResponsesIngress_StreamingPassthrough`（**格 7 流式**）—— 上游配 `openai-responses`，mock 返回 Responses SSE；断言网关原样透传 `event:`/`data:` 行、终态 `response.completed` 的 `id` 被改写为 `resp_` 前缀、且用量被记账（查 `LLMUsageLog`）。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/proxy/ -run 'TestResponsesIngress' -v`
Expected: FAIL —— 路由不存在（404）。

- [ ] **Step 3: 实现 `handleResponses`**

按 Spec §5.4.1 的六步流程实现。本任务只做**无状态**分支：鉴权 → 解析 → `prepareForward` → 按 `ModelType` 二选一（直通 / 转换）→ 流式或非流式 → `logUsage`。`store`/`previous_response_id`/`conversation`/`background` 的消费留到 Task 8/9，本任务遇到这些字段时：`store` 按缺省 `true` 接受但不落库（Task 8 补），`previous_response_id`/`conversation`/`background` 返回 `400`（Task 8/9 放开）。

`Register` 追加 `mux.HandleFunc("POST /v1/responses", h.handleResponses)`。

- [ ] **Step 4: 运行确认通过**

Run: `go test ./internal/proxy/ -race -run 'TestResponsesIngress' -v`
Expected: 全部 PASS（`CrossUserIsolation` 在本任务下因「一律 400」而通过，Task 8 改为真正 404 时该测试仍须通过）。

- [ ] **Step 5: Commit**

```bash
git add internal/proxy
git commit -m "feat(proxy): add POST /v1/responses stateless ingress"
```

---

### Task 7: 9 格矩阵测试

**Files:**
- Create: `internal/proxy/matrix_test.go`（package `proxy_test`）
- Test: 同上

**Interfaces:**
- Consumes: Task 1-6 全部
- Produces: 无（纯验证）

- [ ] **Step 1: 写矩阵测试**

`TestMatrix_AllNinesCells` 表驱动，9 行：

| 入口 | 上游 `model_type` |
|---|---|
| `/v1/chat/completions` | `openai-completions` / `anthropic-messages` / `openai-responses` |
| `/v1/messages` | 同上三种 |
| `/v1/responses` | 同上三种 |

每格：mock 上游按被请求的路径返回对应协议的响应；断言**状态码 200** 且客户端协议的错误/成功形状正确（chat 入口得 `choices[]`、messages 入口得 `content[]`、responses 入口得 `output[]`）。断言 handler 不 panic。

- [ ] **Step 2: 运行**

Run: `go test ./internal/proxy/ -race -run TestMatrix -v`
Expected: 先跑一遍看哪些格失败——失败格即实现缺口，回到对应 Task 修。

- [ ] **Step 3: 修复失败格**

按 Spec §5.4 表格判断是哪一格、走哪条路径，修对应实现。

- [ ] **Step 4: 运行确认全绿**

Run: `go test ./internal/proxy/ -race -run TestMatrix -v`
Expected: 9 格全部 PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/proxy/matrix_test.go
git commit -m "test(proxy): cover all nine ingress x upstream matrix cells"
```

---

### Task 8: 有状态 — `store` / `previous_response_id` / `conversation`

**Files:**
- Modify: `internal/proxy/responses_ingress.go`
- Modify: `internal/config/config.go`（新增两个配置项）
- Modify: `cmd/aigate/main.go:139`（装配配置到 Handler）
- Test: `internal/proxy/responses_state_test.go`（package `proxy_test`）

**Interfaces:**
- Consumes: Task 4 的 `model.*ResponseState` 方法
- Produces:
  - `Handler` 新增字段 `allowResponseStorage bool`，`NewHandler` 签名**不变**，新增 `func (h *Handler) SetResponseStorage(allow bool) error`（供 `main.go` 设置；同时启动清理 goroutine）
  - `func newResponseID() string` → `"resp_"` + 32 位十六进制（`crypto/rand`）

- [ ] **Step 1: 写失败测试**

`TestResponseState_StoreDefaultIsTrue`（**Review Focus 1**）—— 请求体**不含** `store` 字段，断言落库一行（`store.GetResponseState` 可查到）且响应回显 `"store": true`。

`TestResponseState_StoreFalseDoesNotPersist` —— `store:false` 断言不落库。

`TestResponseState_StorageDisabledByConfig`（**Review Focus 1**）—— `SetResponseStorage(false)` 后：不传 `store` 的请求响应回显 `"store": false` 且不落库；显式 `store:true` 断言 **400**。

`TestResponseState_PreviousResponseIDContinues` —— 第一轮创建后，第二轮带 `previous_response_id` 请求，断言 mock 上游收到的 `messages` **包含第一轮的对话内容**（历史被正确拼入）。

`TestResponseState_ConversationContinues` —— 两轮带同一 `conversation`，断言第二轮带上第一轮历史。

`TestResponseState_BothIDAndConversationRejected` —— 同时传两者断言 400。

`TestResponseState_ExpiredIs404` —— 手动把行 `CreatedAt` 改到保留期外并 `Prune`，再用该 id 断言 404。

`TestResponseState_UpstreamStoreForcedFalse` —— 上游配 `openai-responses`，断言网关转发给上游的 body 里 `store` 为 `false`（即使客户端传了 `true`）。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/proxy/ -run 'TestResponseState' -v`
Expected: FAIL。

- [ ] **Step 3: 实现**

- `responses_ingress.go`：按 Spec §5.6「请求侧行为」表实现 `store` 取舍、`previous_response_id` 拼接、`conversation` 拼接、二者互斥 400；响应 `id` 改写为网关 `resp_`；落库时机为**响应完成后**（流式则为流结束）。
- owner 校验：`GetResponseState` 后在 handler 内比较 `owner_user_id`，失配与不存在**返回同一个** 404 文案（同一常量，避免时序/文案差异泄露存在性）。
- `config.go`：`-allow-response-storage`（`AIGATE_ALLOW_RESPONSE_STORAGE`，默认 `true`）、`-response-retention-days`（`AIGATE_RESPONSE_RETENTION_DAYS`，默认 `7`，非法值拒绝启动）。
- `main.go`：调用 `SetResponseStorage`，并在保留期 > 0 时启动清理 goroutine（形态照 `event/dispatcher.go:167-197` 的 `pruneLoop`：启动先清一次 + `time.NewTicker(time.Hour)` + 失败只 `slog.Warn`）。

- [ ] **Step 4: 运行确认通过**

Run: `go test ./internal/proxy/ -race -run 'TestResponseState' -v && go build ./...`
Expected: 全部 PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/proxy internal/config cmd/aigate
git commit -m "feat(proxy): support store, previous_response_id and conversation"
```

---

### Task 9: 有状态 — `background` 与 `GET`/`DELETE`/`cancel`

**Files:**
- Modify: `internal/proxy/responses_ingress.go`, `internal/proxy/proxy.go:48-54`
- Modify: `internal/config/config.go`（`-response-background-timeout`）
- Test: `internal/proxy/responses_background_test.go`（package `proxy_test`）

**Interfaces:**
- Consumes: Task 8 的存储与 `resp_` id
- Produces:
  - 路由 `GET /v1/responses/{id}`、`DELETE /v1/responses/{id}`、`POST /v1/responses/{id}/cancel`
  - `Handler` 字段 `responseBackgroundTimeout time.Duration`

- [ ] **Step 1: 写失败测试**

`TestResponseBackground_ReturnsQueued` —— `background:true` 断言立即 200 且 `status=="queued"`。

`TestResponseBackground_PollUntilCompleted` —— 轮询 `GET /v1/responses/{id}` 直到 `status=="completed"`，断言最终 `output` 内容正确（用带延迟的 mock 上游模拟耗时）。

`TestResponseBackground_CancelNotOverwritten`（**Review Focus 6**）—— 任务进行中调用 `cancel`，等足够长时间后断言状态**仍为** `cancelled`，不会被后台任务写成 `completed`。

`TestResponseBackground_Delete` —— `DELETE` 断言返回 `{"deleted":true}`，随后 `GET` 断言 404。

`TestResponseBackground_CrossUserForbidden` —— 他人密钥访问 id 断言 404。

`TestResponseBackground_StaleInProgressMarkedFailed` —— 手动插入一条超时的 `in_progress` 行，触发清理，断言变为 `failed`。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/proxy/ -run 'TestResponseBackground' -v`
Expected: FAIL —— 路由不存在/状态不变。

- [ ] **Step 3: 实现**

- `background:true`：落 `queued` 行 → 立即返回 → goroutine 执行（`context.WithTimeout(responseBackgroundTimeout)`）→ **只在跃迁时** `UpdateResponseStateStatus`。
- 后台任务结束前**重新读取**当前状态，若已是 `cancelled` 则不再写终态（即使 `UpdateResponseStateStatus` 的条件更新已能兜底，仍要避免无谓写）。
- `GET`/`DELETE`/`cancel` 三个 handler 均先做 owner 校验（与 Task 8 同一函数）。
- `cancel`：`UPDATE ... WHERE status IN ('queued','in_progress')`。
- 启动清理时把超时 `in_progress`/`queued` 标为 `failed`（原因 `"interrupted by restart"`）。
- `Register` 追加三条路由。

- [ ] **Step 4: 运行确认通过**

Run: `go test ./internal/proxy/ -race -run 'TestResponseBackground' -v`
Expected: 全部 PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/proxy internal/config
git commit -m "feat(proxy): add background mode and response lifecycle endpoints"
```

---

### Task 10: 契约与文档

**Files:**
- Modify: `docs/openapi.yaml`（4 处 enum + 新增路径）
- Modify: `docs/API.md`, `docs/API.zh-CN.md`
- Modify: `CHANGELOG.md`
- Modify: `README.md`, `README.zh-CN.md`（可选示例）

**Interfaces:**
- Consumes: Task 1-9 的全部对外行为
- Produces: 无

- [ ] **Step 1: 更新 openapi**

4 处 `enum: [openai-completions, anthropic-messages]`（`docs/openapi.yaml:1962`、`:2015`、`:2053`、`:2896`）追加 `openai-responses`；新增 `/v1/responses`、`/v1/responses/{id}`（GET/DELETE）、`/v1/responses/{id}/cancel` 四条路径定义。

- [ ] **Step 2: 更新 API 文档**

`docs/API.zh-CN.md` 与 `docs/API.md` 同步新增：端点章节（含请求/响应/流式示例）、`model_type` 枚举表加第三值、URL 拼接三条规则、**`store` 缺省 `true` 的警示**、不支持的能力清单（内置工具）、三个新配置项。

- [ ] **Step 3: 更新 CHANGELOG**

按 Keep a Changelog 在 `[Unreleased]` 的 `Added` 下记录：新协议、9 格矩阵、有状态能力、三个配置项。

- [ ] **Step 4: 验证**

Run: `python3 -c "import yaml,sys; yaml.safe_load(open('docs/openapi.yaml'))" && grep -c 'openai-responses' docs/openapi.yaml`
Expected: YAML 解析成功；`openai-responses` 出现 **4** 次（enum）。

- [ ] **Step 5: Commit**

```bash
git add docs CHANGELOG.md
git commit -m "docs: document Responses API endpoints and openai-responses type"
```

---

### Task 11: 全量验收

**Files:**
- Test: `test/e2e/e2e_test.go`（追加 Responses 用例）

**Interfaces:**
- Consumes: 全部
- Produces: 无

- [ ] **Step 1: 跑 CI 口径**

Run:
```bash
test -z "$(gofmt -l cmd internal test)" && go build ./... && go vet ./... && go test -race ./... -count=1
```
Expected: 全部通过，gofmt 无输出。

- [ ] **Step 2: 补 e2e 用例**

在 `test/e2e/e2e_test.go` 增加一条 Responses 用例：经真实网关创建 `openai-responses` 模型 → 调 `/v1/responses` → 断言成功并落库（直连 MySQL 查 `response_states`）。

- [ ] **Step 3: 跑 e2e（需本地 MySQL）**

Run: `make test-e2e`
Expected: 全绿。

- [ ] **Step 4: Commit**

```bash
git add test/e2e
git commit -m "test(e2e): cover Responses API end to end"
```