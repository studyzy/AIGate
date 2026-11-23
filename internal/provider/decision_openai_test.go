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

// 本文件覆盖决策适配器的线上行为：路径拼接、鉴权头、自定义头、非 200 原样透传、
// 网络失败分类，以及协议分发的白名单语义（未知类型不兜底）。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aigate/internal/protocol"
	"aigate/internal/provider"
)

// decisionUpstream 记录最近一次请求并返回预设响应。
type decisionUpstream struct {
	path    string
	headers http.Header
	body    []byte
}

func startDecisionUpstream(t *testing.T, status int, reply string) (*httptest.Server, *decisionUpstream) {
	t.Helper()
	provider.SetAllowPrivateUpstreams(true)
	t.Cleanup(func() { provider.SetAllowPrivateUpstreams(false) })
	rec := &decisionUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path = r.URL.Path
		rec.headers = r.Header.Clone()
		rec.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func probeRequest() *provider.DecisionRequest {
	return &provider.DecisionRequest{
		Model: "up-model",
		State: json.RawMessage(`"hello"`),
		Questions: []provider.DecisionQuestion{
			{Name: "q", Type: provider.DecisionPick, Instructions: "?",
				Choices: []provider.DecisionOption{{Value: "a"}, {Value: "b"}}},
		},
	}
}

const decisionsOK = `{"model":"gpt-6-luna","answers":[{"type":"choice","name":"q","choice":"a",` +
	`"probabilities":[{"value":"a","probability":0.9},{"value":"b","probability":0.1}],"confidence":0.8}],` +
	`"usage":{"input_tokens":10,"output_tokens":0,"total_tokens":10}}`

func TestOpenAIDecisionsProvider_PathAndAuth(t *testing.T) {
	srv, rec := startDecisionUpstream(t, http.StatusOK, decisionsOK)
	p := &provider.OpenAIDecisionsProvider{}
	res, err := p.Decide(context.Background(), "sk-up", srv.URL+"/v1", "up-model", probeRequest(),
		map[string]string{"X-Custom": "1"})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if res.StatusCode != http.StatusOK || res.Resp == nil {
		t.Fatalf("result = %+v", res)
	}
	if rec.path != "/v1/decisions" {
		t.Fatalf("path = %q, want /v1/decisions", rec.path)
	}
	if got := rec.headers.Get("Authorization"); got != "Bearer sk-up" {
		t.Fatalf("authorization = %q", got)
	}
	if got := rec.headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q", got)
	}
	if got := rec.headers.Get("X-Custom"); got != "1" {
		t.Fatalf("custom header = %q", got)
	}
	var sent struct {
		Model     string `json:"model"`
		Questions []struct {
			Type string `json:"type"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(rec.body, &sent); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if sent.Model != "up-model" || sent.Questions[0].Type != "choice" {
		t.Fatalf("upstream body = %s", rec.body)
	}
}

func TestOpenAIDecisionsProvider_NonOKPassthrough(t *testing.T) {
	const upstreamErr = `{"error":{"message":"bad","type":"invalid_request_error","code":"x"}}`
	srv, _ := startDecisionUpstream(t, http.StatusUnprocessableEntity, upstreamErr)
	p := &provider.OpenAIDecisionsProvider{}
	res, err := p.Decide(context.Background(), "sk", srv.URL+"/v1", "m", probeRequest(), nil)
	if err != nil {
		t.Fatalf("non-200 must not be an error, got %v", err)
	}
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if string(res.Body) != upstreamErr {
		t.Fatalf("body = %s", res.Body)
	}
}

// 网络失败必须是 error（=「请求未完成」），而不是某个 4xx/5xx 状态码。
func TestOpenAIDecisionsProvider_NetworkError(t *testing.T) {
	provider.SetAllowPrivateUpstreams(true)
	t.Cleanup(func() { provider.SetAllowPrivateUpstreams(false) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	p := &provider.OpenAIDecisionsProvider{}
	res, err := p.Decide(context.Background(), "sk", url+"/v1", "m", probeRequest(), nil)
	if err == nil || res != nil {
		t.Fatalf("res=%+v err=%v, want a transport error", res, err)
	}
	if !strings.Contains(err.Error(), "upstream request failed") {
		t.Fatalf("err = %v", err)
	}
}

// 严格模式下回环上游必须被 SSRF 校验拒绝（不发出请求）。
// 严格模式下私网字面量必须被 SSRF 校验直接拒绝，且不发任何请求。
func TestOpenAIDecisionsProvider_RejectsPrivateWithoutOptIn(t *testing.T) {
	provider.SetAllowPrivateUpstreams(false)
	p := &provider.OpenAIDecisionsProvider{}
	_, err := p.Decide(context.Background(), "sk", "http://10.1.2.3/v1", "m", probeRequest(), nil)
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("err = %v, want an SSRF rejection", err)
	}
}

func TestGetDecisionProvider(t *testing.T) {
	p, err := provider.GetDecisionProvider(protocol.TypeOpenAIDecisions)
	if err != nil || p.Format() != protocol.FormatOpenAIDecisions {
		t.Fatalf("openai decisions: p=%v err=%v", p, err)
	}
	p, err = provider.GetDecisionProvider(protocol.TypeSystemOne)
	if err != nil || p.Format() != protocol.FormatSystemOne {
		t.Fatalf("system one: p=%v err=%v", p, err)
	}
	for _, bad := range []string{"", "openai-completions", "anthropic-messages", "openai-responses", "nope"} {
		if _, err := provider.GetDecisionProvider(bad); !errors.Is(err, provider.ErrNotDecisionModel) {
			t.Fatalf("%q: err = %v, want ErrNotDecisionModel", bad, err)
		}
		if provider.IsDecisionModelType(bad) {
			t.Fatalf("%q must not be a decision model type", bad)
		}
	}
	if !provider.IsDecisionModelType(protocol.TypeSystemOne) {
		t.Fatal("systemone must be a decision model type")
	}
}

func TestProbeDecision_ReportsStatusError(t *testing.T) {
	srv, _ := startDecisionUpstream(t, http.StatusUnauthorized, `{"error":{"message":"no"}}`)
	_, _, err := provider.ProbeDecision(context.Background(), protocol.TypeSystemOne, "k", srv.URL+"/v1", "m")
	if !errors.Is(err, provider.ErrInvalidAPIKey) {
		t.Fatalf("err = %v, want ErrInvalidAPIKey", err)
	}
	if err != nil && err.Error() == "" {
		t.Fatal("probe error must carry a message")
	}
}
