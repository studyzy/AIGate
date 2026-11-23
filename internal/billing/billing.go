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

// Package billing 实现按金额计费所需的全部纯函数算术：微元换算、三档单价计价、
// 峰谷时段判定、生效价目/汇率快照选择与币种折算。
//
// 边界：
//   - 不访问数据库、不发网络请求、不读时钟——所有「当前时间」均由调用方以参数传入；
//   - 金额一律为整数微元（1 微元 = 1e-6 个币种单位），全程整数运算，
//     唯一的浮点入口是上游自报成本的取整换算（FromReportedCost）；
//   - 计价失败方向为 fail-closed：峰谷时段无法评估时按高峰价计价，绝不静默打折。
package billing

const (
	// MicrosPerUnit 是一个币种单位对应的微元数。
	MicrosPerUnit int64 = 1_000_000
	// MicrosPerMillionTokens 是单价换算的分母：单价以「每百万 token 的微元数」录入。
	// 与 MicrosPerUnit 同值但语义不同，分开命名以免误用。
	MicrosPerMillionTokens int64 = 1_000_000
)
