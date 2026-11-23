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
	"net/http/httptest"
	"strings"
	"testing"

	"aigate/internal/cli"
)

// TestBuildInfoCommands 覆盖网关构建信息的两个等价入口：`build-info` 与
// `version --remote`。二者都走 PlaneNone（无 Authorization 头）GET /version，
// 2xx 响应体原样写 stdout，便于 `| jq`。version --remote 额外先打一行
// 本地二进制标识（含所查询的网关地址），以免与网关信息混淆。
func TestBuildInfoCommands(t *testing.T) {
	respBody := []byte(`{"version":"v0.1.0","commit":"abc1234","build_time":"2026-11-23T00:00:00Z","dirty":false,"go_version":"go1.24.12","started_at":"2026-11-23T00:01:00Z"}`)

	for _, args := range [][]string{{"build-info"}, {"build-info", "build-info"}, {"version", "--remote"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			srv, ch := newRecordingServer(t, http.StatusOK, respBody)
			got := runCLI(t, append([]string{"--url", srv.URL}, args...)...)
			if got.code != 0 {
				t.Fatalf("code = %d, 期望 0 (stdout=%q stderr=%q)", got.code, got.stdout, got.stderr)
			}
			want := string(respBody)
			if args[0] == "version" {
				want = cli.Version + " (local binary; querying gateway at " + srv.URL + ")\n" + want
			}
			if got.stdout != want {
				t.Fatalf("stdout = %q, 期望 %q", got.stdout, want)
			}
			if got.stderr != "" {
				t.Fatalf("2xx stderr 应为空，实际 %q", got.stderr)
			}
			rec := recvRecorded(t, ch)
			if rec.method != http.MethodGet {
				t.Errorf("method = %q, 期望 %q", rec.method, http.MethodGet)
			}
			if rec.path != "/version" {
				t.Errorf("path = %q, 期望 /version", rec.path)
			}
			if rec.auth != "" {
				t.Errorf("构建信息免鉴权，Authorization 应为空，实际 %q", rec.auth)
			}
			if len(rec.body) != 0 {
				t.Errorf("不应带请求体，实际 %q", rec.body)
			}
		})
	}
}

// TestVersionRemoteFailureKeepsStdoutEmpty 覆盖失败路径：网关不可达时 stdout 必须为空
// （header 只在请求成功后写），诊断只进 stderr，退出码为传输错误 5，
// 这样 `version --remote | jq` 不会读到一行半截的 header。
func TestVersionRemoteFailureKeepsStdoutEmpty(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	got := runCLI(t, "--url", deadURL, "version", "--remote")
	if got.code != 5 {
		t.Fatalf("code = %d, 期望 5 (stdout=%q stderr=%q)", got.code, got.stdout, got.stderr)
	}
	if got.stdout != "" {
		t.Fatalf("失败时 stdout 应为空，实际 %q", got.stdout)
	}
	if got.stderr == "" {
		t.Fatal("失败诊断应写 stderr")
	}
}

// TestVersionRemoteRejectsExtraArgs 确认 version --remote 与其它命令一致：
// 多余参数经本地校验退出 2，且不发请求、不写 stdout。
func TestVersionRemoteRejectsExtraArgs(t *testing.T) {
	srv, ch := newRecordingServer(t, http.StatusOK, []byte(`{"version":"v9.9.9"}`))
	got := runCLI(t, "--url", srv.URL, "version", "--remote", "bogus")
	if got.code != 2 {
		t.Fatalf("code = %d, 期望 2 (stdout=%q stderr=%q)", got.code, got.stdout, got.stderr)
	}
	if got.stdout != "" {
		t.Fatalf("本地参数错误时 stdout 应为空，实际 %q", got.stdout)
	}
	select {
	case <-ch:
		t.Fatal("本地参数错误不应发出请求")
	default:
	}
}

// TestVersionLocalStaysOffline 确认裸 `version` 报告的是 aigatectl 二进制自身，
// 显式标注 local binary，且不打印任何远端字段。
func TestVersionLocalStaysOffline(t *testing.T) {
	srv, _ := newRecordingServer(t, http.StatusOK, []byte(`{"version":"v9.9.9"}`))
	got := runCLI(t, "--url", srv.URL, "version")
	if got.code != 0 {
		t.Fatalf("code = %d, 期望 0 (stderr=%q)", got.code, got.stderr)
	}
	if want := "aigatectl " + cli.Version + " (local binary)\n"; got.stdout != want {
		t.Fatalf("stdout = %q, 期望本地版本行 %q", got.stdout, want)
	}
}
