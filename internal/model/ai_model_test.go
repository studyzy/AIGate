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

package model_test

import (
	"testing"

	"aigate/internal/model"
)

func TestOutputTokenCeiling(t *testing.T) {
	cases := []struct {
		name                       string
		modelLimit, keyLimit, want int
	}{
		{"both unset", 0, 0, 0},
		{"only model", 1000, 0, 1000},
		{"only key", 0, 2000, 2000},
		{"model stricter", 1000, 2000, 1000},
		{"key stricter", 3000, 2000, 2000},
		{"equal", 2000, 2000, 2000},
		// 负数视作未配置：与「未指定」同义，不能当成 0 上限把请求压到 0。
		{"negative treated as unset", -1, 2000, 2000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := model.OutputTokenCeiling(c.modelLimit, c.keyLimit); got != c.want {
				t.Fatalf("OutputTokenCeiling(%d, %d) = %d, want %d", c.modelLimit, c.keyLimit, got, c.want)
			}
		})
	}
}

func TestJoinEffortLevels(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"empty", nil, ""},
		{"sorted by ladder, not input order", []string{"high", "low", "medium"}, "low,medium,high"},
		{"case and spaces normalized", []string{" HIGH ", "Low"}, "low,high"},
		{"none sorts first", []string{"low", "none"}, "none,low"},
		{"blank entries dropped", []string{"", "low", "  "}, "low"},
		{"duplicates collapse", []string{"low", "low"}, "low"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := model.JoinEffortLevels(c.in); got != c.want {
				t.Fatalf("JoinEffortLevels(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSplitEffortLevels(t *testing.T) {
	if got := model.SplitEffortLevels(""); got == nil || len(got) != 0 {
		t.Fatalf("empty stored value must yield an empty non-nil slice, got %#v", got)
	}
	got := model.SplitEffortLevels("low, medium ,high")
	if len(got) != 3 || got[0] != "low" || got[1] != "medium" || got[2] != "high" {
		t.Fatalf("SplitEffortLevels = %#v", got)
	}
}

func TestValidateEffortLevels(t *testing.T) {
	if msg := model.ValidateEffortLevels([]string{"low", "medium", "high"}); msg != "" {
		t.Fatalf("valid levels rejected: %q", msg)
	}
	if msg := model.ValidateEffortLevels([]string{"LOW"}); msg != "" {
		t.Fatalf("uppercase level must be accepted: %q", msg)
	}
	if msg := model.ValidateEffortLevels(nil); msg != "" {
		t.Fatalf("empty list must be accepted: %q", msg)
	}
	if msg := model.ValidateEffortLevels([]string{"extreme"}); msg == "" {
		t.Fatal("unknown level must be rejected")
	}
	if msg := model.ValidateEffortLevels([]string{"low", "low"}); msg == "" {
		t.Fatal("duplicate level must be rejected")
	}
}
