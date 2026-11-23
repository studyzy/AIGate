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

package model

// 按金额计费的快照表：价目（ai_model_prices）、计费日历（billing_calendar）、汇率（currency_rates）。
//
// 三张表都是「按 effective_at / 日期追加」的写法，不就地改写历史行：
// 每笔请求的金额在记账时刻定死，此后改价、改汇率、改日历都不影响已落库的用量行。
// 金额一律为整数微元（1 微元 = 1e-6 个币种单位），不出现浮点。

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm/clause"
)

// AIModelPrice 是一份模型价目快照：六列原始单价（峰/谷 × 命中/未命中/输出）+ 峰谷时段。
//
// 单价语义为「每百万 token 的微元数」，计价时 token × 单价 / 1_000_000 即得微元；
// 峰谷时段以 JSON 存在 PeakHours，空对象表示该价目没有时段配置。
type AIModelPrice struct {
	ID          uint      `gorm:"primaryKey"`
	AIModelID   uint      `gorm:"not null;default:0;index;uniqueIndex:uk_ai_model_prices_effective,priority:1"`
	Currency    string    `gorm:"size:3;not null;default:''"` // 冗余快照：改模型币种不影响历史价目
	EffectiveAt time.Time `gorm:"not null;uniqueIndex:uk_ai_model_prices_effective,priority:2"`
	// PeakHours 峰谷时段 JSON（时区 + 星期几 + HH:MM 窗口），空对象表示无时段。
	PeakHours              string    `gorm:"type:text;not null"`
	PeakInputHitMicros     int64     `gorm:"not null;default:0"`
	PeakInputMissMicros    int64     `gorm:"not null;default:0"`
	PeakOutputMicros       int64     `gorm:"not null;default:0"`
	OffpeakInputHitMicros  int64     `gorm:"not null;default:0"`
	OffpeakInputMissMicros int64     `gorm:"not null;default:0"`
	OffpeakOutputMicros    int64     `gorm:"not null;default:0"`
	CreatedAt              time.Time `gorm:"not null"`
}

// TableName 显式指定表名（与 sql/init.sql 一致）。
func (AIModelPrice) TableName() string { return "ai_model_prices" }

// BillingCalendarDay 是计费日历的一天：未登记的日期按星期几推断，登记后以 is_workday 为准。
// Date 为业务时区日期标签（与 localDate / LocalToday 同口径），作为主键。
type BillingCalendarDay struct {
	Date      time.Time `gorm:"type:date;primaryKey"`
	IsWorkday bool      `gorm:"not null;default:true"` // 节假日 false，调休上班 true
	Note      string    `gorm:"size:128;not null;default:''"`
	UpdatedAt time.Time `gorm:"not null"`
}

// TableName 显式指定表名（与 sql/init.sql 一致）。
func (BillingCalendarDay) TableName() string { return "billing_calendar" }

// CurrencyRate 是一条汇率快照：1 单位 FromCurrency = RateMicros/1e6 单位 ToCurrency。
type CurrencyRate struct {
	ID           uint      `gorm:"primaryKey"`
	FromCurrency string    `gorm:"size:3;not null;uniqueIndex:uk_currency_rates_effective,priority:1"`
	ToCurrency   string    `gorm:"size:3;not null;uniqueIndex:uk_currency_rates_effective,priority:2"`
	EffectiveAt  time.Time `gorm:"not null;uniqueIndex:uk_currency_rates_effective,priority:3"`
	RateMicros   int64     `gorm:"not null"`
	CreatedAt    time.Time `gorm:"not null"`
}

// TableName 显式指定表名（与 sql/init.sql 一致）。
func (CurrencyRate) TableName() string { return "currency_rates" }

// calendarDate 把任意时刻归一为日历表的日期标签（业务时区当日 00:00 的 UTC 表示）。
// 只取日期部分，时刻被丢弃：日历是「按天」的数据。
func calendarDate(t time.Time) time.Time {
	return localDate(t)
}

// CreateAIModelPrice 追加一份价目快照（同一模型同一 effective_at 唯一）。
func (s *Store) CreateAIModelPrice(ctx context.Context, p *AIModelPrice) error {
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	if err := s.db.WithContext(ctx).Create(p).Error; err != nil {
		return fmt.Errorf("create ai model price: %w", err)
	}
	return nil
}

