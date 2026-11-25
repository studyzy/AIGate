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

// 本文件提供进程内的 mock Elasticsearch 夹具（R9，供 Task 9 的
// TestIntegration_FilterParity 复用），并以黑盒方式驱动 es 插件的全部端点与形状断言。
package requestlog_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/requestlog"
)

// ---------------------------------------------------------------------------
// R9：可复用 mock ES 夹具（供 Task 9 的 TestIntegration_FilterParity 复用）
//
// 用法（integration_test.go，同为 package requestlog_test）：
//
//	m := newMockES(t)          // 启动进程内 mock ES
//	es := openES(t, m, nil)    // Open(ctx,"es", 指向 m.url 的配置) + 自动 Close
//	// 与 sqlite 写入同一组数据（filterSet/makeEntry/searchIDs 在 sqlite_test.go 中）：
//	filterSet(t, sq)
//	filterSet(t, es)
//	gotES := searchIDs(t, es, q)
//	gotSQ := searchIDs(t, sq, q)
//	// 排序后比较集合一致即可
//
// esRawConfig(t, endpoint, opts) 可单独构造配置；openES 只是它的便捷封装。
// mock 在内存中按与 sqlite 相同的过滤语义实现 _search / _delete_by_query，
// 因此同一 Query 的 []Row.ID 集合与 sqlite 一致。
// ---------------------------------------------------------------------------

// esRequest 是 mock ES 捕获到的一次请求（含认证头），供形状与鉴权断言。
type esRequest struct {
	Method string
	Path   string
	Body   []byte
	Header http.Header
}

// mockES 是进程内的假 Elasticsearch：实现 Init(HEAD/PUT)、_bulk、_search、
// _doc、_delete_by_query，并在内存中按与 sqlite 相同的过滤语义响应查询。
// 所有可变状态由 mu 保护，handler 串行处理每个请求，便于 -race 下复用。
type mockES struct {
	server *httptest.Server
	url    string // = server.URL，直接作为 esConfig.Endpoints 的元素

	mu            sync.Mutex
	docs          map[string]map[string]any // _id -> 完整 _source（snake_case）
	reqs          []esRequest
	headStatus    int    // HEAD /<index> 返回码；404 触发建索引，200 跳过（默认 404）
	bulkErrors    bool   // true 时 _bulk 返回 {"errors":true}
	bulkErrType   string // 可选：注入 items[].index.error.type
	bulkErrReason string // 可选：注入 items[].index.error.reason
}

// newMockES 启动 mock ES 并登记 server 关闭清理。
func newMockES(t *testing.T) *mockES {
	t.Helper()
	m := &mockES{
		docs:       map[string]map[string]any{},
		headStatus: http.StatusNotFound,
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.serveHTTP))
	m.url = m.server.URL
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockES) setHeadStatus(v int) {
	m.mu.Lock()
	m.headStatus = v
	m.mu.Unlock()
}

func (m *mockES) setBulkErrors(v bool) {
	m.mu.Lock()
	m.bulkErrors = v
	m.mu.Unlock()
}

// setBulkItemError 注入 _bulk 响应 items[].index.error 的 type/reason，用于验证 I2 的告警摘要。
func (m *mockES) setBulkItemError(typ, reason string) {
	m.mu.Lock()
	m.bulkErrType = typ
	m.bulkErrReason = reason
	m.mu.Unlock()
}

// requests 返回已捕获请求的快照。
func (m *mockES) requests() []esRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]esRequest, len(m.reqs))
	copy(out, m.reqs)
	return out
}

// docCount 返回当前已写入文档数。
func (m *mockES) docCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.docs)
}

func (m *mockES) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	defer m.mu.Unlock()

	m.reqs = append(m.reqs, esRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Body:   append([]byte(nil), body...),
		Header: r.Header.Clone(),
	})

	path := r.URL.Path
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodHead && isESIndexPath(path):
		w.WriteHeader(m.headStatus)
	case r.Method == http.MethodPut && isESIndexPath(path):
		_, _ = w.Write([]byte(`{"acknowledged":true}`))
	case r.Method == http.MethodPost && path == "/_bulk":
		m.handleBulk(w, body)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/_search"):
		m.handleSearch(w, body)
	case r.Method == http.MethodGet && strings.Contains(path, "/_doc/"):
		m.handleGet(w, path)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/_delete_by_query"):
		m.handleDeleteByQuery(w, body)
	default:
		http.Error(w, "unexpected es request: "+r.Method+" "+path, http.StatusInternalServerError)
	}
}

// isESIndexPath 判断是否为单段索引路径（如 /aigate-request-logs）。
func isESIndexPath(p string) bool {
	return len(p) > 1 && p[0] == '/' && !strings.Contains(p[1:], "/")
}

// handleBulk 解析 NDJSON：每两行为 index action + doc，按 _id 存文档。
func (m *mockES) handleBulk(w http.ResponseWriter, body []byte) {
	lines := bytes.Split(bytes.TrimSpace(body), []byte("\n"))
	for i := 0; i+1 < len(lines); i += 2 {
		var action struct {
			Index struct {
				ID string `json:"_id"`
			} `json:"index"`
		}
		if err := json.Unmarshal(lines[i], &action); err != nil {
			http.Error(w, "bad bulk action: "+err.Error(), http.StatusBadRequest)
			return
		}
		var doc map[string]any
		if err := json.Unmarshal(lines[i+1], &doc); err != nil {
			http.Error(w, "bad bulk doc: "+err.Error(), http.StatusBadRequest)
			return
		}
		m.docs[action.Index.ID] = doc
	}
	if m.bulkErrors {
		if m.bulkErrType != "" || m.bulkErrReason != "" {
			resp := map[string]any{
				"errors": true,
				"items": []any{map[string]any{
					"index": map[string]any{
						"status": 400,
						"error": map[string]any{
							"type":   m.bulkErrType,
							"reason": m.bulkErrReason,
						},
					},
				}},
			}
			data, _ := json.Marshal(resp)
			_, _ = w.Write(data)
		} else {
			_, _ = w.Write([]byte(`{"errors":true,"items":[]}`))
		}
	} else {
		_, _ = w.Write([]byte(`{"errors":false,"items":[]}`))
	}
}

