# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **`auto` pseudo-model (cost-aware routing)** — `GET /v1/models` now lists an `auto`
  entry (owned_by `aigate`) when enabled, and `model: "auto"` on
  `/v1/chat/completions`, `/v1/messages` and `/v1/responses` is routed to the cheapest
  bound model that can handle the request. Candidates are the key's own bindings, using
  the same resolvability filter as `/v1/models` (a `user_key_required` binding with no
  key is excluded, so a routed request never fails for a reason the caller did not
  cause); decision-protocol models are always excluded. Short, single-turn requests that
  hit a local "this is simple" signal skip the classifier and take the cheapest priced
  candidate; everything else asks the configured decision model (a `choice` question
  built from the candidate list, described only by *relative* price tier, never absolute
  numbers). Every failure — no decision model, timeout, non-200, an unknown or
  out-of-pool choice — falls back to the key's primary model, so `auto` never turns a
  normal request into an error. A process-local circuit breaker (30s default cooldown)
  stops repeated timeouts. The response body still echoes `auto`; the model actually used
  is reported in the `X-AIGate-Routed-Model` header. If a key binds a real model named
  `auto`, that explicit binding wins over the pseudo-model. **Billing**: the decision
  call is recorded in usage detail but does *not* count toward quota — a new
  `llm_usage_logs.count_toward_quota` column (default `1`) excludes such rows from the
  global/user/key/group token sums and the cost sum, and the daily rollup is skipped for
  them, so `quota_day` is unaffected too. Existing rows default to `1`, so upgrading
  changes no current behavior. MySQL deployments need
  `sql/20261124-auto-routing.sql`.

- **Model routers (configurable routing rules)** — routing is now a first-class
  resource rather than a single site-wide switch. A new `model_routers` table holds one
  row per rule: a name, a **strategy**, strategy-private `config` JSON, a **scope**
  (`global` / `group` / `user` / `key`) plus scope id, a `priority`, and an enabled
  flag. Adding a strategy needs no schema change, no data-plane change and no page
  change: it is one Go type implementing the `Strategy` interface (`Name`, `Describe`,
  `ValidateConfig`, `Select`) registered in an `init`, and the admin API publishes each
  strategy's field list so the console renders its form from the server's descriptor.
  `decision` (the previous `auto` behavior) is the first strategy. A request is matched
  to exactly one rule: the most specific scope wins (`key` > `user` > `group` >
  `global`), and within one scope the lowest `priority` wins; duplicate
  `(scope, scope_id, priority)` is rejected at write time rather than resolving
  randomly at run time. Only requests naming `model: "auto"` are routed — a request
  naming a real model is untouched. The circuit breaker is per router, so one dead
  decision model does not stop the others. `llm_usage_logs` gains `router_id` so usage
  can be aggregated per rule, and the routed model and matched rule are returned in the
  `X-AIGate-Routed-Model` and `X-AIGate-Router-Id` response headers. Upgrading migrates
  an existing `settings.auto_router_*` configuration into one `global` `decision` rule
  named `auto` (only when it was enabled), so behavior is unchanged after the upgrade;
  the old settings columns are kept for one release as a rollback path and are no
  longer read. MySQL deployments need `sql/20261124-auto-routing.sql`.

- **Model routing admin API and console page** — seven admin endpoints manage rules:
  list, create, read, update, delete, plus `GET /admin/model-routers/strategies` and
  `POST /admin/model-routers/{id}/preview`. The strategies endpoint publishes each
  strategy's config field list (`string`/`int`/`bool`/`enum`/`model`, with defaults,
  ranges and required flags) so a client renders a strategy's form without hardcoding
  it; the preview endpoint runs a rule's strategy for a real key and reports the model
  and cause it would pick without sending anything upstream — including the case where
  it would fall back to the key's primary model. Structural validation (name, strategy,
  legal scope, scope id, unique priority) lives in the admin plane and config validation
  is delegated to the strategy, so a misconfigured rule is a `400` rather than a runtime
  surprise; the strategy normalizes `config` on write. The console gains a **Model
  routing** page (`/ui/model-routers`) with a rules table, an edit drawer whose strategy
  fields come from the server, and a preview panel. `aigatectl routers` is still to
  come.

