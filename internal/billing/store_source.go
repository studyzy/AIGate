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
	"fmt"
	"log/slog"
	"sync"
	"time"

	"aigate/internal/model"
)

// StoreSource 是 Source 的存储实现：从 model.Store 读出模型配置、价目、汇率、日历与
// 平台结算币种，并在进程内缓存。
//
// 缓存安全性：价目、汇率、日历都是「按 effective_at / 日期追加」的不可变快照，模型行
// 在两次管理面写入之间也不变，故缓存到显式失效为止是安全的。管理面在写入后必须调用
// 对应的 Invalidate* 方法；数据面只读，不写缓存以外的任何状态。
type StoreSource struct {
	store *model.Store

	// warnOnce 记录已告警过「peak_hours 无法解析」的模型，避免每笔请求都刷日志。
	warnOnce sync.Map // map[uint]struct{}

	mu sync.RWMutex
	// models 按模型 ID 缓存计价配置（币种与两个开关）。
	models map[uint]ModelInfo
	// prices 按模型 ID 缓存全部价目快照。
	prices map[uint][]DatedPrice
	// rates 按 "FROM/TO" 缓存某币种对的全部汇率快照。
	rates map[string][]DatedRate
	// cal 是已加载的日历；calRange 记录它覆盖的日期区间（闭区间）。
	cal      Calendar
	calFrom  time.Time
	calTo    time.Time
	calValid bool
	// platform 是平台结算币种；platformLoaded 标记是否已读过设置行。
	platform       string
	platformLoaded bool
}

// NewStoreSource 构造基于存储层的 Source。
func NewStoreSource(store *model.Store) *StoreSource {
	return &StoreSource{
		store:  store,
		models: make(map[uint]ModelInfo),
		prices: make(map[uint][]DatedPrice),
		rates:  make(map[string][]DatedRate),
	}
}

// 编译期断言：StoreSource 必须满足 Source 接口。
var _ Source = (*StoreSource)(nil)

// Model 返回模型的计价配置，命中缓存时不访问数据库。
func (s *StoreSource) Model(ctx context.Context, modelID uint) (ModelInfo, error) {
	s.mu.RLock()
	mi, ok := s.models[modelID]
	s.mu.RUnlock()
	if ok {
		return mi, nil
	}

	m, err := s.store.GetAIModel(ctx, modelID)
	if err != nil {
		return ModelInfo{}, fmt.Errorf("get ai model %d: %w", modelID, err)
	}
	mi = ModelInfo{
		Currency:        m.Currency,
		EnablePeak:      m.EnablePeakPricing,
		UseUpstreamCost: m.UseUpstreamCost,
	}

	s.mu.Lock()
	s.models[modelID] = mi
	s.mu.Unlock()
	return mi, nil
}

// Prices 返回某模型的全部价目快照（按 effective_at 倒序），每条都带上解析后的峰谷时段。
//
// peak_hours 解析失败时回落到零值调度：运行期 IsPeak 判为不可评估，引擎据此 fail-closed
// 取高峰价三列，绝不静默打折。同一模型只会告警一次，避免逐笔请求刷日志。
func (s *StoreSource) Prices(ctx context.Context, modelID uint) ([]DatedPrice, error) {
	s.mu.RLock()
	cached, ok := s.prices[modelID]
	s.mu.RUnlock()
	if ok {
		return cached, nil
	}

	rows, err := s.store.ListAIModelPrices(ctx, modelID)
	if err != nil {
		return nil, fmt.Errorf("list prices for model %d: %w", modelID, err)
	}
	out := make([]DatedPrice, 0, len(rows))
	for _, r := range rows {
		sched, perr := ParsePeakSchedule(r.PeakHours)
		if perr != nil {
			if _, warned := s.warnOnce.LoadOrStore(modelID, struct{}{}); !warned {
				slog.Warn("invalid peak_hours, billing peak price until fixed",
					"ai_model_id", modelID, "price_id", r.ID, "error", perr)
			}
			sched = PeakSchedule{}
		}
		out = append(out, DatedPrice{
			EffectiveAt: r.EffectiveAt,
			Schedule:    sched,
			Currency:    r.Currency,
			Price: Price{
				PeakInputHitMicros:     r.PeakInputHitMicros,
				PeakInputMissMicros:    r.PeakInputMissMicros,
				PeakOutputMicros:       r.PeakOutputMicros,
				OffpeakInputHitMicros:  r.OffpeakInputHitMicros,
				OffpeakInputMissMicros: r.OffpeakInputMissMicros,
				OffpeakOutputMicros:    r.OffpeakOutputMicros,
			},
		})
	}

	s.mu.Lock()
	s.prices[modelID] = out
	s.mu.Unlock()
	return out, nil
}

