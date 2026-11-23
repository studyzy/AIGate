# Tencent Cloud AIGate end-to-end smoke test

`test/e2e/` is an end-to-end smoke suite independent from the unit tests: it drives the **real gateway
binary** and the **real `aigatectl` CLI binary** as black boxes, starts in-process mock upstreams
(OpenAI and Anthropic protocols), and connects directly to the database (MySQL or SQLite) to assert
that usage is persisted. It **does not import the `internal` package**.

## Entry points (read this before editing a test)

| Surface | How the suite drives it | Why |
|---------|-------------------------|-----|
| Admin `/admin/*` writes | `aigatectl` (`adminCLI`, `createModel`, `createKey`, `createUser`, …) | every test also exercises the CLI that operators use |
| User `/api/user/*` writes | `aigatectl self` (`userCLI`, `m3bCreateSelfKey`) | same reason, user token plane |
| Data plane `/v1/*` | raw HTTP (`doRequest`, `postChatCompletion`, `postAnthropicMessage`) | no CLI command exists; this is the gateway's own forwarding surface |
| Queries and deliberate failures | raw HTTP | `aigatectl` only surfaces an exit code (0/3/4/5), which would lose the status code and error body the assertion is about |

Anything that needs a CLI-only assertion uses `cliOK` / `cliFailure` (exit code + stdout/stderr).
The exit-code contract is `0` = 2xx, `3` = 401/403, `4` = any other remote error, `5` = transport
error; see `cliExitOK` … `cliExitTransport` in `helpers_test.go`.

## Layout

| File | Contents |
|------|----------|
| `main_test.go` | `TestMain`: removes the SQLite test database after the run |
| `run.sh` | environment lifecycle (database, binaries, cleanup) |
| `helpers_test.go` | shared constants, HTTP primitives, CLI primitives, DB assertions |
| `gateway_test.go` | starts/stops the real gateway process on a random port |
| `upstream_test.go` | OpenAI / Anthropic mock upstreams and their fixed bodies |
| `admin_test.go` | `TestE2E_Admin_*` — models, credentials, import, key TTL, quotas |
| `user_test.go` | `TestE2E_User_*` — self-service keys, BYOK |
| `dataplane_test.go` | `TestE2E_DataPlane_*` — forwarding, streaming, Anthropic ingress |
| `webhook_test.go` | `TestE2E_Webhook_*` — subscriptions, signed delivery, events |
| `webui_test.go` | `TestE2E_WebUI_*` — static hosting contract for `/ui/` |
| `cli_test.go` | `TestE2E_CLI_*` — CLI closed loop and exit-code contract |
| `version_test.go` | `TestE2E_Version` / `TestE2E_CLI_VersionCommands` — public `GET /version` and the gateway-vs-CLI version distinction |

## How to run

```bash
make test-e2e          # same as make test-e2e-sqlite: no external database, no mysql client needed
make test-e2e-mysql    # starts its own MySQL container (docker + the mysql client required)
```

Both targets call the script below with `AIGATE_E2E_DB_TYPE` set accordingly; the script can also be
invoked directly:

```bash
bash test/e2e/run.sh                            # default: MySQL
AIGATE_E2E_DB_TYPE=sqlite bash test/e2e/run.sh
```

The script owns the whole environment lifecycle, so every run starts clean and leaves nothing behind:

- SQLite: delete and recreate the database file (`./.e2e/aigate_e2e.db` by default) and wait until it
  is writable. The gateway creates the tables via `AutoMigrate` on startup, so `sql/init.sql` is not
  involved. The file is deleted again after the run.
- MySQL: remove any leftover container with the same name, start a fresh container
  (`mysql:8.0` by default, `AIGATE_E2E_MYSQL_CONTAINER` / `AIGATE_E2E_MYSQL_IMAGE`), wait until it
  accepts connections (up to 180s), then reset the database `aigate_e2e` and import `sql/init.sql`.
  The container is force-removed (with its data) when the run ends, whether it passed or failed.
  The root password is never empty: it defaults to `AIGate-e2e` and can be overridden with
  `AIGATE_E2E_MYSQL_PASSWORD`. The script passes it via `MYSQL_PWD` and the DSN, so it never shows up
  in command-line arguments; the logged DSN shows only the user.
  Set `AIGATE_E2E_MYSQL_USE_EXISTING=1` to connect to an already running container instead of
  starting one — e.g. a GitHub Actions `services` container, or a MySQL you already have. The
  script then only creates and imports the database and never stops or removes the container.

