# AIGate Responses API 全量矩阵与有状态支持 设计

日期：2026-11-20
状态：待评审

## 1. 意图与为什么

AIGate 目前只支持两种上游协议：`openai-completions` 与 `anthropic-messages`
（`internal/protocol/protocol.go:20-25`）。OpenAI 的 Responses API
（`POST /v1/responses`，2025-03-11 发布）已成为**新项目的官方推荐入口**，
并且是推理模型、内置工具、跨厂商标准化（Open Responses 规范）的落地方向。
官方明确 Chat Completions **未被废弃**、会继续支持，但新能力只优先在
Responses 上提供。

因此本次要做的是**新增第三种协议，而不是替换现有两种**，并且要求
**上下游全量矩阵**：

- **下游（客户端入口）**：新增 `POST /v1/responses`，客户端可用 Responses 协议调用网关。
- **上游（模型提供方）**：新增 `openai-responses` 上游类型，模型可配置为直连
  Responses 上游。
- **全量矩阵**：任意入口 × 任意上游的组合都必须可用，共 9 格。
- **有状态**：支持 `previous_response_id` / `store` / `conversation` / `background`。

## 2. 目标 / 非目标 / 反目标

### 目标

1. 数据面新增 `POST /v1/responses`（含流式），复用现有鉴权、配额、用量记账链路。
2. `ai_models.model_type` 新增取值 `openai-responses`，并实现该上游适配器。
3. **9 格全量矩阵**全部可用，不出现「某入口 + 某上游」组合直接 500/静默走错协议。
4. 有状态能力：
   - `store:true` 持久化响应（供 `previous_response_id` 引用）；
   - `previous_response_id` 续接对话；
   - `conversation` 会话对象；
   - `background:true` 异步任务 + 轮询取回；
   - `GET/DELETE /v1/responses/{id}`、`POST /v1/responses/{id}/cancel`。
5. 契约同步：`docs/openapi.yaml`、`docs/API.md`、`docs/API.zh-CN.md`、`CHANGELOG.md`。

### 非目标（本次不做）

- **不实现内置工具执行**（`web_search` / `file_search` / `code_interpreter` /
  `computer_use` / `image_generation` / `mcp`）。收到即 `400`，明确不支持，
  不静默丢弃。理由：AIGate 定位是转发与计量，不自带工具执行环境。
- **不做 `/v1/responses/compact`、`/input_tokens`、WebSocket 入口**（LiteLLM 有，
  但非 Responses 协议核心，且与本次目标无关）。
- **不做加密 reasoning item 的跨凭据亲和**（LiteLLM 的 `encitem_` / 
  `litellm_enc:` 机制）。本次规定：响应链必须落在同一上游凭据上，
  违反时由上游自然报错并透传。
- 不改 `/v1/chat/completions` 与 `/v1/messages` 的现有对外契约。

### 反目标（做成了也算失败）

- 某个入口打到某个上游时**静默按错误协议转发**（当前 `GetProvider` 的
  `default` 落 OpenAI 正是这个隐患，`internal/provider/provider.go:177-184`）。
- 有状态能力依赖**额外基础设施**（Redis 等）才能启动，破坏「单二进制自托管」。
- 转换丢字段却不报错：Responses 专有字段无法翻译时应显式 `400`，
  参照 LiteLLM `_raise_untranslatable_responses_fields`（`litellm/responses/main.py:742-756`）。
- 用量记成 0：Responses 的 `input_tokens`/`output_tokens` 与现有
  `extractOpenAIUsage` 读的 `prompt_tokens`/`completion_tokens` 字段名不同，
  必须独立提取器（`internal/provider/openai.go:213-251`）。

## 3. 约束（来自仓库现状）

- Go 1.24，模块名 `aigate`，现仅 4 个直接依赖（`go.mod`），**当前无 Redis 客户端**。
- 每个 `.go` 文件以 13 行 Tencent Apache-2.0 头注释开头；注释中文、对外错误文案英文；
  日志 `log/slog` + 结构化字段；导入 `aigate/internal/...`。
- SQLite 启动 `AutoMigrate`；MySQL 不自动迁移，必须同步 `sql/init.sql`
  （全新安装的单一事实源）。
- 出站必须经 SSRF 防护：`validateOutboundURL` + `ssrfSafeDialContext`
  + 重定向校验（`internal/provider/provider.go:445-558`）。
- 已有可复用模式：
  - 异步写队列：`internal/proxy/usagelog.go` 的 `UsageWriter`（队列满即丢弃）。
  - 定时清理：`internal/event/dispatcher.go:167-197` 的 `pruneLoop`，
    保留期来自配置（`AIGATE_WEBHOOK_DELIVERY_RETENTION_DAYS`）。
  - 全局配置单行表：`internal/model/settings.go` 的 `Setting`。