// Rate 返回 from→to 的全部汇率快照（按 effective_at 倒序）。
// 未登记的币种对返回空集与 nil 错误：缺失汇率由 SelectRate 判定，引擎据 ErrMissingRate 记账。
func (s *StoreSource) Rate(ctx context.Context, from, to string) ([]DatedRate, error) {
	key := from + "/" + to
	s.mu.RLock()
	cached, ok := s.rates[key]
	s.mu.RUnlock()
	if ok {
		return cached, nil
	}

	rows, err := s.store.ListCurrencyRates(ctx, from, to)
	if err != nil {
		return nil, fmt.Errorf("list rates %s: %w", key, err)
	}
	out := make([]DatedRate, 0, len(rows))
	for _, r := range rows {
		out = append(out, DatedRate{EffectiveAt: r.EffectiveAt, RateMicros: r.RateMicros})
	}

	s.mu.Lock()
	s.rates[key] = out
	s.mu.Unlock()
	return out, nil
}

// Calendar 返回覆盖 [from, to] 的日历。已缓存的日历完全包住请求区间时直接复用，
// 否则按请求区间重新加载——引擎每笔只取记账时刻前 48 小时的窗口，
// 包不住时重载即可，不必扩表也不必按年切分。
func (s *StoreSource) Calendar(ctx context.Context, from, to time.Time) (Calendar, error) {
	s.mu.RLock()
	if s.calValid && !from.Before(s.calFrom) && !to.After(s.calTo) {
		cal := s.cal
		s.mu.RUnlock()
		return cal, nil
	}
	s.mu.RUnlock()

	rows, err := s.store.ListBillingCalendarDays(ctx, from, to)
	if err != nil {
		return nil, fmt.Errorf("list billing calendar: %w", err)
	}
	overrides := make(map[string]bool, len(rows))
	for _, r := range rows {
		overrides[r.Date.Format(calendarDateLayout)] = r.IsWorkday
	}
	cal := NewCalendar(overrides)

	s.mu.Lock()
	s.cal = cal
	s.calFrom = from
	s.calTo = to
	s.calValid = true
	s.mu.Unlock()
	return cal, nil
}

// PlatformCurrency 返回平台结算币种；设置行首次读取后缓存。
func (s *StoreSource) PlatformCurrency(ctx context.Context) (string, error) {
	s.mu.RLock()
	if s.platformLoaded {
		v := s.platform
		s.mu.RUnlock()
		return v, nil
	}
	s.mu.RUnlock()

	st, err := s.store.GetSettings(ctx)
	if err != nil {
		return "", fmt.Errorf("get settings: %w", err)
	}
	// 未配置时按默认结算币种记账，与设置表列默认值一致。
	cur := st.PlatformCurrency
	if cur == "" {
		cur = model.DefaultPlatformCurrency
	}

	s.mu.Lock()
	s.platform = cur
	s.platformLoaded = true
	s.mu.Unlock()
	return cur, nil
}

// InvalidateModel 清除某模型的计价配置与价目缓存（改模型行或追加/删除价目后调用）。
// 同时清掉该模型的告警去重标记，使修正后的 peak_hours 若再次出错仍会告警。
func (s *StoreSource) InvalidateModel(modelID uint) {
	s.mu.Lock()
	delete(s.models, modelID)
	delete(s.prices, modelID)
	s.mu.Unlock()
	s.warnOnce.Delete(modelID)
}

// InvalidateCalendar 清除日历缓存（登记或删除日历后调用）。
func (s *StoreSource) InvalidateCalendar() {
	s.mu.Lock()
	s.cal = nil
	s.calFrom = time.Time{}
	s.calTo = time.Time{}
	s.calValid = false
	s.mu.Unlock()
}

// InvalidateRates 清除全部汇率缓存（追加或删除汇率后调用）。
func (s *StoreSource) InvalidateRates() {
	s.mu.Lock()
	s.rates = make(map[string][]DatedRate)
	s.mu.Unlock()
}

// InvalidatePlatformCurrency 清除平台结算币种缓存（改设置后调用）。
func (s *StoreSource) InvalidatePlatformCurrency() {
	s.mu.Lock()
	s.platform = ""
	s.platformLoaded = false
	s.mu.Unlock()
}
