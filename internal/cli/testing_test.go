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

// Package cli_test 是命令框架的黑盒测试：经 cli.Run + httptest 走真实 HTTP，
// 只依赖导出接口；共享夹具集中在 testing_test.go。
package cli_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aigate/internal/admin"
	"aigate/internal/cli"
	"aigate/internal/client"
	"aigate/internal/model"
	"aigate/internal/userapi"
)

// init 注册一组测试专用命令，供各用例经 cli.Run 驱动。
// Plane 显式写成 PlaneAdmin，路径以 / 开头，避免依赖零值等隐式约定。
func init() {
	cli.Register("mock", map[string]cli.Command{
		"get": {
			Usage: "mock get",
			Run: func(ctx *cli.Context, _ []string) int {
				return cli.Exec(ctx, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodGet,
					Path:   "/admin/settings",
				})
			},
		},
		"create": {
			Usage: "mock create",
			Run: func(ctx *cli.Context, _ []string) int {
				return cli.Exec(ctx, client.Request{
					Plane:  client.PlaneAdmin,
					Method: http.MethodPost,
					Path:   "/admin/settings",
					Body:   ctx.Data,
				})
			},
		},
	})
}

// cliResult 是一次 cli.Run 的观测结果。
type cliResult struct {
	code   int
	stdout string
	stderr string
}

// defaultEnv 提供测试用环境变量；AIGATE_URL 通常被各用例的 --url 覆盖。
func defaultEnv() map[string]string {
	return map[string]string{
		"AIGATE_URL":         "http://127.0.0.1:1",
		"AIGATE_ADMIN_TOKEN": "adm-test-token",
		"AIGATE_USER_TOKEN":  "usr-test-token",
	}
}

// runCLIWithInput 以指定 stdin 与环境变量驱动 cli.Run，捕获退出码与两路输出。
func runCLIWithInput(t *testing.T, stdin io.Reader, env map[string]string, args ...string) cliResult {
	t.Helper()
	var out, errOut bytes.Buffer
	getenv := func(k string) string { return env[k] }
	code := cli.Run(args, stdin, &out, &errOut, getenv)
	return cliResult{code: code, stdout: out.String(), stderr: errOut.String()}
}

// runCLI 以空 stdin 与默认环境变量驱动 cli.Run。
func runCLI(t *testing.T, args ...string) cliResult {
	t.Helper()
	return runCLIWithInput(t, strings.NewReader(""), defaultEnv(), args...)
}

// newMockServer 起一个恒返回 200 的 mock 服务，并返回命中次数指针。
// 仅在「不应发出请求」的用例里读取该计数，故无需跨 goroutine 同步。
func newMockServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// newSequenceServer 按调用次序返回 statuses/bodies，用原子下标保证 -race 下无竞争。
func newSequenceServer(t *testing.T, statuses []int, bodies [][]byte) *httptest.Server {
	t.Helper()
	var idx atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := int(idx.Add(1)) - 1
		if i >= len(statuses) {
			i = len(statuses) - 1
		}
		w.WriteHeader(statuses[i])
		_, _ = w.Write(bodies[i])
	}))
	t.Cleanup(srv.Close)
	return srv
}

// recorded 是 httptest 服务捕获到的一次请求快照。
type recorded struct {
	method      string
	path        string
	contentType string
	auth        string
	body        []byte
}

// newRecordingServer 把请求快照送入缓冲 channel，避免 -race 下的共享变量竞争。
func newRecordingServer(t *testing.T, status int, respBody []byte) (*httptest.Server, <-chan recorded) {
	t.Helper()
	ch := make(chan recorded, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		select {
		case ch <- recorded{
			method:      r.Method,
			path:        r.URL.Path,
			contentType: r.Header.Get("Content-Type"),
			auth:        r.Header.Get("Authorization"),
			body:        data,
		}:
		default:
		}
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

// recvRecorded 读取一次请求快照；超时即失败，避免 channel 无值时挂死。
func recvRecorded(t *testing.T, ch <-chan recorded) recorded {
	t.Helper()
	select {
	case rec := <-ch:
		return rec
	case <-time.After(2 * time.Second):
		t.Fatal("超时：未捕获到任何请求")
		return recorded{}
	}
}

// newSmokeServer 用临时 SQLite 库装配真实的 admin.Register + userapi.Register，
// 预置一个启用的管理员并返回其明文 token，供真实链路冒烟测试使用。
func newSmokeServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "smoke.db")
	store, err := model.Open(model.DBTypeSQLite, path)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := store.AutoMigrate(); err != nil {
		_ = store.Close()
		t.Fatalf("migrate test db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	plain, hash, prefix, err := model.GenerateToken(model.TokenPrefixAccess)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	u := &model.User{
		Username:       "root",
		Role:           model.RoleAdmin,
		APITokenHash:   hash,
		APITokenPrefix: prefix,
		Enabled:        true,
	}
	if err := store.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("create admin: %v", err)
	}

	mux := http.NewServeMux()
	admin.Register(mux, store)
	userapi.Register(mux, store)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, plain
}
