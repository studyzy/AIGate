# API Reference

[English](API.md) | [简体中文](API.zh-CN.md)

Tencent Cloud AIGate is a self-hosted LLM API gateway: a **pure API service** (no web UI). It answers four surfaces — the data plane, the admin plane, the user plane, and an unauthenticated health check.

All examples below use the base URL `http://127.0.0.1:8080`.

## 1. Overview

### Planes and authentication

| Plane | Path prefix | Authentication | Credential |
|-------|-------------|----------------|------------|
| Data plane | `/v1/*` | `Authorization: Bearer <token>`; `/v1/messages` also accepts `x-api-key` | Data-plane key (`sk-…`) |
| Admin plane | `/admin/*` | `Authorization: Bearer <token>` | Access token of a user with `role=admin` (`gk-…`) |
| User plane | `/api/user/*` | `Authorization: Bearer <token>` | Access token of any enabled user (`gk-…`) |
| Health check | `/healthz` | none | — |
| Build info | `/version` | none | — |

The `Authorization` header value must start with `Bearer ` (case-insensitive scheme match on the admin plane; the data plane requires the exact `Bearer ` prefix). The token is trimmed of surrounding whitespace. Tokens are never accepted from query strings or other headers. On `/v1/messages`, an `x-api-key` header takes precedence over `Authorization` when both are present.

On the user plane, a `user`-role token is forced to act on its own data; an `admin`-role token may target another user with `?user_id=`.

### Error response format

Every error response — on all planes — uses a single shape, with `code` equal to the HTTP status code:

```json
{
  "error": {
    "message": "invalid API key",
    "type": "error",
    "code": 401
  }
}
```

The one exception is the data plane's `/v1/messages`, which uses the Anthropic error envelope (see that endpoint).

Optionally, an error body may carry extra keys next to the mandatory three. Quota rejections add `unit` (`token` or `cost`) and, for cost rules, `currency` — see §3.10.

### Admin token bootstrap

On startup the gateway ensures that at least one `role=admin` user exists:

| Situation | Behaviour |
|-----------|-----------|
| An admin already exists | Nothing is created. If `-init-admin-token` was supplied, it is ignored and a log line is emitted. |
| No admin, `-init-admin-token <TOKEN>` supplied | The user `admin` is created with that token. |
| No admin, no token supplied | A token is generated and **printed once to stdout** as `initial admin token (shown once): gk-…` |

### Token security semantics

| Rule | Detail |
|------|--------|
| Storage | Only the SHA-256 hash and a display prefix (`sk-abcd1234…wxyz`) are persisted. Plaintext is never stored. |
| One-time disclosure | The plaintext is returned exactly once — at creation and at rotation. No list or detail endpoint ever echoes it. |
| Admin `api_key` | The upstream provider credential (`ai_models.api_key`) is never returned by any response. |
| Credential secrets | Credential values are returned masked on every read (see §3.2); the plaintext is never echoed back. |
| Webhook secrets | Webhook secrets are write-only: the read side always returns a mask (see §3.8). |
| Rotation | `rotate` replaces the stored token hash and prefix; the previous credential stops working immediately. |
| Presentation | `token` is omitted from responses outside creation/rotation (`omitempty`). |

### Request body limits

- All JSON request bodies are capped at **1 MiB**. Larger bodies are rejected outright — never truncated: the data plane returns `413`, the admin and user planes return `400`, all with `request body too large`.
- Write endpoints require a JSON object body. An empty body yields `400`: the data plane reports `invalid json body`; the admin and user planes append the parser cause, e.g. `invalid json body: EOF`.

## 2. Data plane

### POST /v1/chat/completions

Forward an OpenAI-style chat completion request to the upstream model bound to the presented key. Streaming and non-streaming are both supported; for `anthropic-messages` models the request/response are translated to and from the Anthropic Messages API.

**Authentication:** `Authorization: Bearer sk-…`

**Request fields**

The body is an OpenAI Chat Completions object. The gateway reads and rewrites only the fields below; everything else is forwarded untouched.

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `model` | string | no | Target model. If present and non-empty it must match a model bound to this key (see resolution rules). If absent or empty, the key's `primary` binding is used. |
| `stream` | bool | no | `true` selects SSE streaming. |
| `max_tokens` | integer | Truncated down to the key's `max_tokens` limit when that limit is `> 0`. |
| `max_completion_tokens` | integer | Truncated by the same ceiling. |
| any other field | — | no | Passed through as-is. |

**Gateway transformations (before forwarding)**

| Transformation | Rule |
|----------------|------|
| Model substitution | The body's `model` is replaced with the resolved upstream `model_id` (original casing preserved). The client's value is never forwarded verbatim. |
| `max_tokens` cap | Applies only when the key's `max_tokens > 0`. Both `max_tokens` and `max_completion_tokens` are capped if present. If **neither** is present, `max_tokens` is injected at the limit. If the limit is `<= 0`, the body is left untouched. The model's `max_output_tokens` is a capability declaration and never caps a request. |
| Parameter normalisation | For OpenAI-type upstreams, GPT-5+ and o-series models receive the parameter under `max_completion_tokens` instead of `max_tokens` (and vice versa for other models). |
| Streaming | For `stream: true`, an OpenAI-type upstream additionally receives `stream: true` and `stream_options.include_usage: true`. |
| Custom headers | See "Custom header passthrough" below. |

**Model resolution rules**

| Client `model` | Key bindings | Result |
|----------------|--------------|--------|
| present, matches a bound **enabled** model | — | Forward using that model's upstream `model_id`. Matching is case-insensitive, and `/` and `:` are treated as equivalent to `-`. A `provider/model_id` prefix (e.g. `qcloudlkeap/deepseek-v3.2`) disambiguates models with the same `model_id` from different providers; the prefix may also be the concatenated form written when a binding is saved (`{provider}-{model_id}`, e.g. `hatchery-glm-4-plus/glm-4-plus`), matched the same way. |
| present, no match | — | `400 requested model is not bound to this key`. There is **no fallback** to primary. |
| absent or empty string | a binding has `role=primary` | Forward using the primary model. |
| absent or empty string | no `primary` (or no enabled binding) | `400 no available model for this key`. |

Only bindings whose model is `enabled=true` participate in resolution.

The same resolution, quota, and `max_tokens` logic applies to `/v1/messages`.

**Custom header passthrough**

A model may declare `custom_http_headers` (a JSON object of string values). Passthrough is all-or-nothing:

| Condition | Behaviour |
|-----------|-----------|
| Every configured header is present on the inbound request with an **exactly equal** value | Those headers are forwarded to the upstream (canonicalised name). |
| Any configured header is missing or has a different value | **No** custom header is forwarded at all, and a warning is logged. |
| Model has no `custom_http_headers` | No custom header handling. |

**Request example**

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

**Response — non-streaming**

The upstream status code and response body are passed through **verbatim** (no re-wrapping). A `200` therefore contains whatever the upstream returned, e.g.:

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

Upstream error statuses (e.g. `400`, `429`) are also passed through unchanged, with the upstream body.

**Response — streaming (`stream: true`)**

The response uses `Content-Type: text/event-stream` with `Cache-Control: no-cache` and `X-Accel-Buffering: no`, and is flushed incrementally. For an `anthropic-messages` upstream the Anthropic SSE events are converted into OpenAI `chat.completion.chunk` frames and terminated with `data: [DONE]`. If the upstream returns a non-`200` status before the stream starts, that status and body are passed through as JSON. Usage is recorded after the stream ends.

**Errors**

| Status | `message` | Cause |
|--------|-----------|-------|
| 401 | `missing API key` / `invalid API key` | No `Authorization` header (`missing`) vs malformed, unknown, or disabled key (`invalid`). |
| 400 | `failed to read request body` | The request body could not be read. |
| 413 | `request body too large` | The request body exceeds 1 MiB; rejected outright, never truncated. |
| 400 | `invalid json body` | The body is not a JSON object. |
| 400 | `requested model is not bound to this key` | An explicit `model` did not match any enabled binding. |
| 400 | `no available model for this key` | No `model` supplied and no enabled `primary` binding exists. |
| 429 / 403 | `Global token quota exceeded` / `Model daily token quota exceeded` / `User token quota exceeded` / `Key token quota exceeded` | A quota check rejected the request. The status is `-quota-exceeded-status` (default `429`) and a `Retry-After` header is set. The body also names the measurement: `unit` (`token` or `cost`) and, for cost rules, `currency` — see §3.10. |
| 500 | `internal server error` | Failed to resolve the model (database error). |
| 500 | `streaming not supported by server` | `stream: true` but the server cannot flush the response. |
| 502 | `failed to connect to LLM backend` | Non-streaming: DNS/connect/TLS/timeout failure reaching the upstream. Details (including upstream addresses) are logged server-side only. |

### POST /v1/messages

Anthropic Messages ingress. Forward an Anthropic-format request to the upstream model bound to the presented key, preserving Anthropic semantics for Anthropic upstreams and translating for OpenAI-compatible ones. Streaming and non-streaming, text, images, and tool calls are supported.

**Authentication:** `x-api-key: sk-…` (preferred) or `Authorization: Bearer sk-…`. Authentication failures always use the Anthropic error envelope.

**Request fields**

The body is an Anthropic Messages object. The gateway validates and rewrites only the fields below.

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `model` | string | **yes** | Must be non-empty. Resolved against the key's bindings exactly as on `/v1/chat/completions`. |
| `max_tokens` | integer | **yes** | Must be an integer `> 0`. Truncated down to the key's `max_tokens` limit when that limit is `> 0`. |
| `stream` | bool | no | `true` selects Anthropic SSE streaming. |
| `messages` / `system` / `tools` / `tool_choice` / `thinking` / `stop_sequences` | — | no | See the two upstream paths below. |
| any other field | — | no | Preserved in passthrough; otherwise forwarded as far as the conversion allows. |

Validation order: `model` missing → `400 model is required`; `max_tokens` missing or `<= 0` → `400 max_tokens is required`.

**Upstream paths**

The resolved model's `model_type` selects one of two paths:

| Upstream | Behaviour |
|----------|-----------|
| `anthropic-messages` | **Native passthrough.** The inbound body is forwarded unchanged except that `model` is replaced with the resolved upstream ID and `max_tokens` is capped by the key limit. Anthropic-only fields such as `cache_control` are therefore preserved end to end. Upstream responses (JSON or SSE) are returned verbatim. |
| `openai-completions` (and any other type, including `openai-responses`) | **Translation.** The Anthropic request is converted to an OpenAI request (`system` → a system message, images → data URIs, `tool_use`/`tool_result` → `tool_calls`/`role=tool`, `tools`/`tool_choice`/`stop_sequences` mapped), and the OpenAI response or SSE stream is translated back into Anthropic format. |

Passthrough request headers: `x-api-key` (resolved upstream key), `anthropic-version` (inbound value, defaulting to `2023-06-01`), `anthropic-beta` (forwarded when present), plus any matched `custom_http_headers`.

Upstream timeouts: 5 minutes non-streaming, 30 minutes streaming.

**Streaming event sequence**

For the translation path, the SSE stream is a well-formed Anthropic event sequence:

```text
message_start → content_block_start → content_block_delta* → content_block_stop
             → (further blocks) → message_delta → message_stop
```

The passthrough path forwards the upstream's own Anthropic event stream unchanged. Either way, usage is recorded after the stream ends (input/cache counts come from `message_start.message.usage`, output from `message_delta.usage`).

**Response — non-streaming**

A `200` returns the Anthropic Messages response object (verbatim for passthrough; translated from the OpenAI response for the conversion path).

**Errors**

All errors use the Anthropic envelope:

```json
{
  "type": "error",
  "error": {"type": "invalid_request_error", "message": "max_tokens is required"}
}
```

| Status | `error.type` | Cause |
|--------|--------------|-------|
| 401 | `authentication_error` | Missing or invalid/disabled key. |
| 400 | `invalid_request_error` | Unreadable body, invalid JSON, missing `model`, or missing/zero `max_tokens`; also a translation failure. |
| 413 | `invalid_request_error` | The request body exceeds 1 MiB; rejected outright, never truncated. |
| 429 / 403 | `rate_limit_error` | Quota exceeded (same status as configured by `-quota-exceeded-status`); a `Retry-After` header is set. |
| 500 | `api_error` | Internal error, or streaming not supported by the server. |
| 502 | `api_error` | Could not connect to the upstream, or the upstream response could not be translated. |

