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
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aigate/internal/billing"
	"aigate/internal/model"
	"aigate/internal/proxy"
)

// ---------------------------------------------------------------------------
// auto 路由夹具
// ---------------------------------------------------------------------------

// autoEnv 在标准 env 之上提供 auto 路由所需的专属构造：
// 一个决策协议上游、一个 chat 上游、以及价格来源装配。
type autoEnv struct {
	*env
	// decision 是决策模型上游（记录请求并按 resp 应答）。
	decision *httptest.Server
	decRec   *upstreamRequest
}

// newAutoEnv 组装 auto 用例夹具。decisionRespond 为 nil 时决策上游返回一个
// 固定指向 "chosen" 的合法应答。
func newAutoEnv(t *testing.T, decisionRespond func(http.ResponseWriter, *http.Request)) *autoEnv {
	t.Helper()
	if decisionRespond == nil {
		decisionRespond = respondConstantJSON(http.StatusOK,
			`{"model":"decider","answers":[{"type":"choice","name":"auto","choice":"chosen-model"}]}`)
	}
	e := newEnv(t, respondConstantJSON(http.StatusOK,
		`{"id":"chatcmpl","object":"chat.completion","model":"upstream","choices":[`+
			`{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))

	decSrv, decRec := startUpstream(t, decisionRespond)

	return &autoEnv{env: e, decision: decSrv, decRec: decRec}
}

// enableAuto 建一条全局 decision 路由（决策模型 ID + 默认阈值）。
//
// 这是迁移后 auto 的启用方式：配置从 settings 列搬到了 router.config。
func (a *autoEnv) enableAuto(t *testing.T, decisionModelID uint) *model.ModelRouter {
	t.Helper()
	return a.createRouter(t, model.RouterScopeGlobal, 0, 0, decisionModelID)
}

// createRouter 建一条 decision 路由并返回它。
func (a *autoEnv) createRouter(
	t *testing.T, scope string, scopeID uint, priority int, decisionModelID uint,
) *model.ModelRouter {
	t.Helper()
	cfg, err := json.Marshal(map[string]any{
		"decision_model_id":     decisionModelID,
		"max_candidates":        8,
		"max_short_tokens":      200,
		"max_context_tokens":    8000,
		"max_chars":             8000,
		"timeout_ms":            2000,
		"cooldown_seconds":      30,
		"short_circuit_enabled": true,
	})
	if err != nil {
		t.Fatalf("marshal router config: %v", err)
	}
	r := &model.ModelRouter{
		Name:     "test-router",
		Strategy: "decision",
		Config:   string(cfg),
		Scope:    scope,
		ScopeID:  scopeID,
		Priority: priority,
		Enabled:  true,
	}
	if err := a.store.CreateModelRouter(ctx(), r); err != nil {
		t.Fatalf("create model router: %v", err)
	}
	return r
}

// updateRouterConfig 改一条路由的配置项（合并到现有 JSON）。
func (a *autoEnv) updateRouterConfig(t *testing.T, router *model.ModelRouter, patch map[string]any) {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal([]byte(router.Config), &cfg); err != nil {
		t.Fatalf("parse router config: %v", err)
	}
	for k, v := range patch {
		cfg[k] = v
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal router config: %v", err)
	}
	if _, err := a.store.UpdateModelRouter(ctx(), router.ID, map[string]any{"config": string(b)}); err != nil {
		t.Fatalf("update router config: %v", err)
	}
}

// mustCreateDecisionModel 入库一个决策协议模型（指向决策上游）。
func (a *autoEnv) mustCreateDecisionModel(t *testing.T, modelID string) *model.AIModel {
	t.Helper()
	m := &model.AIModel{
		Provider:          "mock-decider",
		ModelID:           modelID,
		ModelName:         "name-" + modelID,
		URL:               a.decision.URL + "/v1",
		APIKey:            "decider-secret",
		ModelType:         model.ModelTypeDecisions,
		CustomHTTPHeaders: "",
		QuotaDay:          -1,
		Enabled:           true,
	}
	return mustCreateAIModel(t, a.store, m)
}

// mustCreateChatModel 入库一个 chat 模型（指向同一个 mock 上游）。
func (a *autoEnv) mustCreateChatModel(t *testing.T, modelID string) *model.AIModel {
	t.Helper()
	return mustCreateAIModel(t, a.store, newAIModel(modelID, a.upstream.URL+"/v1"))
}

// autoPriceTime 是价目快照的生效时刻：取一个过去的固定时间，确保"当前"必然取到它。
func autoPriceTime() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// addPrice 给模型追加一份价目快照（输入未命中与输出同价，便于比较）。
func (a *autoEnv) addPrice(t *testing.T, aiModelID uint, priceMicros int64) {
	t.Helper()
	p := &model.AIModelPrice{
		AIModelID:              aiModelID,
		Currency:               "CNY",
		EffectiveAt:            autoPriceTime(),
		PeakHours:              "{}",
		PeakInputMissMicros:    priceMicros,
		OffpeakInputMissMicros: priceMicros,
		PeakOutputMicros:       priceMicros,
		OffpeakOutputMicros:    priceMicros,
	}
	if err := a.store.CreateAIModelPrice(ctx(), p); err != nil {
		t.Fatalf("create model price: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

// auto 未启用时：/v1/models 不列出 auto，请求 auto 返回 400 而非 500 或误解模型。
func TestAuto_Disabled(t *testing.T) {
	a := newAutoEnv(t, nil)
	decider := a.mustCreateDecisionModel(t, "decider")
	cheap := a.mustCreateChatModel(t, "cheap-model")
	key, token := mustCreateKey(t, a.store, a.user.ID, 0,
		primary(decider.ID, 0), fallback(cheap.ID, 1))

	resp := a.getModels(t, token)
	assertStatus(t, resp, http.StatusOK)
	if body := readBody(t, resp); strings.Contains(body, `"auto"`) {
		t.Fatalf("/v1/models must not list auto when disabled: %s", body)
	}

	resp = a.postChat(t, token, reqWithModel("auto"), nil)
	assertStatus(t, resp, http.StatusBadRequest)
	if body := readBody(t, resp); !strings.Contains(body, "auto") {
		t.Fatalf("error should mention auto: %s", body)
	}
	_ = key
}

// 启用后 auto 出现在 /v1/models 最前，且 owned_by 为 aigate。
func TestAuto_ListedFirst(t *testing.T) {
	a := newAutoEnv(t, nil)
	decider := a.mustCreateDecisionModel(t, "decider")
	cheap := a.mustCreateChatModel(t, "cheap-model")
	a.enableAuto(t, decider.ID)
	_, token := mustCreateKey(t, a.store, a.user.ID, 0,
		primary(decider.ID, 0), fallback(cheap.ID, 1))

	resp := a.getModels(t, token)
	assertStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	idxAuto := strings.Index(body, `"id":"auto"`)
	idxOther := strings.Index(body, `"id":"cheap-model"`)
	if idxAuto < 0 {
		t.Fatalf("auto should be listed: %s", body)
	}
	if idxOther >= 0 && idxAuto > idxOther {
		t.Fatalf("auto should be listed first: %s", body)
	}
	if !strings.Contains(body, `"owned_by":"aigate"`) {
		t.Fatalf("auto should be owned by aigate: %s", body)
	}
}

// 单候选：不调决策模型，直接用该候选（省一次决策费用）。
func TestAuto_SingleCandidateSkipsDecision(t *testing.T) {
	a := newAutoEnv(t, nil)
	decider := a.mustCreateDecisionModel(t, "decider")
	only := a.mustCreateChatModel(t, "only-model")
	a.enableAuto(t, decider.ID)
	// 决策模型不绑定到密钥：它走站点级解析，不该进入候选池。
	_, token := mustCreateKey(t, a.store, a.user.ID, 0, primary(only.ID, 0))

	resp := a.postChat(t, token, reqWithModel("auto"), nil)
	assertStatus(t, resp, http.StatusOK)
	if got := a.decRec.Calls(); got != 0 {
		t.Fatalf("decision model calls = %d, want 0 for a single candidate", got)
	}
	if got := a.rec.Body()["model"]; got != "only-model" {
		t.Fatalf("upstream model = %v, want only-model", got)
	}
	if routed := resp.Header.Get("X-AIGate-Routed-Model"); routed != "only-model" {
		t.Fatalf("routed header = %q, want only-model", routed)
	}
}

// 决策路径：规则不短路（无简单信号），问决策模型并按其选择转发。
func TestAuto_DecisionPicksModel(t *testing.T) {
	a := newAutoEnv(t, nil)
	decider := a.mustCreateDecisionModel(t, "decider")
	cheap := a.mustCreateChatModel(t, "cheap-model")
	strong := a.mustCreateChatModel(t, "chosen-model")
	a.addPrice(t, cheap.ID, 100)
	a.addPrice(t, strong.ID, 10_000)
	a.enableAuto(t, decider.ID)
	_, token := mustCreateKey(t, a.store, a.user.ID, 0,
		primary(cheap.ID, 0), fallback(strong.ID, 1))

	// 无简单信号 + 多轮对话 → 必须询问决策模型。
	body := `{"model":"auto","messages":[` +
		`{"role":"user","content":"analyze this distributed system"},` +
		`{"role":"assistant","content":"sure"},` +
		`{"role":"user","content":"why did that fail?"}]}`
	resp := a.postChat(t, token, body, nil)
	assertStatus(t, resp, http.StatusOK)

	if a.decRec.Calls() != 1 {
		t.Fatalf("decision calls = %d, want 1", a.decRec.Calls())
	}
	if got := a.rec.Body()["model"]; got != "chosen-model" {
		t.Fatalf("upstream model = %v, want chosen-model", got)
	}
	if routed := resp.Header.Get("X-AIGate-Routed-Model"); routed != "chosen-model" {
		t.Fatalf("routed header = %q, want chosen-model", routed)
	}
	// 题面只写相对档位，不写绝对价格数字（决策模型对绝对单价没有校准能力）。
	raw := string(a.decRec.RawBody())
	if strings.Contains(raw, "10000") {
		t.Fatalf("decision prompt must not carry absolute prices: %s", raw)
	}
	choices := decisionChoices(t, a.decRec.RawBody())
	if got := choices["cheap-model"]; !strings.Contains(got, "cheapest") {
		t.Fatalf("cheap-model description = %q, want it described as cheapest", got)
	}
	if got := choices["chosen-model"]; !strings.Contains(got, "most expensive") {
		t.Fatalf("chosen-model description = %q, want it described as most expensive", got)
	}
}

// decisionChoices 从决策上游收到的题面里取出 choice 选项（键 = 候选键，值 = 描述）。
func decisionChoices(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	var body struct {
		Questions []struct {
			Choices []struct {
				Value       string `json:"value"`
				Description string `json:"description"`
			} `json:"choices"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode decision request: %v (raw=%s)", err, raw)
	}
	if len(body.Questions) == 0 || len(body.Questions[0].Choices) == 0 {
		t.Fatalf("decision request carries no choices: %s", raw)
	}
	out := make(map[string]string, len(body.Questions[0].Choices))
	for _, c := range body.Questions[0].Choices {
		out[c.Value] = c.Description
	}
	return out
}

// 短路路径：命中简单信号且是单轮短消息 → 不问决策模型，直接选最便宜的。
func TestAuto_ShortCircuitPicksCheapest(t *testing.T) {
	a := newAutoEnv(t, nil)
	decider := a.mustCreateDecisionModel(t, "decider")
	cheap := a.mustCreateChatModel(t, "cheap-model")
	pricey := a.mustCreateChatModel(t, "pricey-model")
	a.addPrice(t, cheap.ID, 100)
	a.addPrice(t, pricey.ID, 10_000)
	a.enableAuto(t, decider.ID)
	// sort_order 故意让贵的在前，验证短路按价格而不是绑定顺序。
	_, token := mustCreateKey(t, a.store, a.user.ID, 0,
		primary(pricey.ID, 0), fallback(cheap.ID, 1))

	resp := a.postChat(t, token, reqWithModel("auto"), nil) // "hi" 无信号
	// "hi" 不含简单信号 → 应走决策模型而非短路。改为含信号的内容。
	_ = resp
	a.decRec.mu.Lock()
	a.decRec.calls = 0
	a.decRec.mu.Unlock()

	body := `{"model":"auto","messages":[{"role":"user","content":"把这句话翻译成英文"}]}`
	resp = a.postChat(t, token, body, nil)
	assertStatus(t, resp, http.StatusOK)

	if got := a.decRec.Calls(); got != 0 {
		t.Fatalf("decision calls = %d, want 0 when short-circuiting", got)
	}
	if got := a.rec.Body()["model"]; got != "cheap-model" {
		t.Fatalf("upstream model = %v, want cheap-model (cheapest)", got)
	}
}

// 决策模型不可用（非 200）→ 回退 primary，请求仍成功。
func TestAuto_FallbackToPrimaryOnDecisionError(t *testing.T) {
	a := newAutoEnv(t, respondConstantJSON(http.StatusInternalServerError, `{"error":"boom"}`))
	decider := a.mustCreateDecisionModel(t, "decider")
	primaryModel := a.mustCreateChatModel(t, "primary-model")
	other := a.mustCreateChatModel(t, "other-model")
	a.enableAuto(t, decider.ID)
	_, token := mustCreateKey(t, a.store, a.user.ID, 0,
		primary(primaryModel.ID, 0), fallback(other.ID, 1))

	body := `{"model":"auto","messages":[` +
		`{"role":"user","content":"analyze"},{"role":"assistant","content":"x"},` +
		`{"role":"user","content":"go on"}]}`
	resp := a.postChat(t, token, body, nil)
	assertStatus(t, resp, http.StatusOK)

	if got := a.rec.Body()["model"]; got != "primary-model" {
		t.Fatalf("upstream model = %v, want primary-model (fallback)", got)
	}
	if routed := resp.Header.Get("X-AIGate-Routed-Model"); routed != "primary-model" {
		t.Fatalf("routed header = %q, want primary-model", routed)
	}
}

// 决策模型返回候选集之外的 choice → 回退 primary，不能把请求打成错误。
func TestAuto_FallbackOnUnknownChoice(t *testing.T) {
	a := newAutoEnv(t, respondConstantJSON(http.StatusOK,
		`{"model":"decider","answers":[{"type":"choice","name":"auto","choice":"does-not-exist"}]}`))
	decider := a.mustCreateDecisionModel(t, "decider")
	primaryModel := a.mustCreateChatModel(t, "primary-model")
	other := a.mustCreateChatModel(t, "other-model")
	a.enableAuto(t, decider.ID)
	_, token := mustCreateKey(t, a.store, a.user.ID, 0,
		primary(primaryModel.ID, 0), fallback(other.ID, 1))

	body := `{"model":"auto","messages":[` +
		`{"role":"user","content":"analyze"},{"role":"assistant","content":"x"},` +
		`{"role":"user","content":"go on"}]}`
	resp := a.postChat(t, token, body, nil)
	assertStatus(t, resp, http.StatusOK)
	if got := a.rec.Body()["model"]; got != "primary-model" {
		t.Fatalf("upstream model = %v, want primary-model", got)
	}
}

// auto 绝不影响显式指定模型的请求。
func TestAuto_ExplicitModelUnaffected(t *testing.T) {
	a := newAutoEnv(t, nil)
	decider := a.mustCreateDecisionModel(t, "decider")
	cheap := a.mustCreateChatModel(t, "cheap-model")
	explicit := a.mustCreateChatModel(t, "explicit-model")
	a.addPrice(t, cheap.ID, 1)
	a.addPrice(t, explicit.ID, 10_000)
	a.enableAuto(t, decider.ID)
	_, token := mustCreateKey(t, a.store, a.user.ID, 0,
		primary(cheap.ID, 0), fallback(explicit.ID, 1))

	resp := a.postChat(t, token, reqWithModel("explicit-model"), nil)
	assertStatus(t, resp, http.StatusOK)
	if got := a.rec.Body()["model"]; got != "explicit-model" {
		t.Fatalf("upstream model = %v, want explicit-model", got)
	}
	if a.decRec.Calls() != 0 {
		t.Fatalf("decision model should not be called for explicit models")
	}
	if routed := resp.Header.Get("X-AIGate-Routed-Model"); routed != "" {
		t.Fatalf("routed header should be absent for explicit models, got %q", routed)
	}
}

// 决策调用必须记账（成本可查），但不能计入用户配额（design §7.7 方案 a）。
func TestAuto_DecisionCallBilledButNotCountedTowardQuota(t *testing.T) {
	a := newAutoEnv(t, nil)
	decider := a.mustCreateDecisionModel(t, "decider")
	cheap := a.mustCreateChatModel(t, "cheap-model")
	strong := a.mustCreateChatModel(t, "chosen-model")
	a.enableAuto(t, decider.ID)
	_, token := mustCreateKey(t, a.store, a.user.ID, 0,
		primary(cheap.ID, 0), fallback(strong.ID, 1))

	body := `{"model":"auto","messages":[` +
		`{"role":"user","content":"analyze"},{"role":"assistant","content":"x"},` +
		`{"role":"user","content":"go on"}]}`
	resp := a.postChat(t, token, body, nil)
	assertStatus(t, resp, http.StatusOK)
	a.stopUsage()

	db := openReadDB(t, a.dsn)
	var rows []model.LLMUsageLog
	if err := db.Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("list usage logs: %v", err)
	}
	var counted, uncounted int
	for _, r := range rows {
		if r.CountsTowardQuota() {
			counted++
		} else {
			uncounted++
		}
	}
	// 决策调用一行（不计配额），最终请求一行（计配额）。
	if uncounted < 1 {
		t.Fatalf("expected at least one non-counted usage row, got %d (rows=%d)", uncounted, len(rows))
	}
	if counted < 1 {
		t.Fatalf("expected the forwarded request to be counted, got %d", counted)
	}
}