- CI 口径：`gofmt -l cmd internal test` 为空、`go build ./...`、`go vet ./...`、
  `go test -race ./... -count=1`。

## 4. 方案（Approaches）

### 4.1 全量矩阵如何实现：三个方案

**方案 A（选定）：以 `Provider` 接口为唯一的转换边界**

先说清楚**代码现状的真实拓扑**（这一节曾写错，已按代码订正）：

- `/v1/chat/completions`（`internal/proxy/proxy.go:55-146`）**不经入口侧转换**：
  `prepareForward` 只替换 `model`、压 `max_tokens`（`:183-190`），随后原样交给
  `provider.GetProvider(resolved.ModelType)`（`:88`）内部的
  `ChatCompletion` / `ChatCompletionStream`。
- `/v1/messages`（`internal/proxy/anthropic_ingress.go:168-226`）是**入口侧先转换**：
  `anthropicRequestToOpenAI`（`:207`）→ `prepareForward`（`:214`）→
  **再按上游类型二选一**（`:221-225`）：anthropic 上游走原生直通
  （`forwardAnthropicPassthrough`，用入站原始 body），其余走上游转换
  （`forwardViaOpenAIConversion`，`:358`，内部再经 `GetProvider`）。

即：**不存在一个全局统一的"chat 中间形态"管道**；真正的收敛点是
`Provider` 接口——它的契约是「无论上游是什么协议，都吐 OpenAI-chat 形状」
（`internal/provider/provider.go:119-153`，且已被 `AnthropicProvider` 兑现：
`internal/provider/anthropic.go:695-730` 内部 `buildAnthropicRequest` +
`anthropicToOpenAI`）。

因此本方案的实施要点是：**让每个入口都按 `/v1/messages` 的既有形态实现**
（入口负责协议转换，上游负责协议适配，两段之间以 chat 形状交接）。

- 需要新写的转换器仍是 **3 个**，但分布是：
  1. responses 入口侧：Responses ↔ chat（新增 `proxy/responses_convert.go`）
  2. responses 上游侧：chat ↔ Responses（新增 `provider/responses.go`）
  3. responses 入口 × responses 上游：直通复用（无需转换器，见 §5.4 格 7）
- 与 LiteLLM 的实际实现一致：provider 没有原生 Responses 适配器就桥到 chat
  （`litellm/responses/main.py:650` `_bridges_to_chat_completions`）。

**方案 B：9 格各写一个转换器**
- 优点：每格可针对性优化保真度。
- 缺点：6 个新转换器，组合爆炸，测试矩阵 9 倍；接口漂移风险高。**否决**。

**方案 C：只做 responses 入口 + responses 上游一条直线**
- 缺点：不满足「全量矩阵」要求。**否决**。

### 4.2 有状态存储：复用 MySQL/SQLite（选定），不引入 Redis

**结论：用现有 MySQL（生产）/ SQLite（开发）存储会话状态，不引入 Redis。**
只需 1 个新配置项（保留期），而不是 Redis 的 6~8 个连接参数。

**决定性证据**——强制 Redis 的实现都是**架构上开不了 DB 连接**，不是 Redis 更优：

| 实现 | 选择 | 原因 |
|---|---|---|
| LiteLLM | 强依赖 Redis，无降级 | 多 pod proxy，会话须跨 pod 共享；`should_use_polling_for_request` 首道门即 `and redis_cache`（`litellm/proxy/response_polling/polling_handler.py:376`），无 Redis 时 GET 直接 500（`endpoints.py:887-891`） |
| Higress `ai-history` | 强制 Redis | Envoy + Wasm 沙箱内无法持有 DB 连接 |
| vLLM semantic-router | **默认 memory**，可选 redis/milvus | 单进程自托管形态 |
| winsurfapi | **纯进程内存储** | 单进程自托管形态 |

AIGate 是**单二进制单进程自托管**，既不是多 pod，也不是 Wasm 沙箱，两条强制 Redis 的理由都不成立。

**为什么不用纯内存**：进程重启后所有 `previous_response_id` 直接 404。升级、OOM、扩容都是日常操作，这对网关是功能级不可用。纯内存只用于测试/演示。

**存储形态：累积快照，不是父指针链**

每行存**该轮完整累积的对话内容**，而不是只存 `previous_response_id` 指针。理由：
- 解析恒为 **O(1) 单行点查**；父指针链需要递归回放，LiteLLM 的做法是
  `WITH` 查询拉回整段会话**全部行**再逐行解析（`session_handler.py:712-742`），
  第 N 轮要拉 N-1 行大字段，整体 O(N²) I/O。
- 中途某行被 TTL 清掉时，要么全有、要么明确失败，**不会静默截断历史**。

代价是单行更大（每行含全部历史），由保留期与清理任务控制。

**归属校验（安全红线）**

`resp_...` 必须校验属于调用方。

