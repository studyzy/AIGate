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

// 用户面用例：/api/user/* 的自助密钥与配额视图。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestE2E_User_SelfServiceKeys(t *testing.T) {
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	openAIMock := newUpstreamMock(kindOpenAI)
	t.Cleanup(openAIMock.Close)

	gw := startGateway(t, bin, dsn)
	adminAuth := adminToken

	db := openE2EDB(t, dsn)
	waitFor(t, "database reachable", usagePollTimeout, db.Ping)

	// 本组共享的夹具：一个引用凭据的可用模型，一个地址不可达的模型（用于校验绑定校验）。
	credID := m3bCreateCredential(t, gw.baseURL, adminAuth, "m3b-self-cred", credSecretE2E)
	credModelRowID := createModel(t, gw.baseURL, adminAuth, map[string]any{
		"provider":      "mock",
		"model_id":      "m3b-self-model",
		"model_name":    "m3b-self-model",
		"url":           openAIMock.URL() + "/v1",
		"api_key":       "",
		"credential_id": credID,
		"model_type":    "openai-completions",
		"quota_day":     -1,
		"enabled":       true,
	})
	downModelRowID := createModel(t, gw.baseURL, adminAuth, map[string]any{
		"provider":   "mock",
		"model_id":   "m3b-self-down",
		"model_name": "m3b-self-down",
		"url":        "http://127.0.0.1:1/v1",
		"api_key":    upstreamAPIKey,
		"model_type": "openai-completions",
		"quota_day":  -1,
		"enabled":    true,
	})
	t.Run("4. self-service keys: creation / scope narrowing / limit / row-level isolation", func(t *testing.T) {
		// 建用户 U，并由管理员为其建一把带绑定的密钥（模型 = 凭据模型）。
		u := createUser(t, gw.baseURL, "m3b-self-user", 0)

		seed := createKey(t, gw.baseURL, map[string]any{
			"name": "m3b-self-seed", "user_id": u.ID,
			"bindings": []map[string]any{{"ai_model_id": credModelRowID, "role": "primary"}},
		})
		if seed.Token == "" {
			t.Fatal("the admin-issued key for U is missing its one-time plaintext token")
		}

		// U 自助创建（绑定同一模型）→ 退出码 0 + 一次性明文。
		status, body, _ := m3bCreateSelfKey(t, gw.baseURL, u.Token, credModelRowID, "self-1")
		if status != cliExitOK {
			t.Fatalf("self-service creation expected exit 0, got %d, stdout: %s", status, body)
		}
		var created createKeyResponse
		if err := json.Unmarshal(body, &created); err != nil || !strings.HasPrefix(created.Token, "sk-") {
			t.Fatalf("self-service creation should return a one-time sk- plaintext: %v, response: %s", err, body)
		}

		// 绑定未获批模型（不可达模型不属于 U 的任何已有密钥）→ 400：CLI 退出码 4，文案落到 stderr。
		status, body, errOut := m3bCreateSelfKey(t, gw.baseURL, u.Token, downModelRowID, "self-bad")
		if status != cliExitRemoteFail {
			t.Fatalf("binding an unapproved model expected exit %d, got %d, stdout: %s", cliExitRemoteFail, status, body)
		}
		if !bytes.Contains(errOut, []byte("model not available for this user")) {
			t.Errorf("stderr should carry the server message, got %q", errOut)
		}

		// 列表只含自己的密钥。
		var list selfKeyListResponse
		userCLI(t, requireEnv(t, envCLIBinKey), gw.baseURL, u.Token, &list, "self", "keys", "list")
		if err := error(nil); err != nil {
			t.Fatalf("failed to parse /api/user/keys response: %v, response: %s", err, body)
		}
		if len(list.Keys) != 2 {
			t.Fatalf("the self-service list expected 2 entries (seeded + self-service), got %+v", list.Keys)
		}
		for i := range list.Keys {
			if list.Keys[i].UserID != u.ID {
				t.Errorf("the self-service list entry[%d] contains another user's key: %+v", i, list.Keys[i])
			}
		}

		// 上限：把 user_key_limit 设为 1 后再次创建 → CLI 退出码 4。
		adminCLI(t, requireEnv(t, envCLIBinKey), gw.baseURL, nil,
			"settings", "update", "--data", "@"+cliDataFile(t, "limit.json", map[string]any{"user_key_limit": 1}))
		status, body, errOut = m3bCreateSelfKey(t, gw.baseURL, u.Token, credModelRowID, "self-limit")
		if status != cliExitRemoteFail {
			t.Fatalf("creation beyond the limit expected exit %d, got %d, stdout: %s", cliExitRemoteFail, status, body)
		}
		if !bytes.Contains(errOut, []byte("key limit reached")) {
			t.Errorf("stderr should carry the server message, got %q", errOut)
		}
	})

	t.Run(
		"7. user-key (BYOK) model: the binding key is used for forwarding; a missing binding key is rejected",
		func(t *testing.T) {
			byokModelID := createModel(t, gw.baseURL, adminAuth, map[string]any{
				"provider": "mock", "model_id": "m3b-byok", "model_name": "m3b-byok",
				"url": openAIMock.URL() + "/v1", "api_key": "",
				"model_type": "openai-completions", "quota_day": -1, "enabled": true,
				"user_key_required": true,
			})

			// 带绑定密钥的密钥：创建响应只暴露 has_key，不回显明文。
			body := createKeysRaw(t, gw.baseURL, map[string]any{
				"name": "m3b-byok-key", "user_id": 1,
				"bindings": []map[string]any{
					{"ai_model_id": byokModelID, "role": "primary", "api_key": "sk-byok-e2e"},
				},
			})
			if bytes.Contains(body, []byte("sk-byok-e2e")) {
				t.Fatalf("the binding key plaintext must never be echoed, response: %s", body)
			}
			var byokKey createKeyResponse
			if err := json.Unmarshal(body, &byokKey); err != nil || byokKey.Token == "" {
				t.Fatalf("failed to parse BYOK key response: %v, response: %s", err, body)
			}
			if len(byokKey.Bindings) != 1 || !byokKey.Bindings[0].HasKey {
				t.Fatalf("the create response should flag has_key=true: %+v", byokKey.Bindings)
			}

			// 转发使用绑定密钥（而非任何共享密钥）。
			status, respBody := postChatCompletion(t, gw.baseURL, byokKey.Token, map[string]any{
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			})
			if status != http.StatusOK {
				t.Fatalf("the BYOK forward expected 200, got %d, response: %s", status, respBody)
			}
			if got := openAIMock.lastRequest().Headers.Get("Authorization"); got != "Bearer sk-byok-e2e" {
				t.Fatalf("the upstream Authorization = %q, want the per-binding key", got)
			}

			// 缺绑定密钥的密钥：数据面 400、带专用文案。
			body = createKeysRaw(t, gw.baseURL, map[string]any{
				"name": "m3b-byok-key2", "user_id": 1,
				"bindings": []map[string]any{{"ai_model_id": byokModelID, "role": "primary"}},
			})
			var noKeyKey createKeyResponse
			if err := json.Unmarshal(body, &noKeyKey); err != nil || noKeyKey.Token == "" {
				t.Fatalf("failed to parse key response: %v, response: %s", err, body)
			}
			status, respBody = postChatCompletion(t, gw.baseURL, noKeyKey.Token, map[string]any{
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			})
			if status != http.StatusBadRequest {
				t.Fatalf("a key-less BYOK binding expected 400, got %d, response: %s", status, respBody)
			}
			if !bytes.Contains(respBody, []byte("user-provided key")) {
				t.Errorf("the 400 response should explain the missing user-provided key, response: %s", respBody)
			}
		},
	)
}
