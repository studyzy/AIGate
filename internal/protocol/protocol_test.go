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
