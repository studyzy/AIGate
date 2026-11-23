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
	"sort"
	"time"
)

// DatedPrice 是一条按 effective_at 追加的价目快照，含该快照的峰谷时段与币种。
// 快照一经写入不可变，故记账时刻选定哪条，历史行就永远按哪条计价。
type DatedPrice struct {
	EffectiveAt time.Time
	Price       Price
	Schedule    PeakSchedule
	Currency    string
}

// SelectPrice 返回 at 时刻生效的最新价目快照。
// 入参可无序（不假设有序），同一 effective_at 以先出现者为准；
// 无快照或全部快照晚于 at 时返回 false。
func SelectPrice(prices []DatedPrice, at time.Time) (DatedPrice, bool) {
	if len(prices) == 0 {
		return DatedPrice{}, false
	}
	sorted := make([]DatedPrice, len(prices))
	copy(sorted, prices)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].EffectiveAt.Before(sorted[j].EffectiveAt)
	})
	i := sort.Search(len(sorted), func(i int) bool {
		return sorted[i].EffectiveAt.After(at)
	})
	if i == 0 {
		return DatedPrice{}, false
	}
	return sorted[i-1], true
}
