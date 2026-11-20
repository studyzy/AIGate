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

// importCommands 返回 import 资源的 1 条子命令。run 以 ctx.Data 作为请求体，
// 单条失败仍返回 HTTP 200，成败只由 HTTP 状态判定，响应体原样输出；多余参数
// 退出 2 不发请求，退出码与输出全部经 execRequest 统一编排。
func importCommands() map[string]Command {
	return map[string]Command{
		"run": {
			Usage: "import run",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodPost,
					Path:   "/admin/import",
					Body:   ctx.Data,
				})
			},
		},
	}
}

// init 把 import 命令表注册到全局注册表，供 cli.Run 分发。
func init() {
	Register("import", importCommands())
}
