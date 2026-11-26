# API 参考

[English](API.md) | [简体中文](API.zh-CN.md)

Tencent Cloud AIGate 是自托管的 LLM API 网关：**纯 API 服务**（无前端界面）。对外提供四个层面——数据面、管理面、用户面，以及一个免鉴权的健康检查。

下文所有示例使用基础地址 `http://127.0.0.1:8080`。

## 1. 概览

### 层面与认证方式

| 层面 | 路径前缀 | 认证方式 | 凭据 |
|------|----------|----------|------|
| 数据面 | `/v1/*` | `Authorization: Bearer <token>`；`/v1/messages` 另接受 `x-api-key` | 数据面密钥（`sk-…`） |
| 管理面 | `/admin/*` | `Authorization: Bearer <token>` | `role=admin` 用户的访问 token（`gk-…`） |
| 用户面 | `/api/user/*` | `Authorization: Bearer <token>` | 任意启用用户的访问 token（`gk-…`） |
| 健康检查 | `/healthz` | 无 | — |
| 构建信息 | `/version` | 无 | — |

`Authorization` 头的值必须以 `Bearer ` 开头（管理面对 scheme 大小写不敏感；数据面要求精确的 `Bearer ` 前缀）。token 两端的空白会被去除。不接受来自查询串或其他头的 token。`/v1/messages` 上若同时携带 `x-api-key` 与 `Authorization`，以 `x-api-key` 为准。

用户面上，`user` 角色 token 被强制作用于本人数据；`admin` 角色 token 可用 `?user_id=` 指定其他用户。

### 错误响应格式

各平面的所有错误响应形状一致，`code` 等于 HTTP 状态码：

```json
{
  "error": {
    "message": "invalid API key",
    "type": "error",
    "code": 401
  }
}
```

唯一例外是数据面的 `/v1/messages`，它使用 Anthropic 错误信封（见该端点）。

错误体可选的还会在必填三键之外带补充键：配额拒绝额外给出 `unit`（`token` 或 `cost`），
金额口径再给出 `currency` —— 见 §3.10。

### 管理员 token 引导

启动时网关确保至少存在一个 `role=admin` 用户：

| 情形 | 行为 |
|------|------|
| 已存在任一管理员 | 不创建任何用户。若指定了 `-init-admin-token`，该参数被忽略并输出一条日志。 |
| 无管理员，且指定 `-init-admin-token <TOKEN>` | 以该 token 创建用户 `admin`。 |
| 无管理员，且未指定 token | 自动生成一个 token，并**向 stdout 打印一次**：`initial admin token (shown once): gk-…` |

### token 与密钥安全语义

| 规则 | 说明 |
|------|------|
| 存储 | 只持久化 SHA-256 哈希与展示前缀（形如 `sk-abcd1234…wxyz`），明文永不落库。 |
| 一次性展示 | 明文仅在创建与 rotate 两个时机返回一次，任何列表/详情接口都不会回显。 |
| 管理面 `api_key` | 上游凭据（`ai_models.api_key`）在任何响应中都不出现。 |
| 凭据密钥 | 凭据中的密钥值在每次读取时都以掩码返回（见 §3.2），明文绝不回显。 |
| webhook secret | webhook 的 secret 只写不读回：读侧恒返回掩码（见 §3.8）。 |
| rotate | rotate 会替换存储的 token 哈希与前缀，旧凭据立即失效。 |
| 回显控制 | 除创建/rotate 外，响应中的 `token` 字段经 `omitempty` 省略。 |

### 请求体限制

- 所有 JSON 请求体上限为 **1 MiB**。超限**直接拒绝、不做截断**：数据面返回 `413`，管理面与用户面返回 `400`，文案均为 `request body too large`。
- 写接口要求 JSON 对象请求体；空请求体返回 `400`：数据面文案为 `invalid json body`，管理面与用户面附带解析器原因（如 `invalid json body: EOF`）。

## 2. 数据面

### POST /v1/chat/completions

把 OpenAI 风格的对话补全请求转发给密钥绑定命中的上游模型。流式与非流式均支持；对
`anthropic-messages` 模型，请求与响应会与 Anthropic Messages API 互相转换。

**认证：** `Authorization: Bearer sk-…`

**请求字段**

请求体是一个 OpenAI Chat Completions 对象。网关只读取并改写下列字段，其余字段原样转发。

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `model` | string | 否 | 目标模型。存在且非空时必须命中该密钥绑定的模型（见解析规则）；缺失或为空串时使用绑定的 `primary`。 |
| `stream` | bool | 否 | 为 `true` 时走 SSE 流式。 |
| `max_tokens` | integer | 否 | 当密钥 `max_tokens` 上限 `> 0` 时被截断至该上限。 |
| `max_completion_tokens` | integer | 否 | 同样受上限截断。 |
| 其他任意字段 | — | 否 | 原样转发。 |

**网关改写（转发前）**

| 改写 | 规则 |
|------|------|
| 模型名替换 | body 中的 `model` 被替换为解析命中的上游 `model_id`（保留原始大小写）。客户端原值绝不原样转发。 |
| `max_tokens` 上限 | 仅在密钥 `max_tokens > 0` 时生效。若存在 `max_tokens` 与 `max_completion_tokens` 则一并封顶；若**两者都不存在**，则按上限注入 `max_tokens`。上限 `<= 0` 时 body 不做改动。模型级 `max_output_tokens` 是能力声明，从不压制请求。 |
| 参数归一化 | 对 OpenAI 类型上游，GPT-5+ 与 o 系列模型使用 `max_completion_tokens` 而非 `max_tokens`（其余模型相反）。 |
| 流式 | `stream: true` 时，OpenAI 类型上游还会收到 `stream: true` 与 `stream_options.include_usage: true`。 |
| 自定义请求头 | 见下文「自定义请求头透传」。 |

**模型解析规则**

| 客户端 `model` | 密钥绑定 | 结果 |
|----------------|----------|------|
| 存在，命中绑定的**启用**模型 | — | 用该模型的上游 `model_id` 转发。匹配大小写不敏感，且 `/`、`:` 与 `-` 等价；`provider/model_id` 前缀（如 `qcloudlkeap/deepseek-v3.2`）可精确指定不同来源的同名模型；前缀同样接受保存绑定时生成的拼接形态 `{provider}-{model_id}`（如 `hatchery-glm-4-plus/glm-4-plus`），按相同规则匹配。 |
| 存在但未命中 | — | `400 requested model is not bound to this key`，**不回退** primary。 |
| 缺失或空串 | 有 `role=primary` 绑定 | 使用 primary 模型转发。 |
| 缺失或空串 | 无 `primary`（或无启用绑定） | `400 no available model for this key`。 |

只有模型 `enabled=true` 的绑定参与解析。

同一套解析、配额与 `max_tokens` 逻辑同样作用于 `/v1/messages`。

**自定义请求头透传**

模型可声明 `custom_http_headers`（字符串值的 JSON 对象）。透传是全有或全无：

| 条件 | 行为 |
|------|------|
| 配置的每个头都在入站请求中**完全相等**地出现 | 这些头被转发到上游（头名做规范化）。 |
| 任一配置头缺失或值不同 | **不转发任何**自定义头，并记录一条告警。 |
| 模型未配置 `custom_http_headers` | 不做任何自定义头处理。 |

**请求示例**

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer <SK_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "glm-5.1",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": false
  }'
```

**响应 —— 非流式**

上游状态码与响应体**原样透传**（不重新包装）。因此 `200` 的内容就是上游返回的内容，例如：

```json
{
  "id": "chatcmpl-…",
  "object": "chat.completion",
  "created": 1730000000,
  "model": "glm-5.1",
  "choices": [
    {
      "index": 0,
      "message": {"role": "assistant", "content": "hi"},
      "finish_reason": "stop"
    }
  ],
  "usage": {"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7}
}
```

上游错误状态码（如 `400`、`429`）连同上游响应体同样原样透传。

**响应 —— 流式（`stream: true`）**

响应使用 `Content-Type: text/event-stream`，带 `Cache-Control: no-cache` 与
`X-Accel-Buffering: no`，并增量 flush。对 `anthropic-messages` 上游，Anthropic SSE
事件会被转成 OpenAI `chat.completion.chunk` 帧并以 `data: [DONE]` 收尾。若上游在流开始
前返回非 `200`，该状态码与响应体按 JSON 原样透传。用量在流结束后记录。

**错误**

| 状态码 | `message` | 原因 |
|--------|-----------|------|
| 401 | `missing API key` / `invalid API key` | 无 `Authorization` 头（missing）vs 格式错误、未知或已禁用（invalid）。 |
| 400 | `failed to read request body` | 请求体读取失败。 |
| 413 | `request body too large` | 请求体超过 1 MiB；直接拒绝，不截断。 |
| 400 | `invalid json body` | 请求体不是 JSON 对象。 |
| 400 | `requested model is not bound to this key` | 显式 `model` 未命中任何启用绑定。 |
| 400 | `no available model for this key` | 未提供 `model` 且无启用 `primary` 绑定。 |
| 429 / 403 | `Global token quota exceeded` / `Model daily token quota exceeded` / `User token quota exceeded` / `Key token quota exceeded` | 配额校验拒绝请求。状态码取 `-quota-exceeded-status`（默认 `429`），并带 `Retry-After` 头。响应体还标出计量口径：`unit`（`token` / `cost`），金额规则另带 `currency` —— 见 §3.10。 |
| 500 | `internal server error` | 模型解析失败（数据库错误）。 |
| 500 | `streaming not supported by server` | `stream: true` 但服务端无法 flush 响应。 |
| 502 | `failed to connect to LLM backend` | 非流式：到上游的 DNS/连接/TLS/超时失败。细节（含上游地址）只记服务端日志，不回显给调用方。 |

### POST /v1/messages

Anthropic Messages 入站。把 Anthropic 格式的请求转发给密钥绑定命中的上游模型：对
Anthropic 上游保留 Anthropic 语义，对 OpenAI 兼容上游做转换。支持流式与非流式、文本、
图片与工具调用。

**认证：** `x-api-key: sk-…`（优先）或 `Authorization: Bearer sk-…`。鉴权失败一律使用
Anthropic 错误信封。

**请求字段**

请求体是一个 Anthropic Messages 对象。网关只校验并改写下列字段。

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `model` | string | **是** | 必须非空。解析规则与 `/v1/chat/completions` 完全一致。 |
| `max_tokens` | integer | **是** | 必须为整数且 `> 0`。当密钥 `max_tokens` 上限 `> 0` 时被截断至该上限。 |
| `stream` | bool | 否 | 为 `true` 时走 Anthropic SSE 流式。 |
| `messages` / `system` / `tools` / `tool_choice` / `thinking` / `stop_sequences` | — | 否 | 见下方两条上游路径。 |
| 其他任意字段 | — | 否 | 直通路径保留；转换路径按可映射范围转发。 |

校验顺序：`model` 缺失 → `400 model is required`；`max_tokens` 缺失或 `<= 0` → `400 max_tokens is required`。

**上游路径**

命中模型的 `model_type` 决定走哪条路径：

| 上游 | 行为 |
|------|------|
| `anthropic-messages` | **原生直通。** 入站 body 原样转发，仅两处修改：`model` 替换为解析命中的上游 ID，`max_tokens` 施加密钥上限。因此 `cache_control` 等 Anthropic 专有字段端到端保留。上游响应（JSON 或 SSE）原样回传。 |
| `openai-completions`（及其它类型） | **转换。** Anthropic 请求转换为 OpenAI 请求（`system` → system 消息、图片 → data URI、`tool_use`/`tool_result` → `tool_calls`/`role=tool`，`tools`/`tool_choice`/`stop_sequences` 映射），OpenAI 响应或 SSE 流再回译为 Anthropic 格式。 |

直通请求头：`x-api-key`（解析出的上游密钥）、`anthropic-version`（入站值，缺省
`2023-06-01`）、`anthropic-beta`（存在时转发），以及命中的 `custom_http_headers`。

上游超时：非流式 5 分钟，流式 30 分钟。

**流式事件序列**

转换路径的 SSE 流是一条合法的 Anthropic 事件序列：

```text
message_start → content_block_start → content_block_delta* → content_block_stop
             → （后续 block）→ message_delta → message_stop
```

直通路径原样转发上游自身的 Anthropic 事件流。两条路径都在流结束后记录用量（输入/cache
计数取自 `message_start.message.usage`，输出取自 `message_delta.usage`）。

**响应 —— 非流式**

`200` 返回 Anthropic Messages 响应对象（直通路径原样，转换路径由 OpenAI 响应回译）。

**错误**

所有错误使用 Anthropic 信封：

```json
{
  "type": "error",
  "error": {"type": "invalid_request_error", "message": "max_tokens is required"}
}
```

| 状态码 | `error.type` | 原因 |
|--------|--------------|------|
| 401 | `authentication_error` | 密钥缺失或无效/已禁用。 |
| 400 | `invalid_request_error` | 请求体不可读、JSON 非法、`model` 缺失，或 `max_tokens` 缺失/为 0；也包括转换失败。 |
| 413 | `invalid_request_error` | 请求体超过 1 MiB；直接拒绝，不截断。 |
| 429 / 403 | `rate_limit_error` | 配额超限（状态码同 `-quota-exceeded-status` 配置）；带 `Retry-After` 头。 |
| 500 | `api_error` | 内部错误，或服务端不支持流式。 |
| 502 | `api_error` | 无法连接上游，或上游响应无法转换。 |

**已知限制**

- 转换路径（OpenAI 兼容上游）对 OpenAI 无对应概念的字段是**有损**的——`cache_control`
  尤其会被丢弃。需要 cache 断点时请使用 `anthropic-messages` 上游。
- 未知请求字段被忽略而非拒绝（前向兼容）。

```bash
curl http://127.0.0.1:8080/v1/messages \
  -H "x-api-key: <SK_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4",
    "max_tokens": 256,
    "messages": [{"role": "user", "content": "hello"}]
  }'
