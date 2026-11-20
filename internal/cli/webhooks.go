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
	"strconv"

	"aigate/internal/client"
)

// webhooksCommands 返回 webhooks 资源的 6 条子命令。带 id 的命令经 execIDRequest
// 校验 --id 后把 ctx.ID 格式化进路径，create/update 以 ctx.Data 作为请求体；
// deliveries 按 docs/openapi.yaml 只暴露 --page / --page-size 两个 query flag，
// 其余命令的 argv 只允许为空。全部经 execRequest 统一解析 flag、编排退出码与
// 输出（解析失败退出 2 不发请求）。
func webhooksCommands() map[string]Command {
	return map[string]Command{
		"list": {
			Usage: "webhooks list",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/webhooks",
					Body:   nil,
				})
			},
		},
		"get": {
			Usage: "webhooks get",
			Run: func(ctx *Context, argv []string) int {
				return execIDRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/webhooks/" + strconv.FormatUint(ctx.ID, 10),
					Body:   nil,
				})
			},
		},
		"create": {
			Usage: "webhooks create",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodPost,
					Path:   "/admin/webhooks",
					Body:   ctx.Data,
				})
			},
		},
		"update": {
			Usage: "webhooks update",
			Run: func(ctx *Context, argv []string) int {
				return execIDRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodPut,
					Path:   "/admin/webhooks/" + strconv.FormatUint(ctx.ID, 10),
					Body:   ctx.Data,
				})
			},
		},
		"delete": {
			Usage: "webhooks delete",
			Run: func(ctx *Context, argv []string) int {
				return execIDRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodDelete,
					Path:   "/admin/webhooks/" + strconv.FormatUint(ctx.ID, 10),
					Body:   nil,
				})
			},
		},
		"deliveries": {
			Usage: "webhooks deliveries",
			Run: func(ctx *Context, argv []string) int {
				return execIDRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/webhooks/" + strconv.FormatUint(ctx.ID, 10) + "/deliveries",
				}, "page", "page-size")
			},
		},
	}
}

// init 把 webhooks 命令表注册到全局注册表，供 cli.Run 分发。
func init() {
	Register("webhooks", webhooksCommands())
}