**Known limitations**

- The translation path (OpenAI-compatible upstream) is **lossy** for fields that have no OpenAI equivalent — `cache_control` in particular is dropped. Use an `anthropic-messages` upstream when cache-control breakpoints matter.
- Unknown request fields are ignored rather than rejected (forward compatible).

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

List the models available to the presented key, in OpenAI list format.

**Authentication:** `Authorization: Bearer sk-…`

**Response**

| Field | Type | Notes |
|-------|------|-------|
| `object` | string | Always `"list"`. |
| `data[].id` | string | Upstream `model_id`, lower-cased. |
| `data[].object` | string | Always `"model"`. |
| `data[].created` | integer | Unix seconds at request time (not a stored timestamp). |
| `data[].owned_by` | string | The model's `provider`. |
| `data[].context_window` | integer \| null | Context window in tokens, so a downstream agent can budget its prompt. `null` when not configured. |
| `data[].max_output_tokens` | integer \| null | Largest output the model can produce. `null` when not configured. Must never be confused with the key's `max_tokens` limit, which is a gateway-side cap. |
| `data[].supports_image` | bool | Whether the model accepts image input. `false` when not configured. |
| `data[].effort_levels` | string[] \| null | Accepted `reasoning_effort` values, ascending (`none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`). `null` when not configured. |

Only **enabled** bound models are returned, de-duplicated by lower-cased `model_id`.

The four capability fields are **declarations only**: they are set on the model in the admin
plane and passed through verbatim so downstream agents can size their requests. The gateway
does not enforce them — it does not reject long prompts, image input or an unlisted
`reasoning_effort`. Output length is still capped solely by the key's `max_tokens`.

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

**Errors**

| Status | `message` |
|--------|-----------|
| 401 | `missing API key` / `invalid API key` |
| 500 | `internal server error` |

### POST /v1/responses

OpenAI Responses API ingress. Forwards a Responses request to the upstream model bound to the
presented key: native passthrough for `openai-responses` upstreams, translation through chat
completions for `openai-completions` / `anthropic-messages` upstreams. Streaming and
non-streaming, text, images and function tool calls are supported, along with the four stateful
options `store`, `previous_response_id`, `conversation` and `background`.

**Authentication:** `Authorization: Bearer sk-…`. Errors use this document's `ErrorResponse`
envelope (**not** the Anthropic envelope), with `code` equal to the HTTP status code.

> ⚠️ **Privacy warning 1: `store` defaults to `true` — omitting it still persists.**
>
> Under OpenAI semantics the default value of `store` is `true`. So **when the client sends no
> `store` field at all, the gateway still writes this turn's conversation content to the database**,
> retained for 7 days by default (`-response-retention-days`). This is the opposite of the intuition
> that nothing is saved unless explicitly requested.
>
> | Goal | How |
> |------|-----|
> | Do not persist this turn | Send `"store": false` **explicitly**. |
> | Forbid persistence site-wide | Start with `-allow-response-storage=false` (or `AIGATE_ALLOW_RESPONSE_STORAGE=false`): every request is treated as `store:false` and all stateful features are unavailable. |
> | Shorten retention | `-response-retention-days` (default `7`, `0` = keep forever). |
>
> With storage disabled site-wide, an **explicit** `"store": true` returns `400 response storage is
> disabled on this gateway` — the gateway rejects it outright instead of silently rewriting it to
> `store:false`, which would leave you believing the data was saved when it was not.

> ⚠️ **Privacy warning 2: `store: false` ≠ "does not read the database".**
>
> Whenever a request carries `previous_response_id` or `conversation`, the gateway **still reads**
> the parent row's history and prepends it to this request's `input` before sending it upstream.
> `store: false` only means "**this turn's result is not written to storage**". It is **not** a
> privacy switch: the previous turn's content is still read out and sent to the upstream provider.
>
> To touch no history at all, send neither `previous_response_id` nor `conversation`, and pass
> `"store": false` explicitly.

**Request fields**

The request body is an OpenAI Responses object. The gateway only reads and rewrites the fields
below; every other field is forwarded verbatim on the passthrough path.

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `model` | string | no | Target model. Resolution rules are identical to `/v1/chat/completions` (including primary fallback and `requested model is not bound to this key`). |
| `input` | string \| array | **yes** | A string, or an array of input items. On the translation path, producing no chat message at all yields `400 invalid input`. |
| `instructions` | string | no | System prompt. Mapped to a system message on the translation path; **not** written into the conversation history snapshot (so a later turn does not re-inject it). |
| `stream` | bool | no | `true` selects SSE streaming. |
| `max_output_tokens` | integer | no | Capped by the key's `max_tokens` limit when that limit is `> 0`. |
| `tools` | array | no | Responses flat tool array; a non-array yields `400 invalid tools`. |
| `tool_choice` / `text` / any other field | — | no | Forwarded verbatim on the passthrough path; forwarded as far as it maps on the translation path. |
| `store` | bool | no | **Defaults to `true`** (i.e. persisted by default — see warning 1). A non-boolean yields `400 invalid store`. |
| `previous_response_id` | string | no | Continue from the given turn (see warning 2). |
| `conversation` | string \| `{"id": "…"}` | no | Continue from the newest completed row in the conversation (see warning 2). |
| `background` | bool | no | `true` runs the request asynchronously (see "Background execution" below). |

**Stateful rules**

| Case | Behaviour |
|------|-----------|
| `previous_response_id` and `conversation` both present | `400 previous_response_id and conversation are mutually exclusive`. |
| `previous_response_id` names a row that is missing, **owned by another user**, or already pruned | `404 previous response not found`. All three cases share the **same code and wording**, leaving no gap for enumerating another user's response IDs. |
| `conversation` names a conversation with no usable row | Treated as a new conversation (no error). |
| The newest row in a conversation is `DELETE`d | That `conversation` falls back to the previous row; with no earlier row it is treated as a new conversation. |
| The parent row's `items` snapshot is corrupt | Degraded to "no history" (the request does not fail). |
| Site `-allow-response-storage=false` plus any stateful field | `400`: `previous_response_id`/`conversation` report `response storage is disabled on this gateway, previous_response_id and conversation are unavailable`; `background:true` reports `response storage is disabled on this gateway, background is unavailable`. |

The history used for continuation is a **cumulative snapshot**: each row's snapshot = the previous
history + this turn's `input` + this turn's output items, **excluding** `instructions`. Another
data-plane key belonging to the same user may continue that user's rows (ownership is checked per
user, not per key).

> ⚠️ **All data-plane keys of the same user share one conversation space.**
> Ownership is checked per **user**, not per key — that way rotating a key does not orphan existing
> conversations. The trade-off: if you issue several keys for one user to serve **different
> downstream applications**, application B can read application A's full conversation history if it
> obtains A's `previous_response_id`. `conversation` deserves extra care: its ID is an arbitrary
> client-supplied string, so the same name refers to the same history for that user.
> **To isolate applications, give each one its own user** rather than relying on separate keys.

**Upstream paths**

The resolved model's `model_type` selects the path:

| Upstream | Behaviour |
|----------|-----------|
| `openai-responses` | **Passthrough.** The request body is forwarded verbatim (only `model` is replaced with the upstream ID and `store:false` is **forced**) to `<url>/responses`. The upstream response and SSE stream are relayed as-is, rewriting only `id` / `model` in terminal events. |
| `openai-completions` / `anthropic-messages` | **Translation.** The Responses request is converted to an OpenAI chat request (`instructions`/system → system message, input items → messages, `max_output_tokens` → `max_tokens`, `text.format` → `response_format`, flat `tools` → nested `tools`), and the chat response or SSE deltas are translated back into a Responses response / event sequence. |

Both paths **force the upstream `store: false`** — the gateway never duplicates your data into the
upstream vendor's storage. Upstream timeouts are the same on both: 5 minutes non-streaming,
30 minutes streaming.

**Response — non-streaming**

`200` returns a Responses response object. `id` is always a gateway-generated `resp_` + 32 hex
digits, never a passthrough of the upstream ID; the upstream ID is not recorded or exposed.

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

`status` values: `queued`, `in_progress`, `completed`, `incomplete`, `failed`, `cancelled`.
An upstream `finish_reason=length` yields `incomplete` with
`incomplete_details: {"reason": "max_output_tokens"}` — truncation is reported explicitly, never
silently treated as success. `failed` carries `error: {"message": "…", "type": "background_error"}`;
common `message` values include `failed to connect to LLM backend`,
`upstream returned status <code>`, `invalid upstream response` and `internal server error`.

**Response — streaming (`stream: true`)**

`Content-Type: text/event-stream` with `Cache-Control: no-cache` and `X-Accel-Buffering: no`. The
translation path's event order is:

```text
response.created → response.in_progress
  → response.output_item.added → response.content_part.added
  → response.output_text.delta* → response.output_text.done
  → response.content_part.done → response.output_item.done
  → response.completed（or response.incomplete / response.failed / error）
```

When the chat upstream streams tool calls, each one becomes its own `function_call` item:

```text
response.output_item.added
  → response.function_call_arguments.delta* → response.function_call_arguments.done
  → response.output_item.done
```

`message` and `function_call` items do not share `output_index`. If the upstream fails mid-stream,
the gateway emits `event: error` followed by a terminal event — a truncated stream is never reported
as `status: "completed"`.

The passthrough path relays the upstream's own Responses event stream as-is, rewriting only `id` /
`model` in terminal events. Usage is recorded after the stream ends.

**Background execution (`background: true`)**

1. The gateway first completes authentication, model resolution and quota checks — **a rejected
   request never leaves a row behind**; it then inserts a row with `status: queued` and **returns
   that object immediately** (HTTP `200`).
2. The background task advances the status to `in_progress` and finally settles on `completed` /
   `failed` (or `cancelled` after a cancel).
3. The client polls `GET /v1/responses/{id}`. The `POST` reply and the `GET` before the background
   write-back are **the same row, byte-for-byte identical**.
4. Total task time is bounded by `-response-background-timeout` (default `30m`).

`background: true` requires persistence (the result needs somewhere to live): `store` is always
`true`, and an explicit `store: false` returns `400 background requires store to be true` (a
self-contradiction in the parameters, checked before the site switch).

A process restart can leave in-flight rows nobody will ever advance: after startup the gateway
marks `queued` / `in_progress` rows older than the background timeout as `failed` every minute,
with `error.message` set to `interrupted by restart`. In a multi-instance deployment the background
task runs on **the instance that received the request** and is not migrated; the status lives in the
database, so polling another instance still observes the correct status.

**Errors**

| Status | `message` | Cause |
|--------|-----------|-------|
| 401 | `missing API key` / `invalid API key` | No `Authorization` header vs malformed, unknown or disabled. |
| 400 | `failed to read request body` | The request body could not be read. |
| 413 | `request body too large` | Body exceeds 1 MiB; rejected outright, never truncated. |
| 400 | `invalid json body` | The body is not a JSON object. |
| 400 | `invalid input` / `invalid tools` | `input` produced no message / `tools` is not an array (translation path). |
| 400 | `invalid store` / `invalid background` | A field that must be boolean is not. |
| 400 | `previous_response_id and conversation are mutually exclusive` | Both continuation fields present. |
| 404 | `previous response not found` | Parent row missing, owned by another user, or pruned. |
| 400 | `background requires store to be true` | `background:true` conflicts with an explicit `store:false`. |
| 400 | `response storage is disabled on this gateway` | Storage disabled site-wide but the client explicitly asked for `store:true`. |
| 400 | `response storage is disabled on this gateway, previous_response_id and conversation are unavailable` | Storage disabled site-wide but a continuation field was sent. |
| 400 | `response storage is disabled on this gateway, background is unavailable` | Storage disabled site-wide but `background:true` was requested. |
| 400 | `requested model is not bound to this key` / `no available model for this key` | Model resolution failed (same rules as `/v1/chat/completions`). |
| 429 / 403 | The same quota messages as `/v1/chat/completions` | Quota check rejected the request; the status comes from `-quota-exceeded-status`, with `Retry-After`. |
| 500 | `internal server error` | Internal error (including entropy failure while generating a `resp_` ID). |
| 502 | `failed to connect to LLM backend` / `invalid upstream response` | Cannot reach the upstream / the upstream response cannot be parsed. |

