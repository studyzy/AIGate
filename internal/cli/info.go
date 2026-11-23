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

// infoCommands 返回网关构建信息资源的命令：build-info 查询远端 GET /version。
// 该端点与 /healthz 一样免鉴权，使用 PlaneNone 不携带 Authorization 头；
// 响应体原样写 stdout，便于 `| jq`。多余参数经 execRequest 退出 2 不发请求。
func infoCommands() map[string]Command {
	return map[string]Command{
		"build-info": {
			Usage: "build-info",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneNone,
					Method: http.MethodGet,
					Path:   "/version",
					Body:   nil,
				})
			},
		},
	}
}

// init 把构建信息资源注册到全局注册表，供 cli.Run 分发。
func init() {
	Register("build-info", infoCommands())
}
