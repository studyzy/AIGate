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

// 本文件钉住决策模型入口（/v1/decisions、/v1/systemone 及无版本别名）的治理路径与守卫：
// 鉴权、模型解析、配额、协议白名单、流式拒绝、以及用量记账口径。

import (
	"net/http"
	"strings"
	"testing"

	"aigate/internal/model"
)

const decisionSystemOneReply = `{"model":"jev-1.14.0","answers":{` +
	`"reachable":{"type":"noul","noul":0.97}},"usage":{"input_tokens":40,"output_tokens":7}}`

const decisionSystemOneRequestBody = `{
  "model": "up-decision",
  "state": "hello",
  "questions": {"reachable": {"type": "noul", "instructions": "Is it reachable?"}}
}`

// newDecisionEnv 构造一个绑定决策模型的密钥；modelType 为其上游协议。
func newDecisionEnv(t *testing.T, modelType string, respond func(w http.ResponseWriter, r *http.Request)) (*env, string) {
	t.Helper()
	e := newEnv(t, respond)
	m := newAIModel("up-decision", e.upstream.URL+"/v1")
	m.ModelType = modelType
	m.BillingMode = model.BillingModeInputOnly
	mustCreateAIModel(t, e.store, m)
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))
	return e, token
}

// postDecision 向被测网关发出一次决策请求。
func (e *env) postDecision(t *testing.T, path, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func TestDecisionIngress_SystemOneToSystemOne(t *testing.T) {
	e, token := newDecisionEnv(t, model.ModelTypeSystemOne, respondConstantJSON(http.StatusOK, decisionSystemOneReply))

	resp := e.postDecision(t, "/v1/systemone", token, decisionSystemOneRequestBody)
	assertStatus(t, resp, http.StatusOK)
	out := readBody(t, resp)
	if !strings.Contains(out, `"noul":0.97`) {
		t.Fatalf("response = %s", out)
	}
	if got := e.rec.Calls(); got != 1 {
		t.Fatalf("upstream calls = %d", got)
	}
	if got := e.rec.Path(); got != "/v1/systemone" {
		t.Fatalf("upstream path = %q", got)
	}
	// 上游收到的 model 必须是解析出的上游原始 ID，而不是客户端写的别名。
	if got := e.rec.Body()["model"]; got != "up-decision" {
		t.Fatalf("upstream model = %v", got)
	}
	if _, ok := e.rec.Body()["questions"].(map[string]any); !ok {
		t.Fatalf("upstream questions must stay an object: %v", e.rec.Body()["questions"])
	}
}

// 无版本别名必须与原路径产生逐字节一致的上游请求。
func TestDecisionIngress_SystemOneAlias(t *testing.T) {
	e, token := newDecisionEnv(t, model.ModelTypeSystemOne, respondConstantJSON(http.StatusOK, decisionSystemOneReply))

	assertStatus(t, e.postDecision(t, "/v1/systemone", token, decisionSystemOneRequestBody), http.StatusOK)
	first := e.rec.RawBody()
	assertStatus(t, e.postDecision(t, "/systemone", token, decisionSystemOneRequestBody), http.StatusOK)
	second := e.rec.RawBody()
	if string(first) != string(second) {
		t.Fatalf("alias request body differs:\n/v1: %s\n   : %s", first, second)
	}
}

func TestDecisionIngress_StreamRejected(t *testing.T) {
	e, token := newDecisionEnv(t, model.ModelTypeSystemOne, respondConstantJSON(http.StatusOK, decisionSystemOneReply))

	body := strings.Replace(decisionSystemOneRequestBody, `"model"`, `"stream": true, "model"`, 1)
	resp := e.postDecision(t, "/v1/systemone", token, body)
	assertStatus(t, resp, http.StatusBadRequest)
	if got := readBody(t, resp); !strings.Contains(got, "streaming is not supported") {
		t.Fatalf("body = %s", got)
	}
	if got := e.rec.Calls(); got != 0 {
		t.Fatalf("upstream must not be called, got %d calls", got)
	}
}

func TestDecisionIngress_NonDecisionModel(t *testing.T) {
	// 密钥绑定的是 chat 模型，打到决策入口必须被协议守卫拒绝。
	e, token := newDecisionEnv(t, model.ModelTypeOpenAI, respondConstantJSON(http.StatusOK, decisionSystemOneReply))

	resp := e.postDecision(t, "/v1/systemone", token, decisionSystemOneRequestBody)
	assertStatus(t, resp, http.StatusBadRequest)
	if got := readBody(t, resp); !strings.Contains(got, "does not support decisions") {
		t.Fatalf("body = %s", got)
	}
	if got := e.rec.Calls(); got != 0 {
		t.Fatalf("upstream must not be called, got %d calls", got)
	}
}

func TestDecisionIngress_QuotaExceeded(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, decisionSystemOneReply))
	m := newAIModel("up-decision", e.upstream.URL+"/v1")
	m.ModelType = model.ModelTypeSystemOne
	m.BillingMode = model.BillingModeInputOnly
	m.QuotaDay = 100
	mustCreateAIModel(t, e.store, m)
	// 当日已用满 100 token：模型级日配额随即拒绝。
	if err := e.store.UpsertDailyUsage(ctx(), &model.LLMUsageLog{
		UserID: e.user.ID, AIModelID: m.ID, Model: m.ModelID, PromptTokens: 100,
		CompletionTokens: 0, TotalTokens: 100,
	}); err != nil {
		t.Fatalf("seed daily usage: %v", err)
	}
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postDecision(t, "/v1/systemone", token, decisionSystemOneRequestBody)
	assertStatus(t, resp, http.StatusTooManyRequests)
	if got := e.rec.Calls(); got != 0 {
		t.Fatalf("upstream must not be called, got %d calls", got)
	}
}

