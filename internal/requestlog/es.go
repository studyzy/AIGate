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

// 本文件实现基于标准库 net/http 的 Elasticsearch 请求日志插件：不引入
// go-elasticsearch 等第三方依赖，只用 net/http + encoding/json 手写认证、
// 索引 mapping、_bulk / _search / _doc / _delete_by_query 请求。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// defaultESIndex 是未配置 index 时使用的默认索引名。
	defaultESIndex = "aigate-request-logs"
	// defaultESRetentionDays 是未配置 retention_days 时的默认保留天数。
	defaultESRetentionDays = 7
	// defaultESTimeout 是未配置 timeout 时的默认 HTTP 客户端超时。
	defaultESTimeout = "5s"
	// maxESRetentionDays 与 sqlite 实现一致（internal/config 的 36500，即 100 年）：
	// 挡住 time.Duration(days)*24*time.Hour 溢出为负数而被当成「永不清理」。
	maxESRetentionDays = 36500
	// defaultESPageSize 与 queryparams.DefaultPageSize 一致，供零值 Query 兜底。
	defaultESPageSize = 50
)

// esRowColumns 是 _search 的 _source 显式列清单：只取 Row 的列，排除四个正文/头列（R8/R15）。
var esRowColumns = []string{
	"id", "created_at", "method", "path", "model", "api_key_hash",
	"status_code", "latency_ms", "stream", "error", "truncated",
}

var (
	errESClosed         = errors.New("request log es plugin is closed")
	errESNotInitialized = errors.New("request log es plugin is not initialized")
)

// esConfig 是插件私有配置；retention_days 为 0 表示永不清理。
type esConfig struct {
	Endpoints     []string `json:"endpoints"`
	Index         string   `json:"index"`
	Username      string   `json:"username"`
	Password      string   `json:"password"`
	APIKey        string   `json:"api_key"`
	RetentionDays int      `json:"retention_days"`
	Timeout       string   `json:"timeout"`
}

// esDoc 是 ES 文档的 snake_case 视图（R19）：契约类型 Entry/Row 无 json tag，
// 写入与回读都经此结构转换，避免下划线/大小写不匹配导致字段全空。
type esDoc struct {
	ID              string    `json:"id"`
	CreatedAt       time.Time `json:"created_at"`
	Method          string    `json:"method"`
	Path            string    `json:"path"`
	Model           string    `json:"model"`
	APIKeyHash      string    `json:"api_key_hash"`
	StatusCode      int       `json:"status_code"`
	LatencyMS       int       `json:"latency_ms"`
	Stream          bool      `json:"stream"`
	RequestBody     string    `json:"request_body"`
	ResponseBody    string    `json:"response_body"`
	RequestHeaders  string    `json:"request_headers"`
	ResponseHeaders string    `json:"response_headers"`
	Error           string    `json:"error"`
	Truncated       bool      `json:"truncated"`
}

func esDocFromEntry(e Entry) esDoc {
	created := e.CreatedAt.UTC()
	if created.IsZero() {
		created = time.Now().UTC()
	}
	return esDoc{
		ID:              e.ID,
		CreatedAt:       created,
		Method:          e.Method,
		Path:            e.Path,
		Model:           e.Model,
		APIKeyHash:      e.APIKeyHash,
		StatusCode:      e.StatusCode,
		LatencyMS:       e.LatencyMS,
		Stream:          e.Stream,
		RequestBody:     e.RequestBody,
		ResponseBody:    e.ResponseBody,
		RequestHeaders:  e.RequestHeaders,
		ResponseHeaders: e.ResponseHeaders,
		Error:           e.Error,
		Truncated:       e.Truncated,
	}
}

func (d esDoc) toRow() Row {
	return Row{
		ID:         d.ID,
		CreatedAt:  d.CreatedAt,
		Method:     d.Method,
		Path:       d.Path,
		Model:      d.Model,
		APIKeyHash: d.APIKeyHash,
		StatusCode: d.StatusCode,
		LatencyMS:  d.LatencyMS,
		Stream:     d.Stream,
		Error:      d.Error,
		Truncated:  d.Truncated,
	}
}

func (d esDoc) toEntry() Entry {
	return Entry{
		ID:              d.ID,
		CreatedAt:       d.CreatedAt,
		Method:          d.Method,
		Path:            d.Path,
		Model:           d.Model,
		APIKeyHash:      d.APIKeyHash,
		StatusCode:      d.StatusCode,
		LatencyMS:       d.LatencyMS,
		Stream:          d.Stream,
		RequestBody:     d.RequestBody,
		ResponseBody:    d.ResponseBody,
		RequestHeaders:  d.RequestHeaders,
		ResponseHeaders: d.ResponseHeaders,
		Error:           d.Error,
		Truncated:       d.Truncated,
	}
}

