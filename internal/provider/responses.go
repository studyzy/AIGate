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

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// errResponsesNotImplemented 是 Responses 适配器占位实现的哨兵错误。
// 协议常量与分发已就绪，真正的方法体由后续任务实现。
var errResponsesNotImplemented = errors.New("not implemented")

// OpenAIResponsesProvider 适配 OpenAI Responses API（/responses）。
type OpenAIResponsesProvider struct{}

var _ Provider = (*OpenAIResponsesProvider)(nil)

// ChatCompletion 发送非流式请求。占位实现：尚未接入 Responses 协议。
func (p *OpenAIResponsesProvider) ChatCompletion(
	ctx context.Context,
	apiKey, apiBase, model string,
	reqBody []byte,
	customHeaders map[string]string,
) (result *CompletionResult, statusCode int, err error) {
	return nil, 0, errResponsesNotImplemented
}

// ChatCompletionStream 发送流式请求。占位实现：尚未接入 Responses 协议。
func (p *OpenAIResponsesProvider) ChatCompletionStream(
	ctx context.Context,
	apiKey, apiBase, model string,
	reqBody []byte,
	w http.ResponseWriter,
	flusher http.Flusher,
	customHeaders map[string]string,
) (result *StreamResult, statusCode int, err error) {
	return nil, 0, errResponsesNotImplemented
}

// CheckConnectivityWithChat 探测上游连通性。占位实现：尚未接入 Responses 协议。
func (p *OpenAIResponsesProvider) CheckConnectivityWithChat(
	ctx context.Context, apiKey, apiBase, model string,
) (latency time.Duration, err error) {
	return 0, errResponsesNotImplemented
}
