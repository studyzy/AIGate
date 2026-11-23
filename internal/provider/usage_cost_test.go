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
	"encoding/json"
	"testing"
)

func TestExtractOpenAIUsage_ParsesReportedCost(t *testing.T) {
	body := `{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"cost":0.00176}}`
	got := extractOpenAIUsage(body, nil)
	if got == nil || got.ReportedCost == nil || *got.ReportedCost != 0.00176 {
		t.Fatalf("usage = %+v", got)
	}
}

func TestExtractOpenAIUsage_NoCostLeavesNil(t *testing.T) {
	body := `{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	got := extractOpenAIUsage(body, nil)
	if got == nil || got.ReportedCost != nil {
		t.Fatalf("usage = %+v, ReportedCost must stay nil", got)
	}
}

func TestExtractOpenAIUsage_ZeroCostIsNil(t *testing.T) {
	body := `{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"cost":0}}`
	got := extractOpenAIUsage(body, nil)
	if got == nil || got.ReportedCost != nil {
		t.Fatalf("usage = %+v, zero cost must not count as reported", got)
	}
}

func TestAnthropicUsage_CarriesReportedCost(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":10,"output_tokens":5,"cost":0.02}}`)
	var ar anthropicResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := anthropicUsage(&ar)
	if got == nil || got.ReportedCost == nil || *got.ReportedCost != 0.02 {
		t.Fatalf("usage = %+v", got)
	}
}

// TestExtractOpenAIUsage_StreamCostKeptWhenChunkOmitsIt：流式分片无 cost 时必须保留
// 此前已解析到的自报成本（与 token 字段同口径），有 cost 时覆盖。
func TestExtractOpenAIUsage_StreamCostKeptWhenChunkOmitsIt(t *testing.T) {
	withCost := `{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"cost":0.00176}}`
	plain := `{"choices":[]}`

	got := extractOpenAIUsage(withCost, nil)
	if got == nil || got.ReportedCost == nil || *got.ReportedCost != 0.00176 {
		t.Fatalf("first chunk usage = %+v", got)
	}
	// 无 usage 的分片沿用 prev。
	got = extractOpenAIUsage(plain, got)
	if got == nil || got.ReportedCost == nil || *got.ReportedCost != 0.00176 {
		t.Fatalf("plain chunk must keep cost: %+v", got)
	}
	// 有 usage 但无 cost 的分片也必须保留。
	got = extractOpenAIUsage(`{"usage":{"prompt_tokens":10,"completion_tokens":6,"total_tokens":16}}`, got)
	if got == nil || got.ReportedCost == nil || *got.ReportedCost != 0.00176 {
		t.Fatalf("cost-less usage chunk must keep cost: %+v", got)
	}
	// 有 cost 的分片覆盖。
	got = extractOpenAIUsage(`{"usage":{"prompt_tokens":10,"completion_tokens":7,"total_tokens":17,"cost":0.002}}`, got)
	if got == nil || got.ReportedCost == nil || *got.ReportedCost != 0.002 {
		t.Fatalf("chunk with cost must overwrite: %+v", got)
	}
}

// TestResponsesUsage_CarriesReportedCost：Responses 协议的 usage.cost 同样映射到 ReportedCost。
func TestResponsesUsage_CarriesReportedCost(t *testing.T) {
	body := `{"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"cost":0.03}}`
	got := extractResponsesUsage(body)
	if got == nil || got.ReportedCost == nil || *got.ReportedCost != 0.03 {
		t.Fatalf("usage = %+v", got)
	}

	zero := `{"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"cost":0}}`
	if got := extractResponsesUsage(zero); got == nil || got.ReportedCost != nil {
		t.Fatalf("usage = %+v, zero cost must not count as reported", got)
	}
}

// TestParseDecisionUsage_CarriesReportedCost：决策协议上游若带 usage.cost 也要保留。
func TestParseDecisionUsage_CarriesReportedCost(t *testing.T) {
	body := []byte(`{"input_tokens":10,"output_tokens":5,"cost":0.04}`)
	got := parseDecisionUsage(body)
	if got == nil || got.ReportedCost == nil || *got.ReportedCost != 0.04 {
		t.Fatalf("usage = %+v", got)
	}

	plain := parseDecisionUsage([]byte(`{"input_tokens":10,"output_tokens":5}`))
	if plain == nil || plain.ReportedCost != nil {
		t.Fatalf("usage = %+v, ReportedCost must stay nil", plain)
	}
}
