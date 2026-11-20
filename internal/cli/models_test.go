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

// commandCase 描述一条命令的黑盒期望：argv 片段、HTTP 方法与路径、期望请求体。
// wantBody 为 nil 表示该命令不带请求体。
type commandCase struct {
	name       string
	args       []string
	wantMethod string
	wantPath   string
	wantBody   []byte
}

// adminAuth 是 defaultEnv 中管理面令牌对应的 Authorization 头，用于验证请求走管理面。
const adminAuth = "Bearer adm-test-token"

// runCommandCases 表驱动地断言每条命令：cli.Run 在 2xx 时返回 0、stdout 原样，
// 且 httptest 捕获的真实请求精确匹配方法、路径、请求体、Content-Type 与管理面令牌。
func runCommandCases(t *testing.T, cases []commandCase) {
	t.Helper()
	respBody := []byte(`{"ok":true,"n":1}`)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, ch := newRecordingServer(t, http.StatusOK, respBody)
			args := append([]string{"--url", srv.URL}, tc.args...)
			got := runCLI(t, args...)
			if got.code != 0 {
				t.Fatalf("code = %d, 期望 0 (stdout=%q stderr=%q)", got.code, got.stdout, got.stderr)
			}
			if got.stdout != string(respBody) {
				t.Fatalf("stdout = %q, 期望原样 %q", got.stdout, respBody)
			}
			if got.stderr != "" {
				t.Fatalf("2xx stderr 应为空，实际 %q", got.stderr)
			}
			rec := recvRecorded(t, ch)
			if rec.method != tc.wantMethod {
				t.Errorf("method = %q, 期望 %q", rec.method, tc.wantMethod)
			}
			if rec.path != tc.wantPath {
				t.Errorf("path = %q, 期望 %q", rec.path, tc.wantPath)
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
			if rec.auth != adminAuth {
				t.Errorf("Authorization = %q, 期望管理面令牌 %q", rec.auth, adminAuth)
			}
		})
	}
}

// modelsDataBody 是 create/update 用例经 --data 传入并期望逐字节送达的请求体。
var modelsDataBody = []byte(`{"name":"m","credential_id":3}`)

// TestModelsCommands 逐字覆盖 §5 表格的 6 条 models 命令。
func TestModelsCommands(t *testing.T) {
	runCommandCases(t, []commandCase{
		{
			name:       "models list",
			args:       []string{"models", "list"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/models",
		},
		{
			name:       "models get --id 7",
			args:       []string{"models", "get", "--id", "7"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/models/7",
		},
		{
			name:       "models create --data",
			args:       []string{"models", "create", "--data", string(modelsDataBody)},
			wantMethod: http.MethodPost,
			wantPath:   "/admin/models",
			wantBody:   modelsDataBody,
		},
		{
			name:       "models update --id 7 --data",
			args:       []string{"models", "update", "--id", "7", "--data", string(modelsDataBody)},
			wantMethod: http.MethodPut,
			wantPath:   "/admin/models/7",
			wantBody:   modelsDataBody,
		},
		{
			name:       "models delete --id 7",
			args:       []string{"models", "delete", "--id", "7"},
			wantMethod: http.MethodDelete,
			wantPath:   "/admin/models/7",
		},
		{
			name:       "models connectivity --id 7",
			args:       []string{"models", "connectivity", "--id", "7"},
			wantMethod: http.MethodPost,
			wantPath:   "/admin/models/7/connectivity",
		},
	})
}

// TestConnectivityOKFalse 覆盖连通性探测返回 200 + {"ok":false} 时，
// CLI 仍以退出码 0 原样输出响应体（不以响应体内容判定成败）。
func TestConnectivityOKFalse(t *testing.T) {
	respBody := []byte(`{"ok":false,"error_kind":"network_unreachable","message":"upstream unreachable"}`)
	srv, _ := newRecordingServer(t, http.StatusOK, respBody)
	got := runCLI(t, "--url", srv.URL, "--id", "7", "models", "connectivity")
	if got.code != 0 {
		t.Fatalf("code = %d, 期望 0 (stdout=%q stderr=%q)", got.code, got.stdout, got.stderr)
	}
	if got.stdout != string(respBody) {
		t.Fatalf("stdout = %q, 期望原样 %q", got.stdout, respBody)
	}
	if got.stderr != "" {
		t.Fatalf("2xx stderr 应为空，实际 %q", got.stderr)
	}
}
