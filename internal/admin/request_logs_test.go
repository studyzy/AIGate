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

package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aigate/internal/admin"
	"aigate/internal/billing"
	"aigate/internal/model"
	"aigate/internal/requestlog"
)

// ---------------------------------------------------------------------------
// 测试替身与夹具
// ---------------------------------------------------------------------------

// fakeReader 是 requestlog.Reader 的测试替身：记录收到的 Query / id，按配置返回结果。
type fakeReader struct {
	lastQuery requestlog.Query
	lastID    string
	searches  int
	gets      int

	page      requestlog.Page
	searchErr error
	entry     *requestlog.Entry
	getErr    error
}

func (f *fakeReader) Search(_ context.Context, q requestlog.Query) (requestlog.Page, error) {
	f.searches++
	f.lastQuery = q
	if f.searchErr != nil {
		return requestlog.Page{}, f.searchErr
	}
	p := f.page
	// 模拟真实插件的分页回显，便于断言响应中的 page / page_size。
	p.Page, p.PageSize = q.Page, q.PageSize
	if p.Rows == nil {
		p.Rows = []requestlog.Row{}
	}
	return p, nil
}

func (f *fakeReader) Get(_ context.Context, id string) (*requestlog.Entry, error) {
	f.gets++
	f.lastID = id
	return f.entry, f.getErr
}

// newRequestLogServer 自建 mux 装配管理面并注入 fake Reader（R27），返回服务、库、
// 管理员 token 与 fake。newAdminServer 仍传 nil，故两个新路由只在本夹具里注册。
func newRequestLogServer(t *testing.T) (*httptest.Server, *model.Store, string, *fakeReader) {
	t.Helper()
	store := newTestStore(t)
	fake := &fakeReader{}
	mux := http.NewServeMux()
	admin.Register(mux, store, billing.NewStoreSource(store), fake)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	_, token := seedAdmin(t, store)
	return srv, store, token, fake
}

// ---------------------------------------------------------------------------
// 响应结构（仅测试使用；字段名与 §7.7 契约一致）
// ---------------------------------------------------------------------------