```

### GET /v1/models

以 OpenAI list 格式列出该密钥可用的模型。

**认证：** `Authorization: Bearer sk-…`

**响应**

| 字段 | 类型 | 说明 |
|------|------|------|
| `object` | string | 恒为 `"list"`。 |
| `data[].id` | string | 上游 `model_id`，小写。 |
| `data[].object` | string | 恒为 `"model"`。 |
| `data[].created` | integer | 请求时刻的 Unix 秒（非存储时间戳）。 |
| `data[].owned_by` | string | 模型的 `provider`。 |
| `data[].context_window` | integer \| null | 上下文窗口 token 数，供下游 Agent 估算提示预算；未配置时为 `null`。 |
| `data[].max_output_tokens` | integer \| null | 模型单次最多能产出的 token 数；未配置时为 `null`。不要与密钥级 `max_tokens`（网关侧上限）混淆。 |
| `data[].supports_image` | bool | 是否支持图片输入；未配置时为 `false`。 |
| `data[].effort_levels` | string[] \| null | 支持的 `reasoning_effort` 取值，升序（`none`、`minimal`、`low`、`medium`、`high`、`xhigh`、`max`）；未配置时为 `null`。 |

只返回**启用**的绑定模型，按小写 `model_id` 去重。

这四个能力字段是**纯声明**：在管理面配置，原样下发，供下游 Agent 自行决定请求规模。
网关不据此做任何强制——不会因提示过长、含图片或 `reasoning_effort` 不在列表里而拒绝请求；
输出长度仍只由密钥级 `max_tokens` 限制。

```bash
curl http://127.0.0.1:8080/v1/models \
  -H "Authorization: Bearer <SK_TOKEN>"
```

```json
{
  "object": "list",
  "data": [
    {
      "id": "glm-5.1",
      "object": "model",
      "created": 1730000000,
      "owned_by": "zhipu",
      "context_window": 200000,
      "max_output_tokens": 128000,
      "supports_image": true,
      "effort_levels": ["low", "medium", "high"]
    }
  ]
}
```

**错误**

| 状态码 | `message` |
|--------|-----------|
| 401 | `missing API key` / `invalid API key` |
| 500 | `internal server error` |

### POST /v1/responses

OpenAI Responses API 入站。把 Responses 请求转发给密钥绑定命中的上游模型：对
`openai-responses` 上游直通，对 `openai-completions` / `anthropic-messages` 上游经 chat
completions 转换。支持流式与非流式、文本、图片与函数工具调用，以及 `store` /
`previous_response_id` / `conversation` / `background` 四项有状态能力。

**认证：** `Authorization: Bearer sk-…`。错误统一使用本文件的 `ErrorResponse` 信封
（**不是** Anthropic 信封），`code` 等于 HTTP 状态码。

> ⚠️ **隐私警示一：`store` 缺省为 `true`——不传 `store` 也会落库。**
>
> OpenAI 语义下 `store` 的默认值是 `true`。因此**客户端完全不带 `store` 字段时，网关同样会把
> 本轮对话正文写入数据库**，默认保留 7 天（`-response-retention-days`）。这与「只有显式要求
> 才保存」的直觉相反。
>
> | 需求 | 做法 |
> |------|------|
> | 本轮不落库 | 请求里**显式**传 `"store": false`。 |
> | 站点整体禁止落库 | 以 `-allow-response-storage=false`（或 `AIGATE_ALLOW_RESPONSE_STORAGE=false`）启动：所有请求按 `store:false` 处理，有状态能力整体不可用。 |
> | 缩短保留期 | `-response-retention-days`（默认 `7`，`0` = 永不清理）。 |
>
> 站点关闭存储后，**显式**传 `"store": true` 会返回 `400 response storage is disabled on this
> gateway`——网关明确拒绝，而不是静默改写为 `store:false` 让你以为数据存下了、实际没有。

> ⚠️ **隐私警示二：`store: false` ≠ 不读库。**
>
> 只要请求带了 `previous_response_id` 或 `conversation`，网关**仍会读取**父行的历史内容，
> 并把它前置拼进本次 `input` 发给上游。`store: false` 只表示「**本轮结果不再写入存储**」，
> 它**不是**隐私开关：上一轮的正文依然会作为上下文被读出、发往上游。
>
> 要完全不碰历史，就既不要传 `previous_response_id` / `conversation`，又显式传
> `"store": false`。

**请求字段**

请求体是一个 OpenAI Responses 对象。网关只读取并改写下列字段，其余字段在直通路径上原样转发。

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `model` | string | 否 | 目标模型，解析规则与 `/v1/chat/completions` 完全一致（含 primary 回退与 `requested model is not bound to this key`）。 |
| `input` | string \| array | **是** | 字符串，或 input item 数组。转换路径上若无法产出任何 chat 消息则 `400 invalid input`。 |
| `instructions` | string | 否 | 系统提示。转换路径映射为 system 消息；**不写入**会话历史快照（避免续接时重复注入）。 |
| `stream` | bool | 否 | 为 `true` 时走 SSE 流式。 |
| `max_output_tokens` | integer | 否 | 当密钥 `max_tokens` 上限 `> 0` 时被截断至该上限。 |
| `tools` | array | 否 | Responses 扁平工具数组；非数组返回 `400 invalid tools`。 |
| `tool_choice` / `text` / 其他任意字段 | — | 否 | 直通路径原样转发；转换路径按可映射范围转发。 |
| `store` | bool | 否 | **缺省 `true`**（即缺省落库，见上方警示一）。非布尔值返回 `400 invalid store`。 |
| `previous_response_id` | string | 否 | 以前一轮为前缀续接（见上方警示二）。 |
| `conversation` | string \| `{"id": "…"}` | 否 | 以会话中最新完成的一行为前缀续接（见上方警示二）。 |
| `background` | bool | 否 | 为 `true` 时异步执行（见下文「后台执行」）。 |

**有状态规则**

| 情形 | 行为 |
|------|------|
| `previous_response_id` 与 `conversation` 同时出现 | `400 previous_response_id and conversation are mutually exclusive`。 |
| `previous_response_id` 指向的行不存在、**不属于本用户**，或已被保留期清理 | `404 previous response not found`。三种情形**同码同文**，不给枚举他人响应 ID 留缝隙。 |
| `conversation` 指定的会话尚无可用行 | 按新会话处理（不报错）。 |
| 会话中最新一行被 `DELETE` 删除 | 该 `conversation` 回退到上一行；若无更早的行，则按新会话处理。 |
| 父行 `items` 快照损坏 | 降级为「无历史」（不让整条请求失败）。 |
| 站点 `-allow-response-storage=false` + 任何有状态字段 | `400`：`previous_response_id`/`conversation` 报 `response storage is disabled on this gateway, previous_response_id and conversation are unavailable`；`background:true` 报 `response storage is disabled on this gateway, background is unavailable`。 |

续接用的历史是**累积快照**：每行的快照 = 上一次的历史 + 本轮 `input` + 本轮输出 items，**不含**
`instructions`。同一用户的另一把数据面密钥可以续接本人的行（归属只按用户校验，不按密钥）。

> ⚠️ **同一用户下的所有数据面密钥共享同一个会话空间。**
> 归属校验的粒度是**用户**，不是密钥——这样设计是为了让密钥轮换（`rotate`）后既有会话不断掉。
> 代价是：若你用同一用户建了多把密钥分别给**不同的下游应用**，应用 A 生成的
> `previous_response_id`，应用 B 若拿到这个 ID 就能读到 A 的完整对话历史。
> `conversation` 尤其要注意：它的 ID 由**客户端任意指定**，同名 `conversation` 在本用户下
> 指向同一片历史。
> **需要按应用隔离时，请为每个应用开独立用户**，不要用同一用户的多把密钥区分应用。

**上游路径**

命中模型的 `model_type` 决定走哪条路径：

| 上游 | 行为 |
|------|------|
| `openai-responses` | **直通。** 请求体原样转发（仅把 `model` 替换为上游 ID，并**强制** `store:false`）到 `<url>/responses`。上游响应与 SSE 流原样回传，只改写终态事件的 `id` / `model`。 |
| `openai-completions` / `anthropic-messages` | **转换。** Responses 请求转换为 OpenAI chat 请求（`instructions`/system → system 消息、input items → messages、`max_output_tokens` → `max_tokens`、`text.format` → `response_format`、扁平 `tools` → 嵌套 `tools`），chat 响应或 SSE 增量再回译为 Responses 响应 / 事件序列。 |

两条路径都**强制上游 `store: false`**——网关不会把数据同时写进上游厂商的存储。
两条路径的上游超时一致：非流式 5 分钟，流式 30 分钟。

**响应 —— 非流式**

`200` 返回 Responses 响应对象。`id` 恒为网关生成的 `resp_` + 32 位十六进制，不直接透传上游
ID；上游 ID 既不记录也不对外暴露。

```json
{
  "id": "resp_0f3a6c1d8b2e4a7c9d5f1b3e6a8c0d2e",
  "object": "response",
  "model": "glm-5.1",
  "status": "completed",
  "output": [
    {
      "type": "message",
      "role": "assistant",
      "content": [{"type": "output_text", "text": "hi", "annotations": []}]
    }
  ],
  "usage": {
    "input_tokens": 5,
    "output_tokens": 2,
    "total_tokens": 7,
    "input_tokens_details": {"cached_tokens": 0},
    "output_tokens_details": {"reasoning_tokens": 0}
  }
}
```

`status` 取值：`queued`、`in_progress`、`completed`、`incomplete`、`failed`、`cancelled`。
上游 `finish_reason=length` 时状态为 `incomplete`，并带
`incomplete_details: {"reason": "max_output_tokens"}`——截断显式告知，不静默当作成功。
`failed` 时带 `error: {"message": "…", "type": "background_error"}`，常见 `message` 有
`failed to connect to LLM backend`、`upstream returned status <code>`、`invalid upstream response`、
`internal server error`。

**响应 —— 流式（`stream: true`）**

`Content-Type: text/event-stream`，带 `Cache-Control: no-cache` 与 `X-Accel-Buffering: no`。
转换路径的事件顺序为：

```text
response.created → response.in_progress
  → response.output_item.added → response.output_text.delta*
  → response.completed（或 response.incomplete / response.failed）
```

直通路径原样转发上游自身的 Responses 事件流，仅改写终态事件里的 `id` / `model`。用量在流结束后记录。

**后台执行（`background: true`）**

1. 网关先完成鉴权、模型解析与配额校验——**被拒的请求不会在库里留下行**；随后建一行
   `status: queued`，**立即返回该对象**（HTTP `200`）。
2. 后台任务把状态推进为 `in_progress`，最终落到 `completed` / `failed`（或取消后的 `cancelled`）。
3. 客户端用 `GET /v1/responses/{id}` 轮询。`POST` 的回包与后台写回之前的 `GET` 是**同一行、
   逐字相同**的内容。
4. 任务总时长受 `-response-background-timeout` 限制（默认 `30m`）。

`background: true` 要求落库（结果要有地方存放）：`store` 恒为 `true`，显式 `store: false` 返回
`400 background requires store to be true`（这是参数自身的矛盾，先于站点开关判定）。

进程重启会留下无人推进的在途行：启动后网关每分钟把「老过后台超时」的 `queued` / `in_progress`
行标记为 `failed`，`error.message` 为 `interrupted by restart`。多实例部署下，后台任务由**接收
请求的那个实例**执行，不迁移；状态在数据库中，因此轮询打到别的实例也能读到正确状态。

**错误**

| 状态码 | `message` | 原因 |
|--------|-----------|------|
| 401 | `missing API key` / `invalid API key` | 无 `Authorization` 头 vs 格式错误、未知或已禁用。 |
| 400 | `failed to read request body` | 请求体读取失败。 |
| 413 | `request body too large` | 请求体超过 1 MiB；直接拒绝，不截断。 |
| 400 | `invalid json body` | 请求体不是 JSON 对象。 |
| 400 | `invalid input` / `invalid tools` | `input` 无法产出任何消息 / `tools` 不是数组（转换路径）。 |
| 400 | `invalid store` / `invalid background` | 字段不是布尔值。 |
| 400 | `previous_response_id and conversation are mutually exclusive` | 两个续接字段同时出现。 |
| 404 | `previous response not found` | 父行不存在、不属于本用户或已被清理。 |
| 400 | `background requires store to be true` | `background:true` 与显式 `store:false` 冲突。 |
| 400 | `response storage is disabled on this gateway` | 站点关闭存储，但客户端显式要求 `store:true`。 |
| 400 | `response storage is disabled on this gateway, previous_response_id and conversation are unavailable` | 站点关闭存储，但带续接字段。 |
| 400 | `response storage is disabled on this gateway, background is unavailable` | 站点关闭存储，但请求 `background:true`。 |
| 400 | `requested model is not bound to this key` / `no available model for this key` | 模型解析失败（规则同 `/v1/chat/completions`）。 |
| 429 / 403 | 同 `/v1/chat/completions` 的配额文案 | 配额校验拒绝；状态码取 `-quota-exceeded-status`，带 `Retry-After`。 |
| 500 | `internal server error` | 内部错误（含无法生成 `resp_` ID 的熵源故障）。 |
| 502 | `failed to connect to LLM backend` / `invalid upstream response` | 无法连接上游 / 上游响应无法解析。 |

**不支持的能力**

- **内置工具**（`web_search`、`file_search`、`code_interpreter` 等）：转换路径没有对应物，
  **被丢弃**；只有 `function` 类型工具会被映射。
- **Responses 专有结构**：`reasoning` item、`encrypted_content`、内置工具输出、`annotations`
  等在转换路径上**有损**（被丢弃）。需要端到端保留这些结构时请使用 `openai-responses` 上游。
- **非函数工具、上游侧 `store`**：上游 `store` 恒被强制为 `false`，网关不接受让上游代存。
- 未知请求字段在直通路径原样转发，在转换路径被忽略而非拒绝（前向兼容）。

**请求示例 —— 无状态（显式不落库）**

```bash
curl http://127.0.0.1:8080/v1/responses \
  -H "Authorization: Bearer <SK_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "glm-5.1",
    "input": "hello",
    "store": false
  }'
