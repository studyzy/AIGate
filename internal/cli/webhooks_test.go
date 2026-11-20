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

// webhooksDataBody 是 create/update 用例经 --data 传入并期望逐字节送达的请求体。
var webhooksDataBody = []byte(`{"url":"https://example.com/hook","events":["key.created"]}`)

// TestWebhooksCommands 逐字覆盖 §5 表格的 6 条 webhooks 命令。
func TestWebhooksCommands(t *testing.T) {
	runCommandCases(t, []commandCase{
		{
			name:       "webhooks list",
			args:       []string{"webhooks", "list"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/webhooks",
		},
		{
			name:       "webhooks get --id 9",
			args:       []string{"webhooks", "get", "--id", "9"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/webhooks/9",
		},
		{
			name:       "webhooks create --data",
			args:       []string{"webhooks", "create", "--data", string(webhooksDataBody)},
			wantMethod: http.MethodPost,
			wantPath:   "/admin/webhooks",
			wantBody:   webhooksDataBody,
		},
		{
			name:       "webhooks update --id 9 --data",
			args:       []string{"webhooks", "update", "--id", "9", "--data", string(webhooksDataBody)},
			wantMethod: http.MethodPut,
			wantPath:   "/admin/webhooks/9",
			wantBody:   webhooksDataBody,
		},
		{
			name:       "webhooks delete --id 9",
			args:       []string{"webhooks", "delete", "--id", "9"},
			wantMethod: http.MethodDelete,
			wantPath:   "/admin/webhooks/9",
		},
		{
			name:       "webhooks deliveries --id 9",
			args:       []string{"webhooks", "deliveries", "--id", "9"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/webhooks/9/deliveries",
		},
	})
}
