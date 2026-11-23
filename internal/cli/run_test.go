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
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aigate/internal/cli"
	"aigate/internal/client"
	"aigate/internal/version"
)

// TestReadData 覆盖四种输入：字面量、@文件、- 标准输入均原样返回；
// 缺失文件、空 stdin、非法 JSON 均返回 error。三种来源都要经过 json.Valid。
func TestReadData(t *testing.T) {
	t.Run("字面量原样", func(t *testing.T) {
		want := []byte(`{"a":1}`)
		got, err := cli.ReadData(`{"a":1}`, strings.NewReader(""))
		if err != nil {
			t.Fatalf("ReadData 返回意外 error: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("返回 %q, 期望原样 %q", got, want)
		}
	})

	t.Run("@文件原样", func(t *testing.T) {
		want, err := os.ReadFile("testdata/model.json")
		if err != nil {
			t.Fatalf("读取夹具失败: %v", err)
		}
		got, err := cli.ReadData("@testdata/model.json", strings.NewReader(""))
		if err != nil {
			t.Fatalf("ReadData 返回意外 error: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("返回 %q, 期望文件内容 %q", got, want)
		}
	})

	t.Run("- stdin 原样", func(t *testing.T) {
		want := []byte(`{"x":  5, "y": [1, 2]}`)
		got, err := cli.ReadData("-", strings.NewReader(string(want)))
		if err != nil {
			t.Fatalf("ReadData 返回意外 error: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("返回 %q, 期望原样 %q", got, want)
		}
	})

	errorCases := []struct {
		name  string
		spec  string
		stdin string
	}{
		{name: "@缺失文件", spec: "@/definitely/not/here.json", stdin: ""},
		{name: "- 空 stdin", spec: "-", stdin: ""},
		{name: "字面量非法 JSON", spec: `{"a":1,}`, stdin: ""},
		{name: "- 非法 JSON", spec: "-", stdin: `not-json`},
	}
	for _, tc := range errorCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := cli.ReadData(tc.spec, strings.NewReader(tc.stdin)); err == nil {
				t.Fatal("期望返回 error，实际为 nil")
			}
		})
	}

	t.Run("@文件非法 JSON", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "invalid.json")
		if err := os.WriteFile(path, []byte(`{"a":1,}`), 0o600); err != nil {
			t.Fatalf("写临时文件失败: %v", err)
		}
		if _, err := cli.ReadData("@"+path, strings.NewReader("")); err == nil {
			t.Fatal("期望返回 error，实际为 nil")
		}
	})
}

// TestParseID 覆盖正整数解析，以及 0/负数/非数字/空串/溢出等非法输入。
func TestParseID(t *testing.T) {
	valid := []struct {
		in   string
		want uint64
	}{
		{in: "42", want: 42},
		{in: "1", want: 1},
		{in: "007", want: 7},
		{in: "18446744073709551615", want: 18446744073709551615},
	}
	for _, tc := range valid {
		got, err := cli.ParseID(tc.in)
		if err != nil {
			t.Fatalf("ParseID(%q) 返回意外 error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ParseID(%q) = %d, 期望 %d", tc.in, got, tc.want)
		}
	}

	invalid := []string{"0", "-1", "abc", "", "+7", "18446744073709551616", "1.5"}
	for _, in := range invalid {
		if _, err := cli.ParseID(in); err == nil {
			t.Errorf("ParseID(%q) 期望返回 error，实际为 nil", in)
		}
	}
}

// TestResolveBaseURL 覆盖根地址解析的优先级：--url > AIGATE_URL > 默认值；
// 并确认显式给出空 --url 时不回退，仍由 client.New 本地报错退出 2。
func TestResolveBaseURL(t *testing.T) {
	const envURL = "http://env.example:9000"
	const flagURL = "http://flag.example:9001"
	cases := []struct {
		name    string
		env     map[string]string
		urlFlag string
		urlSet  bool
		want    string
	}{
		{
			name: "env 有值",
			env:  map[string]string{"AIGATE_URL": envURL},
			want: envURL,
		},
		{
			name:    "--url 覆盖 env",
			env:     map[string]string{"AIGATE_URL": envURL},
			urlFlag: flagURL,
			urlSet:  true,
			want:    flagURL,
		},
		{
			name: "都空→默认",
			env:  map[string]string{},
			want: cli.DefaultBaseURL,
		},
		{
			name:    "显式空 --url 不回退",
			env:     map[string]string{"AIGATE_URL": envURL},
			urlFlag: "",
			urlSet:  true,
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string { return tc.env[k] }
			if got := cli.ResolveBaseURL(getenv, tc.urlFlag, tc.urlSet); got != tc.want {
				t.Fatalf("ResolveBaseURL() = %q, 期望 %q", got, tc.want)
			}
		})
	}
	if cli.DefaultBaseURL != "http://127.0.0.1:8080" {
		t.Fatalf("DefaultBaseURL = %q, 期望 http://127.0.0.1:8080", cli.DefaultBaseURL)
	}
}

