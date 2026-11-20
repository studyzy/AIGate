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

// Package cli 实现 aigatectl 的命令框架：全局 flag 解析、注册表 dispatch、
// 凭据装配与退出码编排。各资源命令在各自文件的 init() 中经 Register 注册，
// 通过 Exec 复用统一的「发请求 → 定退出码 → 写输出」语义；本包不感知业务路径。
//
// 分发约定：Run 只抽取四个全局 flag，用 <resource> <command> 定位命令，并把
// 二者之后剩余的参数原样交给 Command.Run 自行用 flag.NewFlagSet 解析。
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"aigate/internal/client"
)

// 本地错误与传输错误的退出码（其余 0/3/4 由 client.ExitCode 给出）。
const (
	exitLocalError = 2
	exitTransport  = 5
)

// defaultTimeout 是未指定 --timeout 时的请求超时。
const defaultTimeout = 30 * time.Second

// Version 是 CLI 版本号，与 cmd/aigate 的 Version 保持一致；
// 供 cmd/aigatectl 复用，避免两处硬编码。
const Version = "0.1.0"

// Context 携带一次命令执行所需的客户端、IO 与已解析的全局参数。
type Context struct {
	Client         *client.Client
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Data           []byte // --data 解析后的请求体；nil 表示无 body
	ID             uint64 // --id 解析后的正整数；0 表示命令未用
}

// Command 描述一条子命令：Usage 为用法说明，Run 自行解析 argv 中的查询 flag
// 并返回退出码（通常直接返回 Exec 的结果）。
type Command struct {
	Usage string
	Run   func(ctx *Context, argv []string) int
}

// registry 是 resource → command → Command 的全局注册表，由各资源文件 init() 填充。
var registry = map[string]map[string]Command{}

// Register 把某资源的命令表并入注册表；由各资源文件的 init() 调用。
func Register(resource string, cmds map[string]Command) {
	if registry[resource] == nil {
		registry[resource] = make(map[string]Command, len(cmds))
	}
	for name, cmd := range cmds {
		registry[resource][name] = cmd
	}
}

// ReadData 读取 --data 请求体：字面量、@文件、- 标准输入。
// 三种来源都做 json.Valid 校验，但原样返回字节（不重新序列化）。
func ReadData(spec string, stdin io.Reader) ([]byte, error) {
	var (
		data []byte
		err  error
	)
	switch {
	case spec == "-":
		if stdin == nil {
			return nil, errors.New("read --data from stdin: no input available")
		}
		data, err = io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("read --data from stdin: %w", err)
		}
	case strings.HasPrefix(spec, "@"):
		path := spec[1:]
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read --data file %q: %w", path, err)
		}
	default:
		data = []byte(spec)
	}
	if !json.Valid(data) {
		return nil, errors.New("invalid --data: body is not valid JSON")
	}
	return data, nil
}

// ParseID 把 --id 解析为正整数；0、负数、非数字、溢出都报错。
func ParseID(s string) (uint64, error) {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid --id %q: must be a positive integer: %w", s, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("invalid --id %q: must be a positive integer", s)
	}
	return n, nil
}

// Exec 发出一次请求并编排退出码与输出：
//   - 缺 token（client.ErrMissingToken）→ 2；
//   - 其它传输错误 → 5；
//   - 2xx → 响应体原样写 stdout，返回 0；
//   - 非 2xx → 只把 client.ErrorMessage 写 stderr，返回 client.ExitCode 的值。
//
// 各资源命令构造好 client.Request 后调用本函数，避免重复编排逻辑。
func Exec(ctx *Context, req client.Request) int {
	res, err := ctx.Client.Do(context.Background(), req)
	if err != nil {
		if errors.Is(err, client.ErrMissingToken) {
			fmt.Fprintln(ctx.Stderr, "aigatectl: missing token: set AIGATE_ADMIN_TOKEN or AIGATE_USER_TOKEN")
			return exitLocalError
		}
		fmt.Fprintln(ctx.Stderr, "aigatectl:", err)
		return exitTransport
	}
	code := client.ExitCode(res.StatusCode)
	if code == 0 {
		_, _ = ctx.Stdout.Write(res.Body)
		return 0
	}
	fmt.Fprintln(ctx.Stderr, client.ErrorMessage(res.Body))
	return code
}

