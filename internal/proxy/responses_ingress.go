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
//       3. 有状态前置（本轮无状态：这些字段一律 400）；
//       4. 先对 **Responses 形态** 的 body 调 enforceMaxTokens（压 max_output_tokens）；
//       5. 再 responsesRequestToChat 转成 chat 形状（max_tokens 由第 4 步映射而来）；
//       6. 调 resolveAndCheckQuota（模型解析、配额）——不再二次压制。
//   - 按 resolved.ModelType 二选一（§5.4.1 第 5 步）：
//       openai-responses → 原生直通（格 7，仅替换 model、强制 store:false、改写终态 id）；
//       其余             → chat 转换（格 8/9，GetProvider 内部处理上游差异）。
//   - 收尾：logUsage（本轮不落库，store 由 Task 8 消费）。

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
	msgResponsesPreviousRespID  = "previous_response_id is not supported yet"
	msgResponsesConversation    = "conversation is not supported yet"
	msgResponsesBackground      = "background is not supported yet"
	msgResponsesInvalidUpstream = "invalid upstream response"
)

// handleResponses 处理 POST /v1/responses（本轮只有无状态路径）。
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

	// 3. 有状态前置：本轮无状态，相关字段一律拒绝（Task 8/9 放开）。
	// store 不在拒绝之列：按 OpenAI 语义缺省 true，本轮接受但不落库。
	if msg, ok := rejectStatefulResponsesFields(reqBody); !ok {
		httpx.WriteError(w, http.StatusBadRequest, msg)
		return
	}

	// 4. 先对 Responses 形态的 body 施加密钥上限，压的是 max_output_tokens。
	prepareResponsesTokens(key, reqBody)

	// 5. 按上游协议二选一：直通用原始 body（保真 Responses 专有结构），转换用 chat 形状。
	if resolved, ok := h.resolveAndCheckQuota(w, r, key, reqBody); !ok {
		return
	} else if resolved.ModelType == model.ModelTypeResponses {
		h.forwardResponsesPassthrough(w, r, key, resolved, reqBody, requestedModel, isStream, start)
		return
	}
	h.forwardViaChatConversion(w, r, key, reqBody, requestedModel, isStream, start)
}

// rejectStatefulResponsesFields 检查本轮不支持的有状态字段，返回对外错误文案。
//
// 判定口径：字段出现且非 null 即拒绝。JSON null 视同「未设置」（客户端序列化时常见的空值形态），
// background:false 这类显式假值同样拒绝——它表达的是「客户端在用它」，静默忽略会误导调用方。
// previous_response_id / conversation 触发 400 与「本轮无状态」一致；
// Task 8 的跨用户隔离用例（404 且不泄露存在性）届时替换本用例。
func rejectStatefulResponsesFields(reqBody map[string]any) (string, bool) {
	for _, field := range []struct{ key, msg string }{
		{"previous_response_id", msgResponsesPreviousRespID},
		{"conversation", msgResponsesConversation},
		{"background", msgResponsesBackground},
	} {
		if v, ok := reqBody[field.key]; ok && v != nil {
			return field.msg, false
		}
	}
	return "", true
}

// prepareResponsesTokens 对 Responses 形态的 body 施加密钥 max_output_tokens 上限。
//
// 顺序契约（design §5.4.1）：必须在 responsesRequestToChat 之前调用——转换后 body 带
// messages，isResponsesShaped 为假，enforceMaxTokens 不再压制 max_output_tokens。
func prepareResponsesTokens(key *model.APIKey, reqBody map[string]any) {
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
	reqBody map[string]any,
	requestedModel string,
	isStream bool,
	start time.Time,
) {
	chatBody, err := marshalResponsesChatBody(reqBody)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, responsesConversionMessage(err))
		return
	}
	resolved, body, ok := h.prepareForward(w, r, key, chatBody)
	if !ok {
		return
	}
	respID := newResponsesID()

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
		h.logUsage(key, resolved, completionResultUsage(result), statusCode, start)
		httpx.WriteError(w, statusCode, upstreamStatusMessage(statusCode))
		return
	}
	out, err := chatResponseToResponses(result.Body, requestedModel, respID)
	if err != nil {
		h.logUsage(key, resolved, completionResultUsage(result), http.StatusBadGateway, start)
		slog.Error("chat to responses conversion failed", "error", err, "model", resolved.ModelID)
		httpx.WriteError(w, http.StatusBadGateway, msgResponsesInvalidUpstream)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	h.logUsage(key, resolved, completionResultUsage(result), http.StatusOK, start)
}

// marshalResponsesChatBody 把（已压制的）Responses 请求体转成 chat 请求体。
// 转换器返回的英文错误原样作为 400 文案（invalid input / invalid tools / missing model）。
func marshalResponsesChatBody(reqBody map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, errors.New("invalid request body")
	}
	return responsesRequestToChat(raw)
}

// responsesConversionMessage 把转换器错误映射为对外 400 文案。
func responsesConversionMessage(err error) string {
	switch message := err.Error(); message {
	case msgResponsesInvalidInput, msgResponsesInvalidTools:
		return message
	case "missing model":
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
) {
	outBody, err := json.Marshal(buildResponsesPassthroughBody(reqBody, resolved.ModelID))
	if err != nil {
		slog.Error("responses passthrough body rewrite failed", "error", err, "model", resolved.ModelID)
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
			respBody = rewriteResponsesID(respBody, newResponsesID())
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
		gatewayID:     newResponsesID(),
		requestedName: requestedModel,
	}
	_, _ = io.Copy(sink, resp.Body)
	sink.close()
	h.logUsage(key, resolved, sink.usage, resp.StatusCode, start)
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
	out, err := json.Marshal(ev)
	if err != nil {
		return payload, false
	}
	return string(out), true
}

// rewriteResponsesID 改写 JSON 响应体顶层的 id 为网关 ID；模型名回显为客户端请求名。
// 解析失败时原样返回（直通路径不回退成错误）。
func rewriteResponsesID(body []byte, respID string) []byte {
	var resp map[string]any
	if json.Unmarshal(body, &resp) != nil || resp == nil {
		return body
	}
	resp["id"] = respID
	out, err := json.Marshal(resp)
	if err != nil {
		return body
	}
	return out
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

// newResponsesID 生成网关响应 ID：resp_ + 32 位随机十六进制（crypto/rand）。
func newResponsesID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 在本平台不可用时无法生成不可预测 ID；返回空串会被上层判为非法。
		slog.Error("generate responses id failed", "error", err)
		return responsesIDPrefix
	}
	return responsesIDPrefix + hex.EncodeToString(b[:])
}
