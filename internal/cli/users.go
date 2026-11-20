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

// usersCommands 返回 users 资源的全部子命令。路径与管理面契约逐字取自
// docs/openapi.yaml；带 id 的命令把 ctx.ID 格式化进路径，create/update 以
// ctx.Data 作为请求体，其余命令不带请求体，全部经 Exec 统一编排退出码与输出。
// 服务端没有 GET /admin/users/{id}，因此不注册 get 命令。
func usersCommands() map[string]Command {
	return map[string]Command{
		"list": {
			Usage: "users list",
			Run: func(ctx *Context, _ []string) int {
				return Exec(ctx, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/users",
					Body:   nil,
				})
			},
		},
		"create": {
			Usage: "users create",
			Run: func(ctx *Context, _ []string) int {
				return Exec(ctx, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodPost,
					Path:   "/admin/users",
					Body:   ctx.Data,
				})
			},
		},
		"update": {
			Usage: "users update",
			Run: func(ctx *Context, _ []string) int {
				return Exec(ctx, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodPut,
					Path:   "/admin/users/" + strconv.FormatUint(ctx.ID, 10),
					Body:   ctx.Data,
				})
			},
		},
		"delete": {
			Usage: "users delete",
			Run: func(ctx *Context, _ []string) int {
				return Exec(ctx, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodDelete,
					Path:   "/admin/users/" + strconv.FormatUint(ctx.ID, 10),
					Body:   nil,
				})
			},
		},
		"rotate": {
			Usage: "users rotate",
			Run: func(ctx *Context, _ []string) int {
				return Exec(ctx, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodPost,
					Path:   "/admin/users/" + strconv.FormatUint(ctx.ID, 10) + "/rotate",
					Body:   nil,
				})
			},
		},
	}
}

// init 把 users 命令表注册到全局注册表，供 cli.Run 分发。
func init() {
	Register("users", usersCommands())
}
