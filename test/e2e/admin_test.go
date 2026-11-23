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

// 管理面用例：模型、凭据、导入、密钥 TTL、配额分组等。当前经真实 HTTP 走 /admin/*，
// 第二阶段统一改为 aigatectl；数据面转发与落库断言仍然真实执行。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// m2PostChatWithHeader 发起非流式对话请求并返回（状态码, 响应头, 响应体）。
func m2PostChatWithHeader(t *testing.T, baseURL, key string, payload map[string]any) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(mustJSON(t, payload)))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e2eHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("request %s failed: %v", req.URL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	return resp.StatusCode, resp.Header, body
}

// connectivityResult 是连通性探测端点（POST /admin/models/{id}/connectivity）的响应。
type connectivityResult struct {
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	ErrorKind string `json:"error_kind"`
	Message   string `json:"message"`
}

// importSummaryResponse 是 POST /admin/import 响应中本用例需要的最小字段。
type importSummaryResponse struct {
	DryRun  bool `json:"dry_run"`
	Summary struct {
		Created int `json:"created"`
		Skipped int `json:"skipped"`
		Errors  int `json:"errors"`
	} `json:"summary"`
}

// selfKeyListResponse 是 GET /api/user/keys 的响应。
type selfKeyListResponse struct {
	Keys []struct {
		ID     int64 `json:"id"`
		UserID int64 `json:"user_id"`
	} `json:"keys"`
}

// m3bImportPayload 构造 M3b 的导入载荷：新分组 + 新用户 + 新模型 + 引用该模型的新密钥。
func m3bImportPayload(dryRun bool, upstreamURL string) map[string]any {
	return map[string]any{
		"dry_run": dryRun,
		"user_groups": []map[string]any{
			{"name": "m3b-g", "token_quota_rules": "[]"},
		},
		"users": []map[string]any{
			{"username": "m3b-u", "role": "user", "group": "m3b-g"},
		},
		"models": []map[string]any{
			{
				"provider": "mock", "model_id": "m3b-legacy", "model_name": "m3b-legacy",
				"url": upstreamURL + "/v1", "api_key": "sk-legacy-upstream",
				"model_type": "openai-completions", "quota_day": -1, "enabled": true,
			},
		},
		"keys": []map[string]any{
			{
				"name": "m3b-legacy-key", "user": "m3b-u", "group": "m3b-g", "max_tokens": 0,
				"token":    legacyTokenE2E,
				"bindings": []map[string]any{{"model": "m3b-legacy", "role": "primary"}},
			},
		},
	}
}