// TestRunBaseURLSources 经 cli.Run 验证根地址确实按优先级送达客户端：env 有值 /
// --url 覆盖 都命中动态端口的 httptest（不占用固定 8080）；两者都空时 client.New
// 不再因空 URL 失败，命令继续走到分发阶段。
func TestRunBaseURLSources(t *testing.T) {
	srv, ch := newRecordingServer(t, http.StatusOK, []byte(`{"ok":true}`))

	t.Run("env 有值", func(t *testing.T) {
		env := defaultEnv()
		env["AIGATE_URL"] = srv.URL
		got := runCLIWithInput(t, strings.NewReader(""), env, "health")
		if got.code != 0 {
			t.Fatalf("code = %d, 期望 0 (stderr=%q)", got.code, got.stderr)
		}
		if rec := recvRecorded(t, ch); rec.path != "/healthz" {
			t.Fatalf("请求路径 = %q, 期望 /healthz", rec.path)
		}
	})

	t.Run("--url 覆盖 env", func(t *testing.T) {
		env := defaultEnv()
		env["AIGATE_URL"] = "http://127.0.0.1:1"
		got := runCLIWithInput(t, strings.NewReader(""), env, "--url", srv.URL, "health")
		if got.code != 0 {
			t.Fatalf("code = %d, 期望 0 (stderr=%q)", got.code, got.stderr)
		}
		if rec := recvRecorded(t, ch); rec.path != "/healthz" {
			t.Fatalf("请求路径 = %q, 期望 /healthz", rec.path)
		}
	})

	// 不连固定 8080：用未知命令停在命令分发阶段，client.New 若仍拿到空 URL
	// 会先报「invalid base URL scheme」；修复后应继续走到「unknown resource」。
	t.Run("两者都空→默认地址", func(t *testing.T) {
		got := runCLIWithInput(t, strings.NewReader(""), map[string]string{}, "nosuch", "cmd")
		if got.code != 2 {
			t.Fatalf("code = %d, 期望 2 (stderr=%q)", got.code, got.stderr)
		}
		if strings.Contains(got.stderr, "invalid base URL") {
			t.Fatalf("根地址解析不应再报空 URL 错误：stderr=%q", got.stderr)
		}
		if !strings.Contains(got.stderr, "unknown resource") {
			t.Fatalf("应继续到命令分发阶段：stderr=%q", got.stderr)
		}
	})
}

// TestRunLocalErrors 覆盖本地错误：非法 --id、未知命令、--data 读文件失败，
// 均返回 2、stderr 含英文错误、且不发出任何 HTTP 请求。
func TestRunLocalErrors(t *testing.T) {
	srv, hits := newMockServer(t)
	cases := []struct {
		name    string
		args    []string
		keyword string
	}{
		{
			name:    "非法 --id",
			args:    []string{"--url", srv.URL, "--id", "abc", "mock", "get"},
			keyword: "--id",
		},
		{
			name:    "未知命令",
			args:    []string{"--url", srv.URL, "nosuch", "cmd"},
			keyword: "unknown",
		},
		{
			name:    "--data 缺失文件",
			args:    []string{"--url", srv.URL, "--data", "@/definitely/not/here.json", "mock", "create"},
			keyword: "--data",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runCLI(t, tc.args...)
			if got.code != 2 {
				t.Fatalf("code = %d, 期望 2 (stdout=%q stderr=%q)", got.code, got.stdout, got.stderr)
			}
			if !strings.Contains(got.stderr, tc.keyword) {
				t.Fatalf("stderr = %q, 期望含英文错误关键字 %q", got.stderr, tc.keyword)
			}
			if got.stdout != "" {
				t.Fatalf("本地错误不应写 stdout，实际 %q", got.stdout)
			}
		})
	}
	if *hits != 0 {
		t.Fatalf("本地错误不应发出请求，实际命中 %d 次", *hits)
	}
}

