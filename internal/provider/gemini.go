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

package provider

// 本文件实现 Google 生成式语言 API 的上游适配器。
// Provider 接口契约是「恒吐 OpenAI-chat 形状」，故方向是
// chat 请求 → Gemini generateContent 请求、Gemini 响应/流 → chat。
// 关键取舍见 docs/superpowers/specs/2026-11-24-gemini-upstream-design.md §5。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// GeminiProvider 适配 Google Generative Language API（generativelanguage.googleapis.com）。
type GeminiProvider struct{}

var _ Provider = (*GeminiProvider)(nil)

// prepare 构造上游请求体。请求本身不合法（如 n>1）时返回可直接回传的
// 400 响应，而不是当成上游故障——调用方需要知道是自己的请求有问题。
func (p *GeminiProvider) prepare(model string, reqBody []byte) (body []byte, rejected *geminiRejected) {
	body, err := buildGeminiRequest(model, reqBody)
	if err == nil {
		return body, nil
	}
	var reqErr *geminiRequestError
	if !errors.As(err, &reqErr) {
		// 兜底：其余构造期错误也按 400 回给调用方（它们同样源于请求体）。
		return nil, &geminiRejected{
			status: http.StatusBadRequest,
			body:   errorBody(reqErrMessage(err), http.StatusBadRequest),
		}
	}
	return nil, &geminiRejected{
		status: http.StatusBadRequest,
		body:   errorBody(reqErr.Error(), http.StatusBadRequest),
	}
}

// errorBody 构造与入口一致的错误体。
func errorBody(message string, status int) []byte {
	raw, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "invalid_request_error",
			"code":    status,
		},
	})
	return raw
}

// reqErrMessage 取错误的可读文案。
func reqErrMessage(err error) string {
	if err == nil {
		return "invalid request"
	}
	return err.Error()
}

// geminiRequestError 标记「调用方请求不合法」，与上游/网络故障区分。
type geminiRequestError struct{ msg string }

func (e *geminiRequestError) Error() string { return e.msg }

// geminiRejected 是可直接回传给调用方的错误响应。
type geminiRejected struct {
	status int
	body   []byte
}

const (
	// geminiDefaultBase 是未配置 url 时的默认端点。
	geminiDefaultBase = "https://generativelanguage.googleapis.com/v1beta"
	// geminiUpstreamTimeout 是非流式上游超时（与 anthropic 对齐）。
	geminiUpstreamTimeout = 5 * time.Minute
	// geminiStreamTimeout 是流式上游超时（与 anthropic 对齐）。
	geminiStreamTimeout = 30 * time.Minute
	// geminiSSEMaxLineBytes 是单行上限：含 inlineData 的帧会超过 bufio 默认 64 KiB。
	geminiSSEMaxLineBytes = 4 * 1024 * 1024
)

// baseURL 返回上游 base（去尾斜杠），未配置时用默认端点。
func (p *GeminiProvider) baseURL(apiBase string) string {
	if strings.TrimSpace(apiBase) == "" {
		return geminiDefaultBase
	}
	return strings.TrimRight(apiBase, "/")
}

// endpoint 拼出 :generateContent 或 :streamGenerateContent 的完整地址。
func (p *GeminiProvider) endpoint(apiBase, model, action string) string {
	return p.baseURL(apiBase) + "/models/" + geminiModelName(model) + ":" + action
}

// geminiAuthHeaders 注入 Gemini 鉴权头，并删除 Authorization：
// Gemini 同时收到 x-goog-api-key 与 Authorization 会拒绝请求。
func geminiAuthHeaders(apiKey string) func(http.Header) {
	return func(h http.Header) {
		if apiKey != "" {
			h.Set("x-goog-api-key", apiKey)
		}
		h.Del("Authorization")
	}
}

// ChatCompletion 发送非流式请求并把上游响应转成 chat 形状。
func (p *GeminiProvider) ChatCompletion(
	ctx context.Context,
	apiKey, apiBase, model string,
	reqBody []byte,
	customHeaders map[string]string,
) (*CompletionResult, int, error) {
	body, rejected := p.prepare(model, reqBody)
	if rejected != nil {
		// 调用方请求不合法：按上游非 2xx 的既有约定回传状态码与响应体。
		return &CompletionResult{Body: rejected.body}, rejected.status, nil
	}
	httpReq, err := newUpstreamRequest(ctx, p.endpoint(apiBase, model, "generateContent"),
		body, geminiAuthHeaders(apiKey), customHeaders)
	if err != nil {
		return nil, 0, err
	}
	resp, err := doUpstream(httpReq, geminiUpstreamTimeout)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBytes, err := ReadUpstreamBody(resp.Body)
	if err != nil {
		return nil, 0, richErrorf(err, "failed to read upstream response")
	}
	if resp.StatusCode != http.StatusOK {
		return &CompletionResult{Body: respBytes}, resp.StatusCode, nil
	}

	oaiBody, usage, err := geminiToOpenAIForModel(respBytes, model)
	if err != nil {
		// 上游 2xx 但响应体无法解析：原样回传给调用方，不凭空造结果。
		return &CompletionResult{Body: respBytes}, resp.StatusCode, nil
	}
	return &CompletionResult{Body: oaiBody, Usage: usage}, http.StatusOK, nil
}

