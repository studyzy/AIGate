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

// Package queryparams 集中实现列表类接口的 HTTP 查询参数解析与用量明细行视图：
// 管理面（/admin/*）与用户面（/api/user/*）共用同一口径，避免两处实现漂移
// （分页上限、日期解释、group_by 白名单、行级隔离参数等）。请求日志等按时间过滤的
// 列表在此把日期 / RFC3339 边界统一归一为半开 UTC 区间 [Start, End)，供各存储后端直接消费。
package queryparams

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aigate/internal/model"
)

// 分页参数（用量明细、webhook 投递记录等列表接口共用）。
const (
	// DefaultPageSize 默认分页大小。
	DefaultPageSize = 50
	// MaxPageSize 分页大小上限。
	MaxPageSize = 500
	// MaxPage 页码上限：防止 (page-1)*pageSize 在乘法中溢出为负 OFFSET
	//（负 OFFSET 在 MySQL 是语法错误、SQLite 会被当作 0，两者行为不一致）。
	MaxPage = 1_000_000
)

// MaxResultWindow 是列表查询可返回的最大结果窗口（对应 ES index.max_result_window）：
// 分页的 (page-1)*page_size + page_size 不得超过该值，避免深度翻页拖垮存储；等于边界值允许。
const MaxResultWindow = 10000

// RequestLogFilter 是请求日志列表的过滤条件。
// Start/End 为半开 UTC 区间 [Start, End) 的端点（nil 表示该侧不限制）；其余零值字段
// 均表示「不过滤」，由存储层据此组装查询（Task 8 映射为 requestlog.Query）。
type RequestLogFilter struct {
	Start, End                 *time.Time
	APIKeyID                   uint
	APIKeyHash, Model, Path    string
	StatusCode                 int
	Stream                     *bool
	MinLatencyMS, MaxLatencyMS int
	Q                          string
	Page, PageSize             int
}

// UsageLogRow 是用量明细的对外表示（显式 JSON 字段名，不含任何内部/敏感列）。
type UsageLogRow struct {
	ID                     uint   `json:"id"`
	APIKeyID               uint   `json:"api_key_id"`
	UserID                 uint   `json:"user_id"`
	GroupID                uint   `json:"group_id"`
	AIModelID              uint   `json:"ai_model_id"`
	Model                  string `json:"model"`
	Provider               string `json:"provider"`
	PromptTokens           int    `json:"prompt_tokens"`
	CompletionTokens       int    `json:"completion_tokens"`
	TotalTokens            int    `json:"total_tokens"`
	PromptCacheReadTokens  int    `json:"prompt_cache_read_tokens"`
	PromptCacheWriteTokens int    `json:"prompt_cache_write_tokens"`
	StatusCode             int    `json:"status_code"`
	Latency                int    `json:"latency"`
	// Currency 为本行金额的计价币种，取平台结算币种：明细表不冗余币种，
	// 而两列金额中 cost_platform_micros 已按记账时刻汇率统一到结算币种、恒可相加，
	// 故这里报告结算币种而非模型币种（后者需 join ai_models，且与结算金额不同币种）。
	Currency string `json:"currency"`
	// CostVendorMicros 记账时的模型币种金额（微元）；币种见 ai_models.currency，本行 Currency 不适用于它。
	CostVendorMicros int64 `json:"cost_vendor_micros"`
	// CostPlatformMicros 记账时的平台结算币种金额（微元）；与 Currency 同币种。
	CostPlatformMicros int64 `json:"cost_platform_micros"`
	// CountTowardQuota 为 false 表示这是网关内部开销（如 auto 路由的决策调用）：
	// 金额真实发生并记在本行，但不消耗任何配额。报表据此把平台侧路由开销与用户消费分开。
	CountTowardQuota bool      `json:"count_toward_quota"`
	CreatedAt        time.Time `json:"created_at"`
}

// ValidGroupBy 校验聚合维度取值。
func ValidGroupBy(v string) bool {
	switch v {
	case "date", "model", "key", "user", "group":
		return true
	default:
		return false
	}
}