// handleSearch 解析 bool.filter / sort / from / size / _source，按与 sqlite 相同的
// 语义过滤、排序、分页、投影后返回标准 hits 结构。
func (m *mockES) handleSearch(w http.ResponseWriter, body []byte) {
	var req struct {
		Query struct {
			Bool struct {
				Filter []map[string]any `json:"filter"`
			} `json:"bool"`
		} `json:"query"`
		Sort           []map[string]any `json:"sort"`
		From           int              `json:"from"`
		Size           int              `json:"size"`
		TrackTotalHits bool             `json:"track_total_hits"`
		Source         []string         `json:"_source"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad search body: "+err.Error(), http.StatusBadRequest)
		return
	}

	matched := make([]map[string]any, 0, len(m.docs))
	for _, doc := range m.docs {
		ok := true
		for _, clause := range req.Query.Bool.Filter {
			if !m.matchClause(doc, clause) {
				ok = false
				break
			}
		}
		if ok {
			matched = append(matched, doc)
		}
	}

	order := "desc"
	if len(req.Sort) > 0 {
		for _, v := range req.Sort[0] {
			switch x := v.(type) {
			case string:
				order = x
			case map[string]any:
				if o, ok := x["order"].(string); ok {
					order = o
				}
			}
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		ti := parseESDocTime(matched[i]["created_at"])
		tj := parseESDocTime(matched[j]["created_at"])
		if !ti.Equal(tj) {
			if order == "asc" {
				return ti.Before(tj)
			}
			return ti.After(tj)
		}
		// 稳定兜底：created_at 相同时按 id DESC，与 sqlite 的 ORDER BY 一致。
		return fmt.Sprint(matched[i]["id"]) > fmt.Sprint(matched[j]["id"])
	})

	total := int64(len(matched))
	start := req.From
	if start < 0 {
		start = 0
	}
	if start > len(matched) {
		start = len(matched)
	}
	end := start + req.Size
	if req.Size < 0 || end > len(matched) {
		end = len(matched)
	}
	pageDocs := matched[start:end]

	hits := make([]map[string]any, 0, len(pageDocs))
	for _, doc := range pageDocs {
		src := map[string]any{}
		if len(req.Source) == 0 {
			for k, v := range doc {
				src[k] = v
			}
		} else {
			for _, k := range req.Source {
				if v, ok := doc[k]; ok {
					src[k] = v
				}
			}
		}
		hits = append(hits, map[string]any{"_source": src})
	}
	data, _ := json.Marshal(map[string]any{
		"hits": map[string]any{
			"total": map[string]any{"value": total, "relation": "eq"},
			"hits":  hits,
		},
	})
	_, _ = w.Write(data)
}

// matchClause 解释一个 bool.filter 子句（range / term / multi_match）。
func (m *mockES) matchClause(doc map[string]any, clause map[string]any) bool {
	if r, ok := clause["range"].(map[string]any); ok {
		for field, raw := range r {
			bounds, _ := raw.(map[string]any)
			switch field {
			case "created_at":
				t := parseESDocTime(doc["created_at"])
				if gte, ok := bounds["gte"].(string); ok {
					if t.Before(parseESStringTime(gte)) {
						return false
					}
				}
				if lt, ok := bounds["lt"].(string); ok {
					if !t.Before(parseESStringTime(lt)) {
						return false
					}
				}
				if lte, ok := bounds["lte"].(string); ok {
					if t.After(parseESStringTime(lte)) {
						return false
					}
				}
			case "latency_ms":
				got, _ := toESFloat(doc["latency_ms"])
				if gte, ok := bounds["gte"]; ok {
					b, _ := toESFloat(gte)
					if got < b {
						return false
					}
				}
				if lte, ok := bounds["lte"]; ok {
					b, _ := toESFloat(lte)
					if got > b {
						return false
					}
				}
				if lt, ok := bounds["lt"]; ok {
					b, _ := toESFloat(lt)
					if got >= b {
						return false
					}
				}
			}
		}
		return true
	}
	if t, ok := clause["term"].(map[string]any); ok {
		for field, want := range t {
			if !reflect.DeepEqual(doc[field], want) {
				return false
			}
		}
		return true
	}
	if mm, ok := clause["multi_match"].(map[string]any); ok {
		q, _ := mm["query"].(string)
		lq := strings.ToLower(q)
		fields, _ := mm["fields"].([]any)
		for _, f := range fields {
			key, _ := f.(string)
			if strings.Contains(strings.ToLower(fmt.Sprint(doc[key])), lq) {
				return true
			}
		}
		return false
	}
	return true // 未知子句忽略，避免夹具误伤
}

func (m *mockES) handleGet(w http.ResponseWriter, path string) {
	idx := strings.LastIndex(path, "/_doc/")
	id := path[idx+len("/_doc/"):]
	doc, ok := m.docs[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"found":false}`))
		return
	}
	data, _ := json.Marshal(map[string]any{"found": true, "_id": id, "_source": doc})
	_, _ = w.Write(data)
}