// TestRunInvalidURLAndTimeout 覆盖非法 --url、非法 --timeout、flag 缺值 → 2。
func TestRunInvalidURLAndTimeout(t *testing.T) {
	srv, hits := newMockServer(t)
	cases := []struct {
		name string
		args []string
	}{
		{name: "非法 scheme", args: []string{"--url", "ftp://h", "mock", "get"}},
		{name: "非法 timeout", args: []string{"--url", srv.URL, "--timeout", "nope", "mock", "get"}},
		{name: "flag 缺值", args: []string{"--url", srv.URL, "--id"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runCLI(t, tc.args...)
			if got.code != 2 {
				t.Fatalf("code = %d, 期望 2 (stderr=%q)", got.code, got.stderr)
			}
			if got.stderr == "" {
				t.Fatal("本地错误应写英文诊断到 stderr")
			}
		})
	}
	if *hits != 0 {
		t.Fatalf("本地错误不应发出请求，实际命中 %d 次", *hits)
	}
}

// TestRunInvalidDataExits2 覆盖非法 --data 落在退出码 2（不是 4），且不发请求。
func TestRunInvalidDataExits2(t *testing.T) {
	srv, hits := newMockServer(t)
	got := runCLI(t, "--url", srv.URL, "--data", `{"bad":`, "mock", "create")
	if got.code != 2 {
		t.Fatalf("非法 --data 的退出码 = %d, 期望 2 (stderr=%q)", got.code, got.stderr)
	}
	if got.stderr == "" {
		t.Fatal("非法 --data 应写英文诊断到 stderr")
	}
	if *hits != 0 {
		t.Fatalf("非法 --data 不应发出请求，实际命中 %d 次", *hits)
	}
}

// TestRunExitCodes 覆盖退出码契约：200/401/500 → 0/3/4，传输失败 → 5；
// 2xx 时 stdout 是原样 body 且 stderr 为空，非 2xx 只写 stderr。
func TestRunExitCodes(t *testing.T) {
	statuses := []int{http.StatusOK, http.StatusUnauthorized, http.StatusInternalServerError}
	bodies := [][]byte{
		[]byte(`{"ok": true,  "n": 1}`),
		[]byte(`{"error":{"message":"unauthorized"}}`),
		[]byte(`{"error":{"message":"boom"}}`),
	}
	srv := newSequenceServer(t, statuses, bodies)

	wantCodes := []int{0, 3, 4}
	for i, want := range wantCodes {
		got := runCLI(t, "--url", srv.URL, "mock", "get")
		if got.code != want {
			t.Fatalf("第 %d 次请求: code = %d, 期望 %d (stdout=%q stderr=%q)", i, got.code, want, got.stdout, got.stderr)
		}
		if want == 0 {
			if got.stdout != string(bodies[0]) {
				t.Fatalf("2xx stdout = %q, 期望原样 %q", got.stdout, bodies[0])
			}
			if got.stderr != "" {
				t.Fatalf("2xx stderr 应为空，实际 %q", got.stderr)
			}
		} else {
			if got.stdout != "" {
				t.Fatalf("非 2xx 不应写 stdout，实际 %q", got.stdout)
			}
			if msg := client.ErrorMessage(bodies[i]); !strings.Contains(got.stderr, msg) {
				t.Fatalf("非 2xx stderr = %q, 期望含错误文案 %q", got.stderr, msg)
			}
		}
	}

	// 传输失败（监听已关闭）→ 5。
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	got := runCLI(t, "--url", deadURL, "mock", "get")
	if got.code != 5 {
		t.Fatalf("传输失败退出码 = %d, 期望 5 (stderr=%q)", got.code, got.stderr)
	}
}

// TestRunMissingToken 覆盖管理面命令缺 AIGATE_ADMIN_TOKEN → 2，且不发请求。
func TestRunMissingToken(t *testing.T) {
	srv, hits := newMockServer(t)
	env := defaultEnv()
	delete(env, "AIGATE_ADMIN_TOKEN")
	got := runCLIWithInput(t, strings.NewReader(""), env, "--url", srv.URL, "mock", "get")
	if got.code != 2 {
		t.Fatalf("缺 token 退出码 = %d, 期望 2 (stderr=%q)", got.code, got.stderr)
	}
	if !strings.Contains(strings.ToLower(got.stderr), "token") {
		t.Fatalf("stderr = %q, 期望含 token 相关英文诊断", got.stderr)
	}
	if *hits != 0 {
		t.Fatalf("缺 token 不应发出请求，实际命中 %d 次", *hits)
	}
}