**选定策略：按 `user_id` 放宽，`api_key_id` 仅记录作审计。**

- 校验条件：`owner_user_id == 调用方 user_id`。失配一律 **404**
  （不区分「不存在」与「不属于你」，避免枚举探测）。
- `owner_api_key_id` 写入但不参与校验。
- **理由**：仓库有密钥 rotate 功能（`internal/userapi` 的 `/api/user/keys` rotate），
  若按 `api_key_id` 严格校验，用户轮换密钥后**既有会话全部 404**——这对
  「同一用户在多个密钥/多个客户端间续接对话」的正常用法是破坏性的。
  而按 `user_id` 校验已满足真正的安全边界：**跨用户不可读**。
- 该选择与 LiteLLM 不同（它按 `user`/`team`/`api_key` 三元组，因为它是多租户 SaaS），
  但 AIGate 的租户边界就是 `user`。

LiteLLM 的对应注释：*"The session id can be caller supplied, so matching on it alone
would mix owners."*（`session_handler.py:689-692`）——**"不能只按 id 校验"这条必须遵守**，
但「按哪些键」按上方的 AIGate 语义确定。

**计费口径（与现有 `Usage` 字段对应）**

Responses 的 usage 映射到现有 `provider.Usage`（`internal/provider/provider.go:34-40`）
与 `LLMUsageLog`（`internal/model/usage.go:27-43`）时：

| Responses | `provider.Usage` | `LLMUsageLog` |
|---|---|---|
| `usage.input_tokens` | `PromptTokens` | `PromptTokens` |
| `usage.output_tokens` | `CompletionTokens` | `CompletionTokens` |
| `usage.total_tokens` | `TotalTokens` | `TotalTokens` |
| `usage.input_tokens_details.cached_tokens` | `PromptCacheReadTokens` | `PromptCacheReadTokens` |
| `usage.input_tokens_details.cache_write_tokens` | `PromptCacheWriteTokens` | `PromptCacheWriteTokens` |
| `usage.output_tokens_details.reasoning_tokens` | （无对应字段） | `TotalTokens` **不重复计入** |

**关键口径**：`input_tokens` **已包含** cached 部分（与 OpenAI 语义一致，
cache 读是一次便宜计费的 input token，不是额外 token）。因此
`PromptCacheReadTokens` 是 `PromptTokens` 的**子集**，仅作细分展示，
**配额计算不得把它与 `PromptTokens` 相加**——这与现有 OpenAI/Anthropic 路径的
既有口径一致（`anthropic.go:684-691` 的 `Prompt = input + cacheRead + cacheCreation`
是 Anthropic 的反向约定，两者不要混用）。

`reasoning_tokens` 是 `output_tokens` 的子集，只记入明细展示，
不额外叠加到 `TotalTokens`——同样遵循现有 `extractOpenAIUsage` 的处理方式
（`internal/provider/openai.go:213-251` 不读 `reasoning_tokens`）。

**background 状态机**

状态列只在**跃迁时** UPDATE（`queued→in_progress→completed|failed|cancelled`），
避免流式 chunk 高频改同一行造成行锁热点。更新语句必须带
`WHERE status <> 'cancelled'`，防止终态覆盖取消态——LiteLLM 为此专门写了 Lua 脚本
（`polling_handler.py:37-46`），我们用 SQL 条件更新即可达到同一效果。

**清理**

复用 `internal/event/dispatcher.go:167-197` 的 `pruneLoop` 形态（启动先清一次 +
`time.NewTicker(1h)` + 失败只 `slog.Warn` 不阻塞），新增配置
`-response-retention-days` / `AIGATE_RESPONSE_RETENTION_DAYS`（默认 7，
对齐 `-webhook-delivery-retention-days`，0=永不清理）。

**未选的备选方案**

- *纯 Redis*：新增基础设施依赖，破坏单二进制定位；且 Redis 挂掉时
  `/v1/chat/completions` 仍正常而 Responses 有状态失效，形成"部分功能静默失效"的复合故障态。
- *MySQL + Redis 双写*：复杂度最高，需处理"Redis 有而 DB 无"的分叉，
  而 Redis 带来的收益仅剩"多实例共享读缓存"——在多实例场景下读 MySQL 也已足够快。
- *复用 `LLMUsageLog` 表存对话*（LiteLLM 的做法）：该表当前只有 token 计数、
  不存对话正文（`internal/model/usage.go:27-43`），复用会污染审计表语义。新建专用表。

### 4.3 流式协议差异

LiteLLM 源码确证（`litellm/proxy/proxy_server.py:11370` 原注释）：
Chat Completions 流以 `data: [DONE]` 结束，**Responses 流不发 `[DONE]`**，
终止于 `event: response.completed`。

本次采纳该行为：`/v1/responses` 只输出成对的 `event: <type>` + `data: {...}`，
不追加 `data: [DONE]`。

