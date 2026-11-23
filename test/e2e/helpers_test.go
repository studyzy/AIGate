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

//go:build e2e

package e2e

// 通用夹具：环境变量读取、数据库类型、JSON 编解码、HTTP 调用原语、轮询等待、
// 直连数据库断言，以及全部用例共享的常量与响应结构。

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"
)

const (
	envBinKey = "AIGATE_E2E_BIN"
	envDSNKey = "AIGATE_E2E_DSN"
	// envDBTypeKey 指定被测网关与直连断言的数据库类型：mysql（默认）或 sqlite。
	envDBTypeKey = "AIGATE_E2E_DB_TYPE"

	// dbTypeMySQL / dbTypeSQLite 与网关 -db-type 取值一致。
	dbTypeMySQL  = "mysql"
	dbTypeSQLite = "sqlite"

	adminToken     = "e2e-admin-token"
	upstreamAPIKey = "sk-upstream-x"

	// e2eMasterKey 是固定测试主密钥（64 位 hex，解码 32 字节）：
	// 全部 e2e 均在「加密开启」下回归，覆盖密钥类字段的落库密文与转发时实时解密。
	e2eMasterKey = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	// credSecretE2E 是 M3b 凭据中的 header 密钥；legacyTokenE2E 是 M3b 导入的存量明文 token。
	credSecretE2E  = "sk-cred-e2e"
	legacyTokenE2E = "sk-legacy-e2e"

	openAIModelID    = "mock-gpt"
	anthropicModelID = "mock-claude"

	openAIContent    = "hello from mock"
	anthropicContent = "hello from anthropic"

	// healthPollTimeout 是等待网关 /healthz 就绪的上限。
	healthPollTimeout = 20 * time.Second
	// usagePollTimeout 是等待用量异步落库的上限（设计 D7：用量为异步写入，需要轮询）。
	usagePollTimeout = 5 * time.Second
	// webhookPollTimeout 是等待 webhook 事件投递到达 mock 接收端的上限。
	webhookPollTimeout = 10 * time.Second
	// stopTimeout 是强杀网关子进程后等待 Wait 返回的兜底上限。
	stopTimeout = 5 * time.Second

	// gatewayPortMin / gatewayPortMax 是 e2e 网关进程允许使用的端口池（临时端口范围内）：
	// 每次都从池里随机取一个空闲端口，绝不使用 8080 等默认端口，避免与本机常驻服务冲突。
	gatewayPortMin = 20000
	gatewayPortMax = 60999
)

// e2eHTTPClient 是全部网关请求复用的客户端；超时覆盖连接 + 读取整个响应体。
var e2eHTTPClient = &http.Client{Timeout: 30 * time.Second}

func requireEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf(
			"skipping e2e: environment variable %s is not set (see test/e2e/README.md for prerequisites and how to run)",
			key,
		)
	}
	return v
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}
	return b
}

// dbType 返回本次 e2e 的数据库类型：未设置 AIGATE_E2E_DB_TYPE 时沿用历史默认的 mysql。
func dbType() string {
	if v := os.Getenv(envDBTypeKey); v != "" {
		return v
	}
	return dbTypeMySQL
}

// doRequest 发送一次网关 HTTP 请求；bearer 非空时附加 Authorization 头。
//
// 入口约定（详见 test/e2e/README.md 的「Entry points」）：
//   - 数据面 /v1/*：没有对应的 aigatectl 命令，只能用原生 HTTP；
//   - 管理面 /admin/* 与用户面 /api/user/* 的写操作：一律经 aigatectl（见 adminCLI/userCLI）；
//   - 查询与「故意失败」的契约断言：保留原生 HTTP，因为 CLI 只暴露退出码，
//     会丢掉状态码与错误体这两个被断言的对象。
func doRequest(t *testing.T, method, url, bearer string, body []byte) (int, []byte) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("failed to build request %s %s: %v", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := e2eHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("request %s %s failed: %v", method, url, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read %s %s response: %v", method, url, err)
	}
	return resp.StatusCode, respBody
}

