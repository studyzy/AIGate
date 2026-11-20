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
	"flag"
	"io"
	"net/http"
	"net/url"
	"strings"

	"aigate/internal/client"
)

// queryValues 用独立的 FlagSet 解析命令自己的 query flag，把「已设置」的 flag
// 名由连字符换成下划线后经 Query.Set 写入 url.Values；值原样透传，不做业务校验。
// 解析错误与用法输出一律忽略，退出码与输出仍全部由 Exec 统一编排。
func queryValues(argv []string, names ...string) url.Values {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	for _, name := range names {
		fs.String(name, "", "")
	}
	_ = fs.Parse(argv)
	q := url.Values{}
	fs.Visit(func(f *flag.Flag) {
		q.Set(strings.ReplaceAll(f.Name, "-", "_"), f.Value.String())
	})
	return q
}

// usageCommands 返回 usage 资源的 3 条子命令。三者的 query flag 集合逐字取自
// docs/openapi.yaml（见 query-flags.md）：--group-by 只在 summary，--page/
// --page-size 只在 logs，--format 只在 export；路径取自管理面契约，全部经
// Exec 统一编排退出码与输出。
func usageCommands() map[string]Command {
	return map[string]Command{
		"summary": {
			Usage: "usage summary",
			Run: func(ctx *Context, argv []string) int {
				return Exec(ctx, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/usage/summary",
					Query:  queryValues(argv, "group-by", "start", "end", "user-id", "group-id", "api-key-id", "ai-model-id"),
				})
			},
		},
		"logs": {
			Usage: "usage logs",
			Run: func(ctx *Context, argv []string) int {
				return Exec(ctx, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/usage/logs",
					Query:  queryValues(argv, "start", "end", "user-id", "group-id", "api-key-id", "ai-model-id", "page", "page-size"),
				})
			},
		},
		"export": {
			Usage: "usage export",
			Run: func(ctx *Context, argv []string) int {
				return Exec(ctx, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/usage/export",
					Query:  queryValues(argv, "format", "start", "end", "user-id", "group-id", "api-key-id", "ai-model-id"),
				})
			},
		},
	}
}

// init 把 usage 命令表注册到全局注册表，供 cli.Run 分发。
func init() {
	Register("usage", usageCommands())
}
