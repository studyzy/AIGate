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

// healthCommands 返回 health 资源的唯一子命令。按「默认 action」约定，action
// 名与 resource 同名，故 aigatectl health 单 token 即命中；/healthz 无需鉴权，
// 使用 PlaneNone 不携带 Authorization 头。version 由 cli.Run 本地处理，此处
// 不重复实现。多余参数经 execRequest 退出 2 不发请求。
func healthCommands() map[string]Command {
	return map[string]Command{
		"health": {
			Usage: "health",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneNone,
					Method: http.MethodGet,
					Path:   "/healthz",
					Body:   nil,
				})
			},
		},
	}
}

// init 把 health 命令表注册到全局注册表，供 cli.Run 分发。
func init() {
	Register("health", healthCommands())
}
