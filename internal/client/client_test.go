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

package client_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"aigate/internal/client"
)

// recorded 是 httptest 服务捕获到的一次请求快照，用于断言客户端真实发出的内容。
type recorded struct {
	method      string
	path        string
	rawQuery    string
	contentType string
	auth        string
	body        []byte
}

// newRecordingServer 起一个真实 HTTP 服务，把请求快照送入缓冲 channel；
// 用 channel 传递而非共享变量，避免 -race 下 handler goroutine 与测试 goroutine 的数据竞争。
func newRecordingServer(t *testing.T, status int, respBody []byte) (*httptest.Server, <-chan recorded) {
	t.Helper()
	ch := make(chan recorded, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		select {
		case ch <- recorded{
			method:      r.Method,
			path:        r.URL.Path,
			rawQuery:    r.URL.RawQuery,
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

// testConfig 构造带合理超时的配置。
func testConfig(base string) client.Config {
	return client.Config{BaseURL: base, Timeout: 3 * time.Second}
}

// resolveBase 把表驱动用例里的占位主机 http://h 替换为真实服务地址；
// 空串表示直接使用服务地址（无前缀、无尾斜杠）。
func resolveBase(base, serverURL string) string {
	if base == "" {
		return serverURL
	}
	return strings.Replace(base, "http://h", serverURL, 1)
}

// TestNormalizeBaseURL 覆盖 BaseURL 归一化：去除尾部斜杠、保留路径前缀、拒绝非法 scheme。
func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		name     string
		base     string
		wantPath string
		wantErr  bool
	}{
		{name: "空串视为裸服务地址", base: "", wantPath: "/admin/models"},
		{name: "无尾斜杠", base: "http://h", wantPath: "/admin/models"},
		{name: "有尾斜杠", base: "http://h/", wantPath: "/admin/models"},
		{name: "路径前缀", base: "http://h/prefix/", wantPath: "/prefix/admin/models"},
		{name: "非法 scheme", base: "ftp://h", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, ch := newRecordingServer(t, http.StatusOK, []byte("ok"))
			c, err := client.New(testConfig(resolveBase(tc.base, srv.URL)))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("New(%q) 期望返回 error，实际为 nil", tc.base)
				}
				if len(ch) != 0 {
					t.Fatalf("New 失败时不应发出任何请求")
				}
				return
			}
			if err != nil {
				t.Fatalf("New(%q) 返回意外 error: %v", tc.base, err)
			}
			res, err := c.Do(context.Background(), client.Request{
				Plane:  client.PlaneNone,
				Method: http.MethodGet,
				Path:   "/admin/models",
			})
			if err != nil {
				t.Fatalf("Do 返回意外 error: %v", err)
			}
			if res.StatusCode != http.StatusOK {
				t.Fatalf("StatusCode = %d, 期望 200", res.StatusCode)
			}
			rec := recvRecorded(t, ch)
			if rec.path != tc.wantPath {
				t.Fatalf("请求路径 = %q, 期望 %q", rec.path, tc.wantPath)
			}
		})
	}
}

// TestExitCode 覆盖退出码契约：2xx→0、401/403→3、其它→4。
func TestExitCode(t *testing.T) {
	cases := []struct {
		status int
		want   int
	}{
		{200, 0}, {201, 0}, {204, 0},
		{401, 3}, {403, 3},
		{400, 4}, {404, 4}, {409, 4}, {429, 4}, {500, 4}, {502, 4},
	}
	for _, tc := range cases {
		if got := client.ExitCode(tc.status); got != tc.want {
			t.Errorf("ExitCode(%d) = %d, 期望 %d", tc.status, got, tc.want)
		}
	}
}