func TestE2E_Admin_QuotaAndUsage(t *testing.T) {
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	mock := newUpstreamMock(kindOpenAI)
	t.Cleanup(mock.Close)

	gw := startGateway(t, bin, dsn)
	adminAuth := adminToken

	modelID := createModel(t, gw.baseURL, adminAuth, map[string]any{
		"provider":   "mock",
		"model_id":   openAIModelID,
		"model_name": openAIModelID,
		"url":        mock.URL() + "/v1",
		"api_key":    upstreamAPIKey,
		"model_type": "openai-completions",
		"quota_day":  -1,
		"enabled":    true,
	})

	var (
		userID   int64
		userTok  string
		limitedG int64
		unlimG   int64
	)

	t.Run("1. create the user and the groups (limited day=5 / explicitly unlimited)", func(t *testing.T) {
		limitedG = m2CreateGroup(t, gw.baseURL, adminAuth, "m2-limited", `[{"mode":"day","limit":5}]`)
		unlimG = m2CreateGroup(t, gw.baseURL, adminAuth, "m2-unlimited", `[]`)

		u := createUser(t, gw.baseURL, "m2-user", limitedG)
		userID, userTok = u.ID, u.Token
		t.Logf("user id=%d, groups limited=%d / unlimited=%d", userID, limitedG, unlimG)
	})

	t.Run(
		"2. limited-group key: the first request is allowed, exceeding the quota returns 429 (with Retry-After)",
		func(t *testing.T) {
			_, key := m2CreateKey(t, gw.baseURL, adminAuth, userID, limitedG, modelID)

			status, _, body := m2PostChatWithHeader(t, gw.baseURL, key, map[string]any{
				"model":    openAIModelID,
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			})
			if status != http.StatusOK {
				t.Fatalf("the first request from the limited group expected 200, got %d, response: %s", status, body)
			}

			// 日限 5，本次即将记入 18 tokens；等待异步落库后再发第二次。
			waitFor(t, "the limited-group usage is persisted", usagePollTimeout, func() error {
				var resp struct {
					Total int64 `json:"total"`
				}
				if err := adminQueryJSON(t, gw.baseURL, &resp, "usage", "logs", "--group-id", strconv.FormatInt(limitedG, 10)); err != nil {
					return err
				}
				if resp.Total < 1 {
					return fmt.Errorf("the usage of this group is not persisted yet (total=%d)", resp.Total)
				}
				return nil
			})

			status, header, body := m2PostChatWithHeader(t, gw.baseURL, key, map[string]any{
				"model":    openAIModelID,
				"messages": []map[string]string{{"role": "user", "content": "hi again"}},
			})
			if status != http.StatusTooManyRequests {
				t.Fatalf("the exceeding request expected 429, got %d, response: %s", status, body)
			}
			if !bytes.Contains(body, []byte("User token quota exceeded")) {
				t.Errorf("the 429 response should contain %q, response: %s", "User token quota exceeded", body)
			}
			var secs int
			ra := header.Get("Retry-After")
			if _, err := fmt.Sscanf(ra, "%d", &secs); err != nil || secs <= 0 {
				t.Errorf("Retry-After should be a positive number of seconds, got %q (err=%v)", ra, err)
			}
		},
	)

	t.Run("3. keys in the explicitly unlimited group are not affected by the policy", func(t *testing.T) {
		_, key := m2CreateKey(t, gw.baseURL, adminAuth, userID, unlimG, modelID)
		status, _, body := m2PostChatWithHeader(t, gw.baseURL, key, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("the request from the explicitly unlimited group expected 200, got %d, response: %s", status, body)
		}
	})

	t.Run("4. admin usage queries (summary / logs)", func(t *testing.T) {
		waitFor(t, "all usage is persisted", usagePollTimeout, func() error {
			st, b := doRequest(t, http.MethodGet, gw.baseURL+"/admin/usage/logs?page_size=50", adminAuth, nil)
			if st != http.StatusOK {
				return fmt.Errorf("GET /admin/usage/logs returned %d: %s", st, b)
			}
			var resp struct {
				Total int64 `json:"total"`
			}
			if err := json.Unmarshal(b, &resp); err != nil {
				return err
			}
			if resp.Total < 2 {
				return fmt.Errorf("the usage logs have not reached 2 entries yet (total=%d)", resp.Total)
			}
			return nil
		})

		st, b := doRequest(t, http.MethodGet, gw.baseURL+"/admin/usage/summary?group_by=model", adminAuth, nil)
		if st != http.StatusOK {
			t.Fatalf("GET /admin/usage/summary expected 200, got %d, response: %s", st, b)
		}
		var resp struct {
			GroupBy string `json:"group_by"`
			Rows    []struct {
				AIModelID    int64 `json:"ai_model_id"`
				TotalTokens  int64 `json:"total_tokens"`
				RequestCount int64 `json:"request_count"`
			} `json:"rows"`
		}
		if err := json.Unmarshal(b, &resp); err != nil {
			t.Fatalf("failed to parse summary response: %v, response: %s", err, b)
		}
		if resp.GroupBy != "model" || len(resp.Rows) == 0 {
			t.Fatalf("summary should be grouped by model and contain rows, got: %+v", resp)
		}
		if resp.Rows[0].TotalTokens <= 0 || resp.Rows[0].RequestCount <= 0 {
			t.Errorf("summary rows should contain a positive token count and request count, got: %+v", resp.Rows[0])
		}
	})

	t.Run("5. user-facing API: identity / quota view / row-level isolation", func(t *testing.T) {
		st, b := doRequest(t, http.MethodGet, gw.baseURL+"/api/user/me", userTok, nil)
		if st != http.StatusOK {
			t.Fatalf("GET /api/user/me expected 200, got %d, response: %s", st, b)
		}
		var me struct {
			ID      int64 `json:"id"`
			GroupID int64 `json:"group_id"`
		}
		if err := json.Unmarshal(b, &me); err != nil || me.ID != userID || me.GroupID != limitedG {
			t.Fatalf("unexpected me response: %+v (err=%v), response: %s", me, err, b)
		}

		// 配额视图：解析组策略（day/5），用量为该用户全部用量（>0）。
		st, b = doRequest(t, http.MethodGet, gw.baseURL+"/api/user/quota", userTok, nil)
		if st != http.StatusOK {
			t.Fatalf("GET /api/user/quota expected 200, got %d, response: %s", st, b)
		}
		var q struct {
			UserID int64 `json:"user_id"`
			Usages []struct {
				Mode   string `json:"mode"`
				Limit  int    `json:"limit"`
				Used   int64  `json:"used"`
				Active bool   `json:"active"`
			} `json:"usages"`
		}
		if err := json.Unmarshal(b, &q); err != nil {
			t.Fatalf("failed to parse /api/user/quota response: %v, response: %s", err, b)
		}
		if q.UserID != userID || len(q.Usages) != 1 {
			t.Fatalf("unexpected quota view: %+v, response: %s", q, b)
		}
		if u := q.Usages[0]; u.Mode != "day" || u.Limit != 5 || u.Used <= 0 || !u.Active {
			t.Errorf("unexpected quota entry: %+v", u)
		}

		// 行级隔离：user 角色传他人 user_id 被忽略，仍只返回自己的记录。
		// 用量为异步写入，且 ListUsageLogs 的 Count 与 Find 是两条独立 SQL：
		// 必须轮询到 total 与 logs 数组一致且都达到 2 条，避免读到「total 落后于 rows」的中间态。
		var logs struct {
			Total int64 `json:"total"`
			Logs  []struct {
				UserID int64 `json:"user_id"`
			} `json:"logs"`
		}
		waitFor(t, "user-facing usage logs settle (total and rows agree)", usagePollTimeout, func() error {
			st, b := doRequest(
				t,
				http.MethodGet,
				gw.baseURL+"/api/user/usage/logs?user_id=999999&page_size=50",
				userTok,
				nil,
			)
			if st != http.StatusOK {
				return fmt.Errorf("GET /api/user/usage/logs returned %d: %s", st, b)
			}
			if err := json.Unmarshal(b, &logs); err != nil {
				return err
			}
			if logs.Total < 2 || int64(len(logs.Logs)) < 2 || logs.Total != int64(len(logs.Logs)) {
				return fmt.Errorf("usage not settled yet (total=%d rows=%d)", logs.Total, len(logs.Logs))
			}
			return nil
		})
		for i, row := range logs.Logs {
			if row.UserID != userID {
				t.Errorf("the user-facing log entry[%d] contains another user's data, user_id=%d", i, row.UserID)
			}
		}

		// 管理员可显式查询指定用户；同样等异步写入稳定后再断言。
		var adminLogs struct {
			Total int64 `json:"total"`
			Logs  []struct {
				UserID int64 `json:"user_id"`
			} `json:"logs"`
		}
		waitFor(t, "admin usage logs for that user settle", usagePollTimeout, func() error {
			st, b := doRequest(
				t,
				http.MethodGet,
				fmt.Sprintf("%s/api/user/usage/logs?user_id=%d&page_size=50", gw.baseURL, userID),
				adminAuth,
				nil,
			)
			if st != http.StatusOK {
				return fmt.Errorf("GET /api/user/usage/logs returned %d: %s", st, b)
			}
			if err := json.Unmarshal(b, &adminLogs); err != nil {
				return err
			}
			if adminLogs.Total < 2 || int64(len(adminLogs.Logs)) < 2 || adminLogs.Total != int64(len(adminLogs.Logs)) {
				return fmt.Errorf("usage not settled yet (total=%d rows=%d)", adminLogs.Total, len(adminLogs.Logs))
			}
			return nil
		})

		// by-model 固定按模型聚合。
		st, b = doRequest(t, http.MethodGet, gw.baseURL+"/api/user/usage/by-model", userTok, nil)
		if st != http.StatusOK {
			t.Fatalf("GET /api/user/usage/by-model expected 200, got %d, response: %s", st, b)
		}
		var byModel struct {
			GroupBy string `json:"group_by"`
			Rows    []struct {
				TotalTokens int64 `json:"total_tokens"`
			} `json:"rows"`
		}
		if err := json.Unmarshal(b, &byModel); err != nil || byModel.GroupBy != "model" || len(byModel.Rows) == 0 ||
			byModel.Rows[0].TotalTokens <= 0 {
			t.Fatalf("unexpected by-model response: %+v (err=%v), response: %s", byModel, err, b)
		}
	})
}

