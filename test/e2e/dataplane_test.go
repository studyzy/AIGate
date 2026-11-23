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

// 数据面用例：/v1/chat/completions 与 /v1/messages 是网关对外的转发入口，
// aigatectl 不代理数据面，因此这一层保留原生 HTTP 调用。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// postAnthropicMessage 用 x-api-key 头向数据面发起一次 /v1/messages 请求，
// 返回（状态码, 响应头, 响应体）。apiKey 为空时不带凭据。
func postAnthropicMessage(t *testing.T, baseURL, apiKey string, payload map[string]any) (int, http.Header, []byte) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/messages", bytes.NewReader(mustJSON(t, payload)))
	if err != nil {
		t.Fatalf("failed to build the /v1/messages request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
	}

	resp, err := e2eHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("request %s failed: %v", req.URL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read the /v1/messages response: %v", err)
	}
	return resp.StatusCode, resp.Header, body
}

func TestE2E_DataPlane_AnthropicIngress(t *testing.T) {
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	openAIMock := newUpstreamMock(kindOpenAI)
	t.Cleanup(openAIMock.Close)
	anthropicMock := newUpstreamMock(kindAnthropic)
	t.Cleanup(anthropicMock.Close)

	gw := startGateway(t, bin, dsn)
	adminAuth := adminToken

	var (
		apiKeyPlaintext     string
		openAIModelRowID    int64
		anthropicModelRowID int64
	)

	t.Run("1. create two models and a key (primary=OpenAI / fallback=Anthropic)", func(t *testing.T) {
		openAIModelRowID = createModel(t, gw.baseURL, adminAuth, map[string]any{
			"provider":   "mock",
			"model_id":   openAIModelID,
			"model_name": openAIModelID,
			"url":        openAIMock.URL() + "/v1",
			"api_key":    upstreamAPIKey,
			"model_type": "openai-completions",
			"quota_day":  -1,
			"enabled":    true,
		})
		anthropicModelRowID = createModel(t, gw.baseURL, adminAuth, map[string]any{
			"provider":   "mock",
			"model_id":   anthropicModelID,
			"model_name": anthropicModelID,
			"url":        anthropicMock.URL(),
			"api_key":    upstreamAPIKey,
			"model_type": "anthropic-messages",
			"quota_day":  -1,
			"enabled":    true,
		})

		status, respBody := doRequest(
			t,
			http.MethodPost,
			gw.baseURL+"/admin/keys",
			adminAuth,
			mustJSON(t, map[string]any{
				"name":       "m3-key",
				"user_id":    1,
				"group_id":   0,
				"max_tokens": 0,
				"bindings": []map[string]any{
					{"ai_model_id": openAIModelRowID, "role": "primary"},
					{"ai_model_id": anthropicModelRowID, "role": "fallback"},
				},
			}),
		)
		if status != http.StatusCreated {
			t.Fatalf("POST /admin/keys expected 201, got %d, response: %s", status, respBody)
		}
		var resp createKeyResponse
		if err := json.Unmarshal(respBody, &resp); err != nil || resp.Token == "" {
			t.Fatalf("failed to parse /admin/keys response: %v, response: %s", err, respBody)
		}
		apiKeyPlaintext = resp.Token
		t.Logf("M3 key id=%d", resp.ID)
	})

	t.Run("2. non-streaming conversion path (OpenAI upstream -> Anthropic response)", func(t *testing.T) {
		status, _, body := postAnthropicMessage(t, gw.baseURL, apiKeyPlaintext, map[string]any{
			"model":      openAIModelID,
			"max_tokens": 64,
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("non-streaming /v1/messages expected 200, got %d, response: %s", status, body)
		}

		var resp struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			StopReason string `json:"stop_reason"`
			Usage      struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("failed to parse the Anthropic response: %v, response: %s", err, body)
		}
		if resp.Type != "message" {
			t.Errorf("response type expected %q, got %q (response: %s)", "message", resp.Type, body)
		}
		if resp.Role != "assistant" {
			t.Errorf("response role expected %q, got %q (response: %s)", "assistant", resp.Role, body)
		}
		if !strings.HasPrefix(resp.ID, "msg_") {
			t.Errorf("response id expected the msg_ prefix, got %q", resp.ID)
		}
		if resp.StopReason != "end_turn" {
			t.Errorf("response stop_reason expected %q, got %q (response: %s)", "end_turn", resp.StopReason, body)
		}
		if len(resp.Content) != 1 || resp.Content[0].Type != "text" || resp.Content[0].Text != openAIContent {
			t.Errorf(
				"response content expected a single text block %q, got %+v (response: %s)",
				openAIContent,
				resp.Content,
				body,
			)
		}
		if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 {
			t.Errorf("response usage expected input=11 output=7, got %+v (response: %s)", resp.Usage, body)
		}

		got := openAIMock.lastRequest()
		if got.Path != "/v1/chat/completions" {
			t.Errorf("the OpenAI upstream path should be /v1/chat/completions, got %q", got.Path)
		}
	})

	t.Run("3. streaming conversion path (OpenAI SSE -> Anthropic event sequence)", func(t *testing.T) {
		status, _, body := postAnthropicMessage(t, gw.baseURL, apiKeyPlaintext, map[string]any{
			"model":      openAIModelID,
			"max_tokens": 64,
			"stream":     true,
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("streaming /v1/messages expected 200, got %d, response: %s", status, body)
		}
		for _, want := range []string{"event: message_start", "text_delta", "event: message_stop"} {
			if !bytes.Contains(body, []byte(want)) {
				t.Errorf("the streaming response should contain %q, response: %s", want, body)
			}
		}
	})

	t.Run("4. passthrough path (Anthropic upstream, cache_control preserved)", func(t *testing.T) {
		status, _, body := postAnthropicMessage(t, gw.baseURL, apiKeyPlaintext, map[string]any{
			"model":      anthropicModelID,
			"max_tokens": 64,
			"system": []map[string]any{
				{"type": "text", "text": "be brief", "cache_control": map[string]any{"type": "ephemeral"}},
			},
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("passthrough /v1/messages expected 200, got %d, response: %s", status, body)
		}
		if !bytes.Contains(body, []byte(anthropicContent)) {
			t.Errorf(
				"the passthrough response should contain the upstream text %q, response: %s",
				anthropicContent,
				body,
			)
		}

		got := anthropicMock.lastRequest()
		if got.Path != "/v1/messages" {
			t.Errorf("the Anthropic upstream path should be /v1/messages, got %q", got.Path)
		}
		if !bytes.Contains(got.Body, []byte("cache_control")) {
			t.Errorf(
				"the request body received by the Anthropic upstream should preserve cache_control, request body: %s",
				got.Body,
			)
		}
		if !bytes.Contains(got.Body, []byte(`"model":"`+anthropicModelID+`"`)) {
			t.Errorf(
				"the model received by the Anthropic upstream should be %q, request body: %s",
				anthropicModelID,
				got.Body,
			)
		}
		if headers := got.Headers; headers.Get("x-api-key") != upstreamAPIKey {
			t.Errorf("Anthropic upstream x-api-key expected %q, got %q", upstreamAPIKey, headers.Get("x-api-key"))
		}
		if got := got.Headers.Get("anthropic-version"); got == "" {
			t.Error("the Anthropic upstream is missing the anthropic-version header")
		}
	})

	t.Run("5. missing max_tokens returns 400 / a bad key returns 401", func(t *testing.T) {
		status, _, body := postAnthropicMessage(t, gw.baseURL, apiKeyPlaintext, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusBadRequest {
			t.Fatalf("missing max_tokens expected 400, got %d, response: %s", status, body)
		}
		if !bytes.Contains(body, []byte("invalid_request_error")) {
			t.Errorf("the 400 response should be invalid_request_error, response: %s", body)
		}

		status, _, body = postAnthropicMessage(t, gw.baseURL, "sk-this-key-does-not-exist", map[string]any{
			"model":      openAIModelID,
			"max_tokens": 64,
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusUnauthorized {
			t.Fatalf("a bad key expected 401, got %d, response: %s", status, body)
		}
		if !bytes.Contains(body, []byte("authentication_error")) {
			t.Errorf("the 401 response should be authentication_error, response: %s", body)
		}
	})

	t.Run("6. /v1/messages usage is persisted asynchronously", func(t *testing.T) {
		db := openE2EDB(t, dsn)
		waitFor(t, "database reachable", usagePollTimeout, db.Ping)

		waitFor(
			t,
			fmt.Sprintf("/v1/messages usage is persisted (ai_model_id=%d)", openAIModelRowID),
			usagePollTimeout,
			func() error {
				var rows int
				err := db.QueryRow(
					`SELECT COUNT(*) FROM llm_usage_logs WHERE ai_model_id = ? AND total_tokens > 0`,
					openAIModelRowID,
				).Scan(&rows)
				if err != nil {
					return err
				}
				if rows < 1 {
					return fmt.Errorf("currently %d rows, expected >= 1", rows)
				}
				return nil
			},
		)
	})
}