// ParsePagination 解析 page / page_size（默认 1/50，page_size 上限 500，page 上限 MaxPage）。
func ParsePagination(r *http.Request) (int, int, error) {
	q := r.URL.Query()
	page := 1
	if s := strings.TrimSpace(q.Get("page")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > MaxPage {
			return 0, 0, fmt.Errorf("invalid page")
		}
		page = n
	}
	pageSize := DefaultPageSize
	if s := strings.TrimSpace(q.Get("page_size")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return 0, 0, fmt.Errorf("invalid page_size")
		}
		pageSize = n
	}
	if pageSize > MaxPageSize {
		pageSize = MaxPageSize
	}
	return page, pageSize, nil
}

// ParseUsageFilter 解析用量查询过滤参数（start/end 为 YYYY-MM-DD，按业务时区解释，end 含当日）。
// forcedUserID>0 时强制覆盖 user_id 且完全忽略请求中的 user_id（用户面行级隔离用）。
func ParseUsageFilter(r *http.Request, forcedUserID uint) (model.UsageFilter, error) {
	q := r.URL.Query()
	var f model.UsageFilter
	if s := strings.TrimSpace(q.Get("start")); s != "" {
		d, err := model.ParseDateLabel(s)
		if err != nil {
			return f, fmt.Errorf("invalid start date, want YYYY-MM-DD")
		}
		f.StartDate = &d
	}
	if s := strings.TrimSpace(q.Get("end")); s != "" {
		d, err := model.ParseDateLabel(s)
		if err != nil {
			return f, fmt.Errorf("invalid end date, want YYYY-MM-DD")
		}
		f.EndDate = &d
	}
	if forcedUserID > 0 {
		f.UserID = forcedUserID
	} else {
		var err error
		if f.UserID, err = parseUintQuery(q.Get("user_id"), "user_id"); err != nil {
			return f, err
		}
	}
	var err error
	if f.GroupID, err = parseUintQuery(q.Get("group_id"), "group_id"); err != nil {
		return f, err
	}
	if f.APIKeyID, err = parseUintQuery(q.Get("api_key_id"), "api_key_id"); err != nil {
		return f, err
	}
	if f.AIModelID, err = parseUintQuery(q.Get("ai_model_id"), "ai_model_id"); err != nil {
		return f, err
	}
	return f, nil
}

// parseUintQuery 解析非负整型查询参数；空串视为 0（不过滤）。
func parseUintQuery(raw, name string) (uint, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s", name)
	}
	return uint(n), nil
}

// parseIntQuery 解析非负整型查询参数；空串视为 0（不过滤），非法值与负数沿用
// invalid <name> 风格（与 openapi.yaml 的 minimum: 0 一致）。
func parseIntQuery(raw, name string) (int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid %s", name)
	}
	return n, nil
}

// parseTimeBound 解析单个时间边界，统一为 UTC 时刻：
//   - 含 T 的按 RFC3339 解析后转 UTC（end 排他，原样返回）；
//   - 否则按 YYYY-MM-DD 在配置的业务时区（model.BusinessLocation）取当日 00:00，
//     换算成对应的 UTC 瞬间；start 直接取该瞬间，end 含当日则靠 AddDate(0,0,1)
//     挪到次日业务时区 00:00 对应的 UTC 瞬间，作为半开区间的排他上界
//     （在带时区的 time 上做 AddDate，夏令时也正确）。
//
// 空串返回 nil（该侧不限制）；非法值返回 invalid <name> 错误。
func parseTimeBound(raw, name string, isEnd bool) (*time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	if strings.Contains(s, "T") {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, fmt.Errorf("invalid %s", name)
		}
		tu := t.UTC()
		return &tu, nil
	}
	loc := model.BusinessLocation()
	t, err := time.ParseInLocation("2006-01-02", s, loc)
	if err != nil {
		return nil, fmt.Errorf("invalid %s", name)
	}
	if !isEnd {
		tu := t.UTC()
		return &tu, nil
	}
	tu := t.AddDate(0, 0, 1).UTC()
	return &tu, nil
}

