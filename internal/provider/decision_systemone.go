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

// 本文件实现 TypeSafe System One 上游适配器（apiBase 含 /v1，请求 /systemone），
// 以及决策模型的连通性探测：发一个「yes 概率」问句，用返回的概率同时验证可达性与模型可用性。

import (
	"context"
	"net/http"
	"time"

	"aigate/internal/protocol"
)

// systemOnePath 是 System One 上游路径（apiBase 已含 /v1）。
const systemOnePath = "/systemone"

// SystemOneProvider 适配 TypeSafe System One（/v1/systemone）。
type SystemOneProvider struct{}

var _ DecisionProvider = (*SystemOneProvider)(nil)

// Format 返回该上游说哪种线上格式。
func (p *SystemOneProvider) Format() protocol.DecisionFormat {
	return protocol.FormatSystemOne
}

// Decide 把 IR 编成 System One 请求体并发送。
func (p *SystemOneProvider) Decide(
	ctx context.Context,
	apiKey, apiBase, model string,
	req *DecisionRequest,
	customHeaders map[string]string,
) (*DecisionResult, error) {
	prepareDecisionRequest(req, model)
	body, err := EncodeSystemOneRequest(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := newUpstreamRequest(ctx, decisionBaseURL(apiBase)+systemOnePath, body, func(h http.Header) {
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
	parsed, err := ParseSystemOneResponse(respBody, req)
	if err != nil {
		return result, richErrorf(err, "invalid upstream response")
	}
	result.Resp = parsed
	return result, nil
}

// probeQuestion 是探测用的单题请求：一道「是否可达」的二元问句。
const probeQuestion = "Is the service reachable?"

// ProbeDecision 探测一个决策模型：发一道二元问句，返回往返耗时与「是」的概率。
// modelType 必须是决策协议；失败返回 *ConnectivityError（与 chat 探测同口径）。
func ProbeDecision(
	ctx context.Context,
	modelType, apiKey, apiBase, model string,
) (time.Duration, float64, error) {
	p, err := GetDecisionProvider(modelType)
	if err != nil {
		return 0, 0, &ConnectivityError{Kind: ErrUpstreamClient, Cause: err}
	}
	req := &DecisionRequest{
		Model: model,
		State: []byte(`"connectivity probe"`),
		Questions: []DecisionQuestion{{
			Name: "reachable", Type: DecisionPredicate, Instructions: probeQuestion,
		}},
	}
	start := time.Now()
	result, err := p.Decide(ctx, apiKey, apiBase, model, req, nil)
	latency := time.Since(start)
	if err != nil {
		return 0, 0, &ConnectivityError{Kind: ErrNetworkUnreachable, Cause: err}
	}
	if result.StatusCode != http.StatusOK {
		return 0, 0, &ConnectivityError{
			Kind:       classifyHTTPStatus(result.StatusCode),
			StatusCode: result.StatusCode,
			Snippet:    readBodySnippetOf(result.Body, 512),
		}
	}
	if result.Resp == nil || len(result.Resp.Answers) == 0 {
		return 0, 0, &ConnectivityError{Kind: ErrUpstreamServer, StatusCode: result.StatusCode,
			Cause: errNoProbeAnswer}
	}
	if prob := result.Resp.Answers[0].Probability; prob != nil {
		return latency, *prob, nil
	}
	return latency, 0, &ConnectivityError{Kind: ErrUpstreamServer, StatusCode: result.StatusCode,
		Cause: errNoProbeAnswer}
}
