# Request log plugin

Request logging is a **replaceable capability** (an ability with pluggable
implementations), not a plugin chain. One edge middleware wraps the data plane, tees the
raw client-side request/response pair for every inference ingress, and hands it to
whatever implementation is selected — the core only knows the plugin interface and three
configuration strings. It is **off by default**: `-request-log=none` (the default) does
not install the middleware, does not write any request/response body to disk, and does not
register the admin read routes. Enabling it needs **no SQL migration**: the core schema
(`sql/init.sql`) is unchanged.

This document is the source of truth for the plugin contract, per-implementation
configuration, and how to add a new implementation. The HTTP contract (endpoints,
payloads, error codes) lives in [API.md](API.md) §3.12 and [openapi.yaml](openapi.yaml).
[API.zh-CN.md](API.zh-CN.md) and [request-log.zh-CN.md](request-log.zh-CN.md) are the
Chinese mirrors.

## Core configuration

| Flag | Environment variable | Default | Meaning |
|------|----------------------|---------|---------|
| `-request-log` | `AIGATE_REQUEST_LOG` | `none` | Implementation name. |
| `-request-log-config` | `AIGATE_REQUEST_LOG_CONFIG` | `""` | Inline JSON object, handed verbatim to the implementation. |
| `-request-log-config-file` | `AIGATE_REQUEST_LOG_CONFIG_FILE` | `""` | JSON file path; takes precedence over the inline value. |

Precedence is flag > env > default, and a file wins over the inline config. The core only
validates the *shape*: a non-empty name, and that a non-empty inline config is a JSON
object. The content of that object belongs to the implementation — the core has no
`es_endpoints`-style field. An unknown implementation name is rejected at startup.

Built-in implementations: `none` (no-op), `sqlite`, `es`.

## Plugin contract

`internal/requestlog` defines the interface, the named registry and the selector. The
contract files depend only on the standard library; implementation files may depend on
whatever they need (`gorm.io/gorm` + `github.com/glebarez/sqlite` for sqlite, `net/http`
for es). The admin plane depends only on the narrow `Reader` interface, never on gorm.

```go
type Plugin interface {
    Name() string
    Init(ctx context.Context, raw json.RawMessage) error // parse/validate own config, create table/index/connection
    Write(ctx context.Context, entries []Entry) error    // batch write, called by the async queue
    Search(ctx context.Context, q Query) (Page, error)   // list read
    Get(ctx context.Context, id string) (*Entry, error)  // detail read
    Prune(ctx context.Context, before time.Time) (int64, error)
    Retention() time.Duration                            // <= 0 disables the prune loop
    Close() error
}

type Reader interface {   // all the admin plane needs
    Search(ctx context.Context, q Query) (Page, error)
    Get(ctx context.Context, id string) (*Entry, error)
}

type Factory func() Plugin
func Register(name string, f Factory)   // called from each implementation's init; duplicate name panics
func Names() []string                   // sorted names, used in startup error messages
func Open(ctx context.Context, name string, raw json.RawMessage) (Plugin, error)
```

`Open` looks the name up in the registry, constructs an instance and calls `Init`; an
unknown name returns `unknown request log implementation %q (available: es, none, sqlite)`
and **refuses to start** rather than silently falling back. `none` is itself a registered
implementation, so it goes through the same code path.

`Entry` is the full record. `Row` (the list row) is `Entry` minus the four large fields
`request_body`, `response_body`, `request_headers`, `response_headers` — the list API
never pulls bodies back. `Query` carries all filter fields as UTC values; `Page` carries
`rows`, `total`, `page`, `page_size`. The exact JSON field names are in
[API.md](API.md) §3.12.

Each request gets a 16-random-byte (32 hex char) ID, set on the response header
`X-AIGate-Request-Id` before the handler runs, and stored as the row `id`.

## Adding an implementation (three steps)

All implementations live in the **same package** `internal/requestlog`, so an `init()` in
a new file is always compiled into the binary — `cmd/aigate` needs no new import and the
core needs no changes.

1. **New file in `internal/requestlog`** implementing `requestlog.Plugin` (and `Reader`):
   its own config struct, an `Init` that parses/validates config and creates its
   table/index, plus `Write` / `Search` / `Get` / `Prune` / `Retention` / `Close`. It
   owns its schema and its config; the core knows neither.
2. **`init()` calls `Register(name, factory)`** in that file, e.g.
   `func init() { Register("myimpl", func() Plugin { return &myImpl{} }) }`. Names must be
   unique (duplicate registration panics, like `registerStrategy`).
