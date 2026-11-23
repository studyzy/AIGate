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

// 计费管理面：价目、日历、汇率与平台结算币种的 CRUD、校验与缓存失效。
//
// 「写入后立刻读到新值」的断言一律经 newTestBillingServer 返回的**同一个** StoreSource 实例
// 完成：缓存没有自动失效钩子，漏调 Invalidate* 时新数据要等进程重启才对数据面可见，
// 那类回归只有共享同一实例的读回能发现。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aigate/internal/model"
)

// ---------------------------------------------------------------------------
// 请求/响应结构（仅测试使用）
// ---------------------------------------------------------------------------

type priceResp struct {
	ID                     uint   `json:"id"`
	AIModelID              uint   `json:"ai_model_id"`
	Currency               string `json:"currency"`
	EffectiveAt            string `json:"effective_at"`
	PeakHours              string `json:"peak_hours"`
	PeakInputHitMicros     int64  `json:"peak_input_hit_micros"`
	PeakInputMissMicros    int64  `json:"peak_input_miss_micros"`
	PeakOutputMicros       int64  `json:"peak_output_micros"`
	OffpeakInputHitMicros  int64  `json:"offpeak_input_hit_micros"`
	OffpeakInputMissMicros int64  `json:"offpeak_input_miss_micros"`
	OffpeakOutputMicros    int64  `json:"offpeak_output_micros"`
}

type priceListResp struct {
	Prices []priceResp `json:"prices"`
}

type calendarResp struct {
	Days []struct {
		Date      string `json:"date"`
		IsWorkday bool   `json:"is_workday"`
		Note      string `json:"note"`
	} `json:"days"`
}

type rateResp struct {
	ID           uint   `json:"id"`
	FromCurrency string `json:"from_currency"`
	ToCurrency   string `json:"to_currency"`
	EffectiveAt  string `json:"effective_at"`
	RateMicros   int64  `json:"rate_micros"`
}

type rateListResp struct {
	Rates []rateResp `json:"rates"`
}

