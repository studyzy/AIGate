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

import (
	"testing"
	"time"
)

var comparableAt = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

func priceWith(inputMiss, output int64) Price {
	return Price{
		PeakInputMissMicros:    inputMiss,
		PeakOutputMicros:       output,
		OffpeakInputMissMicros: inputMiss,
		OffpeakOutputMicros:    output,
	}
}

// 比价标量必须把输入与输出都算进去：只便宜在输入上、输出极贵的模型不应排在最前。
func TestComparablePrice_WeighsBothDirections(t *testing.T) {
	cheapInput := []DatedPrice{{EffectiveAt: comparableAt.Add(-time.Hour), Price: priceWith(1, 1000)}}
	balanced := []DatedPrice{{EffectiveAt: comparableAt.Add(-time.Hour), Price: priceWith(100, 100)}}

	a, okA := ComparablePrice(cheapInput, comparableAt, false)
	b, okB := ComparablePrice(balanced, comparableAt, false)
	if !okA || !okB {
		t.Fatal("both prices should resolve")
	}
	// 合成量 1000/1000：前者 = 1000×1 + 1000×1000 = 1,001,000；后者 = 1000×100 + 1000×100 = 200,000
	if a <= b {
		t.Fatalf("input-only cheap model scored %d, balanced scored %d; output must weigh in", a, b)
	}
}

// 峰谷两套价要分别取：同一个模型在高峰期可能比另一个更贵。fail-closed 取高峰价由调用方决定。
func TestComparablePrice_PeakSelectsPeakColumns(t *testing.T) {
	p := []DatedPrice{{EffectiveAt: comparableAt.Add(-time.Hour), Price: Price{
		PeakInputMissMicros:    500,
		PeakOutputMicros:       500,
		OffpeakInputMissMicros: 1,
		OffpeakOutputMicros:    1,
	}}}
	peak, _ := ComparablePrice(p, comparableAt, true)
	offpeak, _ := ComparablePrice(p, comparableAt, false)
	if peak <= offpeak {
		t.Fatalf("peak (%d) should exceed offpeak (%d)", peak, offpeak)
	}
}

// 无价目、或全部价目晚于 at：ok=false，调用方据此把候选排到已定价候选之后。
func TestComparablePrice_MissingPrice(t *testing.T) {
	if _, ok := ComparablePrice(nil, comparableAt, false); ok {
		t.Fatal("empty price list should not resolve")
	}
	future := []DatedPrice{{EffectiveAt: comparableAt.Add(time.Hour), Price: priceWith(1, 1)}}
	if _, ok := ComparablePrice(future, comparableAt, false); ok {
		t.Fatal("prices effective only in the future should not resolve")
	}
}

// 只取 at 时刻生效的最新快照，旧价目不参与。
func TestComparablePrice_UsesLatestEffective(t *testing.T) {
	prices := []DatedPrice{
		{EffectiveAt: comparableAt.Add(-2 * time.Hour), Price: priceWith(1, 1)},
		{EffectiveAt: comparableAt.Add(-time.Hour), Price: priceWith(100, 100)},
	}
	got, ok := ComparablePrice(prices, comparableAt, false)
	if !ok {
		t.Fatal("price should resolve")
	}
	want, _ := ComparablePrice([]DatedPrice{prices[1]}, comparableAt, false)
	if got != want {
		t.Fatalf("got %d, want %d (latest snapshot)", got, want)
	}
}