- **Gemini upstream protocol** — `ai_models.model_type` gains `gemini-generate-content`,
  so a model can be backed by the Google Generative Language API
  (`https://generativelanguage.googleapis.com/v1beta`). All three chat ingresses reach it,
  cross-protocol translated as usual: text (single and multi-turn), tool calling
  (single-round and multi-round with `thoughtSignature` round-tripping), streaming, and
  images. Images become `inlineData` for base64 data URIs and `fileData` for `https` URLs
  with a known extension; the gateway never downloads an image itself, so the user-supplied
  URL is fetched by Google instead of becoming a new outbound request from the gateway.
  `thoughtSignature` rides on the returned tool-call `id` as `call_xxx_ts_<signature>` and is
  stripped back to the base id before the request reaches Gemini, with Gemini's
  `skip_thought_signature_validator` sentinel covering clients that do not echo the id.
  `reasoning_effort` maps to `thinkingBudget` on the 2.5 family (per-model ranges) and to
  `thinkingLevel` on Gemini 3 and later, because `thinkingBudget` was removed there; an
  unrecognised model name omits `thinkingConfig` rather than guessing a budget that would be
  rejected. `n > 1` returns 400, since only the first candidate would otherwise be returned.
  Known degradation: through `/v1/responses` the signature always falls back to the sentinel,
  because that ingress does not carry reasoning items.

- **Decision models (both protocols)** — the gateway now serves decision models, which
  answer typed questions with calibrated probabilities instead of text, on two new
  data-plane routes: `POST /v1/decisions` (OpenAI Decisions, aliases `/decisions`) and
  `POST /v1/systemone` (TypeSafe System One, alias `/systemone`). Two new
  `ai_models.model_type` values select the upstream format — `openai-decisions` and
  `systemone` — and either ingress reaches either upstream: same-protocol calls pass
  through, cross-protocol calls are translated through an internal representation
  (`predicate` <-> `noul`, `choices` <-> `criteria`, `levels` <-> `criteria` array,
  `box` <-> `box`, message arrays flattened into `state` with `\n\n`). Fields a target
  protocol cannot express (`assets`, `safety_identifier`, non-user input roles) are
  rejected with a 400 that names them rather than silently dropped. Neither route
  streams. The new `ai_models.billing_mode` column (`per_token` default, `input_only`
  for decision models) records decision usage as input-only, so usage rows carry
  `completion_tokens = 0` while responses keep the upstream `usage` verbatim. The admin
  API and console accept the new model types, `POST /admin/models/{id}/connectivity`
  probes a decision model with a yes/no question and replies `yes=<probability>`, and
  changing `model_type` across the chat/decision families is refused while the model is
  still bound to keys. MySQL deployments need
  `ALTER TABLE ai_models ADD COLUMN billing_mode VARCHAR(32) NOT NULL DEFAULT 'per_token' AFTER model_type;`

- **Build info everywhere** — a public, unauthenticated `GET /version` returns the running
  binary's `version`, `commit`, `build_time`, `dirty`, `go_version` and `started_at`, so an
  operator can confirm which code and commit a deployment actually serves. The value is
  assembled by the new `internal/version` package: `version` is injected at build time via
  `-ldflags` (Makefile and Docker `--build-arg VERSION`; the Makefile default is
  `git describe --tags --always --dirty`, so it is not strict SemVer), while `commit`/`build_time`
  fall back to the VCS metadata the Go toolchain stamps automatically, so even a bare
  `go build` reports a real commit. `dirty` is only meaningful for builds made from a
  working tree; Docker and CI builds copy a clean tree and always report `false`.
  `-version` now prints the same information,
  `aigatectl build-info` (alias `version --remote`, which prefixes a line naming the
  local binary and the gateway URL, written only after a successful response) queries
  the gateway, while bare `aigatectl version`
  keeps reporting the CLI binary itself, and the admin
  console shows the version and short commit at the bottom of the sidebar with the full
  details on hover. `/healthz` is unchanged.

- **`aigatectl` management CLI** — a second binary (`make build-cli` → `bin/aigatectl`)
  that exposes all 53 non-`/v1/*` endpoints (39 `/admin/*` + 12 `/api/user/*` +
  `GET /healthz` + `GET /version`) as discoverable commands, built on a reusable `internal/client` HTTP
  client and covered by end-to-end tests. Credentials are read only from the environment;
  see [docs/CLI.md](docs/CLI.md).
