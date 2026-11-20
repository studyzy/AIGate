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

// credentialsDataBody 是 create/update 用例经 --data 传入并期望逐字节送达的请求体。
var credentialsDataBody = []byte(`{"name":"c","secrets":[{"key":"Authorization","value":"sk-x"}]}`)

// TestCredentialsCommands 逐字覆盖 §5 表格的 5 条 credentials 命令。
func TestCredentialsCommands(t *testing.T) {
	runCommandCases(t, []commandCase{
		{
			name:       "credentials list",
			args:       []string{"credentials", "list"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/credentials",
		},
		{
			name:       "credentials get --id 3",
			args:       []string{"credentials", "get", "--id", "3"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/credentials/3",
		},
		{
			name:       "credentials create --data",
			args:       []string{"credentials", "create", "--data", string(credentialsDataBody)},
			wantMethod: http.MethodPost,
			wantPath:   "/admin/credentials",
			wantBody:   credentialsDataBody,
		},
		{
			name:       "credentials update --id 3 --data",
			args:       []string{"credentials", "update", "--id", "3", "--data", string(credentialsDataBody)},
			wantMethod: http.MethodPut,
			wantPath:   "/admin/credentials/3",
			wantBody:   credentialsDataBody,
		},
		{
			name:       "credentials delete --id 3",
			args:       []string{"credentials", "delete", "--id", "3"},
			wantMethod: http.MethodDelete,
			wantPath:   "/admin/credentials/3",
		},
	})
}
