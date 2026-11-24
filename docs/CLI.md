# CLI Reference (aigatectl)

[English](CLI.md) | [简体中文](CLI.zh-CN.md)

`aigatectl` is the AIGate management CLI. It turns the **62 endpoints** of the admin
plane (`/admin/*`), the user plane (`/api/user/*`), `GET /healthz` and `GET /version` into 62 stable
commands: one command per endpoint, no workflow orchestration, and no wrapping of the
data-plane `/v1/*` inference endpoints. Response bodies are written to stdout verbatim,
diagnostics go only to stderr, and exit codes are fixed so shell scripts, `make`, and CI
can consume the tool directly. The server is unchanged: AIGate remains a pure API service.

## 1. Build and install

Requires Go 1.24+; no new dependencies.

```bash
make build-cli   # produces bin/aigatectl
```

Usage form:

```
aigatectl [global flags] <resource> <command> [flags]
```

`--help` / `-h` and `version` are local commands that issue no HTTP request;
`aigatectl version` prints `aigatectl <version> (local binary)` and exits 0.

`aigatectl` is a **separate binary** from the gateway, so the two versions are distinct:

| You want | Command | Reports |
|---|---|---|
| The CLI binary's own version | `aigatectl version` | This `aigatectl` binary (local, `(local binary)` marker) |
| The gateway's version / commit | `aigatectl build-info` | The running gateway via `GET /version` |
| The gateway's version / commit | `aigatectl version --remote` | Same request and output as `build-info`, prefixed with one line naming this local binary and the gateway URL it queried (the header goes to stdout only when the request succeeds; a failure leaves stdout empty and reports on stderr with exit code 5) |

## 2. Configuration and credentials

`--url` precedence: flag > the `AIGATE_URL` environment variable > the default
`http://127.0.0.1:8080`. A trailing slash on the root address is normalized; an invalid
URL is rejected locally with exit code 2.

| Global flag | Environment variable | Default | Purpose |
|---|---|---|---|
| `--url <url>` | `AIGATE_URL` | `http://127.0.0.1:8080` | Gateway root address |
| `--timeout <duration>` | — | `30s` | Whole-request timeout (`time.Duration` text); raise it for `usage export` and `models connectivity` |
| `--data <json\|@file\|->` | — | (none) | Request body; see §6 |
| `--id <positive int>` | — | (none) | Path parameter; must be a positive integer except on `calendar delete`, where it is the `YYYY-MM-DD` date |

**Credentials are read only from the environment.** The CLI offers no `--token`-style
flag, and tokens never enter argv, `ps` output, or logs.

| Environment variable | Applies to | Purpose |
|---|---|---|
| `AIGATE_ADMIN_TOKEN` | `/admin/*` | Admin access token for the admin plane |
| `AIGATE_USER_TOKEN` | `/api/user/*` | User-plane access token; falls back to `AIGATE_ADMIN_TOKEN` when unset |
| (none) | `/healthz` | The health check needs no credentials |
| (none) | `/version` | Build info needs no credentials either |

The token is selected automatically from the command's resource group — the user does not
switch it. If the required token is missing, local validation fails with exit code 2 and
no request is sent.

### Recursive help

```bash
aigatectl --help                       # top level: global flags and resource list
aigatectl models --help                # resource level: commands under models
aigatectl models create --help         # command level: that command's Usage / parameters
aigatectl self --help                  # commands under self (including subgroups usage / keys)
aigatectl self keys --help             # subgroup: list/get/create/update/delete/rotate/usage
```

`--help` may also be written `-h`. The two `self` subgroups `usage` and `keys` take one
more action token, so those command names have three tokens (e.g. `self usage summary`,
`self keys list`).

## 3. Command reference (62 commands)

Each row gives two equivalent notations: a backticked `METHOD /path` endpoint token, and —
after the arrow — the equivalent request-line form with the method plus a backticked path.
The command table is kept in sync with the 62 endpoints of `docs/openapi.yaml` (no `/v1/*`).
There is no `GET /admin/users/{id}`, so no `users get` command exists.