Two MySQL paths are covered: `run.sh` imports `sql/init.sql` into a freshly reset `aigate_e2e`,
and `mysql_fresh_init_test.go` then creates a *separate* empty database, applies `sql/init.sql`
to it and drives real traffic through the gateway, so a fresh install (the only path production
uses — MySQL never auto-migrates) is verified end to end. `schema_parity_internal_test.go` in
`internal/model` guards the model/`init.sql` column lists at unit-test time.

Then, for both: `go build -o bin/aigate ./cmd/aigate` -> `go build -o bin/aigatectl ./cmd/aigatectl`
-> export environment variables -> `go test -tags e2e -count=1 -v ./test/e2e/`.

**Gateway ports.** Every test case starts its gateway on a random free port taken from the pool
`20000-60999` (`-addr` plus an `AIGATE_ADDR` env override, so an exported `AIGATE_ADDR` cannot pull it
back to a default port). The default `:8080` is never used, so a locally running AIGate service does
not interfere.

**Process cleanup.** Each test case stops its own gateway in `t.Cleanup` (SIGTERM, then SIGKILL after
10s, with a further 5s cap on `Wait`). As a backstop, the script's exit trap SIGTERMs any leftover
`bin/aigate` process whose environment carries the test DSN in `AIGATE_DB_DSN` — it matches on that
marker, so an unrelated gateway (e.g. the local systemd service) is never touched.

To only re-run the test cases (database and binaries already in place):

```bash
AIGATE_E2E_BIN="$(pwd)/bin/aigate" \
AIGATE_E2E_CLI_BIN="$(pwd)/bin/aigatectl" \
AIGATE_E2E_DB_TYPE=mysql \
AIGATE_E2E_DSN='root:AIGate-e2e@tcp(127.0.0.1:3306)/aigate_e2e?charset=utf8mb4&parseTime=True&loc=Local' \
go test -tags e2e -count=1 -v ./test/e2e/
```

Four environment variables point the suite at the built artifacts and the database:

| Variable | Meaning |
|----------|---------|
| `AIGATE_E2E_BIN` | Path to the gateway binary (`bin/aigate`) |
| `AIGATE_E2E_CLI_BIN` | Path to the management CLI binary (`bin/aigatectl`), used by `cli_test.go` |
| `AIGATE_E2E_DSN` | DSN of the test database (MySQL DSN, or a SQLite file path) |
| `AIGATE_E2E_DB_TYPE` | `mysql` (default when unset) or `sqlite`; passed to the gateway as `-db-type` |

By convention, if any of `AIGATE_E2E_BIN`, `AIGATE_E2E_CLI_BIN` or `AIGATE_E2E_DSN` is missing,
the test cases call `t.Skip` (they do not fail), so the suite also compiles and runs cleanly in an
environment without a database. Any test that drives the CLI checks `AIGATE_E2E_CLI_BIN` via `requireEnv`.

## Prerequisites

- MySQL mode: `docker` is available and the `mysql` client image is pullable (the script starts the
  container itself). The host port defaults to `127.0.0.1:3306` and must be free; override with
  `MYSQL_HOST` / `MYSQL_PORT`, or `AIGATE_E2E_MYSQL_CONTAINER` /
  `AIGATE_E2E_MYSQL_IMAGE` / `AIGATE_E2E_MYSQL_PASSWORD`.
  With `AIGATE_E2E_MYSQL_USE_EXISTING=1` a `docker` CLI on the host is still required (the script
  reaches the database through `docker exec`), but no container is created or removed.
- MySQL mode: nothing else — the image's own `mysql` client inside the container creates the database
  and imports the schema via `docker exec`; nothing is installed on the host.
- MySQL mode: `sql/init.sql` exists at the repository root (the source of truth for the schema).
- SQLite mode: only the Go toolchain — the database file is created by the script itself.
- The Go toolchain is available (used to build the binary and to run the tests).

## Covered groups

`e2e_test.go` and `cli_test.go` contain the groups below; run them all with
`go test -tags e2e -count=1 -v ./test/e2e/`. Each group starts its own gateway
process against the shared test database; `cli_test.go` additionally drives the real
`bin/aigatectl` binary against that gateway.