func TestDecisionIngress_Unauthorized(t *testing.T) {
	e, _ := newDecisionEnv(t, model.ModelTypeSystemOne, respondConstantJSON(http.StatusOK, decisionSystemOneReply))

	resp := e.postDecision(t, "/v1/systemone", "", decisionSystemOneRequestBody)
	assertStatus(t, resp, http.StatusUnauthorized)
}

// 上游非 200 原样透传给客户端，不做包装。
func TestDecisionIngress_UpstreamErrorPassthrough(t *testing.T) {
	const upstreamErr = `{"code":"decision_refused","message":"refused"}`
	e, token := newDecisionEnv(t, model.ModelTypeSystemOne, respondConstantJSON(http.StatusUnprocessableEntity, upstreamErr))

	resp := e.postDecision(t, "/v1/systemone", token, decisionSystemOneRequestBody)
	assertStatus(t, resp, http.StatusUnprocessableEntity)
	if got := readBody(t, resp); got != upstreamErr {
		t.Fatalf("body = %s", got)
	}
}

// /v1/decisions 入口落到 systemone 上游：请求被翻译成 state + questions 对象。
func TestDecisionIngress_DecisionsToSystemOne(t *testing.T) {
	e, token := newDecisionEnv(t, model.ModelTypeSystemOne, respondConstantJSON(http.StatusOK, decisionSystemOneReply))

	body := `{"model":"up-decision","input":"hello",
	  "questions":[{"type":"predicate","name":"reachable","instructions":"Is it reachable?"}]}`
	resp := e.postDecision(t, "/v1/decisions", token, body)
	assertStatus(t, resp, http.StatusOK)
	out := readBody(t, resp)
	if !strings.Contains(out, `"answers":[`) || !strings.Contains(out, `"name":"reachable"`) {
		t.Fatalf("response must use the decisions envelope: %s", out)
	}
	if got := e.rec.Path(); got != "/v1/systemone" {
		t.Fatalf("upstream path = %q", got)
	}
	if got := e.rec.Body()["state"]; got != "hello" {
		t.Fatalf("upstream state = %v", got)
	}
	questions, ok := e.rec.Body()["questions"].(map[string]any)
	if !ok {
		t.Fatalf("upstream questions must be an object: %v", e.rec.Body()["questions"])
	}
	if _, ok := questions["reachable"]; !ok {
		t.Fatalf("question name lost: %v", questions)
	}
}

