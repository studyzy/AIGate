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

package proxy

// 本文件实现 background:true 的异步执行（design §5.6「background 流程」）：
//
//  1. 入站：鉴权 → §5.4.1 第 3/4/5 步（有状态前置、max_output_tokens 压制、模型解析与配额）
//     → **先落一行 status=queued** → 立即返回该行的 response 对象 → goroutine 后台执行；
//  2. 后台：queued → in_progress → completed | failed（或用户取消后的 cancelled），
//     只在状态跃迁时 UPDATE，终态一次性写回累积 items 与完整响应体；
//  3. 收尾复用**同一条落库链路**：非流式走 buildResponsesItemsSnapshot + 与同步路径相同的
//     响应体改写，流式走 responsesStreamTranslator 的 TakeTerminal（消费即清）。
//     因此 Ruling 16/17 的不变式在这里**原样继承**：上游中断（合成收尾）的那一轮不落库，
//     状态直接记 failed——它不会成为下一轮续接的父行。
//
// 为什么先建行、再执行：客户端拿到 id 后可以立刻 GET / cancel。若先执行再建行，
// 这段窗口里的 GET 会 404、cancel 会落空，队列语义也就不成立了。
//
// 为什么第 4/5 步在建行之前：模型解析与配额拒绝是「请求根本没被受理」，
// 若先建行，被拒请求会在表里留下永远 queued 的孤儿行（没有 goroutine 会去收尾它们）。
//
// 并发安全见 runResponseBackground 的注释；三个生命周期端点见 responses_lifecycle.go。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"aigate/internal/model"
	"aigate/internal/provider"
)

// responseBackgroundSweepInterval 是「重启遗留行」检查的周期。
//
// 判定与清理**解耦**：进程启动后的首个检查在 responseBackgroundSweepStartDelay 之后立即执行，
// 之后每个周期再检查一次。这样做的理由是「年龄窗口」——残留判据是
// updated_at 早于「现在 − 后台超时」（见 markStaleResponseStates），因此：
//   - 立即扫描时，仍在跑的合法后台任务最新写回（in_progress）在超时窗口内，不会被误伤；
//   - 只靠一小时一次的保留期循环不够：默认后台超时 30 分钟，残留行最长会「看起来在跑」
//     近一个半小时；用短周期把可观测残留窗口压到分钟级。
const (
	// responseBackgroundDefaultTimeout 是后台执行超时的默认值（规格 §5.6：30 分钟）。
	responseBackgroundDefaultTimeout = 30 * time.Minute

	responseBackgroundSweepInterval   = time.Minute
	responseBackgroundSweepStartDelay = time.Second
	// responseRestartMessage 是残留行的失败原因（规格 §5.6 并发与失败）。
	responseRestartMessage = "interrupted by restart"
)

// responsesBackgroundRequest 是一次后台执行的输入快照。
//
// 它**只持有值，不持有请求对象**：原请求的 context 在 handler 返回时即被取消，
// 后台任务自己派生 ctx（见 runResponseBackground）。快照里的 map 由本次 handler 独占，
// 后续不再有别的 goroutine 读它，因此不需要加锁或深拷贝。
type responsesBackgroundRequest struct {
	key      *model.APIKey
	resolved *model.Resolved
	// state 是 §5.4.1 第 3 步的结果；store 恒为 true（background:true 强制存储）。
	state          *responsesStatefulInput
	respID         string
	requestedModel string
	// reqBody 是已拼入历史、已施加密钥上限的 Responses 形态请求体（不含 model 替换）。
	reqBody map[string]any
	// passthrough 为 true 表示上游是 openai-responses（格 7），否则走 chat 转换（格 8/9）。
	passthrough bool
	// customHeaders 是入站请求里匹配成功的透传头（已按配置逐项比对）。
	customHeaders map[string]string
}

