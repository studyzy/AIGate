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
	"errors"
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

// 请求不可用（n>1）时在发请求前就失败，且按网关既有约定返回 400 响应体
// 而不是 502——调用方需要知道是自己的请求有问题。
func TestGeminiProvider_RejectsInvalidRequest(t *testing.T) {
	p := &provider.GeminiProvider{}
	res, status, err := p.ChatCompletion(context.Background(), "sk", "https://example.com/v1beta",
		"gemini-2.5-flash", []byte(`{"messages":[{"role":"user","content":"x"}],"n":2}`), nil)
	if err != nil {
		t.Fatalf("无效请求不应走 error 分支（那会被映射成 502），got %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if res == nil || !strings.Contains(string(res.Body), "single candidate") {
		t.Fatalf("响应体应说明只支持单候选，got %v", res)
	}
}

// 流内出现 {"error":{...}} 必须返回错误，不能当空帧吞掉（Review Focus #5）。
// 状态码必须非 0，否则 proxy 不会记账，上游已产生的 token 就被漏掉了。
func TestGeminiProvider_StreamInStreamError(t *testing.T) {
	srv, _ := startGeminiUpstream(t, http.StatusOK,
		"data: {\"error\":{\"code\":429,\"message\":\"RESOURCE_EXHAUSTED\",\"status\":\"RESOURCE_EXHAUSTED\"}}\n\n")
	p := &provider.GeminiProvider{}
	rec := httptest.NewRecorder()
	_, status, err := p.ChatCompletionStream(context.Background(), "sk", srv.URL+"/v1beta",
		"gemini-2.5-flash", []byte(`{"messages":[{"role":"user","content":"hi"}]}`), rec, rec, nil)
	if err == nil {
		t.Fatalf("流内 error 帧应返回错误")
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429（必须带状态码，否则 proxy 不记账）", status)
	}
}

// 错误帧不带 code 时回落 502，而不是 0。
func TestGeminiProvider_StreamInStreamErrorWithoutCode(t *testing.T) {
	srv, _ := startGeminiUpstream(t, http.StatusOK,
		"data: {\"error\":{\"message\":\"boom\"}}\n\n")
	p := &provider.GeminiProvider{}
	rec := httptest.NewRecorder()
	_, status, err := p.ChatCompletionStream(context.Background(), "sk", srv.URL+"/v1beta",
		"gemini-2.5-flash", []byte(`{"messages":[{"role":"user","content":"hi"}]}`), rec, rec, nil)
	if err == nil {
		t.Fatalf("流内 error 帧应返回错误")
	}
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
}

// SSE 的冒号后空格是可选语法，上游不写空格时不能把整条流都跳过。
func TestGeminiProvider_StreamAcceptsDataWithoutSpace(t *testing.T) {
	srv, _ := startGeminiUpstream(t, http.StatusOK,
		"data:{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"STOP\"}]}\n\n")
	p := &provider.GeminiProvider{}
	rec := httptest.NewRecorder()
	_, _, err := p.ChatCompletionStream(context.Background(), "sk", srv.URL+"/v1beta",
		"gemini-2.5-flash", []byte(`{"messages":[{"role":"user","content":"hi"}]}`), rec, rec, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(rec.Body.String(), `"content":"hi"`) {
		t.Fatalf("data: 无空格时帧不应被丢弃，got %q", rec.Body.String())
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

func TestGeminiProvider_ProbeShape(t *testing.T) {
	srv, rec := startGeminiUpstream(t, http.StatusOK,
		`{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`)
	p := &provider.GeminiProvider{}
	latency, out, err := p.CheckConnectivityWithChat(context.Background(), "sk", srv.URL+"/v1beta", "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if latency <= 0 || out.Reply != "hello" {
		t.Fatalf("latency=%v reply=%q", latency, out.Reply)
	}
	var sent map[string]any
	_ = json.Unmarshal(rec.body, &sent)
	gc := sent["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != float64(8) {
		t.Fatalf("探测应限制输出，maxOutputTokens = %v", gc["maxOutputTokens"])
	}
}

// 2.5 Pro 这类"总是思考"的模型可能把全部探测预算花在 thought 上：
// 只有一个 thought part、没有可见正文，仍说明模型真的跑了，不能报成不可用。
func TestGeminiProvider_ProbeThoughtOnlyReply(t *testing.T) {
	srv, _ := startGeminiUpstream(t, http.StatusOK,
		`{"candidates":[{"content":{"parts":[{"text":"let me think","thought":true}]},`+
			`"finishReason":"MAX_TOKENS"}]}`)
	p := &provider.GeminiProvider{}
	_, out, err := p.CheckConnectivityWithChat(context.Background(), "sk", srv.URL+"/v1beta", "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("只有 thought part 也应算连通，got err = %v", err)
	}
	if out.Reply == "" {
		t.Fatalf("探测回复不应为空")
	}
}

// 探测拿到空回复说明没真正调用模型，必须归类为 upstream_client。
func TestGeminiProvider_ProbeEmptyReply(t *testing.T) {
	srv, _ := startGeminiUpstream(t, http.StatusOK, `{"candidates":[{"content":{"parts":[]}}]}`)
	p := &provider.GeminiProvider{}
	_, _, err := p.CheckConnectivityWithChat(context.Background(), "sk", srv.URL+"/v1beta", "gemini-2.5-flash")
	if err == nil {
		t.Fatalf("空回复应报错")
	}
	var ce *provider.ConnectivityError
	if !errors.As(err, &ce) || !errors.Is(ce.Kind, provider.ErrUpstreamClient) {
		t.Fatalf("err = %v, want ErrUpstreamClient", err)
	}
}