- **Model discovery** — `POST /admin/models/discover` lists the model ids an
  upstream exposes, using a stored credential or a one-off key. Outbound calls
  share the data-plane SSRF protection and reuse the connectivity `error_kind`
  enum. The admin console uses it to populate the model picker when a provider
  is added.
- **Admin Web UI** — a React SPA embedded in the same binary and served under
  `/ui/` (with `/` redirecting there). Covers models, credentials, keys, users,
  groups, usage (summary, request logs, CSV export), settings, webhooks and
  import; English and Simplified Chinese with instant switching. Built with
  pnpm + Vite and embedded via `go:embed` from `internal/webui`, so a plain
  `go build` still works without Node thanks to a committed `.gitkeep`
  placeholder. `make build` now builds the frontend first, the Dockerfile gains
  a Node stage, and CI adds a webui job with Vitest and Playwright.
- **Responses API ingress** — `POST /v1/responses` plus the lifecycle endpoints
  `GET /v1/responses/{id}`, `DELETE /v1/responses/{id}` and
  `POST /v1/responses/{id}/cancel`, authenticated by `sk-` data-plane keys.
  Streaming and non-streaming, with gateway-issued `resp_` IDs, Responses SSE
  events on the translation path, and a `response.deleted` reply for deletes.
- **`openai-responses` upstream protocol** — a new `ai_models.model_type` value
  alongside `openai-completions` and `anthropic-messages`, completing the 3×3
  ingress × upstream matrix: `/v1/responses` reaches every upstream type
  (passthrough for `openai-responses`, translation through chat completions
  otherwise), `/v1/chat/completions` and `/v1/messages` reach
  `openai-responses` upstreams through the provider adapter. All paths force
  the upstream `store:false`; the URL is joined as `<url>/responses`, so `url`
  must include `/v1` (unlike `anthropic-messages`, whose `url` is a bare host).
- **Stateful responses** — `store`, `previous_response_id`, `conversation` and
  `background` on `/v1/responses`, backed by a `response_states` table holding
  the cumulative item snapshot. **`store` defaults to `true`**, so a request
  that omits it is still persisted (7 days by default); `store:false` only
  stops the current turn from being written and does **not** prevent the
  gateway from reading a `previous_response_id`/`conversation` history.
  `background:true` returns a `queued` object immediately and runs the job
  asynchronously with cancel propagation to the upstream; rows abandoned by a
  restart are marked `failed` (`interrupted by restart`).
- **New configuration** — `-allow-response-storage` /
  `AIGATE_ALLOW_RESPONSE_STORAGE` (default `true`; `false` disables all stateful
  features and rejects an explicit `store:true` with `400` rather than silently
  rewriting it), `-response-retention-days` /
  `AIGATE_RESPONSE_RETENTION_DAYS` (default `7`, `0` = keep forever, `<= 36500`),
  and `-response-background-timeout` / `AIGATE_RESPONSE_BACKGROUND_TIMEOUT`
  (default `30m`, must be positive).

- **Cost billing** — the gateway can now account requests in **money** next to
  tokens. Each model carries append-only price snapshots with six raw unit
  prices (peak/off-peak × cache-hit input / cache-miss input / output, stored as
  integer micros per million tokens), written through the new
  `GET|POST /admin/models/{id}/prices` and `DELETE /admin/model-prices/{id}`;
  `ai_models` gains `currency`, `enable_peak_pricing` (default `false`) and
  `use_upstream_cost` (default `true`), and model responses echo them plus the
  currently effective `current_price`. Every usage row records two amounts
  frozen at accounting time — `cost_vendor_micros` in the model currency and
  `cost_platform_micros` in the new global `settings.platform_currency`
  (default `CNY`) — so later price, rate or currency changes never rewrite
  history; a request billed through a currency pair with no effective rate keeps
  its vendor amount and records `0` for the platform amount. Peak pricing is
  data, not code: each snapshot carries a `peak_hours` JSON schedule (IANA
  timezone, `HH:MM` windows on `time.Weekday` days, half-open,
  midnight-wrapping) and a new **billing calendar**
  (`GET|PUT /admin/billing-calendar`, `DELETE /admin/billing-calendar/{date}`)
  overrides the weekday rule with holidays and make-up working days.
  Off-peak is billed whenever `enable_peak_pricing` is `false`; an unevaluable
  schedule bills peak (fail-closed), and the admin plane rejects schedules that
  cannot be evaluated. FX rates are append-only snapshots too
  (`GET|POST /admin/currency-rates`, `DELETE /admin/currency-rates/{id}`,
  `rate_micros` meaning "1 unit `from` = `rate_micros`/1e6 units `to`", bounded
  to 1..1e9, same-currency pairs refused). Upstream-reported `usage.cost > 0`
  wins over computed prices when `use_upstream_cost` is on. Quota rules gain an
  optional `unit` (`token` default, or `cost` compared against settlement-currency
  micros, `limit` capped at 9e12), and requests that reach the upstream but fail
  (interrupted streams, upstream error bodies) are billed instead of being
  dropped. Admin writes invalidate the data plane's in-process caches
  immediately. The console gains the price editor, model pricing controls,
  `/billing-calendar`, `/currency-rates`, the settlement-currency setting, cost
  columns in usage and a spend card on the dashboard; `aigatectl` gains
  `prices`, `calendar` and `rates`.

