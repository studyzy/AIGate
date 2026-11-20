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
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/model"
	"aigate/internal/proxy"
)

// 本文件是 background:true 异步执行与三个生命周期端点
// （GET / DELETE / cancel）的黑盒用例（Task 9）。
//
// 异步观测约定（与前序任务一致，禁止用固定 time.Sleep 作为主要判据）：
//   - 状态变化一律用 waitResponseStatus 轮询到终态，轮询有界（默认 5s），CI 慢机不受影响；
//   - mock 上游用「闸门 channel」精确控制放行时机：断言「此刻状态必须是 X」时上游一定还挂着，
//     不存在「测试跑得慢一点结论就反过来」的窗口；
//   - 需要断言「不再变化」时用有界静默窗（quietWindow）：窗口内反复复查，只要出现一次变化就失败。

// lifecycleNotFoundMessage 是三个生命周期端点共用的 404 文案（与不存在逐字一致）。
const lifecycleNotFoundMessage = "previous response not found"

// backgroundTimeoutMessage 是站点关闭存储时 background:true 的 400 文案。
const backgroundTimeoutMessage = "response storage is disabled on this gateway, background is unavailable"

// backgroundStreamSSE 是流式 chat 上游的响应：一个内容分片 + 收尾分片 + usage + [DONE]。
// 网关的翻译器在收到 [DONE] 后才收尾，因此这是「上游正常结束」的形态。
const backgroundStreamSSE = "data: {\"id\":\"chatcmpl-b\",\"object\":\"chat.completion.chunk\",\"choices\":[" +
	"{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"back\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-b\",\"object\":\"chat.completion.chunk\",\"choices\":[" +
	"{\"index\":0,\"delta\":{\"content\":\"ground\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-b\",\"object\":\"chat.completion.chunk\",\"choices\":[" +
	"{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"id\":\"chatcmpl-b\",\"object\":\"chat.completion.chunk\",\"choices\":[]," +
	"\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n" +
	"data: [DONE]\n\n"

// responsesPollInterval 是状态轮询间隔；responsesPollTimeout 是单次轮询的等待上限
// （真实耗时取决于 mock 上游，慢机只是多转几圈）。
const (
	responsesPollInterval = 5 * time.Millisecond
	responsesPollTimeout  = 5 * time.Second
)

// responseSweepDelayForTest 是「重启遗留行」检查的启动延迟（与生产常量一致，供需要
// 越过该窗口的用例使用）。
const responseSweepDelayForTest = time.Second

// quietWindow 是「断言不再变化」的静默窗长度：远大于一次轮询间隔与一次后台写回。
const quietWindow = 300 * time.Millisecond

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// backgroundGate 是 mock 上游的放行闸门：handler 进入后关闭 entered（只关一次），
// 随后阻塞在 release 上。用例据此精确控制「后台任务此刻一定还挂在上游」。
//
// release 是有缓冲 channel（容量 1）：放行一次 = 投递一个值，最多堆积一次；
// closeGate 关闭 release 让所有等待者一起返回（用例收尾时避免 httptest.Server.Close 挂住）。
type backgroundGate struct {
	mu        sync.Mutex
	entered   bool
	enteredC  chan struct{}
	release   chan struct{}
	closeOnce sync.Once
}

// newBackgroundGate 创建一个未放行的闸门。
func newBackgroundGate() *backgroundGate {
	g := &backgroundGate{enteredC: make(chan struct{}), release: make(chan struct{}, 1)}
	return g
}

// enter 上报「handler 已进入」并阻塞到放行或 ctx 取消。
// 返回 false 表示 ctx 已取消（后台超时/取消），handler 应当直接返回。
func (g *backgroundGate) enter(r *http.Request) bool {
	g.mu.Lock()
	if !g.entered {
		g.entered = true
		close(g.enteredC)
	}
	g.mu.Unlock()
	var done <-chan struct{}
	if r != nil {
		done = r.Context().Done()
	}
	select {
	case <-g.release:
		return true
	case <-done:
		return false
	}
}

// releaseOnce 放行一次（非阻塞；release 容量为 1，重复投递不会阻塞）。
func (g *backgroundGate) release_() {
	select {
	case g.release <- struct{}{}:
	default:
	}
}

// closeGate 关闭闸门：所有阻塞中的 handler 立刻返回（用例收尾用）。
func (g *backgroundGate) closeGate() {
	g.closeOnce.Do(func() { close(g.release) })
}

// respondChatGate 返回一个「闸门放行后回固定 chat JSON」的上游处理器。
func respondChatGate(g *backgroundGate, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if !g.enter(r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}
}

// waitEntered 轮询等待 mock 上游被触达（后台任务启动有效），上限 responsesPollTimeout。
func (g *backgroundGate) waitEntered(t *testing.T, ctxDesc string) {
	t.Helper()
	deadline := time.Now().Add(responsesPollTimeout)
	for {
		g.mu.Lock()
		entered := g.entered
		g.mu.Unlock()
		if entered {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: upstream was not entered within %s", ctxDesc, responsesPollTimeout)
		}
		time.Sleep(responsesPollInterval)
	}
}