## 5. 详细设计

### 5.1 协议常量与分发

**必改清单（缺任一项功能都不可用）**

| # | 位置 | 改动 |
|---|---|---|
| 1 | `internal/protocol/protocol.go:20-25` | 新增 `TypeOpenAIResponses = "openai-responses"` |
| 2 | `internal/model/ai_model.go:31-34` | 新增别名 `ModelTypeResponses = protocol.TypeOpenAIResponses`（列定义 `:46` 的 `size:32` 已够，无需改） |
| 3 | `internal/provider/provider.go:177-184` | `GetProvider` 新增显式 case |
| 4 | `internal/admin/models.go:357-360` `validModelType` | 加 `openai-responses`，否则**管理面创建不出该类型模型** |
| 5 | `internal/admin/models.go:283` / `:338`、`import.go:341` | 三处英文错误文案改为含第三个取值 |
| 6 | `internal/model/model.go:97-111` `AutoMigrate` | 加 `&ResponseState{}` |
| 7 | `internal/proxy/proxy.go:48-54` `Register` | 新增 `/v1/responses`、`GET/DELETE /v1/responses/{id}`、`POST /v1/responses/{id}/cancel` |
| 8 | `cmd/aigate/main.go:139` | 装配摘要为 `proxy.NewHandler(...)` 已覆盖；但**新增的保留期清理 goroutine 需在此启动** |
| 9 | `sql/init.sql` | 新增 `CREATE TABLE IF NOT EXISTS response_states` |
| 10 | `docs/openapi.yaml` 4 处 enum + 新路径 | 见 §5.7 |

`GetProvider` 改为：

```go
func GetProvider(modelType string) Provider {
	switch modelType {
	case protocol.TypeAnthropicMessages:
		return &AnthropicProvider{}
	case protocol.TypeOpenAIResponses:
		return &OpenAIResponsesProvider{}
	default: // openai-completions 及历史空值
		return &OpenAIProvider{}
	}
}
```

> `default` 保留 OpenAI 是为了兼容历史数据（列为空/未知值），
> 但**新增的 `openai-responses` 必须有显式 case**，否则会静默走错协议。

**路径前缀说明**：`openai-responses` 的 `url` 语义与 `openai-completions` 一致
（含 `/v1`），转发时拼 `/responses`；`anthropic-messages` 的 `url` 为 host，
拼 `/v1/messages`。三者不一致，文档 §5.7 需逐条写清。

### 5.2 上游适配器 `internal/provider/responses.go`

实现 `Provider` 接口三方法，内部完成：chat 请求 → Responses 请求、
Responses 响应/流 → chat 形状。复用 `upstream.go` 的
`newUpstreamRequest` / `doUpstream` / `setSSEHeaders` / `forwardUpstreamError` /
`ReadUpstreamBody` / `newProbeRequest` / `runConnectivityProbe`。

URL 拼接：`<url>/responses`（`url` 含 `/v1`），与 `openai-completions`
拼 `/chat/completions` 对称。

### 5.3 字段映射

§5.3 包含**两个方向的映射**，分属两个转换器，不可混用：

- §5.3.1 / §5.3.2：**`proxy/responses_convert.go`** 使用——Responses 入口侧。
- §5.3.3：**`provider/responses.go`** 使用——Responses 上游侧，
  因为 `Provider` 接口的契约是「恒吐 chat 形状」，这里必须是
  **Responses → chat** 方向，而不是相反。

#### 5.3.1 请求方向 responses → chat

| Responses | Chat Completions |
|---|---|
| `input`（string） | `messages:[{role:"user",content}]` |
| `input`（item[]） | 逐项转 message |
| `instructions` | 头部 `{role:"system",content}` |
| item `role=system/developer` | 同 role 的 message（原样保留） |
| content part `input_text` | `{type:"text"}` |
| content part `input_image` | `{type:"image_url"}` |
| content part `input_file` | `{type:"file"}` |
| item `function_call`（`call_id,name,arguments`） | assistant `tool_calls[]{id,function:{name,arguments}}` |
| item `function_call_output`（`call_id,output`） | `{role:"tool",tool_call_id:call_id,content}` |
| `max_output_tokens` | `max_tokens` |
| `text.format` | `response_format` |
| 扁平 tool `{type:function,name,description,parameters,strict}` | 嵌套 `{type:function,function:{...}}` |
| `tool_choice` | `tool_choice` |
| `temperature`/`top_p`/`parallel_tool_calls`/`metadata` | 同名透传 |
| `stream` 为真 | 追加 `stream_options:{include_usage:true}` |

#### 5.3.2 响应方向 chat → responses（供 Responses 入口回给客户端）

