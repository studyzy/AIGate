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

// Package config 负责网关的运行配置：命令行 flag 与 AIGATE_* 环境变量（flag 优先，其次环境变量，最后默认值）。
// 配置非法时直接返回错误，由启动流程拒绝启动（不做静默兜底）。
package config

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// maxRetentionDays 是「按天计的保留期」配置项的统一上界（100 年）：
// -webhook-delivery-retention-days 与 -response-retention-days 共用。
//
// 取它的理由不是业务上限，而是挡住 time.Duration 溢出：装配层统一用
// time.Duration(days) * 24 * time.Hour 换算，超过 ~106751 天就溢出成负数，
// 而负的保留期会被清理循环当成「≤0 = 永不清理」——一次写错的配置就静默关掉了数据清理，
// 且没有任何报错。两个配置项是同一类缺陷，共用同一个上界与同一条校验。
const maxRetentionDays = 36500

const (
	// DBTypeSQLite 本地开发/体验用的嵌入式数据库。
	DBTypeSQLite = "sqlite"
	// DBTypeMySQL 生产部署使用的数据库。
	DBTypeMySQL = "mysql"
)

// Config 网关运行配置。
type Config struct {
	Addr                         string        // 监听地址，如 127.0.0.1:8080
	DBType                       string        // sqlite | mysql
	DBDSN                        string        // SQLite 文件路径；MySQL DSN
	Timezone                     string        // IANA 时区名（配额窗口与日聚合的业务口径）
	UsageBuffer                  int           // 用量异步写入队列容量
	WebhookBuffer                int           // webhook 事件投递队列容量
	WebhookDeliveryRetentionDays int           // webhook 投递记录保留天数（0 = 不清理）
	AllowResponseStorage         bool          // Responses 站点级存储开关（默认 true；false 时有状态能力整体不可用）
	ResponseRetentionDays        int           // Responses 会话状态保留天数（0 = 不清理）
	ResponseBackgroundTimeout    time.Duration // Responses background:true 的后台执行超时（默认 30 分钟）
	QuotaExceededStatus          int           // 配额超限状态码（M2 使用；默认 429）
	InitAdminToken               string        // 初始管理员 token（为空时首次启动自动生成并打印一次）
	AllowPrivateUpstreams        bool          // 是否放行私网/回环上游地址（SSRF 严格模式开关，默认 false）
	MasterKey                    string        // 主密钥（64 位 hex 或 base64，解码后 32 字节）；空 = 禁用加密
	MigrateEncrypt               bool          // 启动开关：把存量明文密钥类字段加密后退出（需主密钥）
	RequestLog                   string        // 请求日志实现名（-request-log / AIGATE_REQUEST_LOG，默认 "none"）
	RequestLogConfig             string        // 请求日志插件内联配置（-request-log-config / AIGATE_REQUEST_LOG_CONFIG，JSON 对象）
	RequestLogConfigFile         string        // 请求日志插件配置文件路径（-request-log-config-file / AIGATE_REQUEST_LOG_CONFIG_FILE）
}

