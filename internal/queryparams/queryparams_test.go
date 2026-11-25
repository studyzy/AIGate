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

package queryparams_test

// 黑盒单测：列表类接口的查询参数解析口径（管理面与用户面共用）。

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"aigate/internal/queryparams"
)

// TestParsePagination 覆盖默认值、上限与非法输入（含 page 溢出的上界保护）。
func TestParsePagination(t *testing.T) {
	cases := []struct {
		name         string
		query        string
		wantPage     int
		wantPageSize int
		wantErr      bool
	}{
		{name: "defaults", query: "", wantPage: 1, wantPageSize: 50},
		{name: "explicit", query: "?page=3&page_size=20", wantPage: 3, wantPageSize: 20},
		{name: "page_size capped", query: "?page_size=100000", wantPage: 1, wantPageSize: 500},
		{name: "page at max allowed", query: "?page=1000000", wantPage: 1000000, wantPageSize: 50},
		{name: "page over max rejected", query: "?page=1000001", wantErr: true},
		{name: "huge page rejected before overflow", query: "?page=18446744073709551", wantErr: true},
		{name: "invalid page", query: "?page=0", wantErr: true},
		{name: "invalid page_size", query: "?page_size=0", wantErr: true},
		{name: "non numeric", query: "?page=abc", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/x"+tc.query, nil)
			page, pageSize, err := queryparams.ParsePagination(r)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got page=%d page_size=%d", page, pageSize)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if page != tc.wantPage || pageSize != tc.wantPageSize {
				t.Fatalf("got page=%d page_size=%d, want %d/%d", page, pageSize, tc.wantPage, tc.wantPageSize)
			}
		})
	}
}

// TestValidGroupBy 覆盖白名单取值。
func TestValidGroupBy(t *testing.T) {
	for _, ok := range []string{"date", "model", "key", "user", "group"} {
		if !queryparams.ValidGroupBy(ok) {
			t.Errorf("ValidGroupBy(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "DATE", "keys", "models"} {
		if queryparams.ValidGroupBy(bad) {
			t.Errorf("ValidGroupBy(%q) = true, want false", bad)
		}
	}
}

// TestParseUsageFilter 覆盖日期解析、过滤字段与行级隔离（forcedUserID 忽略请求中的 user_id）。
func TestParseUsageFilter(t *testing.T) {
	t.Run("invalid dates", func(t *testing.T) {
		for _, q := range []string{"?start=2026/01/01", "?end=01-01-2026"} {
			r := httptest.NewRequest("GET", "/x"+q, nil)
			if _, err := queryparams.ParseUsageFilter(r, 0); err == nil {
				t.Fatalf("%s: want error", q)
			}
		}
	})

	t.Run("forced user id wins", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?user_id=42&group_id=7", nil)
		f, err := queryparams.ParseUsageFilter(r, 99)
		if err != nil {
			t.Fatalf("ParseUsageFilter: %v", err)
		}
		if f.UserID != 99 {
			t.Fatalf("UserID = %d, want forced 99", f.UserID)
		}
		if f.GroupID != 7 {
			t.Fatalf("GroupID = %d, want 7", f.GroupID)
		}
	})

	t.Run("explicit user id and empty values", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?user_id=5", nil)
		f, err := queryparams.ParseUsageFilter(r, 0)
		if err != nil {
			t.Fatalf("ParseUsageFilter: %v", err)
		}
		if f.UserID != 5 || f.GroupID != 0 || f.APIKeyID != 0 || f.AIModelID != 0 {
			t.Fatalf("filter = %+v, want user_id=5 and zeros elsewhere", f)
		}
	})
}

// newFilterReq 用 url.Values 构造 GET 请求，确保 +08:00 偏移、空格等特殊字符被正确编码/还原。
func newFilterReq(v url.Values) *http.Request {
	return httptest.NewRequest("GET", "/x?"+v.Encode(), nil)
}

// inRange 判断时刻是否落在半开区间 [Start, End) 内（End 排他）。
func inRange(ts, start, end time.Time) bool {
	return !ts.Before(start) && ts.Before(end)
}

