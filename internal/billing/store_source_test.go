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

package billing_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"aigate/internal/billing"
	"aigate/internal/model"
)

// newTestStore 创建一个隔离的临时 SQLite Store（已建表，测试结束自动关闭）。
// 与 internal/model 的同名夹具同口径，但后者未导出到其他包，故在此另建一份。
func newTestStore(t *testing.T) *model.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	store, err := model.Open(model.DBTypeSQLite, path)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	if err := store.AutoMigrate(); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestStoreSource_ModelAndPriceCaches(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m := &model.AIModel{ModelID: "m", URL: "https://x", Currency: "CNY", EnablePeakPricing: true, UseUpstreamCost: true}
	if err := store.CreateAIModel(ctx, m); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := store.CreateAIModelPrice(ctx, &model.AIModelPrice{AIModelID: m.ID, Currency: "CNY",
		EffectiveAt: at, PeakOutputMicros: 8_000_000}); err != nil {
		t.Fatal(err)
	}
	src := billing.NewStoreSource(store)
	mi, err := src.Model(ctx, m.ID)
	if err != nil || mi.Currency != "CNY" || !mi.EnablePeak || !mi.UseUpstreamCost {
		t.Fatalf("model = %+v err=%v", mi, err)
	}
	first, _ := src.Prices(ctx, m.ID)
	if len(first) != 1 {
		t.Fatalf("prices = %d", len(first))
	}
	// 不失效：仍返回缓存
	store.CreateAIModelPrice(ctx, &model.AIModelPrice{AIModelID: m.ID, Currency: "CNY",
		EffectiveAt: at.Add(time.Hour), PeakOutputMicros: 9_000_000})
	if cached, _ := src.Prices(ctx, m.ID); len(cached) != 1 {
		t.Fatalf("cache should still hold 1, got %d", len(cached))
	}
	src.InvalidateModel(m.ID)
	if fresh, _ := src.Prices(ctx, m.ID); len(fresh) != 2 {
		t.Fatalf("after invalidate want 2, got %d", len(fresh))
	}
}

func TestStoreSource_PriceScheduleParsed(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.CreateAIModelPrice(ctx, &model.AIModelPrice{AIModelID: 1, Currency: "CNY",
		EffectiveAt: time.Now().UTC(),
		PeakHours:   `{"timezone":"Asia/Shanghai","windows":[{"days":[1],"start":"09:00","end":"12:00"}]}`}); err != nil {
		t.Fatal(err)
	}
	got, err := billing.NewStoreSource(store).Prices(ctx, 1)
	if err != nil || len(got) != 1 || len(got[0].Schedule.Windows) != 1 {
		t.Fatalf("schedule = %+v err=%v", got, err)
	}
}

func TestStoreSource_PlatformCurrencyDefaultsToCNY(t *testing.T) {
	store := newTestStore(t)
	got, err := billing.NewStoreSource(store).PlatformCurrency(context.Background())
	if err != nil || got != "CNY" {
		t.Fatalf("platform = %q err=%v", got, err)
	}
}

func TestStoreSource_CalendarFromRows(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := store.UpsertBillingCalendarDays(ctx, []model.BillingCalendarDay{{Date: day, IsWorkday: false}}); err != nil {
		t.Fatal(err)
	}
	cal, err := billing.NewStoreSource(store).Calendar(ctx, day.AddDate(0, 0, -1), day.AddDate(0, 0, 1))
	if err != nil || cal.IsWorkday(day) {
		t.Fatalf("calendar err=%v workday=%v", err, cal.IsWorkday(day))
	}
}

// 失效粒度：清模型行缓存必须同时清价目缓存，且互不牵连其他模型。
func TestStoreSource_InvalidateModelClearsRowAndPrices(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"a", "b"} {
		if err := store.CreateAIModel(ctx, &model.AIModel{ModelID: id, URL: "https://x", Currency: "CNY"}); err != nil {
			t.Fatal(err)
		}
	}
	src := billing.NewStoreSource(store)
	if _, err := src.Model(ctx, 1); err != nil {
		t.Fatal(err)
	}
	for _, mid := range []uint{1, 2} {
		if err := store.CreateAIModelPrice(ctx, &model.AIModelPrice{AIModelID: mid, Currency: "CNY",
			EffectiveAt: at, PeakOutputMicros: 1_000_000}); err != nil {
			t.Fatal(err)
		}
	}
	for _, mid := range []uint{1, 2} {
		if got, _ := src.Prices(ctx, mid); len(got) != 1 {
			t.Fatalf("model %d prices = %d, want 1", mid, len(got))
		}
	}

	// 改模型行的计价开关并追加价目，失效前必须仍读缓存。
	if _, err := store.UpdateAIModel(ctx, 1, map[string]any{"currency": "USD"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAIModelPrice(ctx, &model.AIModelPrice{AIModelID: 1, Currency: "USD",
		EffectiveAt: at.Add(time.Hour), PeakOutputMicros: 2_000_000}); err != nil {
		t.Fatal(err)
	}
	if mi, _ := src.Model(ctx, 1); mi.Currency != "CNY" {
		t.Fatalf("model row must stay cached, got %q", mi.Currency)
	}
	if got, _ := src.Prices(ctx, 1); len(got) != 1 {
		t.Fatalf("prices must stay cached, got %d", len(got))
	}

	src.InvalidateModel(1)
	if mi, _ := src.Model(ctx, 1); mi.Currency != "USD" {
		t.Fatalf("model row after invalidate = %q, want USD", mi.Currency)
	}
	if got, _ := src.Prices(ctx, 1); len(got) != 2 {
		t.Fatalf("model 1 prices after invalidate = %d, want 2", len(got))
	}
	// 其他模型的缓存不受牵连。
	if got, _ := src.Prices(ctx, 2); len(got) != 1 {
		t.Fatalf("model 2 prices = %d, want 1", len(got))
	}
}

// 日历按区间缓存：登记新日期后必须显式失效才可见。
func TestStoreSource_CalendarCacheAndInvalidate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	src := billing.NewStoreSource(store)
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	day := from.AddDate(0, 0, 1) // 2026-10-02，周五

	cal, err := src.Calendar(ctx, from, to)
	if err != nil || !cal.IsWorkday(day) {
		t.Fatalf("empty calendar workday=%v err=%v", cal.IsWorkday(day), err)
	}
	if err := store.UpsertBillingCalendarDays(ctx, []model.BillingCalendarDay{{Date: day, IsWorkday: false}}); err != nil {
		t.Fatal(err)
	}
	if cached, _ := src.Calendar(ctx, from, to); !cached.IsWorkday(day) {
		t.Fatal("calendar must stay cached before invalidate")
	}
	src.InvalidateCalendar()
	if fresh, _ := src.Calendar(ctx, from, to); fresh.IsWorkday(day) {
		t.Fatal("holiday override must be visible after invalidate")
	}
}