| Group | Scope | Key assertions |
|-------|-------|----------------|
| `TestE2E_M1_ForwardingSmoke` | Model/key creation, `/v1/models`, non-streaming and streaming forwarding, the Anthropic path, auth failures | 201/200 statuses; `sk-` token shape; plaintext never echoed; the upstream receives the resolved model and (Anthropic) the native body; usage lands in `llm_usage_logs` / `daily_usage_summaries` |
| `TestE2E_M2_QuotaAndUsage` | Limited (day=5) vs explicitly unlimited groups; admin and user usage queries | The first request is allowed and the next one returns `429` + `Retry-After`; the unlimited group is unaffected; per-user row-level isolation on the user plane |
| `TestE2E_M3_AnthropicIngress` | `/v1/messages` conversion (Anthropic to/from OpenAI) and native passthrough | Stream and non-stream event sequences; `cache_control` preserved on the passthrough path; persisted usage |
| `TestE2E_M3b_GatewayOps` | Credentials (ciphertext at rest), import dry-run/idempotency, connectivity probes, self-service keys, key TTL lifecycle, model-deletion fallback promotion | `enc:v1:` ciphertext in the database; idempotent import; an expired key is rejected and records `expired_at`; the promoted fallback binding serves the next request |
| `TestE2E_M4_Webhooks` | Subscription CRUD, signed delivery, event coverage, delivery records | The HMAC signature verifies; `request.completed` / `quota.exceeded` / `key.disabled` / `key.expired` are delivered; `key.expired` fires once per validity window; `GET .../deliveries` paginates |
| `TestE2E_CLI_ClosedLoop` (`cli_test.go`) | Drive the real `aigatectl` binary through the 8-step closed loop: `health` → `models create` → `models list`/`get` → `users create` → `keys create` → data-plane forwarding → `self me`/`self keys list` → `keys rotate` | Every step exits 0 with JSON on stdout and empty stderr; the created model row exists in `ai_models` (direct database query); the `sk-` token issued by the CLI really drives `/v1/chat/completions` and its usage lands in `llm_usage_logs`; the rotated-away token is rejected with 401 |
| `TestE2E_CLI_ExitCodes` (`cli_test.go`) | One case per CLI exit code: 0 / 2 / 3 / 4 / 5 | Errors appear only on stderr and stdout is empty |
| `TestE2E_CLI_SpecialSemantics` (`cli_test.go`) | `models connectivity` (`ok:false` but HTTP 200), `import run` (partial failure but HTTP 200), `usage export --format csv` | All still exit 0 with the body verbatim; CSV is raw text and is never JSON-decoded |

## Troubleshooting

- **Port already in use**: the gateway listen port is a free loopback port allocated dynamically by
  the test at runtime, it is never hard-coded; if the process under test leaves something behind,
  clean up the leftover `gateway` process and re-run. The subprocess receives `SIGTERM` in
  `t.Cleanup` and exits gracefully.
- **MySQL not running / cannot connect**: run `make test-e2e-mysql` (or `run.sh` directly) rather than
  `go test` on its own — the script starts the container, waits for readiness and recreates the
  database. If the container never becomes ready the script prints its status and the last 50 log
  lines. A port conflict on `MYSQL_PORT` (3306 by default) is the most common cause.
- **healthz timeout**: the test case prints the gateway subprocess stdout/stderr as well, so check
  that log first:
  - invalid configuration (e.g. a wrong DSN / timezone) is rejected at startup and makes the process exit;
  - if `sql/init.sql` was not imported or the database name does not match, connecting to the database fails;
  - when the binary and the `sql/init.sql` schema are out of sync, table creation / migration errors also show up here.
- **Admin API 401/403**: make sure `-init-admin-token` matches `e2e-admin-token` used by the test
  cases, and that the first startup really created the administrator (a repeated startup should be
  idempotent and should not recreate it).
- **Usage assertion failure**: usage is persisted asynchronously (bounded queue + flush on shutdown)
  and the test case already polls for it; if it stays 0 for a long time, suspect first that the
  metering path was never triggered or that the DSN points at another database.

## Notes

- This suite is a black-box smoke test: the data-plane groups exercise the gateway over real HTTP,
  and every admin/user write goes through the real `aigatectl` binary against that same gateway.
- The contract is defined by the interface frozen by the main owner; when you find a deviation
  between the implementation and the contract, flag it in `test/e2e/` first and sync with the main
  owner.
