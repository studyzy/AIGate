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

// 本文件实现 OpenAI Responses 协议的入站管线（POST /v1/responses）：
//   - 鉴权：Authorization: Bearer；错误恒为 OpenAI 风格 {"error":{...}}；
//   - 执行顺序（design §5.4.1，顺序不可颠倒）：
//       1. 读 Responses 原始 body；
//       2. 解析 model / stream / store / previous_response_id / conversation / background；
//       3. prepareResponsesState：站点开关取舍、两者互斥、取父行/会话并校验归属（失败 404）、
//          把父行 items 前置于本轮 input（background 仍 400，属 Task 9）；
//       4. prepareResponsesTokens：**在转 chat 之前**对 Responses 形态 body 压 max_output_tokens
//          （enforceMaxTokens 靠请求体形态判定协议，转成 chat 后再压就压不到了）；
//       5. resolveAndCheckQuota：模型解析 + 配额（**每条路径恰好一次**）；
//       6. 按 resolved.ModelType 二选一：
//            openai-responses → forwardResponsesPassthrough（格 7，用原始 Responses body，
//                               仅替换 model、强制 store:false、改写终态 id）；
//                               上限完全由第 4 步施加。
//            其余             → forwardViaChatConversion（格 8/9，先 responsesRequestToChat
//                               转 chat，再由 GetProvider 处理上游差异）；转出的 chat 形态
//                               再做一次**兜底压制**（正常流下幂等无操作，用于兜住转换器
//                               未映射 max_output_tokens 的情形，详见该函数内的注释）。
//   - 「解析 + 配额」每条路径恰好一次（第 5 步），两条路径都不调用 prepareForward。
//   - 收尾：logUsage + store 为 true 时落一行 response_states（流式在流结束后）。

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"aigate/internal/httpx"
	"aigate/internal/model"
	"aigate/internal/provider"
)

const (
	// responsesSSEMaxLineBytes 是直通 SSE 单行上限：终态事件可能携带完整 output 与 usage。
	responsesSSEMaxLineBytes = 8 * 1024 * 1024

	// responsesIDPrefix 是网关生成的响应 ID 前缀；随机部分为 32 位十六进制。
	responsesIDPrefix = "resp_"
)

// Responses 入口专用错误文案（恒英文）。
const (
	msgResponsesInvalidInput    = "invalid input"
	msgResponsesInvalidTools    = "invalid tools"
	msgResponsesBackground      = "background is not supported yet"
	msgResponsesInvalidUpstream = "invalid upstream response"

	// msgResponsesNotFound 同时用于「行不存在」与「行不属于本用户」：
	// 两种情形的状态码与响应体必须逐字一致，否则可被用来枚举他人的响应 ID（§5.6 安全）。
	msgResponsesNotFound = "previous response not found"
	// msgResponsesStorageDisabled：站点关闭了响应存储（-allow-response-storage=false），
	// 而客户端显式要求 store:true——明确拒绝，而不是静默改写。
	msgResponsesStorageDisabled = "response storage is disabled on this gateway"
	// msgResponsesStatefulDisabled：站点关闭存储时，previous_response_id / conversation 一律不可用。
	msgResponsesStatefulDisabled = "response storage is disabled on this gateway, previous_response_id and conversation are unavailable"
	// msgResponsesContinuationConflict：两者互斥（OpenAI 语义）。
	msgResponsesContinuationConflict = "previous_response_id and conversation are mutually exclusive"
	// msgResponsesInvalidStore：store 必须是布尔值。
	msgResponsesInvalidStore = "invalid store"
)

