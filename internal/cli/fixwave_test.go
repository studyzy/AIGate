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

// assertLocalReject 断言一次命令被本地拒绝：退出码 2、stdout 为空、
// stderr 含英文诊断、且没有任何 HTTP 请求被记录。
func assertLocalReject(t *testing.T, got cliResult, hits int) {
	t.Helper()
	if got.code != 2 {
		t.Fatalf("code = %d, 期望 2 (stdout=%q stderr=%q)", got.code, got.stdout, got.stderr)
	}
	if got.stdout != "" {
		t.Fatalf("本地错误不应写 stdout，实际 %q", got.stdout)
	}
	if got.stderr == "" {
		t.Fatal("本地错误应写英文诊断到 stderr")
	}
	if hits != 0 {
		t.Fatalf("本地错误不应发出请求，实际捕获 %d 次", hits)
	}
}

// TestFlagParseErrorsExit2 覆盖 I-1：query flag 解析失败（未知 flag、缺值、
// 多余位置参数、拼写错误吃掉后续合法 flag）以及无 query flag 命令上的多余
// 参数，都必须退出 2、写英文 stderr、且不发出任何 HTTP 请求。
func TestFlagParseErrorsExit2(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "query 命令未知 flag", args: []string{"usage", "summary", "--bogus-flag", "xyz"}},
		{name: "query 命令 flag 缺值", args: []string{"usage", "summary", "--start"}},
		{name: "query 命令多余位置参数", args: []string{"usage", "summary", "stray"}},
		{name: "query 命令拼写错误吞掉后续 flag", args: []string{"usage", "summary", "--group-by", "model", "--uers-id", "2", "--start", "2026-01-01"}},
		{name: "无 query 命令未知 flag", args: []string{"models", "list", "--bogus-flag", "xyz"}},
		{name: "无 query 命令多余位置参数", args: []string{"models", "list", "stray"}},
		{name: "无 query 命令 settings 多余位置参数", args: []string{"settings", "get", "stray"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, ch := newRecordingServer(t, http.StatusOK, []byte(`{"ok":true}`))
			got := runCLI(t, append([]string{"--url", srv.URL}, tc.args...)...)
			assertLocalReject(t, got, len(ch))
		})
	}
}

// TestDeclaredFlagsStillParse 是 I-1 的正向回归：修复后所有声明过的 query flag
// 仍能正常解析并精确进入 URL，不得因拒绝多余参数而误伤合法调用。
func TestDeclaredFlagsStillParse(t *testing.T) {
	respBody := []byte(`{"ok":true}`)
	srv, ch := newQueryRecordingServer(t, http.StatusOK, respBody, "")
	got := runCLI(t, "--url", srv.URL, "usage", "summary", "--group-by", "model", "--start", "2026-01-01")
	if got.code != 0 {
		t.Fatalf("code = %d, 期望 0 (stdout=%q stderr=%q)", got.code, got.stdout, got.stderr)
	}
	rec := recvQueryRecorded(t, ch)
	if rec.query.Get("group_by") != "model" || rec.query.Get("start") != "2026-01-01" {
		t.Fatalf("query = %q, 期望含 group_by=model 与 start=2026-01-01", rec.query.Encode())
	}
}

// TestMissingIDExits2 覆盖 I-2：带 id 的命令在缺少 --id 时不得把 0 拼进路径，
// 而是本地报错退出 2、不发请求；显式 --id 0/abc 仍保持退出 2。
func TestMissingIDExits2(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "models get 缺 id", args: []string{"models", "get"}},
		{name: "models connectivity 缺 id", args: []string{"models", "connectivity"}},
		{name: "keys delete 缺 id", args: []string{"keys", "delete"}},
		{name: "keys rotate 缺 id", args: []string{"keys", "rotate"}},
		{name: "credentials get 缺 id", args: []string{"credentials", "get"}},
		{name: "users update 缺 id", args: []string{"users", "update"}},
		{name: "groups get 缺 id", args: []string{"groups", "get"}},
		{name: "webhooks delete 缺 id", args: []string{"webhooks", "delete"}},
		{name: "webhooks deliveries 缺 id", args: []string{"webhooks", "deliveries"}},
		{name: "self keys get 缺 id", args: []string{"self", "keys", "get"}},
		{name: "self keys usage 缺 id", args: []string{"self", "keys", "usage"}},
		{name: "显式 --id 0", args: []string{"models", "get", "--id", "0"}},
		{name: "显式 --id abc", args: []string{"models", "get", "--id", "abc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, ch := newRecordingServer(t, http.StatusOK, []byte(`{"ok":true}`))
			got := runCLI(t, append([]string{"--url", srv.URL}, tc.args...)...)
			assertLocalReject(t, got, len(ch))
		})
	}
}