// chat 系入口命中决策模型必须 400，不能落到 GetProvider 的 chat 兜底把请求发给上游。
func TestChatRejectsDecisionModel(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, `{}`))
	m := newAIModel("up-decision", e.upstream.URL+"/v1")
	m.ModelType = model.ModelTypeSystemOne
	mustCreateAIModel(t, e.store, m)
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postChat(t, token, `{"model":"up-decision","messages":[{"role":"user","content":"hi"}]}`, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	if got := readBody(t, resp); !strings.Contains(got, "does not support chat completions") {
		t.Fatalf("body = %s", got)
	}
	if got := e.rec.Calls(); got != 0 {
		t.Fatalf("upstream must not be called, got %d calls", got)
	}
}

func TestMessagesRejectsDecisionModel(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, `{}`))
	m := newAIModel("up-decision", e.upstream.URL+"/v1")
	m.ModelType = model.ModelTypeDecisions
	mustCreateAIModel(t, e.store, m)
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postDecision(t, "/v1/messages", token,
		`{"model":"up-decision","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	assertStatus(t, resp, http.StatusBadRequest)
	if got := readBody(t, resp); !strings.Contains(got, "does not support chat completions") {
		t.Fatalf("body = %s", got)
	}
	if got := e.rec.Calls(); got != 0 {
		t.Fatalf("upstream must not be called, got %d calls", got)
	}
}

func TestResponsesRejectsDecisionModel(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, `{}`))
	m := newAIModel("up-decision", e.upstream.URL+"/v1")
	m.ModelType = model.ModelTypeSystemOne
	mustCreateAIModel(t, e.store, m)
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postDecision(t, "/v1/responses", token, `{"model":"up-decision","input":"hi","store":false}`)
	assertStatus(t, resp, http.StatusBadRequest)
	if got := readBody(t, resp); !strings.Contains(got, "does not support chat completions") {
		t.Fatalf("body = %s", got)
	}
	if got := e.rec.Calls(); got != 0 {
		t.Fatalf("upstream must not be called, got %d calls", got)
	}
}

// input_only 计费：落库 completion=0、total=prompt；响应体里的 usage 保持上游原值。
func TestDecisionIngress_InputOnlyUsage(t *testing.T) {
	e, token := newDecisionEnv(t, model.ModelTypeSystemOne, respondConstantJSON(http.StatusOK, decisionSystemOneReply))

	resp := e.postDecision(t, "/v1/systemone", token, decisionSystemOneRequestBody)
	assertStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	if !strings.Contains(body, `"output_tokens":7`) {
		t.Fatalf("client must see upstream usage verbatim: %s", body)
	}

	e.stopUsage()
	db := openReadDB(t, e.dsn)
	var row model.LLMUsageLog
	if err := db.Order("id DESC").First(&row).Error; err != nil {
		t.Fatalf("read usage log: %v", err)
	}
	if row.PromptTokens != 40 || row.CompletionTokens != 0 || row.TotalTokens != 40 {
		t.Fatalf("usage row = prompt %d completion %d total %d, want 40/0/40",
			row.PromptTokens, row.CompletionTokens, row.TotalTokens)
	}
}

// per_token 计费：如实记录上游返回的 completion。
func TestDecisionIngress_PerTokenUsage(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, decisionSystemOneReply))
	m := newAIModel("up-decision", e.upstream.URL+"/v1")
	m.ModelType = model.ModelTypeSystemOne
	m.BillingMode = model.BillingModePerToken
	mustCreateAIModel(t, e.store, m)
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postDecision(t, "/v1/systemone", token, decisionSystemOneRequestBody)
	assertStatus(t, resp, http.StatusOK)
	readBody(t, resp)

	e.stopUsage()
	db := openReadDB(t, e.dsn)
	var row model.LLMUsageLog
	if err := db.Order("id DESC").First(&row).Error; err != nil {
		t.Fatalf("read usage log: %v", err)
	}
	if row.PromptTokens != 40 || row.CompletionTokens != 7 || row.TotalTokens != 47 {
		t.Fatalf("usage row = prompt %d completion %d total %d, want 40/7/47",
			row.PromptTokens, row.CompletionTokens, row.TotalTokens)
	}
}
