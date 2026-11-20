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

package proxy_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"aigate/internal/model"
)

// 本文件是 POST /v1/responses 无状态入口的黑盒用例（Task 6）。
// 有状态路径（previous_response_id / conversation / background / 落库）留到 Task 8/9，
// 本任务对这些字段一律 400。

// responsesPath 是被测网关的 Responses 入口路径。
const responsesPath = "/v1/responses"

// responsesOKChatBody 是一条最小 chat 响应（文本 + usage）。
const responsesOKChatBody = `{"id":"chatcmpl-1","object":"chat.completion","model":"mock-a","choices":[` +
	`{"index":0,"message":{"role":"assistant","content":"hello there"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`

// okResponsesBody 是一条最小 Responses 响应（上游直通用）。
const okResponsesBody = `{"id":"resp_upstream_1","object":"response","model":"mock-responses",` +
	`"status":"completed","output":[{"type":"message","role":"assistant",` +
	`"content":[{"type":"output_text","text":"native answer"}]}],` +
	`"usage":{"input_tokens":4,"output_tokens":6,"total_tokens":10}}`

// newResponsesAIModel 构造一个 openai-responses 类型的上游模型（未入库）。
func newResponsesAIModel(modelID, upstreamURL string) *model.AIModel {
	m := newAIModel(modelID, upstreamURL)
	m.ModelType = model.ModelTypeResponses
	// Responses 上游路径已含 /v1，baseURL 直接拼接 /responses。
	m.URL = upstreamURL + "/v1"
	return m
}

// postResponses 向被测网关发出一次 /v1/responses 请求。
func (e *env) postResponses(t *testing.T, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.server.URL+responsesPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build responses request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("do responses request: %v", err)
	}
	return resp
}

// decodeResponsesBody 解析 Responses 响应体。
func decodeResponsesBody(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode responses body %q: %v", body, err)
	}
	return out
}

// responsesText 取出 output[0].content[0].text。
func responsesText(t *testing.T, resp map[string]any) string {
	t.Helper()
	output, _ := resp["output"].([]any)
	if len(output) == 0 {
		t.Fatalf("output empty: %v", resp)
	}
	item, _ := output[0].(map[string]any)
	content, _ := item["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("output[0].content empty: %v", item)
	}
	part, _ := content[0].(map[string]any)
	text, _ := part["text"].(string)
	return text
}

// sseEventTypes 按出现顺序返回 SSE 流中的事件类型（取 data 行的 type 字段）。
func sseEventTypes(t *testing.T, stream string) []string {
	t.Helper()
	var types []string
	for _, line := range strings.Split(stream, "\n") {
		rest, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload := strings.TrimSpace(rest)
		var ev map[string]any
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		if typ, _ := ev["type"].(string); typ != "" {
			types = append(types, typ)
		}
	}
	return types
}

// responsesLatestUsage 排空用量队列后返回最新一条用量记录。
func responsesLatestUsage(t *testing.T, e *env) model.LLMUsageLog {
	t.Helper()
	e.stopUsage()
	return latestUsageLog(t, openReadDB(t, e.dsn))
}

// 异步观测说明：网关在请求处理末尾异步投递用量日志，且 httptest 服务器在响应写完后才提交
// upstreamRequest 记录——两者都与客户端收到响应存在竞争。凡是「请求根本没有触达上游」
// 或「上游收到了第 N 次请求」的断言，都必须先等这条链路走完，否则会读到过期值。
// 等待手段：排空用量写入器（e.stopUsage）后再断言，或轮询 upstreamRequest.Calls()。