| # | Command | Method · path | Example |
|---|---|---|---|
| 1 | `models list` | `GET /admin/models` → GET `/admin/models` | `aigatectl models list` |
| 2 | `models get` | `GET /admin/models/{id}` → GET `/admin/models/{id}` | `aigatectl models get --id 1` |
| 3 | `models create` | `POST /admin/models` → POST `/admin/models` | `aigatectl models create --data @model.json` |
| 4 | `models update` | `PUT /admin/models/{id}` → PUT `/admin/models/{id}` | `aigatectl models update --id 1 --data @model.json` |
| 5 | `models delete` | `DELETE /admin/models/{id}` → DELETE `/admin/models/{id}` | `aigatectl models delete --id 1` |
| 6 | `models connectivity` | `POST /admin/models/{id}/connectivity` → POST `/admin/models/{id}/connectivity` | `aigatectl models connectivity --id 1` |
| 7 | `credentials list` | `GET /admin/credentials` → GET `/admin/credentials` | `aigatectl credentials list` |
| 8 | `credentials get` | `GET /admin/credentials/{id}` → GET `/admin/credentials/{id}` | `aigatectl credentials get --id 1` |
| 9 | `credentials create` | `POST /admin/credentials` → POST `/admin/credentials` | `aigatectl credentials create --data @credential.json` |
| 10 | `credentials update` | `PUT /admin/credentials/{id}` → PUT `/admin/credentials/{id}` | `aigatectl credentials update --id 1 --data @credential.json` |
| 11 | `credentials delete` | `DELETE /admin/credentials/{id}` → DELETE `/admin/credentials/{id}` | `aigatectl credentials delete --id 1` |
| 12 | `keys list` | `GET /admin/keys` → GET `/admin/keys` | `aigatectl keys list` |
| 13 | `keys get` | `GET /admin/keys/{id}` → GET `/admin/keys/{id}` | `aigatectl keys get --id 1` |
| 14 | `keys create` | `POST /admin/keys` → POST `/admin/keys` | `aigatectl keys create --data @key.json` |
| 15 | `keys update` | `PUT /admin/keys/{id}` → PUT `/admin/keys/{id}` | `aigatectl keys update --id 1 --data @key.json` |
| 16 | `keys delete` | `DELETE /admin/keys/{id}` → DELETE `/admin/keys/{id}` | `aigatectl keys delete --id 1` |
| 17 | `keys rotate` | `POST /admin/keys/{id}/rotate` → POST `/admin/keys/{id}/rotate` | `aigatectl keys rotate --id 1` |
| 18 | `users list` | `GET /admin/users` → GET `/admin/users` | `aigatectl users list` |
| 19 | `users create` | `POST /admin/users` → POST `/admin/users` | `aigatectl users create --data @user.json` |
| 20 | `users update` | `PUT /admin/users/{id}` → PUT `/admin/users/{id}` | `aigatectl users update --id 1 --data @user.json` |
| 21 | `users delete` | `DELETE /admin/users/{id}` → DELETE `/admin/users/{id}` | `aigatectl users delete --id 1` |
| 22 | `users rotate` | `POST /admin/users/{id}/rotate` → POST `/admin/users/{id}/rotate` | `aigatectl users rotate --id 1` |
| 23 | `groups list` | `GET /admin/user-groups` → GET `/admin/user-groups` | `aigatectl groups list` |
| 24 | `groups get` | `GET /admin/user-groups/{id}` → GET `/admin/user-groups/{id}` | `aigatectl groups get --id 1` |
| 25 | `groups create` | `POST /admin/user-groups` → POST `/admin/user-groups` | `aigatectl groups create --data @group.json` |
| 26 | `groups update` | `PUT /admin/user-groups/{id}` → PUT `/admin/user-groups/{id}` | `aigatectl groups update --id 1 --data @group.json` |
| 27 | `groups delete` | `DELETE /admin/user-groups/{id}` → DELETE `/admin/user-groups/{id}` | `aigatectl groups delete --id 1` |
| 28 | `usage summary` | `GET /admin/usage/summary` → GET `/admin/usage/summary` | `aigatectl usage summary --group-by date` |
| 29 | `usage logs` | `GET /admin/usage/logs` → GET `/admin/usage/logs` | `aigatectl usage logs --page 1 --page-size 20` |
| 30 | `usage export` | `GET /admin/usage/export` → GET `/admin/usage/export` | `aigatectl usage export --format csv` |
| 31 | `settings get` | `GET /admin/settings` → GET `/admin/settings` | `aigatectl settings get` |
| 32 | `settings update` | `PUT /admin/settings` → PUT `/admin/settings` | `aigatectl settings update --data @settings.json` |
| 33 | `webhooks list` | `GET /admin/webhooks` → GET `/admin/webhooks` | `aigatectl webhooks list` |
| 34 | `webhooks get` | `GET /admin/webhooks/{id}` → GET `/admin/webhooks/{id}` | `aigatectl webhooks get --id 1` |
| 35 | `webhooks create` | `POST /admin/webhooks` → POST `/admin/webhooks` | `aigatectl webhooks create --data @webhook.json` |
| 36 | `webhooks update` | `PUT /admin/webhooks/{id}` → PUT `/admin/webhooks/{id}` | `aigatectl webhooks update --id 1 --data @webhook.json` |
| 37 | `webhooks delete` | `DELETE /admin/webhooks/{id}` → DELETE `/admin/webhooks/{id}` | `aigatectl webhooks delete --id 1` |
| 38 | `webhooks deliveries` | `GET /admin/webhooks/{id}/deliveries` → GET `/admin/webhooks/{id}/deliveries` | `aigatectl webhooks deliveries --id 1 --page 1` |
| 39 | `import run` | `POST /admin/import` → POST `/admin/import` | `aigatectl import run --data @import.json` |
| 40 | `prices list` | `GET /admin/models/{id}/prices` → GET `/admin/models/{id}/prices` | `aigatectl prices list --id 1` |
| 41 | `prices create` | `POST /admin/models/{id}/prices` → POST `/admin/models/{id}/prices` | `aigatectl prices create --id 1 --data @price.json` |
| 42 | `prices delete` | `DELETE /admin/model-prices/{id}` → DELETE `/admin/model-prices/{id}` | `aigatectl prices delete --id 7` |
| 43 | `calendar list` | `GET /admin/billing-calendar` → GET `/admin/billing-calendar` | `aigatectl calendar list --year 2026` |
| 44 | `calendar set` | `PUT /admin/billing-calendar` → PUT `/admin/billing-calendar` | `aigatectl calendar set --data @days.json` |
| 45 | `calendar delete` | `DELETE /admin/billing-calendar/{date}` → DELETE `/admin/billing-calendar/{date}` | `aigatectl calendar delete --id 2026-10-01` |
| 46 | `rates list` | `GET /admin/currency-rates` → GET `/admin/currency-rates` | `aigatectl rates list --from USD --to CNY` |
| 47 | `rates create` | `POST /admin/currency-rates` → POST `/admin/currency-rates` | `aigatectl rates create --data @rate.json` |
| 48 | `rates delete` | `DELETE /admin/currency-rates/{id}` → DELETE `/admin/currency-rates/{id}` | `aigatectl rates delete --id 3` |
| 49 | `self me` | `GET /api/user/me` → GET `/api/user/me` | `aigatectl self me` |
| 50 | `self quota` | `GET /api/user/quota` → GET `/api/user/quota` | `aigatectl self quota` |
| 51 | `self usage summary` | `GET /api/user/usage/summary` → GET `/api/user/usage/summary` | `aigatectl self usage summary` |
| 52 | `self usage logs` | `GET /api/user/usage/logs` → GET `/api/user/usage/logs` | `aigatectl self usage logs` |
| 53 | `self usage by-model` | `GET /api/user/usage/by-model` → GET `/api/user/usage/by-model` | `aigatectl self usage by-model` |
| 54 | `self keys list` | `GET /api/user/keys` → GET `/api/user/keys` | `aigatectl self keys list` |
| 55 | `self keys get` | `GET /api/user/keys/{id}` → GET `/api/user/keys/{id}` | `aigatectl self keys get --id 1` |
| 56 | `self keys create` | `POST /api/user/keys` → POST `/api/user/keys` | `aigatectl self keys create --data @key.json` |
| 57 | `self keys update` | `PUT /api/user/keys/{id}` → PUT `/api/user/keys/{id}` | `aigatectl self keys update --id 1 --data @key.json` |
| 58 | `self keys delete` | `DELETE /api/user/keys/{id}` → DELETE `/api/user/keys/{id}` | `aigatectl self keys delete --id 1` |
| 59 | `self keys rotate` | `POST /api/user/keys/{id}/rotate` → POST `/api/user/keys/{id}/rotate` | `aigatectl self keys rotate --id 1` |
| 60 | `self keys usage` | `GET /api/user/keys/{id}/usage` → GET `/api/user/keys/{id}/usage` | `aigatectl self keys usage --id 1` |
| 61 | `health` | `GET /healthz` → GET `/healthz` | `aigatectl health` |
| 62 | `build-info` | `GET /version` → GET `/version` | `aigatectl build-info` |
| 63 | `routers list` | `GET /admin/model-routers` → GET `/admin/model-routers` | `aigatectl routers list` |
| 64 | `routers strategies` | `GET /admin/model-routers/strategies` → GET `/admin/model-routers/strategies` | `aigatectl routers strategies` |
| 65 | `routers get` | `GET /admin/model-routers/{id}` → GET `/admin/model-routers/{id}` | `aigatectl routers get --id 1` |
| 66 | `routers create` | `POST /admin/model-routers` → POST `/admin/model-routers` | `aigatectl routers create --data @router.json` |
| 67 | `routers update` | `PUT /admin/model-routers/{id}` → PUT `/admin/model-routers/{id}` | `aigatectl routers update --id 1 --data @router.json` |
| 68 | `routers delete` | `DELETE /admin/model-routers/{id}` → DELETE `/admin/model-routers/{id}` | `aigatectl routers delete --id 1` |
| 69 | `routers preview` | `POST /admin/model-routers/{id}/preview` → POST `/admin/model-routers/{id}/preview` | `aigatectl routers preview --id 1 --data @preview.json` |

