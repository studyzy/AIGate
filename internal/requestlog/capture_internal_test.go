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

package requestlog

// 本文件是边缘采集中间件的同包白盒测试：用真实 http.Handler + httptest 驱动
// Middleware，断言请求体还原、响应包装、请求 ID、凭据哈希、头白名单等行为。
// 期望值用手写字面量，不复用被测代码的辅助函数。

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aigate/internal/httpx"
	"aigate/internal/model"
)

// recordingSink 记录 Submit 收到的每一条 Entry。
type recordingSink struct {
	entries []Entry
}

func (s *recordingSink) Submit(e Entry) {
	s.entries = append(s.entries, e)
}

// 编译期断言：recordingSink 满足 Sink 契约。
var _ Sink = (*recordingSink)(nil)

func singleEntry(t *testing.T, s *recordingSink) Entry {
	t.Helper()
	if len(s.entries) != 1 {
		t.Fatalf("期望恰好 1 条 Entry，实得 %d", len(s.entries))
	}
	return s.entries[0]
}

// newCaptureRequest 构造一条命中拦截路径的 POST 请求。
func newCaptureRequest(path string, body []byte) *http.Request {
	return httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
}

func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// TestCapture_BodyRestored：读请求体前缀后必须还原完整 body，超限仍保持 413 语义。
func TestCapture_BodyRestored(t *testing.T) {
	t.Run("within limit", func(t *testing.T) {
		body := []byte(`{"model":"gpt-4","messages":[]}`)
		var (
			got     []byte
			readErr error
		)
		sink := &recordingSink{}
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, readErr = httpx.ReadBody(r)
			w.WriteHeader(http.StatusOK)
		})
		req := newCaptureRequest("/v1/chat/completions", body)
		Middleware(next, sink).ServeHTTP(httptest.NewRecorder(), req)

		if readErr != nil {
			t.Fatalf("handler 读取请求体失败：%v", readErr)
		}
		if !bytes.Equal(got, body) {
			t.Errorf("handler 读到的 body 与原始不一致：\ngot  %q\nwant %q", got, body)
		}
		e := singleEntry(t, sink)
		if e.Truncated {
			t.Error("未超限的请求不应标记 truncated")
		}
		if e.RequestBody != string(body) {
			t.Errorf("RequestBody 应为原始 body，实得 %q", e.RequestBody)
		}
	})

	t.Run("over limit keeps 413 semantics", func(t *testing.T) {
		body := bytes.Repeat([]byte("x"), (1<<20)+100)
		var readErr error
		sink := &recordingSink{}
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, readErr = httpx.ReadBody(r)
			if errors.Is(readErr, httpx.ErrBodyTooLarge) {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
			}
		})
		req := newCaptureRequest("/v1/chat/completions", body)
		Middleware(next, sink).ServeHTTP(httptest.NewRecorder(), req)

		if !errors.Is(readErr, httpx.ErrBodyTooLarge) {
			t.Errorf("handler 仍应得到 ErrBodyTooLarge（413 语义不变），实得 %v", readErr)
		}
		e := singleEntry(t, sink)
		if !e.Truncated {
			t.Error("超限请求应标记 truncated")
		}
		if len(e.RequestBody) != 1<<20 {
			t.Errorf("RequestBody 应只保留 1 MiB 前缀，实得 %d 字节", len(e.RequestBody))
		}
	})
}

// TestCapture_ExactlyMaxBodyBytes（Review Focus 1）：恰好 1 MiB 既不触发 413，也不被截断。
func TestCapture_ExactlyMaxBodyBytes(t *testing.T) {
	body := bytes.Repeat([]byte("a"), 1<<20)
	var (
		got     []byte
		readErr error
	)
	sink := &recordingSink{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, readErr = httpx.ReadBody(r)
		w.WriteHeader(http.StatusOK)
	})
	req := newCaptureRequest("/v1/chat/completions", body)
	Middleware(next, sink).ServeHTTP(httptest.NewRecorder(), req)

	if readErr != nil {
		t.Fatalf("恰好 1 MiB 不应触发 413，实得 %v", readErr)
	}
	if !bytes.Equal(got, body) {
		t.Error("handler 应完整读到 1 MiB 请求体")
	}
	e := singleEntry(t, sink)
	if e.Truncated {
		t.Error("恰好 1 MiB 不应被截断")
	}
	if len(e.RequestBody) != 1<<20 {
		t.Errorf("RequestBody 长度应为 1 MiB，实得 %d", len(e.RequestBody))
	}
}

