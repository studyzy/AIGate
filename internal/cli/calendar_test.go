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
	"bytes"
	"net/http"
	"testing"
)

// calendarDataBody 是 set 用例经 --data 传入并期望逐字节送达的请求体。
var calendarDataBody = []byte(`{"days":[{"date":"2026-10-01","is_workday":false,"note":"国庆"}]}`)

// TestCalendarCommands 覆盖 calendar 资源 3 条命令：list 的 --year 进入 query，
// set 的 --data 作为 PUT 请求体，delete 的 --id 是 YYYY-MM-DD 日期字符串而非
// 正整数，必须原样作为单段路径拼进 /admin/billing-calendar/{date}。
func TestCalendarCommands(t *testing.T) {
	respBody := []byte(`{"year":2026,"days":[]}`)
	cases := []struct {
		name       string
		args       []string
		wantMethod string
		wantPath   string
		wantQuery  string
		wantBody   []byte
	}{
		{
			name:       "calendar list",
			args:       []string{"calendar", "list"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/billing-calendar",
		},
		{
			name:       "calendar list --year 2026",
			args:       []string{"calendar", "list", "--year", "2026"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/billing-calendar",
			wantQuery:  "year=2026",
		},
		{
			name:       "calendar set --data",
			args:       []string{"calendar", "set", "--data", string(calendarDataBody)},
			wantMethod: http.MethodPut,
			wantPath:   "/admin/billing-calendar",
			wantBody:   calendarDataBody,
		},
		{
			name:       "calendar delete --id 2026-10-01",
			args:       []string{"calendar", "delete", "--id", "2026-10-01"},
			wantMethod: http.MethodDelete,
			wantPath:   "/admin/billing-calendar/2026-10-01",
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
			if !bytes.Equal(rec.body, tc.wantBody) {
				t.Errorf("body = %q, 期望 %q", rec.body, tc.wantBody)
			}
			if rec.auth != adminAuth {
				t.Errorf("Authorization = %q, 期望管理面令牌 %q", rec.auth, adminAuth)
			}
		})
	}
}

// TestCalendarDeleteRejectsUnsafeID 覆盖 --id 的空值与含 '/' 值：前者在本地退出 2，
// 后者会越出单段路径（如多级或空段），同样退出 2 且不发请求。
func TestCalendarDeleteRejectsUnsafeID(t *testing.T) {
	for _, id := range []string{"", "2026/10/01", ".."} {
		t.Run("--id="+id, func(t *testing.T) {
			srv, hits := newMockServer(t)
			got := runCLI(t, "--url", srv.URL, "--id", id, "calendar", "delete")
			if got.code != 2 {
				t.Fatalf("--id %q 的退出码 = %d, 期望 2 (stderr=%q)", id, got.code, got.stderr)
			}
			if got.stderr == "" {
				t.Fatalf("--id %q 应写英文诊断到 stderr", id)
			}
			if *hits != 0 {
				t.Fatalf("--id %q 不应发出请求，实际命中 %d 次", id, *hits)
			}
		})
	}
}
