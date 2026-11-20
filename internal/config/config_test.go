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

package config_test

import (
	"errors"
	"flag"
	"strings"
	"testing"

	"aigate/internal/config"
)

// 非法配置必须以英文错误拒绝（开源口径：运行文本恒英文）。
func TestLoad_InvalidValues(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{"unsupported db-type", []string{"-db-type", "oracle"}, "unsupported db-type"},
		{"empty addr", []string{"-addr", ""}, "addr must not be empty"},
		{"bad timezone", []string{"-timezone", "Mars/Olympus"}, "invalid timezone"},
		{"bad usage buffer", []string{"-usage-buffer", "0"}, "usage-buffer must be >= 1"},
		{"bad webhook buffer", []string{"-webhook-buffer", "0"}, "webhook-buffer must be >= 1"},
		{
			"bad delivery retention",
			[]string{"-webhook-delivery-retention-days", "-1"},
			"webhook-delivery-retention-days must be >= 0",
		},
		{"bad quota status", []string{"-quota-exceeded-status", "500"}, "quota-exceeded-status must be 429 or 403"},
		{
			"bad response retention",
			[]string{"-response-retention-days", "-1"},
			"response-retention-days must be >= 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Load(%v) err = %v, want substring %q", tc.args, err, tc.wantSub)
			}
		})
	}
}

// 合法的最小配置可加载（默认值就位）；-h 返回 flag.ErrHelp（由 main 以成功退出处理）。
func TestLoad_DefaultsAndHelp(t *testing.T) {
	cfg, err := config.Load(nil)
	if err != nil {
		t.Fatalf("Load(nil) err = %v", err)
	}
	if cfg.Addr != ":8080" || cfg.DBType != "sqlite" || cfg.DBDSN != "aigate.db" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.Timezone != "Asia/Shanghai" || cfg.UsageBuffer != 256 || cfg.WebhookBuffer != 256 ||
		cfg.WebhookDeliveryRetentionDays != 7 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.QuotaExceededStatus != 429 || cfg.AllowPrivateUpstreams {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	// Responses 有状态能力：存储默认开启、保留期默认 7 天（design §7）。
	if !cfg.AllowResponseStorage || cfg.ResponseRetentionDays != 7 {
		t.Fatalf("unexpected responses defaults: allow=%v retention=%d", cfg.AllowResponseStorage, cfg.ResponseRetentionDays)
	}

	if _, err := config.Load([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("Load(-h) err = %v, want flag.ErrHelp", err)
	}
}

// 环境变量非法时给出英文错误（含变量名与取值）。
func TestLoad_InvalidEnv(t *testing.T) {
	t.Setenv("AIGATE_USAGE_BUFFER", "not-a-number")
	_, err := config.Load(nil)
	if err == nil || !strings.Contains(err.Error(), "invalid integer in environment variable AIGATE_USAGE_BUFFER") {
		t.Fatalf("Load() err = %v, want invalid integer error", err)
	}
}

// Responses 有状态的两个配置项：flag > 环境变量 > 默认，非法环境变量拒绝启动。
func TestLoad_ResponseStorageOptions(t *testing.T) {
	cfg, err := config.Load([]string{"-allow-response-storage=false", "-response-retention-days=0"})
	if err != nil {
		t.Fatalf("Load() err = %v", err)
	}
	if cfg.AllowResponseStorage || cfg.ResponseRetentionDays != 0 {
		t.Fatalf("flags not applied: allow=%v retention=%d", cfg.AllowResponseStorage, cfg.ResponseRetentionDays)
	}

	t.Setenv("AIGATE_ALLOW_RESPONSE_STORAGE", "false")
	t.Setenv("AIGATE_RESPONSE_RETENTION_DAYS", "30")
	cfg, err = config.Load(nil)
	if err != nil {
		t.Fatalf("Load() err = %v", err)
	}
	if cfg.AllowResponseStorage || cfg.ResponseRetentionDays != 30 {
		t.Fatalf("env not applied: allow=%v retention=%d", cfg.AllowResponseStorage, cfg.ResponseRetentionDays)
	}

	t.Setenv("AIGATE_ALLOW_RESPONSE_STORAGE", "not-a-bool")
	if _, err := config.Load(nil); err == nil || !strings.Contains(err.Error(), "AIGATE_ALLOW_RESPONSE_STORAGE") {
		t.Fatalf("Load() err = %v, want invalid boolean error", err)
	}
}