// TestCapture_ResponseWriter：包装器实现 Flusher/Unwrap，记录真实状态码，SSE 原样累积并按上限截断。
func TestCapture_ResponseWriter(t *testing.T) {
	t.Run("flusher unwrap status and sse accumulation", func(t *testing.T) {
		var (
			sink       recordingSink
			sawFlusher bool
			sawUnwrap  bool
		)
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, sawFlusher = w.(http.Flusher)
			_, sawUnwrap = w.(interface{ Unwrap() http.ResponseWriter })
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(207)
			for i := 0; i < 3; i++ {
				if _, err := w.Write([]byte("data: line\n\n")); err != nil {
					t.Errorf("写入 SSE 片段失败：%v", err)
				}
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		})
		req := newCaptureRequest("/v1/messages", []byte(`{}`))
		rec := httptest.NewRecorder()
		Middleware(next, &sink).ServeHTTP(rec, req)

		if !sawFlusher {
			t.Error("包装器应实现 http.Flusher")
		}
		if !sawUnwrap {
			t.Error("包装器应实现 Unwrap() http.ResponseWriter")
		}
		if !rec.Flushed {
			t.Error("Flush() 应透传给底层 ResponseWriter")
		}
		e := singleEntry(t, &sink)
		if e.StatusCode != 207 {
			t.Errorf("WriteHeader(207) 后 StatusCode 应为 207，实得 %d", e.StatusCode)
		}
		want := "data: line\n\ndata: line\n\ndata: line\n\n"
		if e.ResponseBody != want {
			t.Errorf("SSE 多行应原样累积，实得 %q", e.ResponseBody)
		}
		if e.Truncated {
			t.Error("未超限的响应不应标记 truncated")
		}
	})

	t.Run("response over limit is truncated", func(t *testing.T) {
		var sink recordingSink
		big := bytes.Repeat([]byte("z"), (1<<20)+10)
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(big)
		})
		req := newCaptureRequest("/v1/responses", []byte(`{}`))
		Middleware(next, &sink).ServeHTTP(httptest.NewRecorder(), req)

		e := singleEntry(t, &sink)
		if !e.Truncated {
			t.Error("响应超过 1 MiB 应标记 truncated")
		}
		if len(e.ResponseBody) != 1<<20 {
			t.Errorf("ResponseBody 应只保留 1 MiB 前缀，实得 %d 字节", len(e.ResponseBody))
		}
	})

	t.Run("exactly at limit not truncated", func(t *testing.T) {
		var sink recordingSink
		exact := bytes.Repeat([]byte("q"), 1<<20)
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(exact)
			_, _ = w.Write(nil) // 恰好达上限后的空写入不应改变截断状态
		})
		req := newCaptureRequest("/v1/systemone", []byte(`{}`))
		Middleware(next, &sink).ServeHTTP(httptest.NewRecorder(), req)

		e := singleEntry(t, &sink)
		if e.Truncated {
			t.Error("恰好 1 MiB 的响应不应被截断")
		}
		if len(e.ResponseBody) != 1<<20 {
			t.Errorf("ResponseBody 应为 1 MiB，实得 %d 字节", len(e.ResponseBody))
		}
	})
}

var errWriteBoom = errors.New("write boom")

// errorOnSecondWriter 在第二次 Write 返回错误，其余透传给内置 recorder。
type errorOnSecondWriter struct {
	*httptest.ResponseRecorder
	writes int
}

func (w *errorOnSecondWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == 2 {
		return 0, errWriteBoom
	}
	return w.ResponseRecorder.Write(p)
}

// TestCapture_WriteErrorStillSubmits（Review Focus 2）：Write 报错仍须提交且仅提交一条 Entry。
func TestCapture_WriteErrorStillSubmits(t *testing.T) {
	var sink recordingSink
	var gotErr error
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte("first-segment")); err != nil {
			t.Errorf("首段写入不应报错：%v", err)
		}
		_, gotErr = w.Write([]byte("second-segment"))
	})
	base := &errorOnSecondWriter{ResponseRecorder: httptest.NewRecorder()}
	req := newCaptureRequest("/v1/chat/completions", []byte(`{}`))
	Middleware(next, &sink).ServeHTTP(base, req)

	if !errors.Is(gotErr, errWriteBoom) {
		t.Errorf("第二次 Write 的错误应透传给调用方，实得 %v", gotErr)
	}
	// 无 panic、恰好一条、且前一段内容保留：singleEntry 在数量不为 1 时直接失败。
	e := singleEntry(t, &sink)
	if !strings.Contains(e.ResponseBody, "first-segment") {
		t.Errorf("前一段内容应保留在 Entry 中，实得 %q", e.ResponseBody)
	}
}

