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

// groupsDataBody 是 create/update 用例经 --data 传入并期望逐字节送达的请求体。
var groupsDataBody = []byte(`{"name":"g"}`)

// TestGroupsCommands 逐字覆盖 §5 表格的 5 条 groups 命令（资源映射到 /admin/user-groups）。
func TestGroupsCommands(t *testing.T) {
	runCommandCases(t, []commandCase{
		{
			name:       "groups list",
			args:       []string{"groups", "list"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/user-groups",
		},
		{
			name:       "groups get --id 4",
			args:       []string{"groups", "get", "--id", "4"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/user-groups/4",
		},
		{
			name:       "groups create --data",
			args:       []string{"groups", "create", "--data", string(groupsDataBody)},
			wantMethod: http.MethodPost,
			wantPath:   "/admin/user-groups",
			wantBody:   groupsDataBody,
		},
		{
			name:       "groups update --id 4 --data",
			args:       []string{"groups", "update", "--id", "4", "--data", string(groupsDataBody)},
			wantMethod: http.MethodPut,
			wantPath:   "/admin/user-groups/4",
			wantBody:   groupsDataBody,
		},
		{
			name:       "groups delete --id 4",
			args:       []string{"groups", "delete", "--id", "4"},
			wantMethod: http.MethodDelete,
			wantPath:   "/admin/user-groups/4",
		},
	})
}
