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

// 本文件实现 OpenAI Decisions 上游适配器（apiBase 含 /v1，请求 /decisions）。
// 契约是「IR 进、IR 出」，跨协议转换全部在 decision_convert.go。

import (
	"context"
	"net/http"

	"aigate/internal/protocol"
)

// decisionsPath 是 OpenAI Decisions 上游路径（apiBase 已含 /v1）。
const decisionsPath = "/decisions"

// OpenAIDecisionsProvider 适配 OpenAI Decisions（/v1/decisions）。
type OpenAIDecisionsProvider struct{}

var _ DecisionProvider = (*OpenAIDecisionsProvider)(nil)

// Format 返回该上游说哪种线上格式。
func (p *OpenAIDecisionsProvider) Format() protocol.DecisionFormat {
	return protocol.FormatOpenAIDecisions
}

// Decide 把 IR 编成 OpenAI Decisions 请求体并发送。
func (p *OpenAIDecisionsProvider) Decide(
	ctx context.Context,
	apiKey, apiBase, model string,
	req *DecisionRequest,
	customHeaders map[string]string,
) (*DecisionResult, error) {
	prepareDecisionRequest(req, model)
	body, err := EncodeOpenAIDecisionsRequest(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := newUpstreamRequest(ctx, decisionBaseURL(apiBase)+decisionsPath, body, func(h http.Header) {
		h.Set("Authorization", "Bearer "+apiKey)
	}, customHeaders)
	if err != nil {
		return nil, err
	}
	resp, err := doUpstream(httpReq, decisionUpstreamTimeout)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := ReadUpstreamBody(resp.Body)
	if err != nil {
		return nil, richErrorf(err, "failed to read upstream response")
	}
	result := &DecisionResult{Body: respBody, StatusCode: resp.StatusCode}
	if resp.StatusCode != http.StatusOK {
		return result, nil
	}
	parsed, err := ParseOpenAIDecisionsResponse(respBody, req)
	if err != nil {
		// 上游 200 但形状不认识：交回原始 body，由入口层决定；不伪装成功。
		return result, richErrorf(err, "invalid upstream response")
	}
	result.Resp = parsed
	return result, nil
}

// readBodySnippetOf 截取响应体前 maxBytes 字节，用于错误摘要（单行化由调用方保证）。
func readBodySnippetOf(body []byte, maxBytes int) string {
	if len(body) <= maxBytes {
		return string(body)
	}
	return string(body[:maxBytes])
}

// prepareDecisionRequest 把请求体的 model 替换成上游模型 ID（入口层已解析）。
func prepareDecisionRequest(req *DecisionRequest, model string) {
	if model != "" {
		req.Model = model
	}
}