// TestParseRequestLogFilter_DateInclusiveEnd 验证 YYYY-MM-DD 边界：end 含当日靠 +1 天，
// 统一为半开 UTC 区间 [Start, End)；当天 23:00 日志在区间内、次日 00:00 被排除。
func TestParseRequestLogFilter_DateInclusiveEnd(t *testing.T) {
	f, err := queryparams.ParseRequestLogFilter(newFilterReq(url.Values{
		"start": {"2026-11-25"},
		"end":   {"2026-11-25"},
	}))
	if err != nil {
		t.Fatalf("ParseRequestLogFilter: %v", err)
	}
	if f.Start == nil || f.End == nil {
		t.Fatalf("Start/End = %v/%v, want non-nil", f.Start, f.End)
	}
	wantStart := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	if !f.Start.Equal(wantStart) || f.Start.Location() != time.UTC {
		t.Fatalf("Start = %v (loc=%v), want %v in UTC", f.Start, f.Start.Location(), wantStart)
	}
	wantEnd := wantStart.AddDate(0, 0, 1)
	if !f.End.Equal(wantEnd) || f.End.Location() != time.UTC {
		t.Fatalf("End = %v, want %v (inclusive end via +1 day), UTC", f.End, wantEnd)
	}
	if !inRange(time.Date(2026, 11, 25, 23, 0, 0, 0, time.UTC), *f.Start, *f.End) {
		t.Errorf("23:00 of the day should be inside [Start, End)")
	}
	if inRange(wantEnd, *f.Start, *f.End) {
		t.Errorf("next-day 00:00 (== End) should be excluded (half-open)")
	}
	if inRange(wantStart.Add(-time.Second), *f.Start, *f.End) {
		t.Errorf("just before Start should be excluded")
	}
}

// TestParseRequestLogFilter_RFC3339 验证含 T 的 RFC3339 边界：按给定时区偏移转 UTC，
// end 排他（原样返回，不加一天）；结果统一为 UTC 时刻（供 sqlite / ES 直接使用）。
func TestParseRequestLogFilter_RFC3339(t *testing.T) {
	f, err := queryparams.ParseRequestLogFilter(newFilterReq(url.Values{
		"start": {"2026-11-25T10:00:00+08:00"},
		"end":   {"2026-11-25T12:00:00+08:00"},
	}))
	if err != nil {
		t.Fatalf("ParseRequestLogFilter: %v", err)
	}
	wantStart := time.Date(2026, 11, 25, 2, 0, 0, 0, time.UTC) // +08:00 → UTC 02:00
	wantEnd := time.Date(2026, 11, 25, 4, 0, 0, 0, time.UTC)   // +08:00 → UTC 04:00
	if f.Start == nil || !f.Start.Equal(wantStart) || f.Start.Location() != time.UTC {
		t.Fatalf("Start = %v, want %v UTC", f.Start, wantStart)
	}
	if f.End == nil || !f.End.Equal(wantEnd) || f.End.Location() != time.UTC {
		t.Fatalf("End = %v, want %v UTC (RFC3339 end exclusive, no +1 day)", f.End, wantEnd)
	}
	if !inRange(time.Date(2026, 11, 25, 3, 0, 0, 0, time.UTC), *f.Start, *f.End) {
		t.Errorf("03:00 UTC should be inside")
	}
	if inRange(wantEnd, *f.Start, *f.End) {
		t.Errorf("End instant should be excluded (half-open)")
	}
	if inRange(time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC), *f.Start, *f.End) {
		t.Errorf("before Start should be excluded")
	}
}