func (m *mockES) handleDeleteByQuery(w http.ResponseWriter, body []byte) {
	var req struct {
		Query struct {
			Range struct {
				CreatedAt struct {
					LT string `json:"lt"`
				} `json:"created_at"`
			} `json:"range"`
		} `json:"query"`
	}
	_ = json.Unmarshal(body, &req)
	before := parseESStringTime(req.Query.Range.CreatedAt.LT)
	var deleted int64
	for id, doc := range m.docs {
		if parseESDocTime(doc["created_at"]).Before(before) {
			delete(m.docs, id)
			deleted++
		}
	}
	data, _ := json.Marshal(map[string]any{"deleted": deleted})
	_, _ = w.Write(data)
}

func parseESDocTime(v any) time.Time { return parseESStringTime(fmt.Sprint(v)) }

func parseESStringTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func toESFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, _ := x.Float64()
		return f, true
	}
	return 0, false
}

// esRawConfig 构造 es 插件的 Init raw 配置：默认仅 endpoints=[endpoint]，可被 opts 覆盖。
func esRawConfig(t *testing.T, endpoint string, opts map[string]any) json.RawMessage {
	t.Helper()
	m := map[string]any{"endpoints": []string{endpoint}}
	for k, v := range opts {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal es config: %v", err)
	}
	return b
}

// openES 用 esRawConfig 打开 "es" 插件并登记 Close 清理；Task 9 的 FilterParity 直接复用。
func openES(t *testing.T, m *mockES, opts map[string]any) requestlog.Plugin {
	t.Helper()
	p, err := requestlog.Open(context.Background(), "es", esRawConfig(t, m.url, opts))
	if err != nil {
		t.Fatalf("Open(es) 失败：%v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

// TestES_RegisteredAndConfig：R10（Names 含 es，不写精确列表）、默认值、自定义保留期、
// HEAD 200 跳过建索引（R20），以及各类非法配置被 Init 拒绝。
func TestES_RegisteredAndConfig(t *testing.T) {
	names := requestlog.Names()
	if !slices.Contains(names, "es") {
		t.Fatalf("Names() = %v，未包含 es", names)
	}

	t.Run("defaults", func(t *testing.T) {
		m := newMockES(t)
		p := openES(t, m, nil)
		if got := p.Name(); got != "es" {
			t.Errorf("Name() = %q，期望 es", got)
		}
		if got := p.Retention(); got != 7*24*time.Hour {
			t.Errorf("默认 Retention() = %v，期望 7d", got)
		}
		var _ requestlog.Reader = p // 编译期断言：Task 9 会把同一实例当 Reader 用
	})

	t.Run("custom retention and existing index skips PUT", func(t *testing.T) {
		m := newMockES(t)
		m.setHeadStatus(http.StatusOK)
		p := openES(t, m, map[string]any{"retention_days": 30})
		if got := p.Retention(); got != 30*24*time.Hour {
			t.Errorf("自定义 Retention() = %v，期望 30d", got)
		}
		foundHead := false
		for _, r := range m.requests() {
			if r.Method == http.MethodHead && isESIndexPath(r.Path) {
				foundHead = true
			}
			if r.Method == http.MethodPut && isESIndexPath(r.Path) {
				t.Errorf("HEAD 200 时不应发送 PUT 建索引，实得 %s %s", r.Method, r.Path)
			}
		}
		if !foundHead {
			t.Error("Init 应发送 HEAD /<index> 探测")
		}
	})

	t.Run("invalid configs rejected", func(t *testing.T) {
		m := newMockES(t)
		cases := []struct {
			name string
			raw  json.RawMessage
		}{
			{"empty endpoints", json.RawMessage(`{"endpoints":[]}`)},
			{"password and api_key", esRawConfig(t, m.url, map[string]any{"password": "p", "api_key": "k"})},
			{"invalid timeout", esRawConfig(t, m.url, map[string]any{"timeout": "not-a-duration"})},
			{"retention too large", esRawConfig(t, m.url, map[string]any{"retention_days": 36501})},
			{"retention negative", esRawConfig(t, m.url, map[string]any{"retention_days": -1})},
			{"invalid endpoint url", esRawConfig(t, "not-a-url", nil)},
			{"invalid json", json.RawMessage(`{"endpoints":`)},
			{"non-object json", json.RawMessage(`[]`)},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				p, err := requestlog.Open(context.Background(), "es", c.raw)
				if err == nil {
					_ = p.Close()
					t.Fatal("非法配置应被拒绝，却成功")
				}
				if p != nil {
					t.Error("Init 失败时不应返回插件")
				}
				if !strings.Contains(err.Error(), "init request log plugin") {
					t.Errorf("应由 Init 拒绝（而非实现未注册）：%v", err)
				}
			})
		}
	})
}

// TestES_InitCreatesMapping：HEAD 404 后 PUT /<index> 的 mapping 字段类型断言。
func TestES_InitCreatesMapping(t *testing.T) {
	m := newMockES(t) // 默认 HEAD 404
	openES(t, m, nil)

	var put *esRequest
	for _, r := range m.requests() {
		if r.Method == http.MethodPut && isESIndexPath(r.Path) {
			rr := r
			put = &rr
		}
	}
	if put == nil {
		t.Fatal("HEAD 404 后应发送 PUT /<index> 建索引")
	}
	if put.Path != "/aigate-request-logs" {
		t.Errorf("PUT 路径 = %q，期望 /aigate-request-logs", put.Path)
	}
	var body struct {
		Mappings struct {
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
		} `json:"mappings"`
	}
	if err := json.Unmarshal(put.Body, &body); err != nil {
		t.Fatalf("mapping 不是合法 JSON：%v", err)
	}
	props := body.Mappings.Properties
	want := map[string]string{
		"id": "keyword", "created_at": "date_nanos",
		"method": "keyword", "path": "keyword", "model": "keyword",
		"api_key_hash": "keyword", "status_code": "integer",
		"latency_ms": "integer", "stream": "boolean", "truncated": "boolean",
		"request_body": "text", "response_body": "text",
		"request_headers": "text", "response_headers": "text", "error": "text",
	}
	for field, typ := range want {
		got, ok := props[field]
		if !ok {
			t.Errorf("mapping 缺少字段 %s", field)
			continue
		}
		if got.Type != typ {
			t.Errorf("mapping %s.type = %q，期望 %q", field, got.Type, typ)
		}
	}
}

// TestES_BulkShape：_bulk NDJSON 成对 action+doc、_id 等于 Entry.ID、snake_case 正文键，
// 以及 errors:true 时仅告警不返回错误。
func TestES_BulkShape(t *testing.T) {
	m := newMockES(t)
	p := openES(t, m, nil)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	e1 := makeEntry("bulk-1", t0)
	e2 := makeEntry("bulk-2", t0.Add(time.Minute))
	if err := p.Write(ctx, []requestlog.Entry{e1, e2}); err != nil {
		t.Fatalf("Write 失败：%v", err)
	}
	if got := m.docCount(); got != 2 {
		t.Errorf("mock 存储文档数 = %d，期望 2", got)
	}

	var bulk *esRequest
	for _, r := range m.requests() {
		if r.Method == http.MethodPost && r.Path == "/_bulk" {
			rr := r
			bulk = &rr
		}
	}
	if bulk == nil {
		t.Fatal("Write 应发送 POST /_bulk")
	}
	lines := bytes.Split(bytes.TrimSpace(bulk.Body), []byte("\n"))
	if len(lines) != 4 {
		t.Fatalf("_bulk NDJSON 行数 = %d，期望 4（2 对 action+doc）", len(lines))
	}
	for i, wantID := range []string{"bulk-1", "bulk-2"} {
		var action struct {
			Index struct {
				Index string `json:"_index"`
				ID    string `json:"_id"`
			} `json:"index"`
		}
		if err := json.Unmarshal(lines[2*i], &action); err != nil {
			t.Fatalf("action 行 %d 解析失败：%v", 2*i, err)
		}
		if action.Index.ID != wantID {
			t.Errorf("action %d _id = %q，期望 %q", i, action.Index.ID, wantID)
		}
		if action.Index.Index != "aigate-request-logs" {
			t.Errorf("action %d _index = %q", i, action.Index.Index)
		}
		var doc map[string]any
		if err := json.Unmarshal(lines[2*i+1], &doc); err != nil {
			t.Fatalf("doc 行 %d 解析失败：%v", 2*i+1, err)
		}
		if doc["id"] != wantID {
			t.Errorf("doc %d id = %v，期望 %q", i, doc["id"], wantID)
		}
		// R19：文档用 snake_case 键，时间与正文列存在。
		for _, key := range []string{"created_at", "request_body", "response_body", "api_key_hash", "latency_ms"} {
			if _, ok := doc[key]; !ok {
				t.Errorf("doc %d 缺少 snake_case 键 %s", i, key)
			}
		}
	}

	// errors:true 只告警、不返回错误。
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)
	m.setBulkErrors(true)
	if err := p.Write(ctx, []requestlog.Entry{makeEntry("bulk-3", t0.Add(2*time.Minute))}); err != nil {
		t.Errorf("errors:true 时 Write 不应返回错误，实得 %v", err)
	}
	if !strings.Contains(buf.String(), "bulk") {
		t.Errorf("errors:true 时应产生告警日志，实得 %q", buf.String())
	}
}