### Added

- **Model capability declarations** — `ai_models` gains `context_window`,
  `max_output_tokens`, `supports_image` and `effort_levels` (also on
  `POST`/`PUT /admin/models` and in the model response), and `GET /v1/models`
  now includes all four per entry so a downstream agent can size its own
  requests: `context_window` and `max_output_tokens` as integers (or `null` when
  unconfigured), `supports_image` as a boolean, `effort_levels` as an ascending
  array (or `null`). They are **declarations only** — the gateway performs no
  logic on them: long prompts, image input and an unlisted `reasoning_effort`
  are all forwarded unchanged, and output length is still capped solely by the
  key's `max_tokens`. The model edit drawer in the WebUI edits them.
  `effort_levels` is validated (known levels, no duplicates) and stored in
  ascending ladder order. Upgrade path: `sql/20261124-model-capabilities.sql`.

### Changed

- **Pricing settings moved into the price drawer** — the model edit drawer no
  longer carries the pricing currency or the peak/upstream-cost switches; the
  price drawer now opens with a "Pricing settings" block that saves them back to
  the model (they stay model-level, not per price version) and then appends
  price snapshots in that currency. The provider list also shows each model's
  price status inline.
- **Usage reports carry amounts** — the usage summarize/log/export responses are
  extended, additively: summary rows gain `currency`, `cost_vendor_micros`,
  `cost_platform_micros` and, when a row spans currencies, `mixed_currency`
  (with `cost_vendor_micros` pinned to `0` outside `group_by=model`), the summary
  envelope gains `total_cost_platform_micros` and `platform_currency`, detail
  rows gain `currency`, `cost_vendor_micros` and `cost_platform_micros`, and the
  CSV export gains the two `cost_*_micros` columns before `created_at`. The
  user-plane `/api/user/quota` view gains `usages[].unit`. Quota rejections keep
  their status, message and `Retry-After`, but the error body now names the
  measurement with `unit` (`token` / `cost`) and, for cost rules, `currency`.
  Existing rows read back as `0`, which matches "billing starts when accounting
  starts", and rules without `unit` still mean tokens and are stored
  byte-identically.
- **Schema upgrade step** — `sql/20261123-cost-billing.sql` adds the three new
  tables (`ai_model_prices`, `billing_calendar`, `currency_rates`) and the new
  columns (`ai_models.currency` / `enable_peak_pricing` / `use_upstream_cost`,
  `llm_usage_logs.cost_vendor_micros` / `cost_platform_micros`,
  `daily_usage_summaries.cost_vendor_micros` / `cost_platform_micros` /
  `currency`, `settings.platform_currency`) for an **existing** MySQL database;
  `sql/init.sql` already contains all of it for fresh installs. The file is
  deliberately **not** idempotent — MySQL 8 has no `ADD COLUMN IF NOT EXISTS`,
  so re-running it fails loudly with `Duplicate column name` rather than
  silently skipping a missed change. SQLite deployments get everything from
  AutoMigrate at startup. Version bumped to `0.3.0`.

## [0.1.0] - 2026-10-02

First release: a single-binary, self-hostable LLM gateway with a data plane
(OpenAI-compatible + Anthropic Messages), an admin plane, a user-facing API, and
an outbound webhook event channel.

### Added

