# Tencent Cloud AIGate end-to-end smoke test

`test/e2e/` is an end-to-end smoke suite that is independent from the unit tests: it drives the
**real gateway binary** and the **real `aigatectl` CLI binary** as black boxes, starts in-process mock
upstreams (both the OpenAI and the Anthropic protocol), covers the independent scenario groups below
(M1 forwarding smoke, M2 quota & usage, M3 Anthropic ingress, M3b gateway ops, M4 webhooks,
and the aigatectl CLI groups) over real HTTP, and connects directly to MySQL to
assert that usage is persisted. It **does not import the `internal` package**, so it can be
maintained independently while the gateway internals evolve in parallel.

## How to run

```bash
bash test/e2e/run.sh
```

The script performs the following steps in order: reset the test database `aigate_e2e` ->
import `sql/init.sql` from the repository root -> `go build -o bin/aigate ./cmd/aigate` ->
`go build -o bin/aigatectl ./cmd/aigatectl` -> export environment variables ->
`go test -tags e2e -count=1 -v ./test/e2e/`. The script is repeatable: every
run recreates the database and both binaries.

To only re-run the test cases (database and binaries already in place):

```bash
AIGATE_E2E_BIN="$(pwd)/bin/aigate" \
AIGATE_E2E_CLI_BIN="$(pwd)/bin/aigatectl" \
AIGATE_E2E_DSN='root@tcp(127.0.0.1:3306)/aigate_e2e?charset=utf8mb4&parseTime=True&loc=Local' \
go test -tags e2e -count=1 -v ./test/e2e/
```

Three environment variables point the suite at the built artifacts:

| Variable | Meaning |
|----------|---------|
| `AIGATE_E2E_BIN` | Path to the gateway binary (`bin/aigate`) |
| `AIGATE_E2E_CLI_BIN` | Path to the management CLI binary (`bin/aigatectl`), used by `cli_test.go` |
| `AIGATE_E2E_DSN` | MySQL DSN of the `aigate_e2e` database |

By convention, if any of `AIGATE_E2E_BIN`, `AIGATE_E2E_CLI_BIN` or `AIGATE_E2E_DSN` is missing,
the test cases call `t.Skip` (they do not fail), so the suite also compiles and runs cleanly in an
environment without MySQL. The CLI group additionally checks `AIGATE_E2E_CLI_BIN` via `requireEnv`.

## Prerequisites

- A local MySQL is running and `root` can log in without a password (default `127.0.0.1:3306`;
  override with `MYSQL_HOST` / `MYSQL_PORT` / `MYSQL_USER` / `MYSQL_ARGS`).
- The `mysql` command-line client is available (the script uses it to create the database and to
  import the schema).
- `sql/init.sql` exists at the repository root (the source of truth for the schema).
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
| `TestE2E_CLI_ClosedLoop` (`cli_test.go`) | Drive the real `aigatectl` binary through the 8-step closed loop: `health` → `models create` → `models list`/`get` → `users create` → `keys create` → data-plane forwarding → `self me`/`self keys list` → `keys rotate` | Every step exits 0 with JSON on stdout and empty stderr; the created model row exists in `ai_models` (direct MySQL query); the `sk-` token issued by the CLI really drives `/v1/chat/completions` and its usage lands in `llm_usage_logs`; the rotated-away token is rejected with 401 |
| `TestE2E_CLI_ExitCodes` (`cli_test.go`) | One case per CLI exit code: 0 / 2 / 3 / 4 / 5 | Errors appear only on stderr and stdout is empty |
| `TestE2E_CLI_SpecialSemantics` (`cli_test.go`) | `models connectivity` (`ok:false` but HTTP 200), `import run` (partial failure but HTTP 200), `usage export --format csv` | All still exit 0 with the body verbatim; CSV is raw text and is never JSON-decoded |

## Troubleshooting

- **Port already in use**: the gateway listen port is a free loopback port allocated dynamically by
  the test at runtime, it is never hard-coded; if the process under test leaves something behind,
  clean up the leftover `gateway` process and re-run. The subprocess receives `SIGTERM` in
  `t.Cleanup` and exits gracefully.
- **MySQL not running / cannot connect**: make sure a local MySQL is running, `root` can connect
  without a password, and the test database exists (prefer running `run.sh` directly, it recreates
  the database). When step 9 fails, the error message reports the wait timeout and the last query
  error.
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

- This suite is a black-box smoke test: the M1–M4 groups exercise the gateway over real HTTP, and
  the CLI groups drive the real `aigatectl` binary against that same gateway.
- The contract is defined by the interface frozen by the main owner; when you find a deviation
  between the implementation and the contract, flag it in `test/e2e/` first and sync with the main
  owner.