// handleResponses 处理 POST /v1/responses。
func (h *Handler) handleResponses(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// 1. 密钥鉴权（Bearer）。
	key := h.authKey(w, r)
	if key == nil {
		return
	}

	// 2. 读取并解析 Responses 原始 body（未知字段保留在 map 中）。
	respBody, err := httpx.ReadBody(r)
	if err != nil {
		if errors.Is(err, httpx.ErrBodyTooLarge) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, msgBodyTooLarge)
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, msgReadRequestBody)
		return
	}
	var reqBody map[string]any
	if err := json.Unmarshal(respBody, &reqBody); err != nil || reqBody == nil {
		httpx.WriteError(w, http.StatusBadRequest, msgInvalidJSON)
		return
	}
	requestedModel := extractRequestedModel(reqBody)
	isStream, _ := reqBody["stream"].(bool)

	// 3. 有状态前置：站点开关取舍 → 两者互斥 → 取父行/会话并校验归属 → 历史前置于 input。
	//    必须早于第 4 步（见 responses_state.go 顶部注释）：拼接只动 input，不动
	//    max_output_tokens / messages，因此不会让第 4 步的协议形态判定失效。
	state, ok := h.prepareResponsesState(w, r, key, reqBody)
	if !ok {
		return
	}

	// 4. 先对 Responses 形态的 body 施加密钥上限，压的是 max_output_tokens（§5.4.1 第 4 步）。
	prepareResponsesTokens(key, reqBody)

	// 5. 解析模型 + 配额（每条路径恰好一次），再按上游协议二选一。
	//    这里**不能**用 prepareForward：它会原地改写传入的 body。转换路径传入的是 Responses
	//    形态 body，一旦经过它，max_output_tokens 会被再压一次（并混入 chat 的 max_tokens），
	//    第 4 步是否真的执行就再也观测不到了——顺序契约重新变成盲区（review I-2）。
	//    因此解析/配额走 resolveAndCheckQuota，两条路径各自施加自己的一处上限压制：
	//      · 直通：第 4 步已压 max_output_tokens，路径内只改 model 与 store；
	//      · 转换：第 4 步 → 转换器映射成 max_tokens → 转换路径内再幂等复压。
	resolved, ok := h.resolveAndCheckQuota(w, r, key, reqBody)
	if !ok {
		return
	}

	// 6. 按上游协议二选一。
	if resolved.ModelType == model.ModelTypeResponses {
		h.forwardResponsesPassthrough(w, r, key, resolved, reqBody, requestedModel, isStream, start, state)
		return
	}
	h.forwardViaChatConversion(w, r, key, resolved, reqBody, requestedModel, isStream, start, state)
}

// prepareResponsesTokens 对 Responses 形态的 body 施加密钥 max_output_tokens 上限。
//
// 顺序契约（design §5.4.1）：必须在 responsesRequestToChat 之前调用——转换后 body 带
// messages，isResponsesShaped 为假，enforceMaxTokens 不再压制 max_output_tokens。
//
// 口径说明（有意为之，非疏漏）：Responses 形态下，max_output_tokens 为 null / 字符串 / 布尔
// 等**非数值**时会被改写为密钥上限；而 chat 形态下这类值反而会被**原样透传**（chat 的上限
// 一律经 max_tokens 施加）。两者不一致是 §5.4.1 裁决的结果：Responses 上游的
// max_output_tokens 是唯一的输出预算字段，留一个非数值给上游等于放弃上限保护；
// chat 的 max_output_tokens 则是非标准字段，改写它会破坏对严格上游的兼容性。
func prepareResponsesTokens(key *model.APIKey, reqBody map[string]any) {
	// 只认 Responses 形态（带 max_output_tokens、无 messages）。
	// 已转换的 chat 形态绝不能被本步骤改写：否则转换路径的第二道压制变成冗余，
	// 第 4 步是否真的执行就再也观测不到（review I-2 的根因）。
	if _, isChat := reqBody["messages"]; isChat {
		return
	}
	if key.MaxTokens <= 0 {
		return
	}
	// 客户端没给 max_output_tokens：先补上让它成为 Responses 形态，
	// 再由 enforceMaxTokens 统一收敛（它只压制已存在的字段，不会主动注入该字段）。
	if _, exists := reqBody["max_output_tokens"]; !exists {
		reqBody["max_output_tokens"] = float64(key.MaxTokens)
	}
	enforceMaxTokens(reqBody, key.MaxTokens)
}

