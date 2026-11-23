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
	"context"
	"errors"
	"fmt"
	"time"
)

// ModelInfo 是计价所需的模型级配置，由 Source 从存储层读出。
type ModelInfo struct {
	// Currency 是模型计价币种（ISO 4217）；空串表示未配置。
	Currency string
	// EnablePeak 表示是否启用峰谷定价；关闭时恒按空闲价三列计价。
	EnablePeak bool
	// UseUpstreamCost 表示是否优先采信上游自报的 usage.cost。
	UseUpstreamCost bool
}

// Source 是计价引擎的全部数据来源。引擎本身不访问数据库、不读时钟，
// 价目、汇率、日历、平台币种都经此接口注入，便于替换与测试。
type Source interface {
	// Model 返回模型的计价配置。
	Model(ctx context.Context, modelID uint) (ModelInfo, error)
	// Prices 返回该模型的全部价目快照（可无序）。
	Prices(ctx context.Context, modelID uint) ([]DatedPrice, error)
	// Rate 返回 from→to 的全部汇率快照（可无序）。
	Rate(ctx context.Context, from, to string) ([]DatedRate, error)
	// Calendar 返回覆盖 [from, to] 的日历；返回 nil 表示无日历修正。
	Calendar(ctx context.Context, from, to time.Time) (Calendar, error)
	// PlatformCurrency 返回全局唯一的平台结算币种。
	PlatformCurrency(ctx context.Context) (string, error)
}

// Input 是一次计价请求。
type Input struct {
	// At 是记账时刻，价目、汇率、峰谷判定都以它为准。
	At time.Time
	// Tokens 是本次请求的三档 token 数。
	Tokens Tokens
	// ReportedCost 是上游自报的本次成本（币种单位）；nil 表示上游未报。
	ReportedCost *float64
}

// Result 是一次计价的结果。
type Result struct {
	// VendorMicros 是模型币种金额（微元）。
	VendorMicros int64
	// PlatformMicros 是平台结算币种金额（微元）；汇率缺失时为 0。
	PlatformMicros int64
	// ModelCurrency 与 PlatformCurrency 分别是两笔金额的币种。
	ModelCurrency    string
	PlatformCurrency string
	// Peak 表示自算金额取的是高峰价三列；上游自报时为 false。
	Peak bool
	// FromUpstream 表示金额来自上游自报而非自算。
	FromUpstream bool
}

// calendarLookback 是取日历的时间窗：48 小时足以覆盖跨午夜窗口在前一天开始的情形。
const calendarLookback = 48 * time.Hour

// Engine 把 Source 数据与纯函数算术组合成一次完整计价。
type Engine struct {
	src Source
	loc *time.Location
}

// NewEngine 构造计价引擎。loc 为业务时区，用于错误文案中的时刻格式化；nil 时取 UTC。
func NewEngine(src Source, loc *time.Location) *Engine {
	if loc == nil {
		loc = time.UTC
	}
	return &Engine{src: src, loc: loc}
}

// Compute 计算一笔请求的两个金额。
//
// 取价顺序：上游自报优先（UseUpstreamCost 且 ReportedCost > 0），否则按记账时刻判定
// 峰谷后取对应三列原始价自算；峰谷时段无法评估时 fail-closed 按高峰价计价。
//
// 错误语义：无生效价目返回零值 Result 与错误；跨币种折算缺汇率时**仍返回带
// VendorMicros 的 Result** 与非 nil 错误，调用方应照常落库模型币种金额。
func (e *Engine) Compute(ctx context.Context, modelID uint, in Input) (Result, error) {
	mi, err := e.src.Model(ctx, modelID)
	if err != nil {
		return Result{}, fmt.Errorf("load model %d: %w", modelID, err)
	}

	prices, err := e.src.Prices(ctx, modelID)
	if err != nil {
		return Result{}, fmt.Errorf("load prices for model %d: %w", modelID, err)
	}
	selected, ok := SelectPrice(prices, in.At)
	if !ok {
		return Result{}, fmt.Errorf("no effective price for model %d at %s", modelID, in.At.In(e.loc).Format(time.RFC3339))
	}

	res := Result{ModelCurrency: mi.Currency}
	switch {
	case mi.UseUpstreamCost && in.ReportedCost != nil && *in.ReportedCost > 0:
		// 上游自报优先：直接采信，跳过自算档位判定。
		res.VendorMicros = FromReportedCost(*in.ReportedCost)
		res.FromUpstream = true
	default:
		// 未启用峰谷定价的模型恒取空闲价三列，从不高价计价；
		// 启用后时段不可评估时 peak 保持 true——fail-closed 取高峰价，绝不静默打折。
		peak := false
		if mi.EnablePeak {
			// 取 48 小时窗口，覆盖跨午夜窗口在前一天开始的情形；先置 true 作 fail-closed 兜底。
			peak = true
			cal, err := e.src.Calendar(ctx, in.At.Add(-calendarLookback), in.At)
			if err != nil {
				return Result{}, fmt.Errorf("load calendar for model %d: %w", modelID, err)
			}
			if p, evaluable := IsPeak(in.At, selected.Schedule, cal); evaluable {
				peak = p
			}
		}
		res.Peak = peak
		res.VendorMicros = CostMicros(in.Tokens, selected.Price, peak)
	}

	platform, err := e.src.PlatformCurrency(ctx)
	if err != nil {
		return res, fmt.Errorf("load platform currency: %w", err)
	}
	res.PlatformCurrency = platform
	if mi.Currency == platform {
		res.PlatformMicros = res.VendorMicros
		return res, nil
	}

	rates, err := e.src.Rate(ctx, mi.Currency, platform)
	if err != nil {
		return res, fmt.Errorf("load rate %s->%s: %w", mi.Currency, platform, err)
	}
	rateMicros, ok := SelectRate(rates, in.At)
	if !ok {
		// 汇率缺失：平台金额记 0，模型币种金额照常保留。
		return res, fmt.Errorf("%w: %s->%s at %s", ErrMissingRate, mi.Currency, platform, in.At.In(e.loc).Format(time.RFC3339))
	}
	res.PlatformMicros = ConvertMicros(res.VendorMicros, rateMicros)
	return res, nil
}

// ErrMissingRate 表示记账时刻缺少模型币种到平台结算币种的生效汇率。
// 调用方可据 errors.Is 判定是否需要告警；此时 Result.VendorMicros 仍有效。
var ErrMissingRate = errors.New("missing currency rate")