// TestVersionAndHealthHelp 覆盖 M-1：version/health 的 --help 展示本地命令信息
// （而非「unknown resource」或冗余的 health health 列表），退出 0；未知资源的
// 诊断只写 stderr，不写 stdout。
func TestVersionAndHealthHelp(t *testing.T) {
	t.Run("version --help", func(t *testing.T) {
		got := runCLI(t, "version", "--help")
		if got.code != 0 {
			t.Fatalf("code = %d, 期望 0 (stdout=%q stderr=%q)", got.code, got.stdout, got.stderr)
		}
		if got.stdout == "" {
			t.Fatal("version --help 应输出本地命令信息/顶层帮助")
		}
		if strings.Contains(got.stdout, "unknown resource") || strings.Contains(got.stderr, "unknown resource") {
			t.Fatalf("version 不应被当作未知资源: stdout=%q stderr=%q", got.stdout, got.stderr)
		}
		if !strings.Contains(got.stdout, "version") {
			t.Fatalf("version --help 应展示 version 本地命令信息: stdout=%q", got.stdout)
		}
	})

	t.Run("health --help", func(t *testing.T) {
		got := runCLI(t, "health", "--help")
		if got.code != 0 {
			t.Fatalf("code = %d, 期望 0 (stdout=%q stderr=%q)", got.code, got.stdout, got.stderr)
		}
		if got.stdout == "" {
			t.Fatal("health --help 应输出 sensible health 帮助")
		}
		if strings.Contains(got.stdout, "unknown resource") || strings.Contains(got.stderr, "unknown resource") {
			t.Fatalf("health 不应被当作未知资源: stdout=%q stderr=%q", got.stdout, got.stderr)
		}
		if strings.Contains(got.stdout, "Commands:") {
			t.Fatalf("health --help 不应再展示冗余的 health health 命令列表: stdout=%q", got.stdout)
		}
		if !strings.Contains(got.stdout, "health") {
			t.Fatalf("health --help 应展示 health 帮助: stdout=%q", got.stdout)
		}
	})

	t.Run("未知资源帮助诊断写 stderr", func(t *testing.T) {
		got := runCLI(t, "nosuchresource", "--help")
		if !strings.Contains(got.stderr, "unknown resource") {
			t.Fatalf("未知资源诊断应写 stderr: stderr=%q", got.stderr)
		}
		if strings.Contains(got.stdout, "unknown resource") {
			t.Fatalf("未知资源诊断不应写 stdout: stdout=%q", got.stdout)
		}
	})
}

// TestURLWithoutHostExits2 覆盖 M-3：--url "http://" 缺 host 时本地报错退出 2，
// 不得落到传输阶段被误判为退出码 5；stdout 为空、无请求发出。
func TestURLWithoutHostExits2(t *testing.T) {
	got := runCLI(t, "--url", "http://", "health")
	if got.code != 2 {
		t.Fatalf("code = %d, 期望 2 (stdout=%q stderr=%q)", got.code, got.stdout, got.stderr)
	}
	if got.stdout != "" {
		t.Fatalf("本地错误不应写 stdout，实际 %q", got.stdout)
	}
	if got.stderr == "" {
		t.Fatal("非法 --url 应写英文诊断到 stderr")
	}
}