// ParseRequestLogFilter 解析请求日志列表的全部查询参数，归一为半开 UTC 区间与统一口径的过滤条件。
// 复用 ParsePagination 取 page/page_size；非法参数返回英文错误文案（调用方据此回 400），
// 结果窗口校验放在最后。
func ParseRequestLogFilter(r *http.Request) (RequestLogFilter, error) {
	q := r.URL.Query()
	var f RequestLogFilter

	// 分页复用既有口径（默认值、上限、非法值校验）。
	page, pageSize, err := ParsePagination(r)
	if err != nil {
		return f, err
	}
	f.Page, f.PageSize = page, pageSize

	if f.Start, err = parseTimeBound(q.Get("start"), "start", false); err != nil {
		return f, err
	}
	if f.End, err = parseTimeBound(q.Get("end"), "end", true); err != nil {
		return f, err
	}
	if f.APIKeyID, err = parseUintQuery(q.Get("api_key_id"), "api_key_id"); err != nil {
		return f, err
	}
	f.APIKeyHash = strings.TrimSpace(q.Get("api_key_hash"))
	f.Model = strings.TrimSpace(q.Get("model"))
	f.Path = strings.TrimSpace(q.Get("path"))
	if f.StatusCode, err = parseIntQuery(q.Get("status_code"), "status_code"); err != nil {
		return f, err
	}
	if s := strings.TrimSpace(q.Get("stream")); s != "" {
		v, perr := strconv.ParseBool(s)
		if perr != nil {
			return f, fmt.Errorf("invalid stream")
		}
		f.Stream = &v
	}
	if f.MinLatencyMS, err = parseIntQuery(q.Get("min_latency_ms"), "min_latency_ms"); err != nil {
		return f, err
	}
	if f.MaxLatencyMS, err = parseIntQuery(q.Get("max_latency_ms"), "max_latency_ms"); err != nil {
		return f, err
	}
	f.Q = strings.TrimSpace(q.Get("q"))

	// 跨字段校验：仅在两侧都设定时比较（0 表示该侧无上/下界）。
	if f.Start != nil && f.End != nil && f.Start.After(*f.End) {
		return f, fmt.Errorf("invalid time range")
	}
	if f.MinLatencyMS != 0 && f.MaxLatencyMS != 0 && f.MinLatencyMS > f.MaxLatencyMS {
		return f, fmt.Errorf("invalid latency range")
	}
	// 结果窗口校验放最后：对应 ES from+size > max_result_window，边界值（==）允许。
	if (f.Page-1)*f.PageSize+f.PageSize > MaxResultWindow {
		return f, fmt.Errorf("page out of range")
	}
	return f, nil
}

// ToUsageLogRow 将用量日志实体转为对外表示。
// platformCurrency 为平台结算币种（取自设置），作为行级金额币种；空串时退回 model.DefaultPlatformCurrency。
func ToUsageLogRow(l *model.LLMUsageLog, platformCurrency string) UsageLogRow {
	if platformCurrency == "" {
		platformCurrency = model.DefaultPlatformCurrency
	}
	return UsageLogRow{
		ID:                     l.ID,
		APIKeyID:               l.APIKeyID,
		UserID:                 l.UserID,
		GroupID:                l.GroupID,
		AIModelID:              l.AIModelID,
		Model:                  l.Model,
		Provider:               l.Provider,
		PromptTokens:           l.PromptTokens,
		CompletionTokens:       l.CompletionTokens,
		TotalTokens:            l.TotalTokens,
		PromptCacheReadTokens:  l.PromptCacheReadTokens,
		PromptCacheWriteTokens: l.PromptCacheWriteTokens,
		StatusCode:             l.StatusCode,
		Latency:                l.Latency,
		Currency:               platformCurrency,
		CostVendorMicros:       l.CostVendorMicros,
		CostPlatformMicros:     l.CostPlatformMicros,
		CountTowardQuota:       l.CountsTowardQuota(),
		CreatedAt:              l.CreatedAt,
	}
}