// waitUpstreamBody 等待 mock 上游收到第 want 次请求并返回其请求体。
//
// 必要性：网关先写响应、后投递用量日志，而 httptest 服务器在响应写完后才提交 upstreamRequest
// 记录——直接读 e.rec.Body() 会与这次提交竞争，读到最后一次尝试遗留的 nil 或旧值。
func waitUpstreamBody(t *testing.T, e *env, want int) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if e.rec.Calls() >= want {
			if body := e.rec.Body(); body != nil {
				return body
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("upstream did not record %d request(s) with a parsed body (calls=%d)", want, e.rec.Calls())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// 1. 基础成功路径（chat 上游 → Responses 响应）
// ---------------------------------------------------------------------------

func TestResponsesIngress_BasicCompletion(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postResponses(t, token, `{"model":"mock-a","input":"hi"}`)
	assertStatus(t, resp, http.StatusOK)
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	body := readBody(t, resp)
	out := decodeResponsesBody(t, body)
	if out["object"] != "response" {
		t.Fatalf("object = %v, want %q (body=%s)", out["object"], "response", body)
	}
	id, _ := out["id"].(string)
	if !strings.HasPrefix(id, "resp_") || len(id) != len("resp_")+32 {
		t.Fatalf("id = %q, want resp_ + 32 hex chars", id)
	}
	if text := responsesText(t, out); text != "hello there" {
		t.Fatalf("output[0].content[0].text = %q, want %q", text, "hello there")
	}
	if out["model"] != "mock-a" {
		t.Fatalf("model = %v, want the client-requested %q", out["model"], "mock-a")
	}

	// 上游必须收到 chat 形状的请求（messages），而不是 Responses 形状（input）。
	up := e.rec.Body()
	if _, ok := up["messages"]; !ok {
		t.Fatalf("upstream body missing messages: %v", up)
	}
	if _, ok := up["input"]; ok {
		t.Fatalf("chat upstream must not receive a Responses-shaped input: %v", up)
	}

	row := responsesLatestUsage(t, e)
	if row.StatusCode != http.StatusOK {
		t.Fatalf("usage.status_code = %d, want 200", row.StatusCode)
	}
	if row.PromptTokens != 3 || row.CompletionTokens != 2 || row.TotalTokens != 5 {
		t.Fatalf("usage tokens = %+v, want prompt=3 completion=2 total=5", row)
	}
}

// ---------------------------------------------------------------------------
// 2. 鉴权
// ---------------------------------------------------------------------------

func TestResponsesIngress_MissingAPIKey(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	t.Run("no Authorization header", func(t *testing.T) {
		resp := e.postResponses(t, "", `{"model":"mock-a","input":"hi"}`)
		assertErrorResponse(t, resp, http.StatusUnauthorized, "missing API key")
	})

	t.Run("unknown token", func(t *testing.T) {
		resp := e.postResponses(t, "sk-does-not-exist", `{"model":"mock-a","input":"hi"}`)
		assertErrorResponse(t, resp, http.StatusUnauthorized, "invalid API key")
	})

	t.Run("valid token reaches upstream", func(t *testing.T) {
		resp := e.postResponses(t, token, `{"model":"mock-a","input":"hi"}`)
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)
	})

	if e.rec.Calls() != 1 {
		t.Fatalf("upstream calls = %d, want 1 (unauthenticated requests must not be forwarded)", e.rec.Calls())
	}
}

// ---------------------------------------------------------------------------
// 3. 配额
// ---------------------------------------------------------------------------

func TestResponsesIngress_QuotaExceeded(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("quota-m", e.upstream.URL))
	key, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	// 预置一条该密钥的用量，使密钥级 day=1 限额立即超限。
	if _, err := e.store.UpdateAPIKey(ctx(), key.ID, map[string]any{
		"token_quota_rules": `[{"mode":"day","limit":1}]`,
	}); err != nil {
		t.Fatalf("update key rules: %v", err)
	}
	if err := e.store.InsertUsageLog(ctx(), &model.LLMUsageLog{
		APIKeyID:    key.ID,
		UserID:      e.user.ID,
		TotalTokens: 5,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert usage: %v", err)
	}

	resp := e.postResponses(t, token, `{"model":"quota-m","input":"hi"}`)
	assertErrorResponse(t, resp, http.StatusTooManyRequests, "Key token quota exceeded")
	secs, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || secs <= 0 {
		t.Fatalf("Retry-After = %q, want a positive integer number of seconds", resp.Header.Get("Retry-After"))
	}
	if e.rec.Calls() != 0 {
		t.Fatalf("quota-exceeded request must not reach upstream, got %d calls", e.rec.Calls())
	}
}

// ---------------------------------------------------------------------------
// 3b. 密钥上限（★ 顺序契约 §5.4.1：Responses 形态 body 先压制，再转 chat）
// ---------------------------------------------------------------------------

// TestResponsesIngress_KeyMaxTokensCap：转换路径（Responses 请求 → chat 上游）上，
// 密钥上限必须最终体现为发往 chat 上游的 max_tokens。
//
// 判别力说明（重要，勿高估本用例）：本用例只能钉住「转换路径最终发出了上限值」，
// **不能**证明 §5.4.1 第 4 步（转换前对 Responses 形态 body 压制）执行过。
// 因为转换路径的第 6 步 prepareForward 还会对 chat 形态的 max_tokens 再压一次，
// 即使第 4 步被整段删掉，本用例依然通过（实测变异）。
// 真正钉住第 4 步的是 TestResponsesIngress_PassthroughCapsMaxOutputTokens
// （直通路径不经 chat 转换，第 6 步不参与）。
func TestResponsesIngress_KeyMaxTokensCap(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	const keyLimit = 16
	// 显式不限的组：排除全局默认配额规则对本用例的干扰（否则请求会在上游前被拦下或改值）。
	unlimited := &model.Group{Name: "responses-unlimited", TokenQuotaRules: `[]`}
	if err := e.store.CreateGroup(ctx(), unlimited); err != nil {
		t.Fatalf("create group: %v", err)
	}
	_, token := mustCreateKeyWithGroup(t, e.store, e.user.ID, unlimited.ID, keyLimit, primary(m.ID, 0))

	t.Run("explicit max_output_tokens above the key limit is capped", func(t *testing.T) {
		resp := e.postResponses(t, token, `{"model":"mock-a","input":"hi","max_output_tokens":9000}`)
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)
		upstreamBody := waitUpstreamBody(t, e, 1)

		cap, _ := bodyNumber(t, upstreamBody, "max_tokens")
		if cap != keyLimit {
			t.Fatalf("upstream max_tokens = %v, want key limit %d (max_output_tokens must be capped before conversion)",
				cap, keyLimit)
		}
	})

	t.Run("non-positive max_output_tokens is capped", func(t *testing.T) {
		resp := e.postResponses(t, token, `{"model":"mock-a","input":"hi","max_output_tokens":-1}`)
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)
		upstreamBody := waitUpstreamBody(t, e, 2)

		cap, _ := bodyNumber(t, upstreamBody, "max_tokens")
		if cap != keyLimit {
			t.Fatalf("upstream max_tokens = %v, want key limit %d (-1 means unlimited upstream)", cap, keyLimit)
		}
	})

	t.Run("missing max_output_tokens gets the key limit", func(t *testing.T) {
		resp := e.postResponses(t, token, `{"model":"mock-a","input":"hi"}`)
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)
		upstreamBody := waitUpstreamBody(t, e, 3)

		cap, ok := bodyNumber(t, upstreamBody, "max_tokens")
		if !ok || cap != keyLimit {
			t.Fatalf("upstream max_tokens = %v (present=%v), want key limit %d", cap, ok, keyLimit)
		}
	})
}

