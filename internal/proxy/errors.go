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

// 数据面错误文案（恒英文，不做 i18n）。
const (
	msgMissingAPIKey       = "missing API key"
	msgInvalidAPIKey       = "invalid API key"
	msgReadRequestBody     = "failed to read request body"
	msgBodyTooLarge        = "request body too large"
	msgInvalidJSON         = "invalid json body"
	msgModelNotBound       = "requested model is not bound to this key"
	msgNoResolvableModel   = "no available model for this key"
	msgBindingKeyMissing   = "the bound model requires a user-provided key, but none is configured on this binding"
	msgInternalError       = "internal server error"
	msgStreamNotSupported  = "streaming not supported by server"
	msgUpstreamConnect     = "failed to connect to LLM backend"
	msgGlobalQuotaExceeded = "Global token quota exceeded"
	msgModelQuotaExceeded  = "Model daily token quota exceeded"
	msgUserQuotaExceeded   = "User token quota exceeded"
	msgKeyQuotaExceeded    = "Key token quota exceeded"

	// 决策模型相关（/v1/decisions 与 /v1/systemone）。
	msgDecisionStreaming     = "streaming is not supported on decision models"
	msgNotDecisionModel      = "model type %s does not support decisions"
	msgDecisionOnChatRoute   = "model type %s does not support chat completions"
	msgInvalidDecisionInput  = "invalid decisions request: %s"
	msgInvalidSystemOneInput = "invalid system one request: %s"

	// auto 伪模型路由。
	msgAutoUnavailable = "model auto is not available"
)