```

**请求示例 —— 有状态续接**

```bash
# 第一轮：缺省 store=true，本轮会被保存
curl http://127.0.0.1:8080/v1/responses \
  -H "Authorization: Bearer <SK_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"model": "glm-5.1", "input": "你好"}'
# → {"id": "resp_0f3a…", "status": "completed", …}

# 第二轮：以上一轮为前缀续接
curl http://127.0.0.1:8080/v1/responses \
  -H "Authorization: Bearer <SK_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "glm-5.1",
    "input": "再说一遍",
    "previous_response_id": "resp_0f3a…"
  }'
```

### GET /v1/responses/{id}

读取一行已存储的响应。返回的响应对象与 `POST /v1/responses` 同一形状。

**认证：** `Authorization: Bearer sk-…`

**响应**：`200` 返回该行的响应对象——后台任务尚未写回时是 `status: queued` 的对象，写回后是
终态对象（`completed` / `incomplete` / `failed` / `cancelled`）。

```bash
curl http://127.0.0.1:8080/v1/responses/resp_0f3a… \
  -H "Authorization: Bearer <SK_TOKEN>"
```

**错误**

| 状态码 | `message` |
|--------|-----------|
| 401 | `missing API key` / `invalid API key` |
| 404 | `previous response not found`（不存在 / 不属于本用户 / 已过期清理，三者同文同码） |
| 500 | `internal server error` |

### DELETE /v1/responses/{id}

删除已完成的响应。**仅终态**（`completed` / `failed` / `incomplete` / `cancelled`）可删除；
在途的行必须先 `cancel`。

**认证：** `Authorization: Bearer sk-…`

**响应**：`200`

```json
{"id": "resp_0f3a…", "object": "response.deleted", "deleted": true}
```

删除会话中最新一行会使该 `conversation` 回退到上一行；若无更早的行，按新会话处理。删除后该
ID 不能再作为 `previous_response_id` 使用（返回 404）。

**错误**

| 状态码 | `message` | 原因 |
|--------|-----------|------|
| 401 | `missing API key` / `invalid API key` | — |
| 404 | `previous response not found` | 不存在 / 不属于本用户 / 已过期清理。 |
| 400 | `response is still running, cancel it before deleting` | 行仍在 `queued` / `in_progress`。 |
| 500 | `internal server error` | — |

### POST /v1/responses/{id}/cancel

取消一个在途（`queued` / `in_progress`）的响应：状态置为 `cancelled`，**并中断其正在进行的上游
请求**（避免算力与配额白烧到后台超时）。

**认证：** `Authorization: Bearer sk-…`

**响应**：`200` 返回取消后的响应对象（`status: "cancelled"`）。

**已终态的行不可取消**，返回 `400`，响应体是错误信封**加上当前响应对象**——客户端不必再发一次
GET 就能拿到结果或失败原因：

```json
{
  "error": {
    "message": "response already finished and cannot be cancelled",
    "type": "invalid_request_error"
  },
  "response": {
    "id": "resp_0f3a…",
    "object": "response",
    "status": "completed",
    "output": []
  }
}
```

取消与完成竞争时**以先到者为准**：若终态已被写入，取消只会返回该终态。

**错误**

| 状态码 | `message` | 原因 |
|--------|-----------|------|
| 401 | `missing API key` / `invalid API key` | — |
| 404 | `previous response not found` | 不存在 / 不属于本用户 / 已过期清理。 |
| 400 | `response already finished and cannot be cancelled` | 行已终态（响应体另含 `response` 字段）。 |
| 500 | `internal server error` | — |

### POST /v1/decisions 与 POST /v1/systemone（决策模型）

决策模型不生成文本，只回答「有类型的问题」并返回概率。网关同时开放两种协议入口，
任一入口都能落到任一上游格式（跨协议由网关翻译）：

| 入口 | 协议 | 上游 `model_type` | 说明 |
|------|------|-------------------|------|
| `POST /v1/decisions`（别名 `POST /decisions`） | OpenAI Decisions | `openai-decisions` 或 `systemone` | `input` + `questions` 数组 |
| `POST /v1/systemone`（别名 `POST /systemone`） | TypeSafe System One | `systemone` 或 `openai-decisions` | `state` + `questions` 对象 |

两条入口都**不支持流式**：请求体出现 `stream: true` 直接返回 `400`。

#### 请求：OpenAI Decisions 形态

```bash
curl -X POST http://127.0.0.1:8080/v1/decisions \
  -H "Authorization: Bearer sk-xxx" -H "Content-Type: application/json" \
  -d '{"model":"luna","input":"Customer wrote: I was charged twice.",
       "questions":[{"type":"predicate","name":"is_refund","instructions":"Refund request?"},
                    {"type":"choice","name":"dept","instructions":"Team?",
                     "choices":[{"value":"billing"},{"value":"technical"}]},
                    {"type":"score","name":"frustration","instructions":"How angry?",
                     "levels":[{"label":"calm"},{"label":"angry"}]}]}'
```

| 字段 | 规则 |
|------|------|
| `model` | 必填，必须是该密钥绑定的模型名 |
| `input` | 必填；字符串，或 `{role, content}` 消息数组（content 为字符串或 `input_text` / `input_image` part） |
| `questions` | 1..128 项；`type` 为 `predicate` / `choice` / `score` / `box`；`name` 缺省按序号 |
| `questions[].choices` | 2..255 项，`value` 为字符串或布尔，且互不重复 |
| `questions[].levels` | 2..10 项，`label` 必填 |
| `safety_identifier` | 可选；仅在 `openai-decisions` 上游生效，落到 `systemone` 上游返回 400 |

#### 请求：System One 形态

```bash
curl -X POST http://127.0.0.1:8080/v1/systemone \
  -H "Authorization: Bearer sk-xxx" -H "Content-Type: application/json" \
  -d '{"model":"jev","state":"Customer wrote: I was charged twice.",
       "questions":{"is_refund":{"type":"noul","instructions":"Refund request?"},
                    "dept":{"type":"choice","instructions":"Team?","criteria":{"billing":"","technical":""}},
                    "frustration":{"type":"score","instructions":"How angry?","criteria":["calm","angry"]}}}'
