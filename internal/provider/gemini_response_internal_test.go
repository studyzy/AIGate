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
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestGeminiToOpenAI_TextAndUsage(t *testing.T) {
	body, usage, err := geminiToOpenAI([]byte(`{
		"responseId":"r1",
		"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,
			"thoughtsTokenCount":3,"totalTokenCount":15}}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("输出非法 JSON: %v", err)
	}
	choice := got["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["content"] != "hello" {
		t.Fatalf("content = %v", msg["content"])
	}
	if got["id"] != "r1" {
		t.Fatalf("id = %v", got["id"])
	}
	// thoughtsTokenCount 在 candidatesTokenCount 之外，必须加上。
	if usage.PromptTokens != 10 || usage.CompletionTokens != 8 || usage.TotalTokens != 15 {
		t.Fatalf("usage = %+v, want prompt=10 completion=8 total=15", usage)
	}
}

func TestGeminiToOpenAI_ThoughtParts(t *testing.T) {
	body, _, err := geminiToOpenAI([]byte(`{
		"candidates":[{"content":{"parts":[
			{"text":"thinking...","thought":true},{"text":"answer"}]},"finishReason":"STOP"}]}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "answer" {
		t.Fatalf("content = %v, want answer", msg["content"])
	}
	if msg["reasoning_content"] != "thinking..." {
		t.Fatalf("reasoning_content = %v", msg["reasoning_content"])
	}
}

// 签名必须编进 tool_call.id 才能穿过 OpenAI 格式回到客户端。
func TestGeminiToOpenAI_FunctionCallCarriesSignature(t *testing.T) {
	body, _, err := geminiToOpenAI([]byte(`{
		"candidates":[{"content":{"parts":[
			{"functionCall":{"name":"w","args":{"city":"A"},"id":"call_1"},
			 "thoughtSignature":"AQID"}]},"finishReason":"STOP"}]}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls := msg["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %v", calls)
	}
	call := calls[0].(map[string]any)
	id := call["id"].(string)
	if !strings.Contains(id, "_ts_") {
		t.Fatalf("id 应含签名后缀，got %q", id)
	}
	base, sig := splitGeminiCallID(id)
	if base != "call_1" {
		t.Fatalf("base = %q, want call_1", base)
	}
	if !bytes.Equal(sig, []byte{0x01, 0x02, 0x03}) {
		t.Fatalf("sig = %x", sig)
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "w" {
		t.Fatalf("name = %v", fn["name"])
	}
	// arguments 必须是 JSON 字符串而不是对象。
	if _, isStr := fn["arguments"].(string); !isStr {
		t.Fatalf("arguments 必须是字符串，got %T", fn["arguments"])
	}
}

func TestGeminiFinishReasonMapping(t *testing.T) {
	cases := map[string]string{
		"STOP":                    "stop",
		"MAX_TOKENS":              "length",
		"CONTINUATION":            "length",
		"SAFETY":                  "content_filter",
		"RECITATION":              "content_filter",
		"PROHIBITED_CONTENT":      "content_filter",
		"BLOCKLIST":               "content_filter",
		"MALFORMED_FUNCTION_CALL": "stop",
		"TOO_MANY_TOOL_CALLS":     "stop",
		"SOMETHING_NEW":           "stop", // 未在表中的值回落 stop
		"":                        "stop",
	}
	for in, want := range cases {
		if got := geminiMapFinishReason(in); got != want {
			t.Fatalf("geminiMapFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

// promptFeedback.blockReason 表示输入被拦截，应报 content_filter 而不是空响应。
func TestGeminiToOpenAI_PromptBlocked(t *testing.T) {
	body, _, err := geminiToOpenAI([]byte(`{
		"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":5,"totalTokenCount":5}}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	choice := got["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "content_filter" {
		t.Fatalf("finish_reason = %v, want content_filter", choice["finish_reason"])
	}
}

func TestGeminiToOpenAI_CachedTokens(t *testing.T) {
	_, usage, err := geminiToOpenAI([]byte(`{
		"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":10,
			"cachedContentTokenCount":60,"totalTokenCount":110}}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if usage.PromptCacheReadTokens != 60 {
		t.Fatalf("PromptCacheReadTokens = %d, want 60", usage.PromptCacheReadTokens)
	}
	if usage.PromptCacheWriteTokens != 0 {
		t.Fatalf("Gemini 没有显式写缓存计数，PromptCacheWriteTokens 应为 0")
	}
}

// 没有候选也没有 blockReason 时不能 panic，回到空 content 的 stop。
func TestGeminiToOpenAI_EmptyCandidates(t *testing.T) {
	body, _, err := geminiToOpenAI([]byte(`{"usageMetadata":{"promptTokenCount":1}}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("输出非法 JSON: %v", err)
	}
	if len(got["choices"].([]any)) != 1 {
		t.Fatalf("应至少产出一个 choice，got %v", got["choices"])
	}
}
