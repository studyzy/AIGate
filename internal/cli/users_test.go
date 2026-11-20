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
	"strings"
	"testing"
)

// usersDataBody 是 create/update 用例经 --data 传入并期望逐字节送达的请求体。
var usersDataBody = []byte(`{"username":"u"}`)

// TestUsersCommands 逐字覆盖 §5 表格的 5 条 users 命令。
func TestUsersCommands(t *testing.T) {
	runCommandCases(t, []commandCase{
		{
			name:       "users list",
			args:       []string{"users", "list"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/users",
		},
		{
			name:       "users create --data",
			args:       []string{"users", "create", "--data", string(usersDataBody)},
			wantMethod: http.MethodPost,
			wantPath:   "/admin/users",
			wantBody:   usersDataBody,
		},
		{
			name:       "users update --id 2 --data",
			args:       []string{"users", "update", "--id", "2", "--data", string(usersDataBody)},
			wantMethod: http.MethodPut,
			wantPath:   "/admin/users/2",
			wantBody:   usersDataBody,
		},
		{
			name:       "users delete --id 2",
			args:       []string{"users", "delete", "--id", "2"},
			wantMethod: http.MethodDelete,
			wantPath:   "/admin/users/2",
		},
		{
			name:       "users rotate --id 2",
			args:       []string{"users", "rotate", "--id", "2"},
			wantMethod: http.MethodPost,
			wantPath:   "/admin/users/2/rotate",
		},
	})
}

// TestUsersNoGet 断言注册表里不存在 users get：服务端没有 GET /admin/users/{id}。
// 经 cli.Run 驱动 users get 应被判为未知命令（退出码 2、英文诊断、无 stdout），
// 且不发出任何 HTTP 请求。
func TestUsersNoGet(t *testing.T) {
	srv, hits := newMockServer(t)
	got := runCLI(t, "--url", srv.URL, "--id", "2", "users", "get")
	if got.code != 2 {
		t.Fatalf("users get 退出码 = %d, 期望 2 (stdout=%q stderr=%q)", got.code, got.stdout, got.stderr)
	}
	if got.stdout != "" {
		t.Fatalf("未知命令不应写 stdout，实际 %q", got.stdout)
	}
	if !strings.Contains(got.stderr, "unknown") {
		t.Fatalf("stderr = %q, 期望含英文诊断关键字 %q", got.stderr, "unknown")
	}
	if *hits != 0 {
		t.Fatalf("users get 不应发出请求（服务端无 GET /admin/users/{id}），实际命中 %d 次", *hits)
	}
}
