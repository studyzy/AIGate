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

package billing

// 本文件是 billing 包的黑盒测试夹具与计价引擎用例。
// fakeSource 以内存数据实现 Source 接口，使引擎用例不触达数据库与时钟。

import (
	"context"
	"testing"
	"time"
)

type fakeSource struct {
	model    ModelInfo
	prices   []DatedPrice
	rate     []DatedRate
	cal      Calendar
	platform string
	err      error
}

func (f fakeSource) Model(context.Context, uint) (ModelInfo, error)            { return f.model, f.err }
func (f fakeSource) Prices(context.Context, uint) ([]DatedPrice, error)        { return f.prices, f.err }
func (f fakeSource) Rate(context.Context, string, string) ([]DatedRate, error) { return f.rate, f.err }
func (f fakeSource) Calendar(context.Context, time.Time, time.Time) (Calendar, error) {
	if f.cal == nil {
		return NewCalendar(nil), nil
	}
	return f.cal, nil
}
func (f fakeSource) PlatformCurrency(context.Context) (string, error) { return f.platform, nil }

var _ Source = fakeSource{}

func ptr(f float64) *float64 { return &f }

// offpeakAt 是确定性的空闲时刻：2026-11-21 周六 03:00 UTC。
// 用例一律使用固定时刻，不依赖 time.Now()，避免随执行时刻变档导致偶发失败。
var offpeakAt = time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)

func TestEngine_OffpeakScheduleSelectsOffpeakColumns(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, loc) // 周一 10:00 → 高峰
	src := fakeSource{
		model: ModelInfo{Currency: "CNY", EnablePeak: true, UseUpstreamCost: false},
		prices: []DatedPrice{{EffectiveAt: at.Add(-24 * time.Hour), Currency: "CNY",
			Price: Price{PeakOutputMicros: 8_000_000, OffpeakOutputMicros: 4_000_000}}},
		platform: "CNY",
	}
	r, err := NewEngine(src, loc).Compute(context.Background(), 1, Input{At: at, Tokens: Tokens{Output: 1_000_000}})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Peak || r.VendorMicros != 8_000_000 || r.FromUpstream {
		t.Fatalf("peak result = %+v", r)
	}
}

func TestEngine_UpstreamReportedCostWins(t *testing.T) {
	loc := time.UTC
	at := offpeakAt
	src := fakeSource{
		model: ModelInfo{Currency: "CNY", UseUpstreamCost: true},
		prices: []DatedPrice{{EffectiveAt: at.Add(-time.Hour), Currency: "CNY",
			Price: Price{PeakOutputMicros: 99_000_000}}},
		platform: "CNY",
	}
	r, err := NewEngine(src, loc).Compute(context.Background(), 1,
		Input{At: at, Tokens: Tokens{Output: 1_000_000}, ReportedCost: ptr(0.00176)})
	if err != nil {
		t.Fatal(err)
	}
	if !r.FromUpstream || r.VendorMicros != 1760 {
		t.Fatalf("upstream result = %+v, want 1760 from upstream", r)
	}
}

func TestEngine_UpstreamIgnoredWhenDisabled(t *testing.T) {
	loc := time.UTC
	at := offpeakAt // 周六
	// 仅工作日 09:00–12:00 为高峰：周六落在窗口 days 之外 → 可评估且非高峰。
	sched, _ := ParsePeakSchedule(`{"timezone":"UTC","windows":[{"days":[1,2,3,4,5],"start":"09:00","end":"12:00"}]}`)
	src := fakeSource{
		model: ModelInfo{Currency: "CNY", EnablePeak: true, UseUpstreamCost: false},
		prices: []DatedPrice{{EffectiveAt: at.Add(-time.Hour), Currency: "CNY", Schedule: sched,
			Price: Price{OffpeakOutputMicros: 4_000_000}}},
		platform: "CNY",
	}
	r, err := NewEngine(src, loc).Compute(context.Background(), 1,
		Input{At: at, Tokens: Tokens{Output: 1_000_000}, ReportedCost: ptr(9.99)})
	if err != nil {
		t.Fatal(err)
	}
	if r.FromUpstream || r.VendorMicros != 4_000_000 {
		t.Fatalf("result = %+v, want catalog 4000000", r)
	}
}