3. **Select it with `-request-log=<name>`** and supply its own config via
   `-request-log-config` or (preferred for secrets) `-request-log-config-file`. Nothing
   else changes: the middleware, queue, retention loop and admin routes are
   implementation-agnostic.

Keep the read implementations behaviourally aligned: the same `Query` must produce the
same field set, the same half-open date semantics and the same pagination window, so a
filter never silently works in one backend and is ignored in another.

## Implementation configuration

Config is a JSON object. `{}` (or an empty value) means "use the implementation's
defaults"; invalid config is a startup error, never silently normalised.

### sqlite

```json
{"path":"request-logs.db","retention_days":7}
```

| Key | Default | Notes |
|-----|---------|-------|
| `path` | `request-logs.db` | SQLite file, relative to the process working directory. |
| `retention_days` | `7` | `0` = never prune; upper bound `36500`. |

The plugin opens its own SQLite file and creates the `request_logs` table and indexes
itself. It is a **local, single-process store** — for a shared, searchable store in a
MySQL production deployment, use `es`. It does not touch `sql/init.sql`.

### es

```json
{"endpoints":["http://es:9200"],"index":"aigate-request-logs","retention_days":7,"timeout":"5s"}
```

Authentication fields (add them to the same object):

```json
{
  "endpoints": ["http://es:9200"],
  "index": "aigate-request-logs",
  "username": "elastic",
  "password": "<secret>",
  "api_key": "",
  "retention_days": 7,
  "timeout": "5s"
}
```

| Key | Default | Notes |
|-----|---------|-------|
| `endpoints` | (required) | ES REST endpoints; non-empty. |
| `index` | `aigate-request-logs` | Index name; created with mappings if absent. |
| `username` / `password` | `""` | Basic auth. |
| `api_key` | `""` | `Authorization: ApiKey <...>`; **mutually exclusive with `password`** (giving both is an error). |
| `retention_days` | `7` | `0` = never prune; upper bound `36500`. |
| `timeout` | `5s` | Parsed with `time.ParseDuration` (e.g. `5s`, `10s`). |

Only stdlib `net/http` is used (`_bulk` / `_search` / `_doc` / `_delete_by_query`), so
there is no new dependency and no version probing; target ES 8.x/9.x. Endpoints come
from operator config, not from request input — do not put untrusted addresses there.

## Read API and all 13 filter parameters

`GET /admin/request-logs` (list) and `GET /admin/request-logs/{id}` (detail) are
registered **only when a plugin is enabled**; both sit behind the existing admin Bearer
auth. The list returns `{"logs":[Row...],"total":N,"page":P,"page_size":S}` with metadata
only; the detail returns the full `Entry` (the Row plus `request_body`, `response_body`,
`request_headers`, `response_headers`).

All 13 filter parameters are optional and combinable; `0` / empty means "no filter":

| # | Parameter | Meaning |
|---|-----------|---------|
| 1 | `start` | Range start (inclusive). |
| 2 | `end` | Range end (see date semantics below). |
| 3 | `api_key_id` | Key ID; translated by the admin plane to the key's current `proxy_token_hash`. Unknown key → 404. Wins over `api_key_hash`. |
| 4 | `api_key_hash` | Direct credential sha256 hex filter. |
| 5 | `model` | Exact model name parsed from the request body. |
| 6 | `path` | Exact ingress path, e.g. `/v1/messages`. |
| 7 | `status_code` | Exact HTTP status code. |
| 8 | `stream` | `true` / `false` only. |
| 9 | `min_latency_ms` | Inclusive lower latency bound. |
| 10 | `max_latency_ms` | Inclusive upper latency bound; `min > max` → 400. |
| 11 | `q` | Full text, best effort: `LIKE` in sqlite, `multi_match` in es. No consistent scoring is promised. |
| 12 | `page` | 1-based, default `1`. |
| 13 | `page_size` | Default `50`, capped at `500`. |

## Date and pagination semantics

Both implementations consume the same normalised, **half-open UTC interval
`[start, end)`**: `created_at >= start AND created_at < end` (never `BETWEEN`, which
would drop everything after 00:00 on the `end` day).

- `start` / `end` accept `YYYY-MM-DD` (business timezone, whole day) or RFC3339
  (`2026-11-25T10:30:00+08:00` / `...Z`). A value containing `T` is parsed as RFC3339.
- For a **date** `end`, the day is **inclusive**: it is converted to an exclusive upper
  bound by adding one day (`+1 day`). For an **RFC3339** `end`, the supplied instant is
  already the exclusive upper bound.
