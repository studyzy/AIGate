# CLI 参考（aigatectl）

[English](CLI.md) | [简体中文](CLI.zh-CN.md)

`aigatectl` 是 AIGate 的管控命令行工具。它把管理面 `/admin/*`、用户面 `/api/user/*`
与 `GET /healthz`、`GET /version` 共 **53 个端点** 变成 53 条稳定命令：命令与端点 1:1，不做工作流编排，
也不封装数据面 `/v1/*` 推理入口。响应体原样写到 stdout，诊断信息只写 stderr，退出码固定，
便于 shell 脚本、`make` 与 CI 直接调用。服务端一行未改：AIGate 仍是纯 API 服务。

## 1. 构建与安装

需要 Go 1.24+，不引入任何新依赖。

```bash
make build-cli   # 产出 bin/aigatectl
```

用法形式：

```
aigatectl [global flags] <resource> <command> [flags]
```

`--help` / `-h` 与 `version` 是本地命令，不发 HTTP 请求；`aigatectl version` 打印
`aigatectl <版本> (local binary)` 后退出 0。

`aigatectl` 与网关是**两个不同的二进制**，两边的版本必须区分：

| 想查什么 | 命令 | 报告的是 |
|---|---|---|
| CLI 二进制自身的版本 | `aigatectl version` | 本机 `aigatectl`（带 `(local binary)` 标识） |
| 后端服务的版本 / commit | `aigatectl build-info` | 远端网关，经 `GET /version` |
| 后端服务的版本 / commit | `aigatectl version --remote` | 请求与输出同 `build-info`，但先打一行标明本地二进制版本与所查询的网关地址（该行只在请求成功后写 stdout；失败时 stdout 为空、诊断写 stderr、退出码 5） |

## 2. 配置与凭据

`--url` 的优先级为：flag > `AIGATE_URL` 环境变量 > 默认值 `http://127.0.0.1:8080`。
根地址末尾的斜杠会被归一化；非法 URL 在本地报错并退出 2。

| 全局 flag | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `--url <url>` | `AIGATE_URL` | `http://127.0.0.1:8080` | 网关根地址 |
| `--timeout <duration>` | — | `30s` | 整请求超时（`time.Duration` 文本）；`usage export`、`models connectivity` 可调大 |
| `--data <json\|@file\|->` | — | （无） | 请求体，见 §6 |
| `--id <positive int>` | — | （无） | 路径参数，必须为正整数 |

**凭据只从环境变量读取**：CLI 不提供 `--token` 之类的 flag，token 绝不进入 argv、
`ps` 输出或日志。

| 环境变量 | 适用端点 | 说明 |
|---|---|---|
| `AIGATE_ADMIN_TOKEN` | `/admin/*` | 管理面 admin 访问 token |
| `AIGATE_USER_TOKEN` | `/api/user/*` | 用户面访问 token；未设置时回退 `AIGATE_ADMIN_TOKEN` |
| （无） | `/healthz` | 健康检查不需要凭据 |
| （无） | `/version` | 构建信息同样不需要凭据 |

按命令所属资源组自动选择 token，无需用户切换；所需 token 缺失时本地校验失败、退出 2，
且不发出请求。

### 递归帮助

```bash
aigatectl --help                       # 顶层：全局 flag 与资源列表
aigatectl models --help                # 资源级：models 下的命令
aigatectl models create --help         # 命令级：该命令的 Usage / 参数
aigatectl self --help                  # self 下的命令（含子组 usage / keys）
aigatectl self keys --help             # 子组：list/get/create/update/delete/rotate/usage
```

`--help` 可写为 `-h`。`self` 的两个子组 `usage`、`keys` 需要再取一级动作，因此命令名是
三个 token（如 `self usage summary`、`self keys list`）。

## 3. 命令总表（53 条）

