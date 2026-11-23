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
	"database/sql"
	"encoding/json"
	"errors"
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

// ---------------------------------------------------------------------------
// M5：Responses API（无状态 / 有状态 / 生命周期 / 跨用户隔离）
// ---------------------------------------------------------------------------
// ---------------------------------------------------------------------------

// responsesEnvelope 是 POST /v1/responses 与 GET /v1/responses/{id} 回包中本用例断言所需的最小字段。
type responsesEnvelope struct {
	ID     string `json:"id"`
	Object string `json:"object"`
	Status string `json:"status"`
	Model  string `json:"model"`
	Store  *bool  `json:"store"`
	Error  any    `json:"error"`
	Output []struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
}

// outputText 拼接全部 output_text 片段的文本。
func (r *responsesEnvelope) outputText() string {
	var sb strings.Builder
	for _, item := range r.Output {
		for _, c := range item.Content {
			if c.Type == "output_text" {
				sb.WriteString(c.Text)
			}
		}
	}
	return sb.String()
}

// postResponses 向数据面发起一次 POST /v1/responses。
func postResponses(t *testing.T, baseURL, key string, payload map[string]any) (int, []byte) {
	t.Helper()
	return doRequest(t, http.MethodPost, baseURL+"/v1/responses", key, mustJSON(t, payload))
}

// mustResponsesEnvelope 解析 Responses 回包，并断言状态码为 200。
func mustResponsesEnvelope(t *testing.T, status int, body []byte, what string) *responsesEnvelope {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("%s expected 200, got %d, response: %s", what, status, body)
	}
	var resp responsesEnvelope
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("%s returned an unparseable body: %v, response: %s", what, err, body)
	}
	return &resp
}

// responseStateRow 是直连 MySQL 读到的 response_states 行。
type responseStateRow struct {
	ID                 string
	OwnerUserID        int64
	OwnerAPIKeyID      int64
	RequestModel       string
	PreviousResponseID string
	Status             string
	Items              string
}

// queryResponseState 点查 response_states；行不存在时返回 nil（不算错误）。
func queryResponseState(t *testing.T, db *sql.DB, id string) *responseStateRow {
	t.Helper()
	row := &responseStateRow{}
	err := db.QueryRow(
		`SELECT id, owner_user_id, owner_api_key_id, request_model, previous_response_id, status, items`+
			` FROM response_states WHERE id = ?`, id,
	).Scan(&row.ID, &row.OwnerUserID, &row.OwnerAPIKeyID, &row.RequestModel, &row.PreviousResponseID,
		&row.Status, &row.Items)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatalf("failed to query response_states(id=%s): %v", id, err)
	}
	return row
}

// requireResponseState 点查 response_states 并断言行存在。
func requireResponseState(t *testing.T, db *sql.DB, id string) *responseStateRow {
	t.Helper()
	row := queryResponseState(t, db, id)
	if row == nil {
		t.Fatalf("response_states has no row for id=%s, but the response should have been persisted", id)
	}
	return row
}

// itemsContain 报告 response_states.items 快照里是否出现了 want 这段文本。
func itemsContain(items, want string) bool {
	return strings.Contains(items, want)
}

// responsesMockInput 记录 mock 上游（chat 协议）收到的本轮请求体。
//
// 注意：openai-completions 上游走的是「Responses → chat」转换路径，因此上游看到的是
// chat 形态的 messages，而不是 Responses 形态的 input（后者只存在于直通 openai-responses 上游）。
type responsesMockInput struct {
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
}

// flattenUserTurns 按顺序拼接 messages 里的用户文本，用于断言历史确实被拼进了本轮请求。
func flattenUserTurns(raw []byte) (string, error) {
	var payload responsesMockInput
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, msg := range payload.Messages {
		if msg.Role != "user" {
			continue
		}
		switch content := msg.Content.(type) {
		case string:
			sb.WriteString(content)
		case []any:
			for _, part := range content {
				if p, ok := part.(map[string]any); ok {
					if text, ok := p["text"].(string); ok {
						sb.WriteString(text)
					}
				}
			}
		}
	}
	return sb.String(), nil
}

