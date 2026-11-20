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

package provider_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aigate/internal/provider"
)

// 说明：本文件覆盖 Responses 上游适配器（chat 入口 × responses 上游，即 design §5.4 矩阵第 3 格）。
// SSRF 防护恒开，httptest mock 上游落在回环地址，故用例先 allowPrivateForTest 放行私网。

// TestResponsesProvider_ChatCompletion 断言上游 Responses 响应被转换为 chat 形状，
// 且用量按 Responses 字段口径（input_tokens / output_tokens）提取。
func TestResponsesProvider_ChatCompletion(t *testing.T) {
	allowPrivateForTest(t)

	const upstreamBody = `{"id":"resp_abc","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`

	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		gotBody = sb.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer srv.Close()

	p := &provider.OpenAIResponsesProvider{}
	result, status, err := p.ChatCompletion(context.Background(), "sk-test", srv.URL+"/v1", "gpt-5",
		[]byte(`{"model":"gpt-5","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if result == nil || result.Usage == nil {
		t.Fatalf("result = %#v, want body + usage", result)
	}

	if gotPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q, want Bearer sk-test", gotAuth)
	}
	var sent map[string]interface{}
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	if _, ok := sent["messages"]; ok {
		t.Fatalf("upstream body 仍含 chat 字段 messages: %s", gotBody)
	}
	if store, ok := sent["store"].(bool); !ok || store {
		t.Fatalf("upstream body store = %v, want false", sent["store"])
	}

	var chat struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(result.Body, &chat); err != nil {
		t.Fatalf("result body is not JSON: %v (%s)", err, result.Body)
	}
	if len(chat.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(chat.Choices))
	}
	if chat.Choices[0].Message.Content != "hi" {
		t.Fatalf("content = %q, want hi", chat.Choices[0].Message.Content)
	}
	if chat.Choices[0].Message.Role != "assistant" {
		t.Fatalf("role = %q, want assistant", chat.Choices[0].Message.Role)
	}
	if chat.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", chat.Choices[0].FinishReason)
	}
	if result.Usage.PromptTokens != 10 {
		t.Fatalf("PromptTokens = %d, want 10（input_tokens）", result.Usage.PromptTokens)
	}
	if result.Usage.CompletionTokens != 5 {
		t.Fatalf("CompletionTokens = %d, want 5（output_tokens）", result.Usage.CompletionTokens)
	}
	if result.Usage.TotalTokens != 15 {
		t.Fatalf("TotalTokens = %d, want 15", result.Usage.TotalTokens)
	}
}

// TestResponsesProvider_ChatCompletion_UpstreamErrorPassthrough 覆盖非 200：status 与 body 原样返回、不解析。
func TestResponsesProvider_ChatCompletion_UpstreamErrorPassthrough(t *testing.T) {
	allowPrivateForTest(t)

	const upstreamBody = `{"error":{"message":"model not found"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer srv.Close()

	p := &provider.OpenAIResponsesProvider{}
	result, status, err := p.ChatCompletion(context.Background(), "sk", srv.URL+"/v1", "gpt-5",
		[]byte(`{"messages":[{"role":"user","content":"hi"}]}`), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	if result == nil || string(result.Body) != upstreamBody {
		t.Fatalf("body = %q, want 上游错误体原样返回 %q", result.Body, upstreamBody)
	}
	if result.Usage != nil {
		t.Fatalf("usage = %+v, want nil（错误响应不计算用量）", result.Usage)
	}
}

// TestResponsesProvider_ChatCompletion_RejectsMalformedRequestBody 覆盖入站请求体非法。
func TestResponsesProvider_ChatCompletion_RejectsMalformedRequestBody(t *testing.T) {
	p := &provider.OpenAIResponsesProvider{}
	result, status, err := p.ChatCompletion(context.Background(), "sk", "https://api.example.com/v1", "gpt-5", []byte(`{`), nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if result != nil || status != 0 {
		t.Fatalf("result = %#v status = %d, want nil/0", result, status)
	}
	if !strings.Contains(err.Error(), "failed to decode request body") {
		t.Fatalf("err = %v, want a decode failure", err)
	}
}

// TestResponsesProvider_ChatCompletionStream 断言 Responses SSE 被转换为 chat chunk，并以 data: [DONE] 收尾。
func TestResponsesProvider_ChatCompletionStream(t *testing.T) {
	allowPrivateForTest(t)

	const sse = "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_abc","status":"in_progress"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"he"}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"llo"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_abc","status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}` + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	rec := httptest.NewRecorder()
	p := &provider.OpenAIResponsesProvider{}
	result, status, err := p.ChatCompletionStream(context.Background(), "sk", srv.URL+"/v1", "gpt-5",
		[]byte(`{"messages":[{"role":"user","content":"hi"}]}`), rec, rec, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if result == nil || result.Usage == nil {
		t.Fatalf("result = %#v, want usage", result)
	}
	if result.Usage.PromptTokens != 10 || result.Usage.CompletionTokens != 5 {
		t.Fatalf("usage = %+v, want prompt 10 / completion 5", result.Usage)
	}

	body := rec.Body.String()
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("stream 未以 data: [DONE] 收尾:\n%s", body)
	}
	if strings.Contains(body, "response.output_text.delta") {
		t.Fatalf("stream 仍夹带 Responses 专有事件名:\n%s", body)
	}

	var content strings.Builder
	var sawUsage bool
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("chunk 不是 JSON: %v (%s)", err, line)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Fatalf("chunk object = %q, want chat.completion.chunk", chunk.Object)
		}
		if len(chunk.Choices) > 0 {
			content.WriteString(chunk.Choices[0].Delta.Content)
		}
		if chunk.Usage != nil {
			sawUsage = true
			if chunk.Usage.PromptTokens != 10 || chunk.Usage.CompletionTokens != 5 {
				t.Fatalf("chunk usage = %+v, want prompt 10 / completion 5", chunk.Usage)
			}
		}
	}
	if content.String() != "hello" {
		t.Fatalf("拼接后的 content = %q, want hello", content.String())
	}
	if !sawUsage {
		t.Fatalf("缺少带 usage 的收尾 chunk:\n%s", body)
	}
}

// TestResponsesProvider_ChatCompletionStream_UpstreamErrorPassthrough 覆盖流式命中上游非 200。
func TestResponsesProvider_ChatCompletionStream_UpstreamErrorPassthrough(t *testing.T) {
	allowPrivateForTest(t)

	const upstreamBody = `{"error":{"message":"invalid api key"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer srv.Close()

	rec := httptest.NewRecorder()
	p := &provider.OpenAIResponsesProvider{}
	result, status, err := p.ChatCompletionStream(context.Background(), "sk", srv.URL+"/v1", "gpt-5",
		[]byte(`{"messages":[{"role":"user","content":"hi"}]}`), rec, rec, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if result != nil {
		t.Fatalf("result = %#v, want nil", result)
	}
	if rec.Code != http.StatusUnauthorized || strings.TrimSpace(rec.Body.String()) != upstreamBody {
		t.Fatalf("client got %d %q, want 401 %q", rec.Code, rec.Body.String(), upstreamBody)
	}
}

// TestResponsesProvider_CheckConnectivityWithChat 断言探测请求形状：POST <base>/responses +
// 最小 input + max_output_tokens=1 + store:false。
func TestResponsesProvider_CheckConnectivityWithChat(t *testing.T) {
	allowPrivateForTest(t)

	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		gotBody = sb.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_probe","model":"gpt-5-2026-01-01","status":"completed",` +
			`"output":[{"type":"message","role":"assistant",` +
			`"content":[{"type":"output_text","text":"hello"}]}]}`))
	}))
	defer srv.Close()

	p := &provider.OpenAIResponsesProvider{}
	latency, out, err := p.CheckConnectivityWithChat(context.Background(), "sk-probe", srv.URL+"/v1", "gpt-5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if latency <= 0 {
		t.Fatalf("latency = %v, want > 0", latency)
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("probe path = %q, want /v1/responses", gotPath)
	}

	var sent map[string]interface{}
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("probe body is not JSON: %v (%s)", err, gotBody)
	}
	if sent["input"] != "Reply with the single word: hello" {
		t.Fatalf("probe input = %v, want the shared probe prompt", sent["input"])
	}
	if v, ok := sent["max_output_tokens"].(float64); !ok || v != 8 {
		t.Fatalf("probe max_output_tokens = %v, want 8", sent["max_output_tokens"])
	}
	if store, ok := sent["store"].(bool); !ok || store {
		t.Fatalf("probe store = %v, want false", sent["store"])
	}
	if sent["model"] != "gpt-5" {
		t.Fatalf("probe model = %v, want gpt-5", sent["model"])
	}
	if out.Reply != "hello" {
		t.Fatalf("probe reply = %q, want hello", out.Reply)
	}
	if out.UpstreamID != "resp_probe" {
		t.Fatalf("probe upstream id = %q, want resp_probe", out.UpstreamID)
	}
	if out.UpstreamModel != "gpt-5-2026-01-01" {
		t.Fatalf("probe upstream model = %q, want gpt-5-2026-01-01", out.UpstreamModel)
	}
}
