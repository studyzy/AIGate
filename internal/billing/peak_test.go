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
	"testing"
	"time"
)

func deepSeekSchedule(t *testing.T) PeakSchedule {
	t.Helper()
	s, err := ParsePeakSchedule(`{"timezone":"Asia/Shanghai","windows":[
		{"days":[1,2,3,4,5],"start":"09:00","end":"12:00"},
		{"days":[1,2,3,4,5],"start":"14:00","end":"18:00"}]}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return s
}

func TestIsPeak_HalfOpenBoundaries(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	sched := deepSeekSchedule(t)
	cal := NewCalendar(nil)
	mk := func(h, m int) time.Time { return time.Date(2026, 11, 23, h, m, 0, 0, loc) } // 周一
	cases := []struct {
		h, m int
		want bool
	}{
		{8, 59, false}, {9, 0, true}, {11, 59, true}, {12, 0, false},
		{13, 59, false}, {14, 0, true}, {17, 59, true}, {18, 0, false},
	}
	for _, c := range cases {
		got, ok := IsPeak(mk(c.h, c.m), sched, cal)
		if !ok {
			t.Fatalf("%02d:%02d must be evaluable", c.h, c.m)
		}
		if got != c.want {
			t.Errorf("%02d:%02d peak = %v, want %v", c.h, c.m, got, c.want)
		}
	}
}

func TestIsPeak_WeekendNotInDays(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	sat := time.Date(2026, 11, 28, 10, 0, 0, 0, loc)
	if peak, ok := IsPeak(sat, deepSeekSchedule(t), NewCalendar(nil)); peak || !ok {
		t.Fatalf("saturday peak=%v ok=%v, want false,true", peak, ok)
	}
}

func TestIsPeak_HolidayOverrideForcesOffpeak(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	day := time.Date(2026, 10, 1, 10, 0, 0, 0, loc) // 周四，国庆
	cal := NewCalendar(map[string]bool{"2026-10-01": false})
	if peak, ok := IsPeak(day, deepSeekSchedule(t), cal); peak || !ok {
		t.Fatalf("holiday peak=%v ok=%v, want false,true", peak, ok)
	}
}

// 跨午夜：22:00-02:00，days 指窗口开始那天。周一 23:00 与周二 01:00 都算高峰。
func TestIsPeak_WindowWrapsPastMidnight(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	sched, err := ParsePeakSchedule(`{"timezone":"Asia/Shanghai","windows":[{"days":[1],"start":"22:00","end":"02:00"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	mon := time.Date(2026, 11, 23, 23, 0, 0, 0, loc)
	tue := time.Date(2026, 11, 24, 1, 0, 0, 0, loc)
	tue2 := time.Date(2026, 11, 24, 2, 0, 0, 0, loc)
	if peak, _ := IsPeak(mon, sched, NewCalendar(nil)); !peak {
		t.Fatal("monday 23:00 must be peak")
	}
	if peak, _ := IsPeak(tue, sched, NewCalendar(nil)); !peak {
		t.Fatal("tuesday 01:00 must be peak (wrapped from monday)")
	}
	if peak, _ := IsPeak(tue2, sched, NewCalendar(nil)); peak {
		t.Fatal("tuesday 02:00 must be off-peak (half-open)")
	}
}

// 不可评估：时区非法 / 无窗口 / 窗口格式错 / days 全越界。调用方据此取高峰价。
func TestIsPeak_UnevaluableSchedules(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, loc)
	cases := map[string]string{
		"bad timezone":  `{"timezone":"Not/AZone","windows":[{"days":[1],"start":"09:00","end":"12:00"}]}`,
		"no windows":    `{"timezone":"Asia/Shanghai","windows":[]}`,
		"bad clock":     `{"timezone":"Asia/Shanghai","windows":[{"days":[1],"start":"9am","end":"12:00"}]}`,
		"days out of r": `{"timezone":"Asia/Shanghai","windows":[{"days":[7,9],"start":"09:00","end":"12:00"}]}`,
		"no days":       `{"timezone":"Asia/Shanghai","windows":[{"start":"09:00","end":"12:00"}]}`,
	}
	for name, raw := range cases {
		sched, err := ParsePeakSchedule(raw)
		if err != nil {
			t.Fatalf("%s: parse error %v", name, err)
		}
		if _, ok := IsPeak(at, sched, NewCalendar(nil)); ok {
			t.Errorf("%s: must report not evaluable", name)
		}
	}
}

func TestParseClockMinutes_Accepts24(t *testing.T) {
	if _, ok := parseClockMinutes("24:00"); !ok {
		t.Fatal("24:00 must be accepted as end-of-day")
	}
	if _, ok := parseClockMinutes("24:01"); ok {
		t.Fatal("24:01 must be rejected")
	}
}