// SetResponseBackgroundTimeout 配置 background:true 的后台执行超时（-response-background-timeout）。
//
// 必须在装配阶段（Register 之前）调用：后台 goroutine 只在任务启动时读一次该值，
// 运行期不再改变。非正数直接拒绝——0 会让每个后台任务一启动就超时，
// 这种配置错误应当在启动期暴露，而不是变成一片 failed 行。
func (h *Handler) SetResponseBackgroundTimeout(d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("response background timeout must be positive, got %s", d)
	}
	h.responseBackgroundTimeout = d
	return nil
}

// ResponseBackgroundTimeout 返回当前配置的后台执行超时（未装配时为默认 30 分钟）。
//
// 导出给装配层与运维视角使用（日志里打印实际生效值）；同时是残留判据的输入，
// 见 staleResponseStates。
func (h *Handler) ResponseBackgroundTimeout() time.Duration {
	return h.responseBackgroundTimeout
}

// startResponseBackground 为一条 background 请求落 queued 行，并启动后台 goroutine。
//
// 【顺序保证 —— 改这里之前请先读完这一段】
//
// 建行**必须先于**启动后台 goroutine、**先于**回包（本函数在 handleResponses 写出 200 之前
// 被调用）。这是「客户端拿到 queued 回包后立即 GET 必须能查到这一行、且不会 404」的前提
// （规格 §5.6 background 流程第 1 步）。
//
// 该顺序由**代码结构**保证，不是由测试观测保证：「回包那一刻行是否存在」经由 HTTP 没有
// 可观测点——客户端读到 EOF 时后台 goroutine 早已执行完建行，任何「回包后再查」的用例在
// 「回包后建行」的改动下都会存活（实测矩阵见 task-9-report.md §Fix round 1 §3）。
// 因此**不要**调整下面 CreateResponseState 与 `go h.runResponseBackground` 的相对顺序，
// 也不要把它挪到回包之后：这类改动的正确锚点是结构本身，用例抓不住。
//
// 相关的行为断言（客户端可见口径）：
//   - TestResponseBackground_ReturnsQueued：状态序列从 queued 开始，回包与随后 GET 逐字节相同；
//   - TestResponseBackground_RowExistsBeforePostReturns：回包后行存在、GET 回包即该行快照。
func (h *Handler) startResponseBackground(bg *responsesBackgroundRequest) error {
	row := &model.ResponseState{
		ID:                 bg.respID,
		ConversationID:     bg.state.conversationID,
		OwnerUserID:        bg.key.UserID,
		OwnerAPIKeyID:      bg.key.ID,
		RequestModel:       bg.requestedModel,
		PreviousResponseID: bg.state.previousID,
		Status:             model.ResponseStateStatusQueued,
		ResponseBody:       marshalJSONOrEmpty(buildQueuedResponseBody(bg.respID, bg.requestedModel)),
	}
	if err := h.store.CreateResponseState(context.Background(), row); err != nil {
		return err
	}
	go h.runResponseBackground(bg)
	return nil
}

// buildQueuedResponseBody 构造 queued 响应体：POST 的回包与「后台尚未写回时」的 GET 同一份。
func buildQueuedResponseBody(respID, requestedModel string) map[string]any {
	body := map[string]any{
		"id":      respID,
		"object":  respObjectResponse,
		"status":  model.ResponseStateStatusQueued,
		"output":  []any{},
		"store":   true,
		"usage":   nil,
		"created": time.Now().Unix(),
	}
	if requestedModel != "" {
		body["model"] = requestedModel
	}
	return body
}

// --- 在途后台任务的注册表（取消传播的载体） ---

// registerBackgroundRun 登记一个在途后台任务的可取消 ctx：cancel 端点据此中断上游请求。
//
// 不登记后果（实测）：cancel 后状态确实变 cancelled，但上游请求会一直跑到后台超时，
// 算力与配额白烧最多 30 分钟。
func (h *Handler) registerBackgroundRun(respID string, cancel context.CancelFunc) {
	h.backgroundMu.Lock()
	defer h.backgroundMu.Unlock()
	if h.backgroundRuns == nil {
		h.backgroundRuns = make(map[string]context.CancelFunc)
	}
	h.backgroundRuns[respID] = cancel
}

