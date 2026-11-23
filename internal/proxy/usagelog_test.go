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

package proxy_test

import (
	"context"
	"testing"
	"time"

	"aigate/internal/billing"
	"aigate/internal/model"
	"aigate/internal/proxy"
)

// TestUsageWriter_DroppedCountsOverflow 覆盖有界队列满且无消费者时的丢弃计数：
// buffer=1 时第一条入队，后续两条直接丢弃。
func TestUsageWriter_DroppedCountsOverflow(t *testing.T) {
	store, _ := newTestStore(t)

	w := proxy.NewUsageWriter(store, 1, nil)
	// 故意不调用 Start：没有消费者，队列容量恒为 1。
	for i := 0; i < 3; i++ {
		w.Submit(model.LLMUsageLog{TotalTokens: 1, StatusCode: 200})
	}
	if got := w.Dropped(); got < 2 {
		t.Fatalf("Dropped() = %d, want >= 2", got)
	}
}

// TestUsageWriter_StartStopFlushesQueue 覆盖 Start 消费 + Stop 排空后记录落库：
// 表侧断言 llm_usage_logs 逐条落库、daily_usage_summaries 聚合累计。
func TestUsageWriter_StartStopFlushesQueue(t *testing.T) {
	store, dsn := newTestStore(t)
	read := openReadDB(t, dsn)

	w := proxy.NewUsageWriter(store, 8, nil)
	w.Start()
	for i := 0; i < 3; i++ {
		w.Submit(model.LLMUsageLog{
			APIKeyID:         7,
			UserID:           11,
			GroupID:          3,
			AIModelID:        42,
			Model:            "mock-a",
			Provider:         "mock-provider",
			PromptTokens:     10,
			CompletionTokens: 5,
			TotalTokens:      15,
			StatusCode:       200,
		})
	}
	w.Stop(context.Background())

	if got := w.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0 (consumer running)", got)
	}

	if n := countRows(t, read, &model.LLMUsageLog{}); n != 3 {
		t.Fatalf("llm_usage_logs rows = %d, want 3", n)
	}
	if n := countRows(t, read, &model.DailyUsageSummary{}); n != 1 {
		t.Fatalf("daily_usage_summaries rows = %d, want 1 (aggregated by date/user/key/model)", n)
	}

	var summary model.DailyUsageSummary
	if err := read.First(&summary).Error; err != nil {
		t.Fatalf("load daily summary: %v", err)
	}
	if summary.UserID != 11 || summary.APIKeyID != 7 || summary.AIModelID != 42 {
		t.Fatalf("daily summary identity = %+v, want user=11 key=7 model=42", summary)
	}
	if summary.PromptTokens != 30 || summary.CompletionTokens != 15 || summary.TotalTokens != 45 {
		t.Fatalf("daily summary tokens = %+v, want prompt=30 completion=15 total=45", summary)
	}
	if summary.RequestCount != 3 {
		t.Fatalf("daily summary request_count = %d, want 3", summary.RequestCount)
	}
}

// TestUsageWriter_SubmitAfterStopDoesNotPanic 覆盖停机后的 Submit：
// 不得 panic（send on closed channel），并计入丢弃；重复 Stop 也不 panic。
func TestUsageWriter_SubmitAfterStopDoesNotPanic(t *testing.T) {
	store, _ := newTestStore(t)

	w := proxy.NewUsageWriter(store, 4, nil)
	w.Start()
	w.Stop(context.Background())

	before := w.Dropped()
	for i := 0; i < 2; i++ {
		w.Submit(model.LLMUsageLog{TotalTokens: 1})
	}
	if got := w.Dropped() - before; got != 2 {
		t.Fatalf("dropped after stop = %d, want 2", got)
	}
	w.Stop(context.Background()) // 重复 Stop：空操作
}

// ---------------------------------------------------------------------------
// 计价：消费侧把一笔请求折算成两个金额列
// ---------------------------------------------------------------------------

// okSource 让 Engine 计价成功：CNY 模型、一条已生效的空闲价目、同币种无需汇率。
type okSource struct{}