```

| 字段 | 规则 |
|------|------|
| `state` | 必填；字符串、对象或数组 |
| `questions` | 对象，1..128 个 key（**key 的顺序即答案顺序**）；`type` 为 `noul` / `choice` / `score` / `box` |
| `questions[].criteria` | choice：1..255 个 key；score：1..10 个字符串 |
| `images` / `assets` | 可选；`assets` 无法翻译到 `openai-decisions` 上游，跨协议时返回 400 |

两种入口的 `type` 互不通用：在 `/v1/decisions` 上写 `noul`、在 `/v1/systemone` 上写
`predicate` 都会返回 400，并提示应使用的等价类型。

#### 响应

响应永远与**入口**协议一致：

- `/v1/decisions`：`answers` 为数组，顺序与请求 `questions` 一致，每项带 `name` 与 `type`；
- `/v1/systemone`：`answers` 为对象，key 与请求 `questions` 一致。

两边都保留上游 `usage`（`input_tokens` / `output_tokens` / `total_tokens`）。上游拒答时：
OpenAI 入口返回 `{"type":"refusal","name":...}`；System One 上游的 422 `decision_refused`
原样透传到客户端。

#### 跨协议映射

| 概念 | OpenAI Decisions | System One |
|------|------------------|------------|
| 上下文 | `input`（字符串或消息数组） | `state`；消息数组按 `\n\n` 拍平成字符串 |
| 二元判断 | `predicate` → `probability` | `noul` → `noul` |
| 多选 | `choices[{value,description}]`（value 可为布尔） | `criteria{key: description}`（key 恒为字符串） |
| 打分 | `levels[{label,description}]` | `criteria: [label, ...]` |
| 框选 | `box` | `box` |

有损字段一律 400，不静默丢弃：`assets`（→ OpenAI）、`safety_identifier`（→ System One）、
`input` 里非 `user` / `system` / `developer` 角色的消息。

#### 用量计费

决策模型的 `ai_models.billing_mode` 默认 `input_only`：用量表记
`completion_tokens = 0`、`total_tokens = prompt_tokens`，响应体里的 `usage` 仍是上游原值。
显式设成 `per_token` 时按上游返回值如实记账。

## 3. 管理面

所有 `/admin/*` 路由都要求 `Authorization: Bearer <ADMIN_TOKEN>`，且该 token 属于
`role=admin` 用户。路径 `{id}` 必须为正整数；非数字或 0 返回
`400 invalid <model|credential|key|user|group|webhook> id`。目标不存在返回
`404 <model|credential|key|user|group|webhook> not found`。

`401` 与 `403` 的语义见附录「鉴权失败语义」。

### 3.1 模型

模型对象的字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | integer | 服务端分配。 |
| `provider` | string | 自由标签，在 `/v1/models` 中作为 `owned_by`。 |
| `model_id` | string | 发给上游的模型标识。 |
| `model_name` | string | 展示名。 |
| `url` | string | 上游基础地址。 |
| `credential_id` | integer | 引用的凭据 ID；`0` 表示未引用（使用 `api_key`）。 |
| `model_type` | string | 上游协议：`openai-completions`、`anthropic-messages`、`openai-responses`、`openai-decisions`、`systemone` 或 `gemini-generate-content`。同时决定 `url` 的拼接方式（见 §6「运行须知」）。 |
| `billing_mode` | string | 计费口径：`per_token`（默认，如实记账）或 `input_only`（completion 记 0）。决策类 `model_type` 创建时缺省为 `input_only`。 |
| `custom_http_headers` | object \| null | 允许透传的头（字符串值）。 |
| `quota_day` | integer | 模型级日 token 配额；`-1` 表示不限。 |
| `enabled` | bool | 禁用后立即不可路由。 |
| `currency` | string | 计价币种（ISO 4217）。`""` 表示未配置，该模型不参与金额记账（见 §3.10「计费与金额」）。 |
| `enable_peak_pricing` | bool | 峰谷定价开关（默认 `false`）。关闭时请求**恒定按空闲价三列**计价。 |
| `use_upstream_cost` | bool | 是否优先采信上游自报的 `usage.cost`（默认 `true`）。开启且上游自报 `cost > 0` 时直接采用，不再按价目自算。 |
| `context_window` | integer | 上下文窗口 token 数；`0` 表示未配置。纯能力声明，经 `GET /v1/models` 下发。 |
| `max_output_tokens` | integer | 模型单次最多能产出的 token 数；`0` 表示未配置。纯能力声明，经 `GET /v1/models` 下发，**不参与任何强制逻辑**（输出上限只由密钥级 `max_tokens` 决定）。 |
| `supports_image` | bool | 是否支持图片输入。纯能力声明，不拦截含图请求。 |
| `effort_levels` | string[] | 支持的 `reasoning_effort` 等级，按阶梯升序归一化（`none`、`minimal`、`low`、`medium`、`high`、`xhigh`、`max`）；`[]` 表示未配置。纯能力声明，不按等级校验请求。 |
| `current_price` | object \| null | 当前生效的价目快照（`effective_at` 不晚于当前时刻的最新一条）；无价目时为 `null`。形状同价目行（见 §3.10）。 |
| `created_at` / `updated_at` | string | RFC 3339 时间戳。 |

`api_key` **绝不**出现在任何响应中。

#### POST /admin/models

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `provider` | string | 否 | `""` | |
| `model_id` | string | **是** | — | 去空白后必须非空。 |
| `model_name` | string | 否 | `""` | |
| `url` | string | **是** | — | 去空白后必须非空。 |
| `api_key` | string | 否 | `""` | 上游凭据（`credential_id` 为 `0` 时使用）。 |
| `credential_id` | integer | 否 | `0` | 必须引用存在的凭据，否则 `400 credential <id> not found`。 |
| `model_type` | string | **是** | — | 必须为 `openai-completions`、`anthropic-messages`、`openai-responses`、`openai-decisions`、`systemone` 或 `gemini-generate-content`。 |
| `billing_mode` | string | 否 | 按 `model_type` 推 | `per_token` 或 `input_only`；决策类模型缺省为 `input_only`。 |
| `custom_http_headers` | object | 否 | 无 | 字符串值的 JSON 对象；`null` 存为空值。 |
| `quota_day` | integer | 否 | `-1` | |
| `enabled` | bool | 否 | `true` | |
| `user_key_required` | bool | 否 | `false` | 自备 Key（BYOK）模式：模型**不保存**共享密钥，转发密钥取绑定行。与 `api_key`（非空）或 `credential_id > 0` 同时出现返回 `400`；创建后不可变更。 |
| `currency` | string | 否 | `""` | 计价币种，ISO 4217 三位字母（大小写不敏感，归一为大写）。否则返回 `400 currency must be a 3-letter ISO 4217 code`。 |
| `enable_peak_pricing` | bool | 否 | `false` | |
| `use_upstream_cost` | bool | 否 | `true` | |
| `context_window` | integer | 否 | `0` | 必须 `>= 0`，否则 `400 context_window must not be negative`。 |
| `max_output_tokens` | integer | 否 | `0` | 必须 `>= 0`，否则 `400 max_output_tokens must not be negative`。 |
| `supports_image` | bool | 否 | `false` | |
| `effort_levels` | string[] | 否 | `[]` | 未知取值或重复取值返回 `400`。大小写不敏感，按阶梯升序落库。 |

```bash
curl -X POST http://127.0.0.1:8080/admin/models \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "zhipu",
    "model_id": "glm-5.1",
    "model_name": "GLM-5.1",
    "url": "https://open.bigmodel.cn/api/paas/v4",
    "api_key": "<上游密钥>",
    "model_type": "openai-completions",
    "quota_day": -1,
    "enabled": true
  }'
```

响应 `201 Created` —— 上述模型对象。

| 状态码 | 条件 |
|--------|------|
| 201 | 已创建。 |
| 400 | JSON 非法；`model_id` 为空；`url` 为空；`model_type` 非法；`custom_http_headers` 格式错误；`credential_id` 不存在；`user_key_required` 与共享 `api_key`/凭据同时出现。 |
| 401 / 403 | 鉴权 / 授权失败。 |
| 500 | 数据库错误。 |

#### GET /admin/models

响应 `200 OK`：

```json
{"models": [ /* 模型对象 */ ]}
```

无模型时数组为 `[]`。

#### GET /admin/models/{id}

响应 `200 OK` —— 单个模型对象；不存在时 `404 model not found`。

#### PUT /admin/models/{id}

局部更新：只修改请求体中出现的字段（未出现的不动）。

| 字段 | 类型 | 说明 |
|------|------|------|
| `provider` | string | |
| `model_id` | string | 为空返回 `400`。 |
| `model_name` | string | |
| `url` | string | 为空返回 `400`。 |
| `api_key` | string | 仅在非空时应用；空串是无操作（凭据无法被清空）。 |
| `credential_id` | integer | `> 0` 必须引用存在的凭据；`0` 解除引用。 |
| `model_type` | string | 必须是合法枚举。 |
| `custom_http_headers` | object \| null | 对象则替换；`null` 清空。 |
| `quota_day` | integer | |
| `enabled` | bool | |
| `user_key_required` | bool | 不可变更：传值与当前不同返回 `400 user_key_required cannot be changed after creation`。自备 Key 模型携带非空 `api_key` 或 `credential_id > 0` 返回 `400`。 |
| `currency` | string | ISO 4217 三位字母；`""` 清除计价币种。改动后立即失效该模型的计价缓存，且不改写已记账的历史行。 |
| `context_window` | integer | `>= 0`。 |
| `max_output_tokens` | integer | `>= 0`；`0` 清除模型级上限（密钥级上限仍生效）。 |
| `supports_image` | bool | |
| `effort_levels` | string[] | 整体替换；`[]` 清除。校验同创建。 |
| `enable_peak_pricing` | bool | |
| `use_upstream_cost` | bool | |

```bash
curl -X PUT http://127.0.0.1:8080/admin/models/1 \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"enabled": false}'
```

响应 `200 OK` —— 更新后的模型对象。

| 状态码 | 条件 |
|--------|------|
| 200 | 已更新。 |
| 400 | JSON 非法；`model_id`/`url` 为空；`model_type` 非法；`custom_http_headers` 格式错误；`credential_id` 不存在；id 非法。 |
| 404 | 模型不存在。 |

#### DELETE /admin/models/{id}

删除模型并清理引用它的所有密钥绑定。

响应 `200 OK`：

```json
{"ok": true}
```

| 状态码 | 条件 |
|--------|------|
| 200 | 已删除。 |
| 400 | id 非法。 |
| 404 | 模型不存在。 |

#### POST /admin/models/{id}/connectivity

以最小请求（单条 user 消息，要求模型只回一个 `hello`，`max_tokens=8`）探测上游，验证地址可达、凭据有效且模型可调用。

**探测结果一律以 HTTP `200` 返回**，成功/失败体现在 body 的 `ok`：

| Body | 含义 |
|------|------|
| `{"ok": true, "latency_ms": N, "reply": "hello", "upstream_id": "…", "upstream_model": "…"}` | 成功。`latency_ms` 是完整往返毫秒数（读完响应体才计时，不是收到响应头就停），取整且至少为 `1`；`reply` 为上游真实返回的助手文本；`upstream_id`/`upstream_model` 是上游自报的请求 id 与模型名，便于和上游日志对齐。 |
| `{"ok": false, "error_kind": "…", "message": "…"}` | 失败。`error_kind` 为封闭枚举，`message` 为人类可读原因。 |

`error_kind` 取值：

| 取值 | 原因 |
|------|------|
| `network_unreachable` | DNS/连接/TLS/超时，或被 SSRF 拦截（默认兜底项）。 |
| `invalid_api_key` | 上游拒绝凭据。 |
| `forbidden` | 上游禁止该请求。 |
| `rate_limited` | 上游对探测限流。 |
| `upstream_server` | 上游返回 5xx。 |
| `upstream_client` | 上游返回其它 4xx。 |

出站请求复用数据面一致的 SSRF 防护：默认严格模式下私网/回环上游会被拦截并归类为
`network_unreachable`，需要放行时启动加 `-allow-private-upstreams`。上游密钥实时解析
（优先凭据，其次模型自身 `api_key`）；解析失败属服务端配置问题，按 `500` 返回而非探测结果。

`user_key_required`（自备 Key）模型没有共享密钥可探测：返回 `400 user_key_required model has no shared key to probe; verify it from the data plane with a binding key`。

仅路径 id 非法（`400`）与模型不存在（`404`）走统一错误体。

| 状态码 | 条件 |
|--------|------|
| 200 | 探测完成（无论 `ok` 真假）。 |
| 400 | id 非法；模型为 `user_key_required`。 |
| 404 | 模型不存在。 |
| 500 | 凭据/密钥解析失败。 |

#### POST /admin/models/discover

用一把密钥向上游查询可用模型 ID，供管理界面列出候选模型，避免手工抄写 `model_id`。

请求体：

| 字段 | 类型 | 说明 |
|------|------|------|
| `model_type` | string，必填 | `openai-completions`、`anthropic-messages` 或 `gemini-generate-content`。 |
| `url` | string，必填 | 与 `model.url` 语义一致：`openai-completions` 是带 `/v1` 的 base，`anthropic-messages` 是 host。 |
| `api_key` | string，可选 | 直接使用（适用于尚未落库的新密钥）。 |
| `credential_id` | number，可选 | 改用已保存凭据的第一个 header 密钥。 |

两种密钥来源必须给一个；同时给时以 `api_key` 为准。

返回沿用连通性探测的口径：一律 HTTP `200`，成功/失败体现在 body：

| Body | 含义 |
|------|------|
| `{"ok": true, "models": ["gpt-4o", …]}` | 上游 `/models` 返回的模型 ID 列表。 |
| `{"ok": false, "error_kind": "…", "message": "…"}` | `error_kind` 与连通性探测同一枚举。 |

只有 2xx 不算通过：响应体必须能解析出非空的助手消息，否则按 `upstream_client` 判失败，避免「网关回了 200 但没真正调用模型」被当成连通。

出站请求复用数据面 SSRF 防护：默认严格模式下私网/回环地址被拦截，归类为 `network_unreachable`，需要放行时启动加 `-allow-private-upstreams`。

| 状态码 | 条件 |
|--------|------|
| 200 | 探测完成（无论 `ok` 真假）。 |
| 400 | `model_type`/`url` 非法、未给密钥来源、或凭据没有 header 密钥。 |
| 404 | `credential_id` 不存在。 |

### 3.2 凭据

上游凭据把一条或多条密钥组织在一起，由模型的 `credential_id` 引用。转发时网关实时解析
凭据（不使用快照），取首条 `location=header` 的密钥；凭据不存在、已禁用或无 header
密钥时回退到模型自身的 `api_key`。

凭据对象的字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | integer | 服务端分配。 |
| `name` | string | |
| `description` | string | |
| `secrets` | array | 密钥项；`value` 恒为掩码。 |
| `enabled` | bool | 禁用后回退模型自身 `api_key`。 |
| `created_at` / `updated_at` | string | RFC 3339 时间戳。 |

密钥项对象：

| 字段 | 类型 | 说明 |
|------|------|------|
| `key` | string | 头名。 |
| `value` | string | 读取时掩码：长度 > 4 时保留首 2 + `*`*(n-4) + 末 2；更短时整体为 `*`。 |
| `location` | string | 仅支持 `header`；省略/空串会归一化为 `header`。 |

#### POST /admin/credentials

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `name` | string | **是** | — | 去空白后必须非空。 |
| `description` | string | 否 | `""` | |
| `secrets` | array | 否 | 无 | 每项 `key` 必填；`location` 必须为 `header`。 |
| `enabled` | bool | 否 | `true` | |

```bash
curl -X POST http://127.0.0.1:8080/admin/credentials \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "zhipu-prod",
    "description": "生产上游密钥",
    "secrets": [{"key": "Authorization", "value": "Bearer <上游密钥>", "location": "header"}]
  }'
```

响应 `201 Created` —— 带掩码 `secrets` 的凭据对象。

| 状态码 | 条件 |
|--------|------|
| 201 | 已创建。 |
| 400 | JSON 非法；`name` 为空；某条密钥 `key` 为空或 `location` 非 `header`。 |
| 500 | 数据库错误。 |

#### GET /admin/credentials

```json
{"credentials": [ /* 凭据对象，secrets 已掩码 */ ]}
```

#### GET /admin/credentials/{id}

响应 `200 OK` —— 单个凭据对象（secrets 掩码）；不存在时 `404 credential not found`。

#### PUT /admin/credentials/{id}

局部更新。

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | 为空返回 `400`。 |
| `description` | string | |
| `secrets` | array | 出现时**整体替换**；不传则该字段不动。 |
| `enabled` | bool | |

响应 `200 OK` —— 更新后的凭据对象。

#### DELETE /admin/credentials/{id}

删除凭据。若仍有模型引用它，请求被拒绝并返回 `409`，message 含引用数量：

```json
{
  "error": {
    "message": "credential is referenced by 2 model(s); detach them before deleting",
    "type": "error",
    "code": 409
  }
}
```

请先解除引用（把模型的 `credential_id` 置 `0` 或改指其它凭据）。

| 状态码 | 条件 |
|--------|------|
| 200 | 已删除（`{"ok": true}`）。 |
| 404 | 凭据不存在。 |
| 409 | 仍被一个或多个模型引用。 |

### 3.3 密钥

数据面密钥（`sk-…`）归属于某个用户，并可绑定一组模型。

密钥对象的字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | integer | 服务端分配。 |
| `name` | string | |
| `user_id` | integer | 属主用户 ID。 |
| `group_id` | integer | 可选分组，未设置时为 `0`。 |
| `proxy_token_prefix` | string | 展示前缀，形如 `sk-abcd1234…wxyz`。 |
| `max_tokens` | integer | 请求侧上限；`<= 0` 表示不限。 |
| `token_quota_rules` | string | 密钥级周期配额（规范化后的规则 JSON）；`""` = 无密钥级限制。用户面只读。 |
| `enabled` | bool | 禁用后数据面鉴权返回 `401`。 |
| `expires_at` | string / null | 可选有效期（RFC 3339，UTC）；`null` = 永不过期。 |
| `expired_at` | string / null | 首次因过期被拒绝的观测时间；`null` = 未发生。 |
| `created_at` / `updated_at` | string | RFC 3339 时间戳。 |
| `bindings` | array | 绑定视图（无绑定时为 `[]`）。 |
| `token` | string | **仅**在创建与 rotate 响应中出现。 |

绑定视图对象：

| 字段 | 类型 | 说明 |
|------|------|------|
| `ai_model_id` | integer | 引用的模型 ID。 |
| `model_id` | string | |
| `model_name` | string | |
| `provider` | string | |
| `role` | string | `primary` 或 `fallback`。 |
| `sort_order` | integer | 排序提示。 |
| `has_key` | bool | 是否已配置绑定级上游密钥（`user_key_required` 模型必需；值本身永不返回）。 |

#### POST /admin/keys

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `name` | string | 否 | `""` | |
| `user_id` | integer | **是** | — | 必须引用存在的用户，否则 `400 user_id does not exist`。 |
| `group_id` | integer | 否 | `0` | |
| `max_tokens` | integer | 否 | `0` | `<= 0` 表示不封顶。 |
| `token_quota_rules` | string | 否 | `""` | 密钥级周期配额（规则数组 JSON，会做校验与规范化）。`""` = 无密钥级限制；`"[]"` = 显式不限。 |
| `expires_at` | string | 否 | `null` | RFC 3339 时间戳；缺省/`null` = 永不过期。无法解析时报 `400 expires_at must be an RFC3339 timestamp`。 |
| `bindings` | array | 否 | 无 | 见下。 |

每个绑定对象：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `ai_model_id` | integer | **是** | 必须 `> 0` 且引用存在的模型。 |
| `role` | string | **是** | `primary` 或 `fallback`；其它取值被拒绝。 |
| `sort_order` | integer | 否 | 默认 `0`。 |
| `api_key` | string | 否 | 绑定级上游密钥，仅 `user_key_required`（自备 Key）模型使用；普通模型忽略。加密落库、永不回显（仅以 `has_key` 反映是否已配置）。 |

`api_key` 随 `bindings` 整体替换：替换绑定但未重新携带 `api_key` 时该绑定的密钥被清空（`has_key` 变为 `false`）。

`bindings` 非空时必须**恰好一个** `primary`，否则返回
`400 bindings must contain exactly one primary model`。

注意：每用户 `settings.user_key_limit` 只作用于自助接口（§4.4），**不**约束管理面创建的密钥。

```bash
curl -X POST http://127.0.0.1:8080/admin/keys \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "team-a default",
    "user_id": 2,
    "group_id": 0,
    "max_tokens": 8192,
    "bindings": [
      {"ai_model_id": 1, "role": "primary", "sort_order": 0},
      {"ai_model_id": 2, "role": "fallback", "sort_order": 1}
    ]
  }'