// waitFor 轮询 probe 直到返回 nil 或超时；用于等待异步副作用（健康检查、用量落库）。
func waitFor(t *testing.T, desc string, timeout time.Duration, probe func() error) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		lastErr = probe()
		if lastErr == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: timed out after %s, last error: %v", desc, timeout, lastErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// postChatCompletion 向数据面发起一次对话请求，返回状态码与原始响应体。
func postChatCompletion(t *testing.T, baseURL, key string, payload map[string]any) (int, []byte) {
	t.Helper()
	return doRequest(t, http.MethodPost, baseURL+"/v1/chat/completions", key, mustJSON(t, payload))
}

// createModel 调用管理面创建模型，断言 201 + 响应含 id + 不回显 api_key 明文。
func createModel(t *testing.T, baseURL, adminAuth string, payload map[string]any) int64 {
	t.Helper()

	// 经 aigatectl 创建：请求体走临时文件，响应原样写回 stdout。
	code, out, errOut := runCLI(t, requireEnv(t, envCLIBinKey), baseURL, cliEnv{adminToken: adminAuth},
		"models", "create", "--data", "@"+cliDataFile(t, "model.json", payload))
	cliOK(t, "models create", code, out, errOut, nil)
	if bytes.Contains(out, []byte(upstreamAPIKey)) {
		t.Errorf("models create response echoed the api_key plaintext, response: %s", out)
	}

	var resp createModelResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("failed to parse models create response: %v, response: %s", err, out)
	}
	if resp.ID == 0 {
		t.Fatalf("models create response is missing id, response: %s", out)
	}
	return resp.ID
}

// openMySQL 打开直连 MySQL 的连接池（供断言用量落库）。
func openE2EDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()

	// 驱动名与网关自身保持一致：mysql 用 go-sql-driver/mysql，sqlite 用 modernc.org/sqlite。
	driver := dbTypeMySQL
	switch dbType() {
	case dbTypeMySQL:
	case dbTypeSQLite:
		driver = "sqlite"
	default:
		t.Fatalf("unsupported %s %q", envDBTypeKey, dbType())
	}

	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("failed to open %s connection (check %s): %v", driver, envDSNKey, err)
	}
	db.SetMaxOpenConns(2)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// m3bCount 统计表行数，用于导入预演/幂等的前后比对（表名为用例内固定字面量，需反引号避开保留字）。
func m3bCount(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM `" + table + "`").Scan(&n); err != nil {
		t.Fatalf("failed to count rows in %s: %v", table, err)
	}
	return n
}

type createModelResponse struct {
	ID int64 `json:"id"`
}

