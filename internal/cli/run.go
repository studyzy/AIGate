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
	"bytes"
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
	"aigate/internal/version"
)

// 本地错误与传输错误的退出码（其余 0/3/4 由 client.ExitCode 给出）。
const (
	exitLocalError = 2
	exitTransport  = 5
)

// defaultTimeout 是未指定 --timeout 时的请求超时。
const defaultTimeout = 30 * time.Second

// DefaultBaseURL 是 AIGATE_URL 与 --url 都未指定时使用的网关根地址，
// 与服务端默认监听地址（:8080）及 docs/CLI.md 声明的默认值一致。
const DefaultBaseURL = "http://127.0.0.1:8080"

// Version 是 CLI 版本号，复用 internal/version 作为单一事实源；
// 供 cmd/aigatectl 复用，避免两处硬编码。
var Version = version.Version

// Context 携带一次命令执行所需的客户端、IO 与已解析的全局参数。
type Context struct {
	Client         *client.Client
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Data           []byte // --data 解析后的请求体；nil 表示无 body
	ID             uint64 // --id 解析后的正整数；0 表示命令未用
	IDSet          bool   // --id 是否被显式给出；带 id 命令据此拒绝 /.../0 伪请求
	IDRaw          string // --id 的原始文本；仅供路径段为字符串的命令（如日历日期）使用
	IDErr          error  // --id 不是正整数时的解析错误；带 id 命令据此本地报错
}

// Command 描述一条子命令：Usage 为用法说明，Run 自行解析 argv 中的查询 flag
// 并返回退出码（通常直接返回 Exec 的结果）。
type Command struct {
	Usage string
	Run   func(ctx *Context, argv []string) int
}

// registry 是 resource → command → Command 的全局注册表，由各资源文件 init() 填充。
var registry = map[string]map[string]Command{}

// pathIDRegistry 记录 --id 作为字符串路径段的命令（resource → command），由
// RegisterPathID 填充；Run 据此放行「非整数 --id」，其余命令仍在分发前拒绝。
var pathIDRegistry = map[string]map[string]bool{}

// RegisterPathID 声明某条命令的 --id 是字符串路径段而非正整数（如日历日期），
// 由该资源文件的 init() 在 Register 之后调用；只有这些命令接受非整数 --id。
func RegisterPathID(resource, command string) {
	if pathIDRegistry[resource] == nil {
		pathIDRegistry[resource] = make(map[string]bool, 1)
	}
	pathIDRegistry[resource][command] = true
}