// doLifecycle 向被测网关发出一次带 Bearer 的方法请求（get/delete/cancel 共用）。
func (e *env) doLifecycle(t *testing.T, method, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.server.URL+path, nil)
	if err != nil {
		t.Fatalf("build %s %s request: %v", method, path, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("do %s %s request: %v", method, path, err)
	}
	return resp
}

// getResponse 发 GET /v1/responses/{id}。
func (e *env) getResponse(t *testing.T, token, id string) *http.Response {
	t.Helper()
	return e.doLifecycle(t, http.MethodGet, responsesPath+"/"+id, token)
}

// deleteResponse 发 DELETE /v1/responses/{id}。
func (e *env) deleteResponse(t *testing.T, token, id string) *http.Response {
	t.Helper()
	return e.doLifecycle(t, http.MethodDelete, responsesPath+"/"+id, token)
}

// cancelResponse 发 POST /v1/responses/{id}/cancel。
func (e *env) cancelResponse(t *testing.T, token, id string) *http.Response {
	t.Helper()
	return e.doLifecycle(t, http.MethodPost, responsesPath+"/"+id+"/cancel", token)
}

// postBackground 提交一条 background:true 的请求并断言立即 200，返回 response 的 id 与初始 response 对象。
func postBackground(t *testing.T, e *env, token, input string) (string, map[string]any) {
	t.Helper()
	resp := e.postResponses(t, token,
		`{"model":"mock-a","input":"`+input+`","background":true}`)
	assertStatus(t, resp, http.StatusOK)
	out := decodeResponsesBody(t, readBody(t, resp))
	id, _ := out["id"].(string)
	if !strings.HasPrefix(id, "resp_") {
		t.Fatalf("background response id = %q, want a resp_ id (body=%v)", id, out)
	}
	return id, out
}

// responseStatus 从 GET /v1/responses/{id} 的响应体里取 status（404 时返回空串）。
func responseStatus(t *testing.T, e *env, token, id string) string {
	t.Helper()
	resp := e.getResponse(t, token, id)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read polled response body: %v", err)
	}
	status, _ := decodeResponsesBody(t, string(raw))["status"].(string)
	return status
}

// isTerminalStatus 报告 status 是否终态。
func isTerminalStatus(status string) bool {
	switch status {
	case model.ResponseStateStatusCompleted, model.ResponseStateStatusFailed,
		model.ResponseStateStatusIncomplete, model.ResponseStateStatusCancelled:
		return true
	default:
		return false
	}
}

// waitTerminal 轮询 GET 直到进入任一终态，返回终态响应体（不做期望值断言，由调用方断言）。
//
// 刻意**不**在「终态与期望不符」时提前失败：失败断言应带着序列证据（见 statusTracker）
// 出现在用例里，而不是藏在轮询辅助里。
func waitTerminal(t *testing.T, e *env, token, id string) map[string]any {
	t.Helper()
	tr := &statusTracker{}
	deadline := time.Now().Add(responsesPollTimeout)
	for {
		out := pollResponse(t, e, token, id, tr)
		if out == nil {
			t.Fatalf("poll of %q returned non-200", id)
		}
		if status, _ := out["status"].(string); isTerminalStatus(status) {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("response %q never reached a terminal status, seen %v", id, tr.snapshot())
		}
		time.Sleep(responsesPollInterval)
	}
}

// waitResponseStatus 轮询直到进入终态，断言终态正是 want 并返回响应体。
func waitResponseStatus(t *testing.T, e *env, token, id string, want string) map[string]any {
	t.Helper()
	tr := &statusTracker{}
	out := waitTerminalTracked(t, e, token, id, tr)
	if got, _ := out["status"].(string); got != want {
		t.Fatalf("response %q reached terminal status %q, want %q (seen %v)", id, got, want, tr.snapshot())
	}
	return out
}

// waitTerminalTracked 是 waitTerminal 的记录版。
func waitTerminalTracked(t *testing.T, e *env, token, id string, tr *statusTracker) map[string]any {
	t.Helper()
	deadline := time.Now().Add(responsesPollTimeout)
	for {
		out := pollResponse(t, e, token, id, tr)
		if out == nil {
			t.Fatalf("poll of %q returned non-200", id)
		}
		if status, _ := out["status"].(string); isTerminalStatus(status) {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("response %q never reached a terminal status, seen %v", id, tr.snapshot())
		}
		time.Sleep(responsesPollInterval)
	}
}

// storedResponse 读取表侧的单行（不存在时返回 false）。
func storedResponse(t *testing.T, e *env, id string) (model.ResponseState, bool) {
	t.Helper()
	var row model.ResponseState
	err := openReadDB(t, e.dsn).Model(&model.ResponseState{}).Where("id = ?", id).First(&row).Error
	if err != nil {
		return model.ResponseState{}, false
	}
	return row, true
}

