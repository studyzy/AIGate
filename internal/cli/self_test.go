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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// userAuth 是 defaultEnv 中用户面令牌对应的 Authorization 头；断言它可证明
// self 命令走 client.PlaneUser 而非管理面。
const userAuth = "Bearer usr-test-token"

// selfRecorded 是 httptest 服务捕获的一次请求快照，保留方法、路径、原始
// query string、请求体与 Content-Type / Authorization 头，供黑盒断言真实
// 发出的 HTTP 请求。
type selfRecorded struct {
	method      string
	path        string
	rawQuery    string
	contentType string
	auth        string
	body        []byte
}

// newSelfRecordingServer 把请求快照送入缓冲 channel，避免 -race 下的数据竞争。
func newSelfRecordingServer(t *testing.T, status int, respBody []byte) (*httptest.Server, <-chan selfRecorded) {
	t.Helper()
	ch := make(chan selfRecorded, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		select {
		case ch <- selfRecorded{
			method:      r.Method,
			path:        r.URL.Path,
			rawQuery:    r.URL.RawQuery,
			contentType: r.Header.Get("Content-Type"),
			auth:        r.Header.Get("Authorization"),
			body:        data,
		}:
		default:
		}
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

// recvSelfRecorded 读取一次请求快照；超时即失败，避免 channel 无值时挂死。
func recvSelfRecorded(t *testing.T, ch <-chan selfRecorded) selfRecorded {
	t.Helper()
	select {
	case rec := <-ch:
		return rec
	case <-time.After(2 * time.Second):
		t.Fatal("超时：未捕获到任何请求")
		return selfRecorded{}
	}
}

// TestSelfCommands 逐字覆盖 12 条 self 用户面命令：经 cli.Run + httptest 断言
// 真实 method / path / query / body / Content-Type / Authorization，全部走
// PlaneUser（Bearer usr-test-token），并校验退出码 0、stdout 原样、stderr 为空。
func TestSelfCommands(t *testing.T) {
	respBody := []byte(`{"ok":true,"n":1}`)
	dataBody := []byte(`{"name":"k"}`)
	cases := []struct {
		name       string
		args       []string
		wantMethod string
		wantPath   string
		wantQuery  string
		wantBody   []byte
	}{
		{
			name:       "self me",
			args:       []string{"self", "me"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/user/me",
			wantQuery:  "",
		},
		{
			name:       "self quota",
			args:       []string{"self", "quota", "--user-id", "2"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/user/quota",
			wantQuery:  "user_id=2",
		},
		{
			name:       "self usage summary",
			args:       []string{"self", "usage", "summary", "--group-by", "model", "--start", "2026-01-01", "--user-id", "2"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/user/usage/summary",
			wantQuery:  "group_by=model&start=2026-01-01&user_id=2",
		},
		{
			name:       "self usage logs",
			args:       []string{"self", "usage", "logs", "--page", "3", "--page-size", "10", "--start", "2026-01-01"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/user/usage/logs",
			wantQuery:  "page=3&page_size=10&start=2026-01-01",
		},
		{
			name:       "self usage by-model",
			args:       []string{"self", "usage", "by-model", "--start", "2026-01-01", "--user-id", "2"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/user/usage/by-model",
			wantQuery:  "start=2026-01-01&user_id=2",
		},
		{
			name:       "self keys list",
			args:       []string{"self", "keys", "list", "--user-id", "2"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/user/keys",
			wantQuery:  "user_id=2",
		},
		{
			name:       "self keys get",
			args:       []string{"self", "keys", "get", "--id", "6", "--user-id", "2"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/user/keys/6",
			wantQuery:  "user_id=2",
		},
		{
			name:       "self keys create",
			args:       []string{"self", "keys", "create", "--data", string(dataBody), "--user-id", "2"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/user/keys",
			wantQuery:  "user_id=2",
			wantBody:   dataBody,
		},
		{
			name:       "self keys update",
			args:       []string{"self", "keys", "update", "--id", "6", "--data", string(dataBody), "--user-id", "2"},
			wantMethod: http.MethodPut,
			wantPath:   "/api/user/keys/6",
			wantQuery:  "user_id=2",
			wantBody:   dataBody,
		},
		{
			name:       "self keys delete",
			args:       []string{"self", "keys", "delete", "--id", "6", "--user-id", "2"},
			wantMethod: http.MethodDelete,
			wantPath:   "/api/user/keys/6",
			wantQuery:  "user_id=2",
		},
		{
			name:       "self keys rotate",
			args:       []string{"self", "keys", "rotate", "--id", "6", "--user-id", "2"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/user/keys/6/rotate",
			wantQuery:  "user_id=2",
		},
		{
			name:       "self keys usage",
			args:       []string{"self", "keys", "usage", "--id", "6", "--start", "2026-01-01", "--end", "2026-02-01", "--user-id", "2"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/user/keys/6/usage",
			wantQuery:  "end=2026-02-01&start=2026-01-01&user_id=2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, ch := newSelfRecordingServer(t, http.StatusOK, respBody)
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
			rec := recvSelfRecorded(t, ch)
			if rec.method != tc.wantMethod {
				t.Errorf("method = %q, 期望 %q", rec.method, tc.wantMethod)
			}
			if rec.path != tc.wantPath {
				t.Errorf("path = %q, 期望 %q", rec.path, tc.wantPath)
			}
			if rec.rawQuery != tc.wantQuery {
				t.Errorf("query = %q, 期望 %q", rec.rawQuery, tc.wantQuery)
			}
			if !bytes.Equal(rec.body, tc.wantBody) {
				t.Errorf("body = %q, 期望 %q", rec.body, tc.wantBody)
			}
			wantCT := ""
			if tc.wantBody != nil {
				wantCT = "application/json"
			}
			if rec.contentType != wantCT {
				t.Errorf("Content-Type = %q, 期望 %q", rec.contentType, wantCT)
			}
			if rec.auth != userAuth {
				t.Errorf("Authorization = %q, 期望用户面令牌 %q", rec.auth, userAuth)
			}
		})
	}
}

// TestSelfFallsBackToAdminToken 覆盖 Ruling #1：只设置 AIGATE_ADMIN_TOKEN、
// 不设置 AIGATE_USER_TOKEN 时，self 命令经 PlaneUser 回退到 admin token，
// 携带 Bearer adm-test-token 成功返回。
func TestSelfFallsBackToAdminToken(t *testing.T) {
	respBody := []byte(`{"ok":true}`)
	srv, ch := newSelfRecordingServer(t, http.StatusOK, respBody)
	env := map[string]string{
		"AIGATE_ADMIN_TOKEN": "adm-test-token",
		// 故意不设置 AIGATE_USER_TOKEN，触发 PlaneUser 回退。
	}
	got := runCLIWithInput(t, strings.NewReader(""), env, "--url", srv.URL, "self", "me")
	if got.code != 0 {
		t.Fatalf("code = %d, 期望 0 (stdout=%q stderr=%q)", got.code, got.stdout, got.stderr)
	}
	if got.stdout != string(respBody) {
		t.Fatalf("stdout = %q, 期望原样 %q", got.stdout, respBody)
	}
	if got.stderr != "" {
		t.Fatalf("2xx stderr 应为空，实际 %q", got.stderr)
	}
	rec := recvSelfRecorded(t, ch)
	if rec.auth != "Bearer adm-test-token" {
		t.Errorf("Authorization = %q, 期望回退的 admin token %q", rec.auth, "Bearer adm-test-token")
	}
}

// helpHasLine 判断 help 输出中是否存在某一行（去空白后）等于 name。
func helpHasLine(out, name string) bool {
	for _, ln := range strings.Split(out, "\n") {
		if strings.TrimSpace(ln) == name {
			return true
		}
	}
	return false
}

// assertHelpLines 断言 help 输出逐行列出每个期望的名字。
func assertHelpLines(t *testing.T, out string, names ...string) {
	t.Helper()
	for _, name := range names {
		if !helpHasLine(out, name) {
			t.Errorf("help 输出未逐行列出 %q:\n%s", name, out)
		}
	}
}

// assertExit0 断言一次 cli.Run 正常退出（退出码 0，stderr 为空）。
func assertExit0(t *testing.T, got cliResult) {
	t.Helper()
	if got.code != 0 {
		t.Errorf("code = %d, 期望 0 (stderr=%q)", got.code, got.stderr)
	}
	if got.stderr != "" {
		t.Errorf("help stderr 应为空，实际 %q", got.stderr)
	}
}

// TestSelfHelpLadder 覆盖 Ruling #2 的三级递归帮助：
// 层级 0 顶层资源列表（保持不变）；层级 1 self 的命令名；层级 2 usage/keys
// 子组的子动作列表。
func TestSelfHelpLadder(t *testing.T) {
	// 层级 0：aigatectl --help 仍列出全部资源（含 self）。
	got0 := runCLI(t, "--help")
	assertExit0(t, got0)
	if !strings.Contains(got0.stdout, "Resources:") {
		t.Errorf("顶层 help 未列出资源表头:\n%s", got0.stdout)
	}
	if !helpHasLine(got0.stdout, "self") {
		t.Errorf("顶层 help 未列出 self 资源:\n%s", got0.stdout)
	}

	// 层级 1：aigatectl self --help 列出 me / quota / usage / keys。
	got1 := runCLI(t, "self", "--help")
	assertExit0(t, got1)
	if !strings.Contains(got1.stdout, "Commands:") {
		t.Errorf("层级 1 help 未列出命令表头:\n%s", got1.stdout)
	}
	assertHelpLines(t, got1.stdout, "me", "quota", "usage", "keys")

	// 层级 2：aigatectl self usage --help 列出 summary / logs / by-model。
	gotUsage := runCLI(t, "self", "usage", "--help")
	assertExit0(t, gotUsage)
	assertHelpLines(t, gotUsage.stdout, "summary", "logs", "by-model")

	// 层级 2：aigatectl self keys --help 列出 7 个子动作。
	gotKeys := runCLI(t, "self", "keys", "--help")
	assertExit0(t, gotKeys)
	assertHelpLines(t, gotKeys.stdout, "list", "get", "create", "update", "delete", "rotate", "usage")
}