// ChatCompletionStream 发送流式请求并把上游 SSE 转成 chat 分片。
func (p *GeminiProvider) ChatCompletionStream(
	ctx context.Context,
	apiKey, apiBase, model string,
	reqBody []byte,
	w http.ResponseWriter,
	flusher http.Flusher,
	customHeaders map[string]string,
) (*StreamResult, int, error) {
	body, rejected := p.prepare(model, reqBody)
	if rejected != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rejected.status)
		_, _ = w.Write(rejected.body)
		return nil, rejected.status, nil
	}
	url := p.endpoint(apiBase, model, "streamGenerateContent") + "?alt=sse"
	httpReq, err := newUpstreamRequest(ctx, url, body, geminiAuthHeaders(apiKey), customHeaders)
	if err != nil {
		return nil, 0, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	resp, err := doUpstream(httpReq, geminiStreamTimeout)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		forwardUpstreamError(w, resp)
		return nil, resp.StatusCode, nil
	}

	setSSEHeaders(w)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), geminiSSEMaxLineBytes)

	translator := newGeminiStreamTranslator(w, flusher)
	for scanner.Scan() {
		// SSE 的冒号后空格是可选的（规范允许 data:xxx），两种都要认。
		line := scanner.Text()
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		if err := translator.handleFrame(data); err != nil {
			status := http.StatusBadGateway
			var streamErr *geminiStreamError
			if errors.As(err, &streamErr) {
				status = streamErr.statusCode()
			}
			return nil, status, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, richErrorf(err, "failed to read upstream stream")
	}
	// Gemini 的 SSE 不发 [DONE]，由网关补一个，保持与 OpenAI 流一致。
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
	return &StreamResult{Usage: translator.usage}, http.StatusOK, nil
}

// CheckConnectivityWithChat 用一条最小对话请求探测上游可用性。
// 只凭 2xx 不够：要求解析出非空回复，否则按 upstream_client 归类。
func (p *GeminiProvider) CheckConnectivityWithChat(
	ctx context.Context,
	apiKey, apiBase, model string,
) (time.Duration, ProbeOutcome, error) {
	body, err := json.Marshal(map[string]any{
		"contents": []any{map[string]any{
			"role":  "user",
			"parts": []any{map[string]any{"text": probePrompt}},
		}},
		"generationConfig": map[string]any{"maxOutputTokens": probeMaxTokens},
	})
	if err != nil {
		return 0, ProbeOutcome{}, &ConnectivityError{Kind: ErrNetworkUnreachable, Cause: err}
	}

	httpReq, err := newProbeRequest(ctx, p.endpoint(apiBase, model, "generateContent"), body,
		geminiAuthHeaders(apiKey))
	if err != nil {
		return 0, ProbeOutcome{}, err
	}
	return runConnectivityProbe(httpReq, geminiProbeReply)
}

// geminiProbeReply 从 generateContent 响应里拼出文本；空回复视为未真正调用模型。
func geminiProbeReply(body []byte) (ProbeOutcome, error) {
	var parsed geminiResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ProbeOutcome{}, errors.New("response is not a generateContent payload")
	}
	if len(parsed.Candidates) == 0 || parsed.Candidates[0].Content == nil {
		return ProbeOutcome{}, errors.New("response has no text content")
	}
	var parts []string
	var thoughts []string
	for _, part := range parsed.Candidates[0].Content.Parts {
		if part.FunctionCall != nil || part.Text == "" {
			continue
		}
		text := strings.TrimSpace(part.Text)
		if part.Thought {
			thoughts = append(thoughts, text)
			continue
		}
		parts = append(parts, text)
	}
	reply := strings.TrimSpace(strings.Join(parts, " "))
	if reply == "" {
		// 2.5 Pro 等"总是思考"的模型可能把全部探测预算花在 thought 上：
		// 只有思考内容也说明模型真的跑了，按可用处理（与 anthropic 探测同口径）。
		reply = strings.TrimSpace(strings.Join(thoughts, " "))
	}
	if reply == "" {
		return ProbeOutcome{}, errors.New("response has no text content")
	}
	return ProbeOutcome{Reply: truncateReply(reply), UpstreamID: parsed.ResponseID}, nil
}
