# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

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