// forwardViaChatConversion 把 Responses 请求转成 chat 请求交给 provider 管线，
// 再把 chat 响应（或 chat SSE）翻译回 Responses 形状。
func (h *Handler) forwardViaChatConversion(
	w http.ResponseWriter,
	r *http.Request,
	key *model.APIKey,
	resolved *model.Resolved,
	reqBody map[string]any,
	requestedModel string,
	isStream bool,
	start time.Time,
	state *responsesStatefulInput,
) {
	respID := newResponsesID()
	if respID == "" {
		httpx.WriteError(w, http.StatusInternalServerError, msgInternalError)
		return
	}
	chatBody, err := marshalResponsesChatBody(reqBody)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, responsesConversionMessage(err))
		return
	}
	// 兜底压制（**不要当成冗余删掉**）：
	//   · 正常流下幂等无操作——第 4 步已把 max_output_tokens 收敛为上限，转换器再把它
	//     映射成 max_tokens，这里压的值与已有值相同；
	//   · 真实价值在于兜住「转换器漏映射 max_output_tokens」的情形。实测成对变异：
	//     仅让转换器漏映射（本压制仍在）→ 上游 max_tokens 仍为上限（被这里补上）；
	//     转换器漏映射 **且** 删掉本压制 → 上游 chat body 里 max_tokens 直接缺失，
	//     请求等于没有输出预算上限（TestResponsesIngress_ChatUpstreamCapOnlyOnce 变红）；
	//   · 同时覆盖 max_completion_tokens 等 chat 侧字段。
	enforceMaxTokens(chatBody, key.MaxTokens)
	chatBody["model"] = resolved.ModelID
	body, err := json.Marshal(chatBody)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, msgInvalidJSON)
		return
	}

	p := provider.GetProvider(resolved.ModelType)
	customHeaders := extractCustomHeaders(r, resolved.CustomHTTPHeaders)
	ctx := r.Context()

	if isStream {
		flusher, ok := w.(http.Flusher)
		if !ok {
			httpx.WriteError(w, http.StatusInternalServerError, msgStreamNotSupported)
			return
		}
		tw := &responsesTranslationWriter{
			responsesStreamTranslator: newResponsesStreamTranslator(w, flusher, requestedModel, respID),
			w:                         w,
		}
		result, statusCode, err := p.ChatCompletionStream(
			ctx,
			resolved.APIKey,
			resolved.URL,
			resolved.ModelID,
			body,
			tw,
			flusher,
			customHeaders,
		)
		if err != nil {
			// 错误细节只进日志，不回显给调用方（避免泄漏内网地址/拓扑）。
			slog.Error("upstream stream error", "error", err, "model", resolved.ModelID)
			// 契约：先判 Wrote() 再 Finish()（顺序颠倒会让已落盘的 body 吃到 502）。
			if !tw.Wrote() {
				httpx.WriteError(w, http.StatusBadGateway, msgUpstreamConnect)
				return
			}
			// 已写出部分事件后中断：不补发收尾事件，让客户端感知到流不完整。
			return
		}
		// 回退 502 的依据是「上游状态码异常」，不是 Wrote() 为假：
		// 2xx 但零增量时 Finish() 仍会写出 created/in_progress/completed。
		if statusCode >= http.StatusBadRequest && !tw.Wrote() {
			h.logUsage(key, resolved, streamResultUsage(result), statusCode, start)
			httpx.WriteError(w, statusCode, upstreamStatusMessage(statusCode))
			return
		}
		tw.Finish()
		h.persistResponsesStream(r, key, state, respID, requestedModel, reqBody,
			tw.responsesStreamTranslator)
		h.logUsage(key, resolved, streamResultUsage(result), statusCode, start)
		return
	}

	result, statusCode, err := p.ChatCompletion(
		ctx,
		resolved.APIKey,
		resolved.URL,
		resolved.ModelID,
		body,
		customHeaders,
	)
	if err != nil {
		slog.Error("upstream request error", "error", err, "model", resolved.ModelID)
		httpx.WriteError(w, http.StatusBadGateway, msgUpstreamConnect)
		return
	}
	if statusCode != http.StatusOK {
		// 与流式路径、直通路径一致：原样回写上游状态码与响应体。
		// 丢弃上游 body（换成本地拼的 "upstream returned status N"）会让调用方看不到
		// 真正原因（如 context_length_exceeded），也造成流式/非流式行为不一致。
		//
		// 上游没给 body 时才兜底：仍然回 OpenAI 风格错误体（本响应恒定标 application/json，
		// 回纯文本会让客户端按 JSON 解析失败、错误信息随之丢失）。
		upstreamBody := []byte(upstreamStatusMessage(statusCode))
		if result != nil && len(result.Body) > 0 {
			upstreamBody = result.Body
		} else if encoded, encodeErr := json.Marshal(openAIErrorBody(upstreamStatusMessage(statusCode), statusCode)); encodeErr == nil {
			upstreamBody = encoded
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_, _ = w.Write(upstreamBody)
		h.logUsage(key, resolved, completionResultUsage(result), statusCode, start)
		return
	}
	out, err := chatResponseToResponses(result.Body, requestedModel, respID)
	if err != nil {
		h.logUsage(key, resolved, completionResultUsage(result), http.StatusBadGateway, start)
		slog.Error("chat to responses conversion failed", "error", err, "model", resolved.ModelID)
		httpx.WriteError(w, http.StatusBadGateway, msgResponsesInvalidUpstream)
		return
	}
	// 回显 store 并落库（响应已完整构造，落库失败不影响本次回包）。
	out = normalizeResponsesStore(out, state.store)
	if state.store {
		var resp map[string]any
		if json.Unmarshal(out, &resp) == nil {
			items := buildResponsesItemsSnapshot(state.history, reqBody, responsesOutputItems(resp))
			h.persistResponsesState(r.Context(), key, respID, state, requestedModel, items, out)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	h.logUsage(key, resolved, completionResultUsage(result), http.StatusOK, start)
}

// persistResponsesStream 是转换路径流式收尾时的落库：累积 items 取自翻译器已合成的
// 终态 output（message + 各 function_call），响应体取自翻译器写出的终态事件。
//
// 【为什么上游中断时**故意不落库**——这不是遗漏，请不要「修」回去】
//
// 上游流中途断开时，Finish() 会补发一个 response.completed 给客户端；但那一刻上游并没有
// 给出结果，这个终态是网关凭空合成的。本函数据此**不落库**，于是出现一个有意的不对称：
//
//	客户端收到了终止信号（合成的 completed），该轮却不会成为下一轮的父行。
//
// 三条理由：
//  1. 「落库内容 = 客户端所见」这条不变式的方向是**单向**的：它禁止落「客户端没见到的内容」，
//     并不要求「客户端见了什么就必须落什么」。合成的 completed 恰恰是客户端见到、但上游
//     从未产出的内容——把它落库，等于让网关自己伪造一轮对话的作者。
//  2. 半截对话一旦落库就会成为 previous_response_id / conversation 的父行：下一轮会拿它
//     的 items 拼历史，于是**续接重放一份客户端从未真正收到的结果**（被中断的那半轮）。
//     这正是 fix 轮 I-2 要堵的洞，而不是要绕开的边界情况。
//  3. 强改回「落库合成终态」会同时打破 I-1 的不变式（落库内容必须等于客户端所见的上游结果）
//     并让中断轮在会话里看起来与正常轮完全一样，运维再也分不出哪些轮次其实失败了。
//
// 对照实验：TestResponseState_StreamingInterruptedDoesNotPersist（上游只发部分 chunk 即断开
// → 落库 0 行，同时断言客户端**仍**收到 response.completed，证明这不是「没有终态事件」）。
func (h *Handler) persistResponsesStream(
	r *http.Request,
	key *model.APIKey,
	state *responsesStatefulInput,
	respID, requestedModel string,
	reqBody map[string]any,
	t *responsesStreamTranslator,
) {
	if !state.store {
		return
	}
	// take：终态快照消费即清。为空即「上游未自行收尾」（流被中断，quick 收尾是 Finish 合成的）
	// → **不落库**，理由见上方函数注释「为什么上游中断时故意不落库」。
	terminal := t.TakeTerminal()
	if len(terminal) == 0 {
		return
	}
	items := buildResponsesItemsSnapshot(state.history, reqBody, t.finalOutput())
	h.persistResponsesStreamState(r.Context(), key, respID, state, requestedModel, items, terminal)
}

// openAIErrorBody 构造 OpenAI 风格的错误体 {"error":{"message","type","code"}}，
// 与 httpx.WriteError 的结构一致，用于需要自行控制状态码/落盘的场景。
func openAIErrorBody(message string, statusCode int) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "upstream_error",
			"code":    statusCode,
		},
	}
}