```

响应 `201 Created` —— 密钥对象，其 `token` 字段为一次性明文（`sk-…`）。除 rotate 外这是唯一一次返回。

| 状态码 | 条件 |
|--------|------|
| 201 | 已创建。 |
| 400 | JSON 非法；`user_id` 不存在；绑定角色非法；`ai_model_id` 为 `0`；`ai_model_id` 不存在；`primary` 不是恰好一个；绑定重复。 |
| 401 / 403 | 鉴权 / 授权失败。 |
| 500 | 数据库错误。 |

#### GET /admin/keys

响应 `200 OK`：

```json
{"keys": [ /* 密钥对象，不含 token */ ]}
```

#### GET /admin/keys/{id}

响应 `200 OK` —— 单个密钥对象（无 `token`）；不存在时 `404 key not found`。

#### PUT /admin/keys/{id}

局部更新。

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | |
| `max_tokens` | integer | |
| `token_quota_rules` | string | `""` 清除密钥级配额；否则必须是合法规则数组（会做校验与规范化）。 |
| `enabled` | bool | |
| `expires_at` | string / null | RFC 3339 值 = 设置新有效期（并清空 `expired_at`）；显式 `null` = 两者都清除；其余非法值为 `400`。 |
| `bindings` | array | 出现时**整体替换**；`[]` 清空。不传则保持原绑定。 |

`user_id` 与 `group_id` 不可更新。

```bash
curl -X PUT http://127.0.0.1:8080/admin/keys/1 \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"enabled": false, "bindings": []}'
```

响应 `200 OK` —— 更新后的密钥对象（无 `token`）。

| 状态码 | 条件 |
|--------|------|
| 200 | 已更新。 |
| 400 | JSON 非法；绑定集合非法；id 非法。 |
| 404 | 密钥不存在。 |

#### DELETE /admin/keys/{id}

删除密钥及其绑定。响应 `200 OK`：

```json
{"ok": true}
```

#### POST /admin/keys/{id}/rotate

换发新的 `sk-` 凭据，旧凭据立即失效。密钥的绑定与设置保留。

响应 `200 OK` —— 带一次性 `token` 的密钥对象。

| 状态码 | 条件 |
|--------|------|
| 200 | 已 rotate。 |
| 400 | id 非法。 |
| 404 | 密钥不存在。 |

### 3.4 用户

用户持有访问 token（`gk-…`），用于管理面（`role=admin` 时）与用户面鉴权。

用户对象的字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | integer | 服务端分配。 |
| `name` | string | |
| `role` | string | `admin` 或 `user`。 |
| `group_id` | integer | 未设置时为 `0`。 |
| `api_token_prefix` | string | 展示前缀。 |
| `enabled` | bool | 禁用后鉴权返回 `401`。 |
| `created_at` / `updated_at` | string | RFC 3339 时间戳。 |
| `token` | string | **仅**在创建与 rotate 响应中出现。 |

#### POST /admin/users

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `name` | string | 否 | `""` | |
| `role` | string | 否 | `user` | 必须为 `admin` 或 `user`。 |
| `group_id` | integer | 否 | `0` | |
| `enabled` | bool | 否 | `true` | |

```bash
curl -X POST http://127.0.0.1:8080/admin/users \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"username": "alice", "role": "user", "group_id": 0}'
```

响应 `201 Created` —— 用户对象，其 `token` 为一次性明文（`gk-…`）。

| 状态码 | 条件 |
|--------|------|
| 201 | 已创建。 |
| 400 | JSON 非法；`role` 非法。 |
| 500 | 数据库错误。 |

#### GET /admin/users

响应 `200 OK`：

```json
{"users": [ /* 用户对象，不含 token */ ]}
```

没有单用户 `GET /admin/users/{id}` 端点。

#### PUT /admin/users/{id}

局部更新。

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | |
| `role` | string | 必须为 `admin` 或 `user`。 |
| `group_id` | integer | |
| `enabled` | bool | |

响应 `200 OK` —— 更新后的用户对象（无 `token`）；不存在时 `404 user not found`。

#### DELETE /admin/users/{id}

删除用户，并级联删除其密钥及绑定。历史用量日志保留。

响应 `200 OK`：

```json
{"ok": true}
```

#### POST /admin/users/{id}/rotate

换发新的访问 token，旧 token 立即失效。

响应 `200 OK` —— 带一次性 `token` 的用户对象。

### 3.5 分组

分组承载 token 配额策略，用户通过 `user.group_id` 归属。数据面配额判定时，用户所属组的
策略是解析链的第一档（组策略 → 全局默认）。

分组对象的字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | integer | 服务端分配。 |
| `name` | string | |
| `token_quota_rules` | string | 规则数组的 JSON **文本**。`""` 表示**未配置**（回退全局默认）；`"[]"` 表示显式不限；其余为规范化后的规则 JSON。 |
| `global_token_quota_rules` | string | 组级全局配额覆盖。`""` = 未配置（回退站点规则与 `global_quota_scope`）；`"[]"` = 显式不限；否则为组自身规则，且**固定按组计量**。 |
| `created_at` / `updated_at` | string | RFC 3339 时间戳。 |

#### POST /admin/user-groups

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `name` | string | 否 | `""` | |
| `token_quota_rules` | string | 否 | `""` | 省略/空串 = 未配置；否则必须是合法 JSON 数组（会做规则校验与规范化）。 |
| `global_token_quota_rules` | string | 否 | `""` | 可选的组级全局配额覆盖；校验同上。配置后（含 `"[]"`）该组按自身规则、以组用量判定。 |

```bash
curl -X POST http://127.0.0.1:8080/admin/user-groups \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"name": "team-a", "token_quota_rules": "[{\"mode\":\"day\",\"limit\":1000000}]"}'
```

响应 `201 Created` —— 分组对象。

| 状态码 | 条件 |
|--------|------|
| 201 | 已创建。 |
| 400 | JSON 非法；`token_quota_rules` 不是 JSON 数组或规则校验失败。 |
| 500 | 数据库错误。 |

#### GET /admin/user-groups

```json
{"user_groups": [ /* 分组对象 */ ]}
```

#### GET /admin/user-groups/{id}

响应 `200 OK` —— 单个分组对象；不存在时 `404 group not found`。

#### PUT /admin/user-groups/{id}

局部更新。

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | |
| `token_quota_rules` | string | 校验同创建。 |
| `global_token_quota_rules` | string | 校验同创建；`""` 清除组级覆盖。 |

响应 `200 OK` —— 更新后的分组对象。

#### DELETE /admin/user-groups/{id}

只删除分组本身，**不**级联用户/密钥。`group_id` 变为悬空的用户回退全局默认规则。

响应 `200 OK`：

```json
{"ok": true}
```

### 3.6 用量查询

#### GET /admin/usage/summary

聚合 token 用量。查询参数：

| 参数 | 类型 | 说明 |
|------|------|------|
| `group_by` | string | `date`（默认）、`model`、`key`、`user` 或 `group`。 |
| `start` / `end` | string | `YYYY-MM-DD`，按业务时区解释；`end` 含当日整天。 |
| `user_id` / `group_id` / `api_key_id` / `ai_model_id` | integer | 可选过滤。 |

```json
{
  "group_by": "date",
  "rows": [
    {
      "date": "2026-09-01T16:00:00Z",
      "prompt_tokens": 120,
      "completion_tokens": 40,
      "total_tokens": 160,
      "prompt_cache_read_tokens": 0,
      "prompt_cache_write_tokens": 0,
      "request_count": 3,
      "currency": "",
      "cost_vendor_micros": 0,
      "cost_platform_micros": 500000
    }
  ],
  "total_cost_platform_micros": 500000,
  "platform_currency": "CNY"
}
```

`date` 字段仅在 `group_by=date` 时出现。无数据时 `rows` 为 `[]`。

金额字段（口径详见 §3.10）：

| 字段 | 说明 |
|------|------|
| `cost_platform_micros` | 结算币种金额（整数微元）。记账时刻已统一到 `platform_currency`，任何聚合维度都可相加。 |
| `cost_vendor_micros` | 模型币种金额（整数微元）。**仅 `group_by=model` 时有意义**；其余维度可能跨币种、不可相加，恒为 `0`。 |
| `currency` | 本行金额的币种。仅 `group_by=model` 时唯一；其余维度取 `MAX(currency)`。 |
| `mixed_currency` | 本行跨越多个计价币种时出现且为 `true`；此时金额要按模型分别看。 |
| `total_cost_platform_micros` / `platform_currency` | 信封级合计：当前返回各行结算金额之和及其币种。 |

#### GET /admin/usage/logs

逐请求用量明细，按时间倒序。接受与 `summary` 相同的时间区间与 id 过滤，外加 `page`
（默认 1）与 `page_size`（默认 50，上限 500）。

```json
{
  "logs": [
    {
      "id": 1,
      "api_key_id": 3,
      "user_id": 2,
      "group_id": 1,
      "ai_model_id": 1,
      "model": "glm-5.1",
      "provider": "zhipu",
      "prompt_tokens": 120,
      "completion_tokens": 40,
      "total_tokens": 160,
      "prompt_cache_read_tokens": 0,
      "prompt_cache_write_tokens": 0,
      "status_code": 200,
      "latency": 512,
      "currency": "CNY",
      "cost_vendor_micros": 500000,
      "cost_platform_micros": 500000,
      "created_at": "2026-09-01T16:00:00Z"
    }
  ],
  "total": 42,
  "page": 1,
  "page_size": 50
}
```

两者的错误：日期 / `group_by` / id 过滤非法返回 `400`；数据库错误返回 `500`。

#### GET /admin/usage/export

以文件流导出用量明细（字段与 `/admin/usage/logs` 一致）；过滤参数与 `logs` 同源；导出按批流式写出（不会整表载入内存）。

| 参数 | 类型 | 说明 |
|------|------|------|
| `format` | string | `csv`（默认）或 `json`；其余取值返回 `400 format must be csv or json`。 |
| `start` / `end` / `user_id` / `group_id` / `api_key_id` / `ai_model_id` | — | 语义同 `/admin/usage/logs`。 |

- `format=csv` → `text/csv` 附件，首行为表头（`id,api_key_id,user_id,group_id,ai_model_id,model,provider,prompt_tokens,completion_tokens,total_tokens,prompt_cache_read_tokens,prompt_cache_write_tokens,status_code,latency,cost_vendor_micros,cost_platform_micros,created_at`）。
- `format=json` → `{"logs": [ … ]}` 流式输出，行对象与 `logs` 一致（含 `cost_vendor_micros`、`cost_platform_micros` 与 `currency`）。

两种格式里的金额都是**整数微元**，单位与两列含义见 §3.10。

```bash
curl -OJ "http://127.0.0.1:8080/admin/usage/export?format=csv&start=2026-09-01" \
  -H "Authorization: Bearer <ADMIN_TOKEN>"
```

### 3.7 全局设置

全局设置以单行存储。

| 字段 | 类型 | 说明 |
|------|------|------|
| `default_token_quota_rules` | string | 规则数组的 JSON 文本，作用于无组策略的用户。 |
| `global_token_quota_rules` | string | 规则数组的 JSON 文本，即全局档。 |
| `global_quota_scope` | string | `site` 或 `group`，决定全局用量的统计口径。 |
| `user_key_limit` | integer | 每用户自助密钥数量上限，默认 `5`；`0` = 不限（见 §4.4）。 |
| `platform_currency` | string | 平台结算币种（ISO 4217 大写），默认 `CNY`。每笔请求的 `cost_platform_micros` 按记账时刻汇率折算到该币种并定死。 |

#### GET /admin/settings

返回当前设置，首次访问时创建默认行。默认为 `"[]"`、`"[]"`、`site`、`5`、`CNY`。

```json
{
  "default_token_quota_rules": "[]",
  "global_token_quota_rules": "[]",
  "global_quota_scope": "site",
  "user_key_limit": 5,
  "platform_currency": "CNY"
}
```

#### PUT /admin/settings

局部更新。

| 字段 | 类型 | 说明 |
|------|------|------|
| `default_token_quota_rules` | string | 必须是合法 JSON **数组**（空串或 `null` 被拒绝）。 |
| `global_token_quota_rules` | string | 必须是合法 JSON 数组。 |
| `global_quota_scope` | string | 必须为 `site` 或 `group`。 |
| `user_key_limit` | integer | 必须 `>= 0`；`0` 表示不限。 |
| `platform_currency` | string | 必须是 ISO 4217 三位字母；空串被拒绝（结算币种没有「未配置」态）。大小写不敏感，落库为大写。改动只影响此后记账的请求。 |

```bash
curl -X PUT http://127.0.0.1:8080/admin/settings \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"global_quota_scope": "group", "user_key_limit": 10}'
```

响应 `200 OK` —— 完整的设置对象。

| 状态码 | 条件 |
|--------|------|
| 200 | 已更新。 |
| 400 | JSON 非法；规则不是 JSON 数组；scope 非法；`user_key_limit < 0`；`platform_currency` 非法。 |
| 500 | 数据库错误。 |

### 3.8 webhook

webhook 订阅用于接收出站事件。投递是异步且带签名的；确切语义见附录。

webhook 对象的字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | integer | 服务端分配。 |
| `name` | string | |
| `url` | string | 接收端地址；必须为 `http` 或 `https`。 |
| `secret` | string | **恒为掩码**（只写不读回）。 |
| `event_types` | array | 订阅的事件类型；`[]` 表示订阅全部。 |
| `enabled` | bool | 禁用后不接收任何投递。 |
| `created_at` / `updated_at` | string | RFC 3339 时间戳。 |

#### POST /admin/webhooks

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `name` | string | 否 | `""` | |
| `url` | string | **是** | — | 必须能解析为带 host 的 `http`/`https` URL。 |
| `secret` | string | 否 | `""` | 用于签名投递（至多 256 字节）；绝不回读。 |
| `event_types` | array | 否 | `[]` | 非空字符串；`[]`（或省略）表示订阅全部。 |
| `enabled` | bool | 否 | `true` | |

```bash
curl -X POST http://127.0.0.1:8080/admin/webhooks \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"name": "siem", "url": "https://example.com/hooks/llm",
       "secret": "<HMAC_SECRET>", "event_types": ["quota.exceeded"]}'