**Unsupported capabilities**

- **Built-in tools** (`web_search`, `file_search`, `code_interpreter`, …): they have no counterpart
  on the translation path and are **dropped**; only `function`-type tools are mapped.
- **Responses-only structures**: `reasoning` items, `encrypted_content`, built-in tool outputs and
  `annotations` are **lossy** (dropped) on the translation path. Use an `openai-responses` upstream
  when these must survive end to end.
- **Non-function tools and upstream-side `store`**: the upstream `store` is always forced to
  `false`; the gateway does not let the upstream persist on your behalf.
- Unknown request fields are forwarded verbatim on the passthrough path and ignored rather than
  rejected on the translation path (forward compatibility).

**Request example — stateless (explicitly not persisted)**

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

**Request example — stateful continuation**

```bash
# Turn 1: store defaults to true, so this turn IS persisted
curl http://127.0.0.1:8080/v1/responses \
  -H "Authorization: Bearer <SK_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"model": "glm-5.1", "input": "hello"}'
# → {"id": "resp_0f3a…", "status": "completed", …}

# Turn 2: continue from the previous turn
curl http://127.0.0.1:8080/v1/responses \
  -H "Authorization: Bearer <SK_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "glm-5.1",
    "input": "say it again",
    "previous_response_id": "resp_0f3a…"
  }'
```

### GET /v1/responses/{id}

Reads one stored response. The returned object has the same shape as the `POST /v1/responses`
reply.

**Authentication:** `Authorization: Bearer sk-…`

**Response:** `200` returns that row's response object — a `status: queued` object while the
background task has not written back yet, and the terminal object (`completed` / `incomplete` /
`failed` / `cancelled`) afterwards.

```bash
curl http://127.0.0.1:8080/v1/responses/resp_0f3a… \
  -H "Authorization: Bearer <SK_TOKEN>"
```

**Errors**

| Status | `message` |
|--------|-----------|
| 401 | `missing API key` / `invalid API key` |
| 404 | `previous response not found` (missing / owned by another user / pruned — same code and wording) |
| 500 | `internal server error` |

### DELETE /v1/responses/{id}

Deletes a finished response. **Only terminal** rows (`completed` / `failed` / `incomplete` /
`cancelled`) may be deleted; an in-flight row must be `cancel`led first.

**Authentication:** `Authorization: Bearer sk-…`

**Response:** `200`

```json
{"id": "resp_0f3a…", "object": "response.deleted", "deleted": true}
```

Deleting the newest row of a conversation makes that `conversation` fall back to the previous row;
with no earlier row it is treated as a new conversation. After deletion the ID can no longer be used
as a `previous_response_id` (it returns 404).

**Errors**

| Status | `message` | Cause |
|--------|-----------|-------|
| 401 | `missing API key` / `invalid API key` | — |
| 404 | `previous response not found` | Missing / owned by another user / pruned. |
| 400 | `response is still running, cancel it before deleting` | The row is still `queued` / `in_progress`. |
| 500 | `internal server error` | — |

### POST /v1/responses/{id}/cancel

Cancels an in-flight (`queued` / `in_progress`) response: the status becomes `cancelled` and its
in-progress **upstream request is interrupted** (so compute and quota are not burned until the
background timeout).

**Authentication:** `Authorization: Bearer sk-…`

**Response:** `200` returns the cancelled response object (`status: "cancelled"`).

**A terminal row cannot be cancelled** and returns `400`, whose body is the error envelope **plus
the current response object** — the client gets the result or the failure reason without issuing
another GET:

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

When a cancel races a completion, **whichever arrives first wins**: if the terminal status is
already written, the cancel simply returns that terminal status.

**Errors**

| Status | `message` | Cause |
|--------|-----------|-------|
| 401 | `missing API key` / `invalid API key` | — |
| 404 | `previous response not found` | Missing / owned by another user / pruned. |
| 400 | `response already finished and cannot be cancelled` | The row is terminal (the body also carries a `response` field). |
| 500 | `internal server error` | — |

### POST /v1/decisions and POST /v1/systemone (decision models)

Decision models do not generate text. They answer typed questions and return
probabilities. The gateway exposes both protocols and lets either ingress reach
either upstream format (cross-protocol calls are translated by the gateway).

| Ingress | Protocol | Upstream `model_type` |
|---------|----------|-----------------------|
| `POST /v1/decisions` (alias `POST /decisions`) | OpenAI Decisions | `openai-decisions` or `systemone` |
| `POST /v1/systemone` (alias `POST /systemone`) | TypeSafe System One | `systemone` or `openai-decisions` |

Neither ingress streams: `stream: true` is rejected with `400`.

**OpenAI Decisions request** — `model`, `input` (string or message array) and
`questions` (1..128 items, `type` = `predicate` | `choice` | `score` | `box`;
`choices` 2..255 unique string/boolean values; `levels` 2..10 labelled levels;
optional `safety_identifier`).

**System One request** — `model`, `state` (string, object or array) and
`questions` (1..128 keys; key order is the answer order; `type` = `noul` |
`choice` | `score` | `box`; `criteria` is a 1..255 key map for `choice` and a
1..10 string array for `score`; optional `images` / `assets`).

The question-type names are protocol-specific: `noul` on `/v1/decisions` and
`predicate` on `/v1/systemone` are rejected with a 400 that names the
equivalent type.

**Response** — always in the ingress protocol: `/v1/decisions` returns an
`answers` array in request order (each with `name` and `type`), `/v1/systemone`
returns an `answers` object keyed by question name. Both keep the upstream
`usage` object. A refusal surfaces as `{"type":"refusal","name":...}` on the
OpenAI ingress, while a System One upstream `422 decision_refused` is relayed
verbatim.

**Cross-protocol mapping** — message arrays are joined with `\n\n` into
`state`; `predicate` <-> `noul`; `choices[{value,description}]` <-> `criteria{key: description}`
(boolean values become the strings `true`/`false`); `levels[{label,description}]`
<-> `criteria: [label, ...]`; `box` <-> `box`. Lossy fields are rejected with a
400 rather than dropped: `assets` (to OpenAI), `safety_identifier` (to System
One), and `input` messages whose role is neither `user`, `system` nor
`developer`.

**Billing** — decision models default to `billing_mode: input_only`: usage rows
record `completion_tokens = 0` and `total_tokens = prompt_tokens`, while the
response body still carries the upstream usage verbatim. Setting
`billing_mode: per_token` records the upstream numbers as reported.

## 3. Admin plane

All `/admin/*` routes require `Authorization: Bearer <ADMIN_TOKEN>` where the token belongs to a user with `role=admin`. Path `{id}` values must be positive integers; a non-numeric or zero value yields `400 invalid <model|credential|key|user|group|webhook> id`. A missing target yields `404 <model|credential|key|user|group|webhook> not found`.

See "Authentication failures" in the appendix for `401` vs `403` semantics.

### 3.1 Models

Model objects returned by the API:

| Field | Type | Notes |
|-------|------|-------|
| `id` | integer | Server-assigned. |
| `provider` | string | Free-form label; used as `owned_by` in `/v1/models`. |
| `model_id` | string | Upstream model identifier sent to the provider. |
| `model_name` | string | Display name. |
| `url` | string | Upstream base URL. |
| `credential_id` | integer | Credential referenced for the upstream key; `0` means none (use `api_key`). |
| `model_type` | string | Upstream protocol: `openai-completions`, `anthropic-messages`, `openai-responses`, `openai-decisions`, `systemone` or `gemini-generate-content`. It also determines how `url` is joined (see "Upstream URL joining" in §6). |
| `billing_mode` | string | Billing basis: `per_token` (default, recorded as reported) or `input_only` (completion recorded as 0). Decision `model_type`s default to `input_only` on creation. |
| `custom_http_headers` | object \| null | Allowed headers for passthrough (string values). |
| `quota_day` | integer | Model-level daily token quota; `-1` means unlimited. |
| `enabled` | bool | Disabled models are immediately unroutable. |
| `currency` | string | Pricing currency (ISO 4217). `""` = unconfigured, and the model does not participate in cost accounting (see "Cost billing" in §3.10). |
| `enable_peak_pricing` | bool | Peak/off-peak pricing switch (default `false`). When off, requests are always billed at the **off-peak** columns. |
| `use_upstream_cost` | bool | Trust the upstream-reported `usage.cost` (default `true`). When on and the upstream reports `cost > 0`, that value is used instead of computing from the price table. |
| `context_window` | integer | Context window in tokens; `0` = unconfigured. Declared capability, shown to operators only — it does not reject long requests. |
| `max_output_tokens` | integer | Largest output the model can produce; `0` = unconfigured. Declared capability, surfaced in `GET /v1/models` — it never caps a request (only the key's `max_tokens` does). |
| `supports_image` | bool | Declares that the model accepts image input. Shown only — image requests are not rejected yet. |
| `effort_levels` | array of string | Accepted `reasoning_effort` levels, normalised to ascending ladder order (`none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`). `[]` = unconfigured. Declared capability, shown only. |
| `current_price` | object \| null | The currently effective price snapshot (the newest one whose `effective_at` is not in the future); `null` when the model has no price yet. Same shape as a price row (see §3.10). |
| `created_at` / `updated_at` | string | RFC 3339 timestamps. |

`api_key` is **never** present in any response.

#### POST /admin/models

| Field | Type | Required | Default | Notes |
|-------|------|----------|---------|-------|
| `provider` | string | no | `""` | |
| `model_id` | string | **yes** | — | Must be non-empty after trimming. |
| `model_name` | string | no | `""` | |
| `url` | string | **yes** | — | Must be non-empty after trimming. |
| `api_key` | string | no | `""` | Upstream credential (used when `credential_id` is `0`). |
| `credential_id` | integer | no | `0` | Must reference an existing credential, else `400 credential <id> not found`. |
| `model_type` | string | **yes** | — | Must be `openai-completions`, `anthropic-messages`, `openai-responses`, `openai-decisions`, `systemone` or `gemini-generate-content`. |
| `billing_mode` | string | no | derived | `per_token` or `input_only`; decision models default to `input_only`. |
| `custom_http_headers` | object | no | none | JSON object of string values; `null` stores an empty value. |
| `quota_day` | integer | no | `-1` | |
| `enabled` | bool | no | `true` | |
| `user_key_required` | bool | no | `false` | BYOK mode: the model stores **no** shared key and the per-binding key is used instead. Rejected with `400` when combined with a non-empty `api_key` or a `credential_id > 0`; the flag cannot be changed after creation. |
| `currency` | string | no | `""` | Pricing currency, ISO 4217 three letters (case-insensitive, normalised to upper case). Anything else yields `400 currency must be a 3-letter ISO 4217 code`. |
| `enable_peak_pricing` | bool | no | `false` | |
| `use_upstream_cost` | bool | no | `true` | |
| `context_window` | integer | no | `0` | Must be `>= 0`, else `400 context_window must not be negative`. |
| `max_output_tokens` | integer | no | `0` | Must be `>= 0`, else `400 max_output_tokens must not be negative`. |
| `supports_image` | bool | no | `false` | |
| `effort_levels` | array of string | no | `[]` | Each entry must be a known level and must not repeat, else `400` (`effort_levels contains an unknown level: ...` / `... a duplicate level: ...`). Case-insensitive; stored in ladder order. |

```bash
curl -X POST http://127.0.0.1:8080/admin/models \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "zhipu",
    "model_id": "glm-5.1",
    "model_name": "GLM-5.1",
    "url": "https://open.bigmodel.cn/api/paas/v4",
    "api_key": "<UPSTREAM_KEY>",
    "model_type": "openai-completions",
    "quota_day": -1,
    "enabled": true
  }'
```

Response `201 Created` — a model object as described above.

| Status | Condition |
|--------|-----------|
| 201 | Created. |
| 400 | Invalid JSON; empty `model_id`; empty `url`; invalid `model_type`; malformed `custom_http_headers`; unknown `credential_id`; `user_key_required` combined with a shared `api_key`/credential. |
| 401 / 403 | Authentication / authorisation failure. |
| 500 | Database error. |

#### GET /admin/models

Response `200 OK`:

```json
{"models": [ /* model objects */ ]}
```

The array is `[]` when no models exist.

#### GET /admin/models/{id}

Response `200 OK` — a single model object. `404 model not found` when absent.

#### PUT /admin/models/{id}

Partial update. Only the fields present in the body are changed (absent fields are untouched).

| Field | Type | Notes |
|-------|------|-------|
| `provider` | string | |
| `model_id` | string | Rejected with `400` if empty. |
| `model_name` | string | |
| `url` | string | Rejected with `400` if empty. |
| `api_key` | string | Applied only when non-empty; an empty string is a no-op (the credential cannot be cleared). |
| `credential_id` | integer | `> 0` must reference an existing credential; `0` detaches the reference. |
| `model_type` | string | Must be a valid enum. |
| `custom_http_headers` | object \| null | An object replaces the value; `null` clears it. |
| `quota_day` | integer | |
| `enabled` | bool | |
| `user_key_required` | bool | Immutable: sending a value that differs from the current one is rejected with `400 user_key_required cannot be changed after creation`. For BYOK models, a non-empty `api_key` or `credential_id > 0` is rejected with `400`. |
| `currency` | string | ISO 4217 three letters; `""` clears the pricing currency. Changing it invalidates the model's price cache immediately and never rewrites rows already accounted. |
| `enable_peak_pricing` | bool | |
| `use_upstream_cost` | bool | |
| `context_window` | integer | `>= 0`. |
| `max_output_tokens` | integer | `>= 0`; `0` clears the model-level ceiling (the key ceiling still applies). |
| `supports_image` | bool | |
| `effort_levels` | array of string | Replaces the whole list; `[]` clears it. Same validation as creation. |

```bash
curl -X PUT http://127.0.0.1:8080/admin/models/1 \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"enabled": false}'
```

Response `200 OK` — the updated model object.

| Status | Condition |
|--------|-----------|
| 200 | Updated. |
| 400 | Invalid JSON; empty `model_id`/`url`; invalid `model_type`; malformed `custom_http_headers`; unknown `credential_id`; invalid id. |
| 404 | Model not found. |

#### DELETE /admin/models/{id}

Deletes the model and cleans up every key binding that referenced it.

Response `200 OK`:

```json
{"ok": true}
```

| Status | Condition |
|--------|-----------|
| 200 | Deleted. |
| 400 | Invalid id. |
| 404 | Model not found. |

#### POST /admin/models/{id}/connectivity

Probe an upstream with a minimal request — one user message asking the model to reply with the single word `hello`, `max_tokens=8` — to check that the address is reachable, the credential is valid, and the model actually answers.

**The probe result is always returned with HTTP `200`**; success or failure lives in the body's `ok` field:

| Body | Meaning |
|------|---------|
| `{"ok": true, "latency_ms": N, "reply": "hello", "upstream_id": "…", "upstream_model": "…"}` | Success. `latency_ms` is the full round trip in milliseconds — measured after the response body has been read, not when headers arrive — rounded and at least `1`. `reply` is the assistant text the upstream actually returned; `upstream_id`/`upstream_model` are what the upstream reported, for correlating with its own logs. |
| `{"ok": false, "error_kind": "…", "message": "…"}` | Failure. `error_kind` is a closed enum; `message` is human-readable. |

`error_kind` values:

| Value | Cause |
|-------|-------|
| `network_unreachable` | DNS/connect/TLS/timeout, or an SSRF rejection (the default catch-all). |
| `invalid_api_key` | The upstream rejected the credential. |
| `forbidden` | The upstream forbade the request. |
| `rate_limited` | The upstream rate-limited the probe. |
| `upstream_server` | The upstream returned a 5xx. |
| `upstream_client` | The upstream returned another 4xx. |

A 2xx response alone is not treated as success: the body must parse into a non-empty assistant message, otherwise the probe fails with `upstream_client`. This keeps a gateway that answers 200 without actually calling the model from looking reachable.

The outbound request uses the same SSRF protection as the data plane: under the default strict mode a private/loopback upstream is blocked and classified as `network_unreachable`; start with `-allow-private-upstreams` to allow it. The upstream key is resolved live (credential first, then the model's `api_key`); a resolution failure is a server-configuration error and returns `500` rather than a probe result.

A `user_key_required` (BYOK) model has no shared key to probe: the endpoint returns `400 user_key_required model has no shared key to probe; verify it from the data plane with a binding key`.

Only an invalid path id (`400`) and a missing model (`404`) use the standard error envelope.

| Status | Condition |
|--------|-----------|
| 200 | Probe completed (whether `ok` is true or false). |
| 400 | Invalid id; the model is `user_key_required`. |
| 404 | Model not found. |
| 500 | Credential/secret resolution failed. |

#### POST /admin/models/discover

List the model ids an upstream exposes, so a console can offer a picker instead of asking operators to copy `model_id` by hand.

Request body:

| Field | Type | Meaning |
|-------|------|---------|
| `model_type` | string, required | `openai-completions`, `anthropic-messages` or `gemini-generate-content`. |
| `url` | string, required | Same semantics as `model.url`: `openai-completions` is a base ending in `/v1`, `anthropic-messages` is the host. |
| `api_key` | string, optional | Used directly — for a key that has not been stored yet. |
| `credential_id` | number, optional | Resolve the credential's first header secret instead of passing a key. |

Exactly one key source is required: `api_key` wins when both are present.

The result follows the connectivity convention — always HTTP `200`, with success or failure in the body:

| Body | Meaning |
|------|---------|
| `{"ok": true, "models": ["gpt-4o", …]}` | Model ids reported by the upstream `/models` endpoint. |
| `{"ok": false, "error_kind": "…", "message": "…"}` | Same `error_kind` enum as the connectivity probe. |

Outbound requests use the data-plane SSRF protection: private/loopback targets are blocked unless `-allow-private-upstreams` is set, and are reported as `network_unreachable`.

| Status | Condition |
|--------|-----------|
| 200 | Discovery completed (whether `ok` is true or false). |
| 400 | Invalid `model_type`/`url`, or neither key source given, or the credential has no header secret. |
| 404 | `credential_id` not found. |

### 3.2 Credentials

Upstream credentials group one or more secrets and are referenced by a model's `credential_id`. At forward time the gateway resolves the credential live (no snapshot) and uses its first `location=header` secret; if the credential is missing, disabled, or has no header secret it falls back to the model's own `api_key`.

Credential objects returned by the API:

| Field | Type | Notes |
|-------|------|-------|
| `id` | integer | Server-assigned. |
| `name` | string | |
| `description` | string | |
| `secrets` | array | Secret items; `value` is always a mask. |
| `enabled` | bool | Disabled credentials fall back to the model's `api_key`. |
| `created_at` / `updated_at` | string | RFC 3339 timestamps. |

Secret item objects:

| Field | Type | Notes |
|-------|------|-------|
| `key` | string | Header name. |
| `value` | string | Masked on read: first 2 + `*`*(n-4) + last 2 for values longer than 4; an all-`*` mask for shorter values. |
| `location` | string | Only `header` is supported; omitted/empty is normalised to `header`. |

#### POST /admin/credentials

| Field | Type | Required | Default | Notes |
|-------|------|----------|---------|-------|
| `name` | string | **yes** | — | Must be non-empty after trimming. |
| `description` | string | no | `""` | |
| `secrets` | array | no | none | Each item needs a non-empty `key`; `location` must be `header`. |
| `enabled` | bool | no | `true` | |

```bash
curl -X POST http://127.0.0.1:8080/admin/credentials \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "zhipu-prod",
    "description": "production upstream key",
    "secrets": [{"key": "Authorization", "value": "Bearer <UPSTREAM_KEY>", "location": "header"}]
  }'
```

Response `201 Created` — a credential object with masked `secrets`.

| Status | Condition |
|--------|-----------|
| 201 | Created. |
| 400 | Invalid JSON; empty `name`; a secret with an empty `key` or a `location` other than `header`. |
| 500 | Database error. |

#### GET /admin/credentials

```json
{"credentials": [ /* credential objects, secrets masked */ ]}
```

#### GET /admin/credentials/{id}

Response `200 OK` — a single credential object (secrets masked). `404 credential not found` when absent.

#### PUT /admin/credentials/{id}

Partial update.

| Field | Type | Notes |
|-------|------|-------|
| `name` | string | Rejected with `400` if empty. |
| `description` | string | |
| `secrets` | array | When present, **replaces** all secrets. Omit the field to leave them untouched. |
| `enabled` | bool | |

Response `200 OK` — the updated credential object.

#### DELETE /admin/credentials/{id}

Deletes the credential. If any model still references it, the request is rejected with `409` and a message naming the reference count:

```json
{
  "error": {
    "message": "credential is referenced by 2 model(s); detach them before deleting",
    "type": "error",
    "code": 409
  }
}
```

Detach the models (set their `credential_id` to `0` or another credential) first.

| Status | Condition |
|--------|-----------|
| 200 | Deleted (`{"ok": true}`). |
| 404 | Credential not found. |
| 409 | Still referenced by one or more models. |

### 3.3 Keys

Data-plane keys (`sk-…`) owned by a user and optionally bound to a set of models.

Key objects returned by the API:

| Field | Type | Notes |
|-------|------|-------|
| `id` | integer | Server-assigned. |
| `name` | string | |
| `user_id` | integer | Owning user ID. |
| `group_id` | integer | Optional group, `0` when unset. |
| `proxy_token_prefix` | string | Display prefix, e.g. `sk-abcd1234…wxyz`. |
| `max_tokens` | integer | Request-side cap; `<= 0` means unlimited. |
| `token_quota_rules` | string | Per-key period quota as normalised rule JSON; `""` = no key-level limit. Read-only on the user plane. |
| `enabled` | bool | Disabled keys fail data-plane authentication with `401`. |
| `expires_at` | string / null | Optional expiry (RFC 3339, UTC); `null` = never expires. |
| `expired_at` | string / null | First observed rejection after expiry; `null` = not recorded. |
| `created_at` / `updated_at` | string | RFC 3339 timestamps. |
| `bindings` | array | Binding views (`[]` when none). |
| `token` | string | **Only** present in the create and rotate responses. |

Binding view objects:

| Field | Type | Notes |
|-------|------|-------|
| `ai_model_id` | integer | Referenced model ID. |
| `model_id` | string | |
| `model_name` | string | |
| `provider` | string | |
| `role` | string | `primary` or `fallback`. |
| `sort_order` | integer | Ordering hint. |
| `has_key` | bool | Whether a per-binding upstream key is configured (required by `user_key_required` models; the value itself is never returned). |

#### POST /admin/keys

| Field | Type | Required | Default | Notes |
|-------|------|----------|---------|-------|
| `name` | string | no | `""` | |
| `user_id` | integer | **yes** | — | Must reference an existing user; otherwise `400 user_id does not exist`. |
| `group_id` | integer | no | `0` | |
| `max_tokens` | integer | no | `0` | `<= 0` means no cap. |
| `token_quota_rules` | string | no | `""` | Per-key period quota (rule array JSON, validated and normalised). `""` = no key-level limit; `"[]"` = explicitly unlimited. |
| `expires_at` | string | no | `null` | RFC 3339 timestamp; omitted/`null` = never expires. An unparsable value is rejected with `400 expires_at must be an RFC3339 timestamp`. |
| `bindings` | array | no | none | See below. |

Each binding object:

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `ai_model_id` | integer | **yes** | Must be `> 0` and reference an existing model. |
| `role` | string | **yes** | `primary` or `fallback`. Any other value is rejected. |
| `sort_order` | integer | no | Defaults to `0`. |
| `api_key` | string | no | Per-binding upstream key for `user_key_required` (BYOK) models; ordinary models ignore it. Encrypted at rest, never echoed (only `has_key` is reported). |

When `bindings` is non-empty it must contain **exactly** one `primary`, otherwise the request is rejected with `400 bindings must contain exactly one primary model`.

`api_key` follows the whole-array replacement semantics of `bindings`: replacing the bindings without re-sending `api_key` clears it (`has_key` becomes `false`).

Note: the per-user `settings.user_key_limit` applies to the self-service API (§4.4), **not** to admin-created keys.

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

Response `201 Created` — a key object whose `token` field holds the one-time plaintext (`sk-…`). This is the only time it is returned besides rotation.

| Status | Condition |
|--------|-----------|
| 201 | Created. |
| 400 | Invalid JSON; unknown `user_id`; invalid binding role; `ai_model_id` of `0`; unknown `ai_model_id`; not exactly one `primary`; duplicate binding. |
| 401 / 403 | Authentication / authorisation failure. |
| 500 | Database error. |

#### GET /admin/keys

Response `200 OK`:

```json
{"keys": [ /* key objects, without token */ ]}
```

#### GET /admin/keys/{id}

Response `200 OK` — a single key object (no `token`). `404 key not found` when absent.

#### PUT /admin/keys/{id}

Partial update.

| Field | Type | Notes |
|-------|------|-------|
| `name` | string | |
| `max_tokens` | integer | |
| `token_quota_rules` | string | `""` clears the key-level quota; otherwise a valid rule array (validated and normalised). |
| `enabled` | bool | |
| `expires_at` | string / null | An RFC 3339 value sets a new expiry (and clears `expired_at`); an explicit `null` clears both; anything else is `400`. |
| `bindings` | array | When present, **replaces** all bindings; `[]` clears them. Omit the field to leave bindings untouched. |

`user_id` and `group_id` are not updatable.

```bash
curl -X PUT http://127.0.0.1:8080/admin/keys/1 \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"enabled": false, "bindings": []}'
```

Response `200 OK` — the updated key object (no `token`).

| Status | Condition |
|--------|-----------|
| 200 | Updated. |
| 400 | Invalid JSON; invalid binding set; invalid id. |
| 404 | Key not found. |

#### DELETE /admin/keys/{id}

Deletes the key and its bindings. Response `200 OK`:

```json
{"ok": true}
```

#### POST /admin/keys/{id}/rotate

Issues a new `sk-` credential and invalidates the old one immediately. The key's bindings and settings are preserved.

Response `200 OK` — the key object with a fresh one-time `token`.

| Status | Condition |
|--------|-----------|
| 200 | Rotated. |
| 400 | Invalid id. |
| 404 | Key not found. |

### 3.4 Users

Users hold access tokens (`gk-…`) used to authenticate on the admin plane (when `role=admin`) and the user plane.

User objects returned by the API:

| Field | Type | Notes |
|-------|------|-------|
| `id` | integer | Server-assigned. |
| `name` | string | |
| `role` | string | `admin` or `user`. |
| `group_id` | integer | `0` when unset. |
| `api_token_prefix` | string | Display prefix. |
| `enabled` | bool | Disabled users fail authentication with `401`. |
| `created_at` / `updated_at` | string | RFC 3339 timestamps. |
| `token` | string | **Only** present in the create and rotate responses. |

#### POST /admin/users

| Field | Type | Required | Default | Notes |
|-------|------|----------|---------|-------|
| `name` | string | no | `""` | |
| `role` | string | no | `user` | Must be `admin` or `user`. |
| `group_id` | integer | no | `0` | |
| `enabled` | bool | no | `true` | |

```bash
curl -X POST http://127.0.0.1:8080/admin/users \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"username": "alice", "role": "user", "group_id": 0}'
```

Response `201 Created` — a user object whose `token` holds the one-time plaintext (`gk-…`).

| Status | Condition |
|--------|-----------|
| 201 | Created. |
| 400 | Invalid JSON; invalid `role`. |
| 500 | Database error. |

#### GET /admin/users

Response `200 OK`:

```json
{"users": [ /* user objects, without token */ ]}
```

There is no single-user `GET /admin/users/{id}` endpoint.

#### PUT /admin/users/{id}

Partial update.

| Field | Type | Notes |
|-------|------|-------|
| `name` | string | |
| `role` | string | Must be `admin` or `user`. |
| `group_id` | integer | |
| `enabled` | bool | |

Response `200 OK` — the updated user object (no `token`). `404 user not found` when absent.

#### DELETE /admin/users/{id}

Deletes the user and cascades to that user's keys and their bindings. Historical usage logs are retained.

Response `200 OK`:

```json
{"ok": true}
```

#### POST /admin/users/{id}/rotate

Issues a new access token and invalidates the old one immediately.

Response `200 OK` — the user object with a fresh one-time `token`.

### 3.5 Groups

Groups carry a token-quota policy that users can be assigned to (`user.group_id`). During a data-plane quota check, a user's group policy is the first tier of the resolution chain (group policy → global default).

Group objects returned by the API:

| Field | Type | Notes |
|-------|------|-------|
| `id` | integer | Server-assigned. |
| `name` | string | |
| `token_quota_rules` | string | JSON **text** containing a rule array. `""` means **unconfigured** (fall back to the global default); `"[]"` means explicitly unlimited; otherwise the normalised rule JSON. |
| `global_token_quota_rules` | string | Group-level global-quota override. `""` = unconfigured (fall back to the site rules and `global_quota_scope`); `"[]"` = explicitly unlimited; otherwise the group's own rules, counted **per group**. |
| `created_at` / `updated_at` | string | RFC 3339 timestamps. |

#### POST /admin/user-groups

| Field | Type | Required | Default | Notes |
|-------|------|----------|---------|-------|
| `name` | string | no | `""` | |
| `token_quota_rules` | string | no | `""` | Omitted/empty = unconfigured. Otherwise must be a valid JSON array (rules are validated and normalised). |
| `global_token_quota_rules` | string | no | `""` | Optional group-level override of the global quota; same validation. When set (including `"[]"`), the group is checked against its own rules with per-group usage. |

```bash
curl -X POST http://127.0.0.1:8080/admin/user-groups \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"name": "team-a", "token_quota_rules": "[{\"mode\":\"day\",\"limit\":1000000}]"}'
```

Response `201 Created` — a group object.

| Status | Condition |
|--------|-----------|
| 201 | Created. |
| 400 | Invalid JSON; `token_quota_rules` not a JSON array or failing rule validation. |
| 500 | Database error. |

#### GET /admin/user-groups

```json
{"user_groups": [ /* group objects */ ]}
```

#### GET /admin/user-groups/{id}

Response `200 OK` — a single group object. `404 group not found` when absent.

#### PUT /admin/user-groups/{id}

Partial update.

| Field | Type | Notes |
|-------|------|-------|
| `name` | string | |
| `token_quota_rules` | string | Same validation as create. |
| `global_token_quota_rules` | string | Same validation as create; `""` clears the override. |

Response `200 OK` — the updated group object.

#### DELETE /admin/user-groups/{id}

Deletes the group itself; it does **not** cascade to users or keys. A user whose `group_id` now dangles falls back to the global default rules.

Response `200 OK`:

```json
{"ok": true}
```

### 3.6 Usage queries

#### GET /admin/usage/summary

Aggregated token usage. Query parameters:

| Parameter | Type | Notes |
|-----------|------|-------|
| `group_by` | string | `date` (default), `model`, `key`, `user`, or `group`. |
| `start` / `end` | string | `YYYY-MM-DD`, interpreted in the business timezone; `end` includes the whole day. |
| `user_id` / `group_id` / `api_key_id` / `ai_model_id` | integer | Optional filters. |

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

The `date` field is present only when `group_by=date`. `rows` is `[]` when there is no data.

Money fields (details in §3.10):

| Field | Notes |
|-------|-------|
| `cost_platform_micros` | Settlement amount in integer micros of `platform_currency`. Already normalised at accounting time, so it adds up across any grouping. |
| `cost_vendor_micros` | Model-currency amount in integer micros. **Meaningful only when `group_by=model`** — other groupings may span currencies, so it is always `0` there. |
| `currency` | Currency of the row's amounts. Only unique for `group_by=model`; otherwise it is `MAX(currency)`. |
| `mixed_currency` | Present as `true` when the row spans several pricing currencies; treat the amounts per model then. |
| `total_cost_platform_micros` / `platform_currency` | Envelope-level sum of the returned rows, in the platform settlement currency. |

#### GET /admin/usage/logs

Per-request usage details, newest first. Accepts the same time-range and id filters as `summary`, plus `page` (default 1) and `page_size` (default 50, capped at 500).

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

Errors on both: `400` for an invalid date, `group_by`, or id filter; `500` on a database error.

#### GET /admin/usage/export

Streams usage details (the same fields as `/admin/usage/logs`) as a downloadable file. Accepts the same time-range and id filters as `logs`; the export is streamed in batches (never loading the whole table into memory).

| Parameter | Type | Notes |
|-----------|------|-------|
| `format` | string | `csv` (default) or `json`. Anything else returns `400 format must be csv or json`. |
| `start` / `end` / `user_id` / `group_id` / `api_key_id` / `ai_model_id` | — | Same semantics as `/admin/usage/logs`. |

- `format=csv` → `text/csv` attachment with a header row (`id,api_key_id,user_id,group_id,ai_model_id,model,provider,prompt_tokens,completion_tokens,total_tokens,prompt_cache_read_tokens,prompt_cache_write_tokens,status_code,latency,cost_vendor_micros,cost_platform_micros,created_at`).
- `format=json` → `{"logs": [ … ]}` streamed with the same row objects as `logs` (`cost_vendor_micros`, `cost_platform_micros` and `currency` included).

Money amounts are **integer micros** in both formats — see §3.10 for the unit and the meaning of the two columns.

```bash
curl -OJ http://127.0.0.1:8080/admin/usage/export?format=csv&start=2026-09-01   -H "Authorization: Bearer <ADMIN_TOKEN>"
```

### 3.7 Settings

Global settings, stored as a single row.

| Field | Type | Notes |
|-------|------|-------|
| `default_token_quota_rules` | string | JSON text containing a rule array. Applied to a user with no group policy. |
| `global_token_quota_rules` | string | JSON text containing a rule array. The global tier. |
| `global_quota_scope` | string | `site` or `group`. How global usage is counted. |
| `user_key_limit` | integer | Maximum self-service keys per user; default `5`; `0` = unlimited (see §4.4). |
| `platform_currency` | string | Platform settlement currency (ISO 4217, upper case); default `CNY`. Every request's `cost_platform_micros` is converted to this currency at accounting time and frozen there. |

#### GET /admin/settings

Returns the current settings, creating the default row on first access. Defaults are `"[]"`, `"[]"`, `site`, `5`, `CNY`.

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

Partial update.

| Field | Type | Notes |
|-------|------|-------|
| `default_token_quota_rules` | string | Must be a valid JSON **array** (an empty string or `null` is rejected). |
| `global_token_quota_rules` | string | Must be a valid JSON array. |
| `global_quota_scope` | string | Must be `site` or `group`. |
| `user_key_limit` | integer | Must be `>= 0`; `0` means unlimited. |
| `platform_currency` | string | Must be a 3-letter ISO 4217 code; an empty string is rejected (there is no "unset" settlement currency). Case-insensitive, stored upper case. Changing it only affects requests accounted afterwards. |

```bash
curl -X PUT http://127.0.0.1:8080/admin/settings \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"global_quota_scope": "group", "user_key_limit": 10}'
```

Response `200 OK` — the full settings object.

| Status | Condition |
|--------|-----------|
| 200 | Updated. |
| 400 | Invalid JSON; rules not a JSON array; invalid scope; `user_key_limit < 0`; invalid `platform_currency`. |
| 500 | Database error. |

### 3.8 Webhooks

Webhook subscriptions receive outbound events. Delivery is asynchronous and signed; see the appendix for the exact semantics.

Webhook objects returned by the API:

| Field | Type | Notes |
|-------|------|-------|
| `id` | integer | Server-assigned. |
| `name` | string | |
| `url` | string | Receiver URL; must be `http` or `https`. |
| `secret` | string | **Always a mask** (write-only). |
| `event_types` | array | Subscribed types; `[]` means all. |
| `enabled` | bool | Disabled subscriptions receive nothing. |
| `created_at` / `updated_at` | string | RFC 3339 timestamps. |

#### POST /admin/webhooks

| Field | Type | Required | Default | Notes |
|-------|------|----------|---------|-------|
| `name` | string | no | `""` | |
| `url` | string | **yes** | — | Must parse as an `http`/`https` URL with a host. |
| `secret` | string | no | `""` | Used to sign deliveries (at most 256 bytes); never read back. |
| `event_types` | array | no | `[]` | Non-empty strings; `[]` (or omitted) subscribes to all. |
| `enabled` | bool | no | `true` | |

```bash
curl -X POST http://127.0.0.1:8080/admin/webhooks \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"name": "siem", "url": "https://example.com/hooks/llm",
       "secret": "<HMAC_SECRET>", "event_types": ["quota.exceeded"]}'
```

Response `201 Created` — a webhook object; `secret` is a mask.

| Status | Condition |
|--------|-----------|
| 201 | Created. |
| 400 | Invalid JSON; invalid URL; an `event_types` entry that is empty. |
| 500 | Database error. |

#### GET /admin/webhooks

```json
{"webhooks": [ /* webhook objects, secrets masked */ ]}
```

#### GET /admin/webhooks/{id}

Response `200 OK` — a single webhook object (secret masked). `404 webhook not found` when absent.

#### PUT /admin/webhooks/{id}

Partial update.

| Field | Type | Notes |
|-------|------|-------|
| `name` | string | |
| `url` | string | Must be a valid `http(s)` URL. |
| `secret` | string | **An empty string means "keep the current secret"** — the field cannot be read back, so it also cannot be cleared with an empty value. |
| `event_types` | array | Replaces the subscription set; `[]` subscribes to all. |
| `enabled` | bool | |

Response `200 OK` — the updated webhook object.

#### DELETE /admin/webhooks/{id}

Response `200 OK`:

```json
{"ok": true}
```

#### GET /admin/webhooks/{id}/deliveries

Paginated delivery records for the subscription, newest first (`page` default 1, `page_size` default 50, capped at 500).

| Field | Type | Notes |
|-------|------|-------|
| `id` | integer | Delivery ID. |
| `webhook_id` | integer | Owning subscription. |
| `event_type` | string | Delivered event type. |
| `status` | string | `pending`, `success`, or `failed`. |
| `attempts` | integer | Attempts made so far. |
| `last_status` | integer | HTTP status of the last attempt (`0` for a network error). |
| `last_error` | string | Error text of the last attempt. |
| `created_at` / `updated_at` | string | RFC 3339 timestamps. |

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

### 3.9 Import

Import existing data (groups, users, models, keys) in one call.

**Processing order** is fixed as `user_groups → users → models → keys`: a later entry may reference an entity created earlier in the same request. `dry_run=true` validates and plans without writing.

**Idempotency keys** — an entity that already exists is reported as `skipped`:

| Kind | Idempotency key |
|------|-----------------|
| `groups` | `name` |
| `users` | `name` |
| `models` | `(provider, model_id)` |
| `keys` | `token` (compared after hashing) |

**Request body**

| Field | Type | Notes |
|-------|------|-------|
| `dry_run` | bool | Plan only, no writes. |
| `groups[].name` | string | Required. |
| `user_groups[].token_quota_rules` | string | Optional; validated like `/admin/user-groups`. |
| `users[].name` | string | Required. |
| `users[].role` | string | `admin` or `user`; default `user`. |
| `users[].group` | string | Group **name**; empty = no group. Must resolve to an existing/planned group. |
| `models[].provider` / `model_id` | string | `model_id` required; `(provider, model_id)` is the key. |
| `models[].url` | string | Required. |
| `models[].api_key` | string | Optional. |
| `models[].model_type` | string | `openai-completions`, `anthropic-messages`, `openai-responses`, `openai-decisions`, `systemone` or `gemini-generate-content`. |
| `models[].quota_day` / `enabled` | integer / bool | Optional. |
| `keys[].token` | string | **Plaintext**; required. Hashed on import and never stored or echoed. |
| `keys[].user` | string | Owner **name**; required and must resolve. |
| `keys[].group` | string | Group name; empty = none. |
| `keys[].name` / `max_tokens` | string / integer | Optional. |
| `keys[].bindings[]` | array | `{model, role, api_key?}`, where `model` is a **model_id**. Must have exactly one `primary` and no duplicates when non-empty. An optional `api_key` carries the per-binding (BYOK) key; it is encrypted at rest. |

**Response**

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

- `details[].kind` is `group` / `user` / `model` / `key`.
- `details[].action` is `created` / `skipped` / `error`; `message` is present for errors.
- `details[].ref` carries only non-sensitive identifiers (a name, `provider/model_id`, or a token display prefix) — never the token plaintext.

A single failing entry is recorded and the rest continue; the request itself returns `200`. Imported users do **not** return an access token (one is generated and its plaintext discarded); issue one later with `POST /admin/users/{id}/rotate`.

```bash
curl -X POST http://127.0.0.1:8080/admin/import \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "dry_run": true,
    "user_groups": [{"name": "team-a", "token_quota_rules": "[{\"mode\":\"day\",\"limit\":1000000}]"}],
    "users": [{"username": "bob", "role": "user", "group": "team-a"}],
    "models": [{"provider": "zhipu", "model_id": "glm-5.1", "url": "https://open.bigmodel.cn/api/paas/v4",
                "api_key": "<UPSTREAM_KEY>", "model_type": "openai-completions"}],
    "keys": [{"name": "bob-default", "user": "bob", "group": "team-a", "token": "sk-legacy-…",
              "bindings": [{"model": "glm-5.1", "role": "primary"}]}]
  }'
```

### 3.10 Cost billing

The gateway can bill by **money** next to tokens: each model carries a price table, every
request records what it cost, quotas can be limited in money, and the usage reports expose
the amounts. Nothing here is on by default — a model with no `currency` simply keeps
recording `0`.

#### Money is integer micros

Every amount in the API is an **integer number of micros**, one micro being `1e-6` of a
currency unit (`_micros` suffix). One currency unit is `1000000` micros, so four decimal
places (and two more) are exact: `0.0025` CNY is `2500`. Arithmetic on the server is
all-integer; clients divide by `1e6` for display.

Price columns are **micros per million tokens**: "1 CNY per million tokens" is `1000000`,
"0.02 CNY per million" is `20000`.

#### Two money columns, two currencies

Each usage row carries two amounts, both frozen at accounting time:

| Column | Currency | Meaning |
|--------|----------|---------|
| `cost_vendor_micros` | `ai_models.currency` of the model at accounting time | What the request cost in the provider's currency. |
| `cost_platform_micros` | `settings.platform_currency` (default `CNY`) | The same cost converted to the settlement currency, via the FX rate effective at accounting time. |

Both are written once, when the request is accounted. Changing a price, a model currency,
an FX rate or `platform_currency` later **never rewrites history**: old rows keep the
amounts they were billed at. Same-currency models need no rate (`platform = vendor`).
When the model currency differs from the settlement currency and no rate covers the
accounting instant, the vendor amount is still stored and the platform amount is recorded
as `0`.

Usage rows do not carry the model currency: join `ai_models.currency` for
`cost_vendor_micros`. That is why a summary row's `currency` describes only
`cost_platform_micros` / the `group_by=model` amounts, and why `cost_vendor_micros` is `0`
outside `group_by=model`.

#### Price snapshots and the six raw columns

Prices live in append-only snapshots, written through the routes below. A snapshot always
carries six **raw** columns — three peak tiers and three off-peak tiers, never a multiplier:

| Column | Tier |
|--------|------|
| `peak_input_hit_micros` / `offpeak_input_hit_micros` | Input, cache hit |
| `peak_input_miss_micros` / `offpeak_input_miss_micros` | Input, cache miss |
| `peak_output_micros` / `offpeak_output_micros` | Output |

The cost of a request is `Σ(tokens in tier × that tier's price_micros) / 1000000`, picking
one tier set per request:

1. **Upstream-reported cost wins.** When `use_upstream_cost` is `true` (the default) and the
   upstream response carries `usage.cost > 0`, that value (rounded to micros) is stored as
   `cost_vendor_micros` and no price is consulted.
2. **Otherwise the gateway computes it**, and `enable_peak_pricing` is the switch:
   - `enable_peak_pricing = false` (the default) → **always the off-peak columns**, even if
     `peak_hours` is configured or broken;
   - `enable_peak_pricing = true` → the peak/off-peak decision below;
   - when the schedule cannot be evaluated (no `peak_hours`, bad timezone, malformed window,
     all `days` out of range) the request is billed at **peak** prices — fail closed, never a
     silent discount. Only models that opted in can take this path.

#### Peak hours and the billing calendar

`peak_hours` is per price snapshot and is passed as a JSON **string**:

```json
{
  "timezone": "Asia/Shanghai",
  "windows": [
    {"days": [1, 2, 3, 4, 5], "start": "09:00", "end": "12:00"},
    {"days": [1, 2, 3, 4, 5], "start": "14:00", "end": "18:00"}
  ]
}
```

- `timezone` is an IANA name; `windows` must be non-empty when a timezone is given.
- `days` uses Go `time.Weekday` numbering (`0` = Sunday … `6` = Saturday) and names the day
  the window **starts**.
- `start`/`end` are `HH:MM` (`24:00` is valid). The interval is half-open `[start, end)`;
  `end <= start` wraps past midnight, and the next day's `[00:00, end)` also counts.
- `{}` (or an empty string / `null`) means "no schedule".
- The **billing calendar** overrides the weekday rule: a date registered with
  `is_workday: false` is never peak (holidays), a date registered `true` stays a working day
  (make-up working days). Unregistered dates fall back to Monday–Friday = working day.
- Admin writes reject a `peak_hours` that could not be evaluated (bad timezone, malformed
  clock, unusable `days`), because such a value would silently bill peak forever.

#### Routes

| Route | Purpose |
|-------|---------|
| `GET /admin/models/{id}/prices` | List price snapshots, newest first: `{"prices": [...]}`. |
| `POST /admin/models/{id}/prices` | Append a snapshot; `201` with the row. `effective_at` (RFC 3339) is required; `currency` defaults to the model's. A duplicate `effective_at` for the same model returns `409`. |
| `DELETE /admin/model-prices/{id}` | Delete a price row by **price** id: `{"ok": true}`. |
| `GET /admin/billing-calendar?year=2026[&month=10]` | Registered dates for the year (or month): `{"year": 2026, "days": [...]}`. `year` defaults to the current business-timezone year. |
| `PUT /admin/billing-calendar` | Upsert by date: `{"days": [{"date": "2026-10-01", "is_workday": false, "note": "国庆"}]}`. Non-empty array, unique dates, `is_workday` required on every entry; responds with the same shape. |
| `DELETE /admin/billing-calendar/{date}` | Remove the override for a `YYYY-MM-DD` date: `{"ok": true}`. |
| `GET /admin/currency-rates?from=USD&to=CNY` | Rate snapshots for the pair (both required): `{"from_currency": "USD", "to_currency": "CNY", "rates": [...]}`. |
| `POST /admin/currency-rates` | Append a rate: `{"from_currency", "to_currency", "effective_at", "rate_micros"}` → `201`. Same currency returns `400`; duplicate `effective_at` returns `409`. |
| `DELETE /admin/currency-rates/{id}` | Delete a rate row by id: `{"ok": true}`. |

`rate_micros` reads "1 unit of `from_currency` = `rate_micros/1e6` units of `to_currency`"
(the usual FX quotation, applied as a multiplication). It must be between `1` and
`1000000000`.

Every successful write invalidates the data plane's in-process cache for that kind of data,
so the next request already sees the new price, calendar or rate — no restart needed.

```bash
# Price a model at 1 CNY / 0.3 CNY (cache hit) per million tokens, peak twice the off-peak
curl -X POST http://127.0.0.1:8080/admin/models/1/prices \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "effective_at": "2026-11-01T00:00:00Z",
    "peak_hours": "{\"timezone\":\"Asia/Shanghai\",\"windows\":[{\"days\":[1,2,3,4,5],\"start\":\"09:00\",\"end\":\"12:00\"}]}",
    "offpeak_input_hit_micros": 150000, "offpeak_input_miss_micros": 4500000, "offpeak_output_micros": 13500000,
    "peak_input_hit_micros": 300000,    "peak_input_miss_micros": 9000000, "peak_output_micros": 27000000
  }'

# Bill an upstream that prices in USD against a CNY settlement currency
curl -X POST http://127.0.0.1:8080/admin/currency-rates \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"from_currency": "USD", "to_currency": "CNY", "effective_at": "2026-11-01T00:00:00Z", "rate_micros": 7100000}'
```

#### Quota rules in money

Quota rules gained an optional `unit`:

| `unit` | Window usage compared with `limit` |
|--------|------------------------------------|
| `""` / `"token"` (default) | `SUM(total_tokens)` — unchanged behaviour of every pre-existing rule. |
| `"cost"` | `SUM(cost_platform_micros)` against `limit × 1_000_000`; `limit` is a **whole-unit** amount of the settlement currency, capped at `9000000000000`. |

Anything else in `unit` is rejected on write. `unit=token` (and the empty string) is
normalised away, so stored rule JSON for existing rules stays byte-identical. See
`/api/user/quota` (§4.2) for how the unit surfaces in the per-rule view.

#### Quota rejections carry `unit` and `currency`

A quota rejection still logs `… token quota exceeded` and still returns
`-quota-exceeded-status` (default `429`) with `Retry-After`, but the error body now names
the measurement:

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

`unit` is always present (`token` or `cost`; a rule without `unit` reports `token`).
`currency` is present only for `cost` rules and is the platform settlement currency. On
`/v1/messages` the envelope stays Anthropic-shaped and carries no extra keys.

### 3.11 Model routing

A **model router** is one routing rule: a name, a **strategy**, strategy-private
`config` JSON, a **scope** plus scope id, a **priority**, and an enabled flag. Only
requests naming `model: "auto"` are routed; a request naming a real model is never
touched.

**Which rule applies.** The most specific scope wins — `key` > `user` > `group` >
`global`; within one scope the lowest `priority` wins and duplicates are rejected at
write time. An ungrouped key (`group_id = 0`) never matches a `group` rule. If no rule
applies, `auto` returns `400 model auto is not available`.

**Strategies are pluggable.** `GET /admin/model-routers/strategies` publishes each
strategy's config field list so a client can render a form without hardcoding any
strategy. Fields carry `key`, `type` (`string` | `int` | `bool` | `enum` | `model`),
`title`, `description`, `default`, `enum`, `min`, `max` and `required`.

| Route | Purpose |
|-------|---------|
| `GET /admin/model-routers` | List rules in match order: `{"routers": [...]}`. |
| `GET /admin/model-routers/strategies` | Available strategies and their config fields: `{"strategies": [...]}`. `503` when the data plane is not wired. |
| `POST /admin/model-routers` | Create: `{"name", "strategy", "config", "scope", "scope_id", "priority", "enabled"}` → `201`. `enabled` defaults to `true`. `config` may be an object or its JSON string. |
| `GET /admin/model-routers/{id}` | One rule. |
| `PUT /admin/model-routers/{id}` | Partial update; `null`/absent fields are unchanged. Updating `enabled` alone does not disturb scope or priority. |
| `DELETE /admin/model-routers/{id}` | Delete: `204`. Requests it routed fall back to their key's primary model. |
| `POST /admin/model-routers/{id}/preview` | Run this rule's strategy for a real key without sending anything upstream: `{"api_key_id", "request": {...}}` → `{"router_id", "strategy", "ai_model_id", "model_id", "cause", "fallback"}`. |

**Validation.** Structural checks (name and strategy present, legal scope, scope id
required for non-global scopes, unique `(scope, scope_id, priority)`) happen in the
admin plane; the strategy's own validator checks the config (required fields, unknown
fields, ranges). Both run before the row is written, so a misconfigured rule is a `400`
rather than a runtime surprise. `config` is normalized by the strategy on write, so the
stored JSON is self-describing.

**Strategies.** `decision` asks a decision-protocol model which bound candidate is the
cheapest that can still handle the request. Its config: `decision_model_id` (required,
an `ai_models` id whose `model_type` is a decision protocol), `max_candidates`,
`max_short_tokens`, `max_context_tokens`, `max_chars`, `timeout_ms`,
`cooldown_seconds`, `short_circuit_enabled`.

**Billing and observability.** The decision call is recorded in usage detail with
`count_toward_quota = 0`: its cost is visible but it does not consume the user's quota.
`llm_usage_logs.router_id` records which rule served a request, and the routed model and
rule are returned in the `X-AIGate-Routed-Model` and `X-AIGate-Router-Id` response
headers. The response body still echoes `"auto"` as asked.

### 3.12 Request Logs

Request logs record the raw client-side request/response pair for every inference
ingress. They are **off by default** (`-request-log=none`): the two routes below are
registered only when a plugin is enabled and return `404` otherwise. Only admins can
read them (they hang off the existing `/admin/` auth subtree). Each captured request
carries an `X-AIGate-Request-Id` response header equal to the log row's `id`. See
[docs/request-log.md](request-log.md) for the plugin contract, per-implementation
configuration, retention and the recipe for adding an implementation.

#### GET /admin/request-logs

Paginated request-log rows, newest first. **Rows contain metadata only** — no request or
response bodies and no headers; fetch `GET /admin/request-logs/{id}` for the full entry.
All 13 filter parameters are optional and may be combined; `0` / empty means "no filter".

| Parameter | Type | Notes |
|-----------|------|-------|
| `start` / `end` | string | Time range. `YYYY-MM-DD` (business timezone, whole day; `end` includes that day) or RFC3339 (`2026-11-25T10:30:00+08:00` / `...Z`). Both bounds are normalised to a half-open UTC interval `[start, end)`: a date `end` becomes exclusive via +1 day, an RFC3339 `end` is exclusive as given. A value containing `T` is parsed as RFC3339. `start > end` → `400 invalid time range`. |
| `api_key_id` | integer | Filter by key. The admin plane resolves the key's current `proxy_token_hash` and filters on it; an unknown key → `404 api key not found`. When given together with `api_key_hash`, `api_key_id` wins. |
| `api_key_hash` | string | Filter directly by credential sha256 hex. **After a key rotate, older rows are reachable only via this parameter**, not `api_key_id`. |
| `model` | string | Exact match on the model name parsed from the request body. |
| `path` | string | Exact match on the ingress path, e.g. `/v1/messages`. |
| `status_code` | integer | Exact HTTP status code match. |
| `stream` | bool | `true` / `false` — only streaming or non-streaming rows. |
| `min_latency_ms` / `max_latency_ms` | integer | Inclusive latency bounds (ms). `min > max` → `400 invalid latency range`. |
| `q` | string | Full text, best effort: `LIKE` in sqlite, `multi_match` in ES; no consistent scoring is promised. |
| `page` / `page_size` | integer | Defaults `1` / `50`; `page_size` capped at `500`. `(page-1)*page_size + page_size` must not exceed the shared `10000` result window, else `400 page out of range`. |

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

`logs` is `[]` (never `null`) when nothing matches. Both implementations return the same
field set and the same filter/pagination semantics. `created_at` is UTC; `error` is a
status classification only when `status_code >= 400`; `truncated` marks a body cut at the
1 MiB capture limit.

Errors: `400` (invalid parameter, time/latency range, or page outside the result
window), `401` (missing/invalid token), `403` (not an admin), `404` (`api key not found`
when `api_key_id` refers to a missing key), `500` (storage error).

#### GET /admin/request-logs/{id}

Returns the full entry for one request: the list row plus `request_body`,
`response_body`, `request_headers` and `response_headers`. Body fields are raw text
(streaming rows carry the raw SSE bytes); header fields are JSON objects of whitelisted
headers. Credential headers are never stored.

Path parameter `id` — the 32-character hex request ID from `X-AIGate-Request-Id`.

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
  "request_headers": "{\"Content-Type\":\"application/json\",\"User-Agent\":\"openai-python/1.0\"}",
  "response_headers": "{\"Content-Type\":\"application/json\",\"X-AIGate-Request-Id\":\"0123456789abcdef0123456789abcdef\"}"
}
```

Errors: `400` (empty id), `401` (missing/invalid token), `403` (not an admin),
`404` (`request log not found`), `500` (storage error).

## 4. User plane

Prefix `/api/user/*`. Any enabled user token (`user` or `admin`) may authenticate. A `user`-role token is forced to act on its own data (its `user_id` is ignored); an `admin`-role token may target another user with `?user_id=`.

### 4.1 GET /api/user/me

Returns the caller's identity, including the group name (empty when ungrouped or when the group has been deleted).

```json
{"id": 2, "username": "alice", "role": "user", "group_id": 1, "group_name": "team-a"}
```

### 4.2 GET /api/user/quota

Per-rule view of the target user's effective quota rules (resolved through the group policy → global default chain).

| Field | Type | Notes |
|-------|------|-------|
| `user_id` | integer | The resolved target user. |
| `usages[]` | array | One entry per effective rule. |
| `usages[].rule_index` | integer | Rule index within the resolved set. |
| `usages[].mode` | string | `day`, `month`, `year`, or `custom`. |
| `usages[].limit` | integer | `-1` means the window is computed but usage is unlimited. With `unit=cost` it is a whole-unit amount of the settlement currency (see §3.10). |
| `usages[].unit` | string | `token` or `cost`. Always a literal, so a rule written without `unit` reports `token`. |
| `usages[].used` | integer | Usage in the current window: tokens when `unit=token`, settlement-currency **micros** when `unit=cost`. |
| `usages[].active` | bool | Whether the rule currently applies. |
| `usages[].period_start` / `period_end` | integer \| null | Window bounds as Unix seconds; `null` end means open-ended. |

```json
{
  "user_id": 2,
  "usages": [
    {"rule_index": 0, "mode": "day", "limit": 1000000, "unit": "token", "used": 25310,
     "active": true, "period_start": 1758038400, "period_end": 1758124800}
  ]
}
```

### 4.3 Usage queries

`GET /api/user/usage/summary`, `GET /api/user/usage/logs`, and `GET /api/user/usage/by-model` mirror the admin endpoints but are row-level isolated: a `user`-role token always sees its own usage regardless of any `user_id` filter.

| Endpoint | Notes |
|----------|-------|
| `GET /api/user/usage/summary` | Same parameters and response as `/admin/usage/summary` (`group_by`, `start`/`end`, id filters). |
| `GET /api/user/usage/logs` | Same as `/admin/usage/logs`, with `page` / `page_size` pagination (newest first). |
| `GET /api/user/usage/by-model` | Same as `summary` but always grouped by `model`; a `group_by` parameter is ignored. |

### 4.4 Self-service keys

`/api/user/keys` lets a user mint and manage their own data-plane keys. Two guardrails apply:

- **Count limit** — `settings.user_key_limit` (default `5`; `0` = unlimited). Exceeding it returns `400 key limit reached (limit N)`.
- **Model scope** — a new or updated key may only bind models that already appear in **any** of the user's existing keys. Binding a model outside that set returns `400 model not available for this user`; if the user has no bindable models at all, it returns `400 no available models, please contact an administrator`.

Isolation: a key that is missing **or** owned by someone else returns `404 key not found`, so ids cannot be probed.

Methods: `POST /api/user/keys`, `GET /api/user/keys`, `GET /api/user/keys/{id}`, `PUT /api/user/keys/{id}`, `DELETE /api/user/keys/{id}`, `POST /api/user/keys/{id}/rotate`.

The key object is the same shape as the admin key object (§3.3). `token` is returned only by create and rotate.

#### POST /api/user/keys

| Field | Type | Required | Default | Notes |
|-------|------|----------|---------|-------|
| `name` | string | no | `""` | |
| `max_tokens` | integer | no | `0` | `<= 0` means no cap. |
| `bindings` | array | no | none | `{ai_model_id, role, sort_order, api_key?}`; exactly one `primary` when non-empty, and every model must be within the user's scope. The optional `api_key` is the user's own upstream key for BYOK models (encrypted at rest, never echoed). |

`token_quota_rules` is **read-only** on the user plane: the field is returned by the key APIs but attempts to set it are ignored (only administrators can configure key-level quotas).

```bash
curl -X POST http://127.0.0.1:8080/api/user/keys \
  -H "Authorization: Bearer <USER_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-key", "max_tokens": 4096,
       "bindings": [{"ai_model_id": 1, "role": "primary"}]}'
```

Response `201 Created` — the key object with a one-time `token`.

| Status | Condition |
|--------|-----------|
| 201 | Created. |
| 400 | Invalid JSON; key limit reached; a model outside the allowed scope; no available models; an invalid binding set. |
| 404 | The target user does not exist. |

#### GET /api/user/keys

```json
{"keys": [ /* key objects, without token */ ]}
```

#### GET /api/user/keys/{id}

Response `200 OK` — a single key object (no `token`). `404 key not found` when absent or not owned.

#### PUT /api/user/keys/{id}

Partial update.

| Field | Type | Notes |
|-------|------|-------|
| `name` | string | |
| `max_tokens` | integer | |
| `enabled` | bool | |
| `bindings` | array | When present, replaces all bindings; re-validated against the model scope. |

#### DELETE /api/user/keys/{id}

Response `200 OK`:

```json
{"ok": true}
```

#### POST /api/user/keys/{id}/rotate

Issues a new `sk-` credential and invalidates the old one immediately. Response `200 OK` — the key object with a fresh one-time `token`.

#### GET /api/user/keys/{id}/usage

Per-day usage trend of a single key (the same daily aggregation as `/api/user/usage/summary`). `user`-role tokens may only query their own keys (anything else is `404 key not found`); an `admin` token may query any key with `?user_id=`.

| Parameter | Type | Notes |
|-----------|------|-------|
| `start` / `end` | string | `YYYY-MM-DD` (business timezone, inclusive). When both are omitted the range defaults to the **last 30 days**. |

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

## 5. Health check

### GET /healthz

No authentication.

Response `200 OK`:

```json
{"status": "ok"}
```

## 6. Build info

### GET /version

No authentication. Returns the build and startup identity of the running binary, so an
operator can confirm which code and commit a deployment actually serves. It carries no
configuration, dependency or endpoint information.

Response `200 OK`:

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

| Field | Meaning |
|-------|---------|
| `version` | Build-time string: the injected value (`-ldflags`), falling back to the compiled-in default. `make build` injects `git describe --tags --always --dirty` (e.g. `v0.1.0-59-g6d75021-dirty`), so it is **not** strict SemVer — do not parse it as one. |
| `commit` | Full git commit; injected value first, otherwise the Go toolchain's VCS stamp. `unknown` when neither is available. |
| `build_time` | RFC3339 UTC build (commit) time; `unknown` when unavailable. |
| `dirty` | `true` when the tree had uncommitted changes at build time, i.e. the binary matches no commit exactly. Only meaningful for builds made directly from a working tree: Docker and CI builds copy a clean tree, so it is always `false` there. |
| `go_version` | Go toolchain that compiled the binary. |
| `started_at` | RFC3339 UTC process start time; `unknown` if the process did not record it. |

## 7. Appendix

### Status code summary

| Status | Where | Meaning |
|--------|-------|---------|
| 200 | all planes | Success; `DELETE` returns `{"ok": true}`. |
| 201 | admin writes (models, credentials, keys, users, groups, webhooks); user-plane key creation | Resource created. |
| 400 | all planes | Malformed body, invalid path id, or a field/business validation failure. |
| 401 | all planes | Credential missing, malformed, unknown, or disabled. |
| 403 | admin plane | Credential is valid but the user's role is not `admin`. |
| 404 | admin / user planes | Target resource does not exist (or is not owned). |
| 409 | admin plane | Credential is still referenced by a model and cannot be deleted; a price or currency rate already exists at that `effective_at`. |
| 429 / 403 | data plane | Quota exceeded; the exact status is `-quota-exceeded-status` (default `429`) and a `Retry-After` header is set. The body carries `unit` (`token` / `cost`) and, for cost rules, `currency` (§3.10). |
| 500 | all planes | Internal error. |
| 502 | data plane | Upstream connection failure or an untranslatable upstream response. |

### Authentication failures: 401 vs 403

| Condition | Status | `message` |
|-----------|--------|-----------|
| Header missing, not `Bearer …`, or token unknown | 401 | `missing or invalid bearer token` |
| Token valid but the user is disabled | 401 | `user is disabled` |
| Token valid, user enabled, role not permitted | 403 | `insufficient role` |

On the data plane, an absent credential yields `401 missing API key`; an unknown key and a disabled key are indistinguishable and both yield `401 invalid API key`. On `/v1/messages`, the same conditions yield `401` with `error.type = authentication_error`.

### Webhook event delivery

**Envelope.** Each delivery `POST`s a JSON body to the subscription URL:

```json
{
  "id": "11",
  "type": "quota.exceeded",
  "created_at": "2026-09-01T16:00:00Z",
  "data": { }
}
```

`id` is the delivery ID (also the row id in `/admin/webhooks/{id}/deliveries`); `created_at` is UTC.

**Headers.**

| Header | Value |
|--------|-------|
| `Content-Type` | `application/json` |
| `X-Gateway-Event` | Event type (e.g. `request.completed`) |
| `X-Gateway-Delivery` | Delivery ID |
| `X-Gateway-Signature` | `sha256=<hex(hmac-sha256(secret, raw_body))>` |

Verify the signature by computing HMAC-SHA256 over the **raw request body** with the subscription secret, hex-encoding it, and comparing it to the value after `sha256=`.

**Delivery semantics.**

- Delivery is asynchronous and serialised by a single consumer; it never blocks the data plane.
- Each attempt has a **10-second timeout**. The delivery is retried on a non-2xx response or a network error.
- **At most 3 attempts**; before the 2nd and 3rd attempts the dispatcher waits **1s** and **5s** respectively.
- The delivery record is updated on every attempt (`attempts`, `last_status`, `last_error`) and ends in a terminal `success` or `failed`.
- The event queue (`-webhook-buffer`, default `256`) is bounded: when it is full, new events are dropped and counted (`webhook_dropped`) rather than blocking.
- Retries reuse the same delivery ID, so a receiver may see the same event more than once. Treat delivery as **at-least-once** and deduplicate by `X-Gateway-Delivery`. After the final attempt fails, the event is not re-sent.

**Event types and `data` fields.**

| `type` | Emitted when | `data` fields |
|--------|--------------|---------------|
| `request.completed` | A data-plane forward (including streaming) finishes. | `api_key_id`, `user_id`, `group_id`, `model`, `provider`, `status_code`, `latency_ms`, `usage` (`prompt_tokens`, `completion_tokens`, `total_tokens`, `prompt_cache_read_tokens`, `prompt_cache_write_tokens`) |
| `quota.exceeded` | A request is rejected by a quota check. | `api_key_id`, `user_id`, `group_id`, `model`, `kind` (`global` / `model` / `user` / `key`), `limit`, `used` |
| `key.disabled` | An admin disables or deletes a key. | `key_id`, `name`, `user_id`, `reason` (`disabled` / `deleted`) |
| `key.expired` | A key is used after its `expires_at` (emitted once per validity window, on the first rejection). | `key_id`, `name`, `user_id` |

Only enabled subscriptions whose `event_types` is empty or contains the event type receive it.

### Secret encryption

The gateway can encrypt secret columns at rest with AES-256-GCM:

| Topic | Detail |
|-------|--------|
| Covered columns | `ai_models.api_key`, `api_key_models.api_key` (per-binding upstream key), `credentials.secrets_json`, `webhooks.secret`. |
| Ciphertext format | `enc:v1:` + base64(nonce ‖ ciphertext); a value with the prefix is treated as ciphertext, anything else as plaintext. |
| Master key | 64 hex characters or base64 decoding to 32 bytes (`-master-key` / `AIGATE_MASTER_KEY`). |
| Not configured | Values are stored in plaintext and a warning is logged at startup; reads still work. |
| Configured | New/updated values are encrypted; existing plaintext rows are left as-is until migrated. |
| Migration | `-migrate-encrypt` encrypts existing plaintext rows in place and exits, printing `migrated`/`skipped`/`failed`; it is idempotent and requires a master key. |

See [docs/deploy.md](deploy.md) for the full runbook, including the warning to back up the master key (a lost key makes every encrypted value unreadable).

### Operational notes

| Topic | Detail |
|-------|--------|
| Upstream URL joining | See "Upstream URL joining" below. Trailing slashes on `url` are trimmed. |
| SSRF policy | Outbound requests (upstreams and webhook receivers) are validated by default, which **rejects private/loopback addresses**. Local endpoints require `-allow-private-upstreams` (or `AIGATE_ALLOW_PRIVATE_UPSTREAMS=true`). |
| Body size | 1 MiB per request; larger bodies are rejected (`413` on the data plane, `400` on the admin/user planes) — never truncated. |
| Business timezone | Affects quota windows and usage/date bookkeeping only; it does not change API responses. |
| Usage write path | Usage is written asynchronously; under extreme load a small number of entries can be dropped and the gateway is not the billing source of truth. |
| Delivery retention | `webhook_deliveries` rows are kept indefinitely — there is no TTL, pruning, or table cap. |

#### Upstream URL joining

`ai_models.model_type` has three values, and their `url` semantics **differ from each other** —
check them one by one when configuring a model:

| `model_type` | `url` should be | The gateway actually requests |
|--------------|-----------------|-------------------------------|
| `openai-completions` | `<host>/v1` | `<url>/chat/completions` |
| `openai-responses` | `<host>/v1` | `<url>/responses` |
| `anthropic-messages` | `<host>` (**without** `/v1`) | `<url>/v1/messages` |
| `openai-decisions` | `<host>/v1` | `<url>/decisions` |
| `systemone` | `<host>/v1` | `<url>/systemone` |
| `gemini-generate-content` | `https://generativelanguage.googleapis.com/v1beta` | `<url>/models/{model}:generateContent` (streaming: `<url>/models/{model}:streamGenerateContent?alt=sse`) |