`health` is a single-token command (`aigatectl health`) and needs no credentials; every
other command uses two tokens `<resource> <command>` (three for `self` subgroups). The
local commands `version` and `--help` are not counted among these 62 HTTP commands.

## 4. Query flags

Only the following 17 commands accept query flags; parameters not declared in the contract
are never exposed. Flag names use hyphens and are converted to underscores when sent
(e.g. `--group-by` → `group_by`); values are passed through without business validation.

| CLI command | Method · path | Allowed query flags |
|---|---|---|
| `usage export` | `GET /admin/usage/export` | `--format`, `--start`, `--end`, `--user-id`, `--group-id`, `--api-key-id`, `--ai-model-id` |
| `usage logs` | `GET /admin/usage/logs` | `--start`, `--end`, `--user-id`, `--group-id`, `--api-key-id`, `--ai-model-id`, `--page`, `--page-size` |
| `usage summary` | `GET /admin/usage/summary` | `--group-by`, `--start`, `--end`, `--user-id`, `--group-id`, `--api-key-id`, `--ai-model-id` |
| `webhooks deliveries` | `GET /admin/webhooks/{id}/deliveries` | `--page`, `--page-size` |
| `calendar list` | `GET /admin/billing-calendar` | `--year` |
| `rates list` | `GET /admin/currency-rates` | `--from`, `--to` |
| `self quota` | `GET /api/user/quota` | `--user-id` |
| `self usage by-model` | `GET /api/user/usage/by-model` | `--start`, `--end`, `--user-id`, `--group-id`, `--api-key-id`, `--ai-model-id` |
| `self usage logs` | `GET /api/user/usage/logs` | `--start`, `--end`, `--user-id`, `--group-id`, `--api-key-id`, `--ai-model-id`, `--page`, `--page-size` |
| `self usage summary` | `GET /api/user/usage/summary` | `--group-by`, `--start`, `--end`, `--user-id`, `--group-id`, `--api-key-id`, `--ai-model-id` |
| `self keys list` | `GET /api/user/keys` | `--user-id` |
| `self keys get` | `GET /api/user/keys/{id}` | `--user-id` |
| `self keys create` | `POST /api/user/keys` | `--user-id` |
| `self keys update` | `PUT /api/user/keys/{id}` | `--user-id` |
| `self keys delete` | `DELETE /api/user/keys/{id}` | `--user-id` |
| `self keys rotate` | `POST /api/user/keys/{id}/rotate` | `--user-id` |
| `self keys usage` | `GET /api/user/keys/{id}/usage` | `--start`, `--end`, `--user-id` |