// 决策模型若是候选之一（例如同时被绑定），也不能被路由到。
func TestAuto_ExcludesDecisionModelFromCandidates(t *testing.T) {
	a := newAutoEnv(t, nil)
	decider := a.mustCreateDecisionModel(t, "decider")
	only := a.mustCreateChatModel(t, "only-model")
	a.enableAuto(t, decider.ID)
	// 决策模型也绑定到该密钥：它必须被候选池剔除，剩 only-model 一个候选。
	_, token := mustCreateKey(t, a.store, a.user.ID, 0,
		primary(only.ID, 0), fallback(decider.ID, 1))

	resp := a.postChat(t, token, reqWithModel("auto"), nil)
	assertStatus(t, resp, http.StatusOK)
	if got := a.rec.Body()["model"]; got != "only-model" {
		t.Fatalf("upstream model = %v, want only-model", got)
	}
	if a.decRec.Calls() != 0 {
		t.Fatalf("decision model should not be called for a single remaining candidate")
	}
}

// 价格装配存在时，候选按价格排序参与决策题面。
func TestAuto_PriceSourceWired(t *testing.T) {
	a := newAutoEnv(t, nil)
	decider := a.mustCreateDecisionModel(t, "decider")
	cheap := a.mustCreateChatModel(t, "cheap-model")
	pricey := a.mustCreateChatModel(t, "pricey-model")
	a.addPrice(t, cheap.ID, 100)
	a.addPrice(t, pricey.ID, 10_000)

	// 装配价目来源（与记账共用同一个 StoreSource）。
	src := billing.NewStoreSource(a.store)
	h := proxy.NewHandler(a.store, a.usage, 429)
	h.SetAutoPriceSource(src)

	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	a.server = srv
	a.client = srv.Client()

	a.enableAuto(t, decider.ID)
	_, token := mustCreateKey(t, a.store, a.user.ID, 0,
		primary(pricey.ID, 0), fallback(cheap.ID, 1))

	body := `{"model":"auto","messages":[{"role":"user","content":"请翻译这段话"}]}`
	resp := a.postChat(t, token, body, nil)
	assertStatus(t, resp, http.StatusOK)
	if got := a.rec.Body()["model"]; got != "cheap-model" {
		t.Fatalf("upstream model = %v, want cheap-model by price", got)
	}
}

