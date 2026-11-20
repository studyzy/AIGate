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

package cli_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// queryRecorded 是 httptest 服务捕获的一次请求快照，额外保留 r.URL.Query()
// 以便断言 query flag 精确进入 URL，而非只核对方法与路径。
type queryRecorded struct {
	method string
	path   string
	query  url.Values
	auth   string
	body   []byte
}

// newQueryRecordingServer 把含查询参数的请求快照送入缓冲 channel，
// 用 channel 而非共享变量避免 -race 下的数据竞争；响应 Content-Type 可选。
func newQueryRecordingServer(t *testing.T, status int, respBody []byte, respContentType string) (*httptest.Server, <-chan queryRecorded) {
	t.Helper()
	ch := make(chan queryRecorded, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		select {
		case ch <- queryRecorded{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.Query(),
			auth:   r.Header.Get("Authorization"),
			body:   data,
		}:
		default:
		}
		if respContentType != "" {
			w.Header().Set("Content-Type", respContentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

// recvQueryRecorded 读取一次请求快照；超时即失败，避免 channel 无值时挂死。
func recvQueryRecorded(t *testing.T, ch <-chan queryRecorded) queryRecorded {
	t.Helper()
	select {
	case rec := <-ch:
		return rec
	case <-time.After(2 * time.Second):
		t.Fatal("超时：未捕获到任何请求")
		return queryRecorded{}
	}
}

// TestUsageCommands 逐字覆盖 §5 表格的 3 条 usage 命令（不带 query flag）。
func TestUsageCommands(t *testing.T) {
	runCommandCases(t, []commandCase{
		{
			name:       "usage summary",
			args:       []string{"usage", "summary"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/usage/summary",
		},
		{
			name:       "usage logs",
			args:       []string{"usage", "logs"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/usage/logs",
		},
		{
			name:       "usage export",
			args:       []string{"usage", "export"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/usage/export",
		},
	})
}

// TestQueryFlags 按命令拆分，逐字覆盖 query-flags.md 中本任务 4 条命令→flag 映射：
// 断言每个 flag 值精确进入 r.URL.Query()，且方法、路径、管理面令牌不变；
// 末条用例确认无 query flag 的命令不带任何查询参数。
func TestQueryFlags(t *testing.T) {
	respBody := []byte(`{"ok":true}`)
	cases := []struct {
		name      string
		args      []string
		wantPath  string
		wantQuery string
	}{
		{
			name:      "usage summary",
			args:      []string{"usage", "summary", "--group-by", "model", "--start", "2026-01-01", "--user-id", "2"},
			wantPath:  "/admin/usage/summary",
			wantQuery: "group_by=model&start=2026-01-01&user_id=2",
		},
		{
			name:      "usage logs",
			args:      []string{"usage", "logs", "--page", "3", "--page-size", "10", "--start", "2026-01-01"},
			wantPath:  "/admin/usage/logs",
			wantQuery: "page=3&page_size=10&start=2026-01-01",
		},
		{
			name:      "usage export",
			args:      []string{"usage", "export", "--format", "json"},
			wantPath:  "/admin/usage/export",
			wantQuery: "format=json",
		},
		{
			name:      "webhooks deliveries",
			args:      []string{"webhooks", "deliveries", "--id", "9", "--page", "2"},
			wantPath:  "/admin/webhooks/9/deliveries",
			wantQuery: "page=2",
		},
		{
			name:      "no query flags",
			args:      []string{"settings", "get"},
			wantPath:  "/admin/settings",
			wantQuery: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, ch := newQueryRecordingServer(t, http.StatusOK, respBody, "")
			got := runCLI(t, append([]string{"--url", srv.URL}, tc.args...)...)
			if got.code != 0 {
				t.Fatalf("code = %d, 期望 0 (stdout=%q stderr=%q)", got.code, got.stdout, got.stderr)
			}
			if got.stdout != string(respBody) {
				t.Fatalf("stdout = %q, 期望原样 %q", got.stdout, respBody)
			}
			if got.stderr != "" {
				t.Fatalf("2xx stderr 应为空，实际 %q", got.stderr)
			}
			rec := recvQueryRecorded(t, ch)
			if rec.method != http.MethodGet {
				t.Errorf("method = %q, 期望 %q", rec.method, http.MethodGet)
			}
			if rec.path != tc.wantPath {
				t.Errorf("path = %q, 期望 %q", rec.path, tc.wantPath)
			}
			if rec.query.Encode() != tc.wantQuery {
				t.Errorf("query = %q, 期望 %q", rec.query.Encode(), tc.wantQuery)
			}
			if rec.auth != adminAuth {
				t.Errorf("Authorization = %q, 期望管理面令牌 %q", rec.auth, adminAuth)
			}
		})
	}
}

// TestUsageExportCSV 覆盖 usage export --format csv：200 + text/csv 文本流
// 原样写 stdout（不被 JSON 解码），format=csv 精确进入 query。
func TestUsageExportCSV(t *testing.T) {
	csvBody := []byte("id,api_key_id,user_id,ai_model_id,total_tokens\n1,2,3,4,42\n")
	srv, ch := newQueryRecordingServer(t, http.StatusOK, csvBody, "text/csv")
	got := runCLI(t, "--url", srv.URL, "usage", "export", "--format", "csv")
	if got.code != 0 {
		t.Fatalf("code = %d, 期望 0 (stdout=%q stderr=%q)", got.code, got.stdout, got.stderr)
	}
	if got.stdout != string(csvBody) {
		t.Fatalf("stdout = %q, 期望 CSV 文本原样 %q", got.stdout, csvBody)
	}
	if got.stderr != "" {
		t.Fatalf("2xx stderr 应为空，实际 %q", got.stderr)
	}
	rec := recvQueryRecorded(t, ch)
	if rec.method != http.MethodGet {
		t.Errorf("method = %q, 期望 %q", rec.method, http.MethodGet)
	}
	if rec.path != "/admin/usage/export" {
		t.Errorf("path = %q, 期望 /admin/usage/export", rec.path)
	}
	if rec.query.Encode() != "format=csv" {
		t.Errorf("query = %q, 期望 format=csv", rec.query.Encode())
	}
	if rec.auth != adminAuth {
		t.Errorf("Authorization = %q, 期望管理面令牌 %q", rec.auth, adminAuth)
	}
}
