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
	"testing"

	"aigate/internal/billing"
)

func TestCostMicros_ReadsRawColumnsByTier(t *testing.T) {
	// DeepSeek V4-Pro：高峰 命中0.3元/未命中9元/输出27元；空闲 0.15/4.5/13.5（每百万）
	p := billing.Price{
		PeakInputHitMicros:     300000,
		PeakInputMissMicros:    9000000,
		PeakOutputMicros:       27000000,
		OffpeakInputHitMicros:  150000,
		OffpeakInputMissMicros: 4500000,
		OffpeakOutputMicros:    13500000,
	}
	tok := billing.Tokens{InputCacheHit: 1_000_000, InputCacheMiss: 1_000_000, Output: 1_000_000}
	if got := billing.CostMicros(tok, p, true); got != 300000+9000000+27000000 {
		t.Fatalf("peak = %d, want 36300000", got)
	}
	if got := billing.CostMicros(tok, p, false); got != 150000+4500000+13500000 {
		t.Fatalf("offpeak = %d, want 18150000", got)
	}
}

func TestCostMicros_ZeroPriceIsZero(t *testing.T) {
	if got := billing.CostMicros(billing.Tokens{InputCacheMiss: 1_000_000}, billing.Price{}, true); got != 0 {
		t.Fatalf("zero price = %d", got)
	}
}

func TestToTokens_HitIsClampedToPrompt(t *testing.T) {
	got := billing.ToTokens(300, 100, 1000, 50)
	if got.InputCacheHit != 300 || got.InputCacheMiss != 700 || got.Output != 50 {
		t.Fatalf("ToTokens = %+v", got)
	}
	clamped := billing.ToTokens(2000, 0, 1000, 0)
	if clamped.InputCacheHit != 1000 || clamped.InputCacheMiss != 0 {
		t.Fatalf("clamp = %+v", clamped)
	}
}

func TestFromReportedCost_RoundsToMicros(t *testing.T) {
	if got := billing.FromReportedCost(0.00176); got != 1760 {
		t.Fatalf("reported = %d, want 1760", got)
	}
	if got := billing.FromReportedCost(0); got != 0 {
		t.Fatalf("zero = %d", got)
	}
}
