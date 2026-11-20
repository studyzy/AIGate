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
	"net/http/httptest"
	"strings"
	"testing"
)

// TestExtractResponsesUsage 断言用量按 Responses 字段口径提取：
// input_tokens → PromptTokens、output_tokens → CompletionTokens，
// reasoning_tokens 是 output 的子集、不得重复累加。
func TestExtractResponsesUsage(t *testing.T) {
	data := `{"usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150,` +
		`"input_tokens_details":{"cached_tokens":4,"cache_write_tokens":6},` +
		`"output_tokens_details":{"reasoning_tokens":3}}}`

	u := extractResponsesUsage(data)
	if u == nil {
		t.Fatalf("usage = nil, want 解析结果")
	}
	if u.PromptTokens != 100 {
		t.Fatalf("PromptTokens = %d, want 100", u.PromptTokens)
	}
	if u.CompletionTokens != 50 {
		t.Fatalf("CompletionTokens = %d, want 50", u.CompletionTokens)
	}
	if u.TotalTokens != 150 {
		t.Fatalf("TotalTokens = %d, want 150（reasoning_tokens 不得重复累加）", u.TotalTokens)
	}
	if u.PromptCacheReadTokens != 4 {
		t.Fatalf("PromptCacheReadTokens = %d, want 4", u.PromptCacheReadTokens)
	}
	if u.PromptCacheWriteTokens != 6 {
		t.Fatalf("PromptCacheWriteTokens = %d, want 6", u.PromptCacheWriteTokens)
	}

	// 不含 usage 的事件返回 nil（O-1：prev 参数已移除，沿用逻辑由调用方负责）。
	if got := extractResponsesUsage(`{"type":"response.created"}`); got != nil {
		t.Fatalf("无 usage 时应返回 nil，得到 %+v", got)
	}
	// 嵌套在 response.usage 里的用量也要能取到（response.completed 的形态）。
	nested := extractResponsesUsage(`{"type":"response.completed","response":{"usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}}`)
	if nested == nil || nested.PromptTokens != 7 || nested.CompletionTokens != 2 {
		t.Fatalf("嵌套 usage = %+v, want prompt 7 / completion 2", nested)
	}
}

