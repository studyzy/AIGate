# 请求日志插件

请求日志是一个**可替换实现的能力**（能力级插件），不是插件链。一层边缘中间件包住数据面，
tee 每个推理入口在客户端侧看到的原始请求/响应对，交给所选实现；核心只认插件接口与三个
配置字符串。它**默认关闭**：`-request-log=none`（默认值）不安装中间件、不把任何请求/响应
正文写到磁盘、也不注册管理面读路由。启用**不需要 SQL 迁移**：核心 schema
（`sql/init.sql`）未变。

本文是插件契约、各实现配置与「如何新增实现」的事实源。HTTP 契约（端点、报文、错误码）
见 [API.zh-CN.md](API.zh-CN.md) §3.12 与 [openapi.yaml](openapi.yaml)；英文镜像见
[API.md](API.md) 与 [request-log.md](request-log.md)。

## 核心配置

| Flag | 环境变量 | 默认值 | 说明 |
|------|----------|--------|------|
| `-request-log` | `AIGATE_REQUEST_LOG` | `none` | 实现名。 |
| `-request-log-config` | `AIGATE_REQUEST_LOG_CONFIG` | `""` | 内联 JSON 对象，原样交给实现。 |
| `-request-log-config-file` | `AIGATE_REQUEST_LOG_CONFIG_FILE` | `""` | JSON 文件路径，优先于内联配置。 |

优先级为 flag > 环境变量 > 默认值，且配置文件优先于内联配置。核心只校验**结构**：
实现名非空、非空的内联配置必须是 JSON 对象。对象内容归实现所有——核心里没有
`es_endpoints` 这类字段。未知实现名在启动阶段直接拒绝。

内置实现：`none`（空实现）、`sqlite`、`es`。

## 插件契约

`internal/requestlog` 定义接口、具名注册表与选择器。契约文件只依赖标准库；实现文件可用
各自需要的库（sqlite 用 `gorm.io/gorm` + `github.com/glebarez/sqlite`，es 用 `net/http`）。
管理面只依赖窄接口 `Reader`，永远不 import gorm。

```go
type Plugin interface {
    Name() string
    Init(ctx context.Context, raw json.RawMessage) error // 解析/校验自己的配置，建表/索引/连接
    Write(ctx context.Context, entries []Entry) error    // 批量写入，由异步队列调用
    Search(ctx context.Context, q Query) (Page, error)   // 列表读
    Get(ctx context.Context, id string) (*Entry, error)  // 详情读
    Prune(ctx context.Context, before time.Time) (int64, error)
    Retention() time.Duration                            // <= 0 时核心不启动清理循环
    Close() error
}

type Reader interface {   // 管理面只需要它
    Search(ctx context.Context, q Query) (Page, error)
    Get(ctx context.Context, id string) (*Entry, error)
}

type Factory func() Plugin
func Register(name string, f Factory)   // 各实现的 init 调用；重名 panic
func Names() []string                   // 排序输出，用于启动错误提示
func Open(ctx context.Context, name string, raw json.RawMessage) (Plugin, error)
```

`Open` 查注册表、创建实例并调用 `Init`；未知名字返回
`unknown request log implementation %q (available: es, none, sqlite)` 并**拒绝启动**，
不静默兜底。`none` 本身也是注册在表里的实现，与其它名字走同一条代码路径。

`Entry` 是完整记录。`Row`（列表行）是 `Entry` 去掉四个大字段 `request_body`、
`response_body`、`request_headers`、`response_headers`——列表接口绝不回带正文。`Query`
携带全部筛选字段（UTC 值）；`Page` 含 `rows`、`total`、`page`、`page_size`。确切的
JSON 字段名见 [API.zh-CN.md](API.zh-CN.md) §3.12。

每个请求生成一个 16 随机字节（32 位十六进制）的 ID，在 handler 运行前写入响应头
`X-AIGate-Request-Id`，并作为行的 `id` 入库。

## 新增实现三步

所有实现都在**同一个包** `internal/requestlog` 内，因此新文件里的 `init()` 必然被编译进
二进制——`cmd/aigate` 不需要新增 import，核心也不需要改。

1. **在 `internal/requestlog` 新增文件**，实现 `requestlog.Plugin`（以及 `Reader`）：
   自带配置结构体、解析/校验配置并建表/索引的 `Init`，以及 `Write` / `Search` /
   `Get` / `Prune` / `Retention` / `Close`。它自带 schema 与配置，核心一概不知。
2. **在该文件的 `init()` 里调用 `Register(name, factory)`**，例如
   `func init() { Register("myimpl", func() Plugin { return &myImpl{} }) }`。名字必须唯一
   （重复注册直接 panic，与 `registerStrategy` 一致）。