// releaseBackgroundRun 注销登记（任务退出时无条件调用）。只删自己登记的那个 cancel，
// 避免误删「同一 respID 的新任务」（respID 唯一，这里是防御性写法）。
func (h *Handler) releaseBackgroundRun(respID string, cancel context.CancelFunc) {
	h.backgroundMu.Lock()
	defer h.backgroundMu.Unlock()
	if _, exists := h.backgroundRuns[respID]; exists {
		delete(h.backgroundRuns, respID)
	}
	_ = cancel
}

// abortBackgroundRun 中断一个在途后台任务（cancel 端点调用）：返回是否确实有在途任务。
// 没有登记不代表「没在跑」——任务可能尚未登记（goroutine 未调度）或已结束，
// 这两种情形都由 worker 启动前的行为保证：未登记时 cancel 已把行写成 cancelled，
// worker 启动后重新读行会发现 cancelled 并直接退出、不发起上游请求。
func (h *Handler) abortBackgroundRun(respID string) bool {
	h.backgroundMu.Lock()
	cancel, exists := h.backgroundRuns[respID]
	h.backgroundMu.Unlock()
	if !exists {
		return false
	}
	cancel()
	return true
}

// runResponseBackground 是后台任务的 goroutine 入口（**只能由 handleResponses 调用一次**）。
func (h *Handler) runResponseBackground(bg *responsesBackgroundRequest) {
	// 与原请求脱离：handler 返回后 r.Context() 立刻被取消，这里必须用独立 ctx。
	// 该 ctx 同时登记到注册表，cancel 端点可以据此中断正在进行的上游请求。
	ctx, cancel := context.WithTimeout(context.Background(), h.ResponseBackgroundTimeout())
	defer cancel()
	h.registerBackgroundRun(bg.respID, cancel)
	defer h.releaseBackgroundRun(bg.respID, cancel)
	// 状态写回必须**脱离**上面这个 ctx：ctx 到期正是「任务失败」的触发条件，
	// 若写回也用 ctx，超时那一刻连 failed 都写不进去（实测：UPDATE 直接报
	// context deadline exceeded，行永远停在 in_progress）。写回只做一次 UPDATE，
	// 用 background 并受存储层自身的连接/语句超时约束。
	persist := context.Background()

	// 闸门一：上行跃迁 queued → in_progress。
	// 用条件更新（WHERE status <> 'cancelled'）而非原地判断，是为了让「用户刚取消」与
	// 「后台刚启动」之间没有窗口：要么跃迁生效（此时取消会被后续 UPDATE 看到），
	// 要么跃迁无效（已取消 → 下面的执行前置重新读行会看到 cancelled 并立刻退出）。
	if err := h.store.UpdateResponseStateStatus(persist, bg.respID, model.ResponseStateStatusInProgress, "", nil, nil); err != nil {
		slog.Warn("mark response in_progress failed", "error", err, "response_id", bg.respID)
	}
	// 闸门二：执行前置**必须重新读行**。
	// UpdateResponseStateStatus 命中 0 行时静默返回 nil（存储层契约），因此上面那行无法区分
	// 「跃迁成功」与「行已被取消/被清理」；只有重新读行才能区分。读不到就立刻停手，
	// 绝不发起一次没人需要、也没人收尾的上游请求（用户已取消，或行已被保留期清理）。
	row, err := h.store.GetResponseState(ctx, bg.respID)
	switch {
	case errors.Is(err, model.ErrResponseStateNotFound):
		slog.Info("background response row gone before start, skipping", "response_id", bg.respID)
		return
	case err != nil:
		slog.Error("load background response row failed", "error", err, "response_id", bg.respID)
		h.finishResponseBackground(persist, bg.respID, model.ResponseStateStatusFailed, msgInternalError,
			nil, failedResponseBody(bg.respID, msgInternalError))
		return
	case row.Status == model.ResponseStateStatusCancelled:
		slog.Info("background response cancelled before start", "response_id", bg.respID)
		return
	}

	h.executeResponseBackground(ctx, persist, bg)
}