// TestCapture_RequestIDHeader：响应头在调用 next 之前设置，且与 Entry.ID 一致（32 位 hex）。
func TestCapture_RequestIDHeader(t *testing.T) {
	const wantHeader = "X-AIGate-Request-Id"
	var (
		sink       recordingSink
		seenInNext string
	)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 在 next 内部读取，证明请求 ID 头是在调用 next.ServeHTTP 之前就设置好的。
		seenInNext = w.Header().Get(wantHeader)
		w.WriteHeader(http.StatusOK)
	})
	req := newCaptureRequest("/v1/responses", []byte(`{}`))
	rec := httptest.NewRecorder()
	Middleware(next, &sink).ServeHTTP(rec, req)

	if seenInNext == "" {
		t.Fatal("X-AIGate-Request-Id 应在调用 next.ServeHTTP 之前设置")
	}
	if got := rec.Header().Get(wantHeader); got != seenInNext {
		t.Errorf("响应头值应与 next 内观察到的一致：header=%q next=%q", got, seenInNext)
	}
	e := singleEntry(t, &sink)
	if e.ID != seenInNext {
		t.Errorf("Entry.ID 应与响应头一致：id=%q header=%q", e.ID, seenInNext)
	}
	if !isHex32(e.ID) {
		t.Errorf("请求 ID 应为 16 字节（32 位）hex，实得 %q", e.ID)
	}
}

// TestCapture_HeaderAllowlist：仅白名单头进入日志，敏感头与框架头一律丢弃。
func TestCapture_HeaderAllowlist(t *testing.T) {
	var sink recordingSink
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 网关自己加的 X-AIGate-* 响应头也应进入 response_headers。
		w.Header().Set("X-AIGate-Upstream", "mock-upstream")
		w.WriteHeader(http.StatusOK)
	})
	req := newCaptureRequest("/v1/chat/completions", []byte(`{}`))
	// 白名单头
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "unit-test/1.0")
	req.Header.Set("X-Request-Id", "req-abc")
	req.Header.Set("X-AIGate-Client", "sdk")
	// 敏感头
	req.Header.Set("Authorization", "Bearer sk-secret")
	req.Header.Set("x-api-key", "sk-secret2")
	req.Header.Set("Cookie", "session=abc")
	req.Header.Set("Proxy-Authorization", "Basic xyz")
	req.Header.Set("X-Api-Key", "sk-secret3")
	req.Header.Set("X-Auth-Token", "tok")
	req.Header.Set("X-Session-Key", "keyval")
	// 框架头
	req.Header.Set("Content-Length", "2")
	req.Header.Set("Transfer-Encoding", "chunked")
	req.Header.Set("Connection", "keep-alive")

	Middleware(next, &sink).ServeHTTP(httptest.NewRecorder(), req)
	e := singleEntry(t, &sink)
	reqLower := strings.ToLower(e.RequestHeaders)
	respLower := strings.ToLower(e.ResponseHeaders)

	for _, want := range []string{"content-type", "accept", "user-agent", "x-request-id", "x-aigate-client"} {
		if !strings.Contains(reqLower, want) {
			t.Errorf("白名单头 %q 应保留在 RequestHeaders：%s", want, e.RequestHeaders)
		}
	}
	for _, bad := range []string{
		"authorization", "x-api-key", "cookie", "proxy-authorization",
		"x-auth-token", "x-session-key",
		"sk-secret", "sk-secret2", "sk-secret3", "bearer", "session=abc", "basic xyz",
		"content-length", "transfer-encoding", "connection",
	} {
		if strings.Contains(reqLower, bad) {
			t.Errorf("敏感/框架头或值 %q 不应出现在 RequestHeaders：%s", bad, e.RequestHeaders)
		}
	}
	if !strings.Contains(respLower, "x-aigate-upstream") {
		t.Errorf("X-AIGate-* 响应头应保留在 ResponseHeaders：%s", e.ResponseHeaders)
	}
}

