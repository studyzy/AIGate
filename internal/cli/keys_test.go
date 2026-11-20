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

// keysDataBody 是 create/update 用例经 --data 传入并期望逐字节送达的请求体。
var keysDataBody = []byte(`{"name":"k","user_id":5}`)

// TestKeysCommands 逐字覆盖 §5 表格的 6 条 keys 命令。
func TestKeysCommands(t *testing.T) {
	runCommandCases(t, []commandCase{
		{
			name:       "keys list",
			args:       []string{"keys", "list"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/keys",
		},
		{
			name:       "keys get --id 5",
			args:       []string{"keys", "get", "--id", "5"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/keys/5",
		},
		{
			name:       "keys create --data",
			args:       []string{"keys", "create", "--data", string(keysDataBody)},
			wantMethod: http.MethodPost,
			wantPath:   "/admin/keys",
			wantBody:   keysDataBody,
		},
		{
			name:       "keys update --id 5 --data",
			args:       []string{"keys", "update", "--id", "5", "--data", string(keysDataBody)},
			wantMethod: http.MethodPut,
			wantPath:   "/admin/keys/5",
			wantBody:   keysDataBody,
		},
		{
			name:       "keys delete --id 5",
			args:       []string{"keys", "delete", "--id", "5"},
			wantMethod: http.MethodDelete,
			wantPath:   "/admin/keys/5",
		},
		{
			name:       "keys rotate --id 5",
			args:       []string{"keys", "rotate", "--id", "5"},
			wantMethod: http.MethodPost,
			wantPath:   "/admin/keys/5/rotate",
		},
	})
}