type billingModelResp struct {
	ID                uint       `json:"id"`
	Currency          string     `json:"currency"`
	EnablePeakPricing bool       `json:"enable_peak_pricing"`
	UseUpstreamCost   bool       `json:"use_upstream_cost"`
	CurrentPrice      *priceResp `json:"current_price"`
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// createBillingModel 经管理面创建模型并返回其 ID；extra 覆盖请求体字段。
func createBillingModel(t *testing.T, srv *httptest.Server, adminToken string, extra map[string]any) uint {
	t.Helper()
	body := map[string]any{
		"provider":   "mock",
		"model_id":   "m",
		"model_name": "m",
		"url":        "https://upstream.example/v1",
		"api_key":    "sk-upstream-secret",
		"model_type": "openai-completions",
	}
	for k, v := range extra {
		body[k] = v
	}
	status, raw := doRequest(t, srv, http.MethodPost, "/admin/models", adminToken, body)
	assertStatus(t, status, http.StatusCreated, raw)
	return mustDecode[billingModelResp](t, raw).ID
}

// priceBody 构造一份最小价目请求体；extra 覆盖/追加字段。
func priceBody(extra map[string]any) map[string]any {
	body := map[string]any{
		"effective_at":             "2026-11-01T00:00:00Z",
		"peak_output_micros":       8_000_000,
		"offpeak_output_micros":    4_000_000,
		"offpeak_input_hit_micros": 500_000,
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// ---------------------------------------------------------------------------
// 价目
// ---------------------------------------------------------------------------

// TestAdminBilling_PriceCRUD 覆盖价目增删查与重复生效时刻的 409。
func TestAdminBilling_PriceCRUD(t *testing.T) {
	srv, store, src := newTestBillingServer(t)
	_, adminToken := seedAdmin(t, store)
	modelID := createBillingModel(t, srv, adminToken, map[string]any{"currency": "CNY"})
	pricesPath := fmt.Sprintf("/admin/models/%d/prices", modelID)

	body := priceBody(map[string]any{
		"peak_hours": `{"timezone":"Asia/Shanghai","windows":[{"days":[1],"start":"09:00","end":"12:00"}]}`,
	})
	status, raw := doRequest(t, srv, http.MethodPost, pricesPath, adminToken, body)
	assertStatus(t, status, http.StatusCreated, raw)
	created := mustDecode[priceResp](t, raw)
	if created.ID == 0 || created.AIModelID != modelID {
		t.Fatalf("created = %+v", created)
	}
	// 币种缺省取模型当前币种。
	if created.Currency != "CNY" {
		t.Fatalf("currency = %q, want CNY (inherited from the model)", created.Currency)
	}
	if created.PeakOutputMicros != 8_000_000 || created.OffpeakOutputMicros != 4_000_000 {
		t.Fatalf("prices not echoed: %+v", created)
	}

	t.Run("duplicate effective_at is 409", func(t *testing.T) {
		status, raw := doRequest(t, srv, http.MethodPost, pricesPath, adminToken, body)
		assertStatus(t, status, http.StatusConflict, raw)
	})

	t.Run("list newest first", func(t *testing.T) {
		status, raw := doRequest(t, srv, http.MethodPost, pricesPath, adminToken,
			priceBody(map[string]any{"effective_at": "2026-12-01T00:00:00Z"}))
		assertStatus(t, status, http.StatusCreated, raw)

		status, raw = doRequest(t, srv, http.MethodGet, pricesPath, adminToken, nil)
		assertStatus(t, status, http.StatusOK, raw)
		got := mustDecode[priceListResp](t, raw)
		if len(got.Prices) != 2 {
			t.Fatalf("prices = %+v", got.Prices)
		}
		if got.Prices[0].EffectiveAt != "2026-12-01T00:00:00Z" {
			t.Fatalf("prices[0].EffectiveAt = %q, want the newest snapshot first", got.Prices[0].EffectiveAt)
		}
	})

	t.Run("delete", func(t *testing.T) {
		status, raw := doRequest(t, srv, http.MethodDelete,
			fmt.Sprintf("/admin/model-prices/%d", created.ID), adminToken, nil)
		assertStatus(t, status, http.StatusOK, raw)

		status, raw = doRequest(t, srv, http.MethodGet, pricesPath, adminToken, nil)
		assertStatus(t, status, http.StatusOK, raw)
		if got := mustDecode[priceListResp](t, raw); len(got.Prices) != 1 {
			t.Fatalf("prices after delete = %+v", got.Prices)
		}
		// 未知 id 删除仍是 404。
		status, raw = doRequest(t, srv, http.MethodDelete,
			fmt.Sprintf("/admin/model-prices/%d", created.ID), adminToken, nil)
		assertStatus(t, status, http.StatusNotFound, raw)
	})

	t.Run("unknown model is 404", func(t *testing.T) {
		status, raw := doRequest(t, srv, http.MethodGet, "/admin/models/999/prices", adminToken, nil)
		assertStatus(t, status, http.StatusNotFound, raw)
	})

	// 写路径全程都在共享实例上，这里只确认实例可用（真正的失效断言在下面的专项测试里）。
	if _, err := src.Prices(context.Background(), modelID); err != nil {
		t.Fatalf("shared source prices: %v", err)
	}
}

// TestAdminBilling_RejectsBadPeakHours 覆盖时段与币种的写入校验：不可评估的时段必须 400。
func TestAdminBilling_RejectsBadPeakHours(t *testing.T) {
	srv, store, _ := newTestBillingServer(t)
	_, adminToken := seedAdmin(t, store)
	modelID := createBillingModel(t, srv, adminToken, map[string]any{"currency": "CNY"})
	pricesPath := fmt.Sprintf("/admin/models/%d/prices", modelID)

	cases := []struct {
		name      string
		peakHours string
	}{
		{"bad clock", `{"timezone":"Asia/Shanghai","windows":[{"days":[1],"start":"9am","end":"12:00"}]}`},
		{"minute out of range", `{"timezone":"Asia/Shanghai","windows":[{"days":[1],"start":"09:00","end":"12:60"}]}`},
		{"all days out of range", `{"timezone":"Asia/Shanghai","windows":[{"days":[7,-1],"start":"09:00","end":"12:00"}]}`},
		{"empty days", `{"timezone":"Asia/Shanghai","windows":[{"days":[],"start":"09:00","end":"12:00"}]}`},
		{"unknown timezone", `{"timezone":"Mars/Olympus","windows":[{"days":[1],"start":"09:00","end":"12:00"}]}`},
		{"missing timezone", `{"windows":[{"days":[1],"start":"09:00","end":"12:00"}]}`},
		{"empty windows", `{"timezone":"Asia/Shanghai","windows":[]}`},
		{"not json", `peak`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := doRequest(t, srv, http.MethodPost, pricesPath, adminToken,
				priceBody(map[string]any{"peak_hours": tc.peakHours}))
			assertStatus(t, status, http.StatusBadRequest, raw)
			if msg := decodeError(t, raw).Error.Message; msg == "" {
				t.Fatal("400 response must carry an English error message")
			}
		})
	}

	t.Run("cross-midnight window accepted", func(t *testing.T) {
		status, raw := doRequest(t, srv, http.MethodPost, pricesPath, adminToken, priceBody(map[string]any{
			"peak_hours": `{"timezone":"Asia/Shanghai","windows":[{"days":[1,2],"start":"22:00","end":"02:00"}]}`,
		}))
		assertStatus(t, status, http.StatusCreated, raw)
	})

	t.Run("24:00 is a legal end clock", func(t *testing.T) {
		status, raw := doRequest(t, srv, http.MethodPost, pricesPath, adminToken, priceBody(map[string]any{
			"effective_at": "2026-12-01T00:00:00Z",
			"peak_hours":   `{"timezone":"Asia/Shanghai","windows":[{"days":[5],"start":"09:00","end":"24:00"}]}`,
		}))
		assertStatus(t, status, http.StatusCreated, raw)
	})

	t.Run("bad effective_at", func(t *testing.T) {
		status, raw := doRequest(t, srv, http.MethodPost, pricesPath, adminToken,
			priceBody(map[string]any{"effective_at": "2026-11-01"}))
		assertStatus(t, status, http.StatusBadRequest, raw)
	})

	t.Run("bad currency", func(t *testing.T) {
		status, raw := doRequest(t, srv, http.MethodPost, pricesPath, adminToken,
			priceBody(map[string]any{"currency": "RMB1"}))
		assertStatus(t, status, http.StatusBadRequest, raw)
	})

	t.Run("model without currency requires one", func(t *testing.T) {
		bare := createBillingModel(t, srv, adminToken, nil)
		status, raw := doRequest(t, srv, http.MethodPost,
			fmt.Sprintf("/admin/models/%d/prices", bare), adminToken, priceBody(nil))
		assertStatus(t, status, http.StatusBadRequest, raw)
		// 显式给出币种即可写入。
		status, raw = doRequest(t, srv, http.MethodPost,
			fmt.Sprintf("/admin/models/%d/prices", bare), adminToken,
			priceBody(map[string]any{"currency": "usd"}))
		assertStatus(t, status, http.StatusCreated, raw)
		if got := mustDecode[priceResp](t, raw); got.Currency != "USD" {
			t.Fatalf("currency = %q, want normalized USD", got.Currency)
		}
	})
}

// TestAdminBilling_ModelCarriesPricingFields 覆盖模型创建/更新对计价字段的透传与 current_price 回显。
func TestAdminBilling_ModelCarriesPricingFields(t *testing.T) {
	srv, store, _ := newTestBillingServer(t)
	_, adminToken := seedAdmin(t, store)

	body := map[string]any{
		"provider":   "mock",
		"model_id":   "m",
		"url":        "https://x",
		"model_type": "openai-completions",
		"currency":   "cny",
		// 显式关闭「采信上游成本」：零值必须真的落库（列默认 true）。
		"use_upstream_cost":   false,
		"enable_peak_pricing": true,
	}
	status, raw := doRequest(t, srv, http.MethodPost, "/admin/models", adminToken, body)
	assertStatus(t, status, http.StatusCreated, raw)
	created := mustDecode[billingModelResp](t, raw)
	if created.Currency != "CNY" || !created.EnablePeakPricing || created.UseUpstreamCost {
		t.Fatalf("created = %+v", created)
	}
	if created.CurrentPrice != nil {
		t.Fatalf("current_price = %+v, want null without prices", created.CurrentPrice)
	}
	modelPath := fmt.Sprintf("/admin/models/%d", created.ID)

	// 默认值口径：不传时时 currency 为空、峰谷关闭、采信上游成本开启。
	status, raw = doRequest(t, srv, http.MethodPost, "/admin/models", adminToken, map[string]any{
		"provider": "mock", "model_id": "d", "url": "https://x", "model_type": "openai-completions",
	})
	assertStatus(t, status, http.StatusCreated, raw)
	if def := mustDecode[billingModelResp](t, raw); def.Currency != "" || def.EnablePeakPricing || !def.UseUpstreamCost {
		t.Fatalf("defaults = %+v", def)
	}

	status, raw = doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/admin/models/%d/prices", created.ID), adminToken,
		priceBody(map[string]any{"effective_at": "2020-01-01T00:00:00Z"}))
	assertStatus(t, status, http.StatusCreated, raw)

	status, raw = doRequest(t, srv, http.MethodGet, modelPath, adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
	got := mustDecode[billingModelResp](t, raw)
	if got.CurrentPrice == nil || got.CurrentPrice.OffpeakOutputMicros != 4_000_000 {
		t.Fatalf("current_price = %+v, want the effective snapshot", got.CurrentPrice)
	}

	// 局部更新：只改币种与两个开关，其余字段保持。
	status, raw = doRequest(t, srv, http.MethodPut, modelPath, adminToken, map[string]any{
		"currency": "usd", "enable_peak_pricing": false, "use_upstream_cost": true,
	})
	assertStatus(t, status, http.StatusOK, raw)
	updated := mustDecode[billingModelResp](t, raw)
	if updated.Currency != "USD" || updated.EnablePeakPricing || !updated.UseUpstreamCost {
		t.Fatalf("updated = %+v", updated)
	}
	if updated.CurrentPrice == nil {
		t.Fatal("current_price must survive a partial update")
	}

	status, raw = doRequest(t, srv, http.MethodPut, modelPath, adminToken, map[string]any{"currency": "RMB1"})
	assertStatus(t, status, http.StatusBadRequest, raw)
}

// TestAdminBilling_PriceWritesInvalidateCache 是「漏调 InvalidateModel」的回归测试：
// 经管理面写入后，用**同一个** StoreSource 实例必须立刻读到新价目与新的模型计价配置。
func TestAdminBilling_PriceWritesInvalidateCache(t *testing.T) {
	srv, store, src := newTestBillingServer(t)
	_, adminToken := seedAdmin(t, store)
	ctx := context.Background()
	modelID := createBillingModel(t, srv, adminToken, map[string]any{"currency": "CNY"})
	pricesPath := fmt.Sprintf("/admin/models/%d/prices", modelID)

	// 预热缓存：此时数据面已知的是「1 条价目、币种 CNY、峰谷关闭」。
	if _, err := src.Model(ctx, modelID); err != nil {
		t.Fatalf("warm model cache: %v", err)
	}
	if _, err := src.Prices(ctx, modelID); err != nil {
		t.Fatalf("warm price cache: %v", err)
	}

	// 1) 追加价目 → 立刻可见。
	status, raw := doRequest(t, srv, http.MethodPost, pricesPath, adminToken,
		priceBody(map[string]any{"effective_at": "2020-01-01T00:00:00Z"}))
	assertStatus(t, status, http.StatusCreated, raw)
	prices, err := src.Prices(ctx, modelID)
	if err != nil {
		t.Fatalf("prices after create: %v", err)
	}
	if len(prices) != 1 || prices[0].Price.OffpeakOutputMicros != 4_000_000 {
		t.Fatalf("prices after create = %+v, want the new snapshot visible immediately", prices)
	}

	// 2) 改模型行的币种与峰谷开关 → 立刻可见。
	status, raw = doRequest(t, srv, http.MethodPut, fmt.Sprintf("/admin/models/%d", modelID), adminToken,
		map[string]any{"currency": "USD", "enable_peak_pricing": true, "use_upstream_cost": false})
	assertStatus(t, status, http.StatusOK, raw)
	info, err := src.Model(ctx, modelID)
	if err != nil {
		t.Fatalf("model after update: %v", err)
	}
	if info.Currency != "USD" || !info.EnablePeak || info.UseUpstreamCost {
		t.Fatalf("model info after update = %+v, want USD/peak on/upstream off", info)
	}

	// 3) 删价目 → 立刻可见（含删除时按价目定位模型的路径）。
	status, raw = doRequest(t, srv, http.MethodPost, pricesPath, adminToken,
		priceBody(map[string]any{"effective_at": "2026-06-01T00:00:00Z"}))
	assertStatus(t, status, http.StatusCreated, raw)
	if got, _ := src.Prices(ctx, modelID); len(got) != 2 {
		t.Fatalf("prices after second create = %d, want 2", len(got))
	}
	status, raw = doRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("/admin/model-prices/%d", mustDecode[priceResp](t, raw).ID), adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
	if got, _ := src.Prices(ctx, modelID); len(got) != 1 {
		t.Fatalf("prices after delete = %d, want 1", len(got))
	}
}

// ---------------------------------------------------------------------------
// 计费日历
// ---------------------------------------------------------------------------

// TestAdminBilling_CalendarUpsertAndRead 覆盖登记、读取、删除与 is_workday 显式性。
func TestAdminBilling_CalendarUpsertAndRead(t *testing.T) {
	srv, store, _ := newTestBillingServer(t)
	_, adminToken := seedAdmin(t, store)
	body := map[string]any{"days": []map[string]any{
		{"date": "2026-10-01", "is_workday": false, "note": "国庆"},
		{"date": "2026-10-10", "is_workday": true, "note": "调休"},
	}}
	status, raw := doRequest(t, srv, http.MethodPut, "/admin/billing-calendar", adminToken, body)
	assertStatus(t, status, http.StatusOK, raw)

	status, raw = doRequest(t, srv, http.MethodGet, "/admin/billing-calendar?year=2026", adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
	got := mustDecode[calendarResp](t, raw)
	if len(got.Days) != 2 {
		t.Fatalf("days = %+v", got.Days)
	}
	if got.Days[0].Date != "2026-10-01" || got.Days[0].IsWorkday || got.Days[0].Note != "国庆" {
		t.Fatalf("days[0] = %+v", got.Days[0])
	}

	// 只取某月。
	status, raw = doRequest(t, srv, http.MethodGet, "/admin/billing-calendar?year=2026&month=11", adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
	if month := mustDecode[calendarResp](t, raw); len(month.Days) != 0 {
		t.Fatalf("november days = %+v, want none", month.Days)
	}

	// 同日期覆盖写：改为工作日并清空备注。
	status, raw = doRequest(t, srv, http.MethodPut, "/admin/billing-calendar", adminToken, map[string]any{
		"days": []map[string]any{{"date": "2026-10-01", "is_workday": true}},
	})
	assertStatus(t, status, http.StatusOK, raw)
	status, raw = doRequest(t, srv, http.MethodGet, "/admin/billing-calendar?year=2026", adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
	got = mustDecode[calendarResp](t, raw)
	if len(got.Days) != 2 || !got.Days[0].IsWorkday {
		t.Fatalf("days after upsert = %+v", got.Days)
	}

	// 删除某天。
	status, raw = doRequest(t, srv, http.MethodDelete, "/admin/billing-calendar/2026-10-01", adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
	status, raw = doRequest(t, srv, http.MethodGet, "/admin/billing-calendar?year=2026", adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
	if left := mustDecode[calendarResp](t, raw); len(left.Days) != 1 || left.Days[0].Date != "2026-10-10" {
		t.Fatalf("days after delete = %+v", left.Days)
	}
}

// TestAdminBilling_CalendarRejectsBadInput 覆盖日历写入的入场校验。
func TestAdminBilling_CalendarRejectsBadInput(t *testing.T) {
	srv, store, _ := newTestBillingServer(t)
	_, adminToken := seedAdmin(t, store)

	cases := []struct {
		name string
		body any
	}{
		{"missing is_workday", map[string]any{"days": []map[string]any{{"date": "2026-10-01"}}}},
		{"bad date", map[string]any{"days": []map[string]any{{"date": "2026/10/01", "is_workday": false}}}},
		{"duplicate date", map[string]any{"days": []map[string]any{
			{"date": "2026-10-01", "is_workday": false},
			{"date": "2026-10-01", "is_workday": true},
		}}},
		{"empty days", map[string]any{"days": []map[string]any{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := doRequest(t, srv, http.MethodPut, "/admin/billing-calendar", adminToken, tc.body)
			assertStatus(t, status, http.StatusBadRequest, raw)
		})
	}

	t.Run("bad year query", func(t *testing.T) {
		status, raw := doRequest(t, srv, http.MethodGet, "/admin/billing-calendar?year=abc", adminToken, nil)
		assertStatus(t, status, http.StatusBadRequest, raw)
	})
	t.Run("bad delete date", func(t *testing.T) {
		status, raw := doRequest(t, srv, http.MethodDelete, "/admin/billing-calendar/2026-13-01", adminToken, nil)
		assertStatus(t, status, http.StatusBadRequest, raw)
	})
}

// TestAdminBilling_CalendarWritesInvalidateCache 证明日历写入/删除后共享实例立刻可见。
func TestAdminBilling_CalendarWritesInvalidateCache(t *testing.T) {
	srv, store, src := newTestBillingServer(t)
	_, adminToken := seedAdmin(t, store)
	ctx := context.Background()
	// 2026-10-02 是周五：未登记时按星期几推断为工作日。
	day := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	from, to := day.AddDate(0, 0, -1), day.AddDate(0, 0, 1)
	if cal, err := src.Calendar(ctx, from, to); err != nil || !cal.IsWorkday(day) {
		t.Fatalf("warm calendar workday=%v err=%v", cal.IsWorkday(day), err)
	}

	status, raw := doRequest(t, srv, http.MethodPut, "/admin/billing-calendar", adminToken, map[string]any{
		"days": []map[string]any{{"date": "2026-10-02", "is_workday": false, "note": "假日"}},
	})
	assertStatus(t, status, http.StatusOK, raw)
	cal, err := src.Calendar(ctx, from, to)
	if err != nil {
		t.Fatalf("calendar after upsert: %v", err)
	}
	if cal.IsWorkday(day) {
		t.Fatal("holiday override must be visible immediately after PUT")
	}

	status, raw = doRequest(t, srv, http.MethodDelete, "/admin/billing-calendar/2026-10-02", adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
	cal, err = src.Calendar(ctx, from, to)
	if err != nil {
		t.Fatalf("calendar after delete: %v", err)
	}
	if !cal.IsWorkday(day) {
		t.Fatal("calendar entry removal must be visible immediately after DELETE")
	}
}

// ---------------------------------------------------------------------------
// 汇率
// ---------------------------------------------------------------------------

// TestAdminBilling_RateCRUD 覆盖汇率增删查、409 与入场校验。
func TestAdminBilling_RateCRUD(t *testing.T) {
	srv, store, _ := newTestBillingServer(t)
	_, adminToken := seedAdmin(t, store)
	body := map[string]any{"from_currency": "USD", "to_currency": "CNY",
		"effective_at": "2026-11-01T00:00:00Z", "rate_micros": 7_100_000}

	status, raw := doRequest(t, srv, http.MethodPost, "/admin/currency-rates", adminToken, body)
	assertStatus(t, status, http.StatusCreated, raw)
	created := mustDecode[rateResp](t, raw)
	if created.RateMicros != 7_100_000 || created.FromCurrency != "USD" {
		t.Fatalf("created = %+v", created)
	}

	status, raw = doRequest(t, srv, http.MethodPost, "/admin/currency-rates", adminToken, body)
	assertStatus(t, status, http.StatusConflict, raw)

	status, raw = doRequest(t, srv, http.MethodGet, "/admin/currency-rates?from=usd&to=cny", adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
	if got := mustDecode[rateListResp](t, raw); len(got.Rates) != 1 || got.Rates[0].ID != created.ID {
		t.Fatalf("rates = %+v", got.Rates)
	}

	status, raw = doRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("/admin/currency-rates/%d", created.ID), adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
	status, raw = doRequest(t, srv, http.MethodGet, "/admin/currency-rates?from=USD&to=CNY", adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
	if got := mustDecode[rateListResp](t, raw); len(got.Rates) != 0 {
		t.Fatalf("rates after delete = %+v", got.Rates)
	}
}

// TestAdminBilling_RateValidation 覆盖汇率写入的入场校验。
func TestAdminBilling_RateValidation(t *testing.T) {
	srv, store, _ := newTestBillingServer(t)
	_, adminToken := seedAdmin(t, store)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"same currency", map[string]any{"from_currency": "CNY", "to_currency": "CNY",
			"effective_at": "2026-11-01T00:00:00Z", "rate_micros": 1000000}},
		{"zero rate", map[string]any{"from_currency": "USD", "to_currency": "CNY",
			"effective_at": "2026-11-01T00:00:00Z", "rate_micros": 0}},
		{"negative rate", map[string]any{"from_currency": "USD", "to_currency": "CNY",
			"effective_at": "2026-11-01T00:00:00Z", "rate_micros": -1}},
		{"rate over 1e9", map[string]any{"from_currency": "USD", "to_currency": "CNY",
			"effective_at": "2026-11-01T00:00:00Z", "rate_micros": 1_000_000_001}},
		{"bad effective_at", map[string]any{"from_currency": "USD", "to_currency": "CNY",
			"effective_at": "2026-11-01", "rate_micros": 7_100_000}},
		{"bad from currency", map[string]any{"from_currency": "US", "to_currency": "CNY",
			"effective_at": "2026-11-01T00:00:00Z", "rate_micros": 7_100_000}},
		{"missing to currency", map[string]any{"from_currency": "USD", "to_currency": "",
			"effective_at": "2026-11-01T00:00:00Z", "rate_micros": 7_100_000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := doRequest(t, srv, http.MethodPost, "/admin/currency-rates", adminToken, tc.body)
			assertStatus(t, status, http.StatusBadRequest, raw)
		})
	}

	t.Run("list without pair", func(t *testing.T) {
		status, raw := doRequest(t, srv, http.MethodGet, "/admin/currency-rates", adminToken, nil)
		assertStatus(t, status, http.StatusBadRequest, raw)
	})
}

// TestAdminBilling_RateWritesInvalidateCache 证明汇率写入/删除后共享实例立刻可见。
func TestAdminBilling_RateWritesInvalidateCache(t *testing.T) {
	srv, store, src := newTestBillingServer(t)
	_, adminToken := seedAdmin(t, store)
	ctx := context.Background()
	if got, err := src.Rate(ctx, "USD", "CNY"); err != nil || len(got) != 0 {
		t.Fatalf("warm rate cache = %+v err=%v", got, err)
	}

	status, raw := doRequest(t, srv, http.MethodPost, "/admin/currency-rates", adminToken, map[string]any{
		"from_currency": "USD", "to_currency": "CNY",
		"effective_at": "2026-11-01T00:00:00Z", "rate_micros": 7_100_000,
	})
	assertStatus(t, status, http.StatusCreated, raw)
	created := mustDecode[rateResp](t, raw)

	rates, err := src.Rate(ctx, "USD", "CNY")
	if err != nil {
		t.Fatalf("rate after create: %v", err)
	}
	if len(rates) != 1 || rates[0].RateMicros != 7_100_000 {
		t.Fatalf("rates after create = %+v, want the new rate visible immediately", rates)
	}

	status, raw = doRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("/admin/currency-rates/%d", created.ID), adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
	if left, err := src.Rate(ctx, "USD", "CNY"); err != nil || len(left) != 0 {
		t.Fatalf("rates after delete = %+v err=%v, want empty", left, err)
	}
}

// ---------------------------------------------------------------------------
// 平台结算币种
// ---------------------------------------------------------------------------

// TestAdminSettings_PlatformCurrencyNormalized 覆盖结算币种的读取、大写归一、校验与缓存失效。
func TestAdminSettings_PlatformCurrencyNormalized(t *testing.T) {
	srv, store, src := newTestBillingServer(t)
	_, adminToken := seedAdmin(t, store)
	ctx := context.Background()

	status, raw := doRequest(t, srv, http.MethodGet, "/admin/settings", adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
	if got := mustDecode[struct {
		PlatformCurrency string `json:"platform_currency"`
	}](t, raw); got.PlatformCurrency != model.DefaultPlatformCurrency {
		t.Fatalf("platform_currency default = %q, want %q", got.PlatformCurrency, model.DefaultPlatformCurrency)
	}

	// 预热缓存：数据面当前看到的是默认结算币种。
	if cur, err := src.PlatformCurrency(ctx); err != nil || cur != model.DefaultPlatformCurrency {
		t.Fatalf("warm platform currency = %q err=%v", cur, err)
	}

	status, raw = doRequest(t, srv, http.MethodPut, "/admin/settings", adminToken,
		map[string]any{"platform_currency": "usd"})
	assertStatus(t, status, http.StatusOK, raw)
	var got struct {
		PlatformCurrency string `json:"platform_currency"`
	}
	status, raw = doRequest(t, srv, http.MethodGet, "/admin/settings", adminToken, nil)
	assertStatus(t, status, http.StatusOK, raw)
	got = mustDecode[struct {
		PlatformCurrency string `json:"platform_currency"`
	}](t, raw)
	if got.PlatformCurrency != "USD" {
		t.Fatalf("platform_currency = %q, want USD", got.PlatformCurrency)
	}
	if cur, err := src.PlatformCurrency(ctx); err != nil || cur != "USD" {
		t.Fatalf("platform currency after update = %q err=%v, want USD immediately", cur, err)
	}

	t.Run("rejects invalid code", func(t *testing.T) {
		for _, bad := range []string{"", "US", "USDX", "U5D"} {
			status, raw := doRequest(t, srv, http.MethodPut, "/admin/settings", adminToken,
				map[string]any{"platform_currency": bad})
			assertStatus(t, status, http.StatusBadRequest, raw)
		}
	})

	t.Run("partial update keeps currency", func(t *testing.T) {
		status, raw := doRequest(t, srv, http.MethodPut, "/admin/settings", adminToken,
			map[string]any{"user_key_limit": 3})
		assertStatus(t, status, http.StatusOK, raw)
		status, raw = doRequest(t, srv, http.MethodGet, "/admin/settings", adminToken, nil)
		assertStatus(t, status, http.StatusOK, raw)
		if got := mustDecode[struct {
			PlatformCurrency string `json:"platform_currency"`
		}](t, raw); got.PlatformCurrency != "USD" {
			t.Fatalf("platform_currency after unrelated update = %q, want USD", got.PlatformCurrency)
		}
	})
}