| Chat Completions | Responses |
|---|---|
| `choices[0].message.content` | `output:[{type:"message",role:"assistant",content:[{type:"output_text",text,annotations:[]}]}]` |
| `choices[0].message.tool_calls[]` | `output:[{type:"function_call",call_id:tool.id,name,arguments}]` |
| `finish_reason="length"` | `status:"incomplete"` + `incomplete_details:{reason:"max_output_tokens"}` |
| `usage.prompt_tokens` | `usage.input_tokens` |
| `usage.completion_tokens` | `usage.output_tokens` |
| `usage.total_tokens` | `usage.total_tokens` |
| `prompt_tokens_details.cached_tokens` | `input_tokens_details.cached_tokens` |
| `completion_tokens_details.reasoning_tokens` | `output_tokens_details.reasoning_tokens` |
| `id`（`chatcmpl-...`） | 网关生成 `resp_...` |

#### 5.3.3 响应方向 responses → chat（供 `Provider` 接口，**这是 chat 入口 × responses 上游所需**）

`OpenAIResponsesProvider` 收到上游 Responses 响应后，必须转成**纯 chat JSON**
（`proxy.go:142-144` 会 `WriteHeader(statusCode)` 后原样写 `result.Body`，
夹带 responses 专有字段会破坏 chat 入口契约）：

| Responses | Chat Completions |
|---|---|
| `output[]` 中 `type=="message"` 的 item，取其 `content[]` 里 `type=="output_text"` 的 `text` 拼接 | `choices[0].message.content` |
| `output[]` 中 `type=="function_call"` 的 item | `choices[0].message.tool_calls[]{id:call_id,type:"function",function:{name,arguments}}` |
| `status=="completed"` 且含 tool_call | `finish_reason="tool_calls"` |
| `status=="completed"` 且为文本 | `finish_reason="stop"` |
| `status=="incomplete"` 且 `incomplete_details.reason=="max_output_tokens"` | `finish_reason="length"` |
| `usage.input_tokens` | `usage.prompt_tokens` |
| `usage.output_tokens` | `usage.completion_tokens` |
| `usage.total_tokens` | `usage.total_tokens` |
| `usage.input_tokens_details.cached_tokens` | `usage.prompt_tokens_details.cached_tokens` |
| `id`（`resp_...`） | 原样或生成 `chatcmpl-...`（不对外暴露，见 §5.6 安全） |

**流式**：上游 Responses SSE 事件 → chat chunk（`chat.completion.chunk` +
`data: [DONE]`），与 `AnthropicProvider` 对 SSE 的处理形态一致
（`internal/provider/anthropic.go:754-813` 的 `anthropicStreamTranslator`）。
`response.output_text.delta` 的 `delta` → `choices[0].delta.content`；
`response.completed` 的 `usage` 作为最后一条带 usage 的 chunk。
**注意**：这条路径的 `[DONE]` 是给 chat 入口客户端的，与 §4.3
「Responses 入口不发 `[DONE]`」不矛盾——两条路的客户端协议不同。

### 5.4 9 格矩阵的逐格实现路径

这是本次的核心交付。转换边界是 `Provider` 接口，故多数格无需新增转换器：

| # | 入口 | 上游 | 实现路径 | 新增代码 |
|---|---|---|---|---|
| 1 | chat | chat | 直通（现状） | 无 |
| 2 | chat | anthropic | `GetProvider` → `AnthropicProvider` 内部转换（现状） | 无 |
| 3 | chat | **responses** | `GetProvider` → `OpenAIResponsesProvider`，内部 chat→Responses 请求、Responses→chat 响应（§5.3.3） | **`provider/responses.go`** |
| 4 | messages | anthropic | 直通 `forwardAnthropicPassthrough`（现状） | 无 |
| 5 | messages | chat | `forwardViaOpenAIConversion`（现状） | 无 |
| 6 | messages | **responses** | 走 `forwardViaOpenAIConversion` → `GetProvider` 获新 provider → 自动获得格 3 的转换 | 无（随格 3 生效） |
| 7 | **responses** | **responses** | **原生直通**（见 §5.4.2） | `responses_ingress.go` 直通分支 |
| 8 | **responses** | chat | `responses_convert.go` 转成 chat 请求 → `OpenAIProvider` → 响应/流转回 Responses（§5.3.1+5.3.2） | **`proxy/responses_convert.go`** |
| 9 | **responses** | anthropic | 同格 8 的入口转换 → `AnthropicProvider`（内部 chat→Anthropic）→ 响应转回 Responses | 无（复用格 8 的转换器） |

**格 6 与格 9 零新增代码**，这是 `Provider` 作为唯一转换边界的直接红利；
对比「9 格各写一个转换器」需 6 个新转换器。

#### 5.4.1 responses 入口的分派结构

仿照 `/v1/messages` 的既有形态（`anthropic_ingress.go:221-225`），
`responses_ingress.go` 的 `handleResponses` 流程：

