//
//
// Tencent is pleased to support the open source community by making Tencent Cloud AIGate available.
//
// Copyright (C) 2026 Tencent.
// All rights reserved.
//
// If you have downloaded a copy of the Tencent Cloud AIGate source code from Tencent,
// please note that Tencent Cloud AIGate source code is licensed under the Apache 2.0 License,
// A copy of the Apache 2.0 License is included in this file.
//
//

package cli

import (
	"net/http"

	"aigate/internal/client"
)

// usageCommands 返回 usage 资源的 3 条子命令。三者的 query flag 集合逐字取自
// docs/openapi.yaml（见 query-flags.md）：--group-by 只在 summary，--page/
// --page-size 只在 logs，--format 只在 export；路径取自管理面契约，全部经
// execRequest 统一解析 flag、编排退出码与输出（解析失败退出 2 不发请求）。
func usageCommands() map[string]Command {
	return map[string]Command{
		"summary": {
			Usage: "usage summary",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/usage/summary",
				}, "group-by", "start", "end", "user-id", "group-id", "api-key-id", "ai-model-id")
			},
		},
		"logs": {
			Usage: "usage logs",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/usage/logs",
				}, "start", "end", "user-id", "group-id", "api-key-id", "ai-model-id", "page", "page-size")
			},
		},
		"export": {
			Usage: "usage export",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/usage/export",
				}, "format", "start", "end", "user-id", "group-id", "api-key-id", "ai-model-id")
			},
		},
	}
}

// init 把 usage 命令表注册到全局注册表，供 cli.Run 分发。
func init() {
	Register("usage", usageCommands())
}