func (okSource) Model(context.Context, uint) (billing.ModelInfo, error) {
	return billing.ModelInfo{Currency: "CNY", UseUpstreamCost: true}, nil
}

func (okSource) Prices(_ context.Context, _ uint) ([]billing.DatedPrice, error) {
	return []billing.DatedPrice{{EffectiveAt: time.Now().Add(-time.Hour), Currency: "CNY",
		Price: billing.Price{OffpeakOutputMicros: 4_000_000}}}, nil
}

func (okSource) Rate(context.Context, string, string) ([]billing.DatedRate, error) { return nil, nil }

func (okSource) Calendar(context.Context, time.Time, time.Time) (billing.Calendar, error) {
	return billing.NewCalendar(nil), nil
}

func (okSource) PlatformCurrency(context.Context) (string, error) { return "CNY", nil }

// brokenSource 让 Engine 计价失败：没有任何生效价目。
type brokenSource struct{ okSource }

func (brokenSource) Prices(context.Context, uint) ([]billing.DatedPrice, error) { return nil, nil }

// missingRateSource 只缺汇率：模型币种 USD、平台 CNY 且无汇率，引擎仍返回模型币种金额。
type missingRateSource struct{ okSource }

func (missingRateSource) Model(context.Context, uint) (billing.ModelInfo, error) {
	return billing.ModelInfo{Currency: "USD", UseUpstreamCost: true}, nil
}

func TestUsageWriter_StoresTwoCostColumns(t *testing.T) {
	store, dsn := newTestStore(t)
	w := proxy.NewUsageWriter(store, 8, billing.NewEngine(okSource{}, time.UTC))
	w.Start()
	// 1000 输出 token × 4 元/百万 = 0.004 元 = 4000 微元
	w.Submit(model.LLMUsageLog{UserID: 1, AIModelID: 7, CompletionTokens: 1000, TotalTokens: 1000, CreatedAt: time.Now().UTC()})
	w.Stop(context.Background())
	logs, _, err := store.ListUsageLogs(context.Background(), model.UsageFilter{}, 1, 10)
	if err != nil || len(logs) != 1 {
		t.Fatalf("logs = %+v err=%v", logs, err)
	}
	if logs[0].CostVendorMicros != 4000 || logs[0].CostPlatformMicros != 4000 {
		t.Fatalf("costs = %+v, want 4000/4000", logs[0])
	}
	// 日聚合与明细同口径累加。
	var summary model.DailyUsageSummary
	read := openReadDB(t, dsn)
	if err := read.First(&summary).Error; err != nil {
		t.Fatalf("load daily summary: %v", err)
	}
	if summary.CostVendorMicros != 4000 || summary.CostPlatformMicros != 4000 || summary.Currency != "CNY" {
		t.Fatalf("daily summary cost = %+v, want 4000/4000 CNY", summary)
	}
}

func TestUsageWriter_BillingErrorStillWritesRow(t *testing.T) {
	store, _ := newTestStore(t)
	w := proxy.NewUsageWriter(store, 8, billing.NewEngine(brokenSource{}, time.UTC))
	w.Start()
	w.Submit(model.LLMUsageLog{UserID: 1, AIModelID: 7, TotalTokens: 10, CreatedAt: time.Now().UTC()})
	w.Stop(context.Background())
	logs, _, err := store.ListUsageLogs(context.Background(), model.UsageFilter{}, 1, 10)
	if err != nil || len(logs) != 1 {
		t.Fatalf("row must survive billing failure: %+v err=%v", logs, err)
	}
	if logs[0].CostVendorMicros != 0 || logs[0].CostPlatformMicros != 0 {
		t.Fatalf("costs must be zero on failure: %+v", logs[0])
	}
}

