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
	"math"
	"math/bits"
	"sort"
	"time"
)

// SameCurrencyRate 是同币种折算的恒等汇率（模型币种 == 平台结算币种时使用，不落库）。
const SameCurrencyRate int64 = MicrosPerUnit

// DatedRate 是一条按 effective_at 追加的汇率快照。
// RateMicros 语义：1 单位 from = RateMicros/1e6 单位 to（FX 报价惯例，乘法直用）。
type DatedRate struct {
	EffectiveAt time.Time
	RateMicros  int64
}

// SelectRate 返回 at 时刻生效的最新汇率（微元）。入参可无序；
// 无汇率或全部汇率晚于 at 时返回 false，调用方据此记 warn。
func SelectRate(rates []DatedRate, at time.Time) (int64, bool) {
	if len(rates) == 0 {
		return 0, false
	}
	sorted := make([]DatedRate, len(rates))
	copy(sorted, rates)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].EffectiveAt.Before(sorted[j].EffectiveAt)
	})
	i := sort.Search(len(sorted), func(i int) bool {
		return sorted[i].EffectiveAt.After(at)
	})
	if i == 0 {
		return 0, false
	}
	return sorted[i-1].RateMicros, true
}

// ConvertMicros 按汇率把金额从 from 币种折算为 to 币种：amount × rateMicros / 1e6。
//
// 约束（调用方保证）：amount 为金额微元，rateMicros <= 1e9（1 单位 from 至多等于
// 1000 单位 to）。乘积以 128 位计算：若 amount × rateMicros 超过 MaxInt64 × 1e6
// （即折算结果超出 int64）则返回 math.MaxInt64（调用方记 warn），绝不返回静默截断的
// 小值。乘积本身超过 64 位但折算结果仍可表示时（如 1e12 × 1e9）正常返回。
func ConvertMicros(amount, rateMicros int64) int64 {
	neg := false
	if amount < 0 {
		neg = true
		amount = -amount
	}
	if rateMicros < 0 {
		neg = !neg
		rateMicros = -rateMicros
	}
	hi, lo := bits.Mul64(uint64(amount), uint64(rateMicros))
	// 溢出边界：product > math.MaxInt64 * MicrosPerUnit。
	// 该常量按位拆成 hi/lo 两块，用 128 位比较取代除法，避免再引入大数运算。
	bhi, blo := bits.Mul64(math.MaxInt64, uint64(MicrosPerUnit))
	if hi > bhi || (hi == bhi && lo > blo) {
		return math.MaxInt64
	}
	// product <= MaxInt64 * 1e6，quotient 必 <= MaxInt64；hi 不为 0 时按 128 位除 1e6。
	q, _ := bits.Div64(hi, lo, uint64(MicrosPerUnit))
	v := int64(q)
	if neg {
		return -v
	}
	return v
}