// TestParseRequestLogFilter_AllFields 覆盖全部 13 个参数（R25）：逐一映射到对应字段，
// 含 stream=false（非 nil 且为 false）、字符串 trim、分页复用 ParsePagination。
func TestParseRequestLogFilter_AllFields(t *testing.T) {
	f, err := queryparams.ParseRequestLogFilter(newFilterReq(url.Values{
		"start":          {"2026-11-01"},
		"end":            {"2026-11-30"},
		"api_key_id":     {"7"},
		"api_key_hash":   {"  hash-abc  "},
		"model":          {"  gpt-4o  "},
		"path":           {"  /v1/chat/completions  "},
		"status_code":    {"429"},
		"stream":         {"false"},
		"min_latency_ms": {"100"},
		"max_latency_ms": {"900"},
		"q":              {"  timeout  "},
		"page":           {"2"},
		"page_size":      {"10"},
	}))
	if err != nil {
		t.Fatalf("ParseRequestLogFilter: %v", err)
	}
	if f.Start == nil || !f.Start.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Start = %v, want 2026-11-01 UTC", f.Start)
	}
	if f.End == nil || !f.End.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("End = %v, want 2026-12-01 UTC (inclusive 2026-11-30)", f.End)
	}
	if f.APIKeyID != 7 {
		t.Errorf("APIKeyID = %d, want 7", f.APIKeyID)
	}
	if f.APIKeyHash != "hash-abc" {
		t.Errorf("APIKeyHash = %q, want trimmed %q", f.APIKeyHash, "hash-abc")
	}
	if f.Model != "gpt-4o" {
		t.Errorf("Model = %q, want trimmed %q", f.Model, "gpt-4o")
	}
	if f.Path != "/v1/chat/completions" {
		t.Errorf("Path = %q, want trimmed %q", f.Path, "/v1/chat/completions")
	}
	if f.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", f.StatusCode)
	}
	if f.Stream == nil || *f.Stream != false {
		t.Errorf("Stream = %v, want non-nil false", f.Stream)
	}
	if f.MinLatencyMS != 100 || f.MaxLatencyMS != 900 {
		t.Errorf("latency bounds = %d/%d, want 100/900", f.MinLatencyMS, f.MaxLatencyMS)
	}
	if f.Q != "timeout" {
		t.Errorf("Q = %q, want trimmed %q", f.Q, "timeout")
	}
	if f.Page != 2 || f.PageSize != 10 {
		t.Errorf("Page/PageSize = %d/%d, want 2/10", f.Page, f.PageSize)
	}
}

// TestParseRequestLogFilter_Invalid 覆盖非法参数与对应 400 文案：
// start>end、min>max、非法日期/数字/页码，以及结果窗口越界（R23 用 page=21&page_size=500）。
func TestParseRequestLogFilter_Invalid(t *testing.T) {
	cases := []struct {
		name   string
		values url.Values
		want   string // 期望的错误子串
	}{
		{name: "start after end (dates)", values: url.Values{"start": {"2026-11-25"}, "end": {"2026-11-20"}}, want: "invalid time range"},
		{name: "start after end (rfc3339)", values: url.Values{"start": {"2026-11-25T10:00:00Z"}, "end": {"2026-11-25T09:00:00Z"}}, want: "invalid time range"},
		{name: "min latency greater than max", values: url.Values{"min_latency_ms": {"500"}, "max_latency_ms": {"100"}}, want: "invalid latency range"},
		{name: "invalid start date", values: url.Values{"start": {"2026/01/01"}}, want: "invalid start"},
		{name: "invalid end date", values: url.Values{"end": {"not-a-date"}}, want: "invalid end"},
		{name: "invalid rfc3339 start", values: url.Values{"start": {"2026-11-25T99:99:99Z"}}, want: "invalid start"},
		{name: "invalid api_key_id", values: url.Values{"api_key_id": {"abc"}}, want: "invalid api_key_id"},
		{name: "invalid status_code", values: url.Values{"status_code": {"abc"}}, want: "invalid status_code"},
		{name: "invalid min_latency_ms", values: url.Values{"min_latency_ms": {"abc"}}, want: "invalid min_latency_ms"},
		{name: "invalid max_latency_ms", values: url.Values{"max_latency_ms": {"abc"}}, want: "invalid max_latency_ms"},
		{name: "invalid stream", values: url.Values{"stream": {"maybe"}}, want: "invalid stream"},
		{name: "invalid page", values: url.Values{"page": {"0"}}, want: "invalid page"},
		{name: "invalid page_size", values: url.Values{"page_size": {"abc"}}, want: "invalid page_size"},
		{name: "page out of range", values: url.Values{"page": {"21"}, "page_size": {"500"}}, want: "page out of range"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := queryparams.ParseRequestLogFilter(newFilterReq(tc.values))
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want containing %q", err.Error(), tc.want)
			}
		})
	}
}

