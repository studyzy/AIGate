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

// ratesCommands 返回 rates 资源的 3 条子命令，即两币种之间的汇率快照。
// list 按 docs/openapi.yaml 暴露 --from / --to 两个 query flag（服务端两个都是
// 必填，缺省时的报错交给服务端）；create 以 ctx.Data 作为请求体；delete 经
// execIDRequest 校验 --id。全部经 execRequest 统一解析 flag、编排退出码与输出。
func ratesCommands() map[string]Command {
	return map[string]Command{
		"list": {
			Usage: "rates list",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/currency-rates",
					Body:   nil,
				}, "from", "to")
			},
		},
		"create": {
			Usage: "rates create",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodPost,
					Path:   "/admin/currency-rates",
					Body:   ctx.Data,
				})
			},
		},
		"delete": {
			Usage: "rates delete",
			Run: func(ctx *Context, argv []string) int {
				return execIDRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodDelete,
					Path:   "/admin/currency-rates/" + strconv.FormatUint(ctx.ID, 10),
					Body:   nil,
				})
			},
		},
	}
}

// init 把 rates 命令表注册到全局注册表，供 cli.Run 分发。
func init() {
	Register("rates", ratesCommands())
}