// TestES_SearchFilters：bool.filter 映射全部 Query 字段、sort / from / size /
// track_total_hits / _source 形状，以及 _source → Row 的回读映射（R19/R8/R15）。
func TestES_SearchFilters(t *testing.T) {
	m := newMockES(t)
	p := openES(t, m, nil)
	ctx := context.Background()
	base := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	e1 := makeEntry("s1", base.Add(time.Hour))
	e2 := makeEntry("s2", base.Add(2*time.Hour))
	e2.Model, e2.StatusCode, e2.LatencyMS, e2.Stream = "m-special", 429, 250, true
	e3 := makeEntry("s3", base.Add(3*time.Hour))
	if err := p.Write(ctx, []requestlog.Entry{e1, e2, e3}); err != nil {
		t.Fatalf("Write 失败：%v", err)
	}

	// 无过滤条件：验证 _source → Row 映射与 hits.total.value（R19）。
	all, err := p.Search(ctx, requestlog.Query{})
	if err != nil {
		t.Fatalf("Search 失败：%v", err)
	}
	if all.Total != 3 || len(all.Rows) != 3 {
		t.Fatalf("无过滤结果 Total/Rows = %d/%d，期望 3/3", all.Total, len(all.Rows))
	}
	if all.Page != 1 || all.PageSize != 50 {
		t.Errorf("默认分页回显 = %d/%d，期望 1/50", all.Page, all.PageSize)
	}
	gotIDs := []string{all.Rows[0].ID, all.Rows[1].ID, all.Rows[2].ID}
	if !slices.Equal(gotIDs, []string{"s3", "s2", "s1"}) {
		t.Errorf("默认排序 IDs = %v，期望 [s3 s2 s1]", gotIDs)
	}
	row := all.Rows[1] // s2
	if row.Model != "m-special" || row.StatusCode != 429 || row.LatencyMS != 250 || !row.Stream ||
		!row.CreatedAt.Equal(e2.CreatedAt) || row.APIKeyHash != e2.APIKeyHash {
		t.Errorf("_source → Row 字段映射不一致：%+v", row)
	}

	start := base
	end := base.AddDate(0, 0, 1)
	q := requestlog.Query{
		Start:        &start,
		End:          &end,
		APIKeyHash:   "hash-x",
		Model:        "m-special",
		Path:         "/v1/chat/completions",
		StatusCode:   429,
		Stream:       boolPtr(true),
		MinLatencyMS: 100,
		MaxLatencyMS: 500,
		Q:            "needle",
		Page:         2,
		PageSize:     5,
	}
	page, err := p.Search(ctx, q)
	if err != nil {
		t.Fatalf("Search 全筛选失败：%v", err)
	}
	if page.Rows == nil {
		t.Fatal("Rows 应为非 nil")
	}
	if page.Page != 2 || page.PageSize != 5 {
		t.Errorf("分页回显 = %d/%d，期望 2/5", page.Page, page.PageSize)
	}

	var search *esRequest
	for _, r := range m.requests() {
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/_search") {
			// 取最后一次（全筛选）请求。
			rr := r
			search = &rr
		}
	}
	if search == nil {
		t.Fatal("Search 应发送 POST /<index>/_search")
	}
	if !json.Valid(search.Body) {
		t.Fatal("_search 请求体不是合法 JSON")
	}
	var body struct {
		Query struct {
			Bool struct {
				Filter []map[string]any `json:"filter"`
			} `json:"bool"`
		} `json:"query"`
		Sort           []map[string]any `json:"sort"`
		From           int              `json:"from"`
		Size           int              `json:"size"`
		TrackTotalHits bool             `json:"track_total_hits"`
		Source         []string         `json:"_source"`
	}
	if err := json.Unmarshal(search.Body, &body); err != nil {
		t.Fatalf("解析 _search 请求体失败：%v", err)
	}

	filters := body.Query.Bool.Filter
	assertHasTerm := func(field string, want any) {
		t.Helper()
		for _, f := range filters {
			if term, ok := f["term"].(map[string]any); ok {
				if v, ok := term[field]; ok && reflect.DeepEqual(v, want) {
					return
				}
			}
		}
		t.Errorf("bool.filter 缺少 term.%s = %v", field, want)
	}
	assertHasTerm("api_key_hash", "hash-x")
	assertHasTerm("model", "m-special")
	assertHasTerm("path", "/v1/chat/completions")
	assertHasTerm("status_code", float64(429))
	assertHasTerm("stream", true)

	foundRange := false
	foundLatency := false
	for _, f := range filters {
		if r, ok := f["range"].(map[string]any); ok {
			if ca, ok := r["created_at"].(map[string]any); ok {
				foundRange = true
				if ca["gte"] != start.UTC().Format(time.RFC3339Nano) {
					t.Errorf("created_at.gte = %v", ca["gte"])
				}
				if ca["lt"] != end.UTC().Format(time.RFC3339Nano) {
					t.Errorf("created_at.lt = %v", ca["lt"])
				}
				if _, hasLTE := ca["lte"]; hasLTE {
					t.Error("created_at 应为半开区间（lt，而非 lte）")
				}
			}
			if lm, ok := r["latency_ms"].(map[string]any); ok {
				foundLatency = true
				if lm["gte"] != float64(100) || lm["lte"] != float64(500) {
					t.Errorf("latency_ms range = %v，期望 gte=100 lte=500", lm)
				}
			}
		}
	}
	if !foundRange {
		t.Error("bool.filter 缺少 range.created_at")
	}
	if !foundLatency {
		t.Error("bool.filter 缺少 range.latency_ms")
	}

	// R22：multi_match 字面量，不得使用 query_string。
	foundMM := false
	for _, f := range filters {
		if mm, ok := f["multi_match"].(map[string]any); ok {
			foundMM = true
			if mm["query"] != "needle" {
				t.Errorf("multi_match.query = %v，期望字面量 needle", mm["query"])
			}
		}
		if _, ok := f["query_string"]; ok {
			t.Error("Q 不得使用 query_string")
		}
	}
	if !foundMM {
		t.Error("bool.filter 缺少 multi_match（Q）")
	}

	// I1：主键 created_at DESC + 次级键 id DESC，与 sqlite 的 ORDER BY 对齐，
	// 保证 created_at 精度相同时 from/size 分页稳定、不重复/丢行。
	if len(body.Sort) != 2 {
		t.Fatalf("sort 子句数 = %d，期望 2（created_at desc + id desc 次级键）", len(body.Sort))
	}
	assertSortDesc := func(i int, field string) {
		t.Helper()
		clause, ok := body.Sort[i][field]
		if !ok {
			t.Errorf("sort[%d] 应为 %s：%v", i, field, body.Sort[i])
			return
		}
		switch v := clause.(type) {
		case string:
			if v != "desc" {
				t.Errorf("sort[%d] %s = %q，期望 desc", i, field, v)
			}
		case map[string]any:
			if v["order"] != "desc" {
				t.Errorf("sort[%d] %s.order = %v，期望 desc", i, field, v["order"])
			}
		default:
			t.Errorf("sort[%d] %s 形式异常：%T", i, field, clause)
		}
	}
	assertSortDesc(0, "created_at")
	assertSortDesc(1, "id")

	if body.From != 5 {
		t.Errorf("from = %d，期望 (2-1)*5=5", body.From)
	}
	if body.Size != 5 {
		t.Errorf("size = %d，期望 5", body.Size)
	}
	if !body.TrackTotalHits {
		t.Error("track_total_hits 应为 true")
	}
	wantCols := []string{"id", "created_at", "method", "path", "model", "api_key_hash",
		"status_code", "latency_ms", "stream", "error", "truncated"}
	for _, col := range wantCols {
		if !slices.Contains(body.Source, col) {
			t.Errorf("_source 缺少列 %s：%v", col, body.Source)
		}
	}
	for _, col := range []string{"request_body", "response_body", "request_headers", "response_headers"} {
		if slices.Contains(body.Source, col) {
			t.Errorf("_source 不应包含正文列 %s", col)
		}
	}
}