// TestRunDataForwardedVerbatim 覆盖 @文件与 - stdin 的请求体逐字节送达，
// 不做 json.Marshal 重新序列化。
func TestRunDataForwardedVerbatim(t *testing.T) {
	srv, ch := newRecordingServer(t, http.StatusOK, []byte(`{"ok":true}`))

	t.Run("@文件逐字节", func(t *testing.T) {
		body := []byte(`{"a": 1,  "b": [2, 3]}`)
		path := filepath.Join(t.TempDir(), "body.json")
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("写临时文件失败: %v", err)
		}
		got := runCLI(t, "--url", srv.URL, "--data", "@"+path, "mock", "create")
		if got.code != 0 {
			t.Fatalf("code = %d, 期望 0 (stderr=%q)", got.code, got.stderr)
		}
		rec := recvRecorded(t, ch)
		if !bytes.Equal(rec.body, body) {
			t.Fatalf("线上请求体 = %q, 期望逐字节 %q", rec.body, body)
		}
		if rec.contentType != "application/json" {
			t.Fatalf("Content-Type = %q, 期望 application/json", rec.contentType)
		}
	})

	t.Run("- stdin 逐字节", func(t *testing.T) {
		body := []byte(`{"x":  5}`)
		got := runCLIWithInput(t, bytes.NewReader(body), defaultEnv(), "--url", srv.URL, "--data", "-", "mock", "create")
		if got.code != 0 {
			t.Fatalf("code = %d, 期望 0 (stderr=%q)", got.code, got.stderr)
		}
		rec := recvRecorded(t, ch)
		if !bytes.Equal(rec.body, body) {
			t.Fatalf("线上请求体 = %q, 期望逐字节 %q", rec.body, body)
		}
	})
}

// TestRunHelpAndVersion 覆盖本地命令 --help/-h/version 退出 0，且 help 列出资源名。
func TestRunHelpAndVersion(t *testing.T) {
	for _, flag := range []string{"--help", "-h"} {
		got := runCLI(t, flag)
		if got.code != 0 {
			t.Fatalf("%s 退出码 = %d, 期望 0 (stderr=%q)", flag, got.code, got.stderr)
		}
		if !strings.Contains(got.stdout, "mock") {
			t.Fatalf("%s 未列出资源名，stdout = %q", flag, got.stdout)
		}
	}
	got := runCLI(t, "version")
	if got.code != 0 {
		t.Fatalf("version 退出码 = %d, 期望 0 (stderr=%q)", got.code, got.stderr)
	}
	if cli.Version != version.Version {
		t.Fatalf("cli.Version = %q, 期望与 internal/version 一致（%q）", cli.Version, version.Version)
	}
	if !strings.Contains(got.stdout, version.Version) {
		t.Fatalf("version stdout = %q, 期望含版本字符串 %s", got.stdout, version.Version)
	}
}

// TestRunAgainstRealAdminHandlers 用临时 SQLite 装配真实 admin.Register +
// userapi.Register，驱动 cli.Run 一次，断言退出码与响应体逐字节一致。
func TestRunAgainstRealAdminHandlers(t *testing.T) {
	srv, token := newSmokeServer(t)

	// 直接发一次等价请求，取得服务端原始响应体作为期望值。
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/admin/settings", nil)
	if err != nil {
		t.Fatalf("构造期望请求失败: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("发送期望请求失败: %v", err)
	}
	expected, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("读取期望响应失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("期望请求 status = %d, 期望 200, body=%q", resp.StatusCode, expected)
	}

	env := defaultEnv()
	env["AIGATE_ADMIN_TOKEN"] = token
	got := runCLIWithInput(t, strings.NewReader(""), env, "--url", srv.URL, "mock", "get")
	if got.code != 0 {
		t.Fatalf("真实链路 code = %d, 期望 0 (stdout=%q stderr=%q)", got.code, got.stdout, got.stderr)
	}
	if got.stdout != string(expected) {
		t.Fatalf("真实链路 stdout = %q, 期望服务端原样响应体 %q", got.stdout, expected)
	}
	if got.stderr != "" {
		t.Fatalf("2xx 时 stderr 应为空，实际 %q", got.stderr)
	}
}