```

响应 `201 Created` —— webhook 对象；`secret` 为掩码。

| 状态码 | 条件 |
|--------|------|
| 201 | 已创建。 |
| 400 | JSON 非法；URL 非法；`event_types` 含空串。 |
| 500 | 数据库错误。 |

#### GET /admin/webhooks

```json
{"webhooks": [ /* webhook 对象，secret 已掩码 */ ]}
```

#### GET /admin/webhooks/{id}

响应 `200 OK` —— 单个 webhook 对象（secret 掩码）；不存在时 `404 webhook not found`。

#### PUT /admin/webhooks/{id}

局部更新。

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | |
| `url` | string | 必须是合法 `http(s)` URL。 |
| `secret` | string | **空串表示保持不变**——该字段不可读回，因此也无法用空值清空。 |
| `event_types` | array | 整体替换订阅集合；`[]` 表示订阅全部。 |
| `enabled` | bool | |

响应 `200 OK` —— 更新后的 webhook 对象。

#### DELETE /admin/webhooks/{id}

响应 `200 OK`：

```json
{"ok": true}
```

#### GET /admin/webhooks/{id}/deliveries

该订阅的投递记录，按时间倒序分页（`page` 默认 1，`page_size` 默认 50，上限 500）。

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | integer | 投递 ID。 |
| `webhook_id` | integer | 所属订阅。 |
| `event_type` | string | 投递的事件类型。 |
| `status` | string | `pending`、`success` 或 `failed`。 |
| `attempts` | integer | 已尝试次数。 |
| `last_status` | integer | 最后一次尝试的 HTTP 状态码（网络错误为 `0`）。 |
| `last_error` | string | 最后一次尝试的错误文本。 |
| `created_at` / `updated_at` | string | RFC 3339 时间戳。 |

```json
{
  "deliveries": [
    {"id": 11, "webhook_id": 1, "event_type": "quota.exceeded", "status": "success",
     "attempts": 2, "last_status": 200, "last_error": "",
     "created_at": "2026-09-01T16:00:00Z", "updated_at": "2026-09-01T16:00:06Z"}
  ],
  "total": 1,
  "page": 1,
  "page_size": 50
}
```

### 3.9 导入

一次性导入存量数据（分组、用户、模型、密钥）。

**处理顺序**固定为 `user_groups → users → models → keys`：后者可引用同一请求中先前创建的实体。
`dry_run=true` 只做校验与计划，不写库。

**幂等键** —— 已存在的实体记为 `skipped`：

| 种类 | 幂等键 |
|------|--------|
| `groups` | `name` |
| `users` | `name` |
| `models` | `(provider, model_id)` |
| `keys` | `token`（哈希后比较） |

**请求体**

| 字段 | 类型 | 说明 |
|------|------|------|
| `dry_run` | bool | 只做计划，不写库。 |
| `groups[].name` | string | 必填。 |
| `user_groups[].token_quota_rules` | string | 可选；校验同 `/admin/user-groups`。 |
| `users[].name` | string | 必填。 |
| `users[].role` | string | `admin` 或 `user`；默认 `user`。 |
| `users[].group` | string | 分组**名**；空 = 不指定。必须能解析到库中/本次计划的分组。 |
| `models[].provider` / `model_id` | string | `model_id` 必填；`(provider, model_id)` 为幂等键。 |
| `models[].url` | string | 必填。 |
| `models[].api_key` | string | 可选。 |
| `models[].model_type` | string | `openai-completions`、`anthropic-messages`、`openai-responses` 或 `gemini-generate-content`。 |
| `models[].quota_day` / `enabled` | integer / bool | 可选。 |
| `keys[].token` | string | **明文**，必填。导入时哈希，明文不落库、不回显。 |
| `keys[].user` | string | 属主**名**，必填且必须可解析。 |
| `keys[].group` | string | 分组名；空 = 不指定。 |
| `keys[].name` / `max_tokens` | string / integer | 可选。 |
| `keys[].bindings[]` | array | `{model, role, api_key?}`，其中 `model` 是**模型 model_id**。非空时必须恰好一个 `primary` 且无重复；可选 `api_key` 为绑定级（自备 Key）密钥，加密落库。 |

**响应**

```json
{
  "dry_run": false,
  "summary": {"created": 4, "skipped": 1, "errors": 1},
  "details": [
    {"kind": "group", "ref": "team-a", "action": "created"},
    {"kind": "key", "ref": "sk-abcd1234…wxyz", "action": "error", "message": "user not found: bob"}
  ]
}
```

- `details[].kind` 取值 `group` / `user` / `model` / `key`。
- `details[].action` 取值 `created` / `skipped` / `error`；错误时给出 `message`。
- `details[].ref` 只承载非敏感标识（名字、`provider/model_id`、token 展示前缀），绝不含 token 明文。

单条失败会被记录且不中断其余条目；请求本身返回 `200`。导入的用户**不返回**访问 token
（内部生成随机 token 并丢弃明文）；需要时用 `POST /admin/users/{id}/rotate` 换发。

```bash
curl -X POST http://127.0.0.1:8080/admin/import \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "dry_run": true,
    "user_groups": [{"name": "team-a", "token_quota_rules": "[{\"mode\":\"day\",\"limit\":1000000}]"}],
    "users": [{"username": "bob", "role": "user", "group": "team-a"}],
    "models": [{"provider": "zhipu", "model_id": "glm-5.1", "url": "https://open.bigmodel.cn/api/paas/v4",
                "api_key": "<上游密钥>", "model_type": "openai-completions"}],
    "keys": [{"name": "bob-default", "user": "bob", "group": "team-a", "token": "sk-legacy-…",
              "bindings": [{"model": "glm-5.1", "role": "primary"}]}]
  }'
```

### 3.10 计费与金额

网关可以在 token 之外**按钱**计量：每个模型可配价目，每笔请求记录花了多少，配额可以按
金额限额，用量报表也能给出金额。这些都默认关闭——模型不配 `currency` 时一切照旧记 `0`。

#### 金额一律是整数微元

API 里所有金额都是**整数微元**（字段后缀 `_micros`），1 微元 = `1e-6` 个币种单位。
一个币种单位是 `1000000` 微元，因此 4 位小数（再多两位）都是精确的：`0.0025` 元记作
`2500`。服务端全程整数运算；前端负责除以 `1e6` 展示。

单价列的单位是**每百万 token 的微元数**：「1 元/百万 token」记作 `1000000`，
「0.02 元/百万」记作 `20000`。

#### 两个金额列、两种币种

每行用量带两个金额，都在记账时刻定死：

| 字段 | 币种 | 含义 |
|------|------|------|
| `cost_vendor_micros` | 记账时模型的 `ai_models.currency` | 这笔请求在厂商币种下花了多少。 |
| `cost_platform_micros` | `settings.platform_currency`（默认 `CNY`） | 同一笔花费按记账时刻生效的汇率折算到结算币种的结果。 |

两列只在请求记账时写一次。此后改价目、改模型币种、改汇率或改 `platform_currency`
**都不会改写历史**：老行保持当时记账的金额。模型币种与结算币种相同时无需汇率
（`platform = vendor`）。两者不同且记账时刻没有可用汇率时，模型币种金额照常落库，
平台金额记 `0`。

用量行不冗余币种：`cost_vendor_micros` 的币种要去 `ai_models.currency` 取。这也是为什么
汇总行的 `currency` 只描述 `cost_platform_micros`（以及 `group_by=model` 时的模型币种金额），
以及为什么 `cost_vendor_micros` 在 `group_by=model` 之外恒为 `0`。

#### 价目快照与六列原始价

价目以**追加式快照**保存，经下面的路由写入。每份快照总是六列**原始值**——高峰三档、
空闲三档，从不用乘数推导：

| 列 | 档位 |
|----|------|
| `peak_input_hit_micros` / `offpeak_input_hit_micros` | 输入（缓存命中） |
| `peak_input_miss_micros` / `offpeak_input_miss_micros` | 输入（缓存未命中） |
| `peak_output_micros` / `offpeak_output_micros` | 输出 |

一笔请求的金额是 `Σ(该档 token 数 × 该档单价_micros) / 1000000`，每笔只取一套三列：

1. **上游自报优先**：`use_upstream_cost` 为 `true`（默认）且上游响应带 `usage.cost > 0`
   时，直接采信该值（四舍五入到微元）作为 `cost_vendor_micros`，不再查价目。
2. **否则网关自算**，`enable_peak_pricing` 是总闸：
   - `enable_peak_pricing = false`（默认）→ **恒取空闲价三列**，即使配了 `peak_hours`
     或它本身有问题；
   - `enable_peak_pricing = true` → 按下面的峰谷规则判定；
   - 时段**无法评估**（没配 `peak_hours`、时区非法、窗口格式错、`days` 全越界）时按
     **高峰价**计价——fail-closed，绝不静默打折。只有显式开启峰谷的模型会走到这条路径。

#### 峰谷时段与计费日历

`peak_hours` 按价目快照配置，以 JSON **字符串**传递：

```json
{
  "timezone": "Asia/Shanghai",
  "windows": [
    {"days": [1, 2, 3, 4, 5], "start": "09:00", "end": "12:00"},
    {"days": [1, 2, 3, 4, 5], "start": "14:00", "end": "18:00"}
  ]
}
```

- `timezone` 为 IANA 名称；配了时区就必须有非空 `windows`。
- `days` 用 Go `time.Weekday` 编号（`0` = 周日 … `6` = 周六），指窗口**开始**的那一天。
- `start`/`end` 为 `HH:MM`（`24:00` 合法）。区间半开 `[start, end)`；`end <= start`
  表示跨午夜，此时次日 `[00:00, end)` 也算。
- `{}`（或空串 / `null`）表示无时段配置。
- **计费日历**修正星期规则：登记 `is_workday: false` 的日期（节假日）整日判为非高峰，
  登记 `true` 的（调休上班）照常按工作日判定。未登记的日期回落到「周一至周五为工作日」。
- 管理面写入时会拒绝无法评估的 `peak_hours`（时区非法、时钟串格式错、`days` 不可用），
  因为这样的配置会让该模型一直按高峰价计费。

#### 路由

| 路由 | 用途 |
|------|------|
| `GET /admin/models/{id}/prices` | 列价目快照，新版本在前：`{"prices": [...]}`。 |
| `POST /admin/models/{id}/prices` | 追加一份快照；成功返回 `201` 与价目行。`effective_at`（RFC 3339）必填；`currency` 缺省取模型币种。同模型同 `effective_at` 重复追加返回 `409`。 |
| `DELETE /admin/model-prices/{id}` | 按**价目行** id 删除：`{"ok": true}`。 |
| `GET /admin/billing-calendar?year=2026[&month=10]` | 该年（或该月）已登记的日期：`{"year": 2026, "days": [...]}`。`year` 缺省取业务时区当年。 |
| `PUT /admin/billing-calendar` | 按日期 upsert：`{"days": [{"date": "2026-10-01", "is_workday": false, "note": "国庆"}]}`。数组不可为空、`date` 不可重复、`is_workday` 每条必填；响应用同一形状。 |
| `DELETE /admin/billing-calendar/{date}` | 删除某天（`YYYY-MM-DD`）的登记：`{"ok": true}`。 |
| `GET /admin/currency-rates?from=USD&to=CNY` | 列该币种对的汇率快照（两个参数均必填）：`{"from_currency": "USD", "to_currency": "CNY", "rates": [...]}`。 |
| `POST /admin/currency-rates` | 追加一条汇率：`{"from_currency", "to_currency", "effective_at", "rate_micros"}` → `201`。同币种返回 `400`；同 `effective_at` 重复返回 `409`。 |
| `DELETE /admin/currency-rates/{id}` | 按 id 删除汇率行：`{"ok": true}`。 |

`rate_micros` 的含义是「1 单位 `from_currency` = `rate_micros/1e6` 单位 `to_currency`」
（FX 报价惯例，折算直接相乘），取值 `1` 到 `1000000000`。

每个写操作成功后都会失效数据面进程内的对应缓存，下一个请求就能看到新价目、新日历或新
汇率——无需重启。

```bash
# 给模型配价目：命中 0.3 元、未命中 9 元、输出 27 元/百万 token（高峰），空闲价为其一半
curl -X POST http://127.0.0.1:8080/admin/models/1/prices \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "effective_at": "2026-11-01T00:00:00Z",
    "peak_hours": "{\"timezone\":\"Asia/Shanghai\",\"windows\":[{\"days\":[1,2,3,4,5],\"start\":\"09:00\",\"end\":\"12:00\"}]}",
    "offpeak_input_hit_micros": 150000, "offpeak_input_miss_micros": 4500000, "offpeak_output_micros": 13500000,
    "peak_input_hit_micros": 300000,    "peak_input_miss_micros": 9000000, "peak_output_micros": 27000000
  }'