// assertStatusStays 在 quietWindow 内反复复查状态必须恒为 want。
func assertStatusStays(t *testing.T, e *env, token, id, want, ctxDesc string) {
	t.Helper()
	deadline := time.Now().Add(quietWindow)
	for time.Now().Before(deadline) {
		if got := responseStatus(t, e, token, id); got != want {
			t.Fatalf("%s: status = %q, want it to stay %q", ctxDesc, got, want)
		}
		time.Sleep(responsesPollInterval)
	}
}

// waitRowStatus 轮询表侧行（用于跨用户/存储侧断言，不经过 GET 端点）。
func waitRowStatus(t *testing.T, e *env, id, want string) model.ResponseState {
	t.Helper()
	deadline := time.Now().Add(responsesPollTimeout)
	var last string
	for {
		row, ok := storedResponse(t, e, id)
		if ok {
			last = row.Status
			if row.Status == want {
				return row
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("stored row %q status = %q (exists=%v), want %q", id, last, ok, want)
		}
		time.Sleep(responsesPollInterval)
	}
}

// responsesTextOf 取出 output[0].content[0].text（容忍缺失，返回空串）。
func responsesTextOf(resp map[string]any) string {
	output, _ := resp["output"].([]any)
	if len(output) == 0 {
		return ""
	}
	item, _ := output[0].(map[string]any)
	content, _ := item["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	part, _ := content[0].(map[string]any)
	text, _ := part["text"].(string)
	return text
}

// setupBackground 建模型与两把密钥（本人 / 他人），返回本人在用的 token 与他人 token。
func setupBackground(t *testing.T, e *env) (string, string) {
	t.Helper()
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))
	other := mustCreateUser(t, e.store, "other")
	_, otherToken := mustCreateKey(t, e.store, other.ID, 0, primary(m.ID, 0))
	return token, otherToken
}

// statusTracker 记录一次轮询序列里观测到的全部状态（按首次出现顺序）。
//
// 为什么需要它：queued → in_progress 的窗口很窄（后台任务一启动就跃迁），轮询可能整段错过。
// 「必须经过 queued」因此不能用「轮询到 queued」来断言——记录整段序列并在用例末尾检查
// 它包含 queued，才是对状态机形状的稳定断言，而不是对轮询时序的断言。
type statusTracker struct {
	mu   sync.Mutex
	seen []string
}

func (s *statusTracker) observe(status string) {
	if status == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.seen) == 0 || s.seen[len(s.seen)-1] != status {
		s.seen = append(s.seen, status)
	}
}

// has 报告是否观测到过 status。
func (s *statusTracker) has(status string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, seen := range s.seen {
		if seen == status {
			return true
		}
	}
	return false
}

// snapshot 返回观测序列的副本（用于失败信息）。
func (s *statusTracker) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// pollResponse 发一次 GET 并把状态交给 tracker；返回 (status, body)，非 200 返回空串。
func pollResponse(t *testing.T, e *env, token, id string, tr *statusTracker) map[string]any {
	t.Helper()
	resp := e.getResponse(t, token, id)
	raw, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read polled response body: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	out := decodeResponsesBody(t, string(raw))
	if tr != nil {
		status, _ := out["status"].(string)
		tr.observe(status)
	}
	return out
}

