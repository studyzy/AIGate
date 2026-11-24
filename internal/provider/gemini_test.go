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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aigate/internal/provider"
)

type geminiUpstream struct {
	path    string
	query   string
	headers http.Header
	body    []byte
}

func startGeminiUpstream(t *testing.T, status int, reply string) (*httptest.Server, *geminiUpstream) {
	t.Helper()
	provider.SetAllowPrivateUpstreams(true)
	t.Cleanup(func() { provider.SetAllowPrivateUpstreams(false) })
	rec := &geminiUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path = r.URL.Path
		rec.query = r.URL.RawQuery
		rec.headers = r.Header.Clone()
		rec.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

const geminiOK = `{"responseId":"r1","candidates":[{"content":{"role":"model",` +
	`"parts":[{"text":"hello"}]},"finishReason":"STOP"}],` +
	`"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`

func TestGeminiProvider_PathAndAuth(t *testing.T) {
	srv, rec := startGeminiUpstream(t, http.StatusOK, geminiOK)
	p := &provider.GeminiProvider{}
	res, status, err := p.ChatCompletion(context.Background(), "sk-g", srv.URL+"/v1beta",
		"models/gemini-2.5-flash", []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		map[string]string{"X-Custom": "1"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusOK || res == nil {
		t.Fatalf("status=%d res=%v", status, res)
	}
	// models/ 前缀必须被去掉，且 model 落在路径里而不是请求体。
	if rec.path != "/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("path = %q", rec.path)
	}
	if got := rec.headers.Get("x-goog-api-key"); got != "sk-g" {
		t.Fatalf("x-goog-api-key = %q", got)
	}
	// 必须删掉 Authorization：Gemini 同时收到两种凭据会拒绝。
	if got := rec.headers.Get("Authorization"); got != "" {
		t.Fatalf("Authorization 应被删除，got %q", got)
	}
	if got := rec.headers.Get("X-Custom"); got != "1" {
		t.Fatalf("custom header = %q", got)
	}
	var sent map[string]any
	_ = json.Unmarshal(rec.body, &sent)
	if _, has := sent["model"]; has {
		t.Fatalf("请求体不应含 model: %v", sent)
	}
}

func TestGeminiProvider_StreamPath(t *testing.T) {
	srv, rec := startGeminiUpstream(t, http.StatusOK,
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\n")
	p := &provider.GeminiProvider{}
	rec2 := httptest.NewRecorder()
	_, status, err := p.ChatCompletionStream(context.Background(), "sk", srv.URL+"/v1beta",
		"gemini-2.5-flash", []byte(`{"messages":[{"role":"user","content":"hi"}]}`), rec2, rec2, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if rec.path != "/v1beta/models/gemini-2.5-flash:streamGenerateContent" {
		t.Fatalf("path = %q", rec.path)
	}
	if rec.query != "alt=sse" {
		t.Fatalf("query = %q, want alt=sse", rec.query)
	}
}

// 上游非 2xx 时原样透传状态码与响应体（与非流式路径一致）。
func TestGeminiProvider_NonOKPassthrough(t *testing.T) {
	const upstreamErr = `{"error":{"code":400,"message":"bad","status":"INVALID_ARGUMENT"}}`
	srv, _ := startGeminiUpstream(t, http.StatusBadRequest, upstreamErr)
	p := &provider.GeminiProvider{}
	res, status, err := p.ChatCompletion(context.Background(), "sk", srv.URL+"/v1beta",
		"gemini-2.5-flash", []byte(`{"messages":[{"role":"user","content":"x"}]}`), nil)
	if err != nil {
		t.Fatalf("上游 4xx 不应是 error，got %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d", status)
	}
	if string(res.Body) != upstreamErr {
		t.Fatalf("body = %s", res.Body)
	}
}

// 请求不可用（n>1、非法图片）时在发请求前就失败。
func TestGeminiProvider_RejectsInvalidRequest(t *testing.T) {
	p := &provider.GeminiProvider{}
	_, status, err := p.ChatCompletion(context.Background(), "sk", "https://example.com/v1beta",
		"gemini-2.5-flash", []byte(`{"messages":[{"role":"user","content":"x"}],"n":2}`), nil)
	if err == nil {
		t.Fatalf("n=2 应报错")
	}
	if status != 0 {
		t.Fatalf("status = %d, want 0", status)
	}
}

// 流内出现 {"error":{...}} 必须返回错误，不能当空帧吞掉（Review Focus #5）。
func TestGeminiProvider_StreamInStreamError(t *testing.T) {
	srv, _ := startGeminiUpstream(t, http.StatusOK,
		"data: {\"error\":{\"code\":429,\"message\":\"RESOURCE_EXHAUSTED\",\"status\":\"RESOURCE_EXHAUSTED\"}}\n\n")
	p := &provider.GeminiProvider{}
	rec := httptest.NewRecorder()
	_, _, err := p.ChatCompletionStream(context.Background(), "sk", srv.URL+"/v1beta",
		"gemini-2.5-flash", []byte(`{"messages":[{"role":"user","content":"hi"}]}`), rec, rec, nil)
	if err == nil {
		t.Fatalf("流内 error 帧应返回错误")
	}
}

// 正常流结束后网关自己补 [DONE]（Gemini 不发它）。
func TestGeminiProvider_StreamAppendsDone(t *testing.T) {
	srv, _ := startGeminiUpstream(t, http.StatusOK,
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"STOP\"}]}\n\n")
	p := &provider.GeminiProvider{}
	rec := httptest.NewRecorder()
	_, _, err := p.ChatCompletionStream(context.Background(), "sk", srv.URL+"/v1beta",
		"gemini-2.5-flash", []byte(`{"messages":[{"role":"user","content":"hi"}]}`), rec, rec, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("流末尾应补 [DONE]，got %q", rec.Body.String())
	}
}
