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

//go:build e2e

package e2e

// WebUI 用例：/ui/ 静态托管的契约（首页、根路径跳转、前端路由回退、缺失资源）。

import (
	"net/http"
	"strings"
	"testing"
)

// TestE2E_WebUI_ServesSPA 校验 /ui/ 静态托管：首页 200、根路径 302、前端路由回退、缺失资源 404。
func TestE2E_WebUI_ServesSPA(t *testing.T) {
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	gw := startGateway(t, bin, dsn)

	status, body, err := tryGet(gw.baseURL + "/ui/")
	if err != nil {
		t.Fatalf("GET /ui/: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("GET /ui/ status = %d, want 200", status)
	}
	// 未构建前端时 /ui/ 返回运行时提示页，构建过则是 SPA 外壳；两者都应为 HTML。
	// 这里不绑定具体 DOM，避免 e2e 依赖仓库里是否已经产出 dist。
	if !strings.Contains(body, "<html") {
		t.Fatalf("GET /ui/ body is not an html page: %s", body)
	}

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noRedirect.Get(gw.baseURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET / status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/" {
		t.Fatalf("GET / Location = %q, want /ui/", loc)
	}

	status, _, err = tryGet(gw.baseURL + "/ui/keys")
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET /ui/keys status = %d, err = %v, want 200 fallback", status, err)
	}

	status, _, err = tryGet(gw.baseURL + "/ui/assets/missing-abc123.js")
	if err != nil || status != http.StatusNotFound {
		t.Fatalf("GET /ui/assets/missing-abc123.js status = %d, err = %v, want 404", status, err)
	}
}