// waitAnyStatus 轮询直到状态是 wants 之一（非终态且不在 wants 内时继续等）。
func waitAnyStatus(t *testing.T, e *env, token, id string, wants []string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(responsesPollTimeout)
	for {
		out := pollResponse(t, e, token, id, nil)
		if out == nil {
			t.Fatalf("poll of %q returned non-200", id)
		}
		got, _ := out["status"].(string)
		for _, want := range wants {
			if got == want {
				return out
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("response %q status = %q after %s, want one of %v", id, got, responsesPollTimeout, wants)
		}
		time.Sleep(responsesPollInterval)
	}
}

// ---------------------------------------------------------------------------
// 1. 立即返回 queued，且**先建行**（★ 核心契约）
// ---------------------------------------------------------------------------

// TestResponseBackground_ReturnsQueued：background:true 必须立即返回 status:"queued" 的 response 对象，
// 并且**在返回之前**已经落好行——否则客户端拿到 id 后立刻 GET 会 404。
//
// 「先建行」的直接观测：POST 返回后立刻 GET，必须 200 且回包与 POST 回包**逐字节一致**
// （同一份 queued response_body）；表侧行必须已存在且归属正确。
// 之后用 statusTracker 记录整段轮询序列，断言状态机经过 queued——后台任务一旦启动就跃迁到
// in_progress，轮询可能错过该窗口，因此按「整段序列」而不是「某一次读到」断言。
func TestResponseBackground_ReturnsQueued(t *testing.T) {
	gate := newBackgroundGate()
	e := newEnv(t, respondChatGate(gate, responsesOKChatBody))
	t.Cleanup(gate.closeGate)
	token, _ := setupBackground(t, e)

	tr := &statusTracker{}
	resp := e.postResponses(t, token, `{"model":"mock-a","input":"hi queued","background":true}`)
	assertStatus(t, resp, http.StatusOK)
	raw := readBody(t, resp)
	out := decodeResponsesBody(t, raw)
	id, _ := out["id"].(string)
	if !strings.HasPrefix(id, "resp_") {
		t.Fatalf("background response id = %q, want a resp_ id (body=%v)", id, out)
	}
	tr.observe(model.ResponseStateStatusQueued)
	if got, _ := out["status"].(string); got != model.ResponseStateStatusQueued {
		t.Fatalf("initial status = %q, want %q (body=%v)", got, model.ResponseStateStatusQueued, out)
	}
	if got, _ := out["object"].(string); got != "response" {
		t.Fatalf("object = %q, want response", got)
	}
	if store, ok := out["store"].(bool); !ok || !store {
		t.Fatalf("store = %v (present=%v), want true (background forces storage)", out["store"], ok)
	}

	// 先建行：POST 返回的那一刻 GET 必须已经能看到这一行，且回包是同一份 queued body。
	getResp := e.getResponse(t, token, id)
	assertStatus(t, getResp, http.StatusOK)
	if got := readBody(t, getResp); got != raw {
		t.Fatalf("GET right after POST = %q, want byte-identical to the POST body %q", got, raw)
	}
	row, ok := storedResponse(t, e, id)
	if !ok {
		t.Fatalf("response_states row %q missing right after POST returned", id)
	}
	if row.OwnerUserID != e.user.ID || row.OwnerAPIKeyID == 0 {
		t.Fatalf("stored owner = (user=%d, key=%d), want the submitting key", row.OwnerUserID, row.OwnerAPIKeyID)
	}

	gate.release_()
	waitTerminalTracked(t, e, token, id, tr)
	if !tr.has(model.ResponseStateStatusQueued) {
		t.Fatalf("status sequence %v must contain queued (state machine starts at queued)", tr.snapshot())
	}
}

// ---------------------------------------------------------------------------
// 2. 轮询到 completed（非流式与流式两条上游路径）
// ---------------------------------------------------------------------------

// TestResponseBackground_PollUntilCompleted：后台跑完后 GET 返回完整的 completed 响应
// （output 文本正确、usage 落库、store 回显 true）。
func TestResponseBackground_PollUntilCompleted(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody))
	token, _ := setupBackground(t, e)

	id, _ := postBackground(t, e, token, "hello background")
	out := waitResponseStatus(t, e, token, id, model.ResponseStateStatusCompleted)
	row, ok := storedResponse(t, e, id)

	if text := responsesTextOf(out); text != "hello there" {
		t.Fatalf("output text = %q, want %q", text, "hello there")
	}
	if got, _ := out["model"].(string); got != "mock-a" {
		t.Fatalf("model = %q, want mock-a (requested name echoed back)", got)
	}
	usage, _ := out["usage"].(map[string]any)
	if got := usage["input_tokens"]; got != float64(3) {
		t.Fatalf("usage.input_tokens = %v, want 3", got)
	}
	if !ok || row.Status != model.ResponseStateStatusCompleted {
		t.Fatalf("stored row status = %q (exists=%v), want completed", row.Status, ok)
	}
	// usage 以落库的响应体为准（T4 的 UpdateResponseStateStatus 只写状态与载荷；
	// 同步路径落库的 usage_* 列来自同一份响应体，此处断言同源数据）。
	stored := decodeResponsesBody(t, string(row.ResponseBody))
	storedUsage, _ := stored["usage"].(map[string]any)
	if got := storedUsage["total_tokens"]; got != float64(5) {
		t.Fatalf("stored body usage.total_tokens = %v, want 5", got)
	}
	if len(row.Items) == 0 || string(row.Items) == "[]" {
		t.Fatalf("stored items = %q, want the round snapshot (needed for previous_response_id)", row.Items)
	}
	if row.RequestModel != "mock-a" {
		t.Fatalf("stored request_model = %q, want mock-a", row.RequestModel)
	}
}

// TestResponseBackground_StreamingCompletes：stream:true + background:true——上游 SSE 被后台
// 完整消费后落库为 completed（后台任务不向客户端写任何事件，只写终态行）。
func TestResponseBackground_StreamingCompletes(t *testing.T) {
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, backgroundStreamSSE)
	})
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postResponses(t, token, `{"model":"mock-a","input":"stream bg","background":true,"stream":true}`)
	assertStatus(t, resp, http.StatusOK)
	initial := decodeResponsesBody(t, readBody(t, resp))
	if got, _ := initial["status"].(string); got != model.ResponseStateStatusQueued {
		t.Fatalf("initial status = %q, want queued", got)
	}
	id, _ := initial["id"].(string)

	out := waitResponseStatus(t, e, token, id, model.ResponseStateStatusCompleted)
	if text := responsesTextOf(out); text != "background" {
		t.Fatalf("output text = %q, want %q", text, "background")
	}
	row, ok := storedResponse(t, e, id)
	if !ok || row.Status != model.ResponseStateStatusCompleted {
		t.Fatalf("stored row status = %q (exists=%v), want completed", row.Status, ok)
	}
	stored := decodeResponsesBody(t, string(row.ResponseBody))
	storedUsage, _ := stored["usage"].(map[string]any)
	if got := storedUsage["total_tokens"]; got != float64(5) {
		t.Fatalf("stored body usage.total_tokens = %v, want 5", got)
	}
}