// 汇率按币种对缓存：追加新汇率后必须显式失效才可见。
func TestStoreSource_RateCacheAndInvalidate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := store.CreateCurrencyRate(ctx, &model.CurrencyRate{FromCurrency: "USD", ToCurrency: "CNY",
		EffectiveAt: at, RateMicros: 7_000_000}); err != nil {
		t.Fatal(err)
	}
	src := billing.NewStoreSource(store)
	first, err := src.Rate(ctx, "USD", "CNY")
	if err != nil || len(first) != 1 || first[0].RateMicros != 7_000_000 {
		t.Fatalf("rate = %+v err=%v", first, err)
	}
	if err := store.CreateCurrencyRate(ctx, &model.CurrencyRate{FromCurrency: "USD", ToCurrency: "CNY",
		EffectiveAt: at.Add(24 * time.Hour), RateMicros: 7_100_000}); err != nil {
		t.Fatal(err)
	}
	if cached, _ := src.Rate(ctx, "USD", "CNY"); len(cached) != 1 {
		t.Fatalf("rate cache should still hold 1, got %d", len(cached))
	}
	src.InvalidateRates()
	if fresh, _ := src.Rate(ctx, "USD", "CNY"); len(fresh) != 2 {
		t.Fatalf("after invalidate want 2, got %d", len(fresh))
	}
	// 未登记的币种对返回空集而非错误（由 SelectRate 判定缺失）。
	if none, err := src.Rate(ctx, "EUR", "CNY"); err != nil || len(none) != 0 {
		t.Fatalf("unknown pair = %+v err=%v", none, err)
	}
}

// 平台结算币种缓存：改设置后必须显式失效才可见。
func TestStoreSource_PlatformCurrencyCacheAndInvalidate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	src := billing.NewStoreSource(store)
	if got, _ := src.PlatformCurrency(ctx); got != "CNY" {
		t.Fatalf("platform = %q, want CNY", got)
	}
	if _, err := store.UpdateSettings(ctx, map[string]any{"platform_currency": "USD"}); err != nil {
		t.Fatal(err)
	}
	if cached, _ := src.PlatformCurrency(ctx); cached != "CNY" {
		t.Fatalf("platform must stay cached, got %q", cached)
	}
	src.InvalidatePlatformCurrency()
	if got, _ := src.PlatformCurrency(ctx); got != "USD" {
		t.Fatalf("platform after invalidate = %q, want USD", got)
	}
}

// 负例：模型不存在必须返回错误且不写缓存；`peak_hours` 非法时回落零值调度，
// 运行期由 IsPeak 判为不可评估，引擎据此 fail-closed 取峰价。
func TestStoreSource_MissingModelAndBrokenSchedule(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	src := billing.NewStoreSource(store)
	if _, err := src.Model(ctx, 404); err == nil {
		t.Fatal("missing model must return error")
	}
	if got, err := src.Prices(ctx, 404); err != nil || len(got) != 0 {
		t.Fatalf("missing model prices = %+v err=%v", got, err)
	}

	at := time.Now().UTC()
	if err := store.CreateAIModelPrice(ctx, &model.AIModelPrice{AIModelID: 7, Currency: "CNY",
		EffectiveAt: at, PeakHours: `{"timezone":`}); err != nil {
		t.Fatal(err)
	}
	got, err := src.Prices(ctx, 7)
	if err != nil || len(got) != 1 {
		t.Fatalf("broken schedule row must survive: %+v err=%v", got, err)
	}
	if len(got[0].Schedule.Windows) != 0 || got[0].Schedule.Timezone != "" {
		t.Fatalf("broken schedule must fall back to zero value: %+v", got[0].Schedule)
	}
	if _, evaluable := billing.IsPeak(at, got[0].Schedule, nil); evaluable {
		t.Fatal("zero schedule must be not evaluable so the engine bills peak")
	}
}
