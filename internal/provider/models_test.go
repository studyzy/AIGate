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
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"aigate/internal/protocol"
	"aigate/internal/provider"
)

// Gemini 的列表接口响应是 models[].name 而不是 data[].id。
func TestListModels_Gemini(t *testing.T) {
	provider.SetAllowPrivateUpstreams(true)
	t.Cleanup(func() { provider.SetAllowPrivateUpstreams(false) })
	var gotPath, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[{"name":"models/gemini-2.5-flash"},{"name":"models/gemini-2.5-pro"}]}`)
	}))
	t.Cleanup(srv.Close)

	ids, err := provider.ListModels(context.Background(), protocol.TypeGeminiGenerateContent, "sk", srv.URL+"/v1beta")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if gotPath != "/v1beta/models" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotKey != "sk" {
		t.Fatalf("x-goog-api-key = %q", gotKey)
	}
	if len(ids) != 2 || ids[0] != "gemini-2.5-flash" || ids[1] != "gemini-2.5-pro" {
		t.Fatalf("ids = %v（应去掉 models/ 前缀）", ids)
	}
}