// ---------------------------------------------------------------------------
// 3. 取消：in_progress 时生效，且后台完成不覆盖 cancelled（★ Review Focus 6）
// ---------------------------------------------------------------------------

// TestResponseBackground_CancelNotOverwritten：任务进行中 cancel → 状态立即为 cancelled；
// 随后**放行上游**让后台任务真正跑完（它会尝试写 completed），状态必须仍是 cancelled。
//
// 这是 storage 层 `WHERE status <> 'cancelled'` 条件更新的端到端验证：
// 删掉该条件或后台改用无条件写回，本用例都会变红。
func TestResponseBackground_CancelNotOverwritten(t *testing.T) {
	gate := newBackgroundGate()
	e := newEnv(t, respondChatGate(gate, responsesOKChatBody))
	t.Cleanup(gate.closeGate)
	token, _ := setupBackground(t, e)

	id, _ := postBackground(t, e, token, "cancel me")
	// 等后台任务真正进入上游：闸门未放行 ⇒ 任务不可能结束，此刻它只可能处于
	// queued（尚未跃迁）或 in_progress（已跃迁），两者都是合法的取消对象。
	gate.waitEntered(t, "background task")
	before := waitAnyStatus(t, e, token, id,
		[]string{model.ResponseStateStatusInProgress, model.ResponseStateStatusQueued})

	cancelResp := e.cancelResponse(t, token, id)
	assertStatus(t, cancelResp, http.StatusOK)
	cancelled := decodeResponsesBody(t, readBody(t, cancelResp))
	if got, _ := cancelled["status"].(string); got != model.ResponseStateStatusCancelled {
		t.Fatalf("cancel returned status = %q, want cancelled (observed %q before cancel)",
			got, before["status"])
	}

	// 放行上游：后台任务拿到结果后会尝试写回 completed，必须被条件更新挡下。
	gate.release_()
	assertStatusStays(t, e, token, id, model.ResponseStateStatusCancelled, "after background completion")
	row, _ := storedResponse(t, e, id)
	if row.Status != model.ResponseStateStatusCancelled {
		t.Fatalf("stored status = %q, want cancelled", row.Status)
	}
}

// TestResponseBackground_CancelAfterTimeoutStaysCancelled：后台超时被压到 1ns 时，行会在
// 「仍是非终态」与「已 failed」之间竞态收尾。cancel 的两种合法结果都必须自洽：
//   - 行仍非终态 → 200 并置 cancelled，且此后**不再变化**；
//   - 行已 failed → 400，且状态保持 failed（不可取消的终态）。
//
// 无论走哪一支，都不能出现「200 之后又被后台改回 failed」这种取消失效。
func TestResponseBackground_CancelAfterTimeoutStaysCancelled(t *testing.T) {
	gate := newBackgroundGate()
	e := newSteerableEnv(t,
		respondChatGate(gate, responsesOKChatBody),
		func(h *proxy.Handler) { _ = h.SetResponseBackgroundTimeout(time.Nanosecond) })
	t.Cleanup(gate.closeGate)
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	id, _ := postBackground(t, e, token, "queued cancel")

	resp := e.cancelResponse(t, token, id)
	status := resp.StatusCode
	body := readBody(t, resp)
	switch status {
	case http.StatusOK:
		if got, _ := decodeResponsesBody(t, body)["status"].(string); got != model.ResponseStateStatusCancelled {
			t.Fatalf("cancel status = %q, want cancelled", got)
		}
		assertStatusStays(t, e, token, id, model.ResponseStateStatusCancelled, "cancel after background timeout")
	case http.StatusBadRequest:
		if !strings.Contains(body, "already finished") {
			t.Fatalf("400 body = %q, want the already-finished message", body)
		}
		if got := responseStatus(t, e, token, id); got != model.ResponseStateStatusFailed {
			t.Fatalf("status after rejecting a cancel on a finished row = %q, want failed", got)
		}
	default:
		t.Fatalf("cancel status = %d, want 200 or 400 (body=%s)", status, body)
	}
}

// ---------------------------------------------------------------------------
// 4. DELETE：只允许删终态
// ---------------------------------------------------------------------------

