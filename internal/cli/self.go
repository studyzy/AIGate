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
	"fmt"
	"net/http"
	"strconv"

	"aigate/internal/client"
)

// selfLeaf 是子组下一级动作的执行函数；argv 为该动作剩余的 query flag 片段。
// 每个叶子都经 execRequest/execIDRequest 统一解析 flag、发请求，不自行判状态
// 或写输出。
type selfLeaf func(ctx *Context, argv []string) int

// runSelfSubgroup 取 argv[0] 作为子动作并在 leaves 中查找，命中则把 argv[1:]
// 透传给该叶子。未命中按本地错误处理（与 cli.Run 对未知命令一致），不发出
// 任何 HTTP 请求；这里不判 status、不映射业务退出码。
func runSelfSubgroup(ctx *Context, argv []string, group string, leaves map[string]selfLeaf) int {
	if len(argv) == 0 {
		return localError(ctx.Stderr, fmt.Errorf("missing %s action (see --help)", group))
	}
	leaf, ok := leaves[argv[0]]
	if !ok {
		return localError(ctx.Stderr, fmt.Errorf("unknown %s action %q (see --help)", group, argv[0]))
	}
	return leaf(ctx, argv[1:])
}

// selfUsageLeaves 返回 self usage 子组的 3 条动作。路径逐字取自 /api/user/*；
// query flag 集合取自 query-flags.md：summary 独有 --group-by，logs 独有
// --page/--page-size，by-model 只有过滤项，三者互不越界。全部经 execRequest
// 统一解析 flag（解析失败退出 2 不发请求）。
func selfUsageLeaves() map[string]selfLeaf {
	return map[string]selfLeaf{
		"summary": func(ctx *Context, argv []string) int {
			return execRequest(ctx, argv, client.Request{
				Plane:  client.PlaneUser,
				Method: http.MethodGet,
				Path:   "/api/user/usage/summary",
			}, "group-by", "start", "end", "user-id", "group-id", "api-key-id", "ai-model-id")
		},
		"logs": func(ctx *Context, argv []string) int {
			return execRequest(ctx, argv, client.Request{
				Plane:  client.PlaneUser,
				Method: http.MethodGet,
				Path:   "/api/user/usage/logs",
			}, "start", "end", "user-id", "group-id", "api-key-id", "ai-model-id", "page", "page-size")
		},
		"by-model": func(ctx *Context, argv []string) int {
			return execRequest(ctx, argv, client.Request{
				Plane:  client.PlaneUser,
				Method: http.MethodGet,
				Path:   "/api/user/usage/by-model",
			}, "start", "end", "user-id", "group-id", "api-key-id", "ai-model-id")
		},
	}
}

// selfKeysLeaves 返回 self keys 子组的 7 条动作。带 id 的动作经 execIDRequest
// 校验 --id 后把 ctx.ID 格式化进路径；create/update 以 ctx.Data 为请求体；
// 7 条均接受 --user-id，其中 usage 只额外接受 --start/--end（不暴露
// group-id / api-key-id / ai-model-id）。list/create 不带 id，走 execRequest。
func selfKeysLeaves() map[string]selfLeaf {
	return map[string]selfLeaf{
		"list": func(ctx *Context, argv []string) int {
			return execRequest(ctx, argv, client.Request{
				Plane:  client.PlaneUser,
				Method: http.MethodGet,
				Path:   "/api/user/keys",
			}, "user-id")
		},
		"get": func(ctx *Context, argv []string) int {
			return execIDRequest(ctx, argv, client.Request{
				Plane:  client.PlaneUser,
				Method: http.MethodGet,
				Path:   "/api/user/keys/" + strconv.FormatUint(ctx.ID, 10),
			}, "user-id")
		},
		"create": func(ctx *Context, argv []string) int {
			return execRequest(ctx, argv, client.Request{
				Plane:  client.PlaneUser,
				Method: http.MethodPost,
				Path:   "/api/user/keys",
				Body:   ctx.Data,
			}, "user-id")
		},
		"update": func(ctx *Context, argv []string) int {
			return execIDRequest(ctx, argv, client.Request{
				Plane:  client.PlaneUser,
				Method: http.MethodPut,
				Path:   "/api/user/keys/" + strconv.FormatUint(ctx.ID, 10),
				Body:   ctx.Data,
			}, "user-id")
		},
		"delete": func(ctx *Context, argv []string) int {
			return execIDRequest(ctx, argv, client.Request{
				Plane:  client.PlaneUser,
				Method: http.MethodDelete,
				Path:   "/api/user/keys/" + strconv.FormatUint(ctx.ID, 10),
			}, "user-id")
		},
		"rotate": func(ctx *Context, argv []string) int {
			return execIDRequest(ctx, argv, client.Request{
				Plane:  client.PlaneUser,
				Method: http.MethodPost,
				Path:   "/api/user/keys/" + strconv.FormatUint(ctx.ID, 10) + "/rotate",
			}, "user-id")
		},
		"usage": func(ctx *Context, argv []string) int {
			return execIDRequest(ctx, argv, client.Request{
				Plane:  client.PlaneUser,
				Method: http.MethodGet,
				Path:   "/api/user/keys/" + strconv.FormatUint(ctx.ID, 10) + "/usage",
			}, "start", "end", "user-id")
		},
	}
}

// selfCommands 返回 self 资源的 4 条命令：me / quota 为叶子命令，usage / keys
// 为子组命令（Run 内部再取一级动作）。全部走 client.PlaneUser；usage / keys
// 的 Usage 逐行列出子动作，供递归帮助层级 2 展示。
func selfCommands() map[string]Command {
	return map[string]Command{
		"me": {
			Usage: "self me",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneUser,
					Method: http.MethodGet,
					Path:   "/api/user/me",
				})
			},
		},
		"quota": {
			Usage: "self quota",
			Run: func(ctx *Context, argv []string) int {
				return execRequest(ctx, argv, client.Request{
					Plane:  client.PlaneUser,
					Method: http.MethodGet,
					Path:   "/api/user/quota",
				}, "user-id")
			},
		},
		"usage": {
			Usage: "aigatectl self usage <command>\n\nCommands:\n  summary\n  logs\n  by-model",
			Run: func(ctx *Context, argv []string) int {
				return runSelfSubgroup(ctx, argv, "self usage", selfUsageLeaves())
			},
		},
		"keys": {
			Usage: "aigatectl self keys <command>\n\nCommands:\n  list\n  get\n  create\n  update\n  delete\n  rotate\n  usage",
			Run: func(ctx *Context, argv []string) int {
				return runSelfSubgroup(ctx, argv, "self keys", selfKeysLeaves())
			},
		},
	}
}

// init 把 self 命令表注册到全局注册表，供 cli.Run 分发。
func init() {
	Register("self", selfCommands())
}
