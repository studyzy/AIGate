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
	"testing"
)

// routersCreateBody 是 create 用例经 --data 传入并期望逐字节送达的请求体。
var routersCreateBody = []byte(`{"name":"r1","strategy":"decision","config":"{}","scope":"global","scope_id":0,"priority":0}`)

// routersPreviewBody 是 preview 用例的请求体（一条真实密钥 + 一段消息）。
var routersPreviewBody = []byte(`{"api_key_id":3,"request":{"messages":[{"role":"user","content":"hi"}]}}`)

// TestRoutersCommands 覆盖 routers 资源 7 条命令的方法与路径：
// list/strategies 无 --id；get/update/delete/preview 的 --id 是规则 id。
func TestRoutersCommands(t *testing.T) {
	runCommandCases(t, []commandCase{
		{
			name:       "routers list",
			args:       []string{"routers", "list"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/model-routers",
		},
		{
			name:       "routers strategies",
			args:       []string{"routers", "strategies"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/model-routers/strategies",
		},
		{
			name:       "routers get --id 4",
			args:       []string{"routers", "get", "--id", "4"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/model-routers/4",
		},
		{
			name:       "routers create --data",
			args:       []string{"routers", "create", "--data", string(routersCreateBody)},
			wantMethod: http.MethodPost,
			wantPath:   "/admin/model-routers",
			wantBody:   routersCreateBody,
		},
		{
			name:       "routers update --id 4 --data",
			args:       []string{"routers", "update", "--id", "4", "--data", `{"enabled":false}`},
			wantMethod: http.MethodPut,
			wantPath:   "/admin/model-routers/4",
			wantBody:   []byte(`{"enabled":false}`),
		},
		{
			name:       "routers delete --id 4",
			args:       []string{"routers", "delete", "--id", "4"},
			wantMethod: http.MethodDelete,
			wantPath:   "/admin/model-routers/4",
		},
		{
			name:       "routers preview --id 4 --data",
			args:       []string{"routers", "preview", "--id", "4", "--data", string(routersPreviewBody)},
			wantMethod: http.MethodPost,
			wantPath:   "/admin/model-routers/4/preview",
			wantBody:   routersPreviewBody,
		},
	})
}

// 带 --id 的子命令缺 --id 时本地退出 2、不发请求，避免拼出 /0 的伪请求。
func TestRoutersRequireID(t *testing.T) {
	for _, sub := range []string{"get", "update", "delete", "preview"} {
		t.Run(sub, func(t *testing.T) {
			srv, hits := newMockServer(t)
			got := runCLI(t, "--url", srv.URL, "routers", sub)
			if got.code != 2 {
				t.Fatalf("缺 --id 的退出码 = %d, 期望 2 (stderr=%q)", got.code, got.stderr)
			}
			if got.stderr == "" {
				t.Fatal("缺 --id 应写英文诊断到 stderr")
			}
			if *hits != 0 {
				t.Fatalf("缺 --id 不应发出请求，实际 %d 次", *hits)
			}
		})
	}
}

// 多余的未知 flag 退出 2 且不发请求（与其它资源命令一致）。
func TestRoutersRejectsUnknownFlag(t *testing.T) {
	srv, hits := newMockServer(t)
	got := runCLI(t, "--url", srv.URL, "routers", "list", "--nope", "x")
	if got.code != 2 {
		t.Fatalf("未知 flag 的退出码 = %d, 期望 2 (stderr=%q)", got.code, got.stderr)
	}
	if *hits != 0 {
		t.Fatalf("非法参数不应发出请求，实际 %d 次", *hits)
	}
}