// TestBuildResponsesRequest 断言 chat 请求体被转换为 Responses 形状。
func TestBuildResponsesRequest(t *testing.T) {
	body, err := buildResponsesRequest("x", []byte(`{"model":"x","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if req["model"] != "x" {
		t.Fatalf("model = %v, want x", req["model"])
	}
	if v, ok := req["max_output_tokens"].(float64); !ok || v != 100 {
		t.Fatalf("max_output_tokens = %v, want 100", req["max_output_tokens"])
	}
	if _, ok := req["max_tokens"]; ok {
		t.Fatalf("输出不应包含 max_tokens: %s", body)
	}
	if _, ok := req["input"]; !ok {
		t.Fatalf("输出应包含 input: %s", body)
	}
	if store, ok := req["store"].(bool); !ok || store {
		t.Fatalf("store = %v, want false", req["store"])
	}
	if _, ok := req["messages"]; ok {
		t.Fatalf("输出不应包含 messages: %s", body)
	}
}

// TestBuildResponsesRequest_ForceStoreFalse 断言即使入站带 store:true 也不得透传。
func TestBuildResponsesRequest_ForceStoreFalse(t *testing.T) {
	body, err := buildResponsesRequest("x", []byte(`{"model":"x","store":true,"messages":[{"role":"user","content":"hi"}]}`), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if store, ok := req["store"].(bool); !ok || store {
		t.Fatalf("store = %v, want false（必须强制关闭）", req["store"])
	}
}

// TestBuildResponsesRequest_SystemToInstructions 断言 system 消息提升为 instructions，
// tool 结果转 function_call_output。
func TestBuildResponsesRequest_SystemToInstructions(t *testing.T) {
	in := `{"messages":[{"role":"system","content":"be nice"},{"role":"user","content":"hi"}],` +
		`"max_completion_tokens":7,"response_format":{"type":"json_object"}}`
	body, err := buildResponsesRequest("x", []byte(in), true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if req["instructions"] != "be nice" {
		t.Fatalf("instructions = %v, want be nice", req["instructions"])
	}
	if v, ok := req["max_output_tokens"].(float64); !ok || v != 7 {
		t.Fatalf("max_output_tokens = %v, want 7（来自 max_completion_tokens）", req["max_output_tokens"])
	}
	text, _ := req["text"].(map[string]interface{})
	if text == nil {
		t.Fatalf("text = %v, want {format:...}", req["text"])
	}
	format, _ := text["format"].(map[string]interface{})
	if format == nil || format["type"] != "json_object" {
		t.Fatalf("text.format = %v, want response_format 映射", text["format"])
	}
	if req["stream"] != true {
		t.Fatalf("stream = %v, want true", req["stream"])
	}
	if _, ok := req["stream_options"]; ok {
		t.Fatalf("Responses 不需要 stream_options: %s", body)
	}
	items, _ := req["input"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("input items = %v, want 只剩 1 条非 system 消息", req["input"])
	}
}

// TestResponsesToChatResponse_ToolCalls 断言 function_call 输出项转为 chat tool_calls。
func TestResponsesToChatResponse_ToolCalls(t *testing.T) {
	body := []byte(`{"id":"resp_abc","object":"response","status":"completed","output":[` +
		`{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"SZ\"}"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`)

	out, err := responsesToChatResponse(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var chat struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatalf("output is not JSON: %v (%s)", err, out)
	}
	if len(chat.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(chat.Choices))
	}
	if len(chat.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v, want 1 条", chat.Choices[0].Message.ToolCalls)
	}
	tc := chat.Choices[0].Message.ToolCalls[0]
	if tc.Function.Name != "get_weather" {
		t.Fatalf("tool name = %q, want get_weather", tc.Function.Name)
	}
	if tc.ID != "call_1" || tc.Type != "function" {
		t.Fatalf("tool call = %+v, want id=call_1 type=function", tc)
	}
	if tc.Function.Arguments != `{"city":"SZ"}` {
		t.Fatalf("arguments = %q", tc.Function.Arguments)
	}
	if chat.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", chat.Choices[0].FinishReason)
	}
}

// TestResponsesToChatResponse_FinishReasons 断言 status / incomplete_details 决定 finish_reason。
func TestResponsesToChatResponse_FinishReasons(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "incomplete max_output_tokens",
			body: `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},` +
				`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`,
			want: "length",
		},
		{
			name: "completed text",
			body: `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`,
			want: "stop",
		},
		{
			name: "incomplete content_filter",
			body: `{"status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[]}`,
			want: "content_filter",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := responsesToChatResponse([]byte(tc.body))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var chat struct {
				Choices []struct {
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(out, &chat); err != nil {
				t.Fatalf("output is not JSON: %v (%s)", err, out)
			}
			if got := chat.Choices[0].FinishReason; got != tc.want {
				t.Fatalf("finish_reason = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResponsesStreamToChat 断言 Responses SSE 事件被译为 chat chunk 并以 [DONE] 收尾。
func TestResponsesStreamToChat(t *testing.T) {
	const sse = "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_abc"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"he"}` + "\n\n" +
		`data: {"type":"response.output_text.delta","delta":"llo"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}` + "\n\n"

	rec := httptest.NewRecorder()
	usage, err := responsesStreamToChat(rec, rec, strings.NewReader(sse))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 5 {
		t.Fatalf("usage = %+v, want prompt 10 / completion 5", usage)
	}
	got := rec.Body.String()
	if !strings.HasSuffix(strings.TrimSpace(got), "data: [DONE]") {
		t.Fatalf("未以 [DONE] 收尾:\n%s", got)
	}
	if !strings.Contains(got, `"object":"chat.completion.chunk"`) {
		t.Fatalf("chunk 不是 chat.completion.chunk:\n%s", got)
	}
}
