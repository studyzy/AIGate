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

package protocol_test

import (
	"testing"

	"aigate/internal/protocol"
)

// 决策协议的 model_type 字面量是对外契约（落库、导入、文档三处必须一致）。
func TestDecisionTypeConstants(t *testing.T) {
	if protocol.TypeOpenAIDecisions != "openai-decisions" {
		t.Fatalf("TypeOpenAIDecisions = %q", protocol.TypeOpenAIDecisions)
	}
	if protocol.TypeSystemOne != "systemone" {
		t.Fatalf("TypeSystemOne = %q", protocol.TypeSystemOne)
	}
}

// TestGeminiTypeConstant 钉死 gemini-generate-content 的字面量：
// 它同时出现在 ai_models.model_type 落库值、管理面校验与文档三处。
func TestGeminiTypeConstant(t *testing.T) {
	if protocol.TypeGeminiGenerateContent != "gemini-generate-content" {
		t.Fatalf("TypeGeminiGenerateContent = %q", protocol.TypeGeminiGenerateContent)
	}
	if len(protocol.TypeGeminiGenerateContent) > 32 {
		t.Fatalf("model_type 超出 ai_models.model_type 的 VARCHAR(32): %d", len(protocol.TypeGeminiGenerateContent))
	}
}
