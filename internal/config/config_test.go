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
	"time"

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
		{
			"overflowing response retention",
			[]string{"-response-retention-days", "9223372036854775807"},
			"response-retention-days must be <=",
		},
		{
			// 同一类溢出（同源修复）：装配层同样是 time.Duration(days) * 24h。
			"overflowing webhook delivery retention",
			[]string{"-webhook-delivery-retention-days", "9223372036854775807"},
			"webhook-delivery-retention-days must be <=",
		},
		{
			"webhook delivery retention just above the bound",
			[]string{"-webhook-delivery-retention-days", "36501"},
			"webhook-delivery-retention-days must be <=",
		},
		{
			"response retention just above the bound",
			[]string{"-response-retention-days", "36501"},
			"response-retention-days must be <=",
		},
		{
			"non-positive background timeout",
			[]string{"-response-background-timeout", "0s"},
			"response-background-timeout must be positive",
		},
		{
			"malformed background timeout",
			[]string{"-response-background-timeout", "30"},
			"invalid value \"30\" for flag -response-background-timeout",
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
	// background 后台执行超时默认 30 分钟（design §5.6）。
	if cfg.ResponseBackgroundTimeout != 30*time.Minute {
		t.Fatalf("response background timeout default = %v, want 30m", cfg.ResponseBackgroundTimeout)
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

// TestLoad_ResponseBackgroundTimeout：flag 与环境变量都能设置后台执行超时，flag 优先。
func TestLoad_ResponseBackgroundTimeout(t *testing.T) {
	t.Setenv("AIGATE_RESPONSE_BACKGROUND_TIMEOUT", "45m")
	cfg, err := config.Load(nil)
	if err != nil {
		t.Fatalf("Load() err = %v", err)
	}
	if cfg.ResponseBackgroundTimeout != 45*time.Minute {
		t.Fatalf("env value not applied: %v", cfg.ResponseBackgroundTimeout)
	}

	cfg, err = config.Load([]string{"-response-background-timeout", "90s"})
	if err != nil {
		t.Fatalf("Load() err = %v", err)
	}
	if cfg.ResponseBackgroundTimeout != 90*time.Second {
		t.Fatalf("flag value not applied (flag must win): %v", cfg.ResponseBackgroundTimeout)
	}

	t.Setenv("AIGATE_RESPONSE_BACKGROUND_TIMEOUT", "not-a-duration")
	if _, err := config.Load(nil); err == nil ||
		!strings.Contains(err.Error(), "AIGATE_RESPONSE_BACKGROUND_TIMEOUT") {
		t.Fatalf("Load() err = %v, want invalid duration error", err)
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

// TestLoad_RetentionBoundsAreInclusive：上界本身的边界口径——上界值可用，上界 +1 拒绝。
// 两个按天计的保留期共用 maxRetentionDays，这条用例同时钉住「都能停下界内合法值」。
func TestLoad_RetentionBoundsAreInclusive(t *testing.T) {
	cfg, err := config.Load([]string{
		"-response-retention-days", "36500",
		"-webhook-delivery-retention-days", "36500",
	})
	if err != nil {
		t.Fatalf("Load() at the bound err = %v, want it to be accepted", err)
	}
	if cfg.ResponseRetentionDays != 36500 || cfg.WebhookDeliveryRetentionDays != 36500 {
		t.Fatalf("bound values not applied: %+v", cfg)
	}
}

// 请求日志三项配置的默认值：none / "" / ""。
func TestLoad_RequestLogDefaults(t *testing.T) {
	// 显式清空相关环境变量，避免外部 env 干扰默认值断言。
	t.Setenv("AIGATE_REQUEST_LOG", "")
	t.Setenv("AIGATE_REQUEST_LOG_CONFIG", "")
	t.Setenv("AIGATE_REQUEST_LOG_CONFIG_FILE", "")
	cfg, err := config.Load(nil)
	if err != nil {
		t.Fatalf("Load(nil) 失败：%v", err)
	}
	if cfg.RequestLog != "none" || cfg.RequestLogConfig != "" || cfg.RequestLogConfigFile != "" {
		t.Fatalf("请求日志默认值不符：log=%q config=%q file=%q",
			cfg.RequestLog, cfg.RequestLogConfig, cfg.RequestLogConfigFile)
	}
}

// 非法 request-log-config（flag 与 env 覆盖）都在启动期以含 "request-log-config" 的错误拒绝。
func TestLoad_RequestLogInvalidJSON(t *testing.T) {
	for _, bad := range []string{"[]", "not-json", "{bad}"} {
		_, err := config.Load([]string{"-request-log-config", bad})
		if err == nil || !strings.Contains(err.Error(), "request-log-config") {
			t.Errorf("非法 request-log-config %q 应返回含 'request-log-config' 的错误，实得 %v", bad, err)
		}
	}
	// 非法 env 覆盖同口径。
	t.Setenv("AIGATE_REQUEST_LOG_CONFIG", "not-json")
	_, err := config.Load(nil)
	if err == nil || !strings.Contains(err.Error(), "request-log-config") {
		t.Errorf("非法 env 覆盖应同样拒绝，实得 %v", err)
	}
}

// 三项配置遵循 flag > env > 默认，合法值可加载。
func TestLoad_RequestLogOptions(t *testing.T) {
	t.Setenv("AIGATE_REQUEST_LOG", "sqlite")
	t.Setenv("AIGATE_REQUEST_LOG_CONFIG", `{"env":true}`)
	t.Setenv("AIGATE_REQUEST_LOG_CONFIG_FILE", "/env/rl.json")
	cfg, err := config.Load(nil)
	if err != nil {
		t.Fatalf("Load() 失败：%v", err)
	}
	if cfg.RequestLog != "sqlite" || cfg.RequestLogConfig != `{"env":true}` || cfg.RequestLogConfigFile != "/env/rl.json" {
		t.Fatalf("env 未生效：log=%q config=%q file=%q",
			cfg.RequestLog, cfg.RequestLogConfig, cfg.RequestLogConfigFile)
	}

	// flag 覆盖 env。
	cfg, err = config.Load([]string{
		"-request-log", "none",
		"-request-log-config", `{"flag":true}`,
		"-request-log-config-file", "/flag/rl.json",
	})
	if err != nil {
		t.Fatalf("Load() 失败：%v", err)
	}
	if cfg.RequestLog != "none" || cfg.RequestLogConfig != `{"flag":true}` || cfg.RequestLogConfigFile != "/flag/rl.json" {
		t.Fatalf("flag 未覆盖 env：log=%q config=%q file=%q",
			cfg.RequestLog, cfg.RequestLogConfig, cfg.RequestLogConfigFile)
	}
}

// request-log 为空时拒绝，禁止静默归一成 "none"。
func TestLoad_RequestLogNotEmpty(t *testing.T) {
	_, err := config.Load([]string{"-request-log", ""})
	if err == nil || !strings.Contains(err.Error(), "request-log must not be empty") {
		t.Fatalf("空 request-log 应拒绝，实得 %v", err)
	}
}

// request-log-config-file 非空但为空白路径时拒绝；合法非空路径通过。
func TestLoad_RequestLogConfigFilePath(t *testing.T) {
	_, err := config.Load([]string{"-request-log-config-file", "   "})
	if err == nil || !strings.Contains(err.Error(), "request-log-config-file") {
		t.Fatalf("空白路径应拒绝（禁止静默归一），实得 %v", err)
	}
	cfg, err := config.Load([]string{"-request-log-config-file", "/tmp/rl.json"})
	if err != nil {
		t.Fatalf("合法非空路径被拒绝：%v", err)
	}
	if cfg.RequestLogConfigFile != "/tmp/rl.json" {
		t.Fatalf("路径未生效：%q", cfg.RequestLogConfigFile)
	}
}