Trailing slashes on `url` are trimmed. Hence `https://api.openai.com/v1` with `openai-responses`
becomes `https://api.openai.com/v1/responses`, while the same `url` with `anthropic-messages`
becomes `https://api.openai.com/v1/v1/messages` (wrong).

An empty or unknown `model_type` is treated as `openai-completions` (for compatibility with
historical data); set it explicitly when creating a model.

#### Gemini upstream (`gemini-generate-content`) limits

- **Single candidate only**: `n > 1` returns 400.
- **Images**: `image_url` must be an `https://` URL with a file extension
  (`.png`/`.jpg`/`.jpeg`/`.webp`/`.gif`/`.heic`/`.heif`) or a base64 data URI. The gateway
  never downloads the image; an `https` URL is fetched by Google. A missing extension or
  an `http://` URL returns 400.
- **Thought signature**: multi-turn tool calling on Gemini 3.x / 2.5 depends on
  `thoughtSignature`. The gateway encodes it into the returned `tool_calls[].id`
  (as `call_xxx_ts_<signature>`); clients must echo that `id` back unchanged. Through the
  `/v1/responses` ingress the signature degrades to Gemini's skip sentinel, because that
  ingress does not carry reasoning items.
- **Not supported**: audio, video and PDF input; Google-side tools (`googleSearch`,
  `codeExecution`); Vertex AI endpoints; explicit `cachedContents` management.

