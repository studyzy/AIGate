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

// pricesCommands 返回 prices 资源的 3 条子命令，即模型价目快照（六档单价 +
// 峰谷时段）。list/create 的 --id 是**模型** id，拼进 /admin/models/{id}/prices；
// delete 的 --id 是**价目** id，拼进 /admin/model-prices/{id}。create 以 ctx.Data
// 作为请求体、其余命令不带请求体，全部经 execRequest 统一解析 flag、编排退出码
// 与输出（多余参数退出 2 不发请求）。
func pricesCommands() map[string]Command {
	return map[string]Command{
		"list": {
			Usage: "prices list",
			Run: func(ctx *Context, argv []string) int {
				return execIDRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/models/" + strconv.FormatUint(ctx.ID, 10) + "/prices",
					Body:   nil,
				})
			},
		},
		"create": {
			Usage: "prices create",
			Run: func(ctx *Context, argv []string) int {
				return execIDRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodPost,
					Path:   "/admin/models/" + strconv.FormatUint(ctx.ID, 10) + "/prices",
					Body:   ctx.Data,
				})
			},
		},
		"delete": {
			Usage: "prices delete",
			Run: func(ctx *Context, argv []string) int {
				return execIDRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodDelete,
					Path:   "/admin/model-prices/" + strconv.FormatUint(ctx.ID, 10),
					Body:   nil,
				})
			},
		},
	}
}

// init 把 prices 命令表注册到全局注册表，供 cli.Run 分发。
func init() {
	Register("prices", pricesCommands())
}