// TestErrorMessage 覆盖统一错误体解析，以及非该形状/非法 JSON 时原样返回且不 panic。
func TestErrorMessage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "统一错误体", body: `{"error":{"message":"bad","type":"error","code":400}}`, want: "bad"},
		{name: "HTML", body: `<html>502</html>`, want: `<html>502</html>`},
		{name: "空串", body: ``, want: ``},
		{name: "截断 JSON", body: `{"error":{"message":"ba`, want: `{"error":{"message":"ba`},
		{name: "非错误体 JSON", body: `{"result":"ok"}`, want: `{"result":"ok"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := client.ErrorMessage([]byte(tc.body)); got != tc.want {
				t.Fatalf("ErrorMessage(%q) = %q, 期望 %q", tc.body, got, tc.want)
			}
		})
	}
}

// TestDoPlaneToken 覆盖三种面的令牌选择，以及缺令牌时返回 ErrMissingToken 且不发请求。
func TestDoPlaneToken(t *testing.T) {
	respBody := []byte(`{"ok":true}`)
	cases := []struct {
		name       string
		plane      client.Plane
		adminToken string
		userToken  string
		wantAuth   string
		wantErr    error
	}{
		{name: "admin 面", plane: client.PlaneAdmin, adminToken: "adm-tok", userToken: "usr-tok", wantAuth: "Bearer adm-tok"},
		{name: "user 面", plane: client.PlaneUser, adminToken: "adm-tok", userToken: "usr-tok", wantAuth: "Bearer usr-tok"},
		{name: "none 面", plane: client.PlaneNone, adminToken: "adm-tok", userToken: "usr-tok", wantAuth: ""},
		{name: "admin 缺 token", plane: client.PlaneAdmin, userToken: "usr-tok", wantErr: client.ErrMissingToken},
		{name: "user 缺 token", plane: client.PlaneUser, adminToken: "adm-tok", wantErr: client.ErrMissingToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, ch := newRecordingServer(t, http.StatusOK, respBody)
			cfg := testConfig(srv.URL)
			cfg.AdminToken = tc.adminToken
			cfg.UserToken = tc.userToken
			c, err := client.New(cfg)
			if err != nil {
				t.Fatalf("New 返回意外 error: %v", err)
			}
			res, err := c.Do(context.Background(), client.Request{
				Plane:  tc.plane,
				Method: http.MethodGet,
				Path:   "/admin/models",
			})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Do error = %v, 期望 errors.Is(_, %v)", err, tc.wantErr)
				}
				if len(ch) != 0 {
					t.Fatalf("缺 token 时不应发出任何请求")
				}
				return
			}
			if err != nil {
				t.Fatalf("Do 返回意外 error: %v", err)
			}
			if !bytes.Equal(res.Body, respBody) {
				t.Fatalf("响应体 = %q, 期望原样 %q", res.Body, respBody)
			}
			rec := recvRecorded(t, ch)
			if rec.auth != tc.wantAuth {
				t.Fatalf("Authorization = %q, 期望 %q", rec.auth, tc.wantAuth)
			}
		})
	}
}

// TestDoBodyAndQuery 覆盖 method/path/query/Content-Type/请求体原样送达，
// 以及响应体与状态码原样读回；无 body 时不设置 Content-Type。
func TestDoBodyAndQuery(t *testing.T) {
	t.Run("带 body", func(t *testing.T) {
		respBody := []byte(`{"id":7}`)
		srv, ch := newRecordingServer(t, http.StatusCreated, respBody)
		cfg := testConfig(srv.URL)
		cfg.AdminToken = "adm-tok"
		c, err := client.New(cfg)
		if err != nil {
			t.Fatalf("New 返回意外 error: %v", err)
		}
		body := []byte(`{"name":"x"}`)
		res, err := c.Do(context.Background(), client.Request{
			Plane:  client.PlaneAdmin,
			Method: http.MethodPost,
			Path:   "/admin/models",
			Query:  url.Values{"page": {"2"}, "q": {"a b"}},
			Body:   body,
		})
		if err != nil {
			t.Fatalf("Do 返回意外 error: %v", err)
		}
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("StatusCode = %d, 期望 201", res.StatusCode)
		}
		if !bytes.Equal(res.Body, respBody) {
			t.Fatalf("响应体 = %q, 期望原样 %q", res.Body, respBody)
		}
		rec := recvRecorded(t, ch)
		if rec.method != http.MethodPost {
			t.Errorf("Method = %q, 期望 POST", rec.method)
		}
		if rec.path != "/admin/models" {
			t.Errorf("Path = %q, 期望 /admin/models", rec.path)
		}
		if rec.rawQuery != "page=2&q=a+b" {
			t.Errorf("RawQuery = %q, 期望 page=2&q=a+b", rec.rawQuery)
		}
		if rec.contentType != "application/json" {
			t.Errorf("Content-Type = %q, 期望 application/json", rec.contentType)
		}
		if !bytes.Equal(rec.body, body) {
			t.Errorf("请求体 = %q, 期望原样 %q", rec.body, body)
		}
	})

	t.Run("不带 body", func(t *testing.T) {
		srv, ch := newRecordingServer(t, http.StatusOK, []byte("ok"))
		c, err := client.New(testConfig(srv.URL))
		if err != nil {
			t.Fatalf("New 返回意外 error: %v", err)
		}
		if _, err := c.Do(context.Background(), client.Request{
			Plane:  client.PlaneNone,
			Method: http.MethodGet,
			Path:   "/admin/models",
		}); err != nil {
			t.Fatalf("Do 返回意外 error: %v", err)
		}
		rec := recvRecorded(t, ch)
		if rec.contentType != "" {
			t.Errorf("无 body 时 Content-Type 应为空，实际 %q", rec.contentType)
		}
		if len(rec.body) != 0 {
			t.Errorf("无 body 时请求体应为空，实际 %q", rec.body)
		}
	})
}

// TestDoTransportError 覆盖传输失败时 Do 返回包装后的 error。
func TestDoTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	base := srv.URL
	srv.Close() // 关闭监听后连接必然失败

	c, err := client.New(testConfig(base))
	if err != nil {
		t.Fatalf("New 返回意外 error: %v", err)
	}
	_, err = c.Do(context.Background(), client.Request{
		Plane:  client.PlaneNone,
		Method: http.MethodGet,
		Path:   "/admin/models",
	})
	if err == nil {
		t.Fatal("连接失败时 Do 应返回 error，实际为 nil")
	}
	if errors.Unwrap(err) == nil {
		t.Fatalf("传输错误应包装底层错误，实际 %v", err)
	}
}