func TestE2E_Admin_KeyLevelQuota(t *testing.T) {
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	mock := newUpstreamMock(kindOpenAI)
	t.Cleanup(mock.Close)

	gw := startGateway(t, bin, dsn)
	adminAuth := adminToken

	modelID := createModel(t, gw.baseURL, adminAuth, map[string]any{
		"provider":   "mock",
		"model_id":   openAIModelID,
		"model_name": openAIModelID,
		"url":        mock.URL() + "/v1",
		"api_key":    upstreamAPIKey,
		"model_type": "openai-completions",
		"quota_day":  -1,
		"enabled":    true,
	})

	t.Run("6. key-level quota: token_quota_rules on the key -> 429 with Retry-After", func(t *testing.T) {
		// 独立用户（无分组，避免用户级规则干扰），仅密钥级 day=5。
		u := createUser(t, gw.baseURL, "m2-keyquota-user", 0)

		created := createKey(t, gw.baseURL, map[string]any{
			"name": "m2-keyquota", "user_id": u.ID,
			"token_quota_rules": `[{"mode":"day","limit":5}]`,
			"bindings":          []map[string]any{{"ai_model_id": modelID, "role": "primary"}},
		})
		if created.Token == "" {
			t.Fatal("keys create response is missing the one-time plaintext token")
		}

		st, _, b := m2PostChatWithHeader(t, gw.baseURL, created.Token, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if st != http.StatusOK {
			t.Fatalf("the first key-level request expected 200, got %d, response: %s", st, b)
		}
		// 日限 5，本次即将记入 18 tokens；等待该密钥用量异步落库后再发第二次。
		waitFor(t, "the key usage is persisted", usagePollTimeout, func() error {
			st, b := doRequest(
				t,
				http.MethodGet,
				fmt.Sprintf("%s/admin/usage/logs?api_key_id=%d", gw.baseURL, created.ID),
				adminAuth,
				nil,
			)
			if st != http.StatusOK {
				return fmt.Errorf("GET /admin/usage/logs returned %d: %s", st, b)
			}
			var resp struct {
				Total int64 `json:"total"`
			}
			if err := json.Unmarshal(b, &resp); err != nil {
				return err
			}
			if resp.Total < 1 {
				return fmt.Errorf("the key usage is not persisted yet (total=%d)", resp.Total)
			}
			return nil
		})

		st, header, b := m2PostChatWithHeader(t, gw.baseURL, created.Token, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi again"}},
		})
		if st != http.StatusTooManyRequests {
			t.Fatalf("the key-level exceeding request expected 429, got %d, response: %s", st, b)
		}
		if !bytes.Contains(b, []byte("Key token quota exceeded")) {
			t.Errorf("the 429 response should contain %q, response: %s", "Key token quota exceeded", b)
		}
		if ra := header.Get("Retry-After"); ra == "" {
			t.Error("Retry-After should be present on a key-level quota rejection")
		}
	})
}

func TestE2E_Admin_GroupQuotaOverride(t *testing.T) {
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	mock := newUpstreamMock(kindOpenAI)
	t.Cleanup(mock.Close)

	gw := startGateway(t, bin, dsn)
	adminAuth := adminToken

	modelID := createModel(t, gw.baseURL, adminAuth, map[string]any{
		"provider":   "mock",
		"model_id":   openAIModelID,
		"model_name": openAIModelID,
		"url":        mock.URL() + "/v1",
		"api_key":    upstreamAPIKey,
		"model_type": "openai-completions",
		"quota_day":  -1,
		"enabled":    true,
	})

	t.Run("7. group-level global quota override: the group's own rules apply and count per group", func(t *testing.T) {
		// 组 G 显式全局规则（day=5）；站点保持默认不限 → 只有组规则生效。
		g := struct{ ID int64 }{ID: createGroupWith(t, gw.baseURL, "m2-goverride", map[string]any{
			"global_token_quota_rules": `[{"mode":"day","limit":5}]`,
		})}
		var body []byte
		_ = body

		u := createUser(t, gw.baseURL, "m2-goverride-user", g.ID)

		_, key := m2CreateKey(t, gw.baseURL, adminAuth, u.ID, g.ID, modelID)

		st, _, b := m2PostChatWithHeader(t, gw.baseURL, key, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if st != http.StatusOK {
			t.Fatalf("the first request under the group override expected 200, got %d, response: %s", st, b)
		}
		waitFor(t, "the group usage is persisted", usagePollTimeout, func() error {
			st, b := doRequest(
				t,
				http.MethodGet,
				fmt.Sprintf("%s/admin/usage/logs?group_id=%d", gw.baseURL, g.ID),
				adminAuth,
				nil,
			)
			if st != http.StatusOK {
				return fmt.Errorf("GET /admin/usage/logs returned %d: %s", st, b)
			}
			var resp struct {
				Total int64 `json:"total"`
			}
			if err := json.Unmarshal(b, &resp); err != nil {
				return err
			}
			if resp.Total < 1 {
				return fmt.Errorf("the group usage is not persisted yet (total=%d)", resp.Total)
			}
			return nil
		})

		st, _, b = m2PostChatWithHeader(t, gw.baseURL, key, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi again"}},
		})
		if st != http.StatusTooManyRequests {
			t.Fatalf("the group-override exceeding request expected 429, got %d, response: %s", st, b)
		}
		if !bytes.Contains(b, []byte("Global token quota exceeded")) {
			t.Errorf("the 429 response should contain %q, response: %s", "Global token quota exceeded", b)
		}
	})
}

