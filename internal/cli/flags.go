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
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"

	"aigate/internal/client"
)

// queryValues 用独立的 FlagSet 解析命令自身的 query flag，把「已设置」的 flag
// 名由连字符换成下划线后写入 url.Values；值原样透传，不做业务校验。
// 解析出错（未知 flag、flag 缺值）或存在多余位置参数时返回 error，供调用方
// 统一映射为退出码 2 且不发请求；绝不静默吞掉错误或截断后续合法 flag。
func queryValues(argv []string, names ...string) (url.Values, error) {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	for _, name := range names {
		fs.String(name, "", "")
	}
	if err := fs.Parse(argv); err != nil {
		return nil, fmt.Errorf("invalid command flags: %w", err)
	}
	if rest := fs.Args(); len(rest) > 0 {
		return nil, fmt.Errorf("unexpected argument %q (see --help)", rest[0])
	}
	q := url.Values{}
	fs.Visit(func(f *flag.Flag) {
		q.Set(strings.ReplaceAll(f.Name, "-", "_"), f.Value.String())
	})
	return q, nil
}

// execRequest 是所有资源命令的统一出口：先解析命令自身的 query flag，任一
// 解析失败就写英文诊断并返回退出码 2、不发请求；成功则把 query 写入请求后
// 统一 Exec。names 为空时该命令不接受任何参数（覆盖无 query flag 的命令）。
func execRequest(ctx *Context, argv []string, req client.Request, names ...string) int {
	q, err := queryValues(argv, names...)
	if err != nil {
		return localError(ctx.Stderr, err)
	}
	req.Query = q
	return Exec(ctx, req)
}

// requireID 校验 --id 是否已显式给出。未给出时写英文诊断并返回 false，调用方
// 据此返回退出码 2，避免把零值 0 拼进资源路径造成 /.../0 的伪请求。
func requireID(ctx *Context) bool {
	if ctx.IDSet {
		return true
	}
	localError(ctx.Stderr, errors.New("missing required --id (see --help)"))
	return false
}

// execIDRequest 供带 id 的命令使用：先经 requireID 校验 --id，再走 execRequest
// 解析 query flag 并发请求；缺 --id 或 flag 解析失败都返回 2，且不发请求。
func execIDRequest(ctx *Context, argv []string, req client.Request, names ...string) int {
	if !requireID(ctx) {
		return exitLocalError
	}
	return execRequest(ctx, argv, req, names...)
}