// TestES_SearchResultsAreFiltered：用与 sqlite 相同的 filterSet 数据集，验证
// es 插件 + mock 夹具的读取语义（term / range / Q / 分页）与预期一致；
// 同时佐证 R9 夹具的过滤语义正确，可供 Task 9 的 FilterParity 复用。
func TestES_SearchResultsAreFiltered(t *testing.T) {
	m := newMockES(t)
	p := openES(t, m, nil)
	day0, day1 := filterSet(t, p)

	assertIDs(t, p, requestlog.Query{}, "e4", "e3", "e2", "e1", "e0")
	assertIDs(t, p, requestlog.Query{Start: timePtr(day0), End: timePtr(day1)}, "e3", "e2", "e1")
	assertIDs(t, p, requestlog.Query{Start: timePtr(day0.Add(10 * time.Hour))}, "e4", "e3", "e2")
	assertIDs(t, p, requestlog.Query{End: timePtr(day0.Add(10 * time.Hour))}, "e1", "e0")
	assertIDs(t, p, requestlog.Query{APIKeyHash: "h2"}, "e2")
	assertIDs(t, p, requestlog.Query{Model: "m3"}, "e3")
	assertIDs(t, p, requestlog.Query{Path: "/v1/chat/completions"}, "e4", "e2", "e0")
	assertIDs(t, p, requestlog.Query{StatusCode: 200}, "e4", "e0")
	assertIDs(t, p, requestlog.Query{Stream: boolPtr(true)}, "e3", "e1")
	assertIDs(t, p, requestlog.Query{Stream: boolPtr(false)}, "e4", "e2", "e0")
	assertIDs(t, p, requestlog.Query{MinLatencyMS: 500}, "e4", "e3", "e2")
	assertIDs(t, p, requestlog.Query{MaxLatencyMS: 500}, "e2", "e1", "e0")
	assertIDs(t, p, requestlog.Query{MinLatencyMS: 100, MaxLatencyMS: 500}, "e2", "e1")
	assertIDs(t, p, requestlog.Query{Q: "gamma"}, "e2")
	assertIDs(t, p, requestlog.Query{Q: "resp3"}, "e3")
	assertIDs(t, p, requestlog.Query{Page: 2, PageSize: 2}, "e2", "e1")
	assertIDs(t, p, requestlog.Query{
		Start: timePtr(day0), End: timePtr(day1), APIKeyHash: "h2", Model: "m2",
		Path: "/v1/chat/completions", StatusCode: 429, Stream: boolPtr(false),
		MinLatencyMS: 100, MaxLatencyMS: 1000, Q: "gamma",
	}, "e2")
}

