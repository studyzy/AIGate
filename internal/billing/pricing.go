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

import "math"

// Price 是价目快照的六列原始单价，单位为「每百万 token 的微元数」。
// 峰谷两套三列都是原始值，不做乘数推导——峰谷折扣不一定等比。
type Price struct {
	PeakInputHitMicros     int64
	PeakInputMissMicros    int64
	PeakOutputMicros       int64
	OffpeakInputHitMicros  int64
	OffpeakInputMissMicros int64
	OffpeakOutputMicros    int64
}

// Tokens 是本次请求的三档 token 数，与 Price 的三列一一对应。
type Tokens struct {
	InputCacheHit  int64
	InputCacheMiss int64
	Output         int64
}

// CostMicros 按峰/谷选三列原始价计算金额（微元）：
// Σ(该档 token 数 × 该档单价) / 1_000_000，全程整数。
func CostMicros(t Tokens, p Price, peak bool) int64 {
	hit, miss, out := p.OffpeakInputHitMicros, p.OffpeakInputMissMicros, p.OffpeakOutputMicros
	if peak {
		hit, miss, out = p.PeakInputHitMicros, p.PeakInputMissMicros, p.PeakOutputMicros
	}
	return (t.InputCacheHit*hit + t.InputCacheMiss*miss + t.Output*out) / MicrosPerMillionTokens
}

// ToTokens 把上游的用量字段映射到三档计价口径。
// 缓存命中数不得超过 prompt（超出的部分计入未命中档），未命中档为 prompt 的剩余部分。
// cacheWrite 归入未命中档，故此参数保留但不参与计算。
func ToTokens(cacheRead, cacheWrite, prompt, completion int) Tokens {
	hit := int64(cacheRead)
	if hit > int64(prompt) {
		hit = int64(prompt)
	}
	miss := int64(prompt) - hit
	if miss < 0 {
		miss = 0
	}
	return Tokens{InputCacheHit: hit, InputCacheMiss: miss, Output: int64(completion)}
}

// FromReportedCost 把上游自报的金额（币种单位）四舍五入换算为微元。
// 这是本包唯一的浮点运算入口。
func FromReportedCost(cost float64) int64 {
	return int64(math.Round(cost * float64(MicrosPerUnit)))
}
