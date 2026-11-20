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

// settingsCommands 返回 settings 资源的 2 条子命令。get 无请求体，
// update 以 ctx.Data 作为请求体；二者都不带 query flag，全部经 execRequest
// 统一解析 flag、编排退出码与输出（多余参数退出 2 不发请求）。
func settingsCommands() map[string]Command {
	return map[string]Command{
		"get": {
			Usage: "settings get",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/settings",
					Body:   nil,
				})
			},
		},
		"update": {
			Usage: "settings update",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodPut,
					Path:   "/admin/settings",
					Body:   ctx.Data,
				})
			},
		},
	}
}

// init 把 settings 命令表注册到全局注册表，供 cli.Run 分发。
func init() {
	Register("settings", settingsCommands())
}