// TestES_QIsEscaped（Review Focus 4 / R22）：Q 含 ES 保留字符时请求体仍是合法 JSON，
// 且作为 multi_match 的字面量，不产生语法错误。
func TestES_QIsEscaped(t *testing.T) {
	m := newMockES(t)
	p := openES(t, m, nil)
	reserved := `+ - && || ! ( ) { } [ ] ^ " ~ * ? : \ /`
	if _, err := p.Search(context.Background(), requestlog.Query{Q: reserved}); err != nil {
		t.Fatalf("含保留字符的 Q 不应报错：%v", err)
	}
	var search *esRequest
	for _, r := range m.requests() {
		if strings.HasSuffix(r.Path, "/_search") {
			rr := r
			search = &rr
		}
	}
	if search == nil {
		t.Fatal("Search 应发送 _search 请求")
	}
	if !json.Valid(search.Body) {
		t.Fatal("请求体必须是合法 JSON")
	}
	var body struct {
		Query struct {
			Bool struct {
				Filter []map[string]any `json:"filter"`
			} `json:"bool"`
		} `json:"query"`
	}
	if err := json.Unmarshal(search.Body, &body); err != nil {
		t.Fatalf("解析请求体失败：%v", err)
	}
	foundMM := false
	for _, f := range body.Query.Bool.Filter {
		if _, ok := f["query_string"]; ok {
			t.Fatal("Q 不得使用 query_string")
		}
		if mm, ok := f["multi_match"].(map[string]any); ok {
			foundMM = true
			if mm["query"] != reserved {
				t.Errorf("multi_match.query 必须原样保留保留字符，实得 %q", mm["query"])
			}
		}
	}
	if !foundMM {
		t.Error("缺少 multi_match 子句")
	}
}

// TestES_GetAndPrune：GET 404 → ErrNotFound、命中返回完整 Entry；
// Prune → POST /_delete_by_query 带 range created_at < before 并解析 deleted。
func TestES_GetAndPrune(t *testing.T) {
	m := newMockES(t)
	p := openES(t, m, nil)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	e1 := makeEntry("get-1", t0)
	e2 := makeEntry("get-2", t0.Add(2*time.Hour))
	if err := p.Write(ctx, []requestlog.Entry{e1, e2}); err != nil {
		t.Fatalf("Write 失败：%v", err)
	}

	got, err := p.Get(ctx, "get-2")
	if err != nil {
		t.Fatalf("Get 失败：%v", err)
	}
	assertEntryEqual(t, *got, e2)
	var getReq *esRequest
	for _, r := range m.requests() {
		if r.Method == http.MethodGet && strings.Contains(r.Path, "/_doc/") {
			rr := r
			getReq = &rr
		}
	}
	if getReq == nil {
		t.Fatal("Get 应发送 GET /<index>/_doc/<id>")
	}
	if getReq.Path != "/aigate-request-logs/_doc/get-2" {
		t.Errorf("GET 路径 = %q", getReq.Path)
	}

	if _, err := p.Get(ctx, "missing-id"); !errors.Is(err, requestlog.ErrNotFound) {
		t.Errorf("Get 不存在应返回 ErrNotFound，实得 %v", err)
	}

	before := t0.Add(time.Hour)
	n, err := p.Prune(ctx, before)
	if err != nil {
		t.Fatalf("Prune 失败：%v", err)
	}
	if n != 1 {
		t.Errorf("Prune deleted = %d，期望 1（仅 get-1 早于 before）", n)
	}
	var pruneReq *esRequest
	for _, r := range m.requests() {
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/_delete_by_query") {
			rr := r
			pruneReq = &rr
		}
	}
	if pruneReq == nil {
		t.Fatal("Prune 应发送 POST /<index>/_delete_by_query")
	}
	var pruneBody struct {
		Query struct {
			Range struct {
				CreatedAt struct {
					LT string `json:"lt"`
				} `json:"created_at"`
			} `json:"range"`
		} `json:"query"`
	}
	if err := json.Unmarshal(pruneReq.Body, &pruneBody); err != nil {
		t.Fatalf("解析 prune 请求体失败：%v", err)
	}
	if pruneBody.Query.Range.CreatedAt.LT != before.UTC().Format(time.RFC3339Nano) {
		t.Errorf("range.created_at.lt = %q，期望 %q", pruneBody.Query.Range.CreatedAt.LT, before.UTC().Format(time.RFC3339Nano))
	}
	if _, err := p.Get(ctx, "get-1"); !errors.Is(err, requestlog.ErrNotFound) {
		t.Errorf("Prune 后 get-1 应不存在，实得 %v", err)
	}
	if got := m.docCount(); got != 1 {
		t.Errorf("Prune 后剩余文档 = %d，期望 1", got)
	}
}