func TestE2E_M5_ResponsesAPI(t *testing.T) {
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	openAIMock := newUpstreamMock(kindOpenAI)
	t.Cleanup(openAIMock.Close)

	gw := startGateway(t, bin, dsn)
	adminAuth := adminToken

	runs := responsesRuns{
		firstTurn:  "m5-first-turn",
		secondTurn: "m5-second-turn",
		bgTurn:     "m5-background-turn",
	}

	var (
		modelRowID int64
		userAID    int64
		userBID    int64
		keyA       string
		keyB       string
	)

	t.Run("1. prepare: a model, two users and one key per user", func(t *testing.T) {
		modelRowID = createModel(t, gw.baseURL, adminAuth, map[string]any{
			"provider":   "mock",
			"model_id":   openAIModelID,
			"model_name": openAIModelID,
			"url":        openAIMock.URL() + "/v1",
			"api_key":    upstreamAPIKey,
			"model_type": "openai-completions",
			"quota_day":  -1,
			"enabled":    true,
		})

		userAID, keyA = m5CreateUserAndKey(t, gw.baseURL, adminAuth, "m5-user-a", modelRowID)
		userBID, keyB = m5CreateUserAndKey(t, gw.baseURL, adminAuth, "m5-user-b", modelRowID)
		if userAID == userBID {
			t.Fatalf("the two users should get different ids, both got %d", userAID)
		}
		t.Logf("model row id=%d, userA=%d, userB=%d", modelRowID, userAID, userBID)
	})

	t.Run("2. basic call: output carries output_text and the id starts with resp_", func(t *testing.T) {
		status, body := postResponses(t, gw.baseURL, keyA, map[string]any{
			"model": openAIModelID,
			"input": runs.firstTurn,
		})
		resp := mustResponsesEnvelope(t, status, body, "POST /v1/responses (basic)")
		if !strings.HasPrefix(resp.ID, "resp_") {
			t.Errorf("response id should start with resp_, got %q", resp.ID)
		}
		if resp.Status != "completed" {
			t.Errorf("response status should be completed, got %q (response: %s)", resp.Status, body)
		}
		if got := resp.outputText(); got != openAIContent {
			t.Errorf("output_text should be %q, got %q (response: %s)", openAIContent, got, body)
		}

		got := openAIMock.lastRequest()
		if len(got.Body) == 0 {
			t.Fatal("the OpenAI mock received no request body")
		}
	})

	t.Run("3. store defaults to true and the row lands in response_states", func(t *testing.T) {
		db := openE2EDB(t, dsn)

		status, body := postResponses(t, gw.baseURL, keyA, map[string]any{
			"model": openAIModelID,
			"input": runs.firstTurn,
		})
		resp := mustResponsesEnvelope(t, status, body, "POST /v1/responses (store default)")
		if resp.Store == nil || !*resp.Store {
			t.Fatalf("the response should echo store:true when store is omitted, got %v (response: %s)",
				resp.Store, body)
		}

		row := requireResponseState(t, db, resp.ID)
		if row.OwnerUserID != userAID {
			t.Errorf("response_states.owner_user_id should be %d, got %d", userAID, row.OwnerUserID)
		}
		if row.Status != "completed" {
			t.Errorf("response_states.status should be completed, got %q", row.Status)
		}
		if !itemsContain(row.Items, runs.firstTurn) {
			t.Errorf("response_states.items should contain the request input %q, got %s", runs.firstTurn, row.Items)
		}
		if resp.ID != row.ID {
			t.Errorf("the echoed id should equal the persisted row id, got %q vs %q", resp.ID, row.ID)
		}

		explicitStatus, explicitBody := postResponses(t, gw.baseURL, keyA, map[string]any{
			"model": openAIModelID,
			"input": runs.firstTurn,
			"store": true,
		})
		explicit := mustResponsesEnvelope(t, explicitStatus, explicitBody, "POST /v1/responses (store:true)")
		if explicit.Store == nil || !*explicit.Store {
			t.Fatalf("the response should echo store:true when store:true is sent, got %v (response: %s)",
				explicit.Store, explicitBody)
		}
		requireResponseState(t, db, explicit.ID)
		t.Logf("persisted response ids: %s, %s", resp.ID, explicit.ID)
	})

	t.Run("4. store:false does not persist a row", func(t *testing.T) {
		db := openE2EDB(t, dsn)

		status, body := postResponses(t, gw.baseURL, keyA, map[string]any{
			"model": openAIModelID,
			"input": runs.firstTurn,
			"store": false,
		})
		resp := mustResponsesEnvelope(t, status, body, "POST /v1/responses (store:false)")
		if resp.Store == nil || *resp.Store {
			t.Fatalf("the response should echo store:false, got %v (response: %s)", resp.Store, body)
		}
		if row := queryResponseState(t, db, resp.ID); row != nil {
			t.Fatalf("response_states must not contain id=%s under store:false, but a row exists: %+v", resp.ID, row)
		}
		if got := resp.outputText(); got != openAIContent {
			t.Errorf("a store:false call should still return the converted output, got %q", got)
		}
	})

	t.Run("5. previous_response_id replays the earlier turns to the upstream", func(t *testing.T) {
		db := openE2EDB(t, dsn)

		firstStatus, firstBody := postResponses(t, gw.baseURL, keyA, map[string]any{
			"model":        openAIModelID,
			"input":        runs.firstTurn,
			"store":        true,
			"instructions": "m5-system-prompt",
		})
		first := mustResponsesEnvelope(t, firstStatus, firstBody, "POST /v1/responses (turn 1)")
		firstRow := requireResponseState(t, db, first.ID)
		if !itemsContain(firstRow.Items, openAIContent) {
			t.Fatalf("the turn-1 snapshot should contain the assistant output %q, got %s",
				openAIContent, firstRow.Items)
		}
		// instructions 不应进入历史快照：它每轮都会作为 system 重新注入，存进历史会重复。
		if itemsContain(firstRow.Items, "m5-system-prompt") {
			t.Errorf("the items snapshot must not contain instructions, got %s", firstRow.Items)
		}

		secondStatus, secondBody := postResponses(t, gw.baseURL, keyA, map[string]any{
			"model":                openAIModelID,
			"input":                runs.secondTurn,
			"previous_response_id": first.ID,
		})
		second := mustResponsesEnvelope(t, secondStatus, secondBody, "POST /v1/responses (turn 2)")
		if second.ID == first.ID {
			t.Fatalf("the second turn should get a new response id, both are %q", first.ID)
		}

		secondRow := requireResponseState(t, db, second.ID)
		if secondRow.PreviousResponseID != first.ID {
			t.Errorf("response_states.previous_response_id should be %q, got %q",
				first.ID, secondRow.PreviousResponseID)
		}
		for _, want := range []string{runs.firstTurn, openAIContent, runs.secondTurn} {
			if !itemsContain(secondRow.Items, want) {
				t.Errorf("the turn-2 snapshot should accumulate %q, got %s", want, secondRow.Items)
			}
		}

		// 主判据：mock 上游收到的本轮请求体里必须带上上一轮的内容（历史确实被拼接）。
		received := openAIMock.lastRequest()
		if len(received.Body) == 0 {
			t.Fatal("the OpenAI mock received no request body")
		}
		turns, err := flattenUserTurns(received.Body)
		if err != nil {
			t.Fatalf("failed to parse the upstream request body: %v, request body: %s", err, received.Body)
		}
		for _, want := range []string{runs.firstTurn, runs.secondTurn} {
			if !strings.Contains(turns, want) {
				t.Errorf("the upstream request should contain %q after continuation, got user turns %q (request body: %s)",
					want, turns, received.Body)
			}
		}
		if strings.Contains(turns, "m5-system-prompt") {
			t.Errorf("instructions should go out as the system prompt, not as a user turn, got %q", turns)
		}
		// 顺序也必须是「上一轮用户 → 上一轮助手 → 本轮用户」，history 前置于 input。
		if firstIdx, secondIdx := strings.Index(turns, runs.firstTurn), strings.Index(turns, runs.secondTurn); firstIdx > secondIdx {
			t.Errorf("the history should be prepended before the current turn, got %q", turns)
		}
		if !bytes.Contains(received.Body, []byte(`"role":"assistant"`)) {
			t.Errorf("the upstream request should replay the previous assistant turn, request body: %s", received.Body)
		}
	})

	t.Run("6. background:true returns queued immediately and completes asynchronously", func(t *testing.T) {
		db := openE2EDB(t, dsn)

		status, body := postResponses(t, gw.baseURL, keyA, map[string]any{
			"model":      openAIModelID,
			"input":      runs.bgTurn,
			"background": true,
		})
		resp := mustResponsesEnvelope(t, status, body, "POST /v1/responses (background)")
		if resp.Status != "queued" {
			t.Fatalf("a background response should return status queued, got %q (response: %s)", resp.Status, body)
		}
		if !strings.HasPrefix(resp.ID, "resp_") {
			t.Errorf("a background response id should start with resp_, got %q", resp.ID)
		}

		// 行必须已经存在（回包之前建好，见 startResponsesBackground）；此后状态只会向后推进，
		// 因此这里只断言「至少进入过已受理态」，不要求恰好停在 queued。
		row := requireResponseState(t, db, resp.ID)
		if row.Status != "queued" && row.Status != "in_progress" && row.Status != "completed" {
			t.Errorf("background row status should be queued/in_progress/completed, got %q", row.Status)
		}
		if row.OwnerUserID != userAID {
			t.Errorf("background row owner_user_id should be %d, got %d", userAID, row.OwnerUserID)
		}

		// 异步等待用带超时的轮询，不用固定 sleep。
		var final *responsesEnvelope
		waitFor(t, "GET /v1/responses/{id} reaches a terminal status", usagePollTimeout, func() error {
			gotStatus, gotBody := doRequest(t, http.MethodGet, gw.baseURL+"/v1/responses/"+resp.ID, keyA, nil)
			if gotStatus != http.StatusOK {
				return fmt.Errorf("GET /v1/responses/%s returned %d: %s", resp.ID, gotStatus, gotBody)
			}
			var current responsesEnvelope
			if err := json.Unmarshal(gotBody, &current); err != nil {
				return fmt.Errorf("unparseable GET body: %w (%s)", err, gotBody)
			}
			switch current.Status {
			case "completed":
				final = &current
				return nil
			case "failed", "cancelled", "incomplete":
				return fmt.Errorf("background run ended in unexpected status %q: %s", current.Status, gotBody)
			default:
				return fmt.Errorf("still %q", current.Status)
			}
		})
		if final == nil {
			t.Fatal("background polling finished without a completed response")
		}
		if got := final.outputText(); got != openAIContent {
			t.Errorf("the background result output_text should be %q, got %q", openAIContent, got)
		}

		waitFor(t, "response_states row reaches status completed", usagePollTimeout, func() error {
			current := requireResponseState(t, db, resp.ID)
			if current.Status != "completed" {
				return fmt.Errorf("row status is %q, want completed", current.Status)
			}
			return nil
		})
		completedRow := requireResponseState(t, db, resp.ID)
		if !itemsContain(completedRow.Items, runs.bgTurn) {
			t.Errorf("the completed row items should contain %q, got %s", runs.bgTurn, completedRow.Items)
		}
	})

	t.Run("7. GET / DELETE / cancel lifecycle endpoints", func(t *testing.T) {
		db := openE2EDB(t, dsn)

		status, body := postResponses(t, gw.baseURL, keyA, map[string]any{
			"model": openAIModelID,
			"input": runs.firstTurn,
			"store": true,
		})
		resp := mustResponsesEnvelope(t, status, body, "POST /v1/responses (for GET)")
		requireResponseState(t, db, resp.ID)

		// GET 取回同一份响应体。
		getStatus, getBody := doRequest(t, http.MethodGet, gw.baseURL+"/v1/responses/"+resp.ID, keyA, nil)
		got := mustResponsesEnvelope(t, getStatus, getBody, "GET /v1/responses/{id}")
		if got.ID != resp.ID {
			t.Errorf("GET should return the same id %q, got %q", resp.ID, got.ID)
		}
		if gotText := got.outputText(); gotText != openAIContent {
			t.Errorf("GET output_text should be %q, got %q (response: %s)", openAIContent, gotText, getBody)
		}

		// DELETE 终态行成功，删除后 GET 一律 404。
		delStatus, delBody := doRequest(t, http.MethodDelete, gw.baseURL+"/v1/responses/"+resp.ID, keyA, nil)
		if delStatus != http.StatusOK {
			t.Fatalf("DELETE a completed response expected 200, got %d, response: %s", delStatus, delBody)
		}
		var del struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Deleted bool   `json:"deleted"`
		}
		if err := json.Unmarshal(delBody, &del); err != nil {
			t.Fatalf("failed to parse DELETE response: %v, response: %s", err, delBody)
		}
		if del.ID != resp.ID || !del.Deleted || del.Object != "response.deleted" {
			t.Errorf("DELETE response mismatch: %+v (body: %s)", del, delBody)
		}
		if row := queryResponseState(t, db, resp.ID); row != nil {
			t.Errorf("the row should be gone after DELETE, still present: %+v", row)
		}
		if getStatus, getBody := doRequest(t, http.MethodGet, gw.baseURL+"/v1/responses/"+resp.ID, keyA, nil); getStatus != http.StatusNotFound {
			t.Errorf("GET after DELETE expected 404, got %d, response: %s", getStatus, getBody)
		}

		// cancel 对已终态行返回 400（并回带当前对象）；在途行走 200 分支，由后台完成时间决定，
		// 在 e2e 里无法稳定复现，因此这里只覆盖确定性的终态分支。
		cancelStatus, cancelBody := doRequest(t, http.MethodPost, gw.baseURL+"/v1/responses/"+resp.ID+"/cancel", keyA, nil)
		if cancelStatus != http.StatusNotFound {
			// 行已被上一段删除，因此这里期望的是 404 —— 与「不存在」同一出口。
			t.Errorf("cancel on a deleted response expected 404, got %d, response: %s", cancelStatus, cancelBody)
		}

		// 新建一条已完成的行，再对「终态」执行 cancel，断言 400 + 当前对象。
		status, body = postResponses(t, gw.baseURL, keyA, map[string]any{
			"model": openAIModelID,
			"input": runs.firstTurn,
			"store": true,
		})
		terminal := mustResponsesEnvelope(t, status, body, "POST /v1/responses (for cancel)")
		cancelStatus, cancelBody = doRequest(t,
			http.MethodPost, gw.baseURL+"/v1/responses/"+terminal.ID+"/cancel", keyA, nil)
		if cancelStatus != http.StatusBadRequest {
			t.Fatalf("cancel on a completed response expected 400, got %d, response: %s", cancelStatus, cancelBody)
		}
		var conflict struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Response struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"response"`
		}
		if err := json.Unmarshal(cancelBody, &conflict); err != nil {
			t.Fatalf("failed to parse the cancel conflict body: %v, response: %s", err, cancelBody)
		}
		if conflict.Error.Message == "" {
			t.Errorf("the cancel conflict body should carry an error message, got %s", cancelBody)
		}
		if conflict.Response.ID != terminal.ID || conflict.Response.Status != "completed" {
			t.Errorf("the cancel conflict body should carry the current object (%q/completed), got %s",
				terminal.ID, cancelBody)
		}
	})

	t.Run("8. cross-user isolation: another user sees 404", func(t *testing.T) {
		db := openE2EDB(t, dsn)

		status, body := postResponses(t, gw.baseURL, keyA, map[string]any{
			"model": openAIModelID,
			"input": runs.firstTurn,
			"store": true,
		})
		owned := mustResponsesEnvelope(t, status, body, "POST /v1/responses (owner A)")
		requireResponseState(t, db, owned.ID)

		// 用 user B 的密钥续接 user A 的响应：必须 404。
		status, body = postResponses(t, gw.baseURL, keyB, map[string]any{
			"model":                openAIModelID,
			"input":                runs.secondTurn,
			"previous_response_id": owned.ID,
		})
		if status != http.StatusNotFound {
			t.Fatalf("continuing another user's response expected 404, got %d, response: %s", status, body)
		}

		// 同一个 404 也必须覆盖 GET / DELETE / cancel，否则可按状态码差异探测他人 ID 是否存在。
		for _, tc := range []struct {
			method string
			path   string
		}{
			{http.MethodGet, "/v1/responses/" + owned.ID},
			{http.MethodDelete, "/v1/responses/" + owned.ID},
			{http.MethodPost, "/v1/responses/" + owned.ID + "/cancel"},
		} {
			gotStatus, gotBody := doRequest(t, tc.method, gw.baseURL+tc.path, keyB, nil)
			if gotStatus != http.StatusNotFound {
				t.Errorf("%s %s by another user expected 404, got %d, response: %s",
					tc.method, tc.path, gotStatus, gotBody)
			}
		}

		// 原主仍然能读到自己的行（隔离没有误伤）。
		if getStatus, getBody := doRequest(t, http.MethodGet, gw.baseURL+"/v1/responses/"+owned.ID, keyA, nil); getStatus != http.StatusOK {
			t.Errorf("the owner should still read its own response, got %d, response: %s", getStatus, getBody)
		}
		if row := queryResponseState(t, db, owned.ID); row == nil || row.OwnerUserID != userAID {
			t.Errorf("the row should still be owned by user %d, got %+v", userAID, row)
		}
	})
}

// responsesRuns 汇总本组用例使用的各轮输入文本，便于断言历史是否被拼接。
type responsesRuns struct {
	firstTurn  string
	secondTurn string
	bgTurn     string
}

// m5CreateUserAndKey 创建用户并为它签发一把绑定到 modelID 的数据面密钥。
func m5CreateUserAndKey(t *testing.T, baseURL, adminAuth, username string, modelID int64) (int64, string) {
	t.Helper()

	status, body := doRequest(t, http.MethodPost, baseURL+"/admin/users", adminAuth, mustJSON(t, map[string]any{
		"username": username, "role": "user",
	}))
	if status != http.StatusCreated {
		t.Fatalf("POST /admin/users(%s) expected 201, got %d, response: %s", username, status, body)
	}
	var user m2UserResponse
	if err := json.Unmarshal(body, &user); err != nil || user.ID == 0 {
		t.Fatalf("failed to parse /admin/users response: %v, response: %s", err, body)
	}

	status, body = doRequest(t, http.MethodPost, baseURL+"/admin/keys", adminAuth, mustJSON(t, map[string]any{
		"name":     username + "-key",
		"user_id":  user.ID,
		"bindings": []map[string]any{{"ai_model_id": modelID, "role": "primary"}},
	}))
	if status != http.StatusCreated {
		t.Fatalf("POST /admin/keys(%s) expected 201, got %d, response: %s", username, status, body)
	}
	var key createKeyResponse
	if err := json.Unmarshal(body, &key); err != nil || key.Token == "" {
		t.Fatalf("failed to parse /admin/keys response: %v, response: %s", err, body)
	}
	return user.ID, key.Token
}
