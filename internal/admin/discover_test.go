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

package admin_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type discoverResp struct {
	OK        bool     `json:"ok"`
	Models    []string `json:"models"`
	ErrorKind string   `json:"error_kind"`
	Message   string   `json:"message"`
}

// TestDiscoverModels_WithInlineKey 覆盖用请求里的 api_key 向上游拉取模型列表。
func TestDiscoverModels_WithInlineKey(t *testing.T) {
	allowPrivateUpstreamsForTest(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-inline" {
			t.Errorf("authorization = %q, want Bearer sk-inline", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-a"},{"id":"gpt-b"}]}`))
	}))
	t.Cleanup(upstream.Close)

	srv, _, adminToken := newAdminServer(t)
	status, body := doRequest(t, srv, http.MethodPost, "/admin/models/discover", adminToken, map[string]any{
		"model_type": "openai-completions",
		"url":        upstream.URL + "/v1",
		"api_key":    "sk-inline",
	})
	assertStatus(t, status, http.StatusOK, body)
	got := mustDecode[discoverResp](t, body)
	if !got.OK {
		t.Fatalf("ok = false, kind=%q message=%q", got.ErrorKind, got.Message)
	}
	if len(got.Models) != 2 || got.Models[0] != "gpt-a" || got.Models[1] != "gpt-b" {
		t.Fatalf("models = %v, want [gpt-a gpt-b]", got.Models)
	}
}

// TestDiscoverModels_WithCredential 覆盖用已保存凭据的第一个 header 密钥拉取（Anthropic 协议）。
func TestDiscoverModels_WithCredential(t *testing.T) {
	allowPrivateUpstreamsForTest(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "sk-cred" {
			t.Errorf("x-api-key = %q, want sk-cred", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-x"}]}`))
	}))
	t.Cleanup(upstream.Close)

	srv, _, adminToken := newAdminServer(t)
	status, body := doRequest(t, srv, http.MethodPost, "/admin/credentials", adminToken, map[string]any{
		"name":    "vendor-a",
		"secrets": []map[string]string{{"key": "api_key", "value": "sk-cred", "location": "header"}},
	})
	assertStatus(t, status, http.StatusCreated, body)
	cred := mustDecode[struct {
		ID uint `json:"id"`
	}](t, body)

	status, body = doRequest(t, srv, http.MethodPost, "/admin/models/discover", adminToken, map[string]any{
		"model_type":    "anthropic-messages",
		"url":           upstream.URL,
		"credential_id": cred.ID,
	})
	assertStatus(t, status, http.StatusOK, body)
	got := mustDecode[discoverResp](t, body)
	if !got.OK || len(got.Models) != 1 || got.Models[0] != "claude-x" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

// TestDiscoverModels_UpstreamRejectsKey 覆盖上游 401 被归类为 invalid_api_key。
func TestDiscoverModels_UpstreamRejectsKey(t *testing.T) {
	allowPrivateUpstreamsForTest(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	t.Cleanup(upstream.Close)

	srv, _, adminToken := newAdminServer(t)
	status, body := doRequest(t, srv, http.MethodPost, "/admin/models/discover", adminToken, map[string]any{
		"model_type": "openai-completions",
		"url":        upstream.URL + "/v1",
		"api_key":    "sk-bad",
	})
	assertStatus(t, status, http.StatusOK, body)
	got := mustDecode[discoverResp](t, body)
	if got.OK || got.ErrorKind != "invalid_api_key" {
		t.Fatalf("ok=%v kind=%q, want ok=false kind=invalid_api_key", got.OK, got.ErrorKind)
	}
}

// TestDiscoverModels_RequiresKey 覆盖 api_key 与 credential_id 都没给时返回 400。
func TestDiscoverModels_RequiresKey(t *testing.T) {
	srv, _, adminToken := newAdminServer(t)
	status, body := doRequest(t, srv, http.MethodPost, "/admin/models/discover", adminToken, map[string]any{
		"model_type": "openai-completions",
		"url":        "http://127.0.0.1:9/v1",
	})
	assertStatus(t, status, http.StatusBadRequest, body)
}

// TestDiscoverModels_BlocksPrivateUpstream 覆盖默认 SSRF 策略下私网上游被拒。
func TestDiscoverModels_BlocksPrivateUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(upstream.Close)

	srv, _, adminToken := newAdminServer(t)
	status, body := doRequest(t, srv, http.MethodPost, "/admin/models/discover", adminToken, map[string]any{
		"model_type": "openai-completions",
		"url":        upstream.URL + "/v1",
		"api_key":    "sk-x",
	})
	assertStatus(t, status, http.StatusOK, body)
	got := mustDecode[discoverResp](t, body)
	if got.OK || got.ErrorKind != "network_unreachable" {
		t.Fatalf("ok=%v kind=%q, want ok=false kind=network_unreachable", got.OK, got.ErrorKind)
	}
}