// TestResponseBackground_Delete：终态行可删，返回 {"deleted":true}，随后 GET 404。
func TestResponseBackground_Delete(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody))
	token, _ := setupBackground(t, e)

	id, _ := postBackground(t, e, token, "delete me")
	waitResponseStatus(t, e, token, id, model.ResponseStateStatusCompleted)

	resp := e.deleteResponse(t, token, id)
	assertStatus(t, resp, http.StatusOK)
	deleted := decodeResponsesBody(t, readBody(t, resp))
	if got, _ := deleted["deleted"].(bool); !got {
		t.Fatalf("deleted = %v, want true (body=%v)", deleted["deleted"], deleted)
	}
	if got, _ := deleted["id"].(string); got != id {
		t.Fatalf("deleted.id = %q, want %q", got, id)
	}
	if got, _ := deleted["object"].(string); got != "response.deleted" {
		t.Fatalf("deleted.object = %q, want response.deleted", got)
	}

	assertStatus(t, e.getResponse(t, token, id), http.StatusNotFound)
	// 再删一次：行已不存在 → 与「不存在」同一个 404。
	assertStatus(t, e.deleteResponse(t, token, id), http.StatusNotFound)
}

// TestResponseBackground_DeleteNonTerminalRejected：queued / in_progress 行不允许删除（要先 cancel）。
func TestResponseBackground_DeleteNonTerminalRejected(t *testing.T) {
	gate := newBackgroundGate()
	e := newEnv(t, respondChatGate(gate, responsesOKChatBody))
	t.Cleanup(gate.closeGate)
	token, _ := setupBackground(t, e)

	id, _ := postBackground(t, e, token, "not yet done")
	gate.waitEntered(t, "background task")
	// 上游闸门未放行 ⇒ 任务不可能结束，此刻状态只可能是 queued 或 in_progress。
	waitAnyStatus(t, e, token, id, []string{model.ResponseStateStatusInProgress, model.ResponseStateStatusQueued})

	resp := e.deleteResponse(t, token, id)
	assertStatus(t, resp, http.StatusBadRequest)
	readBody(t, resp)
	if row, ok := storedResponse(t, e, id); !ok || isTerminalStatus(row.Status) {
		t.Fatalf("row after rejected delete = (%q, exists=%v), want it to stay non-terminal", row.Status, ok)
	}

	gate.release_()
	waitResponseStatus(t, e, token, id, model.ResponseStateStatusCompleted)
	assertStatus(t, e.deleteResponse(t, token, id), http.StatusOK)
}

// ---------------------------------------------------------------------------
// 5. 跨用户隔离：三个端点一律 404，且与「不存在」逐字节相同
// ---------------------------------------------------------------------------

// TestResponseBackground_CrossUserForbidden：他人密钥访问 GET / DELETE / cancel 全部 404，
// 状态码、响应体与「id 根本不存在」逐字节一致（防存在性枚举）。
func TestResponseBackground_CrossUserForbidden(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody))
	token, otherToken := setupBackground(t, e)

	id, _ := postBackground(t, e, token, "my private job")
	waitResponseStatus(t, e, token, id, model.ResponseStateStatusCompleted)

	missing := "resp_" + strings.Repeat("0", 32)
	cases := []struct {
		name string
		id   string
		call func(*testing.T, string, string) *http.Response
	}{
		{"get", id, e.getResponse},
		{"delete", id, e.deleteResponse},
		{"cancel", id, e.cancelResponse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			foreign := tc.call(t, otherToken, tc.id)
			own := tc.call(t, otherToken, missing)
			foreignCode, foreignBody := foreign.StatusCode, readBody(t, foreign)
			ownCode, ownBody := own.StatusCode, readBody(t, own)
			if foreignCode != http.StatusNotFound || ownCode != http.StatusNotFound {
				t.Fatalf("status = %d / %d, want 404 for both the foreign id and a missing id",
					foreignCode, ownCode)
			}
			if foreignBody != ownBody {
				t.Fatalf("foreign body %q must be byte-identical to missing body %q", foreignBody, ownBody)
			}
			if !strings.Contains(foreignBody, lifecycleNotFoundMessage) {
				t.Fatalf("body %q must carry the shared not-found message", foreignBody)
			}
		})
	}

	// 本人仍可正常访问：跨用户拒绝不能把本人的访问也拒了。
	assertStatus(t, e.getResponse(t, token, id), http.StatusOK)
}

// TestResponseBackground_LifecycleRequiresAuth：三个端点都要求有效密钥（缺失 → 401）。
func TestResponseBackground_LifecycleRequiresAuth(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody))
	token, _ := setupBackground(t, e)
	id, _ := postBackground(t, e, token, "auth")
	waitResponseStatus(t, e, token, id, model.ResponseStateStatusCompleted)

	for name, call := range map[string]func(*testing.T, string, string) *http.Response{
		"get": e.getResponse, "delete": e.deleteResponse, "cancel": e.cancelResponse,
	} {
		t.Run(name, func(t *testing.T) {
			assertStatus(t, call(t, "", id), http.StatusUnauthorized)
		})
	}
}

// ---------------------------------------------------------------------------
// 6. 开关与参数校验
// ---------------------------------------------------------------------------