type createKeyResponse struct {
	ID               int64  `json:"id"`
	Token            string `json:"token"`
	ProxyTokenPrefix string `json:"proxy_token_prefix"`
	Bindings         []struct {
		AIModelID int64 `json:"ai_model_id"`
		HasKey    bool  `json:"has_key"`
	} `json:"bindings"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// m2UserResponse 是 POST /admin/users 响应中本用例需要的最小字段。
type m2UserResponse struct {
	ID    int64  `json:"id"`
	Token string `json:"token"`
}

// ---------------------------------------------------------------- CLI 调用原语
//
// 管理面与用户面的用例统一经真实二进制 aigatectl 驱动；数据面（/v1/*）没有 CLI 命令，
// 仍由 helpers_test.go 里的 doRequest/postChatCompletion 承担。

// aigatectl 的退出码契约（见 docs/CLI.md 与 internal/client.ExitCode）：
// 0 = 2xx 成功；3 = 鉴权失败（401/403）；4 = 其它远端错误（含 400 参数校验失败）；5 = 传输错误。
const (
	cliExitOK         = 0
	cliExitAuth       = 3
	cliExitRemoteFail = 4
	cliExitTransport  = 5
)

// cliEnv 是一次 CLI 调用注入的凭据：token 只经 cmd.Env 传递，绝不进 argv。

// runCLI 用真实二进制执行一次命令，分开捕获 stdout/stderr，返回退出码与两段输出。
// 凭据只从环境变量注入；每次调用前断言 argv 中不含任何已知 token。
func runCLI(t *testing.T, cliBin, baseURL string, creds cliEnv, args ...string) (int, []byte, []byte) {
	t.Helper()

	if creds.adminToken == "" {
		t.Fatal("runCLI: adminToken must not be empty (credentials come only from the environment)")
	}
	tokens := append([]string{creds.adminToken, creds.userToken}, creds.secrets...)
	for _, tok := range tokens {
		if tok == "" {
			continue
		}
		for _, a := range args {
			if strings.Contains(a, tok) {
				t.Fatalf("token leaked into CLI argv: %q appears in %v", tok, args)
			}
		}
	}

	cmd := exec.Command(cliBin, args...)
	cmd.Env = append(os.Environ(),
		"AIGATE_URL="+baseURL,
		"AIGATE_ADMIN_TOKEN="+creds.adminToken,
		"AIGATE_USER_TOKEN="+creds.userToken,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("failed to run %s %v: %v", cliBin, args, err)
		}
		code = exitErr.ExitCode()
	}
	return code, stdout.Bytes(), stderr.Bytes()
}

// cliOK 断言一次 CLI 调用成功：退出码 0、stderr 为空、stdout 为合法 JSON；
// dst 非 nil 时顺带把 stdout 反序列化进去。
func cliOK(t *testing.T, desc string, code int, stdout, stderr []byte, dst any) {
	t.Helper()
	if code != 0 {
		t.Fatalf("%s: exit code expected 0, got %d (stderr=%q, stdout=%q)", desc, code, stderr, stdout)
	}
	if len(stderr) != 0 {
		t.Fatalf("%s: stderr expected empty, got %q", desc, stderr)
	}
	if !json.Valid(stdout) {
		t.Fatalf("%s: stdout is not valid JSON: %q", desc, stdout)
	}
	if dst != nil {
		if err := json.Unmarshal(stdout, dst); err != nil {
			t.Fatalf("%s: failed to decode stdout JSON: %v, body=%q", desc, err, stdout)
		}
	}
}

// cliFailure 断言一次失败调用：退出码匹配、诊断只在 stderr、stdout 为空。
func cliFailure(t *testing.T, desc string, want, code int, stdout, stderr []byte) {
	t.Helper()
	if code != want {
		t.Fatalf("%s: exit code expected %d, got %d (stderr=%q, stdout=%q)", desc, want, code, stderr, stdout)
	}
	if len(stdout) != 0 {
		t.Errorf("%s: stdout expected empty on failure, got %q", desc, stdout)
	}
	if len(stderr) == 0 {
		t.Errorf("%s: stderr should carry the diagnostic, got nothing", desc)
	}
}

// cliDataFile 把请求体写入临时文件，返回供 --data @file 使用的路径。
// 含敏感字段（api_key / token 等）的请求体一律走文件，绝不进 argv。
func cliDataFile(t *testing.T, name string, payload any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, mustJSON(t, payload), 0o600); err != nil {
		t.Fatalf("failed to write CLI --data file: %v", err)
	}
	return path
}

// ---------------------------------------------------------------- 管理面 / 用户面 CLI 夹具
//
// 这些夹具是「用 CLI 准备数据」的唯一入口：上线后每个用例同时也在验证 aigatectl。

// cliBinPath 返回本次运行使用的 aigatectl 路径（未设置时跳过整个用例）。
func cliBinPath(t *testing.T) string {
	t.Helper()
	return requireEnv(t, envCLIBinKey)
}

// adminCLI 以管理员身份执行一次管理面命令，断言成功并解码 JSON 输出。
func adminCLI(t *testing.T, bin, baseURL string, dst any, args ...string) {
	t.Helper()
	code, out, errOut := runCLI(t, bin, baseURL, cliEnv{adminToken: adminToken}, args...)
	cliOK(t, strings.Join(args, " "), code, out, errOut, dst)
}

// adminCLIFail 以管理员身份执行一次预期失败的命令，返回退出码与两侧输出。
func adminCLIFail(t *testing.T, bin, baseURL string, args ...string) (int, []byte, []byte) {
	t.Helper()
	return runCLI(t, bin, baseURL, cliEnv{adminToken: adminToken}, args...)
}

// userCLI 以用户身份（AIGATE_USER_TOKEN）执行一次用户面命令。
func userCLI(t *testing.T, bin, baseURL, userToken string, dst any, args ...string) {
	t.Helper()
	code, out, errOut := runCLI(t, bin, baseURL, cliEnv{adminToken: adminToken, userToken: userToken}, args...)
	cliOK(t, strings.Join(args, " "), code, out, errOut, dst)
}

// createKey 经 aigatectl 创建密钥，返回响应（含一次性明文 token）。
func createKey(t *testing.T, baseURL string, payload map[string]any) createKeyResponse {
	t.Helper()
	var out createKeyResponse
	adminCLI(t, requireEnv(t, envCLIBinKey), baseURL, &out,
		"keys", "create", "--data", "@"+cliDataFile(t, "key.json", payload))
	return out
}

// createGroupWith 经 aigatectl 创建分组；extra 覆盖默认请求体字段（如全局配额规则）。
func createGroupWith(t *testing.T, baseURL, name string, extra map[string]any) int64 {
	t.Helper()
	payload := map[string]any{"name": name}
	for k, v := range extra {
		payload[k] = v
	}
	var out struct {
		ID int64 `json:"id"`
	}
	adminCLI(t, requireEnv(t, envCLIBinKey), baseURL, &out,
		"groups", "create", "--data", "@"+cliDataFile(t, "group.json", payload))
	if out.ID == 0 {
		t.Fatalf("groups create response is missing id for %q", name)
	}
	return out.ID
}

// createUser 经 aigatectl 创建用户并返回 id 与一次性访问 token（gk-…）。
func createUser(t *testing.T, baseURL, username string, groupID int64) m2UserResponse {
	t.Helper()
	payload := map[string]any{"username": username, "role": "user", "group_id": groupID}
	var u m2UserResponse
	adminCLI(t, requireEnv(t, envCLIBinKey), baseURL, &u,
		"users", "create", "--data", "@"+cliDataFile(t, "user.json", payload))
	if u.ID == 0 || u.Token == "" {
		t.Fatalf("users create response is missing id/token for %q", username)
	}
	return u
}

// adminQueryJSON 经 aigatectl 执行一次管理面查询并把 JSON 输出解码到 dst。
// 失败只返回错误、不调用 t.Fail，便于用在 waitFor 里轮询。
func adminQueryJSON(t *testing.T, baseURL string, dst any, args ...string) error {
	t.Helper()
	code, out, errOut := runCLI(t, requireEnv(t, envCLIBinKey), baseURL, cliEnv{adminToken: adminToken}, args...)
	if code != cliExitOK {
		return fmt.Errorf("aigatectl %v exited %d: %s", args, code, errOut)
	}
	if err := json.Unmarshal(out, dst); err != nil {
		return fmt.Errorf("decode aigatectl %v output: %w (body=%s)", args, err, out)
	}
	return nil
}

// listKeysRaw 经 aigatectl 拉取密钥列表并返回原始 JSON 字节（用于断言明文不回显）。
func listKeysRaw(t *testing.T, baseURL string) []byte {
	t.Helper()
	code, out, errOut := runCLI(t, requireEnv(t, envCLIBinKey), baseURL, cliEnv{adminToken: adminToken},
		"keys", "list")
	cliOK(t, "keys list", code, out, errOut, nil)
	return out
}

// createKeysRaw 经 aigatectl 创建密钥并返回原始 JSON 字节：
// 供「响应不回显明文 / 只暴露 has_key」这类字节级断言使用。
func createKeysRaw(t *testing.T, baseURL string, payload map[string]any) []byte {
	t.Helper()
	code, out, errOut := runCLI(t, requireEnv(t, envCLIBinKey), baseURL, cliEnv{adminToken: adminToken},
		"keys", "create", "--data", "@"+cliDataFile(t, "key.json", payload))
	cliOK(t, "keys create", code, out, errOut, nil)
	return out
}