// acceptsPathID 报告 resource command 是否声明了字符串 --id 路径段。
func acceptsPathID(resource, command string) bool {
	return pathIDRegistry[resource][command]
}

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
	if ok, path := helpTokens(args); ok {
		printHelpForPath(stdout, stderr, path)
		return 0
	}

	g, argv, err := extractGlobals(args)
	if err != nil {
		return localError(stderr, err)
	}

	var header string
	if len(argv) > 0 && argv[0] == "version" {
		if len(argv) > 1 && argv[1] == "--remote" {
			extra := argv[2:]
			for _, a := range extra {
				// `--remote` 是它的唯一 flag；`--` 与其后的位置参数不是 flag，
				// 一并本地报错，不转交给 build-info（那里 `--` 之后会被当成参数）。
				if a == "--" || !strings.HasPrefix(a, "-") {
					return localError(stderr, fmt.Errorf("unexpected argument %q (see --help)", a))
				}
			}
			// header 留到请求成功后再写：失败时 stdout 必须为空，诊断只进 stderr。
			header = fmt.Sprintf("%s (local binary; querying gateway at %s)\n",
				Version, ResolveBaseURL(getenv, g.url, g.urlSet))
			argv = append([]string{"build-info", "build-info"}, extra...)
		} else {
			fmt.Fprintf(stdout, "aigatectl %s (local binary)\n", Version)
			return 0
		}
	}

	cfg := client.Config{
		BaseURL:    ResolveBaseURL(getenv, g.url, g.urlSet),
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
	// --id 为整数是全局契约：非正整数在任何命令上都本地报错（分发前即返回 2）。
	// 唯一例外是路径段为字符串的命令（calendar delete --id 2026-10-01），它的
	// --id 本就不是整数；解析错误随 Context 下发，待分发到该命令后才被忽略。
	var id uint64
	var idErr error
	if g.idSet {
		id, idErr = ParseID(g.id)
	}

	// 单 token 默认 action 约定：resource 含同名 action 时（如 health），
	// aigatectl health 等价于 health health，argv[2:] 仍为空；其余单 token
	// 维持「缺 resource 与 command」的本地错误，不改变退出码契约。
	if len(argv) == 1 {
		if cmds, ok := registry[argv[0]]; ok {
			if _, ok := cmds[argv[0]]; ok {
				argv = append(argv, argv[0])
			}
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
	// --id 不是正整数时，只有声明了字符串路径段的命令可以继续（由 execPathRequest
	// 校验原始文本），其余命令保持「分发前拒绝」的既有契约。
	if idErr != nil && !acceptsPathID(resource, name) {
		return localError(stderr, idErr)
	}

	// header 只属于 version --remote：先本地解析 flag，非法参数仍退出 2 且不发请求；
	// 仅当请求成功时把 header 与响应体按顺序写入 stdout，失败时 stdout 保持为空。
	if header != "" {
		if _, err := queryValues(argv[2:]); err != nil {
			return localError(stderr, err)
		}
		var buf bytes.Buffer
		code := cmd.Run(&Context{
			Client: c, Stdin: stdin, Stdout: &buf, Stderr: stderr, Data: data,
			ID: id, IDSet: g.idSet, IDRaw: g.id, IDErr: idErr,
		}, argv[2:])
		if code == 0 {
			_, _ = io.WriteString(stdout, header)
			_, _ = stdout.Write(buf.Bytes())
		}
		return code
	}

	ctx := &Context{
		Client: c,
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Data:   data,
		ID:     id,
		IDSet:  g.idSet,
		IDRaw:  g.id,
		IDErr:  idErr,
	}
	return cmd.Run(ctx, argv[2:])
}

// ResolveBaseURL 按 --url > AIGATE_URL > DefaultBaseURL 的优先级选出网关根地址。
// 抽成纯函数便于表驱动测试：不读取进程环境、不发请求，getenv 由调用方注入。
// urlSet 为 true 时即便 urlFlag 为空也尊重用户显式给出的值（非法 URL 仍由
// client.New 本地报错退出 2），不回退到环境变量或默认值。
func ResolveBaseURL(getenv func(string) string, urlFlag string, urlSet bool) string {
	if urlSet {
		return urlFlag
	}
	if u := getenv("AIGATE_URL"); u != "" {
		return u
	}
	return DefaultBaseURL
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
		case "--":
			// `--` 之后的位置参数不再参与全局 flag 抽取，避免把用户数据
			// （例如紧跟其后的 --url）误当成全局 flag 吞掉。
			return g, append(rest, args[i+1:]...), nil
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

// helpTokens 判断 args 中是否含 --help / -h，并返回其之前的「位置 token 路径」：
// 四个全局 flag 及其取值被跳过，其余以 "-" 开头的 flag 也不计入路径，遇到
// help 标记立即停止。路径按层级解析帮助：0 个→顶层资源，1 个→该资源命令名，
// 2 个及以上→取前两级定位到具体命令并输出其 Usage。
func helpTokens(args []string) (bool, []string) {
	var path []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--help" || a == "-h" {
			return true, path
		}
		name, _, inline := splitFlag(a)
		switch name {
		case "--url", "--timeout", "--data", "--id":
			if !inline {
				i++ // 跳过该全局 flag 的取值
			}
		default:
			if !strings.HasPrefix(a, "-") {
				path = append(path, a)
			}
		}
	}
	return false, path
}

// printHelpForPath 按位置 token 路径分层输出帮助；层级 0 复用原 printHelp，
// 保证顶层资源列表与全局 flag 说明保持不变。
// 本地命令 version / health 在此特判：version --help 展示本地命令信息而非
// 「unknown resource」，health --help 展示 health 帮助而非冗余的 health health
// 列表；未知资源/命令的英文诊断写 stderr，绝不污染 stdout。
func printHelpForPath(stdout, stderr io.Writer, path []string) {
	if len(path) > 0 {
		switch path[0] {
		case "version":
			fmt.Fprintln(stdout, "aigatectl version - print this aigatectl binary's version (local command, no request)")
			fmt.Fprintln(stdout, "aigatectl version --remote - print the *gateway* build info via GET /version (same output as `build-info`)")
			return
		case "health":
			fmt.Fprintln(stdout, "aigatectl health - check gateway health via GET /healthz (no token required)")
			return
		}
	}
	switch {
	case len(path) == 0:
		printHelp(stdout)
	case len(path) == 1:
		printResourceHelp(stdout, stderr, path[0])
	default:
		printCommandHelp(stdout, stderr, path[0], path[1])
	}
}

// printResourceHelp 列出某资源已注册的命令名（排序）；未知资源的英文诊断写 stderr。
func printResourceHelp(stdout, stderr io.Writer, resource string) {
	cmds, ok := registry[resource]
	if !ok {
		fmt.Fprintf(stderr, "aigatectl: unknown resource %q (see --help)\n", resource)
		return
	}
	fmt.Fprintf(stdout, "aigatectl %s <command> [command flags]\n\n", resource)
	fmt.Fprintln(stdout, "Commands:")
	for _, name := range commandNames(cmds) {
		fmt.Fprintln(stdout, "  "+name)
	}
}

// printCommandHelp 输出某条命令的 Usage；self 子组命令的 Usage 逐行列出子动作。
// 未知命令的英文诊断写 stderr。
func printCommandHelp(stdout, stderr io.Writer, resource, name string) {
	cmd, ok := registry[resource][name]
	if !ok {
		fmt.Fprintf(stderr, "aigatectl: unknown command %q %q (see --help)\n", resource, name)
		return
	}
	fmt.Fprintln(stdout, cmd.Usage)
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
	fmt.Fprintln(w, "Local commands: version (aigatectl binary itself), --help, -h")
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

// commandNames 返回某资源下排序后的命令名，保证层级 1 help 输出稳定。
func commandNames(cmds map[string]Command) []string {
	names := make([]string, 0, len(cmds))
	for name := range cmds {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