// auto_router_max_candidates 必须真的生效（design §7.1）：候选过多时按价格升序截断。
func TestAuto_MaxCandidatesTruncates(t *testing.T) {
	a := newAutoEnv(t, nil)
	decider := a.mustCreateDecisionModel(t, "decider")
	// 上限收紧到 2。
	router := a.enableAuto(t, decider.ID)
	a.updateRouterConfig(t, router, map[string]any{"max_candidates": 2})

	// 5 个候选，价格递增：越靠后越贵，截断后只剩最便宜的两个。
	// primary 必须是一个普通 chat 模型（决策模型会被候选池剔除，不能当 primary 兜底）。
	var bindings []model.APIKeyModel
	for i, id := range []string{"m1", "m2", "m3", "m4", "m5"} {
		m := a.mustCreateChatModel(t, id)
		a.addPrice(t, m.ID, int64(100*(i+1)))
		role := fallback
		if i == 0 {
			role = primary
		}
		bindings = append(bindings, role(m.ID, i))
	}
	_, token := mustCreateKey(t, a.store, a.user.ID, 0, bindings...)

	body := `{"model":"auto","messages":[` +
		`{"role":"user","content":"analyze"},{"role":"assistant","content":"x"},` +
		`{"role":"user","content":"go on"}]}`
	resp := a.postChat(t, token, body, nil)
	assertStatus(t, resp, http.StatusOK)

	choices := decisionChoices(t, a.decRec.RawBody())
	if len(choices) != 2 {
		t.Fatalf("decision choices = %d, want 2 (truncated by max_candidates)", len(choices))
	}
	// 保留的必须是较便宜的两个，且最便宜的 m1 描述为 cheapest。
	if _, ok := choices["m1"]; !ok {
		t.Fatalf("cheapest candidate m1 should survive truncation: %v", choices)
	}
	if _, ok := choices["m5"]; ok {
		t.Fatalf("most expensive m5 should be truncated away: %v", choices)
	}
}

