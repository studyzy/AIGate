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

// modelsCommands 返回 models 资源的全部子命令。路径与管理面契约逐字取自
// docs/openapi.yaml；带 id 的命令经 execIDRequest 校验 --id 后把 ctx.ID 格式化
// 进路径，create/update 以 ctx.Data 作为请求体，其余命令不带请求体；全部经
// execRequest 统一解析 flag、编排退出码与输出。
func modelsCommands() map[string]Command {
	return map[string]Command{
		"list": {
			Usage: "models list",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/models",
					Body:   nil,
				})
			},
		},
		"get": {
			Usage: "models get",
			Run: func(ctx *Context, argv []string) int {
				return execIDRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/models/" + strconv.FormatUint(ctx.ID, 10),
					Body:   nil,
				})
			},
		},
		"create": {
			Usage: "models create",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodPost,
					Path:   "/admin/models",
					Body:   ctx.Data,
				})
			},
		},
		"update": {
			Usage: "models update",
			Run: func(ctx *Context, argv []string) int {
				return execIDRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodPut,
					Path:   "/admin/models/" + strconv.FormatUint(ctx.ID, 10),
					Body:   ctx.Data,
				})
			},
		},
		"delete": {
			Usage: "models delete",
			Run: func(ctx *Context, argv []string) int {
				return execIDRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodDelete,
					Path:   "/admin/models/" + strconv.FormatUint(ctx.ID, 10),
					Body:   nil,
				})
			},
		},
		"connectivity": {
			Usage: "models connectivity",
			Run: func(ctx *Context, argv []string) int {
				return execIDRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodPost,
					Path:   "/admin/models/" + strconv.FormatUint(ctx.ID, 10) + "/connectivity",
					Body:   nil,
				})
			},
		},
	}
}

// init 把 models 命令表注册到全局注册表，供 cli.Run 分发。
func init() {
	Register("models", modelsCommands())
}
