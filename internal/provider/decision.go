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

// 本文件定义决策模型的上游适配器契约，与 chat 向 Provider 并列：
//   - chat 契约「恒吐 OpenAI-chat 形状」，决策契约「IR 进、IR 出」，不做流式；
//   - 协议分发按 model_type 白名单，未知类型一律报错，**不**像 GetProvider 那样兜底到 chat
//     （决策请求被当成 chat 发给上游会静默错发，代价高于一次 400）。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"aigate/internal/protocol"
)

// ErrNotDecisionModel 表示 model_type 不是决策协议。
var ErrNotDecisionModel = errors.New("not a decision model type")

// decisionUpstreamTimeout 是决策请求的上游超时。决策模型不生成文本，通常在一秒内返回；
// 留 2 分钟给大 state（64k 上下文）与上游排队。
const decisionUpstreamTimeout = 2 * time.Minute

// DecisionResult 是一次决策调用的结果。
//   - StatusCode == 200 时 Resp 非空，可直接翻回入口格式；
//   - 非 200 时 Body 是上游原始错误体，网关原样回传给客户端；
//   - Body 在两条分支上都保留，便于审计与日志。
type DecisionResult struct {
	Body       []byte
	Resp       *DecisionResponse
	StatusCode int
}

// DecisionProvider 是决策模型的上游适配器。
type DecisionProvider interface {
	// Format 返回该上游说哪种线上格式。
	Format() protocol.DecisionFormat
	// Decide 发送一次决策请求。返回的 error 只表示「请求未完成」（网络/SSRF/解码失败）；
	// 上游 4xx/5xx 通过 DecisionResult.StatusCode 表达。
	Decide(
		ctx context.Context,
		apiKey, apiBase, model string,
		req *DecisionRequest,
		customHeaders map[string]string,
	) (*DecisionResult, error)
}

// IsDecisionModelType 报告 model_type 是否是决策协议（白名单）。
func IsDecisionModelType(modelType string) bool {
	return modelType == protocol.TypeOpenAIDecisions || modelType == protocol.TypeSystemOne
}

// GetDecisionProvider 按 model_type 选决策适配器；非决策类型返回 ErrNotDecisionModel。
func GetDecisionProvider(modelType string) (DecisionProvider, error) {
	switch modelType {
	case protocol.TypeOpenAIDecisions:
		return &OpenAIDecisionsProvider{}, nil
	case protocol.TypeSystemOne:
		return &SystemOneProvider{}, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrNotDecisionModel, modelType)
	}
}

// decisionPath 返回按 model_type 拼接的上游路径（apiBase 已含 /v1）。
func decisionBaseURL(apiBase string) string {
	for len(apiBase) > 0 && apiBase[len(apiBase)-1] == '/' {
		apiBase = apiBase[:len(apiBase)-1]
	}
	return apiBase
}

// errNoProbeAnswer 表示探测响应里没有可用的概率答案。
var errNoProbeAnswer = errors.New("probe response carries no answer")
