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

// 本文件是请求日志能力的端到端接线测试（黑盒，package requestlog_test）：
// 用 httptest 把 Middleware → NewWriter → sqlitePlugin → Reader 串成一条真实 HTTP 链路，
// 复用 es_test.go / sqlite_test.go 里已有的包级夹具，不改动任何既有文件。
package requestlog_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"aigate/internal/requestlog"
)

// ---------------------------------------------------------------------------
// 本测试专属的小工具（以 integration 前缀命名，避免与既有夹具同名）
// ---------------------------------------------------------------------------

// integrationKeyHash 复刻边缘取哈希口径：hex(sha256(token))。
func integrationKeyHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// doIntegrationRequest 打一次推理入口请求，排空响应体后回带 X-AIGate-Request-Id。
func doIntegrationRequest(t *testing.T, baseURL, path, apiKey, body string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求 %s 失败：%v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body) // 排空 SSE 流，确保连接正常收尾
	id := resp.Header.Get(requestlog.RequestIDHeader)
	if id == "" {
		t.Fatalf("响应缺少 %s", requestlog.RequestIDHeader)
	}
	return id
}

// waitIntegrationRows 轮询 Reader 直到落库条数达到 want；异步写入器按批次/周期刷写，
// 黑盒测试不能注入私有周期，故以有界轮询等待消费者完成一次刷写。
func waitIntegrationRows(t *testing.T, r requestlog.Reader, want int) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for {
		page, err := r.Search(context.Background(), requestlog.Query{})
		if err != nil {
			t.Fatalf("等待落库期间 Search 失败：%v", err)
		}
		if len(page.Rows) >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 %d 条落库超时：当前仅 %d 条（写入器生命周期是否已装配？）", want, len(page.Rows))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// 端到端：Middleware → NewWriter → sqlitePlugin → Reader
// ---------------------------------------------------------------------------

// TestIntegration_EndToEnd 打一个非流式 JSON 响应与一个 SSE 流式响应，断言两条日志落库、
// 请求 ID 与行 id 一致、列表不含正文、详情含正文，且时间/密钥/状态/流式/模型筛选生效。
func TestIntegration_EndToEnd(t *testing.T) {
	ctx := context.Background()
	path := tempDBPath(t)
	plugin := openSQLiteAt(t, path, map[string]any{"retention_days": 0})

	writer := requestlog.NewWriter(plugin)
	// R18/R29：启动期显式 Start 拉起消费者；停机期排空队列并关闭插件（复用同一 shutdown 预算）。
	writer.Start()
	t.Cleanup(func() { writer.Stop(context.Background()) })

	// 两个推理入口的桩 handler：一个非流式 429 JSON，一个 200 SSE 流。
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"quota exceeded"}}`)
	})
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range []string{
			"event: content_block_delta\ndata: {\"delta\":{\"text\":\"hel\"}}\n\n",
			"event: content_block_delta\ndata: {\"delta\":{\"text\":\"lo\"}}\n\n",
			"event: message_stop\ndata: [DONE]\n\n",
		} {
			_, _ = io.WriteString(w, chunk)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})

	handler := requestlog.Middleware(mux, writer)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	before := time.Now().UTC().Add(-time.Minute)
	idJSON := doIntegrationRequest(t, srv.URL, "/v1/chat/completions", "key-alpha",
		`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"prompt-alpha"}]}`)
	idSSE := doIntegrationRequest(t, srv.URL, "/v1/messages", "key-beta",
		`{"model":"claude-3","stream":true,"messages":[{"role":"user","content":"prompt-beta"}]}`)
	after := time.Now().UTC().Add(time.Minute)

	// R29：装配层把同一个 plugin 同时交给 writer 与 admin 的 Reader。
	var reader requestlog.Reader = plugin
	waitIntegrationRows(t, reader, 2)

	// 两条日志落库，且 X-AIGate-Request-Id 与行 id 集合一致。
	page, err := reader.Search(ctx, requestlog.Query{})
	if err != nil {
		t.Fatalf("Search 失败：%v", err)
	}
	if page.Total != 2 || len(page.Rows) != 2 {
		t.Fatalf("落库条数 Total/Rows = %d/%d，期望 2/2", page.Total, len(page.Rows))
	}
	gotIDs := []string{page.Rows[0].ID, page.Rows[1].ID}
	wantIDs := []string{idJSON, idSSE}
	sort.Strings(gotIDs)
	sort.Strings(wantIDs)
	if !slices.Equal(gotIDs, wantIDs) {
		t.Errorf("请求 ID 头 %v 与行 id %v 不一致", wantIDs, gotIDs)
	}

	// 列表行不带正文：[]Row 序列化后既无正文字段、也无正文内容。
	rowJSON, err := json.Marshal(page.Rows)
	if err != nil {
		t.Fatalf("序列化列表行失败：%v", err)
	}
	for _, marker := range []string{"RequestBody", "ResponseBody", "prompt-alpha", "prompt-beta"} {
		if strings.Contains(string(rowJSON), marker) {
			t.Errorf("列表行不应携带正文；在 %s 中发现 %q", rowJSON, marker)
		}
	}

	// 详情带回正文与关键元信息。
	eJSON, err := reader.Get(ctx, idJSON)
	if err != nil {
		t.Fatalf("Get(%s) 失败：%v", idJSON, err)
	}
	if !strings.Contains(eJSON.RequestBody, "prompt-alpha") || !strings.Contains(eJSON.ResponseBody, "quota exceeded") {
		t.Errorf("非流式详情缺少正文：req=%q resp=%q", eJSON.RequestBody, eJSON.ResponseBody)
	}
	if eJSON.StatusCode != http.StatusTooManyRequests || eJSON.Error != "Too Many Requests" || eJSON.Stream {
		t.Errorf("非流式详情元信息不符：%+v", eJSON)
	}
	eSSE, err := reader.Get(ctx, idSSE)
	if err != nil {
		t.Fatalf("Get(%s) 失败：%v", idSSE, err)
	}
	if !strings.Contains(eSSE.RequestBody, "prompt-beta") {
		t.Errorf("流式详情缺少请求正文：%q", eSSE.RequestBody)
	}
	for _, marker := range []string{"content_block_delta", "[DONE]", "hel", "lo"} {
		if !strings.Contains(eSSE.ResponseBody, marker) {
			t.Errorf("流式响应正文缺少分块标记 %q：%q", marker, eSSE.ResponseBody)
		}
	}
	if eSSE.StatusCode != http.StatusOK || eSSE.Error != "" || !eSSE.Stream {
		t.Errorf("流式详情元信息不符：%+v", eSSE)
	}

	hashAlpha := integrationKeyHash("key-alpha")
	hashBeta := integrationKeyHash("key-beta")

	// api-key 筛选。
	assertIDs(t, plugin, requestlog.Query{APIKeyHash: hashAlpha}, idJSON)
	assertIDs(t, plugin, requestlog.Query{APIKeyHash: hashBeta}, idSSE)
	assertIDs(t, plugin, requestlog.Query{APIKeyHash: integrationKeyHash("nobody")})

	// 状态筛选。
	assertIDs(t, plugin, requestlog.Query{StatusCode: http.StatusTooManyRequests}, idJSON)
	assertIDs(t, plugin, requestlog.Query{StatusCode: http.StatusOK}, idSSE)
	assertIDs(t, plugin, requestlog.Query{StatusCode: http.StatusInternalServerError})

	// 流式/模型筛选。
	assertIDs(t, plugin, requestlog.Query{Stream: boolPtr(false)}, idJSON)
	assertIDs(t, plugin, requestlog.Query{Stream: boolPtr(true)}, idSSE)
	assertIDs(t, plugin, requestlog.Query{Model: "gpt-4"}, idJSON)
	assertIDs(t, plugin, requestlog.Query{Model: "claude-3"}, idSSE)

	// 时间筛选（半开区间）：窗口内两条、窗口外零条。
	timeIDs := searchIDs(t, plugin, requestlog.Query{Start: timePtr(before), End: timePtr(after)})
	sort.Strings(timeIDs)
	if !slices.Equal(timeIDs, wantIDs) {
		t.Errorf("时间窗口内 IDs = %v，期望 %v", timeIDs, wantIDs)
	}
	assertIDs(t, plugin, requestlog.Query{Start: timePtr(after.Add(time.Hour))})
	assertIDs(t, plugin, requestlog.Query{End: timePtr(before.Add(-time.Hour))})

	// 组合筛选（AND 语义）。
	assertIDs(t, plugin, requestlog.Query{
		StatusCode: http.StatusOK, APIKeyHash: hashBeta, Stream: boolPtr(true),
	}, idSSE)
	assertIDs(t, plugin, requestlog.Query{
		StatusCode: http.StatusTooManyRequests, APIKeyHash: hashAlpha, Stream: boolPtr(false),
	}, idJSON)
}

// ---------------------------------------------------------------------------
// sqlite 与 mock ES 的筛选语义对齐
// ---------------------------------------------------------------------------

// TestIntegration_FilterParity 对同一组 Query 分别在 sqlite 与 mock ES 上执行，
// 断言返回的 []Row.ID 集合一致（R9）。
func TestIntegration_FilterParity(t *testing.T) {
	sq := openSQLiteAt(t, tempDBPath(t), map[string]any{"retention_days": 0})
	m := newMockES(t)
	es := openES(t, m, nil)
	day0, day1 := filterSet(t, sq)
	_, _ = filterSet(t, es)

	queries := []requestlog.Query{
		{},
		{Start: timePtr(day0), End: timePtr(day1)},
		{Start: timePtr(day0.Add(10 * time.Hour))},
		{End: timePtr(day0.Add(10 * time.Hour))},
		{APIKeyHash: "h2"},
		{Model: "m3"},
		{Path: "/v1/chat/completions"},
		{StatusCode: 200},
		{Stream: boolPtr(true)},
		{Stream: boolPtr(false)},
		{MinLatencyMS: 500},
		{MaxLatencyMS: 500},
		{MinLatencyMS: 100, MaxLatencyMS: 500},
		{Q: "gamma"},
		{Q: "resp3"},
		{Page: 2, PageSize: 2},
		{Start: timePtr(day0), End: timePtr(day1), APIKeyHash: "h2", Model: "m2",
			Path: "/v1/chat/completions", StatusCode: 429, Stream: boolPtr(false),
			MinLatencyMS: 100, MaxLatencyMS: 1000, Q: "gamma"},
	}
	for i, q := range queries {
		sqIDs := searchIDs(t, sq, q)
		esIDs := searchIDs(t, es, q)
		sort.Strings(sqIDs)
		sort.Strings(esIDs)
		if !slices.Equal(sqIDs, esIDs) {
			t.Errorf("Query[%d] %+v：sqlite IDs %v != es IDs %v", i, q, sqIDs, esIDs)
		}
	}
}

// ---------------------------------------------------------------------------
// none 实现零开销
// ---------------------------------------------------------------------------

// TestIntegration_NoneIsZeroOverhead 断言 none 在注册表里（仅 contains，不用精确列表，R10），
// 且 Open(none) 返回非 nil 插件、Retention()==0、读写均为空操作。
func TestIntegration_NoneIsZeroOverhead(t *testing.T) {
	ctx := context.Background()
	names := requestlog.Names()
	if !slices.Contains(names, "none") {
		t.Fatalf("Names() = %v，未包含 none", names)
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("Names() 未按字典序返回：%v", names)
	}

	raw, err := requestlog.LoadRawConfig("", "")
	if err != nil {
		t.Fatalf("LoadRawConfig 失败：%v", err)
	}
	if string(raw) != "{}" {
		t.Errorf("空配置 raw = %q，期望 {}", raw)
	}
	p, err := requestlog.Open(ctx, "none", raw)
	if err != nil {
		t.Fatalf("Open(none) 失败：%v", err)
	}
	if p == nil {
		t.Fatal("Open(none) 必须返回非 nil 插件")
	}
	t.Cleanup(func() { _ = p.Close() })
	var _ requestlog.Reader = p // 编译期断言：none 同样满足 Reader
	if p.Name() != "none" {
		t.Errorf("Name() = %q，期望 none", p.Name())
	}
	if got := p.Retention(); got != 0 {
		t.Errorf("Retention() = %v，期望 0（零开销、无清理循环）", got)
	}
	page, err := p.Search(ctx, requestlog.Query{})
	if err != nil {
		t.Fatalf("Search 失败：%v", err)
	}
	if page.Rows == nil || len(page.Rows) != 0 {
		t.Errorf("none Search Rows = %+v，期望非 nil 空切片", page.Rows)
	}
	if _, err := p.Get(ctx, "any"); !errors.Is(err, requestlog.ErrNotFound) {
		t.Errorf("none Get = %v，期望 ErrNotFound", err)
	}
	if err := p.Write(ctx, []requestlog.Entry{makeEntry("none-1", time.Now().UTC())}); err != nil {
		t.Errorf("none Write = %v，期望 nil", err)
	}
}
