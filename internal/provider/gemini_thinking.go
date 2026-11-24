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

// 本文件把 OpenAI 的 reasoning_effort 映射为 Gemini 的 thinkingConfig。
//
// 不能照搬 anthropic.go 的预算常量（128/1024/2048/4096）：
//   - 2.5 系列各有自己的预算区间，flash-lite 下限是 512，发 128 会 400；
//   - 3.x 移除了 thinkingBudget，改收 thinkingLevel，发 thinkingBudget 会 400。
//
// 见 design §4.3.1。

import "strings"

// geminiBudgetRanges 是公开的思考预算区间；最长前缀优先
// （"gemini-2.5-flash" 也是 "gemini-2.5-flash-lite" 的前缀）。
var geminiBudgetRanges = []struct {
	prefix   string
	min, max int
}{
	{"gemini-2.5-flash-lite", 512, 24576},
	{"gemini-2.5-pro", 128, 32768},
	{"gemini-2.5-flash", 0, 24576},
}

// geminiEffortBudgets 是 effort → 预算的基准值，随后按模型区间钳制。
var geminiEffortBudgets = map[string]int{
	"minimal": 1,
	"low":     1024,
	"medium":  2048,
	"high":    4096,
}

// geminiBudgetRange 返回模型的思考预算区间；不是 2.5 系列时 ok 为 false。
func geminiBudgetRange(model string) (int, int, bool) {
	lower := strings.ToLower(model)
	for _, r := range geminiBudgetRanges {
		if strings.Contains(lower, r.prefix) {
			return r.min, r.max, true
		}
	}
	return 0, 0, false
}

// isGemini3Plus 判断模型是否 3.x 及以上（用 thinkingLevel 而不是 thinkingBudget）。
func isGemini3Plus(model string) bool {
	lower := strings.ToLower(model)
	i := strings.Index(lower, "gemini-")
	if i < 0 {
		return false
	}
	rest := lower[i+len("gemini-"):]
	d := 0
	for d < len(rest) && rest[d] >= '0' && rest[d] <= '9' {
		d++
	}
	if d == 0 {
		return false
	}
	return rest[0] >= '3'
}

// geminiThinkingConfig 返回 generationConfig.thinkingConfig；nil 表示不发该字段。
func geminiThinkingConfig(model, effort string, maxOutputTokens int) map[string]any {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if _, ok := geminiEffortBudgets[effort]; !ok {
		return nil
	}

	if isGemini3Plus(model) {
		return map[string]any{
			"thinkingLevel":   effort,
			"includeThoughts": true,
		}
	}

	minB, maxB, ok := geminiBudgetRange(model)
	if !ok {
		return nil
	}
	budget := geminiEffortBudgets[effort]
	if budget < minB {
		budget = minB
	}
	if budget > maxB {
		budget = maxB
	}
	// Gemini 要求输出预算严格大于思考预算。maxOutputTokens<=0 表示
	// 上游没有其它限制可用，此时任何预算都无从保证合法，直接不发。
	if maxOutputTokens <= 0 || budget >= maxOutputTokens {
		budget = maxOutputTokens - 1
	}
	if budget < 1 {
		// 容不下任何预算：整个 config 不发，而不是发一个非法值。
		return nil
	}
	return map[string]any{
		"thinkingBudget":  budget,
		"includeThoughts": true,
	}
}