// marshalResponsesChatBody 把（已压制的）Responses 请求体转成 chat 请求体。
// 转换器返回的英文错误原样作为 400 文案（invalid input / invalid tools / missing model）。
func marshalResponsesChatBody(reqBody map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, errResponsesInvalidBody
	}
	return responsesRequestToChat(raw)
}

// responsesConversionMessage 把转换器错误映射为对外 400 文案。
// 用 errors.Is 判定包级哨兵，而不是比对错误字符串——后者一旦转换器改文案就会静默退化成
// 通用文案（客户端拿到的错误提示随之失真）。
func responsesConversionMessage(err error) string {
	switch {
	case errors.Is(err, errResponsesInvalidInput):
		return msgResponsesInvalidInput
	case errors.Is(err, errResponsesInvalidTools):
		return msgResponsesInvalidTools
	case errors.Is(err, errResponsesMissingModel):
		return msgModelRequired
	default:
		return msgInvalidJSON
	}
}

// responsesTranslationWriter 让流式翻译器可作为 http.ResponseWriter 传给 provider
// （Header/WriteHeader 透传底层，Write 交给翻译器）。
type responsesTranslationWriter struct {
	*responsesStreamTranslator
	w http.ResponseWriter
}

func (tw *responsesTranslationWriter) Header() http.Header  { return tw.w.Header() }
func (tw *responsesTranslationWriter) WriteHeader(code int) { tw.w.WriteHeader(code) }