// upstreamErrorBody 是 mock chat 上游返回的 4xx 错误体（含上游自己的错误详情）。
const upstreamErrorBody = `{"error":{"message":"max_tokens: 999999 exceeds the model limit",` +
	`"type":"invalid_request_error","param":"max_tokens","code":"context_length_exceeded"}}`

// TestResponsesIngress_UpstreamErrorBodyPassedThrough：转换路径上，上游非 200 时
// 状态码与响应体都要原样回给客户端——与流式路径、直通路径一致。
//
// 丢弃上游错误体会让调用方看不到真正原因（如 context_length_exceeded），
// 也造成「同一请求流式与非流式行为不一致」。
func TestResponsesIngress_UpstreamErrorBodyPassedThrough(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusBadRequest, upstreamErrorBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postResponses(t, token, `{"model":"mock-a","input":"hi"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", resp.StatusCode, readBody(t, resp))
	}
	if got := readBody(t, resp); got != upstreamErrorBody {
		t.Fatalf("upstream error body must be passed through verbatim:\n got %q\nwant %q",
			got, upstreamErrorBody)
	}
}

// TestResponsesIngress_UpstreamErrorWithoutBody：上游返回 4xx 但**没有响应体**时，
// 网关必须回一个 OpenAI 风格的错误体（{"error":{...}}），而不是一段纯文本。
//
// 原因：这条响应恒定带 Content-Type: application/json；若回纯文本，客户端按 JSON 解析会失败，
// 拿到的错误信息随之丢失。回错误体也保持了「错误响应恒为 {"error":{...}}」的入口契约。
func TestResponsesIngress_UpstreamErrorWithoutBody(t *testing.T) {
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // 无 body
	})
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postResponses(t, token, `{"model":"mock-a","input":"hi"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", resp.StatusCode, readBody(t, resp))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	body := readBody(t, resp)
	var eb struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &eb); err != nil {
		t.Fatalf("body must be JSON when Content-Type is application/json, decode failed: %v (body=%q)", err, body)
	}
	if eb.Error.Message == "" {
		t.Fatalf("error.message must not be empty, body=%q", body)
	}
	if eb.Error.Type != "upstream_error" {
		t.Fatalf("error.type = %q, want %q", eb.Error.Type, "upstream_error")
	}
}

// TestResponsesIngress_ChatUpstreamCapOnlyOnce：转换路径上，客户端给的大值既不能透出，
// 也不能被二次改写；发往 chat 上游的 max_tokens 恰为密钥上限。
//
// 判别力边界（重要）：本用例**不能**单独证明转换路径那道兜底压制存在。
// 正常流下第 4 步已把 max_output_tokens 收敛为上限、转换器再映射成 max_tokens，
// 因此单删兜底压制（变异 D）时本用例仍然通过——那是两层压制互为掩护，不是缺口：
//
//	· 删第 4 步（变异 A）→ 由 TestResponsesIngress_PassthroughCapsMaxOutputTokens 捕获；
//	· 第 4 步与兜底压制同时删除（A+D）→ 本用例与 KeyMaxTokensCap 同时变红；
//	· 兜底压制单独存在时的真实价值是兜住「转换器漏映射 max_output_tokens」：
//	  实测仅在转换器漏映射时上游仍得到上限；转换器漏映射且兜底被删时上游 max_tokens
//	  直接缺失，本用例变红——那时兜底压制才是唯一在起作用的一道。
func TestResponsesIngress_ChatUpstreamCapOnlyOnce(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	const keyLimit = 16
	unlimited := &model.Group{Name: "responses-chat-cap", TokenQuotaRules: `[]`}
	if err := e.store.CreateGroup(ctx(), unlimited); err != nil {
		t.Fatalf("create group: %v", err)
	}
	_, token := mustCreateKeyWithGroup(t, e.store, e.user.ID, unlimited.ID, keyLimit, primary(m.ID, 0))

	resp := e.postResponses(t, token, `{"model":"mock-a","input":"hi","max_output_tokens":9000}`)
	assertStatus(t, resp, http.StatusOK)
	_ = readBody(t, resp)

	up := waitUpstreamBody(t, e, 1)
	if _, ok := up["max_output_tokens"]; ok {
		t.Fatalf("chat upstream must not receive max_output_tokens: %v", up)
	}
	capped, ok := bodyNumber(t, up, "max_tokens")
	if !ok {
		t.Fatalf("chat upstream body missing max_tokens: %v", up)
	}
	if capped != keyLimit {
		t.Fatalf("chat upstream max_tokens = %v, want key limit %d (client's 9000 must never be forwarded)",
			capped, keyLimit)
	}
}

// ---------------------------------------------------------------------------
// 4. 格 7：openai-responses 上游直通（非流式）
// ---------------------------------------------------------------------------

func TestResponsesIngress_PassthroughToResponsesUpstream(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okResponsesBody))
	m := mustCreateAIModel(t, e.store, newResponsesAIModel("mock-responses", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postResponses(t, token, `{"model":"mock-responses","input":"hi","store":true}`)
	assertStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)

	// 上游必须收到 Responses 形状的请求（input，不含 messages），且 store 被强制改写为 false。
	up := e.rec.Body()
	if _, ok := up["input"]; !ok {
		t.Fatalf("responses upstream body missing input: %v", up)
	}
	if _, ok := up["messages"]; ok {
		t.Fatalf("responses upstream must NOT receive chat messages (no conversion allowed): %v", up)
	}
	if store, ok := up["store"]; !ok || store != false {
		t.Fatalf("upstream body.store = %v (present=%v), want forced false", store, ok)
	}

	// 响应原样透出，仅 id 改写为网关 ID。
	out := decodeResponsesBody(t, body)
	id, _ := out["id"].(string)
	if !strings.HasPrefix(id, "resp_") || id == "resp_upstream_1" {
		t.Fatalf("response.id = %q, want a gateway-generated resp_ id", id)
	}
	if text := responsesText(t, out); text != "native answer" {
		t.Fatalf("passthrough output text = %q, want %q", text, "native answer")
	}

	row := responsesLatestUsage(t, e)
	if row.PromptTokens != 4 || row.CompletionTokens != 6 || row.TotalTokens != 10 {
		t.Fatalf("usage tokens = %+v, want prompt=4 completion=6 total=10", row)
	}
}

