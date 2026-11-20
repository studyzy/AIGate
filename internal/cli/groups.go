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

// groupsCommands 返回 groups 资源的全部子命令（映射到 /admin/user-groups）。
// 路径与管理面契约逐字取自 docs/openapi.yaml；带 id 的命令经 execIDRequest
// 校验 --id 后把 ctx.ID 格式化进路径，create/update 以 ctx.Data 作为请求体，
// 其余命令不带请求体，全部经 execRequest 统一解析 flag、编排退出码与输出。
func groupsCommands() map[string]Command {
	return map[string]Command{
		"list": {
			Usage: "groups list",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/user-groups",
					Body:   nil,
				})
			},
		},
		"get": {
			Usage: "groups get",
			Run: func(ctx *Context, argv []string) int {
				return execIDRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/user-groups/" + strconv.FormatUint(ctx.ID, 10),
					Body:   nil,
				})
			},
		},
		"create": {
			Usage: "groups create",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodPost,
					Path:   "/admin/user-groups",
					Body:   ctx.Data,
				})
			},
		},
		"update": {
			Usage: "groups update",
			Run: func(ctx *Context, argv []string) int {
				return execIDRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodPut,
					Path:   "/admin/user-groups/" + strconv.FormatUint(ctx.ID, 10),
					Body:   ctx.Data,
				})
			},
		},
		"delete": {
			Usage: "groups delete",
			Run: func(ctx *Context, argv []string) int {
				return execIDRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodDelete,
					Path:   "/admin/user-groups/" + strconv.FormatUint(ctx.ID, 10),
					Body:   nil,
				})
			},
		},
	}
}

// init 把 groups 命令表注册到全局注册表，供 cli.Run 分发。
func init() {
	Register("groups", groupsCommands())
}