// TestParseRequestLogFilter_DefaultsAndGuards 覆盖默认值、单侧延迟上界（R24）、
// 结果窗口边界值（R23 中 ==10000 允许）、stream 三态、以及 start/end 各自独立的格式判定。
func TestParseRequestLogFilter_DefaultsAndGuards(t *testing.T) {
	t.Run("no filters", func(t *testing.T) {
		f, err := queryparams.ParseRequestLogFilter(newFilterReq(url.Values{}))
		if err != nil {
			t.Fatalf("ParseRequestLogFilter: %v", err)
		}
		if f.Start != nil || f.End != nil || f.Stream != nil {
			t.Errorf("Start/End/Stream = %v/%v/%v, want all nil", f.Start, f.End, f.Stream)
		}
		if f.APIKeyID != 0 || f.APIKeyHash != "" || f.Model != "" || f.Path != "" ||
			f.StatusCode != 0 || f.MinLatencyMS != 0 || f.MaxLatencyMS != 0 || f.Q != "" {
			t.Errorf("filter = %+v, want zero values", f)
		}
		if f.Page != 1 || f.PageSize != queryparams.DefaultPageSize {
			t.Errorf("Page/PageSize = %d/%d, want 1/%d", f.Page, f.PageSize, queryparams.DefaultPageSize)
		}
	})

	t.Run("single-sided latency bounds allowed (R24)", func(t *testing.T) {
		for _, v := range []url.Values{
			{"min_latency_ms": {"500"}},
			{"max_latency_ms": {"500"}},
		} {
			if _, err := queryparams.ParseRequestLogFilter(newFilterReq(v)); err != nil {
				t.Errorf("%v: want no error, got %v", v, err)
			}
		}
	})

	t.Run("result window boundary allowed (R23)", func(t *testing.T) {
		f, err := queryparams.ParseRequestLogFilter(newFilterReq(url.Values{"page": {"20"}, "page_size": {"500"}}))
		if err != nil {
			t.Fatalf("page=20&page_size=500 (=10000) must be allowed: %v", err)
		}
		if f.Page != 20 || f.PageSize != 500 {
			t.Errorf("Page/PageSize = %d/%d, want 20/500", f.Page, f.PageSize)
		}
	})

	t.Run("stream tri-state", func(t *testing.T) {
		for _, tc := range []struct {
			v       url.Values
			wantNil bool
			want    bool
		}{
			{v: url.Values{"stream": {"true"}}, want: true},
			{v: url.Values{"stream": {"1"}}, want: true},
			{v: url.Values{"stream": {"false"}}, want: false},
			{v: url.Values{"stream": {"0"}}, want: false},
			{v: url.Values{"stream": {""}}, wantNil: true},
			{v: url.Values{}, wantNil: true},
		} {
			f, err := queryparams.ParseRequestLogFilter(newFilterReq(tc.v))
			if err != nil {
				t.Fatalf("%v: %v", tc.v, err)
			}
			if tc.wantNil {
				if f.Stream != nil {
					t.Errorf("%v: Stream = %v, want nil", tc.v, *f.Stream)
				}
				continue
			}
			if f.Stream == nil || *f.Stream != tc.want {
				t.Errorf("%v: Stream = %v, want %v", tc.v, f.Stream, tc.want)
			}
		}
	})

	t.Run("per-field time format is independent", func(t *testing.T) {
		f, err := queryparams.ParseRequestLogFilter(newFilterReq(url.Values{
			"start": {"2026-11-25"},
			"end":   {"2026-11-25T12:00:00Z"},
		}))
		if err != nil {
			t.Fatalf("ParseRequestLogFilter: %v", err)
		}
		if f.Start == nil || !f.Start.Equal(time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("Start = %v, want date-format 2026-11-25 00:00 UTC", f.Start)
		}
		if f.End == nil || !f.End.Equal(time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)) {
			t.Errorf("End = %v, want RFC3339 2026-11-25 12:00 UTC (exclusive, no +1 day)", f.End)
		}
	})
}