每行给出两种等价写法：反引号包裹的 `METHOD /path` 端点标记，以及箭头后「方法 +
反引号路径」的请求行写法；命令表与 `docs/openapi.yaml` 的 53 个端点（不含 `/v1/*`）保持一致。
管理面没有 `GET /admin/users/{id}`，因此不提供 `users get`。

| # | 命令 | 方法 · 路径 | 示例 |
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
| 40 | `self me` | `GET /api/user/me` → GET `/api/user/me` | `aigatectl self me` |
| 41 | `self quota` | `GET /api/user/quota` → GET `/api/user/quota` | `aigatectl self quota` |
| 42 | `self usage summary` | `GET /api/user/usage/summary` → GET `/api/user/usage/summary` | `aigatectl self usage summary` |
| 43 | `self usage logs` | `GET /api/user/usage/logs` → GET `/api/user/usage/logs` | `aigatectl self usage logs` |
| 44 | `self usage by-model` | `GET /api/user/usage/by-model` → GET `/api/user/usage/by-model` | `aigatectl self usage by-model` |
| 45 | `self keys list` | `GET /api/user/keys` → GET `/api/user/keys` | `aigatectl self keys list` |
| 46 | `self keys get` | `GET /api/user/keys/{id}` → GET `/api/user/keys/{id}` | `aigatectl self keys get --id 1` |
| 47 | `self keys create` | `POST /api/user/keys` → POST `/api/user/keys` | `aigatectl self keys create --data @key.json` |
| 48 | `self keys update` | `PUT /api/user/keys/{id}` → PUT `/api/user/keys/{id}` | `aigatectl self keys update --id 1 --data @key.json` |
| 49 | `self keys delete` | `DELETE /api/user/keys/{id}` → DELETE `/api/user/keys/{id}` | `aigatectl self keys delete --id 1` |
| 50 | `self keys rotate` | `POST /api/user/keys/{id}/rotate` → POST `/api/user/keys/{id}/rotate` | `aigatectl self keys rotate --id 1` |
| 51 | `self keys usage` | `GET /api/user/keys/{id}/usage` → GET `/api/user/keys/{id}/usage` | `aigatectl self keys usage --id 1` |
| 52 | `health` | `GET /healthz` → GET `/healthz` | `aigatectl health` |
| 53 | `build-info` | `GET /version` → GET `/version` | `aigatectl build-info` |

`health` 是单 token 命令（`aigatectl health`），不需要任何凭据；其余命令为
`<resource> <command>` 两级（`self` 子组为三级）。本地命令 `version` 与 `--help`
不计入这 53 条 HTTP 命令。

## 4. 查询参数（query flag）

只有下列 15 条命令接受查询 flag；契约未声明的参数一律不暴露。flag 名用连字符，
发送时转换为下划线形式（如 `--group-by` → `group_by`）；值不做业务校验，交给服务端。

| CLI 命令 | 方法 · 路径 | 允许的 query flag |
|---|---|---|
| `usage export` | `GET /admin/usage/export` | `--format`, `--start`, `--end`, `--user-id`, `--group-id`, `--api-key-id`, `--ai-model-id` |
| `usage logs` | `GET /admin/usage/logs` | `--start`, `--end`, `--user-id`, `--group-id`, `--api-key-id`, `--ai-model-id`, `--page`, `--page-size` |
| `usage summary` | `GET /admin/usage/summary` | `--group-by`, `--start`, `--end`, `--user-id`, `--group-id`, `--api-key-id`, `--ai-model-id` |
| `webhooks deliveries` | `GET /admin/webhooks/{id}/deliveries` | `--page`, `--page-size` |
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

要点：

- `--group-by` 只出现在 `usage summary` 与 `self usage summary` 两条命令上。
- `--page` / `--page-size` 只出现在 `usage logs`、`self usage logs`、`webhooks deliveries` 上。
- `--format` 只出现在 `usage export` 上。
- `self keys *` 全部接受 `--user-id`；其中 `self keys usage` 只额外接受
  `--start` / `--end` / `--user-id`，不暴露 `--group-id` / `--api-key-id` / `--ai-model-id`。