3. **用 `-request-log=<name>` 选中它**，并通过 `-request-log-config` 或（放凭据时更推荐的）
   `-request-log-config-file` 提供它自己的配置。其余都不用改：中间件、队列、清理循环与
   管理面路由都与实现无关。

两种读实现的行为必须对齐：同一份 `Query` 必须返回同一套字段、同一套半开日期语义与同一套
分页窗口，绝不能出现某个筛选在一种实现里生效、在另一种里被悄悄忽略。

## 实现配置

配置是一个 JSON 对象。`{}`（或空值）表示「用实现默认值」；非法配置是启动错误，不做静默归一。

### sqlite

```json
{"path":"request-logs.db","retention_days":7}
```

| 键 | 默认值 | 说明 |
|----|--------|------|
| `path` | `request-logs.db` | SQLite 文件，相对进程工作目录。 |
| `retention_days` | `7` | `0` = 永不清理；上界 `36500`。 |

插件自己打开独立的 SQLite 文件并建 `request_logs` 表与索引。它是**本地单进程存储**——
MySQL 生产部署要共享、可检索的存储请用 `es`。它不改 `sql/init.sql`。

### es

```json
{"endpoints":["http://es:9200"],"index":"aigate-request-logs","retention_days":7,"timeout":"5s"}
```

认证字段（加进同一个对象）：

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

| 键 | 默认值 | 说明 |
|----|--------|------|
| `endpoints` | （必填） | ES REST 端点，非空。 |
| `index` | `aigate-request-logs` | 索引名；不存在时带 mappings 创建。 |
| `username` / `password` | `""` | Basic 认证。 |
| `api_key` | `""` | `Authorization: ApiKey <...>`；**与 `password` 二选一**（同时给出报错）。 |
| `retention_days` | `7` | `0` = 永不清理；上界 `36500`。 |
| `timeout` | `5s` | 用 `time.ParseDuration` 解析（如 `5s`、`10s`）。 |

只用标准库 `net/http`（`_bulk` / `_search` / `_doc` / `_delete_by_query`），无新依赖、
无版本探测；目标 ES 8.x/9.x。端点来自运维配置而非请求输入——不要把不可信地址写进去。

## 读接口与全部 13 个筛选参数

`GET /admin/request-logs`（列表）与 `GET /admin/request-logs/{id}`（详情）**仅在启用插件时
注册**，都挂在既有管理面 Bearer 鉴权子树下。列表返回
`{"logs":[Row...],"total":N,"page":P,"page_size":S}`，只含元数据；详情返回完整 `Entry`
（列表行字段之外补出 `request_body`、`response_body`、`request_headers`、
`response_headers`）。

13 个筛选参数全部可选、可组合；`0` / 空串表示「不过滤」：

| # | 参数 | 含义 |
|---|------|------|
| 1 | `start` | 范围起点（含）。 |
| 2 | `end` | 范围终点（语义见下）。 |
| 3 | `api_key_id` | 密钥 ID；由管理面翻译成该密钥当前的 `proxy_token_hash`。密钥不存在 → 404；优先于 `api_key_hash`。 |
| 4 | `api_key_hash` | 直接按凭据 sha256 hex 过滤。 |
| 5 | `model` | 精确匹配从请求体解析出的模型名。 |
| 6 | `path` | 精确匹配入口路径，如 `/v1/messages`。 |
| 7 | `status_code` | 精确匹配 HTTP 状态码。 |
| 8 | `stream` | 仅 `true` / `false`。 |
| 9 | `min_latency_ms` | 延迟下界（含端点）。 |
| 10 | `max_latency_ms` | 延迟上界（含端点）；`min > max` → 400。 |
| 11 | `q` | 全文，尽力而为：sqlite 用 `LIKE`、es 用 `multi_match`，不承诺打分一致。 |
| 12 | `page` | 从 1 开始，默认 `1`。 |
| 13 | `page_size` | 默认 `50`，上限 `500`。 |

## 日期与分页语义

两种实现消费同一份已归一化的**半开 UTC 区间 `[start, end)`**：
`created_at >= start AND created_at < end`（绝不用 `BETWEEN`，否则 `end` 当天 00:00
之后的记录会全部漏掉）。

- `start` / `end` 接受 `YYYY-MM-DD`（业务时区整天）或 RFC3339
  （`2026-11-25T10:30:00+08:00` / `...Z`）；取值含 `T` 时按 RFC3339 解析。
- **日期**形式的 `end` **含当日**：靠 `+1 天` 换算成排他上界；**RFC3339** 形式的 `end`
  所给时刻本身即排他上界。