- `start > end` → `400 invalid time range`.
- `page` / `page_size` default to `1` / `50`, `page_size` is capped at `500`, and the
  shared **`maxResultWindow = 10000`** applies: `(page-1)*page_size + page_size > 10000`
  → `400 page out of range`. sqlite's `OFFSET` has no such limit, so the check is applied
  in the shared parser to keep both backends identical. Results are ordered
  `created_at DESC` (newest first).

## Rotate caveat

`api_key_id` is translated to the key's **current** `proxy_token_hash` at query time.
After a key **rotate** (or delete), that hash changes, so `api_key_id` only matches rows
recorded *after* the rotate. **Older rows are reachable only via `api_key_hash`**, i.e.
by passing the sha256 hex of the old credential directly. This is why the direct
`api_key_hash` filter exists; keep a record of old hashes if you need to audit rotated
keys.

## Security

- Credential headers (`Authorization`, `x-api-key`, `Cookie`, `X-Api-Key`, any
  `*-token` / `*-key` header) are **never stored**; only `hex(sha256(token))` is kept,
  under the same algorithm as `model.HashToken`. Whitelisted headers only
  (`Content-Type`, `Accept`, `User-Agent`, `X-Request-Id`, `X-AIGate-*`) are recorded.
- The **raw plugin config is never logged** and never put into argv display: startup logs
  only carry the implementation name. ES `password` / `api_key` live inside that config,
  so **prefer `-request-log-config-file`** (a file with restricted permissions) over the
  inline `-request-log-config` flag, which would expose secrets in the process command
  line.
- Bodies are the raw bytes the client saw, capped at 1 MiB per side (`truncated` marks a
  cut). They may contain prompts and other user data — set retention to match your
  compliance requirements. Do not put untrusted addresses in `endpoints`.

## Retention

Retention is per-implementation, configured with `retention_days`. `0` means **never
prune**; the default is `7` days and the upper bound is `36500` (avoiding
`time.Duration` overflow). The gateway prunes once at startup and then hourly by calling
`Plugin.Prune(ctx, now-retention)`; `none` starts no prune loop. Pruning is best effort —
a failure is logged, not returned to the caller.

## Schema ownership

Each implementation **owns its own schema**: sqlite runs `AutoMigrate` on its own
`request_logs` table in its own file; es creates its own index and mappings. The core
`sql/init.sql` is **unchanged**, no `autoMigrateModels` entry is added, and **no MySQL
migration / incremental script is needed**. Existing MySQL deployments carry no new
table; enabling the plugin with `sqlite` writes to a local file, and with `es` writes to
the configured cluster. The two backends must still return the same fields, filters and
pagination so the admin UI and API contract stay identical.

## Caveats and known differences

- **`q` matching differs, not just scoring.** sqlite uses a literal `LIKE` substring
  match; es uses a tokenized `multi_match` (split by the analyser). The same `q` can
  therefore match different rows in the two backends — a matching-semantics difference,
  beyond the lack of consistent scoring noted above.
- **Bodies over 1 MiB lose best-effort metadata.** `model` and `stream` are parsed only
  from the truncated request-body prefix (the 1 MiB capture cap). For a request body over
  1 MiB the incomplete prefix usually fails to parse, so both fields are left empty in
  the log.
- **Mixed `x-api-key` and `Authorization: Bearer`.** When a client sends both, the log
  records `sha256(x-api-key)` (x-api-key takes priority), but `/v1/chat/completions`
  authenticates the Bearer key. Filtering by that Bearer key's `api_key_id` may therefore
  not find the row; use the `api_key_hash` of the actually-used x-api-key instead.
- **Unknown config keys are rejected.** Both `sqlite` and `es` use
  `DisallowUnknownFields`: a misspelled key such as `retention_day` is a startup error,
  not a silent fallback to the 7-day default.
- **`es` is tested against a mock; smoke-test your real cluster.** The `es` implementation
  is exercised in tests only against an in-process mock Elasticsearch. Before production
  use, run a smoke test (create index, `_bulk`, `_search`, `_delete_by_query`) against
  your real ES 8.x/9.x cluster and its actual mappings/credentials.
- **`created_at` precision.** `es` maps `created_at` as `date_nanos` and writes it in
  RFC3339Nano (nanosecond precision); sqlite keeps full Go `time.Time` precision. Both
  backends sort `created_at DESC, id DESC`, so `id` breaks ties for stable paging.
