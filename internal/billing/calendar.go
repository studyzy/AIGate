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

// calendarDateLayout 是日历覆盖表的键格式（业务时区日期）。
const calendarDateLayout = "2006-01-02"

// Calendar 决定某个日期是否为工作日，用于修正峰谷时段的星期匹配
// （法定节假日可强制判为非工作日，调休上班日可显式标为工作日）。
type Calendar interface {
	IsWorkday(date time.Time) bool
}

// calendar 是 Calendar 的默认实现：一张按日期覆盖的表 + 星期几兜底。
type calendar struct {
	overrides map[string]bool
}

// NewCalendar 用「YYYY-MM-DD → 是否工作日」的覆盖表构造日历。
// 未登记日期按星期几推断：周一至周五为工作日。入参 map 会被复制，调用方可安全复用。
func NewCalendar(overrides map[string]bool) Calendar {
	c := make(map[string]bool, len(overrides))
	for k, v := range overrides {
		c[k] = v
	}
	return calendar{overrides: c}
}

// IsWorkday 先查覆盖表，未登记时按星期几推断。date 的时区即为业务时区，按 date 自身取值。
func (c calendar) IsWorkday(date time.Time) bool {
	if v, ok := c.overrides[date.Format(calendarDateLayout)]; ok {
		return v
	}
	wd := date.Weekday()
	return wd != time.Saturday && wd != time.Sunday
}
