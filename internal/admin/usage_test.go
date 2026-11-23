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
	"net/http"
	"strings"
	"testing"
	"time"

	"aigate/internal/model"
)

// seedUsage 写入一条用量日志并同步累计日聚合（聚合查询以日聚合表为数据源）。
func seedUsage(t *testing.T, store *model.Store, userID, keyID, groupID, modelID uint, total int, createdAt time.Time) {
	t.Helper()
	entry := &model.LLMUsageLog{
		UserID:       userID,
		APIKeyID:     keyID,
		GroupID:      groupID,
		AIModelID:    modelID,
		Model:        "test-model",
		Provider:     "mock",
		PromptTokens: total,
		TotalTokens:  total,
		StatusCode:   http.StatusOK,
		Latency:      42,
		CreatedAt:    createdAt,
	}
	if err := store.InsertUsageLog(context.Background(), entry); err != nil {
		t.Fatalf("insert usage log: %v", err)
	}
	if err := store.UpsertDailyUsage(context.Background(), entry); err != nil {
		t.Fatalf("upsert daily usage: %v", err)
	}
}

// seedUsageFixture 造三条不同（用户/密钥/分组/模型）组合的用量：
//
//	user1/key11/group21/model31 = 100
//	user1/key11/group21/model32 = 50
//	user2/key12/group22/model31 = 30
func seedUsageFixture(t *testing.T, store *model.Store) {
	t.Helper()
	now := time.Now().UTC()
	seedUsage(t, store, 1, 11, 21, 31, 100, now)
	seedUsage(t, store, 1, 11, 21, 32, 50, now.Add(-time.Minute))
	seedUsage(t, store, 2, 12, 22, 31, 30, now.Add(-2*time.Minute))
}

// sumTotals 汇总聚合行的 total_tokens。
func sumTotals(rows []usageSummaryRowResp) int64 {
	var sum int64
	for _, r := range rows {
		sum += r.TotalTokens
	}
	return sum
}

// TestUsageSummary_GroupByAndFilters 覆盖聚合查询各维度与过滤条件。
func TestUsageSummary_GroupByAndFilters(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	seedUsageFixture(t, store)

	cases := []struct {
		name        string
		query       string
		wantGroupBy string
		wantRows    int
		wantTotal   int64
	}{
		{name: "default group by date", query: "", wantGroupBy: "date", wantRows: 1, wantTotal: 180},
		{name: "group by model", query: "group_by=model", wantGroupBy: "model", wantRows: 2, wantTotal: 180},
		{name: "group by key", query: "group_by=key", wantGroupBy: "key", wantRows: 2, wantTotal: 180},
		{name: "group by user", query: "group_by=user", wantGroupBy: "user", wantRows: 2, wantTotal: 180},
		{name: "group by group", query: "group_by=group", wantGroupBy: "group", wantRows: 2, wantTotal: 180},
		{name: "filter by user", query: "user_id=1", wantGroupBy: "date", wantRows: 1, wantTotal: 150},
		{name: "filter by group", query: "group_id=22", wantGroupBy: "date", wantRows: 1, wantTotal: 30},
		{name: "filter by key", query: "api_key_id=11", wantGroupBy: "date", wantRows: 1, wantTotal: 150},
		{name: "filter by model", query: "ai_model_id=31", wantGroupBy: "date", wantRows: 1, wantTotal: 130},
		{
			name:        "date range includes today",
			query:       "start=2020-01-01&end=2099-12-31",
			wantGroupBy: "date",
			wantRows:    1,
			wantTotal:   180,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := "/admin/usage/summary"
			if tc.query != "" {
				path += "?" + tc.query
			}
			status, body := doRequest(t, srv, http.MethodGet, path, adminToken, nil)
			assertStatus(t, status, http.StatusOK, body)
			got := mustDecode[usageSummaryResp](t, body)
			if got.GroupBy != tc.wantGroupBy {
				t.Errorf("group_by want %q, got %q", tc.wantGroupBy, got.GroupBy)
			}
			if len(got.Rows) != tc.wantRows {
				t.Fatalf("aggregate row count want %d, got %d, body: %s", tc.wantRows, len(got.Rows), body)
			}
			if sum := sumTotals(got.Rows); sum != tc.wantTotal {
				t.Errorf("total_tokens sum want %d, got %d", tc.wantTotal, sum)
			}
		})
	}

	t.Run("group by model breaks down per model", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/admin/usage/summary?group_by=model", adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		byModel := make(map[uint]int64)
		for _, row := range mustDecode[usageSummaryResp](t, body).Rows {
			byModel[row.AIModelID] = row.TotalTokens
		}
		if byModel[31] != 130 || byModel[32] != 50 {
			t.Errorf("group by model summary mismatch, got %+v", byModel)
		}
	})

	t.Run("invalid group_by 400", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/admin/usage/summary?group_by=week", adminToken, nil)
		assertStatus(t, status, http.StatusBadRequest, body)
	})

	t.Run("invalid date 400", func(t *testing.T) {
		for _, q := range []string{"start=2026/01/01", "end=bad-date"} {
			status, body := doRequest(t, srv, http.MethodGet, "/admin/usage/summary?"+q, adminToken, nil)
			if status != http.StatusBadRequest {
				t.Errorf("%s want 400, got %d, body: %s", q, status, body)
			}
		}
	})
}