# 上游按美元计价、平台按人民币结算时，配一条汇率
curl -X POST http://127.0.0.1:8080/admin/currency-rates \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"from_currency": "USD", "to_currency": "CNY", "effective_at": "2026-11-01T00:00:00Z", "rate_micros": 7100000}'
```

#### 按金额限额的配额规则

配额规则新增可选的 `unit`：

| `unit` | 窗口用量与 `limit` 的比较 |
|--------|---------------------------|
| `""` / `"token"`（缺省） | `SUM(total_tokens)` —— 存量规则行为完全不变。 |
| `"cost"` | `SUM(cost_platform_micros)` 与 `limit × 1000000` 比较；`limit` 是结算币种的**整单位**金额，上限 `9000000000000`。 |

其余取值写入时被拒绝。`unit=token`（含空串）会被规范化掉，故存量规则文本逐字节不变。
逐条规则的视图里怎么体现见 `/api/user/quota`（§4.2）。

#### 配额拒绝带 `unit` 与 `currency`

配额拒绝的文案与状态码不变（仍为 `… token quota exceeded`、`-quota-exceeded-status`
默认 `429`、带 `Retry-After`），但错误体现在会标出计量口径：

```json
{
  "error": {
    "message": "User token quota exceeded",
    "type": "error",
    "code": 429,
    "unit": "cost",
    "currency": "CNY"
  }
}
```

`unit` 恒出现（`token` 或 `cost`；没写 `unit` 的规则报 `token`）。`currency` 只在金额
规则下出现，为平台结算币种。`/v1/messages` 仍是 Anthropic 信封，不带这两个键。

### 3.11 模型路由

一条**模型路由**就是一条规则：名称、**策略**、策略私有的 `config` JSON、**作用域**
（含作用域 ID）、**优先级**与启用开关。只有请求 `model: "auto"` 的调用会走路由；
指名真实模型的请求完全不受影响。

**哪条规则生效。** 作用域越具体越优先——`key` > `user` > `group` > `global`；
同一作用域内 `priority` 数字小者优先，重复的 `(scope, scope_id, priority)` 在写入时就
被拒绝，不会变成运行期的随机命中。未分组的密钥（`group_id = 0`）不匹配任何 `group`
规则。没有任何规则适用时，`auto` 返回 `400 model auto is not available`。

**策略是可插拔的。** `GET /admin/model-routers/strategies` 下发每种策略的配置字段，
客户端据此渲染表单，不需要硬编码任何策略。字段含 `key`、`type`
（`string` | `int` | `bool` | `enum` | `model`）、`title`、`description`、`default`、
`enum`、`min`、`max`、`required`。

| 路由 | 说明 |
|------|------|
| `GET /admin/model-routers` | 按匹配顺序列出规则：`{"routers": [...]}`。 |
| `GET /admin/model-routers/strategies` | 可用策略及其配置字段：`{"strategies": [...]}`；数据面未装配时返回 `503`。 |
| `POST /admin/model-routers` | 新建：`{"name", "strategy", "config", "scope", "scope_id", "priority", "enabled"}` → `201`。`enabled` 缺省为 `true`；`config` 可为对象或它的 JSON 字符串。 |
| `GET /admin/model-routers/{id}` | 读取单条规则。 |
| `PUT /admin/model-routers/{id}` | 局部更新；未提供的字段保持不变。仅改 `enabled` 不会扰动作用域与优先级。 |
| `DELETE /admin/model-routers/{id}` | 删除：`204`。由它路由的请求回退到各自密钥的主模型。 |
| `POST /admin/model-routers/{id}/preview` | 用一个真实密钥跑一次本规则的策略判断，不向上游发请求：`{"api_key_id", "request": {...}}` → `{"router_id", "strategy", "ai_model_id", "model_id", "cause", "fallback"}`。 |

**校验。** 结构性校验（名称与策略必填、作用域合法、非全局作用域必须给 scope id、
同作用域优先级唯一）在管理面完成；配置本身由策略的校验器检查（必填项、未知字段、
取值范围）。两者都通过才落库，因此配错的规则是 `400`，而不是运行期的意外。写入时
策略会规范化 `config`，落库的 JSON 自解释。

**策略。** `decision`：用一个决策协议模型判断候选里哪个最便宜且仍能胜任。其配置项为
`decision_model_id`（必填，须是 `model_type` 为决策协议的 `ai_models` id）、
`max_candidates`、`max_short_tokens`、`max_context_tokens`、`max_chars`、
`timeout_ms`、`cooldown_seconds`、`short_circuit_enabled`。

**计费与可观测性。** 决策调用以 `count_toward_quota = 0` 记入用量明细：成本可见，
但不消耗用户配额。`llm_usage_logs.router_id` 记录承接该请求的规则；实际路由到的模型
与命中的规则分别通过 `X-AIGate-Routed-Model` 与 `X-AIGate-Router-Id` 响应头返回。
响应体仍按客户端所问回显 `"auto"`。

### 3.12 请求日志

请求日志记录每个推理入口在客户端侧看到的原始请求/响应对。该能力**默认关闭**
（`-request-log=none`）：只有启用插件时才会注册下面两个路由，否则返回 `404`。仅管理员
可读（挂在既有 `/admin/` 鉴权子树下）。每个被采集的请求都会带一个
`X-AIGate-Request-Id` 响应头，取值与日志行的 `id` 一致。插件契约、各实现的配置、
保留期与「如何新增实现」见 [docs/request-log.zh-CN.md](request-log.zh-CN.md)。

#### GET /admin/request-logs

请求日志分页列表，按时间倒序。**列表行只含元数据**——不含请求/响应正文，也不含头列；
完整字段需调 `GET /admin/request-logs/{id}`。13 个筛选参数全部可选、可组合；`0` / 空串
表示「不过滤」。

| 参数 | 类型 | 说明 |
|------|------|------|
| `start` / `end` | string | 时间范围。`YYYY-MM-DD`（业务时区整天，`end` 含当日）或 RFC3339（`2026-11-25T10:30:00+08:00` / `...Z`）。两侧统一归一为半开 UTC 区间 `[start, end)`：日期形式的 `end` 靠 +1 天变成排他上界，RFC3339 形式的 `end` 本身即排他。取值含 `T` 时按 RFC3339 解析。`start > end` → `400 invalid time range`。 |
| `api_key_id` | integer | 按密钥过滤。管理面取出该密钥当前的 `proxy_token_hash` 后据此过滤；密钥不存在 → `404 api key not found`。与 `api_key_hash` 同时给出时以 `api_key_id` 为准。 |
| `api_key_hash` | string | 直接按凭据 sha256 hex 过滤。**密钥 rotate 之后，旧日志只能用本参数查到**，不能用 `api_key_id`。 |
| `model` | string | 精确匹配从请求体解析出的模型名。 |
| `path` | string | 精确匹配入口路径，如 `/v1/messages`。 |
| `status_code` | integer | 精确匹配 HTTP 状态码。 |
| `stream` | bool | `true` / `false`，只取流式或非流式行。 |
| `min_latency_ms` / `max_latency_ms` | integer | 延迟区间（毫秒，含端点）。`min > max` → `400 invalid latency range`。 |
| `q` | string | 全文，尽力而为：sqlite 用 `LIKE`、ES 用 `multi_match`，不承诺打分一致。 |
| `page` / `page_size` | integer | 默认 `1` / `50`，`page_size` 上限 `500`。`(page-1)*page_size + page_size` 不得超过共享的 `10000` 结果窗口，否则 `400 page out of range`。 |

```json
{
  "logs": [
    {
      "id": "0123456789abcdef0123456789abcdef",
      "created_at": "2026-11-25T08:12:34.512Z",
      "method": "POST",
      "path": "/v1/chat/completions",
      "model": "glm-5.1",
      "api_key_hash": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
      "status_code": 200,
      "latency_ms": 512,
      "stream": false,
      "error": "",
      "truncated": false
    }
  ],
  "total": 42,
  "page": 1,
  "page_size": 50
}
```

无匹配时 `logs` 为 `[]`（不会是 `null`）。两种实现返回同一套字段、同一套筛选与分页
语义。`created_at` 为 UTC；`error` 仅在 `status_code >= 400` 时填状态归类；
`truncated` 表示正文在 1 MiB 采集上限处被截断。

错误码：`400`（参数非法、时间/延迟区间非法或页码超出结果窗口）、`401`（凭据缺失或无效）、
`403`（非管理员）、`404`（`api_key_id` 指向的密钥不存在时返回 `api key not found`）、
`500`（存储错误）。

#### GET /admin/request-logs/{id}

返回单条请求的完整记录：列表行字段之外，补出 `request_body`、`response_body`、
`request_headers`、`response_headers`。`request_body` 是客户端发来的原始请求体文本；
`response_body` 是原始响应体文本（流式响应为原始 SSE 字节）。头字段是白名单头的
JSON 对象。凭据类头一律不入库。

路径参数 `id` —— 取自 `X-AIGate-Request-Id` 的 32 位十六进制请求 ID。

```json
{
  "id": "0123456789abcdef0123456789abcdef",
  "created_at": "2026-11-25T08:12:34.512Z",
  "method": "POST",
  "path": "/v1/chat/completions",
  "model": "glm-5.1",
  "api_key_hash": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
  "status_code": 200,
  "latency_ms": 512,
  "stream": false,
  "error": "",
  "truncated": false,
  "request_body": "{\"model\":\"glm-5.1\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}",
  "response_body": "{\"id\":\"chatcmpl-1\",\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}",
  "request_headers": "{\"Content-Type\":[\"application/json\"],\"User-Agent\":[\"openai-python/1.0\"]}",
  "response_headers": "{\"Content-Type\":[\"application/json\"],\"X-AIGate-Request-Id\":[\"0123456789abcdef0123456789abcdef\"]}"
}
```

错误码：`400`（id 为空）、`401`（凭据缺失或无效）、`403`（非管理员）、
`404`（`request log not found`）、`500`（存储错误）。

## 4. 用户面

前缀 `/api/user/*`。任意启用用户 token（`user` 或 `admin`）均可访问。`user` 角色 token
被强制作用于本人数据（其中的 `user_id` 被忽略）；`admin` 角色 token 可用 `?user_id=`
指定其他用户。

### 4.1 GET /api/user/me

返回调用者身份，含分组名（未分组或分组已删除时为空串）。

```json
{"id": 2, "username": "alice", "role": "user", "group_id": 1, "group_name": "team-a"}
```

### 4.2 GET /api/user/quota

目标用户生效规则的逐条视图（经由组策略 → 全局默认链解析）。

| 字段 | 类型 | 说明 |
|------|------|------|
| `user_id` | integer | 解析后的目标用户。 |
| `usages[]` | array | 每条生效规则一项。 |
| `usages[].rule_index` | integer | 规则在解析集合中的序号。 |
| `usages[].mode` | string | `day`、`month`、`year` 或 `custom`。 |
| `usages[].limit` | integer | `-1` 表示参与窗口计算但不限用量。`unit=cost` 时是结算币种的整单位金额（见 §3.10）。 |
| `usages[].unit` | string | `token` 或 `cost`，恒为字面量：没写 `unit` 的规则报 `token`。 |
| `usages[].used` | integer | 当前窗口用量：`unit=token` 时为 token 数，`unit=cost` 时为结算币种**微元**。 |
| `usages[].active` | bool | 规则当前是否生效。 |
| `usages[].period_start` / `period_end` | integer \| null | 窗口起止（Unix 秒）；`end` 为 `null` 表示无终止。 |

```json
{
  "user_id": 2,
  "usages": [
    {"rule_index": 0, "mode": "day", "limit": 1000000, "unit": "token", "used": 25310,
     "active": true, "period_start": 1758038400, "period_end": 1758124800}
  ]
}
```

### 4.3 用量查询

`GET /api/user/usage/summary`、`GET /api/user/usage/logs` 与
`GET /api/user/usage/by-model` 与管理面对应端点同源，但行级隔离：`user` 角色 token
无论带什么 `user_id` 过滤都只能看到自己的用量。

| 端点 | 说明 |
|------|------|
| `GET /api/user/usage/summary` | 参数与响应同 `/admin/usage/summary`（`group_by`、`start`/`end`、id 过滤）。 |
| `GET /api/user/usage/logs` | 同 `/admin/usage/logs`，带 `page` / `page_size` 分页（时间倒序）。 |
| `GET /api/user/usage/by-model` | 同 `summary`，但固定按 `model` 聚合；`group_by` 参数被忽略。 |

### 4.4 自助密钥

`/api/user/keys` 让用户自行签发与管理数据面密钥。两条护栏：

- **数量上限** —— `settings.user_key_limit`（默认 `5`；`0` = 不限）。超限返回
  `400 key limit reached (limit N)`。
- **模型范围** —— 新建或更新的密钥只能绑定「该用户名下**任意**已有密钥出现过的模型」。
  绑定集合外模型返回 `400 model not available for this user`；若用户完全没有可绑定模型，
  返回 `400 no available models, please contact an administrator`。

隔离：密钥不存在**或**不属于本人一律返回 `404 key not found`，避免通过 id 探测。

方法：`POST /api/user/keys`、`GET /api/user/keys`、`GET /api/user/keys/{id}`、
`PUT /api/user/keys/{id}`、`DELETE /api/user/keys/{id}`、
`POST /api/user/keys/{id}/rotate`。

密钥对象形状与管理面一致（§3.3）。`token` 仅在创建与 rotate 时返回。

#### POST /api/user/keys

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `name` | string | 否 | `""` | |
| `max_tokens` | integer | 否 | `0` | `<= 0` 表示不封顶。 |
| `bindings` | array | 否 | 无 | `{ai_model_id, role, sort_order, api_key?}`；非空时必须恰好一个 `primary`，且每个模型都在用户范围内。可选 `api_key` 为用户自有上游密钥（自备 Key 模型用；加密落库、不回显）。 |

`token_quota_rules`（密钥级配额）在用户面**只读**：接口会返回该字段，但用户面的写入会被忽略（仅管理员可配置密钥级配额）。

```bash
curl -X POST http://127.0.0.1:8080/api/user/keys \
  -H "Authorization: Bearer <USER_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-key", "max_tokens": 4096,
       "bindings": [{"ai_model_id": 1, "role": "primary"}]}'
```

响应 `201 Created` —— 带一次性 `token` 的密钥对象。

| 状态码 | 条件 |
|--------|------|
| 201 | 已创建。 |
| 400 | JSON 非法；已达数量上限；模型超出允许范围；无可绑定模型；绑定集合非法。 |
| 404 | 目标用户不存在。 |

#### GET /api/user/keys

```json
{"keys": [ /* 密钥对象，不含 token */ ]}
```

#### GET /api/user/keys/{id}

响应 `200 OK` —— 单个密钥对象（无 `token`）；不存在或非本人时 `404 key not found`。

#### PUT /api/user/keys/{id}

局部更新。

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | |
| `max_tokens` | integer | |
| `enabled` | bool | |
| `bindings` | array | 出现时整体替换绑定，并重新按模型范围校验。 |

#### DELETE /api/user/keys/{id}

响应 `200 OK`：

```json
{"ok": true}
```

#### POST /api/user/keys/{id}/rotate

换发新的 `sk-` 凭据，旧凭据立即失效。响应 `200 OK` —— 带一次性 `token` 的密钥对象。

#### GET /api/user/keys/{id}/usage

单个密钥的按日用量趋势（与 `/api/user/usage/summary` 同一套日聚合）。`user` 角色只能查询自己名下的密钥（其余返回 `404 key not found`）；`admin` 可用 `?user_id=` 查询任意密钥。

| 参数 | 类型 | 说明 |
|------|------|------|
| `start` / `end` | string | `YYYY-MM-DD`（业务时区，含首含尾）；两者都不传时默认**最近 30 天**。 |

```json
{
  "key_id": 3,
  "group_by": "date",
  "start": "2026-09-03",
  "end": "2026-10-02",
  "rows": [
    {"date": "2026-10-02T00:00:00Z", "prompt_tokens": 120, "completion_tokens": 40,
     "total_tokens": 160, "prompt_cache_read_tokens": 0, "prompt_cache_write_tokens": 0,
     "request_count": 1}
  ]
}
```

## 5. 健康检查

### GET /healthz

无需鉴权。

响应 `200 OK`：

```json
{"status": "ok"}
```

## 6. 构建信息

### GET /version

无需鉴权。返回当前运行二进制的构建与启动身份，供运维核对一次部署实际服务的是哪份代码、哪个 commit。
响应只含构建元数据，不含任何配置、依赖或内部端点信息。

响应 `200 OK`：

```json
{
  "version": "v0.1.0",
  "commit": "b5744933e2f316b6d50f780ed7f7c7fb1cb87dcc",
  "build_time": "2026-11-23T03:49:23Z",
  "dirty": true,
  "go_version": "go1.24.12",
  "started_at": "2026-11-23T05:56:29Z"
}
```

| 字段 | 含义 |
|------|------|
| `version` | 构建期注入的字符串（`-ldflags`），未注入时回落到编译内置默认值。`make build` 注入的是 `git describe --tags --always --dirty`（例如 `v0.1.0-59-g6d75021-dirty`），**不是**严格 SemVer，不要按 SemVer 解析。 |
| `commit` | 完整 git commit；优先取注入值，其次取 Go 工具链写入的 VCS 元数据，都没有时为 `unknown`。 |
| `build_time` | 构建（即 commit）时间，RFC3339 UTC；不可得时为 `unknown`。 |
| `dirty` | 构建时工作区是否有未提交改动；为 `true` 说明该二进制不对应任何 git commit。只对直接在工作区构建出的二进制有意义：Docker 与 CI 构建复制的是干净树，恒为 `false`。 |
| `go_version` | 编译所用的 Go 工具链版本。 |
| `started_at` | 进程启动时间，RFC3339 UTC；未记录时为 `unknown`。 |

## 7. 附录

### 状态码汇总

| 状态码 | 出现位置 | 含义 |
|--------|----------|------|
| 200 | 全部层面 | 成功；`DELETE` 返回 `{"ok": true}`。 |
| 201 | 管理面写接口（models、credentials、keys、users、groups、webhooks）与用户面密钥创建 | 资源已创建。 |
| 400 | 全部层面 | 请求体格式错误、路径 id 非法，或字段/业务校验失败。 |
| 401 | 全部层面 | 凭据缺失、格式错误、不存在或已禁用。 |
| 403 | 管理面 | 凭据有效但用户角色不是 `admin`。 |
| 404 | 管理面 / 用户面 | 目标资源不存在（或不属于本人）。 |
| 409 | 管理面 | 凭据仍被模型引用，无法删除；同一 `effective_at` 的价目或汇率已存在。 |
| 429 / 403 | 数据面 | 配额超限；确切状态码由 `-quota-exceeded-status` 决定（默认 `429`），并带 `Retry-After` 头。响应体带 `unit`（`token` / `cost`），金额规则另带 `currency`（§3.10）。 |
| 500 | 全部层面 | 内部错误。 |
| 502 | 数据面 | 上游连接失败，或上游响应无法转换。 |

### 鉴权失败语义：401 vs 403

| 条件 | 状态码 | `message` |
|------|--------|-----------|
| 头缺失、非 `Bearer …`，或 token 不存在 | 401 | `missing or invalid bearer token` |
| token 有效但用户被禁用 | 401 | `user is disabled` |
| token 有效、用户启用，但角色不允许 | 403 | `insufficient role` |

数据面上，未携带凭据返回 `401 missing API key`；「密钥不存在」与「密钥被禁用」不可区分，
均返回 `401 invalid API key`。在 `/v1/messages` 上，同类情形返回 `401` 且
`error.type = authentication_error`。

### webhook 事件交付

**信封。** 每次投递向订阅 URL `POST` 一个 JSON body：

```json
{
  "id": "11",
  "type": "quota.exceeded",
  "created_at": "2026-09-01T16:00:00Z",
  "data": { }
}
```

`id` 即投递 ID（与 `/admin/webhooks/{id}/deliveries` 的行 id 一致）；`created_at` 为 UTC。

**请求头。**

| 头 | 值 |
|----|----|
| `Content-Type` | `application/json` |
| `X-Gateway-Event` | 事件类型（如 `request.completed`） |
| `X-Gateway-Delivery` | 投递 ID |
| `X-Gateway-Signature` | `sha256=<hex(hmac-sha256(secret, 原始 body))>` |

校验签名：用订阅 secret 对**原始请求 body** 计算 HMAC-SHA256 并 hex 编码，与 `sha256=`
之后的值比较。

**投递语义。**

- 投递异步、由单 consumer 串行处理，绝不阻塞数据面。
- 单次尝试超时 **10 秒**；非 2xx 响应或网络错误都会触发重试。
- **至多 3 次尝试**；第 2、3 次尝试前分别等待 **1s** 与 **5s**。
- 每次尝试都更新投递记录（`attempts`、`last_status`、`last_error`），最终落终态
  `success` 或 `failed`。
- 事件队列（`-webhook-buffer`，默认 `256`）有界：队满时新事件被丢弃并计数
  （`webhook_dropped`），而非阻塞调用方。
- 重试复用同一投递 ID，因此接收端可能多次收到同一事件。请按**至少一次**语义处理，并用
  `X-Gateway-Delivery` 去重。最后一次尝试失败后事件不再重发。

**事件类型与 `data` 字段。**

| `type` | 触发时机 | `data` 字段 |
|--------|----------|-------------|
| `request.completed` | 一次数据面转发（含流式）完成。 | `api_key_id`、`user_id`、`group_id`、`model`、`provider`、`status_code`、`latency_ms`、`usage`（`prompt_tokens`、`completion_tokens`、`total_tokens`、`prompt_cache_read_tokens`、`prompt_cache_write_tokens`） |
| `quota.exceeded` | 请求因配额校验被拒绝。 | `api_key_id`、`user_id`、`group_id`、`model`、`kind`（`global` / `model` / `user` / `key`）、`limit`、`used` |
| `key.disabled` | 管理员禁用或删除密钥。 | `key_id`、`name`、`user_id`、`reason`（`disabled` / `deleted`） |
| `key.expired` | 密钥在 `expires_at` 之后被使用（首次拒绝时发出，同一有效期内只发一次）。 | `key_id`、`name`、`user_id` |

只有 `enabled=true` 且 `event_types` 为空或包含该类型的订阅才会收到。

### 密钥加密

网关可用 AES-256-GCM 对密钥类列做静态加密：

| 事项 | 说明 |
|------|------|
| 覆盖列 | `ai_models.api_key`、`api_key_models.api_key`（绑定级自备密钥）、`credentials.secrets_json`、`webhooks.secret`。 |
| 密文格式 | `enc:v1:` + base64(nonce ‖ ciphertext)；带前缀的值按密文处理，其余按明文。 |
| 主密钥 | 64 位 hex 或 base64（解码后 32 字节），经 `-master-key` / `AIGATE_MASTER_KEY` 注入。 |
| 未配置 | 明文存储并在启动时输出告警；读取照常可用。 |
| 已配置 | 新增/更新值加密；存量明文行在迁移前保持原样。 |
| 迁移 | `-migrate-encrypt` 就地加密存量明文行后退出，输出 `migrated`/`skipped`/`failed`；幂等且需主密钥。 |

完整操作手册（含「务必备份主密钥」的告诫——主密钥丢失将使所有密文不可读）见
[docs/deploy.zh-CN.md](deploy.zh-CN.md)。

### 运行须知

| 事项 | 说明 |
|------|------|
| 上游 URL 拼接 | 见下方「上游 URL 拼接规则」。`url` 末尾的斜杠会被去除。 |
| SSRF 策略 | 出站请求（上游与 webhook 接收端）默认校验，会**拒绝私网/回环地址**。本地端点需以 `-allow-private-upstreams`（或 `AIGATE_ALLOW_PRIVATE_UPSTREAMS=true`）启动。 |
| 请求体大小 | 每请求 1 MiB；超限直接拒绝（数据面 413、管理面/用户面 400），不截断。 |
| 业务时区 | 仅影响配额窗口与用量/日期记账口径，不改变 API 响应。 |
| 用量写入 | 用量异步写入；极端负载下可能丢失少量条目，网关不承担计费真源职责。 |
| 投递保留 | `webhook_deliveries` 记录无限保留——没有 TTL、裁剪或表容量上限。 |

#### 上游 URL 拼接规则

`ai_models.model_type` 的取值 `url` 语义**不一致**——配置模型时务必逐条对照：

| `model_type` | `url` 应写成 | 网关实际请求的地址 |
|--------------|--------------|--------------------|
| `openai-completions` | `<host>/v1` | `<url>/chat/completions` |
| `openai-responses` | `<host>/v1` | `<url>/responses` |
| `anthropic-messages` | `<host>`（**不含** `/v1`） | `<url>/v1/messages` |
| `openai-decisions` | `<host>/v1` | `<url>/decisions` |
| `systemone` | `<host>/v1` | `<url>/systemone` |
| `gemini-generate-content` | `https://generativelanguage.googleapis.com/v1beta` | `<url>/models/{model}:generateContent`（流式 `<url>/models/{model}:streamGenerateContent?alt=sse`） |