- **Core proxy** — `POST /v1/chat/completions` (streaming and non-streaming) and
  `GET /v1/models`, authenticated by `sk-` data-plane keys. Case-insensitive
  model resolution with primary fallback, forced replacement of the upstream
  model ID, `max_tokens` cap enforcement, and whitelisted custom-header
  passthrough. OpenAI-compatible and Anthropic Messages upstream adapters share
  a single request path. SSRF protection (URL validation, dial-time
  re-resolution, redirect validation) applies to every outbound request.
- **Anthropic ingress** — `POST /v1/messages` with `x-api-key` or Bearer
  authentication, native passthrough for Anthropic upstreams (`cache_control`
  preserved), and Anthropic → OpenAI request translation with response/SSE
  back-translation for OpenAI-compatible upstreams.
- **Quota engine** — rule-based token quotas over natural day/month/year and
  anchored custom windows with `429`/`Retry-After` rejection (configurable to
  `403`), including fail-closed handling of inactive windows and `limit=-1`.
  Checks run in four tiers — global → model daily → user → key — where the user
  tier resolves through the group policy → global default chain, the global tier
  supports group-level overrides (`groups.global_token_quota_rules`) with
  per-group accounting, and each key can carry its own `token_quota_rules`
  (unset or `[]` = no key-level limit).
- **Usage accounting and queries** — asynchronous per-request `llm_usage_logs`
  plus daily `daily_usage_summaries` (including cache read/write tokens),
  written through a bounded queue without blocking forwarding. Admin and
  user-facing query APIs (summary by date/model/key/user/group, paginated
  details), a streaming CSV/JSON export (`GET /admin/usage/export`), and a
  per-key daily trend endpoint (`GET /api/user/keys/{id}/usage`).
- **Key lifecycle** — optional per-key expiry (`expires_at`, RFC 3339; an
  explicit `null` clears it) with `expired_at` recording the first rejection:
  an expired key is rejected exactly like an invalid credential and emits
  `key.expired` once per validity window. Deleting a model promotes the affected
  keys' smallest-`sort_order` remaining binding to primary.
- **Bring your own key (BYOK)** — mark a model `user_key_required` and it stores
  no shared credential: each key binding carries its own upstream key (encrypted
  at rest, never echoed — only `has_key` is reported), resolved per request and
  isolated per binding; an unresolvable binding is rejected instead of silently
  falling back to the shared key. `/v1/models` skips bindings without a key.
- **Credential vault** — upstream credentials are managed separately from models
  and referenced by `credential_id`; secrets are masked on read and encrypted at
  rest with AES-256-GCM when a master key is configured (binding-level BYOK keys
  included); `-migrate-encrypt` re-seals existing plaintext rows.
- **Legacy import** — `POST /admin/import` ingests existing groups, users,
  models, and keys (with optional per-binding BYOK keys) in one idempotent,
  dry-run-capable call.
- **Connectivity testing** — `POST /admin/models/{id}/connectivity` probes an
  upstream with a minimal request and returns a structured error classification
  (BYOK models are reported as not probeable without a binding key).
- **Self-service keys** — users mint and rotate their own keys under
  `/api/user/keys`, bounded by a per-user limit and a model scope derived from
  their existing keys; per-key quotas are administrator-managed and read-only on
  the user plane.
- **Webhook event egress** — `/admin/webhooks` CRUD with write-only secrets
  (read-side masking), http/https URL validation, and `event_types` filtering.
  Asynchronous delivery with HMAC-SHA256 signatures (`X-Gateway-Signature`),
  10-second per-attempt timeout, up to 3 attempts with 1s/5s retry waits, and
  terminal `success`/`failed` status; delivery records are queryable via
  `GET /admin/webhooks/{id}/deliveries` and pruned after
  `-webhook-delivery-retention-days` (default 7, `0` = keep forever). Events:
  `request.completed`, `quota.exceeded` (`kind`: `global` / `model` / `user` /
  `key`), `key.disabled`, `key.expired`.
- **Admin and user planes** — Bearer-only authentication on `/admin/*` and
  `/api/user/*` (no frontend); users, groups, models, credentials, keys,
  settings, webhooks, import, and usage queries on the admin side; identity,
  quota view, usage queries, and self-service keys with row-level isolation on
  the user side.
- **Deployment** — single binary with SQLite for local evaluation and MySQL for
  production; explicit schema management (`sql/init.sql` as the single source of
  truth; in-place upgrades via `sql/YYYYMMDD-<slug>.sql`); multi-stage
  `Dockerfile`; graceful shutdown draining the usage and webhook queues;
  `GET /healthz`; `-version`.