// --- 直通路径：openai-responses 上游（格 7） ---

// forwardResponsesPassthrough 把 Responses 请求原样转发到 Responses 上游，并原样回传响应。
//
// 请求体只做三件事：替换 model 为解析命中的上游 ID、max_output_tokens 已按密钥上限压制、
// store 强制改写为 false（数据面最小化：不把同一份正文同时留在上游）。
func (h *Handler) forwardResponsesPassthrough(
	w http.ResponseWriter,
	r *http.Request,
	key *model.APIKey,
	resolved *model.Resolved,
	reqBody map[string]any,
	requestedModel string,
	isStream bool,
	start time.Time,
	state *responsesStatefulInput,
) {
	outBody, err := json.Marshal(buildResponsesPassthroughBody(reqBody, resolved.ModelID))
	if err != nil {
		slog.Error("responses passthrough body rewrite failed", "error", err, "model", resolved.ModelID)
		httpx.WriteError(w, http.StatusInternalServerError, msgInternalError)
		return
	}
	respID := newResponsesID()
	if respID == "" {
		httpx.WriteError(w, http.StatusInternalServerError, msgInternalError)
		return
	}

	url := strings.TrimRight(resolved.URL, "/") + "/responses"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(outBody))
	if err != nil {
		slog.Error("build responses passthrough request failed", "error", err, "model", resolved.ModelID)
		httpx.WriteError(w, http.StatusBadGateway, msgUpstreamConnect)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+resolved.APIKey)
	for k, v := range extractCustomHeaders(r, resolved.CustomHTTPHeaders) {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}

	timeout := responsesIngressTimeout
	if isStream {
		timeout = responsesIngressStreamTimeout
	}
	resp, err := provider.DoSSRFProtectedRequest(req, timeout)
	if err != nil {
		slog.Error("responses upstream request error", "error", err, "model", resolved.ModelID)
		httpx.WriteError(w, http.StatusBadGateway, msgUpstreamConnect)
		return
	}
	defer resp.Body.Close()

	// 非流式（或流式请求遇到上游错误）：JSON 原样回传（仅改写 id）。
	if !isStream || resp.StatusCode >= http.StatusBadRequest {
		respBody, readErr := provider.ReadUpstreamBody(resp.Body)
		if readErr != nil {
			slog.Error("read responses upstream response failed", "error", readErr, "model", resolved.ModelID)
			httpx.WriteError(w, http.StatusBadGateway, msgResponsesInvalidUpstream)
			return
		}
		if resp.StatusCode < http.StatusBadRequest {
			// 改写 id 之后，响应体的 id 与 store 已经是网关口径；落库与回包使用同一份字节。
			rewritten, parsed := buildResponsesBody(respBody, respID, requestedModel, state.store)
			respBody = rewritten
			if state.store && parsed != nil {
				items := buildResponsesItemsSnapshot(state.history, reqBody, responsesOutputItems(parsed))
				h.persistResponsesState(r.Context(), key, respID, state, requestedModel, items, respBody)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
		h.logUsage(key, resolved, providerUsageFromResponsesBody(respBody), resp.StatusCode, start)
		return
	}

	// 流式：SSE 头就绪后逐行透传，仅在终态事件上改写 id 并抓取 usage。
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpx.WriteError(w, http.StatusInternalServerError, msgStreamNotSupported)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)

	sink := &responsesPassthroughStream{
		w:             w,
		flusher:       flusher,
		gatewayID:     respID,
		requestedName: requestedModel,
		store:         state.store,
	}
	_, _ = io.Copy(sink, resp.Body)
	sink.close()
	// takeTerminal 是「消费即清」：取走终态快照后 stream 不再持有引用。
	if terminal := sink.takeTerminal(); terminal != nil {
		items := buildResponsesItemsSnapshot(state.history, reqBody, responsesOutputItems(terminal))
		h.persistResponsesState(r.Context(), key, respID, state, requestedModel, items,
			marshalJSONOrEmpty(terminal))
	}
	h.logUsage(key, resolved, sink.usage, resp.StatusCode, start)
}