// TestES_AuthHeader：api_key → ApiKey 头；basic → Basic 头；且请求体/日志不出现密码明文。
func TestES_AuthHeader(t *testing.T) {
	t.Run("api_key", func(t *testing.T) {
		m := newMockES(t)
		p := openES(t, m, map[string]any{"api_key": "sk-secret-123"})
		if err := p.Write(context.Background(), []requestlog.Entry{makeEntry("a1", time.Now().UTC())}); err != nil {
			t.Fatalf("Write 失败：%v", err)
		}
		for _, r := range m.requests() {
			if got := r.Header.Get("Authorization"); got != "ApiKey sk-secret-123" {
				t.Errorf("Authorization = %q，期望 ApiKey sk-secret-123", got)
			}
			if bytes.Contains(r.Body, []byte("sk-secret-123")) {
				t.Error("请求体不应包含 api_key 明文")
			}
		}
	})
	t.Run("basic", func(t *testing.T) {
		m := newMockES(t)
		p := openES(t, m, map[string]any{"username": "u1", "password": "p-secret-456"})
		if err := p.Write(context.Background(), []requestlog.Entry{makeEntry("b1", time.Now().UTC())}); err != nil {
			t.Fatalf("Write 失败：%v", err)
		}
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("u1:p-secret-456"))
		for _, r := range m.requests() {
			if got := r.Header.Get("Authorization"); got != want {
				t.Errorf("Authorization = %q，期望 %q", got, want)
			}
			if bytes.Contains(r.Body, []byte("p-secret-456")) {
				t.Error("请求体不应包含 password 明文")
			}
		}
	})
}