// TestResponseBackground_RequiresStore：background:true 与 store:false 互斥（结果无处存放）→ 400；
// background:false 是缺省语义，照常走同步路径。
func TestResponseBackground_RequiresStore(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postResponses(t, token, `{"model":"mock-a","input":"hi","background":true,"store":false}`)
	assertStatus(t, resp, http.StatusBadRequest)
	if body := readBody(t, resp); !strings.Contains(body, "background requires store") {
		t.Fatalf("body = %q, want the background/store conflict message", body)
	}
	if e.rec.Calls() != 0 {
		t.Fatalf("upstream calls = %d, want 0 (rejected request must not be forwarded)", e.rec.Calls())
	}

	// background:false / null 与缺省等价：同步路径立即返回 completed。
	for _, body := range []string{
		`{"model":"mock-a","input":"hi","background":false}`,
		`{"model":"mock-a","input":"hi","background":null}`,
	} {
		out := decodeResponsesBody(t, readBody(t, e.postResponses(t, token, body)))
		if got, _ := out["status"].(string); got != model.ResponseStateStatusCompleted {
			t.Fatalf("synchronous background=false status = %q, want completed", got)
		}
	}
}

// TestResponseBackground_StorageDisabled：站点关闭存储时 background:true → 400（无存储无法异步）。
func TestResponseBackground_StorageDisabled(t *testing.T) {
	e := newSteerableEnv(t,
		respondConstantJSON(http.StatusOK, responsesOKChatBody),
		func(h *proxy.Handler) { _ = h.SetResponseStorage(false) })
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postResponses(t, token, `{"model":"mock-a","input":"hi","background":true}`)
	assertStatus(t, resp, http.StatusBadRequest)
	if body := readBody(t, resp); !strings.Contains(body, backgroundTimeoutMessage) {
		t.Fatalf("body = %q, want the storage-disabled background message", body)
	}
	if e.rec.Calls() != 0 {
		t.Fatalf("upstream calls = %d, want 0", e.rec.Calls())
	}
}

// TestResponseBackground_InvalidBackground：background 非布尔 → 400。
func TestResponseBackground_InvalidBackground(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postResponses(t, token, `{"model":"mock-a","input":"hi","background":"yes"}`)
	assertStatus(t, resp, http.StatusBadRequest)
	readBody(t, resp)
}

// ---------------------------------------------------------------------------
// 7. 后台超时 → failed
// ---------------------------------------------------------------------------

// TestResponseBackground_TimeoutFails：上游一直不回（挂到 ctx 取消），后台超时后行变 failed，
// 且失败原因可观测（error 列非空）。
func TestResponseBackground_TimeoutFails(t *testing.T) {
	gate := newBackgroundGate()
	e := newSteerableEnv(t,
		respondChatGate(gate, responsesOKChatBody),
		func(h *proxy.Handler) { h.SetResponseBackgroundTimeout(100 * time.Millisecond) })
	t.Cleanup(gate.closeGate)
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	id, _ := postBackground(t, e, token, "slow upstream")
	gate.waitEntered(t, "background task")
	out := waitResponseStatus(t, e, token, id, model.ResponseStateStatusFailed)
	if text := responsesTextOf(out); text != "" {
		t.Fatalf("failed response must not carry output, got %q", text)
	}
	errorField, _ := out["error"].(map[string]any)
	if msg, _ := errorField["message"].(string); msg == "" {
		t.Fatalf("failed response must explain the failure, got %v", out)
	}

	row, ok := storedResponse(t, e, id)
	if !ok {
		t.Fatalf("row %q missing", id)
	}
	if row.Status != model.ResponseStateStatusFailed {
		t.Fatalf("stored status = %q, want failed", row.Status)
	}
	if row.Error == "" {
		t.Fatalf("stored error must explain the failure (timed out)")
	}
	// failed 是终态：可以删除。
	assertStatus(t, e.deleteResponse(t, token, id), http.StatusOK)
}

// TestResponseBackground_UpstreamErrorFails：上游 500 时后台任务收尾为 failed（不静默丢弃）。
func TestResponseBackground_UpstreamErrorFails(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusInternalServerError, `{"error":{"message":"boom"}}`))
	token, _ := setupBackground(t, e)

	id, _ := postBackground(t, e, token, "upstream down")
	waitResponseStatus(t, e, token, id, model.ResponseStateStatusFailed)
	row, _ := storedResponse(t, e, id)
	if row.Error == "" {
		t.Fatalf("stored error must record the upstream failure status")
	}
}

// TestResponseBackground_PreflightRejectionHasNoRow：§5.4.1 第 4/5 步（模型解析、配额）在**建行之前**
// 完成——被拒的请求不能在表里留下永久 queued 的孤儿行，也不能进上游。
func TestResponseBackground_PreflightRejectionHasNoRow(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	// model 未被密钥绑定 → resolveAndCheckQuota 回 400。
	resp := e.postResponses(t, token, `{"model":"not-bound","input":"hi","background":true}`)
	assertStatus(t, resp, http.StatusBadRequest)
	readBody(t, resp)

	if rows := storedResponseRows(t, e); len(rows) != 0 {
		t.Fatalf("response_states rows = %d, want 0 (rejected before the row was created)", len(rows))
	}
	if e.rec.Calls() != 0 {
		t.Fatalf("upstream calls = %d, want 0", e.rec.Calls())
	}
	_ = m
}

