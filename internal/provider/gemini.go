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
	body, err := buildGeminiRequest(model, reqBody)
	if err != nil {
		return nil, 0, err
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

	oaiBody, usage, err := geminiToOpenAI(respBytes)
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
	body, err := buildGeminiRequest(model, reqBody)
	if err != nil {
		return nil, 0, err
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
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		if err := translator.handleFrame(data); err != nil {
			return nil, 0, err
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
	for _, part := range parsed.Candidates[0].Content.Parts {
		if part.FunctionCall != nil || part.Thought {
			continue
		}
		if text := strings.TrimSpace(part.Text); text != "" {
			parts = append(parts, text)
		}
	}
	reply := strings.TrimSpace(strings.Join(parts, " "))
	if reply == "" {
		return ProbeOutcome{}, errors.New("response has no text content")
	}
	return ProbeOutcome{Reply: truncateReply(reply), UpstreamID: parsed.ResponseID}, nil
}