// 时区非法 → fail-closed 取高峰价
func TestEngine_UnevaluableScheduleBillsPeak(t *testing.T) {
	loc := time.UTC
	at := offpeakAt
	sched, _ := ParsePeakSchedule(`{"timezone":"Not/AZone","windows":[{"days":[1],"start":"09:00","end":"12:00"}]}`)
	src := fakeSource{
		model: ModelInfo{Currency: "CNY", EnablePeak: true},
		prices: []DatedPrice{{EffectiveAt: at.Add(-time.Hour), Currency: "CNY", Schedule: sched,
			Price: Price{PeakOutputMicros: 8_000_000, OffpeakOutputMicros: 4_000_000}}},
		platform: "CNY",
	}
	r, err := NewEngine(src, loc).Compute(context.Background(), 1, Input{At: at, Tokens: Tokens{Output: 1_000_000}})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Peak || r.VendorMicros != 8_000_000 {
		t.Fatalf("unevaluable must bill peak: %+v", r)
	}
}

// enable_peak_pricing=false 从不高价计价：即使价目行带着无法评估的时段。
func TestEngine_PeakDisabledNeverBillsPeak(t *testing.T) {
	loc := time.UTC
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, loc) // 周一 10:00，若启用峰谷则为高峰
	sched, _ := ParsePeakSchedule(`{"timezone":"Not/AZone","windows":[{"days":[1],"start":"09:00","end":"12:00"}]}`)
	src := fakeSource{
		model: ModelInfo{Currency: "CNY", EnablePeak: false},
		prices: []DatedPrice{{EffectiveAt: at.Add(-time.Hour), Currency: "CNY", Schedule: sched,
			Price: Price{PeakOutputMicros: 8_000_000, OffpeakOutputMicros: 4_000_000}}},
		platform: "CNY",
	}
	r, err := NewEngine(src, loc).Compute(context.Background(), 1, Input{At: at, Tokens: Tokens{Output: 1_000_000}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Peak || r.VendorMicros != 4_000_000 {
		t.Fatalf("peak-disabled must bill offpeak: %+v", r)
	}
}

func TestEngine_CrossCurrencyUsesRate(t *testing.T) {
	at := offpeakAt
	src := fakeSource{
		model: ModelInfo{Currency: "USD", EnablePeak: false},
		prices: []DatedPrice{{EffectiveAt: at.Add(-time.Hour), Currency: "USD",
			Price: Price{OffpeakOutputMicros: 8_000_000}}},
		rate:     []DatedRate{{EffectiveAt: at.Add(-time.Hour), RateMicros: 7_100_000}},
		platform: "CNY",
	}
	r, err := NewEngine(src, time.UTC).Compute(context.Background(), 1, Input{At: at, Tokens: Tokens{Output: 1_000_000}})
	if err != nil {
		t.Fatal(err)
	}
	// 8 USD = 8_000_000 微元 → ×7.1 = 56_800_000 微元 CNY
	if r.VendorMicros != 8_000_000 || r.PlatformMicros != 56_800_000 || r.PlatformCurrency != "CNY" {
		t.Fatalf("result = %+v", r)
	}
}

func TestEngine_NoPriceIsError(t *testing.T) {
	src := fakeSource{model: ModelInfo{Currency: "CNY"}, platform: "CNY"}
	if _, err := NewEngine(src, time.UTC).Compute(context.Background(), 1, Input{At: offpeakAt, Tokens: Tokens{Output: 1}}); err == nil {
		t.Fatal("missing price must return error so caller stores zero and warns")
	}
}

func TestEngine_MissingRateStillReturnsVendorAmount(t *testing.T) {
	at := offpeakAt
	src := fakeSource{
		model: ModelInfo{Currency: "USD", EnablePeak: false},
		prices: []DatedPrice{{EffectiveAt: at.Add(-time.Hour), Currency: "USD",
			Price: Price{OffpeakOutputMicros: 8_000_000}}},
		platform: "CNY",
	}
	r, err := NewEngine(src, time.UTC).Compute(context.Background(), 1, Input{At: at, Tokens: Tokens{Output: 1_000_000}})
	if err == nil {
		t.Fatal("missing rate must surface as error")
	}
	if r.VendorMicros != 8_000_000 || r.PlatformMicros != 0 {
		t.Fatalf("vendor amount must survive: %+v", r)
	}
}