- `start > end` → `400 invalid time range`。
- `page` / `page_size` 默认 `1` / `50`，`page_size` 上限 `500`，并共用
  **`maxResultWindow = 10000`**：`(page-1)*page_size + page_size > 10000` →
  `400 page out of range`。sqlite 的 `OFFSET` 没有该限制，因此校验放在共享解析器里，
  保证两种后端一致。结果按 `created_at DESC`（时间倒序）排列。

## rotate 兜底

`api_key_id` 在查询时被翻译成密钥**当前**的 `proxy_token_hash`。密钥 **rotate**（或删除）
后该 hash 会变，所以 `api_key_id` 只能查到 rotate **之后**的记录。**更早的历史行只能用
`api_key_hash` 查到**——即直接传旧凭据的 sha256 hex。这正是保留 `api_key_hash` 直查参数的
原因；若要审计已轮换的密钥，请自行留存旧 hash。

## 安全

- 凭据类头（`Authorization`、`x-api-key`、`Cookie`、`X-Api-Key`、任何 `*-token` /
  `*-key` 头）**一律不入库**；只存 `hex(sha256(token))`，算法与 `model.HashToken` 一致。
  只记录白名单头（`Content-Type`、`Accept`、`User-Agent`、`X-Request-Id`、
  `X-AIGate-*`）。
- **原始配置绝不写日志、绝不出现在 argv 展示里**：启动日志只带实现名。ES 的
  `password` / `api_key` 就在这份配置里，因此**优先用 `-request-log-config-file`**
  （权限受限的文件），不要用内联的 `-request-log-config`——后者会让凭据出现在进程命令行。
- 正文是客户端看到的原始字节，每侧上限 1 MiB（超限置 `truncated`）。其中可能含
  prompt 等用户数据，请按合规要求设置保留期；不要把不可信地址写进 `endpoints`。

## 保留期

保留期按实现各自配置，字段为 `retention_days`。`0` 表示**永不清理**；默认 `7` 天，
上界 `36500`（避免 `time.Duration` 溢出）。网关启动时清理一次、此后每小时调用
`Plugin.Prune(ctx, now-retention)` 清理一次；`none` 不启动清理循环。清理是尽力而为——
失败只记日志，不回给调用方。

## schema 归属

每种实现**自带 schema**：sqlite 在自己的文件里对自己的 `request_logs` 表跑
`AutoMigrate`；es 自己创建索引与 mappings。核心 `sql/init.sql` **未改动**，不新增
`autoMigrateModels` 条目，**不需要 MySQL 迁移 / 增量脚本**。存量 MySQL 部署不会多出
新表；启用 `sqlite` 时写到本地独立文件，启用 `es` 时写到配置的集群。两种后端仍须返回
同一套字段、筛选与分页语义，保证管理面接口与 WebUI 契约一致。

## 已知差异与限制

- **`q` 的匹配语义不同，不只是打分不同。** sqlite 用字面 `LIKE` 子串匹配，es 用分词的
  `multi_match`（按分析器切词）。同一 `q` 在两种后端可能命中不同的行——这是匹配语义差异，
  不只是上文提到的打分不一致。
- **超过 1 MiB 的正文会丢失尽力解析的元数据。** `model` / `stream` 只从截断后的请求体前缀
  （1 MiB 采集上限）解析；请求体超过 1 MiB 时，不完整的前缀通常解析失败，日志里这两个字段
  会留空。
- **同时带 `x-api-key` 与 `Authorization: Bearer`。** 客户端同时给两者时，日志记录
  `sha256(x-api-key)`（`x-api-key` 优先），但 `/v1/chat/completions` 用 Bearer 密钥鉴权。
  因此按该 Bearer 密钥的 `api_key_id` 过滤可能查不到这一行；应改用实际生效的 `x-api-key`
  的 `api_key_hash`。
- **未知配置键会被拒绝。** `sqlite` 与 `es` 都用 `DisallowUnknownFields`：拼错的键（如
  `retention_day`）是启动错误，不会静默回退到默认 7 天。
- **`es` 仅对 mock 测试，生产前请做真实集群冒烟。** `es` 实现的测试只在进程内的 mock
  Elasticsearch 上进行。生产使用前，请针对真实的 ES 8.x/9.x 集群及其实际 mappings/凭据做
  一次冒烟（建索引、`_bulk`、`_search`、`_delete_by_query`）。
- **`created_at` 精度。** `es` 把 `created_at` 映射为 `date_nanos`，并以 RFC3339Nano
  （纳秒精度）写入；sqlite 保留 Go `time.Time` 的完整精度。两种后端都按
  `created_at DESC, id DESC` 排序，用 `id` 打破并列以保证分页稳定。
