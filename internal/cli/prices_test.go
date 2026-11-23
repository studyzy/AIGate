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

// pricesDataBody 是 create 用例经 --data 传入并期望逐字节送达的请求体。
var pricesDataBody = []byte(`{"currency":"CNY","effective_at":"2026-11-01T00:00:00Z","offpeak_output_micros":4000000}`)

// TestPricesCommands 覆盖 prices 资源 3 条命令：list/create 的 --id 是模型 id，
// delete 的 --id 是价目 id，断言方法、路径、请求体与管理面令牌逐字匹配。
func TestPricesCommands(t *testing.T) {
	runCommandCases(t, []commandCase{
		{
			name:       "prices list --id 7",
			args:       []string{"prices", "list", "--id", "7"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/models/7/prices",
		},
		{
			name:       "prices create --id 7 --data",
			args:       []string{"prices", "create", "--id", "7", "--data", string(pricesDataBody)},
			wantMethod: http.MethodPost,
			wantPath:   "/admin/models/7/prices",
			wantBody:   pricesDataBody,
		},
		{
			name:       "prices delete --id 11",
			args:       []string{"prices", "delete", "--id", "11"},
			wantMethod: http.MethodDelete,
			wantPath:   "/admin/model-prices/11",
		},
	})
}

// TestPricesCreateRequiresModelID 覆盖 prices create 缺 --id 时本地退出 2、不发请求，
// 避免把模型 id 的零值拼成 /admin/models/0/prices 的伪请求。
func TestPricesCreateRequiresModelID(t *testing.T) {
	srv, hits := newMockServer(t)
	got := runCLI(t, "--url", srv.URL, "--data", string(pricesDataBody), "prices", "create")
	if got.code != 2 {
		t.Fatalf("缺 --id 的退出码 = %d, 期望 2 (stderr=%q)", got.code, got.stderr)
	}
	if got.stderr == "" {
		t.Fatal("缺 --id 应写英文诊断到 stderr")
	}
	if *hits != 0 {
		t.Fatalf("缺 --id 不应发出请求，实际命中 %d 次", *hits)
	}
}