- 其余命令不接受任何 query flag，其 argv 只允许为空或仅含命令自身的参数。

## 5. 退出码

| 码 | 含义 | 触发 |
|---|---|---|
| `0` | 成功 | HTTP 2xx（含 `models connectivity` 的 `ok:false` 与 `import run` 的部分失败）；`--help`、`version` 也为 0 |
| `2` | 用法/本地错误 | flag 解析失败、未知命令、非法 `--id` / `--url`、缺少 token、`--data` 读文件/JSON 解析失败 |
| `3` | 鉴权/授权失败 | 远端 401 / 403 |
| `4` | 远端业务错误 | 其它非 2xx（400/404/409/413/429/5xx 等） |
| `5` | 传输错误 | DNS / 连接 / TLS / 超时 / 响应体读取失败 |

stdout 只承载远端响应体；非 2xx 时把错误体的 `message`（或原样响应体）写到 stderr。

## 6. 请求体 `--data` 与敏感字段

`--data` 接受三种形式：JSON 字面量、`@file`（读文件）、`-`（读 stdin）。三种来源都会
在本地做 JSON 合法性校验，但**原样转发、不重新序列化**；带请求体的命令自动加
`Content-Type: application/json`。

```bash
aigatectl groups create --data '{"name":"demo"}'              # 不含敏感字段时可用字面量
aigatectl credentials create --data @credential.json           # 推荐：从文件读取
cat credential.json | aigatectl credentials create --data -     # 或从 stdin 读取
```

> **安全警告**：`--data` 的 JSON 字面量会出现在 argv 中，可能被 `ps`、shell 历史或
> 进程列表看到。**含敏感字段的请求体——上游 `api_key`、`secret`、绑定级 `api_key`、
> 导入数据里的明文 `token`——必须用 `--data @file` 或 `--data -`，绝不能放 argv。**
> CLI 不检测请求体内容，是否走文件/stdin 由调用方负责。同理，凭据只从环境变量读取，
> 不提供 `--token`。

## 7. 特殊语义

- `models connectivity` 的探测结果恒为 HTTP 200，成败体现在 body 的 `ok`；CLI 按 HTTP
  状态判定，仍退出 0，body 原样输出，由调用方读 `ok`。
- `import run` 单条失败仍返回 200（`details[].action=error`）；同样按 HTTP 判定、退出 0。
- `usage export --format csv` 返回 `text/csv` 文本流，CLI 原样打印，不做 JSON 解码。
- `DELETE` 类命令返回 `{"ok":true}`，原样输出。
- `create` / `rotate` 类命令响应里的 `token` 是一次性明文，只经 stdout 出现一次，
  不写日志、不缓存、不落盘；之后任何列表/详情接口都不再回显。
- 管理面没有 `GET /admin/users/{id}`，故不提供 `users get`。
- `health` 对应 `GET /healthz`，不需要任何凭据；`version` 是本地命令，报告的是
  **aigatectl 二进制自身**，不发请求。
- `build-info` 对应 `GET /version`，不需要凭据，原样打印**网关**的构建信息
  （`version`、`commit`、`build_time`、`dirty`、`go_version`、`started_at`），
  可直接 `aigatectl build-info | jq .commit`。`version --remote` 是它的别名：
  请求与 JSON 输出完全相同，只是先加一行标明本地二进制版本与所查询的网关地址，
  避免把两个二进制的版本混为一谈。这一行只在请求成功后写 stdout；网关不可达或返回
  非 2xx 时 stdout 保持为空，失败信息只进 stderr，因此 `| jq` 不会读到多余的一行。
- 2xx 响应体原样写 stdout（不解析、不重排、不加装饰）；非 2xx 的诊断信息只写 stderr。
