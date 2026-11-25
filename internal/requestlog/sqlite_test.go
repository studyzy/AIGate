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

// 本文件的 sqlite 用例以黑盒方式驱动 Plugin/Reader 契约：
// 每个用例独占 t.TempDir() 里的 SQLite 文件，并用 database/sql 直读插件自有表，
// 不依赖任何插件内部符号。
package requestlog_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"aigate/internal/requestlog"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// tempDBPath 返回一个位于独立临时目录的 db 文件路径。
func tempDBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "request-logs.db")
}

// rawConfig 把路径与可选配置拼成 Init 的 raw JSON。
func rawConfig(t *testing.T, path string, opts map[string]any) json.RawMessage {
	t.Helper()
	m := map[string]any{"path": path}
	for k, v := range opts {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal sqlite config: %v", err)
	}
	return b
}

// openSQLiteAt 在指定路径打开插件并登记清理（Close 幂等，重复调用无害）。
func openSQLiteAt(t *testing.T, path string, opts map[string]any) requestlog.Plugin {
	t.Helper()
	p, err := requestlog.Open(context.Background(), "sqlite", rawConfig(t, path, opts))
	if err != nil {
		t.Fatalf("Open(sqlite) 失败：%v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// openReadDB 打开指向同一文件的第二条连接，用于直读 schema / PRAGMA。
func openReadDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open read db: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("read db handle: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

// tableNames 返回库内全部表名。
func tableNames(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatalf("query tables: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		out[name] = true
	}
	return out
}

// tableColumns 返回指定表的全部列名。
func tableColumns(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("query table_info(%s): %v", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var dflt any
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		out[name] = true
	}
	return out
}

// indexColumns 返回每个用户索引的列序列（按 seqno 顺序）。
func indexColumns(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='index' AND sql IS NOT NULL`)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index name: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close index rows: %v", err)
	}

	out := map[string][]string{}
	for _, name := range names {
		quoted := strings.ReplaceAll(name, "'", "''")
		r, err := db.Query("PRAGMA index_info('" + quoted + "')")
		if err != nil {
			t.Fatalf("query index_info(%s): %v", name, err)
		}
		var cols []string
		for r.Next() {
			var seqno, cid int
			var col sql.NullString
			if err := r.Scan(&seqno, &cid, &col); err != nil {
				t.Fatalf("scan index_info(%s): %v", name, err)
			}
			if col.Valid {
				cols = append(cols, col.String)
			}
		}
		if err := r.Close(); err != nil {
			t.Fatalf("close index_info rows: %v", err)
		}
		out[name] = cols
	}
	return out
}

// makeEntry 构造一条字段齐全的日志（正文/头均带唯一标记，便于与列表行比对）。
func makeEntry(id string, created time.Time) requestlog.Entry {
	return requestlog.Entry{
		ID:              id,
		CreatedAt:       created,
		Method:          "POST",
		Path:            "/v1/chat/completions",
		Model:           "gpt-4",
		APIKeyHash:      "hash-a",
		StatusCode:      200,
		LatencyMS:       100,
		Stream:          false,
		RequestBody:     `{"prompt":"body-` + id + `"}`,
		ResponseBody:    `{"answer":"answer-` + id + `"}`,
		RequestHeaders:  `{"Content-Type":"application/json"}`,
		ResponseHeaders: `{"Content-Type":"application/json"}`,
		Error:           "",
		Truncated:       false,
	}
}

// searchIDs 执行查询并断言无错误，返回行 ID 序列。
func searchIDs(t *testing.T, p requestlog.Plugin, q requestlog.Query) []string {
	t.Helper()
	page, err := p.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search(%+v) 失败：%v", q, err)
	}
	ids := make([]string, 0, len(page.Rows))
	for _, r := range page.Rows {
		ids = append(ids, r.ID)
	}
	return ids
}

// assertIDs 断言查询结果的 ID 序列（含顺序）与期望一致。
func assertIDs(t *testing.T, p requestlog.Plugin, q requestlog.Query, want ...string) {
	t.Helper()
	got := searchIDs(t, p, q)
	if !slices.Equal(got, want) {
		t.Errorf("Search(%+v) IDs = %v, want %v", q, got, want)
	}
}

func boolPtr(v bool) *bool { return &v }

func timePtr(v time.Time) *time.Time { return &v }

// assertEntryEqual 逐字段比对 Entry：时间用 Equal（驱动回读的 Location 可能与写入时不同），
// 其余字段用 ==；避免用结构体 == 比较 time.Time。
func assertEntryEqual(t *testing.T, got, want requestlog.Entry) {
	t.Helper()
	if got.ID != want.ID || !got.CreatedAt.Equal(want.CreatedAt) ||
		got.Method != want.Method || got.Path != want.Path || got.Model != want.Model ||
		got.APIKeyHash != want.APIKeyHash || got.StatusCode != want.StatusCode ||
		got.LatencyMS != want.LatencyMS || got.Stream != want.Stream ||
		got.RequestBody != want.RequestBody || got.ResponseBody != want.ResponseBody ||
		got.RequestHeaders != want.RequestHeaders || got.ResponseHeaders != want.ResponseHeaders ||
		got.Error != want.Error || got.Truncated != want.Truncated {
		t.Errorf("Entry 不一致：got %+v, want %+v", got, want)
	}
}

// ---------------------------------------------------------------------------
// 注册表与自有 schema
// ---------------------------------------------------------------------------

// TestSQLite_Registered：R10 —— 只断言 Names() 含 sqlite（Task 1 测试会注册 fake 插件，
// 不能用精确列表），并验证 Open 走同一条选择器路径、插件同时满足 Reader。
func TestSQLite_Registered(t *testing.T) {
	names := requestlog.Names()
	if !slices.Contains(names, "sqlite") {
		t.Fatalf("Names() = %v，未包含 sqlite", names)
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("Names() 未按字典序返回：%v", names)
	}

	p := openSQLiteAt(t, tempDBPath(t), nil)
	if got := p.Name(); got != "sqlite" {
		t.Errorf("Name() = %q，期望 sqlite", got)
	}
	var _ requestlog.Reader = p // 编译期断言：Task 9 会把同一实例当 Reader 用
}

// TestSQLite_InitCreatesOwnTable：插件自带 request_logs schema，不复用核心库表；
// 断言列、索引与 WAL 持久化，且主库表 llm_usage_logs 不出现在该文件。
func TestSQLite_InitCreatesOwnTable(t *testing.T) {
	path := tempDBPath(t)
	openSQLiteAt(t, path, nil)

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Init 应创建自有 db 文件 %q：%v", path, err)
	}
	db := openReadDB(t, path)

	tables := tableNames(t, db)
	if !tables["request_logs"] {
		t.Fatalf("插件文件缺少 request_logs 表：%v", tables)
	}
	if tables["llm_usage_logs"] {
		t.Error("插件自有文件不应包含主库表 llm_usage_logs（核心 schema 必须保持不变）")
	}

	cols := tableColumns(t, db, "request_logs")
	for _, want := range []string{
		"id", "created_at", "method", "path", "model", "api_key_hash",
		"status_code", "latency_ms", "stream",
		"request_body", "response_body", "request_headers", "response_headers",
		"error", "truncated",
	} {
		if !cols[want] {
			t.Errorf("request_logs 缺少列 %s；实得 %v", want, cols)
		}
	}

	indexes := indexColumns(t, db)
	for _, col := range []string{"created_at", "api_key_hash", "model", "status_code", "path", "latency_ms"} {
		found := false
		for _, cols := range indexes {
			if len(cols) == 1 && cols[0] == col {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("缺少 %s 单列索引；实得 %v", col, indexes)
		}
	}
	composite := false
	for _, cols := range indexes {
		if len(cols) == 2 && cols[0] == "created_at" && cols[1] == "api_key_hash" {
			composite = true
		}
	}
	if !composite {
		t.Errorf("缺少 (created_at, api_key_hash) 复合索引；实得 %v", indexes)
	}

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q，期望 wal", mode)
	}
}

// TestSQLite_ConfigValidation：默认值、0=永不、自定义保留期与非法配置拒绝。
func TestSQLite_ConfigValidation(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir) // 默认路径落在当前工作目录，避免污染仓库
		p, err := requestlog.Open(context.Background(), "sqlite", json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Open(sqlite, {}) 失败：%v", err)
		}
		t.Cleanup(func() { _ = p.Close() })
		if got := p.Retention(); got != 7*24*time.Hour {
			t.Errorf("Retention() = %v，期望默认 7d", got)
		}
		if _, err := os.Stat(filepath.Join(dir, "request-logs.db")); err != nil {
			t.Errorf("默认路径 request-logs.db 应被创建：%v", err)
		}
	})

	t.Run("retention zero means never", func(t *testing.T) {
		p := openSQLiteAt(t, tempDBPath(t), map[string]any{"retention_days": 0})
		if got := p.Retention(); got != 0 {
			t.Errorf("Retention() = %v，0 应表示永不清理", got)
		}
	})

	t.Run("retention custom", func(t *testing.T) {
		p := openSQLiteAt(t, tempDBPath(t), map[string]any{"retention_days": 30})
		if got := p.Retention(); got != 30*24*time.Hour {
			t.Errorf("Retention() = %v，期望 30d", got)
		}
	})

	t.Run("invalid rejected", func(t *testing.T) {
		path := tempDBPath(t)
		for i, raw := range []json.RawMessage{
			rawConfig(t, path, map[string]any{"retention_days": 36501}),
			rawConfig(t, path, map[string]any{"retention_days": -1}),
			json.RawMessage(`{"path":""}`),
			json.RawMessage(`{"path":`),     // 非法 JSON
			json.RawMessage(`{"path":123}`), // 类型不符
		} {
			p, err := requestlog.Open(context.Background(), "sqlite", raw)
			if err == nil {
				_ = p.Close()
				t.Errorf("case %d (%s) 应被拒绝，却成功了", i, raw)
			}
			if p != nil {
				t.Errorf("case %d Init 失败时不应返回插件", i)
			}
			if err != nil && !strings.Contains(err.Error(), "init request log plugin") {
				t.Errorf("case %d 应由 Init 拒绝（而非实现未注册）：%v", i, err)
			}
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("非法配置不应创建 db 文件：%v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// 写入、列表、详情、幂等、清理
// ---------------------------------------------------------------------------

// TestSQLite_WriteSearchGetPrune：默认排序、分页回显、列表不含正文（R8：Row 无正文字段，
// 改用 Select 列清单与 Get 对比）、详情含正文、不存在、幂等与 Prune 行数。
func TestSQLite_WriteSearchGetPrune(t *testing.T) {
	ctx := context.Background()
	p := openSQLiteAt(t, tempDBPath(t), nil)
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	e1 := makeEntry("id-1", t0)
	e1.APIKeyHash, e1.Model, e1.StatusCode, e1.LatencyMS, e1.Stream = "hash-1", "model-1", 200, 50, false
	e2 := makeEntry("id-2", t0.Add(time.Hour))
	e2.APIKeyHash, e2.Model, e2.StatusCode, e2.LatencyMS, e2.Stream = "hash-2", "model-2", 429, 200, true
	e3 := makeEntry("id-3", t0.Add(2*time.Hour))
	e3.APIKeyHash, e3.Model, e3.StatusCode, e3.LatencyMS, e3.Stream = "hash-3", "model-3", 500, 350, false
	if err := p.Write(ctx, []requestlog.Entry{e1, e2, e3}); err != nil {
		t.Fatalf("Write 失败：%v", err)
	}

	page, err := p.Search(ctx, requestlog.Query{})
	if err != nil {
		t.Fatalf("Search 失败：%v", err)
	}
	if page.Rows == nil {
		t.Fatal("Rows 必须是非 nil 的 []Row（空结果序列化为 [] 而非 null）")
	}
	if page.Total != 3 {
		t.Errorf("Total = %d，期望 3", page.Total)
	}
	if page.Page != 1 || page.PageSize != 50 {
		t.Errorf("分页回显 = %d/%d，期望默认 1/50", page.Page, page.PageSize)
	}
	want := []string{"id-3", "id-2", "id-1"} // created_at DESC
	got := make([]string, 0, len(page.Rows))
	for _, r := range page.Rows {
		got = append(got, r.ID)
	}
	if !slices.Equal(got, want) {
		t.Errorf("默认排序 IDs = %v，期望 %v", got, want)
	}
	if r := page.Rows[1]; r.ID != "id-2" || r.Model != "model-2" || r.StatusCode != 429 ||
		r.LatencyMS != 200 || !r.Stream || r.APIKeyHash != "hash-2" || r.Path != e2.Path ||
		r.Method != e2.Method || r.Error != e2.Error || r.Truncated != e2.Truncated ||
		!r.CreatedAt.Equal(e2.CreatedAt) {
		t.Errorf("列表行字段与写入不一致：%+v", r)
	}
	// R8：列表行类型是 []Row，本身不含四个正文/头字段；详情才带回正文。
	detail, err := p.Get(ctx, "id-2")
	if err != nil {
		t.Fatalf("Get 失败：%v", err)
	}
	assertEntryEqual(t, *detail, e2)
	if detail.RequestBody == "" || detail.ResponseBody == "" ||
		detail.RequestHeaders == "" || detail.ResponseHeaders == "" {
		t.Error("Get 必须带回四列正文/头内容")
	}
	if _, err := p.Get(ctx, "missing-id"); !errors.Is(err, requestlog.ErrNotFound) {
		t.Errorf("Get 不存在应返回 ErrNotFound，实得 %v", err)
	}

	empty, err := p.Search(ctx, requestlog.Query{APIKeyHash: "no-such-hash"})
	if err != nil {
		t.Fatalf("Search 空结果失败：%v", err)
	}
	if empty.Rows == nil || len(empty.Rows) != 0 || empty.Total != 0 {
		t.Errorf("空结果应为非 nil 空切片 + Total=0，实得 %+v", empty)
	}

	// 同 id 重复写入：OnConflict DoNothing 幂等，首写内容保持不变。
	dup := e1
	dup.RequestBody, dup.Model = "changed", "changed"
	if err := p.Write(ctx, []requestlog.Entry{dup}); err != nil {
		t.Fatalf("幂等 Write 失败：%v", err)
	}
	dupPage, err := p.Search(ctx, requestlog.Query{})
	if err != nil {
		t.Fatalf("幂等后 Search 失败：%v", err)
	}
	if dupPage.Total != 3 {
		t.Errorf("幂等写入后 Total = %d，期望仍为 3", dupPage.Total)
	}
	first, err := p.Get(ctx, "id-1")
	if err != nil {
		t.Fatalf("幂等后 Get 失败：%v", err)
	}
	assertEntryEqual(t, *first, e1)

	// Prune 用 created_at < before（半开），返回受影响行数。
	before := t0.Add(2 * time.Hour)
	n, err := p.Prune(ctx, before)
	if err != nil {
		t.Fatalf("Prune 失败：%v", err)
	}
	if n != 2 {
		t.Errorf("Prune 删除行数 = %d，期望 2", n)
	}
	rest, err := p.Search(ctx, requestlog.Query{})
	if err != nil {
		t.Fatalf("Prune 后 Search 失败：%v", err)
	}
	if rest.Total != 1 || len(rest.Rows) != 1 || rest.Rows[0].ID != "id-3" {
		t.Errorf("Prune 后应只剩 id-3，实得 %+v", rest.Rows)
	}
	n2, err := p.Prune(ctx, before)
	if err != nil || n2 != 0 {
		t.Errorf("等于 before 的行不应被删除：n=%d err=%v", n2, err)
	}
}

// TestSQLite_OrderTieBreak：created_at 相同时按 id DESC 兜底。
func TestSQLite_OrderTieBreak(t *testing.T) {
	p := openSQLiteAt(t, tempDBPath(t), nil)
	same := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	if err := p.Write(context.Background(), []requestlog.Entry{
		makeEntry("aaa", same), makeEntry("bbb", same),
	}); err != nil {
		t.Fatalf("Write 失败：%v", err)
	}
	assertIDs(t, p, requestlog.Query{}, "bbb", "aaa")
}

// ---------------------------------------------------------------------------
// 全量筛选（R16：Start/End 已是解析好的 UTC 时刻，插件不解析日期字符串）
// ---------------------------------------------------------------------------

// filterSet 写 5 条在各维度上互不相同的日志，返回时间基准。
func filterSet(t *testing.T, p requestlog.Plugin) (day0, day1 time.Time) {
	t.Helper()
	day0 = time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	day1 = day0.AddDate(0, 0, 1) // 半开区间的排他上界：次日 00:00 UTC

	entries := []requestlog.Entry{
		makeEntry("e0", time.Date(2026, 11, 24, 23, 0, 0, 0, time.UTC)),
		makeEntry("e1", day0.Add(2*time.Hour)),
		makeEntry("e2", day0.Add(10*time.Hour)),
		makeEntry("e3", day0.Add(23*time.Hour+59*time.Minute+59*time.Second)),
		makeEntry("e4", day1),
	}
	entries[0].APIKeyHash, entries[0].Model, entries[0].Path = "h0", "m0", "/v1/chat/completions"
	entries[0].StatusCode, entries[0].LatencyMS, entries[0].Stream = 200, 10, false
	entries[0].RequestBody, entries[0].ResponseBody = "alpha", "resp0"

	entries[1].APIKeyHash, entries[1].Model, entries[1].Path = "h1", "m1", "/v1/messages"
	entries[1].StatusCode, entries[1].LatencyMS, entries[1].Stream = 400, 100, true
	entries[1].RequestBody, entries[1].ResponseBody = "beta", "resp1"

	entries[2].APIKeyHash, entries[2].Model, entries[2].Path = "h2", "m2", "/v1/chat/completions"
	entries[2].StatusCode, entries[2].LatencyMS, entries[2].Stream = 429, 500, false
	entries[2].RequestBody, entries[2].ResponseBody = "gamma", "resp2"

	entries[3].APIKeyHash, entries[3].Model, entries[3].Path = "h3", "m3", "/v1/responses"
	entries[3].StatusCode, entries[3].LatencyMS, entries[3].Stream = 500, 1000, true
	entries[3].RequestBody, entries[3].ResponseBody = "delta", "resp3"

	entries[4].APIKeyHash, entries[4].Model, entries[4].Path = "h4", "m4", "/v1/chat/completions"
	entries[4].StatusCode, entries[4].LatencyMS, entries[4].Stream = 200, 2000, false
	entries[4].RequestBody, entries[4].ResponseBody = "epsilon", "resp4"

	if err := p.Write(context.Background(), entries); err != nil {
		t.Fatalf("Write filterSet 失败：%v", err)
	}
	return day0, day1
}

// TestSQLite_AllFilters：逐个与组合验证 Query 的每个字段。
func TestSQLite_AllFilters(t *testing.T) {
	p := openSQLiteAt(t, tempDBPath(t), nil)
	day0, day1 := filterSet(t, p)
	desc := []string{"e4", "e3", "e2", "e1", "e0"} // created_at DESC 全序

	t.Run("no filter", func(t *testing.T) {
		assertIDs(t, p, requestlog.Query{}, desc...)
	})

	t.Run("start/end half-open inclusive day, exclusive next day", func(t *testing.T) {
		// 含 end 当天 23:59:59，排除恰好等于 End 的 e4（R16：直接构造 UTC 时刻）。
		assertIDs(t, p, requestlog.Query{Start: timePtr(day0), End: timePtr(day1)}, "e3", "e2", "e1")
	})

	t.Run("start only", func(t *testing.T) {
		assertIDs(t, p, requestlog.Query{Start: timePtr(day0.Add(10 * time.Hour))}, "e4", "e3", "e2")
	})

	t.Run("end only", func(t *testing.T) {
		assertIDs(t, p, requestlog.Query{End: timePtr(day0.Add(10 * time.Hour))}, "e1", "e0")
	})

	t.Run("start and end exclude equal end", func(t *testing.T) {
		assertIDs(t, p, requestlog.Query{
			Start: timePtr(day0.Add(2 * time.Hour)),
			End:   timePtr(day0.Add(10 * time.Hour)),
		}, "e1")
	})

	t.Run("api_key_hash", func(t *testing.T) { assertIDs(t, p, requestlog.Query{APIKeyHash: "h2"}, "e2") })
	t.Run("model", func(t *testing.T) { assertIDs(t, p, requestlog.Query{Model: "m3"}, "e3") })
	t.Run("path", func(t *testing.T) {
		assertIDs(t, p, requestlog.Query{Path: "/v1/chat/completions"}, "e4", "e2", "e0")
	})
	t.Run("status_code", func(t *testing.T) {
		assertIDs(t, p, requestlog.Query{StatusCode: 200}, "e4", "e0")
	})
	t.Run("stream true", func(t *testing.T) {
		assertIDs(t, p, requestlog.Query{Stream: boolPtr(true)}, "e3", "e1")
	})
	t.Run("stream false", func(t *testing.T) {
		assertIDs(t, p, requestlog.Query{Stream: boolPtr(false)}, "e4", "e2", "e0")
	})
	t.Run("min latency", func(t *testing.T) {
		assertIDs(t, p, requestlog.Query{MinLatencyMS: 500}, "e4", "e3", "e2")
	})
	t.Run("max latency", func(t *testing.T) {
		assertIDs(t, p, requestlog.Query{MaxLatencyMS: 500}, "e2", "e1", "e0")
	})
	t.Run("latency range", func(t *testing.T) {
		assertIDs(t, p, requestlog.Query{MinLatencyMS: 100, MaxLatencyMS: 500}, "e2", "e1")
	})
	t.Run("q request body", func(t *testing.T) {
		assertIDs(t, p, requestlog.Query{Q: "gamma"}, "e2")
	})
	t.Run("q response body", func(t *testing.T) {
		assertIDs(t, p, requestlog.Query{Q: "resp3"}, "e3")
	})

	t.Run("all combined", func(t *testing.T) {
		assertIDs(t, p, requestlog.Query{
			Start:        timePtr(day0),
			End:          timePtr(day1),
			APIKeyHash:   "h2",
			Model:        "m2",
			Path:         "/v1/chat/completions",
			StatusCode:   429,
			Stream:       boolPtr(false),
			MinLatencyMS: 100,
			MaxLatencyMS: 1000,
			Q:            "gamma",
		}, "e2")
	})

	t.Run("pagination", func(t *testing.T) {
		page, err := p.Search(context.Background(), requestlog.Query{Page: 2, PageSize: 2})
		if err != nil {
			t.Fatalf("Search 分页失败：%v", err)
		}
		if page.Total != 5 || page.Page != 2 || page.PageSize != 2 {
			t.Errorf("分页元数据 = %+v，期望 Total=5 Page=2 PageSize=2", page)
		}
		ids := make([]string, 0, len(page.Rows))
		for _, r := range page.Rows {
			ids = append(ids, r.ID)
		}
		if !slices.Equal(ids, []string{"e2", "e1"}) {
			t.Errorf("第二页 IDs = %v，期望 [e2 e1]", ids)
		}
		last, err := p.Search(context.Background(), requestlog.Query{Page: 3, PageSize: 2})
		if err != nil {
			t.Fatalf("Search 末页失败：%v", err)
		}
		if last.Rows == nil || len(last.Rows) != 1 || last.Rows[0].ID != "e0" {
			t.Errorf("末页应只剩 e0，实得 %+v", last.Rows)
		}
		beyond, err := p.Search(context.Background(), requestlog.Query{Page: 99, PageSize: 2})
		if err != nil {
			t.Fatalf("Search 越界页失败：%v", err)
		}
		if beyond.Rows == nil || len(beyond.Rows) != 0 || beyond.Total != 5 {
			t.Errorf("越界页应为非 nil 空切片且 Total=5，实得 %+v", beyond)
		}
	})
}

// TestSQLite_QIsLiteral（Review Focus 4）：%/_/\ 必须转义，Q 按字面子串匹配两个正文列。
func TestSQLite_QIsLiteral(t *testing.T) {
	p := openSQLiteAt(t, tempDBPath(t), nil)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	entries := []requestlog.Entry{
		makeEntry("lit-1", now.Add(4*time.Second)),
		makeEntry("trap-1", now.Add(3*time.Second)),
		makeEntry("lit-2", now.Add(2*time.Second)),
		makeEntry("lit-3", now.Add(time.Second)),
		makeEntry("trap-2", now),
	}
	entries[0].RequestBody, entries[0].ResponseBody = "prefix 100%_done suffix", "ok"
	entries[1].RequestBody, entries[1].ResponseBody = "100XXdone", "ok" // 若 _/% 未转义会被误匹配
	entries[2].RequestBody, entries[2].ResponseBody = "ok", "100%_done" // 命中响应正文列
	entries[3].RequestBody, entries[3].ResponseBody = `literal 50\%_off here`, "ok"
	entries[4].RequestBody, entries[4].ResponseBody = "50%Xoff", "ok" // 若 \ 未转义会被误匹配
	if err := p.Write(context.Background(), entries); err != nil {
		t.Fatalf("Write 失败：%v", err)
	}

	assertIDs(t, p, requestlog.Query{Q: "100%_done"}, "lit-1", "lit-2")
	assertIDs(t, p, requestlog.Query{Q: `50\%_off`}, "lit-3")
	assertIDs(t, p, requestlog.Query{Q: "not-in-any-body"}) // 无命中
}

// ---------------------------------------------------------------------------
// R18：Close 幂等 / 并发安全，关闭后写入返回良性错误而非 panic
// ---------------------------------------------------------------------------

func TestSQLite_CloseIsIdempotent(t *testing.T) {
	ctx := context.Background()
	p := openSQLiteAt(t, tempDBPath(t), nil)
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

func TestSQLite_CloseConcurrentWithWrite(t *testing.T) {
	p := openSQLiteAt(t, tempDBPath(t), nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := makeEntry(fmt.Sprintf("concurrent-%d", i), time.Now().UTC())
			if err := p.Write(context.Background(), []requestlog.Entry{e}); err != nil {
				// 关闭竞态下返回错误是允许的（良性），但不能 panic。
				_ = err
			}
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

// TestSQLite_ConfigRejectsUnknownKeys（T1）：拼错的未知配置键必须在 Init 阶段被拒绝，
// 不能静默回退到默认的 7 天保留期；拒绝前不得创建 db 文件。
func TestSQLite_ConfigRejectsUnknownKeys(t *testing.T) {
	path := tempDBPath(t)
	raw := rawConfig(t, path, map[string]any{"retention_day": 3})
	p, err := requestlog.Open(context.Background(), "sqlite", raw)
	if err == nil {
		_ = p.Close()
		t.Fatal("未知配置键应被拒绝，却成功")
	}
	if p != nil {
		t.Error("Init 失败时不应返回插件")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("错误信息 %q 未包含 'unknown field'", err.Error())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("非法配置不应创建 db 文件：%v", err)
	}
}