// TestUsageSummary_IncludesCacheTokens 断言聚合结果含 cache read/write token 列与请求数。
func TestUsageSummary_IncludesCacheTokens(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	entry := &model.LLMUsageLog{
		UserID: 7, APIKeyID: 8, GroupID: 9, AIModelID: 10,
		PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7,
		PromptCacheReadTokens: 3, PromptCacheWriteTokens: 4,
		StatusCode: http.StatusOK, Latency: 11,
	}
	if err := store.InsertUsageLog(context.Background(), entry); err != nil {
		t.Fatalf("insert usage log: %v", err)
	}
	if err := store.UpsertDailyUsage(context.Background(), entry); err != nil {
		t.Fatalf("upsert daily usage: %v", err)
	}

	status, body := doRequest(t, srv, http.MethodGet, "/admin/usage/summary?user_id=7", adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	rows := mustDecode[usageSummaryResp](t, body).Rows
	if len(rows) != 1 {
		t.Fatalf("aggregate row count want 1, got %d, body: %s", len(rows), body)
	}
	row := rows[0]
	if row.PromptCacheReadTokens != 3 || row.PromptCacheWriteTokens != 4 {
		t.Errorf("cache token want 3/4, got %d/%d", row.PromptCacheReadTokens, row.PromptCacheWriteTokens)
	}
	if row.TotalTokens != 7 || row.PromptTokens != 5 || row.CompletionTokens != 2 || row.RequestCount != 1 {
		t.Errorf("aggregate fields mismatch, got %+v", row)
	}
}

// TestUsageLogs_PaginationAndOrder 覆盖明细查询的分页、倒序与过滤。
func TestUsageLogs_PaginationAndOrder(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	now := time.Now().UTC()
	seedUsage(t, store, 1, 11, 21, 31, 100, now)
	seedUsage(t, store, 1, 11, 21, 31, 200, now.Add(-time.Minute))
	seedUsage(t, store, 2, 12, 22, 32, 300, now.Add(-2*time.Minute))

	status, body := doRequest(t, srv, http.MethodGet, "/admin/usage/logs", adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	got := mustDecode[usageLogsResp](t, body)
	if got.Total != 3 || got.Page != 1 || got.PageSize != 50 || len(got.Logs) != 3 {
		t.Fatalf("default pagination mismatch, body: %s", body)
	}
	// 时间倒序：最新的 total=100 在最前，最旧的 total=300 在最后。
	wantOrder := []int{100, 200, 300}
	for i, want := range wantOrder {
		if got.Logs[i].TotalTokens != want {
			t.Errorf("row %d total_tokens want %d (desc), got %d", i, want, got.Logs[i].TotalTokens)
		}
	}
	first := got.Logs[0]
	if first.ID == 0 || first.Model != "test-model" || first.Provider != "mock" ||
		first.UserID != 1 || first.APIKeyID != 11 || first.GroupID != 21 || first.AIModelID != 31 ||
		first.StatusCode != http.StatusOK || first.Latency != 42 || first.CreatedAt == "" {
		t.Errorf("log row fields mismatch, got %+v", first)
	}

	t.Run("pagination", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/admin/usage/logs?page=1&page_size=2", adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		page1 := mustDecode[usageLogsResp](t, body)
		if page1.Total != 3 || len(page1.Logs) != 2 || page1.PageSize != 2 {
			t.Fatalf("page 1 mismatch, body: %s", body)
		}

		status, body = doRequest(t, srv, http.MethodGet, "/admin/usage/logs?page=2&page_size=2", adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		page2 := mustDecode[usageLogsResp](t, body)
		if page2.Total != 3 || len(page2.Logs) != 1 || page2.Logs[0].TotalTokens != 300 {
			t.Errorf("page 2 mismatch, body: %s", body)
		}
	})

	t.Run("page_size clamped to upper limit", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/admin/usage/logs?page_size=9999", adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		if got := mustDecode[usageLogsResp](t, body); got.PageSize != 500 {
			t.Errorf("page_size want clamped to 500, got %d", got.PageSize)
		}
	})

	t.Run("filter by user", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/admin/usage/logs?user_id=2", adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[usageLogsResp](t, body)
		if got.Total != 1 || got.Logs[0].UserID != 2 {
			t.Errorf("filter by user mismatch, body: %s", body)
		}
	})

	t.Run("invalid pagination and date 400", func(t *testing.T) {
		for _, q := range []string{"page=0", "page=abc", "page_size=0", "start=2026/01/01", "end=oops"} {
			status, body := doRequest(t, srv, http.MethodGet, "/admin/usage/logs?"+q, adminToken, nil)
			if status != http.StatusBadRequest {
				t.Errorf("%s want 400, got %d, body: %s", q, status, body)
			}
		}
	})
}

