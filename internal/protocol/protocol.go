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

// Package protocol 定义上游协议类型的共享常量。
// model（持久化与校验）与 provider（协议实现分发）都从这里取值，
// 保证单一事实源、避免字面量在多处漂移。
package protocol

// 上游协议类型取值（对应 ai_models.model_type 列）。
const (
	// TypeOpenAICompletions 是 OpenAI 兼容端点（URL 含 /v1，转发时拼 /chat/completions）。
	TypeOpenAICompletions = "openai-completions"
	// TypeAnthropicMessages 是 Anthropic Messages 协议（URL 为 host，转发时拼 /v1/messages）。
	TypeAnthropicMessages = "anthropic-messages"
	// TypeOpenAIResponses 是 OpenAI Responses 协议（URL 含 /v1，转发时拼 /responses）。
	TypeOpenAIResponses = "openai-responses"
	// TypeOpenAIDecisions 是 OpenAI Decisions 决策协议（URL 含 /v1，转发时拼 /decisions）。
	TypeOpenAIDecisions = "openai-decisions"
	// TypeSystemOne 是 TypeSafe System One 决策协议（URL 含 /v1，转发时拼 /systemone）。
	TypeSystemOne = "systemone"
	// TypeGeminiGenerateContent 是 Google 生成式语言 API（URL 含 /v1beta，
	// 转发时拼 /models/{model}:generateContent；流式用 :streamGenerateContent?alt=sse）。
	TypeGeminiGenerateContent = "gemini-generate-content"
)

// DecisionFormat 标识上游决策协议说哪种线上格式。
// 入口格式与它相同则语义不变（仅规范化），不同则由网关按映射表转换。
type DecisionFormat string

const (
	// FormatOpenAIDecisions 对应 openai-decisions：input + questions 数组。
	FormatOpenAIDecisions DecisionFormat = "openai-decisions"
	// FormatSystemOne 对应 systemone：state + questions 对象。
	FormatSystemOne DecisionFormat = "systemone"
)