1. 鉴权（复用 `h.authKey`，Bearer）；
2. 读 body、解析 `model` / `stream` / `store` / `previous_response_id` / `conversation` / `background`；
3. **有状态前置**：若带 `previous_response_id` 或 `conversation`，先查
   `responsestore` 取历史并校验 owner（§5.6）；失败 `404`；
4. 构造 chat 形态请求（含历史前缀）→ `prepareForward`（复用现有：模型解析、
   配额、`max_tokens` 压制）；
5. **按 `resolved.ModelType` 二选一**：
   - `openai-responses` → `forwardResponsesPassthrough`（格 7，§5.4.2）
   - 其余 → `forwardViaChatConversion`（格 8/9，`GetProvider` 内部处理上游差异）
6. 收尾：`logUsage` + 若 `store` 则落库，生成 `resp_...` 并改写响应里的 `id`。

> 第 4 步注意：`prepareForward` 会注入/压制 `max_tokens`。Responses 协议的
> 对应字段是 `max_output_tokens`，入口侧转换时须在调用 `prepareForward` **之前**
> 把 `max_output_tokens` 映射为 `max_tokens`，回包时再映射回去；
> 或在 `enforceMaxTokens` 增加对 `max_output_tokens` 的支持。**实现时二选一，
> 推荐后者**（一处改动，三个入口同时受益）。

#### 5.4.2 格 7（responses 入口 × responses 上游）：直通 vs 往返

**选定：原生直通。** 理由：

- 往返转换（responses→chat→responses）会丢失 `reasoning` item、
  `encrypted_content`、内置工具输出、`annotations` 等 Responses 专有结构，
  并使 `response.created` 的 `output[]` 结构失真——**违反 §2 反目标
  「转换丢字段却不报错」**。
- 直通路径需要额外处理的只有四件事：
  1. 请求体替换 `model`、压制 `max_output_tokens` 为密钥上限（与
     `buildAnthropicPassthroughBody` 对称，`anthropic_ingress.go:327-344`）；
  2. `store` 与 owner 校验在网关侧完成（上游的 `store` 语义由网关接管，
     转发时强制改写为 `store:false`，避免把数据同时留存在上游）；
  3. SSE 透传时**原样转发 `event:`/`data:` 行**，仅在终态事件
     `response.completed` 内改写 `id` 为网关的 `resp_...`，并抓取 `usage` 记账；
  4. 若上游未发 `[DONE]`（OpenAI 原生就不发），本路径也不补发。

因此**只需一套 SSE 状态机**（§5.5 的 chat→responses 合成器），
直通路径不需要第二套：它只做「透传 + 终态事件改写」。

#### 5.4.3 直通时的元数据改写边界

直通要改写 `response.completed` 内的字段，需解析该事件的 `data: {...}`。
为避免破坏大响应，采用**流式扫描**：逐行读 SSE，只对 `event: response.completed`
那一行做 JSON 解析与 `id`/`usage` 提取，其余行原样 `io.Copy` 给客户端。

### 5.5 流式事件合成状态机

从 chat chunk 合成 Responses 语义事件（参照 LiteLLM
`litellm/responses/litellm_completion_transformation/streaming_iterator.py`）：

```
response.created        （首个 chunk 前补发，status=in_progress, output=[]）
response.in_progress
response.output_item.added
response.content_part.added
response.output_text.delta   ×N   （每个 content 增量一条）
response.output_text.done
response.content_part.done
response.output_item.done
response.completed       （携带完整 response 与最终 usage）
```

规则：

- `response.created` 与 `response.completed` 的 `id` 必须**同一个** `resp_...`；
- 首块缓冲：在确定上游返回 2xx 之前不写 SSE 头，避免「已开流才失败」
  （参照 LiteLLM `create_response`，`litellm/proxy/common_request_processing.py:1663`）；
- 流中错误输出 `event: error\ndata: {...}`，不追加 `[DONE]`；
- 上游不发 `[DONE]` 时也能正常收尾（以 `response.completed` 为界）。

### 5.6 有状态设计

**新表 `response_states`**（SQLite 由 `AutoMigrate` 建；MySQL 加到 `sql/init.sql`，
`CREATE TABLE IF NOT EXISTS`，参照现有 `webhook_deliveries` 模板）：