// marshalJSONOrEmpty 序列化落库用的响应体；失败返回空字节（persistResponsesState 会按 {} 处理）。
func marshalJSONOrEmpty(v map[string]any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		slog.Error("marshal stored responses body failed", "error", err)
		return nil
	}
	return raw
}

// buildResponsesPassthroughBody 复刻 Responses 请求体，仅替换 model 并强制 store:false。
// max_output_tokens 的密钥上限压制已在 prepareResponsesTokens 完成（§5.4.1 顺序契约）。
func buildResponsesPassthroughBody(reqBody map[string]any, upstreamModelID string) map[string]any {
	out := make(map[string]any, len(reqBody)+2)
	for k, v := range reqBody {
		out[k] = v
	}
	out["model"] = upstreamModelID
	// 安全红线：恒关闭上游存储，忽略入站 store。
	out["store"] = false
	return out
}

// responsesIngressTimeout / responsesIngressStreamTimeout 是直通路径的上游超时
// （与 provider 侧 responses 适配器一致：非流式 5 分钟、流式 30 分钟）。
const (
	responsesIngressTimeout       = 5 * time.Minute
	responsesIngressStreamTimeout = 30 * time.Minute
)

// responsesPassthroughStream 是直通路径的 SSE 写出器：逐行扫描上游，只解析终态事件
// （response.completed / incomplete / failed）的 data 行，改写 id / model 并抽取 usage；
// 其余行原样写出（design §5.4.3：流式扫描，不重编码大响应）。
type responsesPassthroughStream struct {
	w             http.ResponseWriter
	flusher       http.Flusher
	gatewayID     string
	requestedName string
	// pending 缓存尚未遇到换行的行片段。
	pending []byte
	// event 是当前事件块的 event: 行取值（为空的块按 data.type 判定）。
	event string
	usage *provider.Usage
	// store 是回显给客户端的 store 值（上游一律被改写为 false，客户端看到的必须是网关口径）。
	store bool
	// terminal 是**首个**终态事件里 response 对象的快照，供有状态路径落库；
	// 落库后由 takeTerminal 取走并置 nil。只有 store 为真时才保留（不落库的请求不占内存）。
	terminal map[string]any
}