func TestE2E_Admin_CredentialAndImport(t *testing.T) {
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	openAIMock := newUpstreamMock(kindOpenAI)
	t.Cleanup(openAIMock.Close)

	gw := startGateway(t, bin, dsn)
	adminAuth := adminToken

	db := openE2EDB(t, dsn)
	waitFor(t, "database reachable", usagePollTimeout, db.Ping)

	var (
		credModelRowID int64
		downModelRowID int64
	)
	const credModelID = "mock-cred-e2e"
	t.Run(
		"1. credential + encrypted storage: forwarding uses the credential secret and stores it as ciphertext",
		func(t *testing.T) {
			credID := m3bCreateCredential(t, gw.baseURL, adminAuth, "m3b-cred", credSecretE2E)
			credModelRowID = createModel(t, gw.baseURL, adminAuth, map[string]any{
				"provider":      "mock",
				"model_id":      credModelID,
				"model_name":    credModelID,
				"url":           openAIMock.URL() + "/v1",
				"api_key":       "",
				"credential_id": credID,
				"model_type":    "openai-completions",
				"quota_day":     -1,
				"enabled":       true,
			})

			credKey := createKey(t, gw.baseURL, map[string]any{
				"name": "m3b-cred-key", "user_id": 1,
				"bindings": []map[string]any{{"ai_model_id": credModelRowID, "role": "primary"}},
			})
			key := credKey
			if key.Token == "" {
				t.Fatal("keys create response is missing the one-time plaintext token")
			}

			// 转发成功，且 mock 收到的是凭据中的 header 密钥。
			status, respBody := postChatCompletion(t, gw.baseURL, key.Token, map[string]any{
				"model":    credModelID,
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			})
			if status != http.StatusOK {
				t.Fatalf(
					"forwarding to a model that references a credential expected 200, got %d, response: %s",
					status,
					respBody,
				)
			}
			got := openAIMock.lastRequest()
			if auth := got.Headers.Get("Authorization"); auth != "Bearer "+credSecretE2E {
				t.Errorf(
					"upstream Authorization expected the credential value %q, got %q",
					"Bearer "+credSecretE2E,
					auth,
				)
			}

			// 直连数据库：凭据落库为密文，且不出现明文。
			var secrets string
			if err := db.QueryRow(`SELECT secrets_json FROM credentials WHERE id = ?`, credID).Scan(&secrets); err != nil {
				t.Fatalf("failed to read credentials.secrets_json directly: %v", err)
			}
			if !strings.HasPrefix(secrets, "enc:v1:") {
				t.Errorf("credentials.secrets_json expected ciphertext with the enc:v1: prefix, got %q", secrets)
			}
			if strings.Contains(secrets, credSecretE2E) {
				t.Errorf("credentials.secrets_json contains plaintext: %q", secrets)
			}

			// 引用凭据的模型自身 api_key 为空（密钥只在凭据里）。
			var modelAPIKey string
			if err := db.QueryRow(`SELECT api_key FROM ai_models WHERE id = ?`, credModelRowID).Scan(&modelAPIKey); err != nil {
				t.Fatalf("failed to read ai_models.api_key directly: %v", err)
			}
			if modelAPIKey != "" {
				t.Errorf("a model that references a credential should have an empty api_key, got %q", modelAPIKey)
			}
		},
	)

	t.Run(
		"2. legacy import: dry run persists nothing, the real run takes effect, a rerun is idempotent, and the imported t"+
			"oken can forward",
		func(t *testing.T) {
			tables := []string{"user_groups", "users", "ai_models", "api_keys"}
			before := make(map[string]int, len(tables))
			for _, tb := range tables {
				before[tb] = m3bCount(t, db, tb)
			}

			// 预演：只给计划，不写库。
			status, body := doRequest(t, http.MethodPost, gw.baseURL+"/admin/import", adminAuth,
				mustJSON(t, m3bImportPayload(true, openAIMock.URL())))
			if status != http.StatusOK {
				t.Fatalf("POST /admin/import(dry_run) expected 200, got %d, response: %s", status, body)
			}
			var dry importSummaryResponse
			if err := json.Unmarshal(body, &dry); err != nil {
				t.Fatalf("failed to parse the dry_run response: %v, response: %s", err, body)
			}
			if !dry.DryRun || dry.Summary.Created <= 0 {
				t.Fatalf("dry_run expected created>0, got %+v", dry)
			}
			for _, tb := range tables {
				if got := m3bCount(t, db, tb); got != before[tb] {
					t.Errorf("dry_run should not persist anything: %s row count %d -> %d", tb, before[tb], got)
				}
			}

			// 正式导入：四类实体各新建一条。
			status, body = doRequest(t, http.MethodPost, gw.baseURL+"/admin/import", adminAuth,
				mustJSON(t, m3bImportPayload(false, openAIMock.URL())))
			if status != http.StatusOK {
				t.Fatalf("POST /admin/import expected 200, got %d, response: %s", status, body)
			}
			var real importSummaryResponse
			if err := json.Unmarshal(body, &real); err != nil {
				t.Fatalf("failed to parse the real import response: %v, response: %s", err, body)
			}
			if real.Summary.Created != 4 || real.Summary.Errors != 0 {
				t.Fatalf("the real import expected created=4 errors=0, got %+v", real.Summary)
			}
			for _, tb := range tables {
				if got := m3bCount(t, db, tb); got <= before[tb] {
					t.Errorf(
						"after the real import the row count of %s did not increase: %d -> %d",
						tb,
						before[tb],
						got,
					)
				}
			}

			// 用导入的明文 token 转发成功。
			status, respBody := postChatCompletion(t, gw.baseURL, legacyTokenE2E, map[string]any{
				"model":    "m3b-legacy",
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			})
			if status != http.StatusOK {
				t.Fatalf("forwarding with the imported key expected 200, got %d, response: %s", status, respBody)
			}

			// 重跑：全部跳过、无新建。
			status, body = doRequest(t, http.MethodPost, gw.baseURL+"/admin/import", adminAuth,
				mustJSON(t, m3bImportPayload(false, openAIMock.URL())))
			if status != http.StatusOK {
				t.Fatalf("the repeated import expected 200, got %d, response: %s", status, body)
			}
			var again importSummaryResponse
			if err := json.Unmarshal(body, &again); err != nil {
				t.Fatalf("failed to parse the repeated import response: %v, response: %s", err, body)
			}
			if again.Summary.Created != 0 || again.Summary.Skipped <= 0 {
				t.Errorf("the repeated import expected created=0 skipped>0, got %+v", again.Summary)
			}
		},
	)

	t.Run(
		"3. connectivity: a reachable model returns latency, an unreachable one is classified as network_unreachable",
		func(t *testing.T) {
			var okResp connectivityResult
			adminCLI(t, requireEnv(t, envCLIBinKey), gw.baseURL, &okResp,
				"models", "connectivity", "--id", strconv.FormatInt(credModelRowID, 10))
			if !okResp.OK || okResp.LatencyMS < 1 {
				t.Errorf("the reachable model expected ok=true and latency_ms>=1, got %+v", okResp)
			}

			downModelRowID = createModel(t, gw.baseURL, adminAuth, map[string]any{
				"provider":   "mock",
				"model_id":   "mock-down-e2e",
				"model_name": "mock-down-e2e",
				"url":        "http://127.0.0.1:1/v1",
				"api_key":    "sk-down-upstream",
				"model_type": "openai-completions",
				"quota_day":  -1,
				"enabled":    true,
			})
			// 不可达也是 HTTP 200（结论在响应体里），因此 CLI 仍应退出 0 并原样输出该结论。
			var down connectivityResult
			adminCLI(t, requireEnv(t, envCLIBinKey), gw.baseURL, &down,
				"models", "connectivity", "--id", strconv.FormatInt(downModelRowID, 10))
			if down.OK || down.ErrorKind != "network_unreachable" {
				t.Errorf("the unreachable model expected ok=false kind=network_unreachable, got %+v", down)
			}
		},
	)

	t.Run("8. usage export (CSV/JSON) and single-key usage trend", func(t *testing.T) {
		// 导出：CSV 表头 + JSON 结构 + 非法格式 400（此时 M3b 已产生多条用量）。
		status, body := doRequest(t, http.MethodGet, gw.baseURL+"/admin/usage/export", adminAuth, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /admin/usage/export expected 200, got %d, response: %s", status, body)
		}
		lines := strings.Split(strings.TrimSpace(string(body)), "\n")
		if len(lines) < 2 || !strings.HasPrefix(lines[0], "id,api_key_id,user_id,group_id") {
			t.Fatalf("unexpected CSV header/first rows: %q", lines[0])
		}
		status, body = doRequest(t, http.MethodGet, gw.baseURL+"/admin/usage/export?format=json", adminAuth, nil)
		if status != http.StatusOK || !bytes.Contains(body, []byte(`"logs"`)) {
			t.Fatalf("GET /admin/usage/export?format=json expected 200 with logs, got %d, response: %s", status, body)
		}
		status, _ = doRequest(t, http.MethodGet, gw.baseURL+"/admin/usage/export?format=xml", adminAuth, nil)
		if status != http.StatusBadRequest {
			t.Fatalf("an invalid export format expected 400, got %d", status)
		}

		// 单密钥趋势：建用户 + 密钥 + 一次真实转发，再按用户 token 查询。
		u := createUser(t, gw.baseURL, "m3b-trend-user", 0)
		trendKeyID, keyTok := m2CreateKey(t, gw.baseURL, adminAuth, u.ID, 0, credModelRowID)
		status, respBody := postChatCompletion(t, gw.baseURL, keyTok, map[string]any{
			"model":    credModelID,
			"messages": []map[string]string{{"role": "user", "content": "trend"}},
		})
		if status != http.StatusOK {
			t.Fatalf("the trend sample request expected 200, got %d, response: %s", status, respBody)
		}
		// 等用量落库后查询该用户自己的密钥趋势（行级隔离路径）。
		waitFor(t, "the trend usage is persisted", usagePollTimeout, func() error {
			st, b := doRequest(
				t,
				http.MethodGet,
				fmt.Sprintf("%s/admin/usage/logs?user_id=%d", gw.baseURL, u.ID),
				adminAuth,
				nil,
			)
			if st != http.StatusOK {
				return fmt.Errorf("GET /admin/usage/logs returned %d: %s", st, b)
			}
			var resp struct {
				Total int64 `json:"total"`
			}
			if err := json.Unmarshal(b, &resp); err != nil {
				return err
			}
			if resp.Total < 1 {
				return fmt.Errorf("usage not persisted yet (total=%d)", resp.Total)
			}
			return nil
		})
		status, body = doRequest(
			t,
			http.MethodGet,
			fmt.Sprintf("%s/api/user/keys/%d/usage", gw.baseURL, trendKeyID),
			u.Token,
			nil,
		)
		if status != http.StatusOK {
			t.Fatalf("GET /api/user/keys/{id}/usage expected 200, got %d, response: %s", status, body)
		}
		var trend struct {
			KeyID uint `json:"key_id"`
			Rows  []struct {
				TotalTokens int64 `json:"total_tokens"`
			} `json:"rows"`
		}
		if err := json.Unmarshal(body, &trend); err != nil || len(trend.Rows) == 0 || trend.Rows[0].TotalTokens <= 0 {
			t.Fatalf("unexpected trend response: %+v (err=%v), response: %s", trend, err, body)
		}
	})
}

