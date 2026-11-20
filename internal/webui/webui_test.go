// Tencent is pleased to support the open source community by making Tencent Cloud AIGate available.
//
// Copyright (C) 2026 Tencent.
// All rights reserved.
//
// If you have downloaded a copy of the Tencent Cloud AIGate source code from Tencent,
// please note that Tencent Cloud AIGate source code is licensed under the Apache 2.0 License,
// A copy of the Apache 2.0 License is included in this file.
package webui_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aigate/internal/webui"
)

// serve 用给定路径请求 Handler 并返回响应。
func serve(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	webui.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestHandlerServesIndex(t *testing.T) {
	rec := serve(t, "/ui/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("cache-control = %q, want no-cache", cc)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "<html") {
		t.Fatalf("body does not look like an html page")
	}
}

func TestHandlerFallsBackForClientRoutes(t *testing.T) {
	for _, target := range []string{"/ui/keys", "/ui/models/42/edit"} {
		rec := serve(t, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 fallback", target, rec.Code)
		}
	}
}

func TestHandlerReturns404ForMissingAssets(t *testing.T) {
	rec := serve(t, "/ui/assets/missing-abc123.js")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandlerRejectsPathTraversal(t *testing.T) {
	rec := serve(t, "/ui/../../go.mod")
	if rec.Code == http.StatusOK {
		body, _ := io.ReadAll(rec.Body)
		if strings.Contains(string(body), "module aigate") {
			t.Fatalf("served a file outside dist")
		}
	}
}