// ListAIModelPrices 列出某模型的全部价目快照，按 effective_at 倒序（新快照在前）。
func (s *Store) ListAIModelPrices(ctx context.Context, aiModelID uint) ([]AIModelPrice, error) {
	var prices []AIModelPrice
	if err := s.db.WithContext(ctx).
		Where("ai_model_id = ?", aiModelID).
		Order("effective_at DESC").Find(&prices).Error; err != nil {
		return nil, fmt.Errorf("list ai model prices: %w", err)
	}
	return prices, nil
}

// DeleteAIModelPrice 按 ID 删除一份价目快照（删除不存在的行不算错误）。
func (s *Store) DeleteAIModelPrice(ctx context.Context, id uint) error {
	if err := s.db.WithContext(ctx).Delete(&AIModelPrice{}, id).Error; err != nil {
		return fmt.Errorf("delete ai model price: %w", err)
	}
	return nil
}

// ListBillingCalendarDays 列出 [from, to] 内的日历登记（含首含尾，按日期升序）。
func (s *Store) ListBillingCalendarDays(ctx context.Context, from, to time.Time) ([]BillingCalendarDay, error) {
	var days []BillingCalendarDay
	if err := s.db.WithContext(ctx).
		Where("date >= ? AND date <= ?", calendarDate(from), calendarDate(to)).
		Order("date ASC").Find(&days).Error; err != nil {
		return nil, fmt.Errorf("list billing calendar days: %w", err)
	}
	return days, nil
}

// UpsertBillingCalendarDays 批量登记日历（按日期覆盖写）。
//
// IsWorkday 有 default:true，Create 会跳过零值，放到 OnConflict 里显式赋值，
// 保证「改为节假日」不会被默认值悄悄改写回 true。
func (s *Store) UpsertBillingCalendarDays(ctx context.Context, days []BillingCalendarDay) error {
	if len(days) == 0 {
		return nil
	}
	rows := make([]map[string]any, 0, len(days))
	now := time.Now().UTC()
	for _, d := range days {
		if d.Date.IsZero() {
			// 零值时间会被 GORM 当作「未设置」而落 NULL，主键为 NULL 时每次插入都是新行。
			return fmt.Errorf("upsert billing calendar days: %w", invalidInput("calendar day date must not be zero"))
		}
		// 逐行用 map 写入：is_workday 有 default:true，走结构体时 GORM 会跳过 false 这个零值，
		// 于是「把某天改成节假日」会被列默认值悄悄改回 true。
		rows = append(rows, map[string]any{
			"date":       calendarDate(d.Date),
			"is_workday": d.IsWorkday,
			"note":       d.Note,
			"updated_at": now,
		})
	}
	err := s.db.WithContext(ctx).Model(&BillingCalendarDay{}).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "date"}},
		DoUpdates: clause.AssignmentColumns([]string{"is_workday", "note", "updated_at"}),
	}).Create(&rows).Error
	if err != nil {
		return fmt.Errorf("upsert billing calendar days: %w", err)
	}
	return nil
}

// DeleteBillingCalendarDay 删除某一天的登记（之后该日回落到按星期几推断）。
func (s *Store) DeleteBillingCalendarDay(ctx context.Context, date time.Time) error {
	if err := s.db.WithContext(ctx).Where("date = ?", calendarDate(date)).
		Delete(&BillingCalendarDay{}).Error; err != nil {
		return fmt.Errorf("delete billing calendar day: %w", err)
	}
	return nil
}

// CreateCurrencyRate 追加一条汇率快照（同一币种对同一 effective_at 唯一）。
func (s *Store) CreateCurrencyRate(ctx context.Context, r *CurrencyRate) error {
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if err := s.db.WithContext(ctx).Create(r).Error; err != nil {
		return fmt.Errorf("create currency rate: %w", err)
	}
	return nil
}

// ListCurrencyRates 列出某币种对的全部汇率快照，按 effective_at 倒序（最新在前）。
func (s *Store) ListCurrencyRates(ctx context.Context, from, to string) ([]CurrencyRate, error) {
	var rates []CurrencyRate
	if err := s.db.WithContext(ctx).
		Where("from_currency = ? AND to_currency = ?", from, to).
		Order("effective_at DESC").Find(&rates).Error; err != nil {
		return nil, fmt.Errorf("list currency rates: %w", err)
	}
	return rates, nil
}

// DeleteCurrencyRate 按 ID 删除一条汇率快照（删除不存在的行不算错误）。
func (s *Store) DeleteCurrencyRate(ctx context.Context, id uint) error {
	if err := s.db.WithContext(ctx).Delete(&CurrencyRate{}, id).Error; err != nil {
		return fmt.Errorf("delete currency rate: %w", err)
	}
	return nil
}
