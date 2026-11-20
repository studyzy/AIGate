# 管控 WebUI

AIGate 自带浏览器管理后台，编译进同一个二进制，不需要额外部署前端服务。

## 访问与登录

- 启动后打开 `http://<host>:8080/ui/`；访问 `/` 会自动 302 跳转到 `/ui/`。
- 登录凭据为管理员 token：`-init-admin-token`（或环境变量 `AIGATE_INIT_ADMIN_TOKEN`）
  初始化值，或任何 `role=admin` 用户的访问 token。
- token 只存在浏览器的 `sessionStorage`：关掉标签页即失效，不落 cookie，也不写 localStorage。
- 任意接口返回 401 会自动清掉本地 token 并跳回登录页。

## 页面

| 分组 | 页面 | 能力 |
| --- | --- | --- |
| 监控 | 总览 | 时间范围切换、请求与 token 趋势、模型与密钥计数、模型 Top |
| 监控 | 用量 | 汇总（按日期/模型/密钥/用户/分组）与请求明细（状态码、延迟、token 拆分、分页），CSV 导出 |
| 资源 | 供应商 | 按 `provider` 分组：一行一个厂商，展开看该厂商下的模型。表单内联维护凭据（名称 + 一个或多个 API Key）、上游地址与模型类型，「拉取模型」列出上游 `/models` 结果供勾选；整组启停/重命名/删除，模型级改显示名、配额、启停与连通性测试 |
| 访问 | 密钥 | CRUD、模型绑定、配额规则行式编辑、rotate 后一次性明文 |
| 访问 | 用户 | CRUD、角色与分组、rotate 后一次性明文 |
| 访问 | 分组 | CRUD、密钥级与全局配额规则 |
| 系统 | 设置 | 默认配额、全局配额与作用域、用户密钥上限 |
| 系统 | Webhook | CRUD、投递记录 |
| 系统 | 导入 | 上传 JSON → 本地预览 → 写入并展示逐条结果 |

界面支持 English / 简体中文，默认跟随浏览器语言，切换即时生效并记忆在 `localStorage`。

## 构建

前端源码在 `webui/`（React + TypeScript + Vite + Tailwind）。

```bash
make build          # 先 pnpm build 生成 internal/webui/dist，再 go build
make build-webui    # 只构建前端
make test-webui     # 前端单测（Vitest）
make test-e2e-webui # 浏览器端到端（Playwright + 真实二进制 + mock 上游）
```

`internal/webui/dist` 只提交了占位 `.gitkeep`：没有 node 环境时 `go build ./...` 仍可
编译，此时访问 `/ui/` 会看到提示构建方式的页面。Docker 镜像走 `Dockerfile` 的 node
阶段，产物始终是真实前端。

## 本地开发

```bash
make run            # 网关监听 :8080
pnpm --dir webui dev  # Vite dev server :5173，代理 /admin、/api、/v1 到 :8080
```

## 约束

- 前端只调用现有管理面接口，不改变任何 API 契约；字段类型手写对齐 `docs/openapi.yaml`。
- SPA 挂载在 `/ui/` 前缀内，未知路径回退到 `index.html`，不影响 `/admin`、`/api`、`/v1` 的 404 语义。