// TestUsageWriter_MissingRateKeepsVendorCost 覆盖「仅缺汇率」：模型币种金额照常落库，
// 平台币种金额记 0，绝不因为折算失败就把整行金额清零。
func TestUsageWriter_MissingRateKeepsVendorCost(t *testing.T) {
	store, _ := newTestStore(t)
	w := proxy.NewUsageWriter(store, 8, billing.NewEngine(missingRateSource{}, time.UTC))
	w.Start()
	w.Submit(model.LLMUsageLog{UserID: 1, AIModelID: 7, CompletionTokens: 1000, TotalTokens: 1000, CreatedAt: time.Now().UTC()})
	w.Stop(context.Background())
	logs, _, err := store.ListUsageLogs(context.Background(), model.UsageFilter{}, 1, 10)
	if err != nil || len(logs) != 1 {
		t.Fatalf("logs = %+v err=%v", logs, err)
	}
	if logs[0].CostVendorMicros != 4000 || logs[0].CostPlatformMicros != 0 {
		t.Fatalf("costs = %+v, want vendor=4000 platform=0", logs[0])
	}
}

// TestUsageWriter_ReportedCostPreferred 覆盖上游自报优先：自报 0.5 单位 = 500000 微元，
// 不得被空闲价目自算的 4000 覆盖。
func TestUsageWriter_ReportedCostPreferred(t *testing.T) {
	store, _ := newTestStore(t)
	w := proxy.NewUsageWriter(store, 8, billing.NewEngine(okSource{}, time.UTC))
	w.Start()
	reported := 0.5
	w.Submit(model.LLMUsageLog{UserID: 1, AIModelID: 7, CompletionTokens: 1000, TotalTokens: 1000,
		ReportedCost: &reported, CreatedAt: time.Now().UTC()})
	w.Stop(context.Background())
	logs, _, err := store.ListUsageLogs(context.Background(), model.UsageFilter{}, 1, 10)
	if err != nil || len(logs) != 1 {
		t.Fatalf("logs = %+v err=%v", logs, err)
	}
	if logs[0].CostVendorMicros != 500_000 || logs[0].CostPlatformMicros != 500_000 {
		t.Fatalf("costs = %+v, want 500000/500000 from upstream report", logs[0])
	}
}

// TestUsageWriter_NilBillerSkipsPricing 覆盖未装配计价：行照常落库、金额保持 0。
func TestUsageWriter_NilBillerSkipsPricing(t *testing.T) {
	store, _ := newTestStore(t)
	w := proxy.NewUsageWriter(store, 8, nil)
	w.Start()
	w.Submit(model.LLMUsageLog{UserID: 1, AIModelID: 7, TotalTokens: 10, CreatedAt: time.Now().UTC()})
	w.Stop(context.Background())
	logs, _, err := store.ListUsageLogs(context.Background(), model.UsageFilter{}, 1, 10)
	if err != nil || len(logs) != 1 {
		t.Fatalf("logs = %+v err=%v", logs, err)
	}
	if logs[0].CostVendorMicros != 0 || logs[0].CostPlatformMicros != 0 || logs[0].Currency != "" {
		t.Fatalf("costs = %+v, want all zero without biller", logs[0])
	}
}

// TestUsageLog_TransportFieldsNotPersisted 覆盖 gorm:"-" 传输字段不进库：
// 落库后回读，ReportedCost 必须为空（否则上游自报成本会随每次读行重现）。
func TestUsageLog_TransportFieldsNotPersisted(t *testing.T) {
	store, dsn := newTestStore(t)
	reported := 1.25
	entry := model.LLMUsageLog{UserID: 1, AIModelID: 7, TotalTokens: 1, CreatedAt: time.Now().UTC(),
		ReportedCost: &reported, Currency: "CNY"}
	if err := store.InsertUsageLog(context.Background(), &entry); err != nil {
		t.Fatalf("insert usage log: %v", err)
	}
	read := openReadDB(t, dsn)
	var row model.LLMUsageLog
	if err := read.Order("id DESC").First(&row).Error; err != nil {
		t.Fatalf("load usage log: %v", err)
	}
	if row.ReportedCost != nil || row.Currency != "" {
		t.Fatalf("transport fields leaked into database: %+v", row)
	}
	// 表结构里也不得出现这两列。
	if read.Migrator().HasColumn(&model.LLMUsageLog{}, "reported_cost") ||
		read.Migrator().HasColumn(&model.LLMUsageLog{}, "currency") {
		t.Fatalf("transport fields must not be columns of llm_usage_logs")
	}
}