// TestES_UsesFirstEndpoint：R21 —— 多 endpoint 时只使用第一个（第二个不可达也不影响）。
func TestES_UsesFirstEndpoint(t *testing.T) {
	m := newMockES(t)
	raw := esRawConfig(t, m.url, map[string]any{"endpoints": []string{m.url, "http://127.0.0.1:1"}})
	p, err := requestlog.Open(context.Background(), "es", raw)
	if err != nil {
		t.Fatalf("Open(es) 失败：%v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if err := p.Write(context.Background(), []requestlog.Entry{makeEntry("first", time.Now().UTC())}); err != nil {
		t.Fatalf("Write 失败：%v", err)
	}
	if got := m.docCount(); got != 1 {
		t.Errorf("请求应打到第一个 endpoint，mock 文档数 = %d，期望 1", got)
	}
}

// TestES_CloseIsIdempotent：R18 —— Close 幂等，关闭后 Write/Prune/Search/Get 返回良性错误。
func TestES_CloseIsIdempotent(t *testing.T) {
	ctx := context.Background()
	m := newMockES(t)
	p, err := requestlog.Open(ctx, "es", esRawConfig(t, m.url, nil))
	if err != nil {
		t.Fatalf("Open(es) 失败：%v", err)
	}
	if err := p.Write(ctx, []requestlog.Entry{makeEntry("x", time.Now().UTC())}); err != nil {
		t.Fatalf("Write 失败：%v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("首次 Close 失败：%v", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("重复 Close 应为幂等空操作，实得 %v", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("第三次 Close 应为幂等空操作，实得 %v", err)
	}
	if err := p.Write(ctx, []requestlog.Entry{makeEntry("y", time.Now().UTC())}); err == nil {
		t.Error("关闭后 Write 应返回良性错误")
	}
	if _, err := p.Prune(ctx, time.Now().UTC()); err == nil {
		t.Error("关闭后 Prune 应返回良性错误")
	}
	if _, err := p.Search(ctx, requestlog.Query{}); err == nil {
		t.Error("关闭后 Search 应返回良性错误")
	}
	if _, err := p.Get(ctx, "x"); err == nil {
		t.Error("关闭后 Get 应返回良性错误")
	}
}

// TestES_CloseConcurrentWithWrite：R18 —— 并发 Close / Write 不 panic。
func TestES_CloseConcurrentWithWrite(t *testing.T) {
	m := newMockES(t)
	p := openES(t, m, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := makeEntry(fmt.Sprintf("concurrent-%d", i), time.Now().UTC())
			_ = p.Write(context.Background(), []requestlog.Entry{e})
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := p.Close(); err != nil {
			t.Errorf("并发 Close 失败：%v", err)
		}
	}()
	wg.Wait()
	if err := p.Close(); err != nil {
		t.Errorf("并发结束后再次 Close 仍应幂等：%v", err)
	}
}

// TestES_CreatedAtPrecisionIsConsistent（I1(b)）：写入的 created_at 精度必须与 mapping
// 一致。ES 用 date_nanos 映射并以 RFC3339Nano（纳秒精度）写入 _bulk、_source 回读同样解析，
// 避免 date（毫秒）映射把纳秒写值截断后产生大量 created_at 相同的时间点。
func TestES_CreatedAtPrecisionIsConsistent(t *testing.T) {
	m := newMockES(t)
	p := openES(t, m, nil)
	// 带亚毫秒（纳秒）分量的时刻，用来验证写值精度不被截断。
	ts := time.Date(2026, 11, 25, 8, 12, 34, 123456789, time.UTC)
	if err := p.Write(context.Background(), []requestlog.Entry{makeEntry("prec-1", ts)}); err != nil {
		t.Fatalf("Write 失败：%v", err)
	}

	// mapping 必须是 date_nanos，才能承载 RFC3339Nano 的纳秒精度。
	var put *esRequest
	for _, r := range m.requests() {
		if r.Method == http.MethodPut && isESIndexPath(r.Path) {
			rr := r
			put = &rr
		}
	}
	if put == nil {
		t.Fatal("HEAD 404 后应发送 PUT /<index> 建索引")
	}
	var mapping struct {
		Mappings struct {
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
		} `json:"mappings"`
	}
	if err := json.Unmarshal(put.Body, &mapping); err != nil {
		t.Fatalf("mapping 不是合法 JSON：%v", err)
	}
	if got := mapping.Mappings.Properties["created_at"].Type; got != "date_nanos" {
		t.Errorf("created_at mapping = %q，期望 date_nanos（与纳秒写值一致）", got)
	}

	// _bulk 文档的 created_at 必须是 RFC3339Nano 纳秒精度，与 date_nanos 映射一致。
	var bulk *esRequest
	for _, r := range m.requests() {
		if r.Method == http.MethodPost && r.Path == "/_bulk" {
			rr := r
			bulk = &rr
		}
	}
	if bulk == nil {
		t.Fatal("Write 应发送 POST /_bulk")
	}
	lines := bytes.Split(bytes.TrimSpace(bulk.Body), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("_bulk NDJSON 行数 = %d，期望 2（1 对 action+doc）", len(lines))
	}
	var doc map[string]any
	if err := json.Unmarshal(lines[1], &doc); err != nil {
		t.Fatalf("解析 _bulk doc 行失败：%v", err)
	}
	got, _ := doc["created_at"].(string)
	want := ts.UTC().Format(time.RFC3339Nano)
	if got != want {
		t.Errorf("写入的 created_at = %q，期望 %q（RFC3339Nano 纳秒精度）", got, want)
	}
}

// TestES_BulkItemErrorsAreLogged（I2）：_bulk errors:true 且带 items[].index.error 时，
// 告警日志须含解析出的（截断）reason/type 摘要，且 Write 仍只告警、不返回错误、不重试。
func TestES_BulkItemErrorsAreLogged(t *testing.T) {
	capture := func(t *testing.T, reason string) (string, error) {
		t.Helper()
		m := newMockES(t)
		p := openES(t, m, nil)
		m.setBulkErrors(true)
		m.setBulkItemError("mapper_parsing_exception", reason)
		var buf bytes.Buffer
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
		defer slog.SetDefault(old)
		err := p.Write(context.Background(), []requestlog.Entry{makeEntry("be-1", time.Now().UTC())})
		return buf.String(), err
	}

	t.Run("reason and type included", func(t *testing.T) {
		out, err := capture(t, "failed to parse field [created_at]")
		if err != nil {
			t.Fatalf("errors:true + item error 时 Write 不应返回错误，实得 %v", err)
		}
		for _, want := range []string{"bulk", "mapper_parsing_exception", "failed to parse field [created_at]"} {
			if !strings.Contains(out, want) {
				t.Errorf("告警日志应含 %q，实得 %q", want, out)
			}
		}
	})

	t.Run("long reason truncated", func(t *testing.T) {
		long := strings.Repeat("E", 1000)
		out, err := capture(t, long)
		if err != nil {
			t.Fatalf("errors:true + item error 时 Write 不应返回错误，实得 %v", err)
		}
		if !strings.Contains(out, "first_error") {
			t.Errorf("告警日志应含 first_error 结构化字段，实得 %q", out)
		}
		if strings.Contains(out, long) {
			t.Error("过长 reason 应被截断，不应整段进入日志")
		}
	})
}

// TestES_ConfigValidationStrictness（I3 + T1）：非正 timeout 与未知配置键必须在 Init
// 阶段被拒绝（英文错误），且拒绝前不得访问 ES。
func TestES_ConfigValidationStrictness(t *testing.T) {
	m := newMockES(t)
	cases := []struct {
		name    string
		opts    map[string]any
		wantErr string
	}{
		{"zero timeout", map[string]any{"timeout": "0s"}, "es timeout must be positive"},
		{"negative timeout", map[string]any{"timeout": "-1s"}, "es timeout must be positive"},
		{"unknown key", map[string]any{"retention_day": 3}, "unknown field"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := len(m.requests())
			p, err := requestlog.Open(context.Background(), "es", esRawConfig(t, m.url, c.opts))
			if err == nil {
				_ = p.Close()
				t.Fatal("非法配置应被拒绝，却成功")
			}
			if p != nil {
				t.Error("Init 失败时不应返回插件")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("错误信息 %q 未包含 %q", err.Error(), c.wantErr)
			}
			if got := len(m.requests()); got != before {
				t.Errorf("非法配置不应访问 ES，实得新增 %d 个请求", got-before)
			}
		})
	}
}