// esConn 是 Init 一次性装配好的不可变连接态；mu 只保护 closed 标志与 conn 指针的发布。
type esConn struct {
	client  *http.Client
	baseURL string
	index   string
	apiKey  string
	user    string
	pass    string
}

func (c *esConn) newRequest(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("build es request: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "ApiKey "+c.apiKey)
	} else if c.user != "" || c.pass != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req, nil
}

func (c *esConn) do(req *http.Request) ([]byte, int, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("es request %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read es response: %w", err)
	}
	return data, resp.StatusCode, nil
}

// esPlugin 独占一个 http.Client；mu 保护关闭与在途读写的竞态（R18）。
type esPlugin struct {
	mu sync.RWMutex

	conn      *esConn
	retention time.Duration
	closed    bool
}

var (
	_ Plugin = (*esPlugin)(nil)
	_ Reader = (*esPlugin)(nil)
)

func init() {
	Register("es", func() Plugin { return &esPlugin{} })
}

func (*esPlugin) Name() string { return "es" }

// parseESConfig 解析并校验私有配置：先写默认值再用 raw 覆盖，因此 {} 用默认值、
// 显式 0 才是「永不清理」；非法值一律拒绝，不做静默归一化。
func parseESConfig(raw json.RawMessage) (esConfig, error) {
	cfg := esConfig{
		Index:         defaultESIndex,
		RetentionDays: defaultESRetentionDays,
		Timeout:       defaultESTimeout,
	}
	if len(raw) > 0 {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return esConfig{}, errors.New("es config must be a JSON object")
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return esConfig{}, fmt.Errorf("invalid es config: %w", err)
		}
	}
	if len(cfg.Endpoints) == 0 {
		return esConfig{}, errors.New("es endpoints must not be empty")
	}
	for i, ep := range cfg.Endpoints {
		u, err := url.Parse(ep)
		// 不回显 endpoint 原文，避免其中可能含有的凭据进入错误/启动日志。
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return esConfig{}, fmt.Errorf("es endpoints[%d] must be a non-empty http(s) URL", i)
		}
	}
	if cfg.Password != "" && cfg.APIKey != "" {
		return esConfig{}, errors.New("es password and api_key must not both be set")
	}
	if cfg.Index == "" {
		return esConfig{}, errors.New("es index must not be empty")
	}
	if cfg.RetentionDays < 0 || cfg.RetentionDays > maxESRetentionDays {
		return esConfig{}, fmt.Errorf("es retention_days must be between 0 and %d", maxESRetentionDays)
	}
	if _, err := time.ParseDuration(cfg.Timeout); err != nil {
		return esConfig{}, fmt.Errorf("invalid es timeout: %w", err)
	}
	return cfg, nil
}

// esIndexMapping 定义索引字段类型：筛选字段用 keyword/date/integer/boolean，正文用 text。
func esIndexMapping() ([]byte, error) {
	type field struct {
		Type string `json:"type"`
	}
	properties := map[string]field{
		"id":               {Type: "keyword"},
		"created_at":       {Type: "date"},
		"method":           {Type: "keyword"},
		"path":             {Type: "keyword"},
		"model":            {Type: "keyword"},
		"api_key_hash":     {Type: "keyword"},
		"status_code":      {Type: "integer"},
		"latency_ms":       {Type: "integer"},
		"stream":           {Type: "boolean"},
		"truncated":        {Type: "boolean"},
		"request_body":     {Type: "text"},
		"response_body":    {Type: "text"},
		"request_headers":  {Type: "text"},
		"response_headers": {Type: "text"},
		"error":            {Type: "text"},
	}
	return json.Marshal(map[string]any{
		"mappings": map[string]any{"properties": properties},
	})
}