func TestE2E_Admin_KeyTTLAndBindingPromotion(t *testing.T) {
	const credModelID = "m3b-ttl-model"
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	openAIMock := newUpstreamMock(kindOpenAI)
	t.Cleanup(openAIMock.Close)

	gw := startGateway(t, bin, dsn)
	adminAuth := adminToken

	db := openE2EDB(t, dsn)
	waitFor(t, "database reachable", usagePollTimeout, db.Ping)

	// 本组共享的夹具：一个引用凭据的可用模型（TTL 与绑定提升用例的绑定目标）。
	credID := m3bCreateCredential(t, gw.baseURL, adminAuth, "m3b-ttl-cred", credSecretE2E)
	credModelRowID := createModel(t, gw.baseURL, adminAuth, map[string]any{
		"provider":      "mock",
		"model_id":      "m3b-ttl-model",
		"model_name":    "m3b-ttl-model",
		"url":           openAIMock.URL() + "/v1",
		"api_key":       "",
		"credential_id": credID,
		"model_type":    "openai-completions",
		"quota_day":     -1,
		"enabled":       true,
	})
	t.Run("5. key TTL: an expired key is rejected and recorded; clearing the TTL restores access", func(t *testing.T) {
		key := createKey(t, gw.baseURL, map[string]any{
			"name": "m3b-ttl-key", "user_id": 1,
			"expires_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
			"bindings":   []map[string]any{{"ai_model_id": credModelRowID, "role": "primary"}},
		})
		if key.Token == "" {
			t.Fatal("keys create response is missing the one-time plaintext token")
		}

		// 数据面：过期密钥按「无效凭据」口径拒绝。
		status, respBody := postChatCompletion(t, gw.baseURL, key.Token, map[string]any{
			"model":    credModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusUnauthorized {
			t.Fatalf("an expired key expected 401, got %d, response: %s", status, respBody)
		}

		// 管理面：expires_at 与首次过期观测（expired_at）均已记录。
		var detail struct {
			ExpiresAt *string `json:"expires_at"`
			ExpiredAt *string `json:"expired_at"`
		}
		adminCLI(t, requireEnv(t, envCLIBinKey), gw.baseURL, &detail,
			"keys", "get", "--id", strconv.FormatInt(key.ID, 10))
		if detail.ExpiresAt == nil || detail.ExpiredAt == nil {
			t.Fatalf("the admin detail should expose expires_at and expired_at, got %+v", detail)
		}

		// 非法格式 → CLI 退出码 4；显式 null 清除有效期 → 恢复访问。
		code, out, errOut := runCLI(t, requireEnv(t, envCLIBinKey), gw.baseURL, cliEnv{adminToken: adminToken},
			"keys", "update", "--id", strconv.FormatInt(key.ID, 10),
			"--data", "@"+cliDataFile(t, "bad-ttl.json", map[string]any{"expires_at": "not-a-timestamp"}))
		cliFailure(t, "keys update with an invalid expires_at", cliExitRemoteFail, code, out, errOut)

		adminCLI(t, requireEnv(t, envCLIBinKey), gw.baseURL, nil,
			"keys", "update", "--id", strconv.FormatInt(key.ID, 10),
			"--data", "@"+cliDataFile(t, "clear-ttl.json", map[string]any{"expires_at": nil}))

		status, respBody = postChatCompletion(t, gw.baseURL, key.Token, map[string]any{
			"model":    credModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("after clearing the TTL the key should work again, got %d, response: %s", status, respBody)
		}
	})

	t.Run("6. deleting the primary model promotes the remaining fallback binding", func(t *testing.T) {
		modelA := createModel(t, gw.baseURL, adminAuth, map[string]any{
			"provider": "mock", "model_id": "m3b-promote-a", "model_name": "m3b-promote-a",
			"url": openAIMock.URL() + "/v1", "api_key": "sk-mock-upstream",
			"model_type": "openai-completions", "quota_day": -1, "enabled": true,
		})
		modelB := createModel(t, gw.baseURL, adminAuth, map[string]any{
			"provider": "mock", "model_id": "m3b-promote-b", "model_name": "m3b-promote-b",
			"url": openAIMock.URL() + "/v1", "api_key": "sk-mock-upstream",
			"model_type": "openai-completions", "quota_day": -1, "enabled": true,
		})
		key := createKey(t, gw.baseURL, map[string]any{
			"name": "m3b-promote-key", "user_id": 1,
			"bindings": []map[string]any{
				{"ai_model_id": modelA, "role": "primary"},
				{"ai_model_id": modelB, "role": "fallback", "sort_order": 5},
			},
		})
		if key.Token == "" {
			t.Fatal("keys create response is missing the one-time plaintext token")
		}

		// 删除 primary 模型 → 剩余 fallback 自动提升；未指定 model 的请求应命中 B。
		adminCLI(t, requireEnv(t, envCLIBinKey), gw.baseURL, nil, "models", "delete", "--id", strconv.FormatInt(modelA, 10))
		status, respBody := postChatCompletion(t, gw.baseURL, key.Token, map[string]any{
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("forwarding after promotion expected 200, got %d, response: %s", status, respBody)
		}
		var sent map[string]any
		if err := json.Unmarshal(openAIMock.lastRequest().Body, &sent); err != nil {
			t.Fatalf("failed to parse the recorded upstream body: %v", err)
		}
		if sent["model"] != "m3b-promote-b" {
			t.Errorf("the promoted binding should be used, upstream model = %v, want m3b-promote-b", sent["model"])
		}
	})
}

func TestE2E_Admin_ModelsAndForwardingFixture(t *testing.T) {
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	openAIMock := newUpstreamMock(kindOpenAI)
	t.Cleanup(openAIMock.Close)
	anthropicMock := newUpstreamMock(kindAnthropic)
	t.Cleanup(anthropicMock.Close)

	gw := startGateway(t, bin, dsn)
	adminAuth := adminToken

	var (
		openAIModelRowID    int64
		anthropicModelRowID int64
		apiKeyPlaintext     string
		apiKeyPrefix        string
	)

	t.Run("1. create two models", func(t *testing.T) {
		openAIModelRowID = createModel(t, gw.baseURL, adminAuth, map[string]any{
			"provider":   "mock",
			"model_id":   openAIModelID,
			"model_name": openAIModelID,
			// OpenAI 兼容端点的 base url 带 /v1，网关会拼 /chat/completions。
			"url":        openAIMock.URL() + "/v1",
			"api_key":    upstreamAPIKey,
			"model_type": "openai-completions",
			"quota_day":  -1,
			"enabled":    true,
		})
		t.Logf("OpenAI-type model row id=%d", openAIModelRowID)

		anthropicModelRowID = createModel(t, gw.baseURL, adminAuth, map[string]any{
			"provider":   "mock",
			"model_id":   anthropicModelID,
			"model_name": anthropicModelID,
			// Anthropic 端点不带 /v1，网关会拼 /v1/messages。
			"url":        anthropicMock.URL(),
			"api_key":    upstreamAPIKey,
			"model_type": "anthropic-messages",
			"quota_day":  -1,
			"enabled":    true,
		})
		t.Logf("Anthropic-type model row id=%d", anthropicModelRowID)

		if openAIModelRowID == anthropicModelRowID {
			t.Errorf("the two models should get different ids, both got %d", openAIModelRowID)
		}
	})

	t.Run("2. create a key bound to the primary/fallback models", func(t *testing.T) {
		resp := createKey(t, gw.baseURL, map[string]any{
			"name":       "e2e",
			"user_id":    1,
			"group_id":   0,
			"max_tokens": 0,
			"bindings": []map[string]any{
				{"ai_model_id": openAIModelRowID, "role": "primary"},
				{"ai_model_id": anthropicModelRowID, "role": "fallback"},
			},
		})
		if !strings.HasPrefix(resp.Token, "sk-") {
			t.Fatalf("the token returned by key creation should start with sk-, got: %q", resp.Token)
		}
		if resp.ProxyTokenPrefix == "" {
			t.Fatal("key creation response is missing proxy_token_prefix")
		}
		apiKeyPlaintext = resp.Token
		apiKeyPrefix = resp.ProxyTokenPrefix
		t.Logf("key id=%d, proxy_token_prefix=%s", resp.ID, apiKeyPrefix)
	})

	t.Run("3. the key list does not echo the plaintext", func(t *testing.T) {
		if apiKeyPlaintext == "" {
			t.Fatal("the previous step did not obtain the key plaintext, cannot assert non-echo")
		}
		keyListRaw := listKeysRaw(t, gw.baseURL)
		if bytes.Contains(keyListRaw, []byte(apiKeyPlaintext)) {
			t.Fatalf("keys list response echoed the key plaintext, response: %s", keyListRaw)
		}
		if !bytes.Contains(keyListRaw, []byte(apiKeyPrefix)) {
			t.Errorf("keys list response should contain proxy_token_prefix %q, response: %s", apiKeyPrefix, keyListRaw)
		}
	})

	t.Run("4. the model list returns the bound models", func(t *testing.T) {
		status, respBody := doRequest(t, http.MethodGet, gw.baseURL+"/v1/models", apiKeyPlaintext, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/models expected 200, got %d, response: %s", status, respBody)
		}
		for _, want := range []string{openAIModelID, anthropicModelID} {
			if !bytes.Contains(respBody, []byte(want)) {
				t.Errorf("GET /v1/models should contain model %q, response: %s", want, respBody)
			}
		}
	})

	t.Run("5. non-streaming forward to an OpenAI-type upstream", func(t *testing.T) {
		status, respBody := postChatCompletion(t, gw.baseURL, apiKeyPlaintext, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("non-streaming /v1/chat/completions expected 200, got %d, response: %s", status, respBody)
		}

		var resp chatCompletionResponse
		if err := json.Unmarshal(respBody, &resp); err != nil {
			t.Fatalf("failed to parse chat completion response: %v, response: %s", err, respBody)
		}
		if len(resp.Choices) == 0 {
			t.Fatalf("response is missing choices, response: %s", respBody)
		}
		if resp.Choices[0].Message.Content != openAIContent {
			t.Errorf(
				"forwarded content expected %q, got %q (response: %s)",
				openAIContent,
				resp.Choices[0].Message.Content,
				respBody,
			)
		}

		got := openAIMock.lastRequest()
		if len(got.Body) == 0 {
			t.Fatal("the OpenAI mock received no request body")
		}
		var upstreamReq struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(got.Body, &upstreamReq); err != nil {
			t.Fatalf(
				"failed to parse the request body received by the OpenAI mock: %v, request body: %s",
				err,
				got.Body,
			)
		}
		if upstreamReq.Model != openAIModelID {
			t.Errorf(
				"the model received by the upstream should be %q, got %q (request body: %s)",
				openAIModelID,
				upstreamReq.Model,
				got.Body,
			)
		}
	})

	t.Run("6. streaming forward returns SSE chunks", func(t *testing.T) {
		status, respBody := postChatCompletion(t, gw.baseURL, apiKeyPlaintext, map[string]any{
			"model":    openAIModelID,
			"stream":   true,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("streaming /v1/chat/completions expected 200, got %d, response: %s", status, respBody)
		}
		if !bytes.Contains(respBody, []byte("data:")) {
			t.Errorf("streaming response should contain data: chunks, response: %s", respBody)
		}
		if !bytes.Contains(respBody, []byte("data: [DONE]")) {
			t.Errorf("streaming response should end with data: [DONE], response: %s", respBody)
		}
	})

	t.Run("7. Anthropic path request and response conversion", func(t *testing.T) {
		status, respBody := postChatCompletion(t, gw.baseURL, apiKeyPlaintext, map[string]any{
			"model":    anthropicModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("Anthropic path expected 200, got %d, response: %s", status, respBody)
		}

		var resp chatCompletionResponse
		if err := json.Unmarshal(respBody, &resp); err != nil {
			t.Fatalf("failed to parse chat completion response: %v, response: %s", err, respBody)
		}
		if len(resp.Choices) == 0 {
			t.Fatalf("response is missing choices, response: %s", respBody)
		}
		if resp.Choices[0].Message.Content != anthropicContent {
			t.Errorf(
				"forwarded content expected %q, got %q (response: %s)",
				anthropicContent,
				resp.Choices[0].Message.Content,
				respBody,
			)
		}

		got := anthropicMock.lastRequest()
		if len(got.Body) == 0 {
			t.Fatal("the Anthropic mock received no request body")
		}
		if got.Path != "/v1/messages" {
			t.Errorf("the Anthropic upstream path should be /v1/messages, got %q", got.Path)
		}
		var upstreamReq map[string]json.RawMessage
		if err := json.Unmarshal(got.Body, &upstreamReq); err != nil {
			t.Fatalf(
				"failed to parse the request body received by the Anthropic mock: %v, request body: %s",
				err,
				got.Body,
			)
		}
		if _, ok := upstreamReq["messages"]; !ok {
			t.Errorf("the Anthropic request body should contain the messages field, request body: %s", got.Body)
		}
		if _, ok := upstreamReq["max_tokens"]; !ok {
			t.Errorf("the Anthropic request body should contain the max_tokens field, request body: %s", got.Body)
		}
	})

	t.Run("8. authentication failure returns 401", func(t *testing.T) {
		status, respBody := doRequest(t, http.MethodGet, gw.baseURL+"/v1/models", "sk-this-key-does-not-exist", nil)
		if status != http.StatusUnauthorized {
			t.Errorf("accessing /v1/models with an invalid key expected 401, got %d, response: %s", status, respBody)
		}

		status, respBody = doRequest(t, http.MethodGet, gw.baseURL+"/v1/models", "", nil)
		if status != http.StatusUnauthorized {
			t.Errorf("accessing /v1/models without Authorization expected 401, got %d, response: %s", status, respBody)
		}
	})

	t.Run("9. usage is persisted asynchronously", func(t *testing.T) {
		db := openE2EDB(t, dsn)
		waitFor(t, "database reachable", usagePollTimeout, db.Ping)

		waitFor(
			t,
			"llm_usage_logs has at least 3 rows and at least one with total_tokens>0",
			usagePollTimeout,
			func() error {
				var total int
				if err := db.QueryRow(`SELECT COUNT(*) FROM llm_usage_logs`).Scan(&total); err != nil {
					return err
				}
				if total < 3 {
					return fmt.Errorf("currently %d rows, expected >= 3", total)
				}
				var positive int
				if err := db.QueryRow(`SELECT COUNT(*) FROM llm_usage_logs WHERE total_tokens > 0`).Scan(&positive); err != nil {
					return err
				}
				if positive == 0 {
					return fmt.Errorf("no usage log row with total_tokens>0 yet")
				}
				return nil
			},
		)

		waitFor(t, "daily_usage_summaries has at least 1 row with total_tokens>0", usagePollTimeout, func() error {
			var positive int
			if err := db.QueryRow(`SELECT COUNT(*) FROM daily_usage_summaries WHERE total_tokens > ` +
				`0`).Scan(&positive); err != nil {
				return err
			}
			if positive < 1 {
				return fmt.Errorf("currently %d rows with total_tokens>0, expected >= 1", positive)
			}
			return nil
		})

		var (
			model    string
			apiKeyID int64
		)
		err := db.QueryRow(`SELECT model, api_key_id FROM llm_usage_logs WHERE total_tokens > 0 LIMIT 1`).
			Scan(&model, &apiKeyID)
		if err != nil {
			t.Fatalf("failed to sample a usage log row: %v", err)
		}
		if model == "" {
			t.Error("the sampled usage log row has an empty model")
		}
		if apiKeyID == 0 {
			t.Error("the sampled usage log row has api_key_id equal to 0")
		}
	})
}

// m2CreateGroup 经 aigatectl 创建分组，返回分组 id。
func m2CreateGroup(t *testing.T, baseURL, adminAuth, name, rules string) int64 {
	t.Helper()
	payload := map[string]any{"name": name}
	if rules != "" {
		payload["token_quota_rules"] = rules
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	adminCLI(t, requireEnv(t, envCLIBinKey), baseURL, &resp,
		"groups", "create", "--data", "@"+cliDataFile(t, "group.json", payload))
	if resp.ID == 0 {
		t.Fatalf("groups create response is missing id for %q", name)
	}
	return resp.ID
}

// m2CreateKey 经 aigatectl 创建一把绑定模型的密钥，返回 (id, 一次性明文 token)。
func m2CreateKey(t *testing.T, baseURL, adminAuth string, ownerID, groupID, modelID int64) (int64, string) {
	t.Helper()
	payload := map[string]any{
		"name":     "m2-key",
		"user_id":  ownerID,
		"group_id": groupID,
		"bindings": []map[string]any{{"ai_model_id": modelID, "role": "primary"}},
	}
	var resp createKeyResponse
	adminCLI(t, requireEnv(t, envCLIBinKey), baseURL, &resp,
		"keys", "create", "--data", "@"+cliDataFile(t, "key.json", payload))
	if resp.Token == "" {
		t.Fatal("keys create response is missing the one-time plaintext token")
	}
	return resp.ID, resp.Token
}

// m3bCreateCredential 经 aigatectl 创建一条 header 密钥凭据，返回凭据 id。
func m3bCreateCredential(t *testing.T, baseURL, adminAuth, name, secret string) int64 {
	t.Helper()
	payload := map[string]any{
		"name": name,
		"secrets": []map[string]any{
			{"key": "Authorization", "value": secret, "location": "header"},
		},
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	adminCLI(t, requireEnv(t, envCLIBinKey), baseURL, &resp,
		"credentials", "create", "--data", "@"+cliDataFile(t, "credential.json", payload))
	if resp.ID == 0 {
		t.Fatal("credentials create response is missing id")
	}
	return resp.ID
}

// m3bCreateSelfKey 以用户身份经 aigatectl self 创建一把绑定指定模型的密钥，
// 返回退出码、stdout 与 stderr：非 2xx 时 aigatectl 把服务端错误文案写 stderr 并返回退出码 4，
// 调用方据此既能断言退出码也能断言文案。
func m3bCreateSelfKey(t *testing.T, baseURL, userToken string, modelRowID int64, name string) (int, []byte, []byte) {
	t.Helper()
	payload := map[string]any{
		"name":       name,
		"max_tokens": 0,
		"bindings":   []map[string]any{{"ai_model_id": modelRowID, "role": "primary"}},
	}
	code, out, errOut := runCLI(t, requireEnv(t, envCLIBinKey), baseURL,
		cliEnv{adminToken: adminToken, userToken: userToken},
		"self", "keys", "create", "--data", "@"+cliDataFile(t, "self-key.json", payload))
	return code, out, errOut
}