// TestResponsesIngress_PassthroughCapsMaxOutputTokens：直通路径上 max_output_tokens 同样受密钥上限保护。
//
// 这条用例与 TestResponsesIngress_KeyMaxTokensCap 互补：转换路径上 prepareForward 会对 chat 形态的
// max_tokens 再压一次，掩盖第 4 步是否执行；直通路径不经过 chat 转换，字段自始至终是 max_output_tokens，
// 因此只有「§5.4.1 第 4 步对 Responses 形态 body 压制」能把它收敛到上限。
func TestResponsesIngress_PassthroughCapsMaxOutputTokens(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okResponsesBody))
	m := mustCreateAIModel(t, e.store, newResponsesAIModel("mock-responses", e.upstream.URL))
	unlimited := &model.Group{Name: "responses-passthrough-unlimited", TokenQuotaRules: `[]`}
	if err := e.store.CreateGroup(ctx(), unlimited); err != nil {
		t.Fatalf("create group: %v", err)
	}
	const keyLimit = 16
	_, token := mustCreateKeyWithGroup(t, e.store, e.user.ID, unlimited.ID, keyLimit, primary(m.ID, 0))

	t.Run("explicit max_output_tokens above the key limit is capped", func(t *testing.T) {
		resp := e.postResponses(t, token, `{"model":"mock-responses","input":"hi","max_output_tokens":9000}`)
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)

		cap, ok := bodyNumber(t, waitUpstreamBody(t, e, 1), "max_output_tokens")
		if !ok || cap != keyLimit {
			t.Fatalf("upstream max_output_tokens = %v (present=%v), want key limit %d", cap, ok, keyLimit)
		}
	})

	t.Run("missing max_output_tokens gets the key limit", func(t *testing.T) {
		resp := e.postResponses(t, token, `{"model":"mock-responses","input":"hi"}`)
		assertStatus(t, resp, http.StatusOK)
		_ = readBody(t, resp)

		cap, ok := bodyNumber(t, waitUpstreamBody(t, e, 2), "max_output_tokens")
		if !ok || cap != keyLimit {
			t.Fatalf("upstream max_output_tokens = %v (present=%v), want key limit %d", cap, ok, keyLimit)
		}
	})
}