// Load 解析配置。args 为命令行参数（不含程序名）。
func Load(args []string) (*Config, error) {
	usageBufferDefault, err := envInt("AIGATE_USAGE_BUFFER", 256)
	if err != nil {
		return nil, err
	}
	webhookBufferDefault, err := envInt("AIGATE_WEBHOOK_BUFFER", 256)
	if err != nil {
		return nil, err
	}
	webhookRetentionDefault, err := envInt("AIGATE_WEBHOOK_DELIVERY_RETENTION_DAYS", 7)
	if err != nil {
		return nil, err
	}
	quotaStatusDefault, err := envInt("AIGATE_QUOTA_EXCEEDED_STATUS", 429)
	if err != nil {
		return nil, err
	}
	allowPrivateDefault, err := envBool("AIGATE_ALLOW_PRIVATE_UPSTREAMS", false)
	if err != nil {
		return nil, err
	}
	allowResponseStorageDefault, err := envBool("AIGATE_ALLOW_RESPONSE_STORAGE", true)
	if err != nil {
		return nil, err
	}
	responseRetentionDefault, err := envInt("AIGATE_RESPONSE_RETENTION_DAYS", 7)
	if err != nil {
		return nil, err
	}
	responseBackgroundTimeoutDefault, err := envDuration("AIGATE_RESPONSE_BACKGROUND_TIMEOUT", 30*time.Minute)
	if err != nil {
		return nil, err
	}

	fs := flag.NewFlagSet("aigate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg := &Config{}
	fs.StringVar(&cfg.Addr, "addr", envOr("AIGATE_ADDR", ":8080"), "listen address")
	fs.StringVar(&cfg.DBType, "db-type", envOr("AIGATE_DB_TYPE", DBTypeSQLite), "database type: sqlite | mysql")
	fs.StringVar(&cfg.DBDSN, "db-dsn", envOr("AIGATE_DB_DSN", "aigate.db"), "SQLite file path or MySQL DSN")
	fs.StringVar(
		&cfg.Timezone,
		"timezone",
		envOr("AIGATE_TIMEZONE", "Asia/Shanghai"),
		"IANA timezone name (quota windows and daily aggregation)",
	)
	fs.IntVar(&cfg.UsageBuffer, "usage-buffer", usageBufferDefault, "capacity of the async usage-write queue")
	fs.IntVar(&cfg.WebhookBuffer, "webhook-buffer", webhookBufferDefault, "capacity of the webhook event queue")
	fs.IntVar(
		&cfg.WebhookDeliveryRetentionDays,
		"webhook-delivery-retention-days",
		webhookRetentionDefault,
		"days to retain webhook delivery records (0 = keep forever)",
	)
	fs.BoolVar(
		&cfg.AllowResponseStorage,
		"allow-response-storage",
		allowResponseStorageDefault,
		"allow POST /v1/responses to persist response state (false disables store/previous_response_id/conversation)",
	)
	fs.IntVar(
		&cfg.ResponseRetentionDays,
		"response-retention-days",
		responseRetentionDefault,
		"days to retain stored responses (0 = keep forever)",
	)
	fs.DurationVar(
		&cfg.ResponseBackgroundTimeout,
		"response-background-timeout",
		responseBackgroundTimeoutDefault,
		"max run time of a background:true Responses job (e.g. 30m, 90s)",
	)
	fs.IntVar(
		&cfg.QuotaExceededStatus,
		"quota-exceeded-status",
		quotaStatusDefault,
		"quota-exceeded status code (429 or 403)",
	)
	fs.StringVar(
		&cfg.InitAdminToken,
		"init-admin-token",
		os.Getenv("AIGATE_INIT_ADMIN_TOKEN"),
		"bootstrap admin token (generated and printed once when empty)",
	)
	fs.BoolVar(
		&cfg.AllowPrivateUpstreams,
		"allow-private-upstreams",
		allowPrivateDefault,
		"allow private/loopback upstreams (relaxes SSRF strict mode)",
	)
	fs.StringVar(
		&cfg.MasterKey,
		"master-key",
		os.Getenv("AIGATE_MASTER_KEY"),
		"AES-256-GCM master key (64 hex or base64, 32 bytes decoded; empty = plaintext storage)",
	)
	fs.BoolVar(
		&cfg.MigrateEncrypt,
		"migrate-encrypt",
		false,
		"encrypt existing plaintext secret rows and exit (requires -master-key)",
	)
	fs.StringVar(
		&cfg.RequestLog,
		"request-log",
		envOr("AIGATE_REQUEST_LOG", "none"),
		"request log implementation name",
	)
	fs.StringVar(
		&cfg.RequestLogConfig,
		"request-log-config",
		envOr("AIGATE_REQUEST_LOG_CONFIG", ""),
		"inline request log plugin config (JSON object)",
	)
	fs.StringVar(
		&cfg.RequestLogConfigFile,
		"request-log-config-file",
		envOr("AIGATE_REQUEST_LOG_CONFIG_FILE", ""),
		"path to request log plugin config file (JSON object)",
	)
	if err := fs.Parse(args); err != nil {
		// -h/--help：把用法与 flag 列表打印到 stdout，交给调用方以成功退出。
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(os.Stdout)
			fmt.Fprintln(os.Stdout, "Usage: aigate [flags]")
			fs.PrintDefaults()
			return nil, flag.ErrHelp
		}
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Location 返回业务时区。
func (c *Config) Location() (*time.Location, error) {
	return time.LoadLocation(c.Timezone)
}

func (c *Config) validate() error {
	if c.Addr == "" {
		return fmt.Errorf("addr must not be empty")
	}
	switch c.DBType {
	case DBTypeSQLite, DBTypeMySQL:
	default:
		return fmt.Errorf("unsupported db-type %q (want sqlite or mysql)", c.DBType)
	}
	if c.DBDSN == "" {
		return fmt.Errorf("db-dsn must not be empty")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("invalid timezone %q: %w", c.Timezone, err)
	}
	if c.UsageBuffer < 1 {
		return fmt.Errorf("usage-buffer must be >= 1")
	}
	// webhook-buffer 与 usage-buffer 同策略：非法配置直接拒绝启动（不做静默归一），
	// 避免「配置写错但服务照常起来」的隐性偏差。
	if c.WebhookBuffer < 1 {
		return fmt.Errorf("webhook-buffer must be >= 1")
	}
	if c.WebhookDeliveryRetentionDays < 0 {
		return fmt.Errorf("webhook-delivery-retention-days must be >= 0")
	}
	if c.ResponseRetentionDays < 0 {
		return fmt.Errorf("response-retention-days must be >= 0")
	}
	// 上界校验（两个按天计的保留期共用，理由见 maxRetentionDays 的注释）：
	// 把 time.Duration 溢出挡在启动期，而不是让它静默退化成「永不清理」。
	if c.WebhookDeliveryRetentionDays > maxRetentionDays {
		return fmt.Errorf("webhook-delivery-retention-days must be <= %d", maxRetentionDays)
	}
	if c.ResponseRetentionDays > maxRetentionDays {
		return fmt.Errorf("response-retention-days must be <= %d", maxRetentionDays)
	}
	// 后台超时必须为正：0 会让每个 background 任务一启动即超时，
	// 这类配置错误应当在启动期暴露，而不是变成一片 failed 行。
	if c.ResponseBackgroundTimeout <= 0 {
		return fmt.Errorf("response-background-timeout must be positive")
	}
	if c.QuotaExceededStatus != 429 && c.QuotaExceededStatus != 403 {
		return fmt.Errorf("quota-exceeded-status must be 429 or 403")
	}
	// 请求日志三项配置：非法值一律在启动期拒绝，禁止静默归一（design：请求日志插件能力）。
	if c.RequestLog == "" {
		return fmt.Errorf("request-log must not be empty")
	}
	if c.RequestLogConfig != "" && !isJSONObject(c.RequestLogConfig) {
		return fmt.Errorf("request-log-config must be a valid JSON object")
	}
	if c.RequestLogConfigFile != "" && strings.TrimSpace(c.RequestLogConfigFile) == "" {
		return fmt.Errorf("request-log-config-file must be a non-empty path")
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// isJSONObject 判断 s 是否为合法 JSON 对象：首个非空字符为 '{' 且整体为合法 JSON。
func isJSONObject(s string) bool {
	trimmed := strings.TrimSpace(s)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid([]byte(s))
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid integer in environment variable %s: %q", key, v)
	}
	return n, nil
}

// envDuration 解析时长类环境变量（time.ParseDuration 语法，如 "30m"、"90s"）。
func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid duration in environment variable %s: %q", key, v)
	}
	return d, nil
}

func envBool(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("invalid boolean in environment variable %s: %q", key, v)
	}
	return b, nil
}