| 列 | 类型 | 说明 |
|---|---|---|
| `id` | `VARCHAR(64)` PK | `resp_` + 32 位随机十六进制 |
| `conversation_id` | `VARCHAR(64)`，索引 | `conversation` 参数对应的会话 ID；空串表示未加入会话 |
| `owner_user_id` | `BIGINT UNSIGNED`，索引 | 归属用户 |
| `owner_api_key_id` | `BIGINT UNSIGNED` | 归属密钥；与 `owner_user_id` 同进同出，用于双键校验 |
| `request_model` | `VARCHAR(191)` | 客户端请求的模型名（回显用） |
| `previous_response_id` | `VARCHAR(64)` | 本轮基于哪一条；空串表示对话起点（仅留作追溯） |
| `status` | `VARCHAR(16)` | `queued` / `in_progress` / `completed` / `failed` / `incomplete` / `cancelled` |
| `items` | `LONGTEXT` | **累积快照**：JSON 数组，完整的对话 items（输入 + 本轮输出） |
| `response_body` | `LONGTEXT` | 完整 Responses 响应 JSON（`GET /v1/responses/{id}` 原样返回） |
| `usage_*` | 四个 `INT` | input/output/total/reasoning token，便于排查 |
| `error` | `TEXT` | 失败原因（成功时为空串） |
| `created_at` / `updated_at` | `DATETIME(3)` | `created_at` 建索引供清理用 |

索引：`PRIMARY KEY(id)`、`KEY idx_response_states_created_at(created_at)`、
`KEY idx_response_states_owner(owner_user_id, owner_api_key_id)`、
`KEY idx_response_states_conversation(conversation_id)`。

**存储实现：放在 `internal/model` 包内**

原计划新建独立包 `internal/responsestore`，但 `model.Store.db` 是**未导出字段**
（`internal/model/model.go:41-43`），全包无 `DB()` 访问器（`grep 'func (s \*Store)'
internal/model/model.go` 仅有 `AutoMigrate`/`Ping`/`Close`）。包外拿不到 `*gorm.DB`。

两个选择：

- (a) 给 `model.Store` 加导出 `DB() *gorm.DB`——把 GORM 泄漏到包外，
  与 AGENTS.md「生产代码不直接访问数据库、不 import gorm」的边界约定冲突。
- **(b) 选定**：`ResponseState` 模型与存取方法直接写在 `internal/model` 包内，
  与 `Webhook`/`WebhookDelivery` 的既有做法完全一致。包内访问 `s.db`，无需导出器。
  后台清理逻辑（`pruneLoop` 形态）放在 `internal/proxy` 或 `cmd/aigate` 装配层，
  只调用 `store.PruneResponseStates(...)`。

对应 `internal/model/responses.go`（新文件）提供：

```go
// PruneResponseStates 删除创建时间早于 before 的行，返回删除行数。
func (s *Store) PruneResponseStates(ctx context.Context, before time.Time) (int64, error)
```

并新增 `ResponseState` GORM 模型 + `CreateResponseState` / `GetResponseState` /
`UpdateResponseStateStatus` / `ListResponseStateItems` 等方法。

**`AutoMigrate` 同步**：`internal/model/model.go:97-111` 是硬编码表列表，
必须把 `&ResponseState{}` 加进去，否则 SQLite 本地开发直接不可用。

**请求侧行为**

