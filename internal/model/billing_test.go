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

package model_test

// 按金额计费的数据层：价目快照、计费日历、汇率，以及用量表的两个金额列。

import (
	"testing"
	"time"

	"aigate/internal/model"
)

// 价目六列原始值与时段 JSON 全字段回读。
func TestAIModelPrice_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx()
	p := &model.AIModelPrice{
		AIModelID: 1, Currency: "CNY",
		EffectiveAt:            time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		PeakHours:              `{"timezone":"Asia/Shanghai","windows":[]}`,
		PeakInputMissMicros:    9_000_000,
		OffpeakInputMissMicros: 4_500_000,
	}
	if err := s.CreateAIModelPrice(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.ListAIModelPrices(ctx, 1)
	if err != nil || len(got) != 1 || got[0].OffpeakInputMissMicros != 4_500_000 {
		t.Fatalf("list = %+v err=%v", got, err)
	}
	if got[0].Currency != "CNY" || got[0].PeakHours != p.PeakHours {
		t.Fatalf("row = %+v, want currency CNY and peak_hours %q", got[0], p.PeakHours)
	}
	if !got[0].EffectiveAt.Equal(p.EffectiveAt) {
		t.Fatalf("EffectiveAt = %s, want %s", got[0].EffectiveAt, p.EffectiveAt)
	}
	if got[0].CreatedAt.IsZero() {
		t.Fatal("CreatedAt should be filled in")
	}
}

// 同一模型同一生效时刻只允许一条价目（uk_ai_model_prices_effective）。
func TestAIModelPrice_UniqueEffectiveAt(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx()
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := s.CreateAIModelPrice(ctx, &model.AIModelPrice{AIModelID: 1, Currency: "CNY", EffectiveAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAIModelPrice(ctx, &model.AIModelPrice{AIModelID: 1, Currency: "CNY", EffectiveAt: at}); err == nil {
		t.Fatal("duplicate effective_at must be rejected")
	}
	// 另一个模型同生效时刻是不同快照，允许。
	if err := s.CreateAIModelPrice(ctx, &model.AIModelPrice{AIModelID: 2, Currency: "CNY", EffectiveAt: at}); err != nil {
		t.Fatalf("same effective_at on another model: %v", err)
	}
}

// 价目列表按 effective_at 倒序（新快照在前）。
func TestListAIModelPrices_NewestFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{base, base.Add(2 * time.Hour), base.Add(time.Hour)} {
		if err := s.CreateAIModelPrice(ctx, &model.AIModelPrice{AIModelID: 7, Currency: "CNY", EffectiveAt: at}); err != nil {
			t.Fatalf("create %s: %v", at, err)
		}
	}
	got, err := s.ListAIModelPrices(ctx, 7)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("prices = %d, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if !got[i-1].EffectiveAt.After(got[i].EffectiveAt) {
			t.Fatalf("prices not in effective_at DESC order: %s before %s", got[i-1].EffectiveAt, got[i].EffectiveAt)
		}
	}
}