// executeResponseBackground 发起上游请求并把结果写回行（状态跃迁的**唯一**终态出口是
// finishResponseBackground）。返回时终态已写回（或行已被取消/清理，无需写回）。
func (h *Handler) executeResponseBackground(ctx, persist context.Context, bg *responsesBackgroundRequest) {
	start := time.Now()
	if bg.passthrough {
		h.runResponseBackgroundPassthrough(ctx, persist, bg, start)
		return
	}
	h.runResponseBackgroundConversion(ctx, persist, bg, start)
}

// runResponseBackgroundConversion 是格 8/9 的后台执行：chat 形状上游 → Responses 响应。
//
// 与同步路径 forwardViaChatConversion 共用同一套转换与落库函数，差别只有两点：
//   - 输出目标是 responsesBufferWriter（不写客户端）；
//   - 终态落库走 finishResponseBackground（条件更新），而不是同步路径的无条件插入。
func (h *Handler) runResponseBackgroundConversion(
	ctx context.Context,
	persist context.Context,
	bg *responsesBackgroundRequest,
	start time.Time,
) {
	chatBody, err := marshalResponsesChatBody(bg.reqBody)
	if err != nil {
		reason := responsesConversionMessage(err)
		h.finishResponseBackground(persist, bg.respID, model.ResponseStateStatusFailed, reason,
			nil, failedResponseBody(bg.respID, reason))
		return
	}
	// 与同步路径完全相同的兜底压制（理由见 forwardViaChatConversion 内的长注释）。
	enforceMaxTokens(chatBody, bg.key.MaxTokens)
	chatBody["model"] = bg.resolved.ModelID
	body, err := json.Marshal(chatBody)
	if err != nil {
		h.finishResponseBackground(persist, bg.respID, model.ResponseStateStatusFailed, msgInternalError,
			nil, failedResponseBody(bg.respID, msgInternalError))
		return
	}

	p := provider.GetProvider(bg.resolved.ModelType)
	if isStream, _ := bg.reqBody["stream"].(bool); isStream {
		h.runResponseBackgroundStream(ctx, persist, bg, p, body, start)
		return
	}

	result, statusCode, err := p.ChatCompletion(
		ctx, bg.resolved.APIKey, bg.resolved.URL, bg.resolved.ModelID, body, bg.customHeaders)
	if err != nil {
		slog.Error("background upstream request error", "error", err, "model", bg.resolved.ModelID,
			"response_id", bg.respID)
		h.finishResponseBackground(persist, bg.respID, model.ResponseStateStatusFailed, msgUpstreamConnect,
			nil, failedResponseBody(bg.respID, msgUpstreamConnect))
		return
	}
	if statusCode == http.StatusOK {
		h.logUsage(bg.key, bg.resolved, completionResultUsage(result), statusCode, start)
	}
	if statusCode != http.StatusOK {
		reason := upstreamStatusMessage(statusCode)
		h.finishResponseBackground(persist, bg.respID, model.ResponseStateStatusFailed, reason,
			nil, failedResponseBody(bg.respID, reason))
		return
	}
	out, err := chatResponseToResponses(result.Body, bg.requestedModel, bg.respID)
	if err != nil {
		slog.Error("background chat to responses conversion failed", "error", err,
			"model", bg.resolved.ModelID, "response_id", bg.respID)
		h.finishResponseBackground(persist, bg.respID, model.ResponseStateStatusFailed,
			msgResponsesInvalidUpstream, nil, failedResponseBody(bg.respID, msgResponsesInvalidUpstream))
		return
	}
	out = normalizeResponsesStore(out, true)
	var resp map[string]any
	if json.Unmarshal(out, &resp) != nil {
		h.finishResponseBackground(persist, bg.respID, model.ResponseStateStatusFailed,
			msgResponsesInvalidUpstream, nil, failedResponseBody(bg.respID, msgResponsesInvalidUpstream))
		return
	}
	items := marshalResponseItems(buildResponsesItemsSnapshot(bg.state.history, bg.reqBody, responsesOutputItems(resp)))
	h.finishResponseBackground(persist, bg.respID, responsesStatusOf(resp), "", items, out)
}