// ---------------------------------------------------------------------------
// 5. 有状态字段本轮一律 400（Task 8/9 放开）
// ---------------------------------------------------------------------------

// TestResponsesIngress_BackgroundAbsentIsSynchronous：background 缺省 / false / null 与不传等价，
// 走同步路径并直接返回 completed。
//
// 本用例原名 TestResponsesIngress_BackgroundRejected（Task 9 之前 background 一律 400）。
// 那两条 400 断言描述的是「尚未实现」的临时状态，本任务有意推翻（Ruling 14 的同一条理由），
// 因此改写而不是保留；background:true 的完整验收见 responses_background_test.go。
func TestResponsesIngress_BackgroundAbsentIsSynchronous(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	for name, body := range map[string]string{
		"false": `{"model":"mock-a","input":"hi","background":false}`,
		"null":  `{"model":"mock-a","input":"hi","background":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			resp := e.postResponses(t, token, body)
			assertStatus(t, resp, http.StatusOK)
			out := decodeResponsesBody(t, readBody(t, resp))
			if got, _ := out["status"].(string); got != "completed" {
				t.Fatalf("status = %q, want completed (synchronous path)", got)
			}
		})
	}
	if e.rec.Calls() != 2 {
		t.Fatalf("upstream calls = %d, want 2 (both requests run synchronously)", e.rec.Calls())
	}
}

// ---------------------------------------------------------------------------
// 6. 流式（chat 上游 → 合成的 Responses 事件序列）
// ---------------------------------------------------------------------------

// chatSSEStream 是 mock chat 上游的流式响应：两个内容分片 + 收尾分片 + [DONE]。
const chatSSEStream = "data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"choices\":[" +
	"{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"choices\":[" +
	"{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"choices\":[" +
	"{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"choices\":[]," +
	"\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n" +
	"data: [DONE]\n\n"

// respondChatSSE 返回一个流式 chat 上游处理器。
func respondChatSSE() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(chatSSEStream))
		w.(http.Flusher).Flush()
	}
}

func TestResponsesIngress_Streaming(t *testing.T) {
	e := newEnv(t, respondChatSSE())
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postResponses(t, token, `{"model":"mock-a","input":"hi","stream":true}`)
	assertStatus(t, resp, http.StatusOK)
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	stream := readBody(t, resp)

	if strings.Contains(stream, "[DONE]") {
		t.Fatalf("responses stream must never contain [DONE], got:\n%s", stream)
	}

	types := sseEventTypes(t, stream)
	if len(types) < 2 {
		t.Fatalf("event types = %v, want at least created + completed (stream=%s)", types, stream)
	}
	if types[0] != "response.created" {
		t.Fatalf("first event = %q, want response.created (types=%v)", types[0], types)
	}
	if last := types[len(types)-1]; last != "response.completed" {
		t.Fatalf("last event = %q, want response.completed (types=%v)", last, types)
	}

	// created 与 completed 的 id 必须一致，且为网关生成的 resp_ ID。
	createdID := sseResponseID(t, stream, "response.created")
	completedID := sseResponseID(t, stream, "response.completed")
	if !strings.HasPrefix(createdID, "resp_") {
		t.Fatalf("created response.id = %q, want resp_ prefix", createdID)
	}
	if createdID != completedID {
		t.Fatalf("created id %q != completed id %q", createdID, completedID)
	}
	if !strings.Contains(stream, `"delta":"Hel"`) || !strings.Contains(stream, `"delta":"lo"`) {
		t.Fatalf("stream should carry both text deltas, got:\n%s", stream)
	}

	row := responsesLatestUsage(t, e)
	if row.PromptTokens != 7 || row.CompletionTokens != 2 || row.TotalTokens != 9 {
		t.Fatalf("usage tokens = %+v, want prompt=7 completion=2 total=9", row)
	}
}

// sseResponseID 取出指定事件类型所在的 response 对象 id（兼容扁平与 response 嵌套两种形状）。
func sseResponseID(t *testing.T, stream, eventType string) string {
	t.Helper()
	for _, line := range strings.Split(stream, "\n") {
		rest, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(rest)), &ev) != nil {
			continue
		}
		if ev["type"] != eventType {
			continue
		}
		if nested, ok := ev["response"].(map[string]any); ok {
			id, _ := nested["id"].(string)
			return id
		}
		id, _ := ev["id"].(string)
		return id
	}
	t.Fatalf("event %q not found in stream:\n%s", eventType, stream)
	return ""
}

// TestResponsesIngress_StreamingWithoutDeltas_stillCompletes：上游 2xx 但零增量（只发 [DONE]）时，
// 仍按 SSE 收尾为完整事件序列，绝不回退 502。
//
// 钉住 T5 契约：502 的依据是「上游状态码异常」，不是 Wrote() 为假——Finish() 在零增量时
// 依然会写出 created/in_progress/completed，若据此回退就会得到「既有 SSE 事件体、又带 502」的矛盾响应。
func TestResponsesIngress_StreamingWithoutDeltas_stillCompletes(t *testing.T) {
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		w.(http.Flusher).Flush()
	})
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postResponses(t, token, `{"model":"mock-a","input":"hi","stream":true}`)
	assertStatus(t, resp, http.StatusOK)
	stream := readBody(t, resp)

	if strings.Contains(stream, "502") || strings.Contains(stream, "failed to connect") {
		t.Fatalf("upstream 2xx must not fall back to 502, got:\n%s", stream)
	}
	types := sseEventTypes(t, stream)
	if len(types) == 0 || types[0] != "response.created" {
		t.Fatalf("first event = %v, want response.created (stream=%s)", types, stream)
	}
	if last := types[len(types)-1]; last != "response.completed" {
		t.Fatalf("last event = %q, want response.completed (types=%v)", last, types)
	}
	if strings.Contains(stream, "[DONE]") {
		t.Fatalf("responses stream must never contain [DONE], got:\n%s", stream)
	}
}

// ---------------------------------------------------------------------------
// 7. 格 7 流式：openai-responses 上游直通 SSE + 终态 id 改写 + 用量记账
// ---------------------------------------------------------------------------

// responsesSSEStream 是 mock Responses 上游的 SSE：原样透传，终态事件带 usage。
const responsesSSEStream = "event: response.created\n" +
	"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_up_1\",\"object\":\"response\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
	"event: response.output_text.delta\n" +
	"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"delta\":\"nat\"}\n\n" +
	"event: response.completed\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_up_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":11,\"output_tokens\":13,\"total_tokens\":24}}}\n\n"

// TestResponsesIngress_StreamingPassthroughKeepsUpstreamDone：上游**自己发了** [DONE] 时，
// 直通路径原样转发，不主动删除。
//
// 取舍理由：§5.4.2 第 4 条只管「上游没发 [DONE] 时不补发」；上游既然发了，那就是上游的数据，
// 网关的职责是透传而非裁剪——静默删掉上游数据属于「转换丢字段却不报错」的反目标。
// 注意这与转换路径不同：转换路径（chat 上游）的 [DONE] 由 chat→responses 翻译器吞掉，
// 因为 Responses 客户端不认这个终止符；直通路径没有翻译器，故不做此处理。
func TestResponsesIngress_StreamingPassthroughKeepsUpstreamDone(t *testing.T) {
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(responsesSSEStream + "data: [DONE]\n\n"))
		w.(http.Flusher).Flush()
	})
	m := mustCreateAIModel(t, e.store, newResponsesAIModel("mock-responses", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postResponses(t, token, `{"model":"mock-responses","input":"hi","stream":true}`)
	assertStatus(t, resp, http.StatusOK)
	stream := readBody(t, resp)

	if !strings.Contains(stream, "data: [DONE]") {
		t.Fatalf("upstream-sent [DONE] must be forwarded verbatim, got:\n%s", stream)
	}
	// 透传不应影响终态事件的改写与用量抓取。
	if id := sseResponseID(t, stream, "response.completed"); !strings.HasPrefix(id, "resp_") || id == "resp_up_1" {
		t.Fatalf("completed response.id = %q, want a gateway-generated resp_ id", id)
	}
	row := responsesLatestUsage(t, e)
	if row.PromptTokens != 11 || row.CompletionTokens != 13 || row.TotalTokens != 24 {
		t.Fatalf("usage tokens = %+v, want prompt=11 completion=13 total=24", row)
	}
}

func TestResponsesIngress_StreamingPassthrough(t *testing.T) {
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(responsesSSEStream))
		w.(http.Flusher).Flush()
	})
	m := mustCreateAIModel(t, e.store, newResponsesAIModel("mock-responses", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postResponses(t, token, `{"model":"mock-responses","input":"hi","stream":true}`)
	assertStatus(t, resp, http.StatusOK)
	stream := readBody(t, resp)

	// event:/data: 行原样透传。
	if !strings.Contains(stream, "event: response.output_text.delta") {
		t.Fatalf("passthrough must keep upstream event: lines, got:\n%s", stream)
	}
	if !strings.Contains(stream, `"delta":"nat"`) {
		t.Fatalf("passthrough must keep upstream data payloads, got:\n%s", stream)
	}
	// 上游本次没有发 [DONE]：网关不得「凭空补发」。
	// （上游真发了 [DONE] 时会原样转发，见 TestResponsesIngress_StreamingPassthroughKeepsUpstreamDone。）
	if strings.Contains(stream, "[DONE]") {
		t.Fatalf("gateway must not append [DONE] when upstream sent none, got:\n%s", stream)
	}

	// 上游模型名已替换、store 强制 false。
	up := e.rec.Body()
	if up["model"] != "mock-responses" {
		t.Fatalf("upstream body.model = %v, want %q", up["model"], "mock-responses")
	}
	if store, ok := up["store"]; !ok || store != false {
		t.Fatalf("upstream body.store = %v (present=%v), want forced false", store, ok)
	}
	if _, ok := up["input"]; !ok {
		t.Fatalf("responses upstream body missing input: %v", up)
	}

	// 终态事件的 id 改写为网关 resp_ ID。
	id := sseResponseID(t, stream, "response.completed")
	if !strings.HasPrefix(id, "resp_") || id == "resp_up_1" {
		t.Fatalf("completed response.id = %q, want a gateway-generated resp_ id", id)
	}

	row := responsesLatestUsage(t, e)
	if row.PromptTokens != 11 || row.CompletionTokens != 13 || row.TotalTokens != 24 {
		t.Fatalf("usage tokens = %+v, want prompt=11 completion=13 total=24", row)
	}
}
