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

// settingsDataBody 是 update 用例经 --data 传入并期望逐字节送达的请求体。
var settingsDataBody = []byte(`{"default_model_id":1}`)

// TestSettingsCommands 逐字覆盖 §5 表格的 2 条 settings 命令。
func TestSettingsCommands(t *testing.T) {
	runCommandCases(t, []commandCase{
		{
			name:       "settings get",
			args:       []string{"settings", "get"},
			wantMethod: http.MethodGet,
			wantPath:   "/admin/settings",
		},
		{
			name:       "settings update --data",
			args:       []string{"settings", "update", "--data", string(settingsDataBody)},
			wantMethod: http.MethodPut,
			wantPath:   "/admin/settings",
			wantBody:   settingsDataBody,
		},
	})
}