// ---------------------------------------------------------------------------
// 8. 重启遗留行：queued / in_progress 标记为 failed
// ---------------------------------------------------------------------------

// TestResponseBackground_StaleInProgressMarkedFailed：进程重启后遗留在途行（goroutine 已不存在）
// 必须由保留期清理循环的首次执行标记为 failed，原因 "interrupted by restart"；
// 未超过后台超时的在途行与已完成的旧行都不受影响。
func TestResponseBackground_StaleInProgressMarkedFailed(t *testing.T) {
	store, _ := newTestStore(t)
	usage := proxy.NewUsageWriter(store, 0)
	usage.Start()
	t.Cleanup(func() { usage.Stop(t.Context()) })

	now := time.Now().UTC()
	seed := func(id, status string, age time.Duration) {
		t.Helper()
		row := &model.ResponseState{
			ID:          id,
			OwnerUserID: 1,
			Status:      status,
			CreatedAt:   now.Add(-age),
			UpdatedAt:   now.Add(-age),
		}
		if err := store.CreateResponseState(t.Context(), row); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	// 在途行的时间戳是「建行时刻」：超过后台超时的才会被判定为遗留。
	seed("resp_stale_in_progress", model.ResponseStateStatusInProgress, 3*time.Minute)
	seed("resp_stale_queued", model.ResponseStateStatusQueued, 5*time.Minute)
	// 仍在后台超时窗口内的在途行：不得误伤。年龄取「超时 + 一小段余量」之内
	// （超时 1 分钟，行 30 秒前），因此只要残留判据用了「老过后台超时」这条闸门，
	// 它就必须被保留；任何忽略年龄窗的判据都会把它打成 failed（变异 M9 可复现）。
	seed("resp_fresh_in_progress", model.ResponseStateStatusInProgress, 30*time.Second)
	seed("resp_old_completed", model.ResponseStateStatusCompleted, time.Hour)

	h := proxy.NewHandler(store, usage, http.StatusTooManyRequests)
	if err := h.SetResponseBackgroundTimeout(time.Minute); err != nil {
		t.Fatalf("set background timeout: %v", err)
	}
	h.StartResponseRetention(24 * time.Hour)
	t.Cleanup(h.StopResponseRetention)

	for _, id := range []string{"resp_stale_in_progress", "resp_stale_queued"} {
		row := waitRowStatusStore(t, store, id, model.ResponseStateStatusFailed)
		if row.Error != "interrupted by restart" {
			t.Fatalf("%s error = %q, want %q", id, row.Error, "interrupted by restart")
		}
	}

	fresh, err := store.GetResponseState(t.Context(), "resp_fresh_in_progress")
	if err != nil || fresh.Status != model.ResponseStateStatusInProgress {
		t.Fatalf("fresh in-flight row = (%+v, %v), want untouched in_progress", fresh, err)
	}
	done, err := store.GetResponseState(t.Context(), "resp_old_completed")
	if err != nil || done.Status != model.ResponseStateStatusCompleted {
		t.Fatalf("completed row = (%+v, %v), want untouched completed", done, err)
	}
}

// waitRowStatusStore 是不依赖 env 的表侧轮询版本。
func waitRowStatusStore(t *testing.T, store *model.Store, id, want string) *model.ResponseState {
	t.Helper()
	deadline := time.Now().Add(responsesPollTimeout)
	var last string
	for {
		row, err := store.GetResponseState(t.Context(), id)
		switch {
		case err == nil:
			last = row.Status
			if row.Status == want {
				return row
			}
		case errors.Is(err, model.ErrResponseStateNotFound):
			last = "<missing>"
		default:
			t.Fatalf("get %s: %v", id, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("row %q status = %q, want %q", id, last, want)
		}
		time.Sleep(responsesPollInterval)
	}
}

// TestResponseBackground_SetTimeoutRejectsNonPositive：装配期校验——后台超时必须是正数。
func TestResponseBackground_SetTimeoutRejectsNonPositive(t *testing.T) {
	h := proxy.NewHandler(nil, nil, http.StatusTooManyRequests)
	for _, d := range []time.Duration{0, -time.Second} {
		if err := h.SetResponseBackgroundTimeout(d); err == nil {
			t.Fatalf("SetResponseBackgroundTimeout(%v) = nil, want an error", d)
		}
	}
}

// TestResponseBackground_DefaultTimeout：不装配时缺省 30 分钟（规格 §5.6）。
func TestResponseBackground_DefaultTimeout(t *testing.T) {
	h := proxy.NewHandler(nil, nil, http.StatusTooManyRequests)
	if got := h.ResponseBackgroundTimeout(); got != 30*time.Minute {
		t.Fatalf("default background timeout = %v, want 30m", got)
	}
}
