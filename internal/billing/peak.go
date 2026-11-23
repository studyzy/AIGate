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
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// PeakWindow 是一个高峰时段窗口。
//   - Days 用 Go time.Weekday 编号（0=周日 … 6=周六），指窗口开始的那一天；
//   - Start/End 为 "HH:MM"，半开区间 [start, end)；end <= start 表示跨午夜；
//   - "24:00" 是合法的当日结束时刻。
type PeakWindow struct {
	Days  []int  `json:"days"`
	Start string `json:"start"`
	End   string `json:"end"`
}

// PeakSchedule 是模型级的峰谷时段配置，时区为 IANA 名称。
type PeakSchedule struct {
	Timezone string       `json:"timezone"`
	Windows  []PeakWindow `json:"windows"`
}

// minutesPerDay 是一天的分钟数，也是 "24:00" 的分钟值。
const minutesPerDay = 24 * 60

// locCache 缓存已解析的时区，避免每次判定都走文件系统（time.LoadLocation）。
var locCache sync.Map // map[string]*time.Location

// ParsePeakSchedule 反序列化 peak_hours JSON。
// 空串与 "{}" 返回零值调度与 nil 错误（表示无时段配置）。
// JSON 格式错误返回 error：管理面应拒绝写入，运行期出现即视为数据损坏。
func ParsePeakSchedule(raw string) (PeakSchedule, error) {
	if raw == "" || raw == "{}" {
		return PeakSchedule{}, nil
	}
	var s PeakSchedule
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return PeakSchedule{}, fmt.Errorf("parse peak schedule: %w", err)
	}
	return s, nil
}

// parseClockMinutes 把 "HH:MM" 解析为当天的分钟数。
// 只接受两位小时、两位分钟；小时 0..23，或 24:00（=1440，当日结束时刻）。
func parseClockMinutes(s string) (int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	for _, i := range []int{0, 1, 3, 4} {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	if m > 59 {
		return 0, false
	}
	if h == 24 {
		if m != 0 {
			return 0, false
		}
		return minutesPerDay, true
	}
	if h > 24 {
		return 0, false
	}
	return h*60 + m, true
}

// loadLocation 解析 IANA 时区名并缓存结果；失败不缓存（可能因 tzdata 缺失而暂时失败）。
func loadLocation(name string) (*time.Location, error) {
	if v, ok := locCache.Load(name); ok {
		return v.(*time.Location), nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	v, _ := locCache.LoadOrStore(name, loc)
	return v.(*time.Location), nil
}

// parsedWindow 是校验通过的窗口，分钟值已归一化。
type parsedWindow struct {
	days   []int
	start  int
	finish int
	wraps  bool // end <= start，跨午夜
}

// parseWindows 校验并归一化全部窗口。
// 无 days、days 全越界或时钟串非法的窗口被丢弃；返回值为空表示调度不可评估。
func parseWindows(ws []PeakWindow) []parsedWindow {
	out := make([]parsedWindow, 0, len(ws))
	for _, w := range ws {
		days := make([]int, 0, len(w.Days))
		for _, d := range w.Days {
			if d >= 0 && d <= 6 {
				days = append(days, d)
			}
		}
		if len(days) == 0 {
			continue
		}
		start, ok := parseClockMinutes(w.Start)
		if !ok {
			continue
		}
		end, ok := parseClockMinutes(w.End)
		if !ok {
			continue
		}
		out = append(out, parsedWindow{
			days:   days,
			start:  start,
			finish: end,
			wraps:  end <= start,
		})
	}
	return out
}

// inDays 判断 wd 是否落在 days 中。
func (w parsedWindow) inDays(wd time.Weekday) bool {
	for _, d := range w.days {
		if time.Weekday(d) == wd {
			return true
		}
	}
	return false
}

// hitOnStartDay 判断窗口在其开始那天的 [start, end) 内是否覆盖 minute。
func (w parsedWindow) hitOnStartDay(minute int) bool {
	if w.wraps {
		// 跨午夜：开始当天只覆盖 [start, 24:00)。
		return minute >= w.start
	}
	return minute >= w.start && minute < w.finish
}

// hitOnNextDay 判断跨午夜窗口在次日 [00:00, end) 内是否覆盖 minute。
func (w parsedWindow) hitOnNextDay(minute int) bool {
	return w.wraps && minute < w.finish
}

// IsPeak 判定 at 是否处于高峰时段。
//
// 返回值 evaluable=false 表示调度无法评估（无窗口、时区非法、全部窗口的时钟串非法或
// days 全越界），此时 peak 恒为 false；调用方必须据此 fail-closed 取高峰价。
// 可评估且日历判该日非工作日时返回 (false, true)。
// at 为 nil 日历（或未传入）时按星期几推断工作日。
func IsPeak(at time.Time, sched PeakSchedule, cal Calendar) (peak bool, evaluable bool) {
	if len(sched.Windows) == 0 {
		return false, false
	}
	if sched.Timezone == "" {
		return false, false
	}
	loc, err := loadLocation(sched.Timezone)
	if err != nil {
		return false, false
	}
	windows := parseWindows(sched.Windows)
	if len(windows) == 0 {
		return false, false
	}

	local := at.In(loc)
	if cal != nil && !cal.IsWorkday(local) {
		return false, true
	}
	minute := local.Hour()*60 + local.Minute()
	prevDay := local.AddDate(0, 0, -1).Weekday()
	today := local.Weekday()
	for _, w := range windows {
		if w.inDays(today) && w.hitOnStartDay(minute) {
			return true, true
		}
		// 跨午夜窗口：匹配前一天开始的窗口延续到今天。
		if w.wraps && w.inDays(prevDay) && w.hitOnNextDay(minute) {
			return true, true
		}
	}
	return false, true
}