// TestCapture_BothCredentialHeaders（Review Focus 3）：两种凭据头同时存在时 x-api-key 优先，
// 且只存哈希、两个头本身都不进日志。
func TestCapture_BothCredentialHeaders(t *testing.T) {
	var sink recordingSink
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := newCaptureRequest("/v1/messages", []byte(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-a")
	req.Header.Set("Authorization", "Bearer sk-b")
	Middleware(next, &sink).ServeHTTP(httptest.NewRecorder(), req)

	e := singleEntry(t, &sink)
	if want := model.HashToken("sk-a"); e.APIKeyHash != want {
		t.Errorf("x-api-key 优先：APIKeyHash 应为 %q，实得 %q", want, e.APIKeyHash)
	}
	lower := strings.ToLower(e.RequestHeaders)
	if strings.Contains(lower, "x-api-key") || strings.Contains(lower, "authorization") {
		t.Errorf("凭据头名不应出现在 RequestHeaders：%s", e.RequestHeaders)
	}
	if strings.Contains(lower, "sk-a") || strings.Contains(lower, "sk-b") {
		t.Errorf("凭据明文不应出现在 RequestHeaders：%s", e.RequestHeaders)
	}
	if !strings.Contains(lower, "content-type") {
		t.Errorf("Content-Type 应保留：%s", e.RequestHeaders)
	}
}

// trackingBody 记录 body 是否被读取，用于断言直通路由不读请求体。
type trackingBody struct {
	r     io.Reader
	reads int
}

func (b *trackingBody) Read(p []byte) (int, error) {
	b.reads++
	return b.r.Read(p)
}

func (b *trackingBody) Close() error { return nil }

// TestCapture_NonInferencePassthrough：非推理路由不读 body、不包装 writer、不产生 Entry。
func TestCapture_NonInferencePassthrough(t *testing.T) {
	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/models"},
		{http.MethodGet, "/admin/settings"},
		{http.MethodPost, "/admin/settings"},
		{http.MethodGet, "/v1/chat/completions"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var sink recordingSink
			var seen http.ResponseWriter
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = w // 故意不读 body
				w.WriteHeader(http.StatusOK)
			})
			tb := &trackingBody{r: strings.NewReader("some body")}
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Body = tb
			rec := httptest.NewRecorder()
			Middleware(next, &sink).ServeHTTP(rec, req)

			if tb.reads != 0 {
				t.Errorf("直通路由不应读 body，实际读 %d 次", tb.reads)
			}
			if len(sink.entries) != 0 {
				t.Errorf("直通路由不应产生 Entry，实得 %d 条", len(sink.entries))
			}
			if seen != http.ResponseWriter(rec) {
				t.Error("直通路由不应包装 ResponseWriter")
			}
		})
	}
}

// TestCapture_LatencyAndError：status>=400 才置 error，LatencyMS 非负。
func TestCapture_LatencyAndError(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"502 has error", http.StatusBadGateway, true},
		{"200 has no error", http.StatusOK, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sink recordingSink
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			})
			req := newCaptureRequest("/v1/decisions", []byte(`{}`))
			Middleware(next, &sink).ServeHTTP(httptest.NewRecorder(), req)

			e := singleEntry(t, &sink)
			if e.StatusCode != tc.status {
				t.Errorf("StatusCode 应为 %d，实得 %d", tc.status, e.StatusCode)
			}
			if tc.wantErr && e.Error == "" {
				t.Error("status_code >= 400 时 Error 应非空")
			}
			if !tc.wantErr && e.Error != "" {
				t.Errorf("成功请求 Error 应为空串，实得 %q", e.Error)
			}
			if e.LatencyMS < 0 {
				t.Errorf("LatencyMS 不应为负，实得 %d", e.LatencyMS)
			}
		})
	}
}

// TestCapture_ModelAndStream：尽力解析请求体中的 model/stream，并组装基础元数据字段。
func TestCapture_ModelAndStream(t *testing.T) {
	var sink recordingSink
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	body := []byte(`{"model":"gpt-4o","stream":true,"messages":[]}`)
	req := newCaptureRequest("/v1/chat/completions", body)
	Middleware(next, &sink).ServeHTTP(httptest.NewRecorder(), req)

	e := singleEntry(t, &sink)
	if e.Model != "gpt-4o" {
		t.Errorf("应尽力解析请求体中的 model，实得 %q", e.Model)
	}
	if !e.Stream {
		t.Error("请求体 stream=true 时 Stream 应为 true")
	}
	if e.Method != http.MethodPost {
		t.Errorf("Method 应为 POST，实得 %q", e.Method)
	}
	if e.Path != "/v1/chat/completions" {
		t.Errorf("Path 应为 /v1/chat/completions，实得 %q", e.Path)
	}
	if e.CreatedAt.IsZero() {
		t.Error("CreatedAt 应被赋值")
	}
}