#### Responses stateful options and retention

| Topic | Detail |
|-------|--------|
| Storage switch | `-allow-response-storage` (`AIGATE_ALLOW_RESPONSE_STORAGE`, default `true`). Once `false`, every request is treated as `store:false` and `store:true` / `previous_response_id` / `conversation` / `background` all return `400`. |
| Retention | `-response-retention-days` (`AIGATE_RESPONSE_RETENTION_DAYS`, default `7`): days to retain `response_states` rows; `0` = keep forever; must be between `0` and `36500`. The gateway prunes once at startup and then hourly. |
| Background timeout | `-response-background-timeout` (`AIGATE_RESPONSE_BACKGROUND_TIMEOUT`, default `30m`): total run time of a `background:true` job; must be positive. |
| What is stored | Each row keeps the rewritten cumulative `items` snapshot (excluding `instructions`) and the response body. **Conversation content is user data** — configure the switch and retention to match your compliance requirements. |
| Expiry | Once a row is pruned its ID can no longer be used as `previous_response_id` (`404`); a `conversation` whose newest row it was falls back to the previous row. |

### Request log configuration

Request logging is a pluggable capability, **off by default**. The core only carries the
three "how to hand config to the plugin" settings below; the JSON content belongs to the
implementation (see [docs/request-log.md](request-log.md)).

| Flag | Environment variable | Default | Notes |
|------|----------------------|---------|-------|
| `-request-log` | `AIGATE_REQUEST_LOG` | `none` | Implementation name: `none` / `sqlite` / `es`. Unknown names are rejected at startup. |
| `-request-log-config` | `AIGATE_REQUEST_LOG_CONFIG` | (empty) | Inline JSON object passed verbatim to the implementation; a non-empty value must be a valid JSON object. |
| `-request-log-config-file` | `AIGATE_REQUEST_LOG_CONFIG_FILE` | (empty) | Path to a JSON config file; takes precedence over `-request-log-config`. Use it for ES credentials so they stay out of argv. |

The core storage schema is unchanged — **no SQL migration is needed**.