// takeTerminal 取走并清空终态快照（消费即清：返回后 stream 不再持有该引用）。
func (s *responsesPassthroughStream) takeTerminal() map[string]any {
	terminal := s.terminal
	s.terminal = nil
	return terminal
}

// Write 实现 io.Writer：按行切分后逐行处理。
func (s *responsesPassthroughStream) Write(p []byte) (int, error) {
	for len(p) > 0 {
		if idx := bytes.IndexByte(p, '\n'); idx >= 0 {
			line := append(s.pending, p[:idx+1]...)
			s.pending = nil
			if _, err := s.writeLine(line); err != nil {
				return len(p), err
			}
			p = p[idx+1:]
			continue
		}
		if len(s.pending) < responsesSSEMaxLineBytes {
			s.pending = append(s.pending, p...)
		}
		break
	}
	s.flusher.Flush()
	return len(p), nil
}

// close 写出残留的最后一行片段（上游末行不带换行时）。
func (s *responsesPassthroughStream) close() {
	if len(s.pending) == 0 {
		return
	}
	line := s.pending
	s.pending = nil
	_, _ = s.writeLine(line)
	s.flusher.Flush()
}

// writeLine 处理单行：普通行原样写出；终态事件的 data 行改写后再写出。
func (s *responsesPassthroughStream) writeLine(line []byte) (int, error) {
	text := string(line)
	body := strings.TrimSuffix(text, "\n")
	body = strings.TrimSuffix(body, "\r")
	if body == "" {
		// 空行是事件分隔符：本块结束，清空 event 记忆。
		s.event = ""
		return s.raw(line)
	}
	if rest, ok := strings.CutPrefix(body, "event:"); ok {
		s.event = strings.TrimSpace(rest)
		return s.raw(line)
	}
	payload, ok := strings.CutPrefix(body, "data:")
	if !ok {
		return s.raw(line)
	}
	out, changed := s.rewriteEvent(strings.TrimSpace(payload))
	if !changed {
		return s.raw(line)
	}
	// 保留原行的换行符，只替换 data 负载。
	return s.raw([]byte("data: " + out + text[len(body):]))
}

// raw 把一行原样写出。
func (s *responsesPassthroughStream) raw(line []byte) (int, error) {
	n, err := s.w.Write(line)
	s.flusher.Flush()
	return n, err
}

