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

// 本文件覆盖 System One 适配器的线上行为与决策模型连通性探测。

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"aigate/internal/protocol"
	"aigate/internal/provider"
)

const systemOneOK = `{"model":"jev-1.14.0","answers":{"q":{"type":"choice","choice":"a",` +
	`"probabilities":{"a":0.9,"b":0.1},"confidence":0.8}},"usage":{"input_tokens":12,"output_tokens":3}}`

func TestSystemOneProvider_PathAndAuth(t *testing.T) {
	srv, rec := startDecisionUpstream(t, http.StatusOK, systemOneOK)
	p := &provider.SystemOneProvider{}
	res, err := p.Decide(context.Background(), "ts-key", srv.URL+"/v1", "jev-latest", probeRequest(), nil)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if res.StatusCode != http.StatusOK || res.Resp == nil {
		t.Fatalf("result = %+v", res)
	}
	if rec.path != "/v1/systemone" {
		t.Fatalf("path = %q, want /v1/systemone", rec.path)
	}
	if got := rec.headers.Get("Authorization"); got != "Bearer ts-key" {
		t.Fatalf("authorization = %q", got)
	}
	var sent struct {
		State     string          `json:"state"`
		Questions json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal(rec.body, &sent); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if sent.State == "" || !strings.Contains(string(sent.Questions), `"criteria":{"a":"","b":""}`) {
		t.Fatalf("upstream body = %s", rec.body)
	}
}

func TestSystemOneProvider_NonOK(t *testing.T) {
	const refused = `{"code":"decision_refused","message":"refused"}`
	srv, _ := startDecisionUpstream(t, http.StatusUnprocessableEntity, refused)
	p := &provider.SystemOneProvider{}
	res, err := p.Decide(context.Background(), "k", srv.URL+"/v1", "m", probeRequest(), nil)
	if err != nil {
		t.Fatalf("non-200 must not be an error: %v", err)
	}
	if res.StatusCode != http.StatusUnprocessableEntity || string(res.Body) != refused {
		t.Fatalf("result = %d %s", res.StatusCode, res.Body)
	}
}

// 探测必须发一道二元问句，并从 noul / predicate 的回答里取出概率。
func TestProbeDecision_SystemOne(t *testing.T) {
	const reply = `{"model":"jev-1.14.0","answers":{"reachable":{"type":"noul","noul":0.97}},` +
		`"usage":{"input_tokens":20,"output_tokens":1}}`
	srv, rec := startDecisionUpstream(t, http.StatusOK, reply)
	latency, yes, err := provider.ProbeDecision(context.Background(), protocol.TypeSystemOne,
		"k", srv.URL+"/v1", "jev-latest")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if yes != 0.97 {
		t.Fatalf("yes = %v", yes)
	}
	if latency <= 0 {
		t.Fatalf("latency = %v", latency)
	}
	if rec.path != "/v1/systemone" {
		t.Fatalf("path = %q", rec.path)
	}
	if !strings.Contains(string(rec.body), `"type":"noul"`) {
		t.Fatalf("probe must ask a noul question: %s", rec.body)
	}
}

func TestProbeDecision_OpenAI(t *testing.T) {
	const reply = `{"model":"gpt-6-luna","answers":[{"type":"predicate","name":"reachable","probability":0.9}],` +
		`"usage":{"input_tokens":20,"output_tokens":0,"total_tokens":20}}`
	srv, rec := startDecisionUpstream(t, http.StatusOK, reply)
	_, yes, err := provider.ProbeDecision(context.Background(), protocol.TypeOpenAIDecisions,
		"k", srv.URL+"/v1", "gpt-6-luna")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if yes != 0.9 {
		t.Fatalf("yes = %v", yes)
	}
	if rec.path != "/v1/decisions" || !strings.Contains(string(rec.body), `"type":"predicate"`) {
		t.Fatalf("probe path/body = %s %s", rec.path, rec.body)
	}
}

func TestProbeDecision_UnknownFormatFails(t *testing.T) {
	_, _, err := provider.ProbeDecision(context.Background(), "openai-completions", "k", "https://h/v1", "m")
	if err == nil {
		t.Fatal("non-decision model type must fail the probe")
	}
}