// runResponseBackgroundStream 是流式上游的后台执行。
//
// 【核心不变式继承（Ruling 16/17）】终态快照只来自翻译器的 TakeTerminal——
// 它是「消费即清」的，且**只在 null 上游自行收尾**时被写入；上游中断时翻译器会补发合成的
// response.completed 给（这里的）缓冲 writer，但 TakeTerminal 为空。
// 因此这里与同步路径完全同口径：TakeTerminal 为空 ⇒ 该轮不落库，状态记 failed。
// **不要**为了「失败也要有个响应体」而改成保存合成终态——那会让半截对话落库并成为父节点。
func (h *Handler) runResponseBackgroundStream(
	ctx context.Context,
	persist context.Context,
	bg *responsesBackgroundRequest,
	p provider.Provider,
	body []byte,
	start time.Time,
) {
	sink := &responsesBufferWriter{}
	tw := &responsesTranslationWriter{
		responsesStreamTranslator: newResponsesStreamTranslator(sink, sink, bg.requestedModel, bg.respID),
		w:                         sink,
	}
	result, statusCode, err := p.ChatCompletionStream(
		ctx, bg.resolved.APIKey, bg.resolved.URL, bg.resolved.ModelID, body, tw, sink, bg.customHeaders)
	if err != nil {
		slog.Error("background upstream stream error", "error", err,
			"model", bg.resolved.ModelID, "response_id", bg.respID)
		h.logUsage(bg.key, bg.resolved, streamResultUsage(result), statusCode, start)
		h.finishResponseBackground(persist, bg.respID, model.ResponseStateStatusFailed, msgUpstreamConnect,
			nil, failedResponseBody(bg.respID, msgUpstreamConnect))
		return
	}
	if statusCode < http.StatusBadRequest {
		h.logUsage(bg.key, bg.resolved, streamResultUsage(result), statusCode, start)
	}
	if statusCode >= http.StatusBadRequest {
		reason := upstreamStatusMessage(statusCode)
		h.finishResponseBackground(persist, bg.respID, model.ResponseStateStatusFailed, reason,
			nil, failedResponseBody(bg.respID, reason))
		return
	}
	// 契约：先读 Wrote() 再 Finish()（顺序颠倒会让已落盘的 body 吃到错误的收尾，见 Finish 注释）。
	_ = tw.Wrote()
	tw.Finish()
	terminal := tw.TakeTerminal()
	if len(terminal) == 0 {
		// 上游中断：网关合成的 completed 不是本轮结果，不落库、状态记 failed（Ruling 16）。
		reason := "upstream stream interrupted"
		h.finishResponseBackground(persist, bg.respID, model.ResponseStateStatusFailed, reason,
			nil, failedResponseBody(bg.respID, reason))
		return
	}
	var ev map[string]any
	if err := json.Unmarshal(terminal, &ev); err != nil {
		slog.Warn("decode background terminal event failed", "error", err, "response_id", bg.respID)
		h.finishResponseBackground(persist, bg.respID, model.ResponseStateStatusFailed,
			msgResponsesInvalidUpstream, nil, failedResponseBody(bg.respID, msgResponsesInvalidUpstream))
		return
	}
	raw, exists := ev["response"]
	if exists {
		raw = ev["response"]
	} else {
		raw = ev
	}
	resp, ok := raw.(map[string]any)
	if !ok {
		h.finishResponseBackground(persist, bg.respID, model.ResponseStateStatusFailed,
			msgResponsesInvalidUpstream, nil, failedResponseBody(bg.respID, msgResponsesInvalidUpstream))
		return
	}
	items := marshalResponseItems(buildResponsesItemsSnapshot(bg.state.history, bg.reqBody, responsesOutputItems(resp)))
	h.finishResponseBackground(persist, bg.respID, responsesStatusOf(resp), "", items, terminal)
}

