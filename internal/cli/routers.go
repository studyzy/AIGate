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

// routersCommands 返回 routers 资源的 7 条子命令，即模型路由规则（名称 + 策略 +
// 策略私有 JSON 配置 + 作用域 + 优先级）。
//
// 路径与 docs/openapi.yaml 逐字对应：list/strategies 无 --id；get/update/delete/
// preview 的 --id 是**规则** id；create/update 以 ctx.Data 作为请求体（--data
// 支持字面量、@文件与 - 标准输入）。create/update/preview 必须带请求体，其余
// 命令拒绝 --data，避免"看起来生效其实没发"的误用。
func routersCommands() map[string]Command {
	return map[string]Command{
		"list": {
			Usage: "routers list",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/model-routers",
				})
			},
		},
		"strategies": {
			Usage: "routers strategies",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/model-routers/strategies",
				})
			},
		},
		"get": {
			Usage: "routers get",
			Run: func(ctx *Context, argv []string) int {
				return execIDRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   routerPath(ctx),
				})
			},
		},
		"create": {
			Usage: "routers create",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodPost,
					Path:   "/admin/model-routers",
					Body:   ctx.Data,
				})
			},
		},
		"update": {
			Usage: "routers update",
			Run: func(ctx *Context, argv []string) int {
				return execIDRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodPut,
					Path:   routerPath(ctx),
					Body:   ctx.Data,
				})
			},
		},
		"delete": {
			Usage: "routers delete",
			Run: func(ctx *Context, argv []string) int {
				return execIDRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodDelete,
					Path:   routerPath(ctx),
				})
			},
		},
		"preview": {
			Usage: "routers preview",
			Run: func(ctx *Context, argv []string) int {
				return execIDRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodPost,
					Path:   routerPath(ctx) + "/preview",
					Body:   ctx.Data,
				})
			},
		},
	}
}

// routerPath 拼出 /admin/model-routers/{id}，与 credentials 等既有命令同口径：
// 在 execIDRequest 内 requireID 校验之前就会求值，未给 --id 时拼出 /0；
// 但那条路径不会发出请求（requireID 失败即返回退出码 2），故无害。
func routerPath(ctx *Context) string {
	return "/admin/model-routers/" + strconv.FormatUint(ctx.ID, 10)
}

// init 把 routers 命令表注册到全局注册表，供 cli.Run 分发。
func init() {
	Register("routers", routersCommands())
}