// rewriteEvent 解析终态事件并改写 id / model；返回改写后的 data 与是否发生了改写。
func (s *responsesPassthroughStream) rewriteEvent(payload string) (string, bool) {
	if payload == "" || payload == "[DONE]" {
		return payload, false
	}
	var ev map[string]any
	if json.Unmarshal([]byte(payload), &ev) != nil {
		return payload, false
	}
	eventType, _ := ev["type"].(string)
	if eventType == "" {
		eventType = s.event
	}
	switch eventType {
	case "response.completed", "response.incomplete", "response.failed":
	default:
		return payload, false
	}
	if u := providerUsageFromResponsesData(payload); u != nil {
		s.usage = u
	}
	target, ok := ev["response"].(map[string]any)
	if !ok {
		// 扁平形状（极少见）：顶层就是 response 对象。
		target = ev
	}
	target["id"] = s.gatewayID
	if s.requestedName != "" {
		if _, exists := target["model"]; exists {
			target["model"] = s.requestedName
		}
	}
	target["store"] = s.store
	// 不变式：**落库内容必须等于客户端所见**。
	//   · 只保留**首个**终态事件：客户端收到的是它，后续终态事件（上游异常多发的
	//     response.completed，或同一块内的多个 data: 行）不得改写落库内容——否则异常上游
	//     可以用后一个终态决定下一轮续接时重放什么；
	//   · usage 仍取各终态事件里最后一个非空值（usage 的注释见上），与这里互不影响。
	if s.store && s.terminal == nil {
		s.terminal = target
	}
	out, err := json.Marshal(ev)
	if err != nil {
		return payload, false
	}
	return string(out), true
}

// providerUsageFromResponsesBody 从 Responses 响应体解析用量；无 usage 时返回 nil。
func providerUsageFromResponsesBody(body []byte) *provider.Usage {
	return providerUsageFromResponsesData(string(body))
}

// providerUsageFromResponsesData 从 Responses 响应体或 SSE data 中解析用量。
// 口径与 provider 侧适配器一致：output_tokens 已含 reasoning_tokens，不重复累加。
func providerUsageFromResponsesData(data string) *provider.Usage {
	var env struct {
		Usage    *responsesIngressUsage `json:"usage"`
		Response *struct {
			Usage *responsesIngressUsage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal([]byte(data), &env) != nil {
		return nil
	}
	u := env.Usage
	if u == nil && env.Response != nil {
		u = env.Response.Usage
	}
	if u == nil {
		return nil
	}
	return &provider.Usage{
		PromptTokens:           u.InputTokens,
		CompletionTokens:       u.OutputTokens,
		TotalTokens:            u.TotalTokens,
		PromptCacheReadTokens:  u.InputTokensDetails.CachedTokens,
		PromptCacheWriteTokens: u.InputTokensDetails.CacheWriteTokens,
	}
}

// responsesIngressUsage 是 Responses 用量对象的入站解析形状。
type responsesIngressUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
}

// randRead 是 crypto/rand.Read 的间接引用，**仅作为测试缝**存在：
// 熵源失败（newResponsesID 返回空串 → 调用方回 500）在生产环境无法稳定复现，
// 只能靠替换本变量来覆盖那条分支（TestNewResponsesID_ErrorReturnsEmpty）。
//
// 生产路径行为不变：仍是同一份 crypto/rand.Read，无额外开销、无状态。
// 之所以保留包级 var 而不是改成依赖注入：这里只有一个调用点，注入会把
// newResponsesID 的签名污染到所有调用方；该变量在运行期只读，已由 -race 全量用例验证无竞态。
var randRead = rand.Read

// newResponsesID 生成网关响应 ID：resp_ + 32 位随机十六进制（crypto/rand）。
func newResponsesID() string {
	var b [16]byte
	if _, err := randRead(b[:]); err != nil {
		// 熵源不可用时无法生成不可预测 ID：返回空串，由调用方按 500 拒绝。
		// 绝不能回落到 "resp_" 之类的固定值——那会成为一个看似合法、实则可能与他人重复的 ID。
		slog.Error("generate responses id failed", "error", err)
		return ""
	}
	return responsesIDPrefix + hex.EncodeToString(b[:])
}