Key points:

- `--group-by` appears only on `usage summary` and `self usage summary`.
- `--page` / `--page-size` appear only on `usage logs`, `self usage logs`, and
  `webhooks deliveries`.
- `--format` appears only on `usage export`.
- `--year` (a 4-digit year; omitted means the current business year) appears only on
  `calendar list`; `--from` / `--to` (ISO 4217 codes) appear only on `rates list` and are
  required by the server — omitting one is reported by the gateway as a 400.
- Every `self keys *` command accepts `--user-id`; `self keys usage` accepts only
  `--start` / `--end` / `--user-id` in addition, and does not expose `--group-id`,
  `--api-key-id`, or `--ai-model-id`.
- All other commands accept no query flags; their argv may be empty or hold only the
  command's own arguments.

## 5. Exit codes

| Code | Meaning | Trigger |
|---|---|---|
| `0` | Success | HTTP 2xx (including `models connectivity` with `ok:false` and a partial `import run` failure); `--help` and `version` also return 0 |
| `2` | Usage / local error | Flag parse failure, unknown command, invalid `--id` / `--url`, missing token, `--data` file-read or JSON parse failure; `calendar delete` rejects an empty `--id`, one containing `/`, `?`, `#`, or the segments `.` / `..` |
| `3` | Authentication / authorization failure | Remote 401 / 403 |
| `4` | Remote business error | Any other non-2xx (400/404/409/413/429/5xx, etc.) |
| `5` | Transport error | DNS / connect / TLS / timeout / response-body read failure |

