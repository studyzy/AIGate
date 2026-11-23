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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ratesDataBody 是 create 用例经 --data 传入并期望逐字节送达的请求体。
var ratesDataBody = []byte(`{"from_currency":"USD","to_currency":"CNY","effective_at":"2026-11-01T00:00:00Z","rate_micros":7100000}`)

// TestRatesCommands 覆盖 rates 资源 3 条命令：list 的 --from / --to 进入 query，
// create 的 --data 作为 POST 请求体，delete 的 --id 是汇率 id。
func TestRatesCommands(t *testing.T) {
	respBody := []byte(`{"from_currency":"USD","to_currency":"CNY","rates":[]}`)
	cases := []struct {
		name       string
		args       []string
		wantMethod string
		wantPath   string
		wantQuery  string
		wantBody   []byte
	}{
		{
			name:       "rates list",
			args:       []string{"rates", "list"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/currency-rates",
		},
		{
			name:       "rates list --from --to",
			args:       []string{"rates", "list", "--from", "USD", "--to", "CNY"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/currency-rates",
			wantQuery:  "from=USD&to=CNY",
		},
		{
			name:       "rates create --data",
			args:       []string{"rates", "create", "--data", string(ratesDataBody)},
			wantMethod: http.MethodPost,
			wantPath:   "/admin/currency-rates",
			wantBody:   ratesDataBody,
		},
		{
			name:       "rates delete --id 5",
			args:       []string{"rates", "delete", "--id", "5"},
			wantMethod: http.MethodDelete,
			wantPath:   "/admin/currency-rates/5",
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
			if rec.method != tc.wantMethod {
				t.Errorf("method = %q, 期望 %q", rec.method, tc.wantMethod)
			}
			if rec.path != tc.wantPath {
				t.Errorf("path = %q, 期望 %q", rec.path, tc.wantPath)
			}
			if rec.query.Encode() != tc.wantQuery {
				t.Errorf("query = %q, 期望 %q", rec.query.Encode(), tc.wantQuery)
			}
			if len(tc.wantBody) > 0 && string(rec.body) != string(tc.wantBody) {
				t.Errorf("body = %q, 期望 %q", rec.body, tc.wantBody)
			}
			if rec.auth != adminAuth {
				t.Errorf("Authorization = %q, 期望管理面令牌 %q", rec.auth, adminAuth)
			}
		})
	}
}

// TestRatesCreateForwardsDataFile 覆盖 --data @文件 原样转发：请求体逐字等于文件
// 内容（不重新序列化），Content-Type 为 application/json。
func TestRatesCreateForwardsDataFile(t *testing.T) {
	body := []byte("{\"from_currency\": \"USD\",  \"rate_micros\": 7100000}")
	path := filepath.Join(t.TempDir(), "rate.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("写临时文件失败: %v", err)
	}
	srv, ch := newQueryRecordingServer(t, http.StatusOK, []byte(`{"id":1}`), "")
	got := runCLI(t, "--url", srv.URL, "rates", "create", "--data", "@"+path)
	if got.code != 0 {
		t.Fatalf("code = %d, 期望 0 (stdout=%q stderr=%q)", got.code, got.stdout, got.stderr)
	}
	rec := recvQueryRecorded(t, ch)
	if rec.method != http.MethodPost || rec.path != "/admin/currency-rates" {
		t.Fatalf("method/path = %q %q, 期望 POST /admin/currency-rates", rec.method, rec.path)
	}
	if string(rec.body) != string(body) {
		t.Fatalf("线上请求体 = %q, 期望逐字节 %q", rec.body, body)
	}
}

// TestHelpListsNewResources 覆盖顶层 --help 与资源级 --help 列出新增资源及其命令。
func TestHelpListsNewResources(t *testing.T) {
	got := runCLI(t, "--help")
	if got.code != 0 {
		t.Fatalf("--help 退出码 = %d, 期望 0 (stderr=%q)", got.code, got.stderr)
	}
	for _, r := range []string{"prices", "calendar", "rates"} {
		if !strings.Contains(got.stdout, r) {
			t.Fatalf("顶层 help 缺少资源 %s, stdout = %q", r, got.stdout)
		}
	}
	want := map[string][]string{
		"prices":   {"list", "create", "delete"},
		"calendar": {"list", "set", "delete"},
		"rates":    {"list", "create", "delete"},
	}
	for resource, subcommands := range want {
		got := runCLI(t, resource, "--help")
		if got.code != 0 {
			t.Fatalf("%s --help 退出码 = %d, 期望 0 (stderr=%q)", resource, got.code, got.stderr)
		}
		for _, sub := range subcommands {
			if !strings.Contains(got.stdout, "\n  "+sub+"\n") {
				t.Fatalf("%s --help 缺少命令 %s, stdout = %q", resource, sub, got.stdout)
			}
		}
	}
}