> **`store` 的缺省值是 `true`**，这点反直觉但必须照做——OpenAI 官方
> [数据控制文档](https://developers.openai.com/api/docs/guides/your-data) 原文：
> *"the Responses API has a 30 day Application State retention period **by default**,
> or when the `store` parameter is set to true"*；LangChain 参考亦写明
> *"Defaults to True for the Responses API and False for the Chat Completions API"*。
> **客户端不显式设置时就会落库**，因此必须在 `docs/API.zh-CN.md` 显著位置警示，
> 并提供站点级开关（见下方「隐私兜底」）。

| 客户端传入 | 行为 |
|---|---|
| `store` 缺省 / `true` | 落库，响应回显 `"store": true` |
| `store: false` | **不落库**；若同时带 `previous_response_id`，仍会读父行用于拼历史，但不产生新行（与 OpenAI 语义一致） |
| `previous_response_id` | `Get` 父行 → 校验 owner 双键（失配 404）→ 以父行 `items` 作为历史前缀 → 拼本轮 `input` |
| `conversation` | 取该会话最新一条 `completed` 行（按 `created_at DESC, id DESC` 稳定排序）的 `items` 作为历史前缀；提交成功后新行写入同一 `conversation_id` |
| `previous_response_id` 与 `conversation` 同时出现 | `400`（OpenAI 语义：二者互斥） |
| `background: true` | 见下方异步流程；响应立即返回 `status:"queued"` |
| 保留期外 / 已清理的 id | `404` |

**隐私兜底（新增站点级开关）**

因为缺省即落库，站点管理员必须能一票否决。新增配置：

- `-allow-response-storage` / `AIGATE_ALLOW_RESPONSE_STORAGE`（默认 `true`）。
  设为 `false` 时：所有请求按 `store:false` 处理，响应体回显 `"store": false`；
  显式传入 `store:true` 返回 `400`（明确拒绝而非静默改写，符合 §2 反目标）。
- 该开关同时决定 §5.6 中 `response_states` 是否写入。关闭后
  `previous_response_id` / `conversation` / `background` 一律 `400`。

**上游 `store` 转发规则**

无论网关是否落库，**转发给上游时一律强制改写为 `store:false`**：
避免同一份对话正文同时留在上游与网关两处（数据面最小化）。
这一条在直通路径（格 7）与转换路径（格 8/9）上行为一致。

**background 流程**

1. 入站：鉴权 → 配额 → 落一行 `status:"queued"` → **立即返回**该行响应体。
2. 后台 goroutine 发起上游请求，流式读取，**只在状态跃迁时** UPDATE：
   `queued→in_progress`，结束时 `→completed|failed`；期间累积 items 并在终态一次性写入。
3. `GET /v1/responses/{id}`：点查返回当前 `response_body`。
4. `POST /v1/responses/{id}/cancel`：`UPDATE ... SET status='cancelled' WHERE id=? AND status IN ('queued','in_progress')`；
   后台任务发现状态已被置 `cancelled` 就停止写回终态（条件更新已保证不会覆盖）。
5. `DELETE /v1/responses/{id}`：删除行，返回 `{"id":..., "object":"response.deleted", "deleted":true}`。

**并发与失败**

- 后台任务用 `context.WithTimeout` 限制总时长：新增
  `-response-background-timeout` / `AIGATE_RESPONSE_BACKGROUND_TIMEOUT`（默认 30 分钟）。
- **前台流式超时**：与 `anthropicStreamTimeout` 对齐（`internal/proxy/anthropic_ingress.go:48`
  的 `30 * time.Minute`），定义为 `responsesStreamTimeout = 30 * time.Minute`；
  非流式 `responsesUpstreamTimeout = 5 * time.Minute`（对齐 `:47`）。
- 进程重启导致 `in_progress` 残留：启动时把超过 `background-timeout` 的
  `in_progress`/`queued` 行标记为 `failed`（原因 `"interrupted by restart"`），
  在清理 goroutine 首次执行时顺带完成。

**安全**

- 所有 `GET`/`DELETE`/`cancel` 先校验 owner 双键；
- `resp_` 前缀避免与上游 `chatcmpl-`/`resp_` 混用：网关注入的 id 一律
  `resp_` + 32 hex，不直接透传上游 id（上游 id 记在 `response_body` 内的
  额外字段 `upstream_id`，不对外暴露）。

### 5.7 契约与文档

- `docs/openapi.yaml`：新增 `/v1/responses`、`/v1/responses/{id}`、
  `/v1/responses/{id}/cancel`；4 处 `model_type` enum 追加
  `openai-responses`（`:1962`、`:2015`、`:2053`、`:2896`）。
- `docs/API.md` / `docs/API.zh-CN.md`：新增端点章节、`model_type` 枚举、
  URL 拼接说明、能力限制清单。
- `CHANGELOG.md`：按 Keep a Changelog 记录。
- 前端（独立 worktree `worktree-webui`）`providers/index.tsx` 的 Select 增加选项。

## 6. 测试策略

- `internal/provider/responses_test.go`（黑盒）+ `responses_internal_test.go`（白盒）：
  字段映射、usage 提取、事件合成。
- `internal/proxy/responses_ingress_test.go`（黑盒）：鉴权、配额、无状态请求、
  有状态续接、流式事件序、错误体。
- 9 格矩阵表驱动测试：每个「入口 × 上游」组合各一条用例。
- `test/e2e`：真实网关 + mock Responses 上游。

## 7. 新增配置项

| flag | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `-allow-response-storage` | `AIGATE_ALLOW_RESPONSE_STORAGE` | `true` | 站点级开关。`false` 时全部按 `store:false` 处理，显式 `store:true` 返回 `400`，有状态能力整体不可用 |
| `-response-retention-days` | `AIGATE_RESPONSE_RETENTION_DAYS` | `7` | `response_states` 保留天数，`0` = 永不清理（对齐 `-webhook-delivery-retention-days`） |
| `-response-background-timeout` | `AIGATE_RESPONSE_BACKGROUND_TIMEOUT` | `30m` | `background:true` 任务总时长上限 |

全部按现有约定实现：flag > env > 默认，非法值拒绝启动
（`internal/config/config.go` 现有风格）。

## 8. 待决项

1. **`background` 是否需要跨实例存活**：本设计按「单实例自托管」实现（后台 goroutine
   存活于处理请求的进程）。多实例部署时，`background` 任务由接收请求的实例执行，
   状态写 MySQL；轮询打到别的实例也能读到正确状态（因为状态在 DB），但**任务本身
   不会迁移**。若将来需要任务级高可用，再引入独立 worker，不在本次范围。
2. **`conversation` 会话寿命**：当前设计下会话随最新一行被清理而自动失效。
   是否需要「会话本身有过期时间、独立于单行保留期」——若需要，要再加一列
   `conversation_expires_at`。本次先不做。