`url` 末尾的斜杠会被去除。因此：
`https://api.openai.com/v1` 配 `openai-responses` → `https://api.openai.com/v1/responses`；
同一条 `url` 配 `anthropic-messages` → 变成 `https://api.openai.com/v1/v1/messages`（错误）。

`model_type` 为空或未知取值时按 `openai-completions` 处理（兼容历史数据）；新建模型建议显式填写。

#### Gemini 上游（`gemini-generate-content`）的限制

- **只支持单候选**：`n > 1` 返回 400。
- **图片**：`image_url` 只能是 `https://` 且带文件扩展名（`.png`/`.jpg`/`.jpeg`/`.webp`/`.gif`/`.heic`/`.heif`），
  或 base64 data URI。网关**不会下载**图片，`https` URL 由 Google 抓取；
  扩展名缺失或 `http://` 直接 400。
- **思考签名**：Gemini 3.x / 2.5 的多轮工具调用依赖 `thoughtSignature`，网关把它编进
  返回的 `tool_calls[].id`（形如 `call_xxx_ts_<签名>`）。客户端回传时需原样带回 `id`。
  经 `/v1/responses` 入口时签名会降级为 Gemini 的跳过哨兵（该入口不保留 reasoning）。
- **不支持**：音频、视频、PDF 输入；`googleSearch` / `codeExecution` 等 Google 侧工具；
  Vertex AI 端点；`cachedContents` 显式缓存管理。

#### Responses 有状态能力与保留期

| 事项 | 说明 |
|------|------|
| 存储开关 | `-allow-response-storage`（`AIGATE_ALLOW_RESPONSE_STORAGE`，默认 `true`）。置 `false` 后全部请求按 `store:false` 处理，`store:true` / `previous_response_id` / `conversation` / `background` 一律 `400`。 |
| 保留期 | `-response-retention-days`（`AIGATE_RESPONSE_RETENTION_DAYS`，默认 `7`）：`response_states` 行的保留天数；`0` = 永不清理；取值须在 `0`–`36500` 之间。网关启动时清理一次，此后每小时清理一次。 |
| 后台超时 | `-response-background-timeout`（`AIGATE_RESPONSE_BACKGROUND_TIMEOUT`，默认 `30m`）：`background:true` 任务的总时长上限；须为正数。 |
| 落库内容 | 每行保存请求体改写后的 `items` 累积快照（不含 `instructions`）与响应体。**对话正文属于用户数据**，请据保留期与合规要求配置开关。 |
| 过期 | 行被清理后，其 ID 不能再作为 `previous_response_id`（`404`）；以它为最新行的 `conversation` 回退到上一行。 |

### 请求日志配置

请求日志是可替换实现的能力，**默认关闭**。核心只保留下面三项「如何把配置交给插件」
的设置；JSON 内容归实现所有（见
[docs/request-log.zh-CN.md](request-log.zh-CN.md)）。

| Flag | 环境变量 | 默认值 | 说明 |
|------|----------|--------|------|
| `-request-log` | `AIGATE_REQUEST_LOG` | `none` | 实现名：`none` / `sqlite` / `es`；未知名字在启动阶段拒绝。 |
| `-request-log-config` | `AIGATE_REQUEST_LOG_CONFIG` | （空） | 内联 JSON 对象，原样交给实现；非空时必须是合法 JSON 对象。 |
| `-request-log-config-file` | `AIGATE_REQUEST_LOG_CONFIG_FILE` | （空） | JSON 配置文件路径，优先于 `-request-log-config`；ES 凭据建议放这里，避免出现在命令行。 |

核心存储 schema 未变，**不需要 SQL 迁移**。
