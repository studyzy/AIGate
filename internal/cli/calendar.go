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

// calendarCommands 返回 calendar 资源的 3 条子命令，即计费日历（工作日/节假日）。
// list 暴露 --year query flag（YYYY，缺省时服务端按当前业务年度返回）；
// set 以 ctx.Data 作为请求体批量覆盖写；delete 的 --id 是 YYYY-MM-DD 日期字符串，
// 不是整数，故经 execPathRequest 原样拼进 /admin/billing-calendar/{date}。
func calendarCommands() map[string]Command {
	return map[string]Command{
		"list": {
			Usage: "calendar list",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/billing-calendar",
					Body:   nil,
				}, "year")
			},
		},
		"set": {
			Usage: "calendar set",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodPut,
					Path:   "/admin/billing-calendar",
					Body:   ctx.Data,
				})
			},
		},
		"delete": {
			Usage: "calendar delete",
			Run: func(ctx *Context, argv []string) int {
				return execPathRequest(ctx, argv, "/admin/billing-calendar/", client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodDelete,
					Body:   nil,
				})
			},
		},
	}
}

// init 把 calendar 命令表注册到全局注册表，供 cli.Run 分发；delete 的 --id 是
// 日期字符串，额外声明为字符串路径段，否则 Run 会在分发前把它当非法整数拒绝。
func init() {
	Register("calendar", calendarCommands())
	RegisterPathID("calendar", "delete")
}