// 删除价目后不再出现在列表里。
func TestDeleteAIModelPrice(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx()
	p := &model.AIModelPrice{AIModelID: 1, Currency: "CNY", EffectiveAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	if err := s.CreateAIModelPrice(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAIModelPrice(ctx, p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, _ := s.ListAIModelPrices(ctx, 1); len(got) != 0 {
		t.Fatalf("prices left after delete: %+v", got)
	}
}

// 删除模型时其价目在同一事务内级联删除。
func TestDeleteAIModel_CascadesPrices(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx()
	m := &model.AIModel{ModelID: "m", URL: "https://x", Currency: "CNY"}
	if err := s.CreateAIModel(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAIModelPrice(ctx, &model.AIModelPrice{AIModelID: m.ID, Currency: "CNY", EffectiveAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAIModel(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListAIModelPrices(ctx, m.ID); len(got) != 0 {
		t.Fatalf("prices must be deleted with the model: %d left", len(got))
	}
}

// 日历按日期主键覆盖写：同一日期先登记节假日、再改为工作日，只留一行。
func TestBillingCalendar_UpsertOverrides(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx()
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := s.UpsertBillingCalendarDays(ctx, []model.BillingCalendarDay{{Date: day, IsWorkday: false, Note: "国庆"}}); err != nil {
		t.Fatal(err)
	}
	// 再改回工作日：覆盖写，行数仍为 1。
	if err := s.UpsertBillingCalendarDays(ctx, []model.BillingCalendarDay{{Date: day, IsWorkday: true}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListBillingCalendarDays(ctx, day, day)
	if err != nil || len(got) != 1 || !got[0].IsWorkday {
		t.Fatalf("calendar = %+v err=%v", got, err)
	}
	if got[0].Note != "" {
		t.Fatalf("Note = %q, want overwritten to empty", got[0].Note)
	}
	if got[0].UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt should be filled in")
	}
	// 反向再覆盖一次：节假日（false 是零值）也必须真的写进去。
	if err := s.UpsertBillingCalendarDays(ctx, []model.BillingCalendarDay{{Date: day, IsWorkday: false, Note: "补登"}}); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListBillingCalendarDays(ctx, day, day)
	if err != nil || len(got) != 1 || got[0].IsWorkday || got[0].Note != "补登" {
		t.Fatalf("calendar = %+v err=%v, want single holiday row with note", got, err)
	}
}

// 日历区间含首含尾，区间外的日期不返回。
func TestListBillingCalendarDays_RangeInclusive(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	days := make([]model.BillingCalendarDay, 0, 5)
	for i := 0; i < 5; i++ {
		days = append(days, model.BillingCalendarDay{Date: base.AddDate(0, 0, i), IsWorkday: i%2 == 0})
	}
	if err := s.UpsertBillingCalendarDays(ctx, days); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListBillingCalendarDays(ctx, base.AddDate(0, 0, 1), base.AddDate(0, 0, 3))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("days = %d, want 3 (%+v)", len(got), got)
	}
	for i, want := range []time.Time{base.AddDate(0, 0, 1), base.AddDate(0, 0, 2), base.AddDate(0, 0, 3)} {
		if !got[i].Date.Equal(want) {
			t.Fatalf("days[%d].Date = %s, want %s (按日期升序)", i, got[i].Date, want)
		}
	}
}

// 单个日期登记可被删除。
func TestDeleteBillingCalendarDay(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx()
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := s.UpsertBillingCalendarDays(ctx, []model.BillingCalendarDay{{Date: day, IsWorkday: false}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBillingCalendarDay(ctx, day); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, _ := s.ListBillingCalendarDays(ctx, day, day); len(got) != 0 {
		t.Fatalf("days left after delete: %+v", got)
	}
}

// 汇率往返；同一币种对同一生效时刻只允许一条。
func TestCurrencyRate_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx()
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	r := &model.CurrencyRate{FromCurrency: "USD", ToCurrency: "CNY", EffectiveAt: at, RateMicros: 7_100_000}
	if err := s.CreateCurrencyRate(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListCurrencyRates(ctx, "USD", "CNY")
	if err != nil || len(got) != 1 || got[0].RateMicros != 7_100_000 {
		t.Fatalf("rates = %+v err=%v", got, err)
	}
	if got[0].CreatedAt.IsZero() {
		t.Fatal("CreatedAt should be filled in")
	}
	if err := s.CreateCurrencyRate(ctx, &model.CurrencyRate{FromCurrency: "USD", ToCurrency: "CNY", EffectiveAt: at, RateMicros: 7_200_000}); err == nil {
		t.Fatal("duplicate (from, to, effective_at) must be rejected")
	}
}

// 汇率按币种对过滤且按 effective_at 倒序；删除后不再返回。
func TestListCurrencyRates_FiltersAndOrders(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, row := range []model.CurrencyRate{
		{FromCurrency: "USD", ToCurrency: "CNY", EffectiveAt: base, RateMicros: 7_000_000},
		{FromCurrency: "USD", ToCurrency: "CNY", EffectiveAt: base.Add(time.Hour), RateMicros: 7_100_000},
		{FromCurrency: "EUR", ToCurrency: "CNY", EffectiveAt: base, RateMicros: 7_800_000},
	} {
		row := row
		if err := s.CreateCurrencyRate(ctx, &row); err != nil {
			t.Fatalf("create %s/%s: %v", row.FromCurrency, row.ToCurrency, err)
		}
	}
	got, err := s.ListCurrencyRates(ctx, "USD", "CNY")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 || got[0].RateMicros != 7_100_000 || got[1].RateMicros != 7_000_000 {
		t.Fatalf("rates = %+v, want USD/CNY newest first", got)
	}
	if err := s.DeleteCurrencyRate(ctx, got[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if left, _ := s.ListCurrencyRates(ctx, "USD", "CNY"); len(left) != 1 || left[0].RateMicros != 7_000_000 {
		t.Fatalf("rates after delete = %+v", left)
	}
}

// 消息记录表只有两列金额：累加正确，且不引入分项列。
func TestUpsertDailyUsage_AccumulatesTwoCostColumns(t *testing.T) {
	store, dsn := newTestStoreAt(t)
	db := openReadDB(t, dsn)
	ctx := testCtx()
	for _, c := range [][2]int64{{150, 1050}, {250, 1750}} {
		if err := store.UpsertDailyUsage(ctx, &model.LLMUsageLog{UserID: 1, TotalTokens: 10, Currency: "CNY",
			CostVendorMicros: c[0], CostPlatformMicros: c[1]}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	var row model.DailyUsageSummary
	if err := db.Where("user_id = 1").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.CostVendorMicros != 400 || row.CostPlatformMicros != 2800 || row.Currency != "CNY" {
		t.Fatalf("daily = %+v", row)
	}
	// 币种首写即定：后续写入的新币种不覆盖已有行（currency 不在 DoUpdates 中）。
	if err := store.UpsertDailyUsage(ctx, &model.LLMUsageLog{UserID: 1, TotalTokens: 10, Currency: "USD"}); err != nil {
		t.Fatalf("upsert with other currency: %v", err)
	}
	if err := db.Where("user_id = 1").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Currency != "CNY" {
		t.Fatalf("Currency = %q, want CNY (currency must not be overwritten)", row.Currency)
	}
}

// 结算币种默认 CNY：首建默认行即带值，后续可更新。
func TestGetSettings_PlatformCurrencyDefaultsToCNY(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx()
	st, err := s.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if st.PlatformCurrency != "CNY" {
		t.Fatalf("PlatformCurrency = %q, want CNY", st.PlatformCurrency)
	}
	updated, err := s.UpdateSettings(ctx, map[string]any{"platform_currency": "USD"})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if updated.PlatformCurrency != "USD" {
		t.Fatalf("PlatformCurrency = %q, want USD", updated.PlatformCurrency)
	}
}

// 窗口内金额求和：按用户过滤，左闭右开；未配置金额的旧行仍计入 0。
func TestSumUsageCostMicrosInWindow(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx()
	base := time.Now().UTC().Add(-time.Hour)
	for i, c := range []int64{100, 200, 400} {
		entry := &model.LLMUsageLog{
			UserID: 1, TotalTokens: 1, CostPlatformMicros: c,
			CostVendorMicros: c / 2, CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if err := s.InsertUsageLog(ctx, entry); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	// 另一个用户的金额不计入。
	if err := s.InsertUsageLog(ctx, &model.LLMUsageLog{UserID: 2, TotalTokens: 1, CostPlatformMicros: 9999, CreatedAt: base}); err != nil {
		t.Fatal(err)
	}
	got, err := s.SumUsageCostMicrosInWindow(ctx, model.UsageFilter{UserID: 1}, base.Add(-time.Minute), nil)
	if err != nil || got != 700 {
		t.Fatalf("sum = %d err=%v, want 700", got, err)
	}
	// to 为开区间上界：只计入前两条。
	to := base.Add(2 * time.Minute)
	got, err = s.SumUsageCostMicrosInWindow(ctx, model.UsageFilter{UserID: 1}, base, &to)
	if err != nil || got != 300 {
		t.Fatalf("sum with upper bound = %d err=%v, want 300", got, err)
	}
	if got, err := s.SumUsageCostMicrosInWindow(ctx, model.UsageFilter{UserID: 3}, base, nil); err != nil || got != 0 {
		t.Fatalf("sum for unknown user = %d err=%v, want 0", got, err)
	}
}

// count_toward_quota=false 的用量（网关内部开销，如 auto 路由的决策调用）必须：
//   - 排除出四类 token 求和与 cost 求和（这些规则读 llm_usage_logs 明细表）；
//   - 不进入日聚合表（模型日配额读 daily_usage_summaries）。
//
// 这是 design §7.7 方案 a 的核心不变量，也是这次改动最大的回归面（§9）。
func TestCountTowardQuota_ExcludedFromAllQuotaSums(t *testing.T) {
	s, dsn := newTestStoreAt(t)
	ctx := testCtx()
	base := time.Now().UTC().Add(-time.Hour)

	const (
		userID  = 7
		keyID   = 11
		groupID = 13
		modelID = 17
	)
	// 两条用量：一条普通请求（计入），一条网关内部开销（不计入）。
	counted := &model.LLMUsageLog{
		UserID: userID, APIKeyID: keyID, GroupID: groupID, AIModelID: modelID,
		TotalTokens: 100, PromptTokens: 100,
		CostPlatformMicros: 500, CostVendorMicros: 400,
		CreatedAt: base,
	}
	counted.SetCountTowardQuota(true)
	internal := &model.LLMUsageLog{
		UserID: userID, APIKeyID: keyID, GroupID: groupID, AIModelID: modelID,
		TotalTokens: 900, PromptTokens: 900,
		CostPlatformMicros: 5000, CostVendorMicros: 4000,
		CreatedAt: base.Add(time.Minute),
	}
	internal.SetCountTowardQuota(false)

	for _, e := range []*model.LLMUsageLog{counted, internal} {
		if err := s.InsertUsageLog(ctx, e); err != nil {
			t.Fatalf("insert usage: %v", err)
		}
		// 与 UsageWriter 的消费行为一致：不计配额的行跳过日聚合。
		if e.CountsTowardQuota() {
			if err := s.UpsertDailyUsage(ctx, e); err != nil {
				t.Fatalf("upsert daily: %v", err)
			}
		}
	}

	from := base.Add(-time.Minute)
	cases := []struct {
		name string
		run  func() (int64, error)
		want int64
	}{
		{"user tokens", func() (int64, error) { return s.SumUserTokensInWindow(ctx, userID, 0, from, nil) }, 100},
		{"key tokens", func() (int64, error) { return s.SumKeyTokensInWindow(ctx, keyID, from, nil) }, 100},
		{"group tokens", func() (int64, error) { return s.SumGroupTokensInWindow(ctx, groupID, from, nil) }, 100},
		{"global tokens", func() (int64, error) { return s.SumGlobalTokensInWindow(ctx, from, nil) }, 100},
		{"cost micros", func() (int64, error) {
			return s.SumUsageCostMicrosInWindow(ctx, model.UsageFilter{UserID: userID}, from, nil)
		}, 500},
		{"model daily tokens", func() (int64, error) { return s.SumModelDailyTokens(ctx, modelID) }, 100},
	}
	for _, tc := range cases {
		got, err := tc.run()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s = %d, want %d (gateway-internal usage must not be counted)", tc.name, got, tc.want)
		}
	}

	// 明细表仍保留两行：成本可查，只是不计配额。
	db := openReadDB(t, dsn)
	if got := countRows(t, db, &model.LLMUsageLog{}, ""); got != 2 {
		t.Fatalf("usage detail rows = %d, want 2 (both rows must remain auditable)", got)
	}
	if got := countRows(t, db, &model.DailyUsageSummary{}, ""); got != 1 {
		t.Fatalf("daily summary rows = %d, want 1 (uncounted rows skip the rollup)", got)
	}
}