// Run 是 CLI 入口：抽取全局 flag、装配客户端与 Context、dispatch 到注册表，
// 并把本地错误统一映射为退出码 2。
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	if hasHelp(args) {
		printHelp(stdout)
		return 0
	}

	g, argv, err := extractGlobals(args)
	if err != nil {
		return localError(stderr, err)
	}

	if len(argv) > 0 && argv[0] == "version" {
		fmt.Fprintf(stdout, "aigatectl %s\n", Version)
		return 0
	}

	baseURL := getenv("AIGATE_URL")
	if g.urlSet {
		baseURL = g.url
	}
	cfg := client.Config{
		BaseURL:    baseURL,
		AdminToken: getenv("AIGATE_ADMIN_TOKEN"),
		UserToken:  getenv("AIGATE_USER_TOKEN"),
		Timeout:    defaultTimeout,
	}
	if g.timeoutSet {
		d, err := time.ParseDuration(g.timeout)
		if err != nil {
			return localError(stderr, fmt.Errorf("invalid --timeout %q: %w", g.timeout, err))
		}
		cfg.Timeout = d
	}
	c, err := client.New(cfg)
	if err != nil {
		return localError(stderr, err)
	}

	var data []byte
	if g.dataSet {
		data, err = ReadData(g.data, stdin)
		if err != nil {
			return localError(stderr, err)
		}
	}
	var id uint64
	if g.idSet {
		id, err = ParseID(g.id)
		if err != nil {
			return localError(stderr, err)
		}
	}

	if len(argv) < 2 {
		return localError(stderr, errors.New("missing resource and command (see --help)"))
	}
	resource, name := argv[0], argv[1]
	cmds, ok := registry[resource]
	if !ok {
		return localError(stderr, fmt.Errorf("unknown resource %q (see --help)", resource))
	}
	cmd, ok := cmds[name]
	if !ok {
		return localError(stderr, fmt.Errorf("unknown command %q %q (see --help)", resource, name))
	}

	ctx := &Context{
		Client: c,
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Data:   data,
		ID:     id,
	}
	return cmd.Run(ctx, argv[2:])
}

// globals 是抽取出的四个全局 flag 及其「是否被指定」标记。
type globals struct {
	url, timeout, data, id string
	urlSet, timeoutSet     bool
	dataSet, idSet         bool
}

// extractGlobals 从 args 中摘出四个全局 flag（支持 --flag value 与 --flag=value），
// 其余 token 保持原顺序、原样放进 argv，供命令自行解析，不经过任何 FlagSet。
func extractGlobals(args []string) (globals, []string, error) {
	var g globals
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		name, val, inline := splitFlag(args[i])
		switch name {
		case "--url", "--timeout", "--data", "--id":
			if !inline {
				if i+1 >= len(args) {
					return g, nil, fmt.Errorf("missing value for %s", name)
				}
				i++
				val = args[i]
			}
			switch name {
			case "--url":
				g.url, g.urlSet = val, true
			case "--timeout":
				g.timeout, g.timeoutSet = val, true
			case "--data":
				g.data, g.dataSet = val, true
			case "--id":
				g.id, g.idSet = val, true
			}
		default:
			rest = append(rest, args[i])
		}
	}
	return g, rest, nil
}

// splitFlag 拆分 --name=value；无等号时 inline 为 false，由调用方取下一个 token。
func splitFlag(a string) (name, val string, inline bool) {
	if strings.HasPrefix(a, "--") {
		if i := strings.IndexByte(a, '='); i >= 0 {
			return a[:i], a[i+1:], true
		}
	}
	return a, "", false
}

// hasHelp 判断 args 中是否出现 --help / -h。
func hasHelp(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}

// localError 把本地错误写 stderr（英文诊断）并返回退出码 2。
func localError(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "aigatectl:", err)
	return exitLocalError
}

// printHelp 输出用法、全局 flag 与已注册资源名（凭据只从环境变量读取）。
func printHelp(w io.Writer) {
	fmt.Fprintln(w, "aigatectl - AIGate management CLI")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Usage: aigatectl [global flags] <resource> <command> [command flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Global flags:")
	fmt.Fprintln(w, "  --url <url>           base URL (overrides AIGATE_URL)")
	fmt.Fprintln(w, "  --timeout <duration>  request timeout, e.g. 30s")
	fmt.Fprintln(w, "  --data <json|@file|-> request body (validated as JSON, forwarded verbatim)")
	fmt.Fprintln(w, "  --id <positive int>   resource id")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Local commands: version, --help, -h")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Credentials come only from the environment:")
	fmt.Fprintln(w, "  AIGATE_URL, AIGATE_ADMIN_TOKEN, AIGATE_USER_TOKEN")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Resources:")
	for _, r := range resourceNames() {
		fmt.Fprintln(w, "  "+r)
	}
}

// resourceNames 返回排序后的资源名，保证 help 输出稳定。
func resourceNames() []string {
	names := make([]string, 0, len(registry))
	for r := range registry {
		names = append(names, r)
	}
	sort.Strings(names)
	return names
}
