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
	"context"
	"net/http"
	"time"
)

// GeminiProvider 适配 Google Generative Language API（generativelanguage.googleapis.com）。
type GeminiProvider struct{}

var _ Provider = (*GeminiProvider)(nil)

// ChatCompletion 发送非流式请求并把上游响应转成 chat 形状。
func (p *GeminiProvider) ChatCompletion(
	ctx context.Context,
	apiKey, apiBase, model string,
	reqBody []byte,
	customHeaders map[string]string,
) (*CompletionResult, int, error) {
	return nil, 0, richErrorf(nil, "gemini provider not implemented")
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
	return nil, 0, richErrorf(nil, "gemini provider not implemented")
}

// CheckConnectivityWithChat 用一条最小对话请求探测上游可用性。
func (p *GeminiProvider) CheckConnectivityWithChat(
	ctx context.Context,
	apiKey, apiBase, model string,
) (time.Duration, ProbeOutcome, error) {
	return 0, ProbeOutcome{}, richErrorf(nil, "gemini provider not implemented")
}
