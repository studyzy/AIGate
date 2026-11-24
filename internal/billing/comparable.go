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

import "time"

// comparableInputTokens 与 comparableOutputTokens 是比价用的合成请求量。
//
// 比价的目的是给候选模型排出一个「谁更便宜」的次序，而不是估算某次请求的真实金额，
// 因此固定一组典型量即可：1000 输入 + 1000 输出。它让输出单价以同等权重参与排序，
// 避免"输入极便宜但输出极贵"的模型被误判为最便宜。
const (
	comparableInputTokens  = 1000
	comparableOutputTokens = 1000
)

// ComparablePrice 把候选模型在 at 时刻生效的单价折算成一个可比较的标量（微元）。
//
// 只需比较大小，不需要绝对金额，也不用于记账——记账一律走 Engine.Compute。
//
// peak 决定取高峰还是空闲三列，由调用方按 Engine 同口径（IsPeak）预先判定；
// 价目缺失或全部晚于 at 时 ok=false，调用方应把该候选排在已定价候选之后。
func ComparablePrice(prices []DatedPrice, at time.Time, peak bool) (micros int64, ok bool) {
	selected, found := SelectPrice(prices, at)
	if !found {
		return 0, false
	}
	tokens := Tokens{
		// 全部计入未命中档：这是最保守（最贵）的假设，可避免缓存命中优惠歪曲排序。
		InputCacheMiss: comparableInputTokens,
		Output:         comparableOutputTokens,
	}
	return CostMicros(tokens, selected.Price, peak), true
}