// seedCostUsage 写入一条带金额的用量并同步日聚合：日聚合按聚合键去重累计，
// 故不同模型（AIModelID）才能形成跨币种的两行。
func seedCostUsage(t *testing.T, store *model.Store, modelID uint, currency string, vendor, platform int64) {
	t.Helper()
	entry := &model.LLMUsageLog{
		UserID:             1,
		APIKeyID:           1,
		AIModelID:          modelID,
		Model:              "cost-model",
		Provider:           "mock",
		TotalTokens:        10,
		StatusCode:         http.StatusOK,
		Currency:           currency,
		CostVendorMicros:   vendor,
		CostPlatformMicros: platform,
	}
	if err := store.UpsertDailyUsage(context.Background(), entry); err != nil {
		t.Fatalf("upsert daily usage: %v", err)
	}
	if err := store.InsertUsageLog(context.Background(), entry); err != nil {
		t.Fatalf("insert usage log: %v", err)
	}
}

// 聚合响应顶层带结算金额合计与结算币种。
func TestUsageSummary_IncludesCostTotals(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	seedCostUsage(t, store, 1, "CNY", 500_000, 500_000)
	seedCostUsage(t, store, 2, "USD", 100_000, 710_000)

	for _, groupBy := range []string{"date", "user", "group"} {
		status, body := doRequest(t, srv, http.MethodGet, "/admin/usage/summary?group_by="+groupBy, adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[usageSummaryResp](t, body)
		if got.TotalCostPlatformMicros != 1_210_000 || got.PlatformCurrency != "CNY" {
			t.Fatalf("group_by=%s summary = %+v", groupBy, got)
		}
	}

	// 过滤后合计只覆盖命中行。
	status, body := doRequest(t, srv, http.MethodGet, "/admin/usage/summary?ai_model_id=2", adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	if got := mustDecode[usageSummaryResp](t, body); got.TotalCostPlatformMicros != 710_000 {
		t.Fatalf("filtered total = %d, want 710000", got.TotalCostPlatformMicros)
	}
}

// 非 model 维度跨币种时 cost_vendor_micros 置 0 且 mixed_currency=true；
// group_by=model 时行币种唯一，模型币种金额保留。
func TestUsageSummary_MixedCurrencyFlag(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	seedCostUsage(t, store, 1, "CNY", 500_000, 500_000)
	seedCostUsage(t, store, 2, "USD", 100_000, 710_000)

	status, body := doRequest(t, srv, http.MethodGet, "/admin/usage/summary?group_by=date", adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	rows := mustDecode[usageSummaryResp](t, body).Rows
	if len(rows) != 1 || !rows[0].MixedCurrency {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].CostVendorMicros != 0 || rows[0].CostPlatformMicros != 1_210_000 {
		t.Fatalf("mixed row costs = %d/%d, want 0/1210000", rows[0].CostVendorMicros, rows[0].CostPlatformMicros)
	}

	status, body = doRequest(t, srv, http.MethodGet, "/admin/usage/summary?group_by=model", adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	byModel := make(map[uint]usageSummaryRowResp)
	for _, row := range mustDecode[usageSummaryResp](t, body).Rows {
		byModel[row.AIModelID] = row
	}
	if r := byModel[1]; r.Currency != "CNY" || r.CostVendorMicros != 500_000 || r.MixedCurrency {
		t.Errorf("model 1 row = %+v", r)
	}
	if r := byModel[2]; r.Currency != "USD" || r.CostVendorMicros != 100_000 || r.MixedCurrency {
		t.Errorf("model 2 row = %+v", r)
	}
}

// 明细行带两列金额，币种取平台结算币种（明细表不冗余币种）。
func TestUsageLogs_IncludesCostColumns(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	seedCostUsage(t, store, 1, "USD", 100_000, 710_000)

	status, body := doRequest(t, srv, http.MethodGet, "/admin/usage/logs", adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	logs := mustDecode[usageLogsResp](t, body).Logs
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
	row := logs[0]
	if row.Currency != "CNY" || row.CostVendorMicros != 100_000 || row.CostPlatformMicros != 710_000 {
		t.Fatalf("log row = %+v", row)
	}
}

// CSV 表头与管理面明细 JSON 字段一一对应，金额列在 created_at 之前。
func TestUsageExport_HeaderHasCostColumns(t *testing.T) {
	srv, store, adminToken := newAdminServer(t)
	seedCostUsage(t, store, 1, "USD", 100_000, 710_000)

	status, body := doRequest(t, srv, http.MethodGet, "/admin/usage/export?format=csv", adminToken, nil)
	assertStatus(t, status, http.StatusOK, body)
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	want := "status_code,latency,cost_vendor_micros,cost_platform_micros,created_at"
	if !strings.HasSuffix(lines[0], want) {
		t.Fatalf("csv header = %s, want suffix %s", lines[0], want)
	}
	if len(lines) != 2 {
		t.Fatalf("csv rows = %q, want header + 1 record", lines)
	}
	if !strings.Contains(lines[1], ",100000,710000,") {
		t.Fatalf("csv data row lacks cost columns: %s", lines[1])
	}

	t.Run("json export mirrors cost columns", func(t *testing.T) {
		status, body := doRequest(t, srv, http.MethodGet, "/admin/usage/export?format=json", adminToken, nil)
		assertStatus(t, status, http.StatusOK, body)
		got := mustDecode[struct {
			Logs []usageLogResp `json:"logs"`
		}](t, body)
		if len(got.Logs) != 1 {
			t.Fatalf("json export logs = %d, want 1", len(got.Logs))
		}
		if row := got.Logs[0]; row.CostPlatformMicros != 710_000 || row.Currency != "CNY" {
			t.Fatalf("json export row = %+v", row)
		}
	})
}