// Init 校验配置、装配 http.Client，再用 HEAD /<index> 探测索引是否存在：
// 404 时 PUT 建索引并带 mapping，200 时跳过创建（R20）。
func (p *esPlugin) Init(ctx context.Context, raw json.RawMessage) error {
	cfg, err := parseESConfig(raw)
	if err != nil {
		return err
	}
	timeout, _ := time.ParseDuration(cfg.Timeout) // parseESConfig 已校验
	conn := &esConn{
		client:  &http.Client{Timeout: timeout},
		baseURL: strings.TrimRight(cfg.Endpoints[0], "/"),
		index:   cfg.Index,
		apiKey:  cfg.APIKey,
		user:    cfg.Username,
		pass:    cfg.Password,
	}

	indexPath := "/" + url.PathEscape(conn.index)
	headReq, err := conn.newRequest(ctx, http.MethodHead, indexPath, "", nil)
	if err != nil {
		return err
	}
	_, status, err := conn.do(headReq)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
		// 索引已存在，跳过创建。
	case http.StatusNotFound:
		mapping, err := esIndexMapping()
		if err != nil {
			return fmt.Errorf("build es index mapping: %w", err)
		}
		putReq, err := conn.newRequest(ctx, http.MethodPut, indexPath, "application/json", bytes.NewReader(mapping))
		if err != nil {
			return err
		}
		_, putStatus, err := conn.do(putReq)
		if err != nil {
			return err
		}
		if putStatus < 200 || putStatus >= 300 {
			return fmt.Errorf("create es index %q: unexpected status %d", conn.index, putStatus)
		}
	default:
		return fmt.Errorf("check es index %q: unexpected status %d", conn.index, status)
	}

	p.mu.Lock()
	p.conn = conn
	p.retention = time.Duration(cfg.RetentionDays) * 24 * time.Hour
	p.closed = false
	p.mu.Unlock()
	return nil
}

// acquire 在关闭后或未初始化时返回良性错误；R18：Close 与在途 Write/Prune 并发时不 panic。
func (p *esPlugin) acquire() (*esConn, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return nil, errESClosed
	}
	if p.conn == nil {
		return nil, errESNotInitialized
	}
	return p.conn, nil
}

// esTime 把已解析好的 UTC 时刻格式化为 ES date 字段可识别的 RFC3339 字符串（R16）。
func esTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func esTerm(field string, value any) map[string]any {
	return map[string]any{"term": map[string]any{field: value}}
}

// buildESFilters 逐项把 Query 映射为 bool.filter 子句：时间用半开 [Start, End)，
// Q 用 multi_match 字面量（R22，保留字符无需转义）。
func buildESFilters(q Query) []any {
	filters := make([]any, 0)
	created := map[string]any{}
	if q.Start != nil {
		created["gte"] = esTime(*q.Start)
	}
	if q.End != nil {
		created["lt"] = esTime(*q.End)
	}
	if len(created) > 0 {
		filters = append(filters, map[string]any{"range": map[string]any{"created_at": created}})
	}
	if q.APIKeyHash != "" {
		filters = append(filters, esTerm("api_key_hash", q.APIKeyHash))
	}
	if q.Model != "" {
		filters = append(filters, esTerm("model", q.Model))
	}
	if q.Path != "" {
		filters = append(filters, esTerm("path", q.Path))
	}
	if q.StatusCode != 0 {
		filters = append(filters, esTerm("status_code", q.StatusCode))
	}
	if q.Stream != nil {
		filters = append(filters, esTerm("stream", *q.Stream))
	}
	latency := map[string]any{}
	if q.MinLatencyMS > 0 {
		latency["gte"] = q.MinLatencyMS
	}
	if q.MaxLatencyMS > 0 {
		latency["lte"] = q.MaxLatencyMS
	}
	if len(latency) > 0 {
		filters = append(filters, map[string]any{"range": map[string]any{"latency_ms": latency}})
	}
	if q.Q != "" {
		filters = append(filters, map[string]any{
			"multi_match": map[string]any{
				"query":  q.Q,
				"fields": []string{"request_body", "response_body"},
			},
		})
	}
	return filters
}

// Write 用 NDJSON _bulk 批量写入，_id 取 Entry.ID；响应 errors:true 只告警不返回错误。
func (p *esPlugin) Write(ctx context.Context, entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	c, err := p.acquire()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, e := range entries {
		action, err := json.Marshal(map[string]any{
			"index": map[string]string{"_index": c.index, "_id": e.ID},
		})
		if err != nil {
			return fmt.Errorf("encode es bulk action: %w", err)
		}
		doc, err := json.Marshal(esDocFromEntry(e))
		if err != nil {
			return fmt.Errorf("encode es bulk doc: %w", err)
		}
		buf.Write(action)
		buf.WriteByte('\n')
		buf.Write(doc)
		buf.WriteByte('\n')
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/_bulk", "application/x-ndjson", &buf)
	if err != nil {
		return err
	}
	data, status, err := c.do(req)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("es bulk write: unexpected status %d", status)
	}
	if len(data) > 0 {
		var bulkResp struct {
			Errors bool `json:"errors"`
		}
		if err := json.Unmarshal(data, &bulkResp); err != nil {
			return fmt.Errorf("decode es bulk response: %w", err)
		}
		if bulkResp.Errors {
			slog.Warn("es bulk write reported item errors", "entries", len(entries))
		}
	}
	return nil
}