// runResponseBackgroundPassthrough 是格 7（responses 入口 × openai-responses 上游）的后台执行。
//
// 直通路径的「流式」在同步实现里表现为「SSE 透传」，但后台任务没有客户端可透传：
// 因此统一用**非流式**上游调用（不带 stream），响应体按同步路径同一套 id/store 改写后落库。
// 这是有意的口径选择：后台任务的契约是「结果为一条完整响应」，而不是「结果是事件流」。
func (h *Handler) runResponseBackgroundPassthrough(
	ctx context.Context,
	persist context.Context,
	bg *responsesBackgroundRequest,
	start time.Time,
) {
	upstreamBody := buildResponsesPassthroughBody(bg.reqBody, bg.resolved.ModelID)
	delete(upstreamBody, "stream")
	outBody, err := json.Marshal(upstreamBody)
	if err != nil {
		h.finishResponseBackground(persist, bg.respID, model.ResponseStateStatusFailed, msgInternalError,
			nil, failedResponseBody(bg.respID, msgInternalError))
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(bg.resolved.URL, "/")+"/responses", bytes.NewReader(outBody))
	if err != nil {
		h.finishResponseBackground(persist, bg.respID, model.ResponseStateStatusFailed, msgInternalError,
			nil, failedResponseBody(bg.respID, msgInternalError))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bg.resolved.APIKey)
	for k, v := range bg.customHeaders {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := provider.DoSSRFProtectedRequest(req, h.ResponseBackgroundTimeout())
	if err != nil {
		slog.Error("background responses upstream request error", "error", err,
			"model", bg.resolved.ModelID, "response_id", bg.respID)
		h.finishResponseBackground(persist, bg.respID, model.ResponseStateStatusFailed, msgUpstreamConnect,
			nil, failedResponseBody(bg.respID, msgUpstreamConnect))
		return
	}
	defer resp.Body.Close()

	respBody, readErr := provider.ReadUpstreamBody(resp.Body)
	if readErr != nil {
		slog.Error("read background responses upstream body failed", "error", readErr,
			"model", bg.resolved.ModelID, "response_id", bg.respID)
		h.finishResponseBackground(persist, bg.respID, model.ResponseStateStatusFailed,
			msgResponsesInvalidUpstream, nil, failedResponseBody(bg.respID, msgResponsesInvalidUpstream))
		return
	}
	if resp.StatusCode < http.StatusBadRequest {
		h.logUsage(bg.key, bg.resolved, providerUsageFromResponsesBody(respBody), resp.StatusCode, start)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		reason := upstreamStatusMessage(resp.StatusCode)
		h.finishResponseBackground(persist, bg.respID, model.ResponseStateStatusFailed, reason,
			nil, failedResponseBody(bg.respID, reason))
		return
	}
	rewritten, parsed := buildResponsesBody(respBody, bg.respID, bg.requestedModel, true)
	if parsed == nil {
		h.finishResponseBackground(persist, bg.respID, model.ResponseStateStatusFailed,
			msgResponsesInvalidUpstream, nil, failedResponseBody(bg.respID, msgResponsesInvalidUpstream))
		return
	}
	items := marshalResponseItems(buildResponsesItemsSnapshot(bg.state.history, bg.reqBody, responsesOutputItems(parsed)))
	h.finishResponseBackground(persist, bg.respID, responsesStatusOf(parsed), "", items, rewritten)
}

// finishResponseBackground 是后台任务的**唯一终态出口**。
//
// 三件事：
//  1. 写回状态与载荷用 UpdateResponseStateStatus：它带 WHERE status <> 'cancelled'，
//     用户取消后任何终态写回都不会覆盖 cancelled（这是取消语义的**唯一**保障，
//     不要为了「先读再写」的直觉把它换成 Get + 无条件 Update）；
//  2. 若写回后行仍是 cancelled，说明用户取消了：丢弃这次写回并记 Info（见 verifyBackgroundFinalState）；
//  3. 写回后**重新读行**确认：若行仍是 cancelled（用户取消了），说明本次终态已被条件更新
//     挡下，记 Info 即可，不做任何补偿写入。
//
// 参数：items / body 为该轮的累积载荷；body 为空表示「没有可落库的结果」（失败路径传失败响应体）。
func (h *Handler) finishResponseBackground(
	ctx context.Context,
	respID, status, errMsg string,
	items, body []byte,
) {
	if err := h.store.UpdateResponseStateStatus(ctx, respID, status, errMsg, items, body); err != nil {
		slog.Error("finish background response failed", "error", err,
			"response_id", respID, "status", status)
		return
	}
	h.verifyBackgroundFinalState(ctx, respID, status)
}

// verifyBackgroundFinalState 在写回之后重新读取状态，识别「终态被取消挡下」：
// 判断依据必须是**写回之后**的状态，而不是写回前的快照——否则「写回前读、取消插在中间」
// 的窗口会让人误以为终态已经落库。
func (h *Handler) verifyBackgroundFinalState(
	ctx context.Context,
	respID, status string,
) (*model.ResponseState, bool) {
	row, err := h.store.GetResponseState(ctx, respID)
	switch {
	case errors.Is(err, model.ErrResponseStateNotFound):
		// 行已被保留期清理：状态无从观测，载荷也写不进去，记 Warn 即可。
		slog.Warn("background response row disappeared before final state", "response_id", respID)
		return nil, false
	case err != nil:
		slog.Warn("verify background response final state failed", "error", err, "response_id", respID)
		return nil, false
	case row.Status == model.ResponseStateStatusCancelled:
		slog.Info("background response cancelled, terminal state discarded", "response_id", respID,
			"attempted_status", status)
		return row, false
	}
	return row, true
}

// marshalResponseItems 序列化累积 items；编码失败返回 nil（零值永不落库，状态仍然正确）。
func marshalResponseItems(items []any) []byte {
	raw, err := json.Marshal(items)
	if err != nil {
		slog.Error("marshal background response items failed", "error", err)
		return nil
	}
	return raw
}

// failedResponseBody 把失败响应体序列化（编码失败返回 nil：此时只会丢失响应体，状态仍然正确）。
func failedResponseBody(respID, message string) []byte {
	return marshalJSONOrEmpty(buildFailedResponseBody(respID, message))
}

// buildFailedResponseBody 构造失败响应的响应体：id / object / status=failed / error.message。
//
// 为什么失败也要写这个体：GET 的契约是「返回该 response 的 response_body」，行里若留着
// 建行时的 "{}"，客户端拿到的就是一个既没有 status 也没有错误原因的空对象。
func buildFailedResponseBody(respID, message string) map[string]any {
	if message == "" {
		message = "background task failed"
	}
	return map[string]any{
		"id":     respID,
		"object": respObjectResponse,
		"status": model.ResponseStateStatusFailed,
		"output": []any{},
		"error":  map[string]any{"message": message, "type": "background_error"},
	}
}

// cancelResponseState 把仍在途（queued / in_progress）的行置为 cancelled；
// 返回 (true, row, nil) 表示取消生效，(false, row, nil) 表示行已是终态（调用方按 400 处理）。
// 行不存在返回 model.ErrResponseStateNotFound（由调用方映射为 404）。
//
// 取消生效时**同时把 response_body 的 status 改写为 cancelled**：GET 原样返回 response_body，
// 若不改写，客户端取消成功后 GET 仍看到 queued，取消就像没生效。
// 改写后的 body 交给调用方直接作为取消响应回包（不必再读一次行）。
func (h *Handler) cancelResponseState(ctx context.Context, respID string) (bool, *model.ResponseState, error) {
	row, err := h.store.GetResponseState(ctx, respID)
	if err != nil {
		return false, nil, err
	}
	switch row.Status {
	case model.ResponseStateStatusQueued, model.ResponseStateStatusInProgress:
	default:
		return false, row, nil
	}
	cancelled := cancelResponseObject(nil, row)
	if err := h.store.UpdateResponseStateStatus(ctx, respID, model.ResponseStateStatusCancelled,
		"", nil, marshalJSONOrEmpty(cancelled)); err != nil {
		return false, row, err
	}
	// 复核：条件更新可能被并发的「后台完成」抢在前面（那时行已是终态，取消无效）。
	// 以后台任务的终态为准，取消返回 400——正是 OpenAI「已结束不可取消」的语义。
	after, err := h.store.GetResponseState(ctx, respID)
	if err != nil {
		return false, row, err
	}
	if after.Status != model.ResponseStateStatusCancelled {
		return false, after, nil
	}
	row.ResponseBody = marshalJSONOrEmpty(cancelResponseObject(nil, row))
	return true, row, nil
}

// staleResponseStates 把「重启遗留」的在途行标记为 failed（规格 §5.6 并发与失败），返回处理行数。
//
// 判据是 updated_at 早于「现在 − 后台超时」，而不是单纯的进程启动时刻。理由：
//   - 健康任务每周期都会写 in_progress（updated_at 新鲜），因此年龄窗口不会误伤本实例的
//     在途任务；
//   - 而「早于启动时刻」会把**另一个进程**（滚动重启的新旧交叠、或同库旁挂的排障实例）正在
//     执行的任务一并打成 failed，随后那个进程还会写 completed，状态来回翻转。
//     本任务不跨实例迁移后台任务，但残留判据不该依赖「同一时刻只有一个进程写库」这个假设。
//
// 更新同样走 UpdateResponseStateStatus：WHERE status <> 'cancelled'，已取消的行维持原状。
func (h *Handler) staleResponseStates(ctx context.Context, now time.Time) int {
	cutoff := now.UTC().Add(-h.ResponseBackgroundTimeout())
	marked := 0
	for _, status := range []string{model.ResponseStateStatusQueued, model.ResponseStateStatusInProgress} {
		ids, err := h.store.ListResponseStatesUpdatedBefore(ctx, status, cutoff)
		if err != nil {
			slog.Warn("list stale response states failed", "error", err, "status", status)
			continue
		}
		for _, id := range ids {
			if err := h.store.UpdateResponseStateStatus(ctx, id, model.ResponseStateStatusFailed,
				responseRestartMessage, nil, nil); err != nil {
				slog.Warn("mark stale response state failed", "error", err, "response_id", id)
				continue
			}
			marked++
			slog.Info("marked stale background response as failed", "response_id", id, "previous_status", status)
		}
	}
	return marked
}

// responsesBufferWriter 是后台流式执行的输出目标：只记住是否被写入，**丢弃全部字节**。
//
// 后台任务的产出只经由翻译器的终态快照落库（与同步路径完全同源），这里保存事件流没有意义，
// 还会让长回答占用双份内存。实现 http.Flusher 是因为 provider 的流式接口要求 Flusher。
type responsesBufferWriter struct {
	wrote bool
}

// Header 返回一个丢弃用的响应头表（翻译器会往里写 Content-Type 等，无人读取）。
func (w *responsesBufferWriter) Header() http.Header { return http.Header{} }

// WriteHeader 丢弃状态码。
func (w *responsesBufferWriter) WriteHeader(int) {}

// Write 丢弃字节，只记录「写过」。
func (w *responsesBufferWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.wrote = true
	}
	return len(p), nil
}

// Flush 满足 provider 的 Flusher 要求（丢弃目标无需刷盘）。
func (w *responsesBufferWriter) Flush() {}

// 编译期断言：缓冲 writer 同时满足翻译器与 provider 对输出目标的要求。
var (
	_ http.ResponseWriter = (*responsesBufferWriter)(nil)
	_ http.Flusher        = (*responsesBufferWriter)(nil)
)
