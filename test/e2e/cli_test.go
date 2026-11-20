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

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// envCLIBinKey 由 test/e2e/run.sh 导出，指向构建出的 bin/aigatectl。
const envCLIBinKey = "AIGATE_E2E_CLI_BIN"

// cliEnv 是一次 CLI 调用注入的凭据：token 只经 cmd.Env 传递，绝不进 argv。
// secrets 保存本组用例已知的全部明文 token，用于 argv 防泄漏断言。
type cliEnv struct {
	adminToken string
	userToken  string
	secrets    []string
}

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

// deadURL 返回一个当前确认没有进程监听的环回地址，用于触发传输错误（退出码 5）。
func deadURL(t *testing.T) string {
	t.Helper()
	for i := 0; i < 5; i++ {
		port := freePort(t)
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return "http://" + addr
		}
		_ = conn.Close()
	}
	t.Fatal("failed to find an unlistened loopback port")
	return ""
}

// ---------------------------------------------------------------------------
// CLI 闭环：真实 aigatectl 二进制 → 真实网关 → 真实 MySQL → 数据面
// ---------------------------------------------------------------------------

func TestE2E_CLI_ClosedLoop(t *testing.T) {
	cliBin := requireEnv(t, envCLIBinKey)
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	mock := newUpstreamMock(kindOpenAI)
	t.Cleanup(mock.Close)

	gw := startGateway(t, bin, dsn)
	db := openMySQL(t, dsn)
	waitFor(t, "MySQL reachable", usagePollTimeout, db.Ping)

	creds := cliEnv{adminToken: adminToken}
	const cliModelID = "cli-mock-gpt"

	var (
		modelRowID int64
		userRowID  int64
		userTok    string
		keyRowID   int64
		keyTok     string
	)

	// 1. health 是单 token 命令，无需凭据，响应体原样写 stdout。
	t.Run("1. health", func(t *testing.T) {
		code, out, errOut := runCLI(t, cliBin, gw.baseURL, creds, "health")
		var body struct {
			Status string `json:"status"`
		}
		cliOK(t, "health", code, out, errOut, &body)
		if body.Status != "ok" {
			t.Errorf("health status expected ok, got %q (stdout=%q)", body.Status, out)
		}
		if strings.TrimSpace(string(out)) != `{"status":"ok"}` {
			t.Errorf("health body expected verbatim {\"status\":\"ok\"}, got %q", out)
		}
	})

	// 2. models create --data @file：解析 id，并直连 MySQL 断言 ai_models 落库。
	t.Run("2. models create and the row exists in ai_models", func(t *testing.T) {
		file := cliDataFile(t, "model.json", map[string]any{
			"provider":   "mock",
			"model_id":   cliModelID,
			"model_name": cliModelID,
			"url":        mock.URL() + "/v1",
			"api_key":    upstreamAPIKey,
			"model_type": "openai-completions",
			"quota_day":  -1,
			"enabled":    true,
		})
		code, out, errOut := runCLI(t, cliBin, gw.baseURL, creds, "models", "create", "--data", "@"+file)
		var created createModelResponse
		cliOK(t, "models create", code, out, errOut, &created)
		if created.ID == 0 {
			t.Fatalf("models create response is missing id: %q", out)
		}
		modelRowID = created.ID

		var stored string
		if err := db.QueryRow(`SELECT model_id FROM ai_models WHERE id = ?`, modelRowID).Scan(&stored); err != nil {
			t.Fatalf("failed to read ai_models row %d directly: %v", modelRowID, err)
		}
		if stored != cliModelID {
			t.Errorf("ai_models.model_id expected %q, got %q", cliModelID, stored)
		}
	})

	// 3. models list / models get 都能看到刚创建的模型。
	t.Run("3. models list / get show the created model", func(t *testing.T) {
		code, out, errOut := runCLI(t, cliBin, gw.baseURL, creds, "models", "list")
		cliOK(t, "models list", code, out, errOut, nil)
		if !bytes.Contains(out, []byte(cliModelID)) {
			t.Errorf("models list should contain %q, got %q", cliModelID, out)
		}

		code, out, errOut = runCLI(t, cliBin, gw.baseURL, creds, "models", "get", "--id", strconv.FormatInt(modelRowID, 10))
		var detail struct {
			ID      int64  `json:"id"`
			ModelID string `json:"model_id"`
		}
		cliOK(t, "models get", code, out, errOut, &detail)
		if detail.ID != modelRowID || detail.ModelID != cliModelID {
			t.Errorf("models get returned unexpected detail: %+v", detail)
		}
	})

	// 4. users create 返回一次性 gk- 访问 token（服务端没有 users get）。
	t.Run("4. users create returns a one-time gk- token", func(t *testing.T) {
		file := cliDataFile(t, "user.json", map[string]any{
			"username": "cli-closedloop-user",
			"role":     "user",
		})
		code, out, errOut := runCLI(t, cliBin, gw.baseURL, creds, "users", "create", "--data", "@"+file)
		var created struct {
			ID    int64  `json:"id"`
			Token string `json:"token"`
		}
		cliOK(t, "users create", code, out, errOut, &created)
		if created.ID == 0 || !strings.HasPrefix(created.Token, "gk-") {
			t.Fatalf("users create should return id and a gk- token, got %+v (stdout=%q)", created, out)
		}
		userRowID, userTok = created.ID, created.Token
		creds.secrets = append(creds.secrets, userTok)
	})

	// 5. keys create 把该模型与该用户绑定，返回一次性 sk- 数据面凭据。
	t.Run("5. keys create binds the model and the user", func(t *testing.T) {
		file := cliDataFile(t, "key.json", map[string]any{
			"name":    "cli-closedloop-key",
			"user_id": userRowID,
			"bindings": []map[string]any{
				{"ai_model_id": modelRowID, "role": "primary"},
			},
		})
		code, out, errOut := runCLI(t, cliBin, gw.baseURL, creds, "keys", "create", "--data", "@"+file)
		var created createKeyResponse
		cliOK(t, "keys create", code, out, errOut, &created)
		if created.ID == 0 || !strings.HasPrefix(created.Token, "sk-") {
			t.Fatalf("keys create should return id and an sk- token, got %+v (stdout=%q)", created, out)
		}
		if len(created.Bindings) != 1 || created.Bindings[0].AIModelID != modelRowID {
			t.Errorf("keys create should bind ai_model_id=%d, got %+v", modelRowID, created.Bindings)
		}
		keyRowID, keyTok = created.ID, created.Token
		creds.secrets = append(creds.secrets, keyTok)
	})

	// 6. 关键断言：CLI 签发的 sk- 凭据真的能驱动数据面，且用量落到 MySQL。
	t.Run("6. the CLI-issued sk- token drives the data plane and usage lands in MySQL", func(t *testing.T) {
		status, body := postChatCompletion(t, gw.baseURL, keyTok, map[string]any{
			"model":    cliModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("POST /v1/chat/completions with the CLI-issued key expected 200, got %d, response: %s", status, body)
		}
		var chat chatCompletionResponse
		if err := json.Unmarshal(body, &chat); err != nil || len(chat.Choices) == 0 ||
			chat.Choices[0].Message.Content != openAIContent {
			t.Fatalf("unexpected chat completion body: %q (err=%v)", body, err)
		}

		waitFor(
			t,
			fmt.Sprintf("usage of api_key_id=%d lands in llm_usage_logs", keyRowID),
			usagePollTimeout,
			func() error {
				var rows int
				if err := db.QueryRow(
					`SELECT COUNT(*) FROM llm_usage_logs WHERE api_key_id = ? AND total_tokens > 0`, keyRowID,
				).Scan(&rows); err != nil {
					return err
				}
				if rows < 1 {
					return fmt.Errorf("currently %d rows for api_key_id=%d, expected >= 1", rows, keyRowID)
				}
				return nil
			},
		)
	})

	// 7. self me / self keys list（三 token 路径）只返回该用户自己的数据。
	t.Run("7. self me / self keys list expose only that user's data", func(t *testing.T) {
		userCreds := cliEnv{adminToken: adminToken, userToken: userTok, secrets: creds.secrets}

		code, out, errOut := runCLI(t, cliBin, gw.baseURL, userCreds, "self", "me")
		var me struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		}
		cliOK(t, "self me", code, out, errOut, &me)
		if me.ID != userRowID {
			t.Errorf("self me id expected %d, got %d (stdout=%q)", userRowID, me.ID, out)
		}

		code, out, errOut = runCLI(t, cliBin, gw.baseURL, userCreds, "self", "keys", "list")
		var list struct {
			Keys []struct {
				ID     int64 `json:"id"`
				UserID int64 `json:"user_id"`
			} `json:"keys"`
		}
		cliOK(t, "self keys list", code, out, errOut, &list)
		if len(list.Keys) == 0 {
			t.Fatalf("self keys list should contain the key created in step 5, got %q", out)
		}
		found := false
		for i, k := range list.Keys {
			if k.UserID != userRowID {
				t.Errorf("self keys list entry[%d] belongs to another user: %+v", i, k)
			}
			if k.ID == keyRowID {
				found = true
			}
		}
		if !found {
			t.Errorf("self keys list should contain key id=%d, got %q", keyRowID, out)
		}
	})

	// 8. keys rotate 换发新凭据后，旧 sk- 凭据被数据面拒绝（401）。
	t.Run("8. keys rotate invalidates the old sk- token on the data plane", func(t *testing.T) {
		code, out, errOut := runCLI(t, cliBin, gw.baseURL, creds, "keys", "rotate", "--id", strconv.FormatInt(keyRowID, 10))
		var rotated struct {
			ID    int64  `json:"id"`
			Token string `json:"token"`
		}
		cliOK(t, "keys rotate", code, out, errOut, &rotated)
		if rotated.ID != keyRowID || !strings.HasPrefix(rotated.Token, "sk-") || rotated.Token == keyTok {
			t.Fatalf("keys rotate should return a fresh sk- token, got %+v", rotated)
		}
		creds.secrets = append(creds.secrets, rotated.Token)

		status, body := postChatCompletion(t, gw.baseURL, keyTok, map[string]any{
			"model":    cliModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusUnauthorized {
			t.Fatalf("the rotated-away sk- token expected 401 on the data plane, got %d, response: %s", status, body)
		}
	})
}

// ---------------------------------------------------------------------------
// 退出码契约：0 / 2 / 3 / 4 / 5 各一次，错误只在 stderr
// ---------------------------------------------------------------------------

func TestE2E_CLI_ExitCodes(t *testing.T) {
	cliBin := requireEnv(t, envCLIBinKey)
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	gw := startGateway(t, bin, dsn)
	admin := cliEnv{adminToken: adminToken}

	t.Run("0 success", func(t *testing.T) {
		code, out, errOut := runCLI(t, cliBin, gw.baseURL, admin, "health")
		cliOK(t, "health", code, out, errOut, nil)
	})

	t.Run("2 invalid --id", func(t *testing.T) {
		code, out, errOut := runCLI(t, cliBin, gw.baseURL, admin, "models", "get", "--id", "abc")
		cliFailure(t, "invalid --id", 2, code, out, errOut)
	})

	t.Run("3 wrong admin token", func(t *testing.T) {
		wrong := cliEnv{adminToken: "e2e-wrong-admin-token"}
		code, out, errOut := runCLI(t, cliBin, gw.baseURL, wrong, "models", "list")
		cliFailure(t, "wrong admin token", 3, code, out, errOut)
	})

	t.Run("4 remote 404", func(t *testing.T) {
		code, out, errOut := runCLI(t, cliBin, gw.baseURL, admin, "models", "get", "--id", "999999")
		cliFailure(t, "models get missing id", 4, code, out, errOut)
	})

	t.Run("5 transport error", func(t *testing.T) {
		code, out, errOut := runCLI(t, cliBin, gw.baseURL, admin, "health", "--url", deadURL(t))
		cliFailure(t, "unlistened --url", 5, code, out, errOut)
	})
}

// ---------------------------------------------------------------------------
// 特殊语义：HTTP 200 但业务失败 / 非 JSON 响应体，均按状态判定为 0
// ---------------------------------------------------------------------------

func TestE2E_CLI_SpecialSemantics(t *testing.T) {
	cliBin := requireEnv(t, envCLIBinKey)
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	gw := startGateway(t, bin, dsn)
	admin := cliEnv{adminToken: adminToken}

	t.Run("connectivity ok=false is still HTTP 200 -> exit 0 with body verbatim", func(t *testing.T) {
		file := cliDataFile(t, "down-model.json", map[string]any{
			"provider":   "mock",
			"model_id":   "cli-down-model",
			"model_name": "cli-down-model",
			"url":        "http://127.0.0.1:1/v1",
			"api_key":    upstreamAPIKey,
			"model_type": "openai-completions",
			"quota_day":  -1,
			"enabled":    true,
		})
		code, out, errOut := runCLI(t, cliBin, gw.baseURL, admin, "models", "create", "--data", "@"+file)
		var created createModelResponse
		cliOK(t, "models create (unreachable)", code, out, errOut, &created)
		if created.ID == 0 {
			t.Fatalf("models create response is missing id: %q", out)
		}

		code, out, errOut = runCLI(t, cliBin, gw.baseURL, admin, "models", "connectivity", "--id", strconv.FormatInt(created.ID, 10))
		var probe connectivityResult
		cliOK(t, "models connectivity", code, out, errOut, &probe)
		if probe.OK {
			t.Errorf("unreachable model expected ok=false, got %+v (stdout=%q)", probe, out)
		}
		if !bytes.Contains(out, []byte(`"ok":false`)) {
			t.Errorf("connectivity body should be printed verbatim with ok:false, got %q", out)
		}
	})

	t.Run("import run partial failure is still HTTP 200 -> exit 0", func(t *testing.T) {
		file := cliDataFile(t, "import.json", map[string]any{
			"dry_run": true,
			"users": []map[string]any{
				{"username": "cli-import-ok", "role": "user"},
				{"username": "", "role": "user"},
			},
		})
		code, out, errOut := runCLI(t, cliBin, gw.baseURL, admin, "import", "run", "--data", "@"+file)
		var resp struct {
			DryRun  bool `json:"dry_run"`
			Summary struct {
				Created int `json:"created"`
				Errors  int `json:"errors"`
			} `json:"summary"`
			Details []struct {
				Action string `json:"action"`
			} `json:"details"`
		}
		cliOK(t, "import run", code, out, errOut, &resp)
		if !resp.DryRun {
			t.Errorf("import run should echo dry_run=true, got %+v", resp)
		}
		if resp.Summary.Errors < 1 {
			t.Errorf("import run summary.errors expected >= 1, got %+v (stdout=%q)", resp.Summary, out)
		}
		errDetails := 0
		for _, d := range resp.Details {
			if d.Action == "error" {
				errDetails++
			}
		}
		if errDetails == 0 {
			t.Fatalf("import run should carry a details[].action=error entry, got %q", out)
		}
	})

	t.Run("usage export --format csv prints raw CSV text (never JSON-decoded)", func(t *testing.T) {
		code, out, errOut := runCLI(t, cliBin, gw.baseURL, admin, "usage", "export", "--format", "csv")
		if code != 0 {
			t.Fatalf("usage export --format csv expected exit 0, got %d (stderr=%q, stdout=%q)", code, errOut, out)
		}
		if len(errOut) != 0 {
			t.Fatalf("usage export --format csv stderr expected empty, got %q", errOut)
		}
		if json.Valid(out) {
			t.Errorf("CSV stdout must not be valid JSON (the CLI must not decode/re-serialize it): %q", out)
		}
		if !strings.HasPrefix(string(out), "id,api_key_id,user_id,group_id,") {
			t.Errorf("usage export --format csv should print the raw CSV header verbatim, got %q", out)
		}
		if !bytes.Contains(out, []byte("\n")) {
			t.Errorf("CSV stdout should contain a line terminator, got %q", out)
		}

		// 响应体原样输出：与直连同一端点得到的字节完全一致。
		status, direct := doRequest(t, http.MethodGet, gw.baseURL+"/admin/usage/export?format=csv", adminToken, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /admin/usage/export?format=csv expected 200, got %d, response: %s", status, direct)
		}
		if !bytes.Equal(out, direct) {
			t.Errorf("CLI should print the response body verbatim: CLI=%q direct=%q", out, direct)
		}
	})
}
