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
	"strings"
	"testing"
)

// 本文件覆盖两个单命令资源：import.go 的 import 与 misc.go 的 health；
// 文件清单未单列 import_test.go，故两类黑盒用例合并于此。
// importDataBody 是 import run 用例经 --data 传入并期望逐字节送达的请求体。
var importDataBody = []byte(`{"models":[],"users":[]}`)

// TestImportCommands 逐字覆盖 §5 表格的 import run 命令。
func TestImportCommands(t *testing.T) {
	runCommandCases(t, []commandCase{
		{
			name:       "import run --data",
			args:       []string{"import", "run", "--data", string(importDataBody)},
			wantMethod: http.MethodPost,
			wantPath:   "/admin/import",
			wantBody:   importDataBody,
		},
	})
}

// TestImportPartialFailure 覆盖 import run 部分失败：HTTP 200 且
// details[].action=error 时 CLI 仍以退出码 0 原样输出响应体，不检查 details。
func TestImportPartialFailure(t *testing.T) {
	respBody := []byte(`{"imported":1,"failed":1,"details":[{"row":2,"action":"error","message":"duplicate"},{"row":3,"action":"created"}]}`)
	srv, ch := newRecordingServer(t, http.StatusOK, respBody)
	got := runCLI(t, "--url", srv.URL, "--data", string(importDataBody), "import", "run")
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
	if rec.method != http.MethodPost {
		t.Errorf("method = %q, 期望 %q", rec.method, http.MethodPost)
	}
	if rec.path != "/admin/import" {
		t.Errorf("path = %q, 期望 /admin/import", rec.path)
	}
	if !bytes.Equal(rec.body, importDataBody) {
		t.Errorf("body = %q, 期望 %q", rec.body, importDataBody)
	}
	if rec.auth != adminAuth {
		t.Errorf("Authorization = %q, 期望管理面令牌 %q", rec.auth, adminAuth)
	}
}

// TestHealthCommand 覆盖单 token `aigatectl health`：走 PlaneNone（无 Authorization
// 头）、GET /healthz，2xx 响应体原样写 stdout；并确认默认 action 约定的边界与 help 列表。
func TestHealthCommand(t *testing.T) {
	respBody := []byte(`{"status":"ok"}`)
	srv, ch := newRecordingServer(t, http.StatusOK, respBody)
	got := runCLI(t, "--url", srv.URL, "health")
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
	if rec.method != http.MethodGet {
		t.Errorf("method = %q, 期望 %q", rec.method, http.MethodGet)
	}
	if rec.path != "/healthz" {
		t.Errorf("path = %q, 期望 /healthz", rec.path)
	}
	if rec.auth != "" {
		t.Errorf("health 使用 PlaneNone，Authorization 应为空，实际 %q", rec.auth)
	}
	if len(rec.body) != 0 {
		t.Errorf("health 不应带请求体，实际 %q", rec.body)
	}
	if rec.contentType != "" {
		t.Errorf("health 不应带 Content-Type，实际 %q", rec.contentType)
	}

	t.Run("help 列出 health", func(t *testing.T) {
		got := runCLI(t, "--help")
		if got.code != 0 {
			t.Fatalf("--help code = %d, 期望 0", got.code)
		}
		if !strings.Contains(got.stdout, "health") {
			t.Errorf("--help 未列出 health 资源，stdout = %q", got.stdout)
		}
	})

	t.Run("无同名 action 的单 token 仍报 2", func(t *testing.T) {
		got := runCLI(t, "models")
		if got.code != 2 {
			t.Fatalf("code = %d, 期望 2 (stdout=%q stderr=%q)", got.code, got.stdout, got.stderr)
		}
		if !strings.Contains(got.stderr, "missing resource and command") {
			t.Fatalf("stderr = %q, 期望含 missing resource and command", got.stderr)
		}
	})
}
