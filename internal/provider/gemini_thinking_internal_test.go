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

import "testing"

func TestGeminiThinkingConfig_25UsesBudgetPerModel(t *testing.T) {
	cases := []struct {
		model, effort string
		wantBudget    int
	}{
		{"gemini-2.5-flash", "minimal", 1},        // flash 下限是 0，但 budget 0 关闭思考，用 1
		{"gemini-2.5-flash-lite", "minimal", 512}, // flash-lite 下限 512
		{"gemini-2.5-pro", "minimal", 128},        // pro 下限 128
		{"gemini-2.5-flash", "low", 1024},
		{"gemini-2.5-flash", "medium", 2048},
		{"gemini-2.5-flash", "high", 4096},
	}
	for _, c := range cases {
		cfg := geminiThinkingConfig(c.model, c.effort, 8192)
		if cfg == nil {
			t.Fatalf("%s/%s: config = nil", c.model, c.effort)
		}
		if got := cfg["thinkingBudget"]; got != c.wantBudget {
			t.Fatalf("%s/%s: thinkingBudget = %v, want %d", c.model, c.effort, got, c.wantBudget)
		}
		if _, has := cfg["thinkingLevel"]; has {
			t.Fatalf("%s: 2.5 系列不应发 thinkingLevel", c.model)
		}
		if cfg["includeThoughts"] != true {
			t.Fatalf("%s: includeThoughts 必须为 true（否则签名拿不到）", c.model)
		}
	}
}

// Gemini 3.x 移除了 thinkingBudget，改收 thinkingLevel；发 budget 会 400。
func TestGeminiThinkingConfig_3xUsesLevel(t *testing.T) {
	for _, effort := range []string{"minimal", "low", "medium", "high"} {
		cfg := geminiThinkingConfig("gemini-3-pro-preview", effort, 8192)
		if cfg == nil {
			t.Fatalf("effort=%s: config = nil", effort)
		}
		if _, has := cfg["thinkingBudget"]; has {
			t.Fatalf("effort=%s: 3.x 不应发 thinkingBudget", effort)
		}
		if cfg["thinkingLevel"] != effort {
			t.Fatalf("effort=%s: thinkingLevel = %v", effort, cfg["thinkingLevel"])
		}
		if cfg["includeThoughts"] != true {
			t.Fatalf("effort=%s: includeThoughts 必须为 true", effort)
		}
	}
}

// 模型名认不出时宁可不发 thinkingConfig，也不猜一个可能越界的预算。
func TestGeminiThinkingConfig_UnknownModelOmitsConfig(t *testing.T) {
	for _, model := range []string{"", "gpt-4o", "gemini", "gemini-flash"} {
		if cfg := geminiThinkingConfig(model, "high", 8192); cfg != nil {
			t.Fatalf("model=%q: 期望 nil，got %v", model, cfg)
		}
	}
}

// 无法识别的 effort 不发 thinkingConfig（与 anthropic 侧 mapReasoningEffort 一致）。
func TestGeminiThinkingConfig_UnknownEffortOmitsConfig(t *testing.T) {
	for _, effort := range []string{"", "none", "turbo"} {
		if cfg := geminiThinkingConfig("gemini-2.5-flash", effort, 8192); cfg != nil {
			t.Fatalf("effort=%q: 期望 nil，got %v", effort, cfg)
		}
	}
}

// Gemini 要求输出预算严格大于思考预算；越界时把预算钳到 maxOutputTokens-1。
func TestGeminiThinkingConfig_ClampsBudgetBelowMaxOutput(t *testing.T) {
	cfg := geminiThinkingConfig("gemini-2.5-flash", "high", 2048) // high=4096 > 2048
	if cfg == nil {
		t.Fatalf("config = nil")
	}
	if got := cfg["thinkingBudget"]; got != 2047 {
		t.Fatalf("thinkingBudget = %v, want 2047", got)
	}
}

// maxOutputTokens 小到容不下任何预算时，整个 thinkingConfig 不发，而不是发一个非法预算。
func TestGeminiThinkingConfig_DropsWhenMaxOutputTooSmall(t *testing.T) {
	for _, budget := range []int{0, 1} {
		if cfg := geminiThinkingConfig("gemini-2.5-flash-lite", "minimal", budget); cfg != nil {
			t.Fatalf("maxOutputTokens=%d: 期望 nil，got %v", budget, cfg)
		}
	}
}

func TestGeminiBudgetRange(t *testing.T) {
	if min, max, ok := geminiBudgetRange("gemini-2.5-flash-lite"); !ok || min != 512 || max != 24576 {
		t.Fatalf("flash-lite = %d..%d ok=%v", min, max, ok)
	}
	if min, max, ok := geminiBudgetRange("gemini-2.5-pro"); !ok || min != 128 || max != 32768 {
		t.Fatalf("pro = %d..%d ok=%v", min, max, ok)
	}
	if min, max, ok := geminiBudgetRange("gemini-2.5-flash"); !ok || min != 0 || max != 24576 {
		t.Fatalf("flash = %d..%d ok=%v", min, max, ok)
	}
	// 最长前缀优先：flash-lite 不能被 flash 抢走。
	if _, _, ok := geminiBudgetRange("gemini-2.5-flash-lite-preview"); !ok {
		t.Fatalf("flash-lite-preview 应命中 flash-lite 区间")
	}
	if _, _, ok := geminiBudgetRange("gemini-3-pro-preview"); ok {
		t.Fatalf("3.x 不应有 budget 区间")
	}
}

// 钳制不能把预算压到模型下限之下：flash-lite 下限 512，maxOutputTokens=500
// 钳成 499 后仍非法，应整个不发 thinkingConfig 而不是发一个上游会 400 的值。
func TestGeminiThinkingConfig_DoesNotClampBelowModelFloor(t *testing.T) {
	for _, effort := range []string{"minimal", "low", "medium", "high"} {
		if cfg := geminiThinkingConfig("gemini-2.5-flash-lite", effort, 500); cfg != nil {
			t.Fatalf("effort=%s maxOutputTokens=500：低于模型下限应不发，got %v", effort, cfg)
		}
	}
	// 边界：maxOutputTokens 刚好容得下下限时仍应发出下限值。
	cfg := geminiThinkingConfig("gemini-2.5-flash-lite", "minimal", 513)
	if cfg == nil || cfg["thinkingBudget"] != 512 {
		t.Fatalf("maxOutputTokens=513 应发下限 512，got %v", cfg)
	}
}