stdout carries only the remote response body; on a non-2xx response the error body's
`message` (or the raw body) is written to stderr.

## 6. Request bodies and `--data` with sensitive fields

`--data` accepts three forms: a JSON literal, `@file` (read a file), or `-` (read stdin).
All three are validated as JSON locally but are **forwarded byte-for-byte, without
re-serialization**; commands with a body automatically set
`Content-Type: application/json`.

```bash
aigatectl groups create --data '{"name":"demo"}'              # literal is fine without sensitive fields
aigatectl credentials create --data @credential.json           # preferred: read from a file
cat credential.json | aigatectl credentials create --data -     # or read from stdin
```

> **Security warning**: a `--data` JSON literal appears in argv and can be seen by `ps`,
> shell history, or process listings. **Bodies containing sensitive fields — upstream
> `api_key`, `secret`, binding-level `api_key`, or plaintext `token` values in import
> data — must use `--data @file` or `--data -`; never put them in argv.** The CLI does not
> inspect body contents, so choosing file/stdin is the caller's responsibility. Likewise,
> credentials come only from the environment; there is no `--token` flag.

## 7. Special semantics

- `models connectivity` always returns HTTP 200; success or failure is carried in the
  body's `ok`. The CLI judges by HTTP status, still exits 0, prints the body verbatim, and
  leaves the `ok` check to the caller.
- `import run` still returns 200 when individual entries fail
  (`details[].action=error`); likewise judged by HTTP status with exit 0.
- `usage export --format csv` returns a `text/csv` stream that the CLI prints verbatim
  without JSON decoding.
- `DELETE` commands return `{"ok":true}`, printed verbatim.
- The `token` in `create` / `rotate` responses is a one-time plaintext value: it appears
  on stdout exactly once, is never logged, cached, or written to disk, and no list or
  detail endpoint ever echoes it again.
- There is no `GET /admin/users/{id}`, so no `users get` command is provided.
- `health` maps to `GET /healthz` and needs no credentials; `version` is a local command
  that issues no request.
- `build-info` maps to `GET /version` and needs no credentials. It prints the **gateway's**
  build info (`version`, `commit`, `build_time`, `dirty`, `go_version`,
  `started_at`) verbatim, so `aigatectl build-info | jq .commit` works in scripts.
  `version --remote` is an alias: same request and same JSON, preceded by one stdout line
  naming this local binary and the gateway URL, so the two binaries are never confused.
  That header is written only after a successful response — when the gateway is
  unreachable or returns non-2xx, stdout stays empty (or carries only the verbatim error
  body rules above) and the failure is reported on stderr, so piping to `jq` never sees a
  stray header line.
- `prices list` / `prices create` take the **model** id, while `prices delete` takes the
  **price-row** id (`/admin/model-prices/{id}`); the two are not interchangeable.
- `calendar delete --id` is the only command whose `--id` is not a positive integer: it is
  a `YYYY-MM-DD` date string sent verbatim as a single path segment. Every other command
  still rejects a non-integer `--id` locally before dispatch.
- `rates list` requires both `--from` and `--to`; the CLI forwards whatever is given and
  the gateway validates the currency codes (`400` on a missing or malformed pair).
- 2xx response bodies are written to stdout verbatim (no parsing, reordering, or
  decoration); diagnostics for non-2xx go only to stderr.