// 非 chat 入口的系统提示必须进入决策题面（design §7.4：state = 对话文本）。
// 只看 messages 会让决策模型在看不见角色设定的情况下被迫路由。
func TestAuto_AnthropicSystemPromptIncluded(t *testing.T) {
	a := newAutoEnv(t, nil)
	decider := a.mustCreateDecisionModel(t, "decider")
	cheap := a.mustCreateChatModel(t, "cheap-model")
	strong := a.mustCreateChatModel(t, "chosen-model")
	a.enableAuto(t, decider.ID)
	_, token := mustCreateKey(t, a.store, a.user.ID, 0,
		primary(cheap.ID, 0), fallback(strong.ID, 1))

	body := `{"model":"auto","system":"You are a strict reviewer.","max_tokens":64,` +
		`"messages":[{"role":"user","content":"thanks"}]}`
	resp := a.postMessagesBearer(t, token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)

	raw := string(a.decRec.RawBody())
	if !strings.Contains(raw, "strict reviewer") {
		t.Fatalf("system prompt should reach the decision model: %s", raw)
	}
}

// 策略配置的默认值必须真正生效：未提供 short_circuit_enabled 时落库应为 true。
// bool 字段无法用零值判断缺省，从零值反序列化会把它静默改成 false。
func TestDecisionStrategyConfigDefaults(t *testing.T) {
	s, err := proxy.StrategyByName("decision")
	if err != nil {
		t.Fatalf("strategy missing: %v", err)
	}
	got, err := s.ValidateConfig(ctx(), `{"decision_model_id":3}`)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(got, &cfg); err != nil {
		t.Fatalf("decode normalized config: %v", err)
	}
	if v, ok := cfg["short_circuit_enabled"].(bool); !ok || !v {
		t.Fatalf("short_circuit_enabled = %v, want true by default", cfg["short_circuit_enabled"])
	}
	if v, _ := cfg["max_candidates"].(float64); v != 8 {
		t.Fatalf("max_candidates = %v, want 8", cfg["max_candidates"])
	}
	// 显式 false 必须保留（不能被默认值覆盖）。
	got, err = s.ValidateConfig(ctx(), `{"decision_model_id":3,"short_circuit_enabled":false}`)
	if err != nil {
		t.Fatalf("validate explicit false: %v", err)
	}
	if err := json.Unmarshal(got, &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v, ok := cfg["short_circuit_enabled"].(bool); !ok || v {
		t.Fatalf("explicit false should be preserved, got %v", cfg["short_circuit_enabled"])
	}
}