// Search 把 Query 映射为 _search 请求，解析 hits.total.value 与 _source 到 Row。
func (p *esPlugin) Search(ctx context.Context, q Query) (Page, error) {
	empty := Page{Rows: []Row{}}
	c, err := p.acquire()
	if err != nil {
		return empty, err
	}
	page, pageSize := q.Page, q.PageSize
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultESPageSize
	}
	body := map[string]any{
		"query": map[string]any{
			"bool": map[string]any{"filter": buildESFilters(q)},
		},
		"sort":             []any{map[string]any{"created_at": map[string]any{"order": "desc"}}},
		"from":             (page - 1) * pageSize,
		"size":             pageSize,
		"track_total_hits": true,
		"_source":          esRowColumns,
	}
	data, err := json.Marshal(body)
	if err != nil {
		return empty, fmt.Errorf("encode es search: %w", err)
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/"+url.PathEscape(c.index)+"/_search", "application/json", bytes.NewReader(data))
	if err != nil {
		return empty, err
	}
	respData, status, err := c.do(req)
	if err != nil {
		return empty, err
	}
	if status < 200 || status >= 300 {
		return empty, fmt.Errorf("es search: unexpected status %d", status)
	}
	var resp struct {
		Hits struct {
			Total struct {
				Value int64 `json:"value"`
			} `json:"total"`
			Hits []struct {
				Source esDoc `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(respData, &resp); err != nil {
		return empty, fmt.Errorf("decode es search response: %w", err)
	}
	out := Page{
		Rows:     make([]Row, 0, len(resp.Hits.Hits)),
		Total:    resp.Hits.Total.Value,
		Page:     page,
		PageSize: pageSize,
	}
	for _, h := range resp.Hits.Hits {
		out.Rows = append(out.Rows, h.Source.toRow())
	}
	return out, nil
}

// Get 返回含正文的完整 Entry；404 或 found:false 映射为契约哨兵 ErrNotFound。
func (p *esPlugin) Get(ctx context.Context, id string) (*Entry, error) {
	c, err := p.acquire()
	if err != nil {
		return nil, err
	}
	path := "/" + url.PathEscape(c.index) + "/_doc/" + url.PathEscape(id)
	req, err := c.newRequest(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, err
	}
	data, status, err := c.do(req)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("es get %q: unexpected status %d", id, status)
	}
	var resp struct {
		Found  bool  `json:"found"`
		Source esDoc `json:"_source"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("decode es get response: %w", err)
	}
	if !resp.Found {
		return nil, ErrNotFound
	}
	e := resp.Source.toEntry()
	if e.ID == "" {
		e.ID = id // 兜底：部分 mapping 不把 id 存入 _source。
	}
	return &e, nil
}

// Prune 删除 created_at < before 的文档（半开），返回 ES 报回的 deleted 条数。
func (p *esPlugin) Prune(ctx context.Context, before time.Time) (int64, error) {
	c, err := p.acquire()
	if err != nil {
		return 0, err
	}
	body := map[string]any{
		"query": map[string]any{
			"range": map[string]any{"created_at": map[string]any{"lt": esTime(before)}},
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("encode es delete_by_query: %w", err)
	}
	path := "/" + url.PathEscape(c.index) + "/_delete_by_query"
	req, err := c.newRequest(ctx, http.MethodPost, path, "application/json", bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	respData, status, err := c.do(req)
	if err != nil {
		return 0, err
	}
	if status < 200 || status >= 300 {
		return 0, fmt.Errorf("es delete_by_query: unexpected status %d", status)
	}
	var resp struct {
		Deleted int64 `json:"deleted"`
	}
	if err := json.Unmarshal(respData, &resp); err != nil {
		return 0, fmt.Errorf("decode es delete_by_query response: %w", err)
	}
	return resp.Deleted, nil
}

// Retention 返回配置的保留期；0 表示核心不启动清理循环。
func (p *esPlugin) Retention() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.retention
}

// Close 关闭空闲连接；幂等且并发安全（R18），Init 失败后调用也安全。
func (p *esPlugin) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	c := p.conn
	p.mu.Unlock()

	if c == nil {
		return nil
	}
	c.client.CloseIdleConnections()
	return nil
}
