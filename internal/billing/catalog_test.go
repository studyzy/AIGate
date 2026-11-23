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
	"math"
	"testing"
	"time"

	"aigate/internal/billing"
)

func TestSelectPrice_PicksLatestEffective(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	prices := []billing.DatedPrice{
		{EffectiveAt: t0, Price: billing.Price{PeakOutputMicros: 1}},
		{EffectiveAt: t1, Price: billing.Price{PeakOutputMicros: 2}},
	}
	if got, ok := billing.SelectPrice(prices, t1.Add(-time.Second)); !ok || got.Price.PeakOutputMicros != 1 {
		t.Fatalf("before t1 = %+v ok=%v", got, ok)
	}
	if got, ok := billing.SelectPrice(prices, t1); !ok || got.Price.PeakOutputMicros != 2 {
		t.Fatalf("at t1 = %+v ok=%v", got, ok)
	}
	if _, ok := billing.SelectPrice(nil, time.Now()); ok {
		t.Fatal("empty catalog must not match")
	}
	future := []billing.DatedPrice{{EffectiveAt: time.Now().Add(time.Hour)}}
	if _, ok := billing.SelectPrice(future, time.Now()); ok {
		t.Fatal("future-only catalog must not match")
	}
}

func TestSelectPrice_SortsUnorderedInput(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	prices := []billing.DatedPrice{
		{EffectiveAt: t1, Price: billing.Price{PeakOutputMicros: 2}},
		{EffectiveAt: t0, Price: billing.Price{PeakOutputMicros: 1}},
	}
	if got, _ := billing.SelectPrice(prices, t1); got.Price.PeakOutputMicros != 2 {
		t.Fatalf("unordered input = %+v", got)
	}
}

func TestConvertMicros_ForwardRate(t *testing.T) {
	// 1 USD = 7.1 CNY；100 USD = 100e6 微元 → 710e6 微元
	if got := billing.ConvertMicros(100*billing.MicrosPerUnit, 7_100_000); got != 710*billing.MicrosPerUnit {
		t.Fatalf("convert = %d", got)
	}
}

func TestConvertMicros_Identity(t *testing.T) {
	if got := billing.ConvertMicros(12345, billing.SameCurrencyRate); got != 12345 {
		t.Fatalf("identity = %d", got)
	}
}

// 溢出边界：折算结果超出 int64 才饱和到 MaxInt64；中间乘积超 64 位但结果可表示时必须正常返回。
func TestConvertMicros_SaturatesOnlyOnTrueOverflow(t *testing.T) {
	if got := billing.ConvertMicros(math.MaxInt64, billing.MicrosPerUnit); got != math.MaxInt64 {
		t.Fatalf("overflow = %d, want MaxInt64", got)
	}
	// 乘积 1e21 超过 int64，但折算结果 1e12 × 1e9 / 1e6 = 1e15 可表示。
	if got := billing.ConvertMicros(1_000_000_000_000, 1_000_000_000); got != 1_000_000_000_000_000 {
		t.Fatalf("wide product = %d, want 1000000000000000", got)
	}
}

func TestSelectRate_LatestEffective(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	rates := []billing.DatedRate{
		{EffectiveAt: base, RateMicros: 7_000_000},
		{EffectiveAt: base.Add(time.Hour), RateMicros: 7_100_000},
	}
	if got, ok := billing.SelectRate(rates, base.Add(30*time.Minute)); !ok || got != 7_000_000 {
		t.Fatalf("rate = %d ok=%v", got, ok)
	}
	if _, ok := billing.SelectRate(nil, base); ok {
		t.Fatal("empty rates must not match")
	}
}