type requestLogRowResp struct {
	ID         string `json:"id"`
	CreatedAt  string `json:"created_at"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Model      string `json:"model"`
	APIKeyHash string `json:"api_key_hash"`
	StatusCode int    `json:"status_code"`
	LatencyMS  int    `json:"latency_ms"`
	Stream     bool   `json:"stream"`
	Error      string `json:"error"`
	Truncated  bool   `json:"truncated"`
}

type requestLogDetailResp struct {
	requestLogRowResp
	RequestBody     string `json:"request_body"`
	ResponseBody    string `json:"response_body"`
	RequestHeaders  string `json:"request_headers"`
	ResponseHeaders string `json:"response_headers"`
}

type requestLogsResp struct {
	Logs     []requestLogRowResp `json:"logs"`
	Total    int64               `json:"total"`
	Page     int                 `json:"page"`
	PageSize int                 `json:"page_size"`
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

// TestRequestLogs_RequiresAdmin 验证两个新路由挂在既有 RequireAdmin 子树内：无 token 401、普通用户 403。
func TestRequestLogs_RequiresAdmin(t *testing.T) {
	srv, store, _, _ := newRequestLogServer(t)
	_, userToken := seedUser(t, store, "normal", model.RoleUser, true)

	paths := []string{"/admin/request-logs", "/admin/request-logs/abc"}
	for _, path := range paths {
		status, body := doRequest(t, srv, http.MethodGet, path, "", nil)
		assertStatus(t, status, http.StatusUnauthorized, body)
	}
	for _, path := range paths {
		status, body := doRequest(t, srv, http.MethodGet, path, userToken, nil)
		assertStatus(t, status, http.StatusForbidden, body)
	}
}

// TestRequestLogs_NoReaderNotRegistered 验证 logs == nil 时两个路由不注册（404）。
func TestRequestLogs_NoReaderNotRegistered(t *testing.T) {
	store := newTestStore(t)
	mux := http.NewServeMux()
	admin.Register(mux, store, billing.NewStoreSource(store), nil)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	_, token := seedAdmin(t, store)

	for _, path := range []string{"/admin/request-logs", "/admin/request-logs/abc"} {
		status, body := doRequest(t, srv, http.MethodGet, path, token, nil)
		assertStatus(t, status, http.StatusNotFound, body)
	}
}

// TestRequestLogs_Pagination 验证默认分页、page_size 上限收敛与空列表必须是 [] 而非 null。
func TestRequestLogs_Pagination(t *testing.T) {
	srv, _, token, fake := newRequestLogServer(t)

	status, body := doRequest(t, srv, http.MethodGet, "/admin/request-logs", token, nil)
	assertStatus(t, status, http.StatusOK, body)
	if !strings.Contains(string(body), `"logs":[]`) {
		t.Fatalf("empty logs must be [] not null, body: %s", body)
	}
	resp := mustDecode[requestLogsResp](t, body)
	if resp.Logs == nil || len(resp.Logs) != 0 {
		t.Fatalf("logs = %#v, want empty non-nil slice", resp.Logs)
	}
	if resp.Page != 1 || resp.PageSize != 50 {
		t.Fatalf("echo pagination = %d/%d, want 1/50", resp.Page, resp.PageSize)
	}
	if fake.lastQuery.Page != 1 || fake.lastQuery.PageSize != 50 {
		t.Fatalf("query pagination = %d/%d, want 1/50", fake.lastQuery.Page, fake.lastQuery.PageSize)
	}

	status, body = doRequest(t, srv, http.MethodGet, "/admin/request-logs?page_size=9999", token, nil)
	assertStatus(t, status, http.StatusOK, body)
	if got := mustDecode[requestLogsResp](t, body).PageSize; got != 500 {
		t.Fatalf("response page_size = %d, want 500", got)
	}
	if fake.lastQuery.PageSize != 500 {
		t.Fatalf("query page_size = %d, want 500", fake.lastQuery.PageSize)
	}
}

// TestRequestLogs_FilterMapping 验证 13 个筛选参数全部映射到 requestlog.Query（R25）。
// api_key_id 指向一个真实密钥，其 hash 与 api_key_hash 参数取同一值，使两者效果在 Query 中同时可见。
func TestRequestLogs_FilterMapping(t *testing.T) {
	srv, store, token, fake := newRequestLogServer(t)
	const keyHash = "deadbeef"
	key := &model.APIKey{Name: "mapping", ProxyTokenHash: keyHash}
	if err := store.CreateAPIKey(context.Background(), key, nil); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	path := "/admin/request-logs?start=2026-11-25T10:00:00Z&end=2026-11-25T11:00:00Z" +
		"&api_key_id=" + itoa(key.ID) +
		"&api_key_hash=" + keyHash +
		"&model=gpt-4o&path=/v1/chat/completions&status_code=429&stream=true" +
		"&min_latency_ms=100&max_latency_ms=500&q=timeout&page=3&page_size=20"
	status, body := doRequest(t, srv, http.MethodGet, path, token, nil)
	assertStatus(t, status, http.StatusOK, body)

	q := fake.lastQuery
	wantStart := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC)
	if q.Start == nil || !q.Start.Equal(wantStart) {
		t.Errorf("start = %v, want %v", q.Start, wantStart)
	}
	if q.End == nil || !q.End.Equal(wantEnd) {
		t.Errorf("end = %v, want %v (exclusive)", q.End, wantEnd)
	}
	if q.APIKeyHash != keyHash {
		t.Errorf("api_key_hash = %q, want %q", q.APIKeyHash, keyHash)
	}
	if q.Model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", q.Model)
	}
	if q.Path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", q.Path)
	}
	if q.StatusCode != 429 {
		t.Errorf("status_code = %d, want 429", q.StatusCode)
	}
	if q.Stream == nil || !*q.Stream {
		t.Errorf("stream = %v, want non-nil true", q.Stream)
	}
	if q.MinLatencyMS != 100 {
		t.Errorf("min_latency_ms = %d, want 100", q.MinLatencyMS)
	}
	if q.MaxLatencyMS != 500 {
		t.Errorf("max_latency_ms = %d, want 500", q.MaxLatencyMS)
	}
	if q.Q != "timeout" {
		t.Errorf("q = %q, want timeout", q.Q)
	}
	if q.Page != 3 || q.PageSize != 20 {
		t.Errorf("page/page_size = %d/%d, want 3/20", q.Page, q.PageSize)
	}
}

// TestRequestLogs_APIKeyIDTranslation 验证 api_key_id → proxy_token_hash 的翻译与优先级（R28）。
func TestRequestLogs_APIKeyIDTranslation(t *testing.T) {
	srv, store, token, fake := newRequestLogServer(t)
	key := &model.APIKey{Name: "translate", ProxyTokenHash: "hash-from-key"}
	if err := store.CreateAPIKey(context.Background(), key, nil); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	// 仅 api_key_id：翻译成密钥的 proxy_token_hash。
	status, body := doRequest(t, srv, http.MethodGet, "/admin/request-logs?api_key_id="+itoa(key.ID), token, nil)
	assertStatus(t, status, http.StatusOK, body)
	if fake.lastQuery.APIKeyHash != key.ProxyTokenHash {
		t.Fatalf("APIKeyHash = %q, want %q", fake.lastQuery.APIKeyHash, key.ProxyTokenHash)
	}

	// 不存在的密钥 id → 404 api key not found。
	before := fake.searches
	status, body = doRequest(t, srv, http.MethodGet, "/admin/request-logs?api_key_id=999999", token, nil)
	assertStatus(t, status, http.StatusNotFound, body)
	if msg := decodeError(t, body).Error.Message; msg != "api key not found" {
		t.Fatalf("message = %q, want api key not found", msg)
	}
	if fake.searches != before {
		t.Fatalf("missing api key must not reach reader, searches %d -> %d", before, fake.searches)
	}

	// 仅 api_key_hash：直接映射。
	status, body = doRequest(t, srv, http.MethodGet, "/admin/request-logs?api_key_hash=raw-hash", token, nil)
	assertStatus(t, status, http.StatusOK, body)
	if fake.lastQuery.APIKeyHash != "raw-hash" {
		t.Fatalf("APIKeyHash = %q, want raw-hash", fake.lastQuery.APIKeyHash)
	}

	// 两者同时给出：以 api_key_id 的翻译结果为准。
	path := "/admin/request-logs?api_key_id=" + itoa(key.ID) + "&api_key_hash=ignored-hash"
	status, body = doRequest(t, srv, http.MethodGet, path, token, nil)
	assertStatus(t, status, http.StatusOK, body)
	if fake.lastQuery.APIKeyHash != key.ProxyTokenHash {
		t.Fatalf("APIKeyHash = %q, want %q (api_key_id wins)", fake.lastQuery.APIKeyHash, key.ProxyTokenHash)
	}
}

// TestRequestLogs_Detail 验证详情返回完整 Entry（snake_case 键、含正文/头列）、不存在 → 404、空 id → 400。
func TestRequestLogs_Detail(t *testing.T) {
	srv, _, token, fake := newRequestLogServer(t)
	fake.entry = &requestlog.Entry{
		ID:              "abc123",
		CreatedAt:       time.Date(2026, 11, 25, 9, 30, 0, 0, time.UTC),
		Method:          "POST",
		Path:            "/v1/chat/completions",
		Model:           "gpt-4o",
		APIKeyHash:      "deadbeef",
		StatusCode:      200,
		LatencyMS:       42,
		Stream:          true,
		RequestBody:     `{"prompt":"hi"}`,
		ResponseBody:    `{"answer":"yo"}`,
		RequestHeaders:  `{"content-type":"application/json"}`,
		ResponseHeaders: `{"content-type":"application/json"}`,
		Error:           "",
		Truncated:       true,
	}

	status, body := doRequest(t, srv, http.MethodGet, "/admin/request-logs/abc123", token, nil)
	assertStatus(t, status, http.StatusOK, body)
	detail := mustDecode[requestLogDetailResp](t, body)
	if detail.ID != "abc123" || detail.Method != "POST" || detail.StatusCode != 200 || !detail.Stream || !detail.Truncated {
		t.Fatalf("detail metadata = %+v", detail.requestLogRowResp)
	}
	if detail.RequestBody != `{"prompt":"hi"}` || detail.ResponseBody != `{"answer":"yo"}` {
		t.Fatalf("bodies = %q / %q", detail.RequestBody, detail.ResponseBody)
	}
	if detail.RequestHeaders != `{"content-type":"application/json"}` ||
		detail.ResponseHeaders != `{"content-type":"application/json"}` {
		t.Fatalf("headers = %q / %q", detail.RequestHeaders, detail.ResponseHeaders)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatalf("decode detail keys: %v", err)
	}
	for _, k := range []string{
		"id", "created_at", "method", "path", "model", "api_key_hash", "status_code",
		"latency_ms", "stream", "error", "truncated",
		"request_body", "response_body", "request_headers", "response_headers",
	} {
		if _, ok := keys[k]; !ok {
			t.Errorf("missing snake_case key %q in detail response", k)
		}
	}
	if fake.lastID != "abc123" {
		t.Fatalf("Get id = %q, want abc123", fake.lastID)
	}

	// 不存在 → 404。
	fake.entry = nil
	fake.getErr = requestlog.ErrNotFound
	status, body = doRequest(t, srv, http.MethodGet, "/admin/request-logs/missing", token, nil)
	assertStatus(t, status, http.StatusNotFound, body)
	if msg := decodeError(t, body).Error.Message; msg != "request log not found" {
		t.Fatalf("message = %q, want request log not found", msg)
	}

	// 空 id → 400（尾斜杠路径命中详情处理器，PathValue("id") 为空串）。
	fake.getErr = nil
	status, body = doRequest(t, srv, http.MethodGet, "/admin/request-logs/", token, nil)
	assertStatus(t, status, http.StatusBadRequest, body)
}

// TestRequestLogs_InvalidParams 验证非法参数回 400 且不下沉到 Reader；并覆盖单边延迟过滤合法（R24）。
func TestRequestLogs_InvalidParams(t *testing.T) {
	srv, _, token, fake := newRequestLogServer(t)

	cases := []struct {
		name string
		path string
	}{
		{"start-after-end", "/admin/request-logs?start=2026-11-26T00:00:00Z&end=2026-11-25T00:00:00Z"},
		{"min-greater-than-max", "/admin/request-logs?min_latency_ms=500&max_latency_ms=100"},
		{"deep-pagination", "/admin/request-logs?page=21&page_size=500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := doRequest(t, srv, http.MethodGet, tc.path, token, nil)
			assertStatus(t, status, http.StatusBadRequest, body)
		})
	}
	if fake.searches != 0 {
		t.Fatalf("invalid params must not reach reader, searches = %d", fake.searches)
	}

	// 只给 min_latency_ms（max 为 0，表示该侧无上界）是合法的。
	status, body := doRequest(t, srv, http.MethodGet, "/admin/request-logs?min_latency_ms=500", token, nil)
	assertStatus(t, status, http.StatusOK, body)
	if fake.lastQuery.MinLatencyMS != 500 || fake.lastQuery.MaxLatencyMS != 0 {
		t.Fatalf("single-sided latency mapping = %d/%d, want 500/0",
			fake.lastQuery.MinLatencyMS, fake.lastQuery.MaxLatencyMS)
	}
}

// TestRequestLogs_ReaderErrors 验证 ErrNotFound 经包装仍映射 404、其它读错误回固定 500 文案。
func TestRequestLogs_ReaderErrors(t *testing.T) {
	srv, _, token, fake := newRequestLogServer(t)

	fake.getErr = fmt.Errorf("get request log: %w", requestlog.ErrNotFound)
	status, body := doRequest(t, srv, http.MethodGet, "/admin/request-logs/missing", token, nil)
	assertStatus(t, status, http.StatusNotFound, body)

	fake.searchErr = errors.New("db exploded")
	status, body = doRequest(t, srv, http.MethodGet, "/admin/request-logs", token, nil)
	assertStatus(t, status, http.StatusInternalServerError, body)
	if msg := decodeError(t, body).Error.Message; msg != "internal server error" {
		t.Fatalf("message = %q, want internal server error", msg)
	}
	if strings.Contains(string(body), "db exploded") {
		t.Fatalf("internal error detail must not leak, body: %s", body)
	}
}
