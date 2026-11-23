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

package quota

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"aigate/internal/billing"
	"aigate/internal/model"
)

// 超限维度。
const (
	kindGlobal = "global"
	kindModel  = "model"
	kindUser   = "user"
	kindKey    = "key"
)

// 全局配额作用域。
const (
	scopeSite  = "site"
	scopeGroup = "group"
)

// Status 是一次配额检查的结果。
type Status struct {
	Exceeded   bool
	Kind       string // "global" | "model" | "user" | "key"；未超限时为空
	Used       int64
	Limit      int
	Unit       string        // 非空即 UnitCost（token 规则留空以保持旧结果不变）；cost 时 Used 为微元、Limit 为整单位
	Currency   string        // cost 规则的结算币种（由调用侧按平台结算币种填充）
	RetryAfter time.Duration // 0 = 无法推算，调用方回退默认值（如 3600s）
}

// 检查结果未超限时的零值（用于 fail-open 与通过路径）。
func allowed() Status { return Status{} }

// checkRules 在规则窗口内逐条检查用量上限，供全局 / 用户 / 密钥三级配额复用：
//   - limit < 0 的规则跳过；
//   - 规则当前不生效（active=false）→ fail-closed：判定超限（Used=0 / RetryAfter=0）；
//   - 用量读取故障 → fail-open：调用 warnUsageReadFailed 记警告并放行；
//   - used >= limit → 超限（携带 Used / Limit / RetryAfter；cost 规则另带 Unit=UnitCost）。
//
// 计量单位按 rule.EffectiveUnit() 选择：token 规则用 sum（token 总量）；
// cost 规则用 sumCost（窗口内平台结算币种微元），限额换算为 int64(limit)*billing.MicrosPerUnit。
// sumCost 为 nil 时 cost 规则同样按用量读取故障 fail-open（记警告），绝不误拒。
// 结果只带计量单位；cost 规则的结算币种由 CheckRequest 统一补齐（fillCurrency）。
// kind 写入超限结果的 Kind 字段。
func checkRules(
	ctx context.Context,
	rules []Rule,
	now time.Time,
	kind string,
	sum func(ctx context.Context, from time.Time, to *time.Time) (int64, error),
	sumCost func(ctx context.Context, from time.Time, to *time.Time) (int64, error),
	warnUsageReadFailed func(err error),
) Status {
	for _, rule := range rules {
		if rule.Limit < 0 {
			continue
		}
		unit := rule.EffectiveUnit()
		from, to, active := rule.Window(now)
		if !active {
			// fail-closed：窗口不生效即拒绝，只带计量单位与限额，不编造用量。
			return Status{Exceeded: true, Kind: kind, Limit: rule.Limit, Unit: unitStatus(unit)}
		}
		read := sum
		if unit == UnitCost {
			read = sumCost
		}
		if read == nil {
			warnUsageReadFailed(errors.New("cost usage reader not available"))
			return allowed()
		}
		used, err := read(ctx, from, to)
		if err != nil {
			warnUsageReadFailed(err)
			return allowed()
		}
		// token 规则比较 token 总量；cost 规则的 used 为微元，限额换算为 整单位 × MicrosPerUnit。
		// 上界已由 ValidateRules 保证不溢出；此处仍做饱和兜底，避免直接构造的非法规则溢出为负。
		limit := int64(rule.Limit)
		if unit == UnitCost {
			limit = mulMicrosSaturating(int64(rule.Limit))
		}
		if used >= limit {
			return Status{
				Exceeded:   true,
				Kind:       kind,
				Used:       used,
				Limit:      rule.Limit,
				Unit:       unitStatus(unit),
				RetryAfter: retryAfter(to, now),
			}
		}
	}
	return allowed()
}

// unitStatus 把规则的生效单位映射为 Status.Unit：token 缺省不填（保持旧结果的零值与
// 既有断言逐字段相等），仅 cost 显式标注；调用方据 Status.Unit == UnitCost 判断
// Used 是微元、Limit 是整单位。
func unitStatus(effectiveUnit string) string {
	if effectiveUnit == UnitCost {
		return UnitCost
	}
	return ""
}

// mulMicrosSaturating 返回 units × billing.MicrosPerUnit，溢出时饱和到 math.MaxInt64。
func mulMicrosSaturating(units int64) int64 {
	if units > math.MaxInt64/billing.MicrosPerUnit {
		return math.MaxInt64
	}
	return units * billing.MicrosPerUnit
}

// CheckGlobal 检查全站或某个分组在规则窗口内的用量是否超限。
//
// scope=group 时按 groupID 的分组口径统计（groupID=0 即未分组桶，仍与其它组隔离）；
// 其它取值按全站口径统计。rules 为空表示不限制。
//
// 规则当前不生效（active=false）→ fail-closed：判定超限（Used=0，RetryAfter=0）。
// 用量读取故障 → fail-open：记警告并放行。
func CheckGlobal(
	ctx context.Context,
	store *model.Store,
	scope string,
	groupID uint,
	rules []Rule,
	now time.Time,
) Status {
	if len(rules) == 0 {
		return allowed()
	}
	sum := func(ctx context.Context, from time.Time, to *time.Time) (int64, error) {
		if scope == scopeGroup {
			return store.SumGroupTokensInWindow(ctx, groupID, from, to)
		}
		return store.SumGlobalTokensInWindow(ctx, from, to)
	}
	// cost 口径的过滤维度与 token 口径一致：组作用域按组，其余为全站（无过滤）。
	sumCost := func(ctx context.Context, from time.Time, to *time.Time) (int64, error) {
		filter := model.UsageFilter{}
		if scope == scopeGroup {
			filter.GroupID = groupID
		}
		return store.SumUsageCostMicrosInWindow(ctx, filter, from, to)
	}
	warn := func(err error) {
		slog.Warn(
			"global quota usage read failed, allowing request",
			"error",
			err,
			"scope",
			scope,
			"group_id",
			groupID,
		)
	}
	return checkRules(ctx, rules, now, kindGlobal, sum, sumCost, warn)
}

// CheckModelDaily 检查模型在业务时区「今天」的用量是否超出 quotaDay（<0 表示不限制）。
// 用量读取故障 → fail-open：记警告并放行。
func CheckModelDaily(ctx context.Context, store *model.Store, aiModelID uint, quotaDay int, now time.Time) Status {
	if quotaDay < 0 {
		return allowed()
	}
	used, err := store.SumModelDailyTokens(ctx, aiModelID)
	if err != nil {
		slog.Warn("model daily quota usage read failed, allowing request", "error", err, "ai_model_id", aiModelID)
		return allowed()
	}
	if used >= int64(quotaDay) {
		return Status{
			Exceeded:   true,
			Kind:       kindModel,
			Used:       used,
			Limit:      quotaDay,
			RetryAfter: retryUntilNextLocalMidnight(now),
		}
	}
	return allowed()
}

// CheckUser 检查用户在规则窗口内的用量是否超限。
//
// groupID>0 时只统计该分组下的用量，groupID=0 统计该用户全部用量。rules 为空表示不限制。
// 规则当前不生效 → fail-closed；用量读取故障 → fail-open（记警告并放行）。
func CheckUser(ctx context.Context, store *model.Store, userID, groupID uint, rules []Rule, now time.Time) Status {
	if len(rules) == 0 {
		return allowed()
	}
	sum := func(ctx context.Context, from time.Time, to *time.Time) (int64, error) {
		return store.SumUserTokensInWindow(ctx, userID, groupID, from, to)
	}
	// cost 口径沿用同一过滤维度：user_id 必加，group_id>0 时再按分组收窄。
	sumCost := func(ctx context.Context, from time.Time, to *time.Time) (int64, error) {
		return store.SumUsageCostMicrosInWindow(ctx, model.UsageFilter{UserID: userID, GroupID: groupID}, from, to)
	}
	warn := func(err error) {
		slog.Warn(
			"user quota usage read failed, allowing request",
			"error",
			err,
			"user_id",
			userID,
			"group_id",
			groupID,
		)
	}
	return checkRules(ctx, rules, now, kindUser, sum, sumCost, warn)
}

// CheckKey 检查密钥级周期配额（照搬 hatchery CheckAgentTokenQuota 语义）。
// rules 为空（含空串/非法 JSON 解析结果）表示不限制；limit<0 跳过；
// 窗口不生效 → fail-closed；用量读取故障 → fail-open（记警告并放行）。
func CheckKey(ctx context.Context, store *model.Store, apiKeyID uint, rules []Rule, now time.Time) Status {
	if len(rules) == 0 {
		return allowed()
	}
	sum := func(ctx context.Context, from time.Time, to *time.Time) (int64, error) {
		return store.SumKeyTokensInWindow(ctx, apiKeyID, from, to)
	}
	// cost 口径按密钥 id 过滤，与 token 口径一致。
	sumCost := func(ctx context.Context, from time.Time, to *time.Time) (int64, error) {
		return store.SumUsageCostMicrosInWindow(ctx, model.UsageFilter{APIKeyID: apiKeyID}, from, to)
	}
	warn := func(err error) {
		slog.Warn("key quota usage read failed, allowing request", "error", err, "api_key_id", apiKeyID)
	}
	return checkRules(ctx, rules, now, kindKey, sum, sumCost, warn)
}

// CheckRequest 按「全局 → 模型日配额 → 用户 → 密钥」顺序检查一次请求，返回首个超限项；
// 全部通过返回 Status{Exceeded:false}。
//
// 模型日配额按本次实际路由到的模型计量（resolved.AIModelID / resolved.QuotaDay）；
// resolved 为 nil 或 quotaDay<0 时跳过；配置读取失败按 fail-open 放行（记警告）。
//
// 分组口径：组规则解析与按组统计均使用「密钥的分组快照」（key.GroupID，建密钥时写入）——
// 用户换组后存量密钥仍按原组规则执行，重签/新建密钥后收敛到新组。
//
// cost 规则的结算币种在出现超限结果时才解析（fillCurrency），一次请求至多读一次设置；
// 未超限的请求完全不读设置，也不改变既有判定路径。
func CheckRequest(
	ctx context.Context,
	store *model.Store,
	key *model.APIKey,
	resolved *model.Resolved,
	now time.Time,
) Status {
	globalRules, scope, err := ResolveGlobalScope(ctx, store, key.GroupID)
	if err != nil {
		slog.Warn("resolve global quota rules failed, allowing request", "error", err)
	} else if st := CheckGlobal(ctx, store, scope, key.GroupID, globalRules, now); st.Exceeded {
		return fillCurrency(ctx, store, st)
	}

	if resolved != nil && resolved.QuotaDay >= 0 {
		if st := CheckModelDaily(ctx, store, resolved.AIModelID, resolved.QuotaDay, now); st.Exceeded {
			return st
		}
	}

	userRules, err := ResolveUserRules(ctx, store, key.GroupID)
	if err != nil {
		slog.Warn("resolve user quota rules failed, allowing request", "error", err, "group_id", key.GroupID)
	} else if st := CheckUser(ctx, store, key.UserID, key.GroupID, userRules, now); st.Exceeded {
		return fillCurrency(ctx, store, st)
	}

	// 密钥级周期配额（照搬 hatchery 单 Agent 配额语义）：空串/非法 JSON/[] 一律视为不限制。
	keyRules, _ := ParseRules(key.TokenQuotaRules)
	if st := CheckKey(ctx, store, key.ID, keyRules, now); st.Exceeded {
		return fillCurrency(ctx, store, st)
	}
	return allowed()
}

// fillCurrency 为 cost 规则的超限结果补上结算币种（Status.Currency）。
//
// token 规则与模型日配额（Status.Unit 为空）原样返回，不读设置也不改变零值；
// 币种读取失败时留空（记警告）——币种只用于向调用方说明金额口径，不影响拒绝本身。
func fillCurrency(ctx context.Context, store *model.Store, st Status) Status {
	if st.Unit != UnitCost {
		return st
	}
	settlement, err := settlementCurrency(ctx, store)
	if err != nil {
		slog.Warn("resolve settlement currency for quota rejection failed", "error", err)
		return st
	}
	st.Currency = settlement
	return st
}

// settlementCurrency 读取平台结算币种（settings.platform_currency）；未配置时回退默认币种。
func settlementCurrency(ctx context.Context, store *model.Store) (string, error) {
	st, err := store.GetSettings(ctx)
	if err != nil {
		return "", fmt.Errorf("get settings: %w", err)
	}
	if st.PlatformCurrency == "" {
		return model.DefaultPlatformCurrency, nil
	}
	return st.PlatformCurrency, nil
}

// ResolveUserRules 解析用户的生效规则：所属分组的策略优先，未配置时回退全局默认规则。
//
//   - 组不存在（或未指定分组，groupID=0）→ 回退全局默认规则；
//   - 组策略为空串（未配置）或非法 JSON → 回退全局默认规则；
//   - 组策略为 "[]" → 显式不限，返回空切片（不回退）；
//   - 其余 → 返回组策略规则。
//
// 读取故障返回 error，由调用方按 fail-open 处理。
func ResolveUserRules(ctx context.Context, store *model.Store, groupID uint) ([]Rule, error) {
	if groupID > 0 {
		g, err := store.GetGroup(ctx, groupID)
		switch {
		case err == nil:
			if rules, ok := ParseRules(g.TokenQuotaRules); ok {
				return rules, nil
			}
			// TokenQuotaRules 为空串或非法 JSON → 回退全局默认规则。
		case !model.IsNotFound(err):
			return nil, fmt.Errorf("get group %d: %w", groupID, err)
		}
	}
	return defaultUserRules(ctx, store)
}

// ResolveGlobalScope 解析全局配额规则与作用域（照搬 hatchery
// resolveEffectiveGlobalTokenQuotaScope 的「组策略优先 → 站点默认」链）：
//
//   - groupID>0 且该组配置了显式全局规则（含 "[]" 显式不限）→ 用组规则，
//     且作用域固定为按组计量（组用量 vs 组规则，独立池）；
//   - 组未配置（空串/组不存在/非法 JSON）→ 回退站点规则，作用域按
//     settings.global_quota_scope（非法值归一为 site）。
//
// 读取故障返回 error（调用方按 fail-open 放行）。
func ResolveGlobalScope(ctx context.Context, store *model.Store, groupID uint) ([]Rule, string, error) {
	if groupID > 0 {
		g, err := store.GetGroup(ctx, groupID)
		switch {
		case err == nil:
			if rules, ok := ParseRules(g.GlobalTokenQuotaRules); ok {
				return rules, scopeGroup, nil
			}
			// 空串或非法 JSON → 未配置，回退站点规则。
		case !model.IsNotFound(err):
			return nil, "", fmt.Errorf("get group %d: %w", groupID, err)
		}
	}
	st, err := store.GetSettings(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("get settings: %w", err)
	}
	rules, _ := ParseRules(st.GlobalTokenQuotaRules)
	scope := st.GlobalQuotaScope
	if scope != scopeSite && scope != scopeGroup {
		scope = scopeSite
	}
	return rules, scope, nil
}

// defaultUserRules 返回全局默认用户规则（空串/非法 JSON → 空规则，即不限制）。
func defaultUserRules(ctx context.Context, store *model.Store) ([]Rule, error) {
	st, err := store.GetSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("get settings: %w", err)
	}
	rules, _ := ParseRules(st.DefaultTokenQuotaRules)
	return rules, nil
}

// retryAfter 由窗口结束时间推算重试间隔；无窗口结束时间返回 0，下限 1s。
func retryAfter(to *time.Time, now time.Time) time.Duration {
	if to == nil {
		return 0
	}
	d := to.Sub(now)
	if d < time.Second {
		return time.Second
	}
	return d
}

// retryUntilNextLocalMidnight 返回距下一个业务时区零点的时长（下限 1s）。
func retryUntilNextLocalMidnight(now time.Time) time.Duration {
	loc := model.BusinessLocation()
	y, m, d := now.In(loc).Date()
	next := time.Date(y, m, d, 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	if d := next.Sub(now); d >= time.Second {
		return d
	}
	return time.Second
}

// RuleUsage 单条规则的用户用量视图（用户面配额展示）。
//
// 与 Status 的零值约定不同，这里是直接对外序列化的结构：Unit 一律给出明文
// （token / cost），不依赖调用方把空串理解为 token。cost 规则时 Used 为微元。
type RuleUsage struct {
	Index       int    `json:"rule_index"`
	Mode        string `json:"mode"`
	Limit       int    `json:"limit"`
	Unit        string `json:"unit"`
	Used        int64  `json:"used"`
	Active      bool   `json:"active"`
	PeriodStart *int64 `json:"period_start"`
	PeriodEnd   *int64 `json:"period_end"`
}

// UserRuleUsages 逐条计算规则的窗口与用量，用于用户面配额视图。
//
// 生效规则（Active=true）给出窗口边界（Unix 秒，PeriodEnd 为空表示无终止）与窗口内用量；
// 不生效规则（Active=false）Used=0，周期字段回填规则配置的锚点（custom 的 start/end，若有）。
//
// 用量口径按 rule.EffectiveUnit() 选择：token 规则累计 token 总量，
// cost 规则累计平台结算币种金额（微元）；两条口径互不混算。
func UserRuleUsages(
	ctx context.Context,
	store *model.Store,
	userID, groupID uint,
	rules []Rule,
	now time.Time,
) ([]RuleUsage, error) {
	out := make([]RuleUsage, 0, len(rules))
	for i, rule := range rules {
		unit := rule.EffectiveUnit()
		u := RuleUsage{Index: i, Mode: rule.Mode, Limit: rule.Limit, Unit: unit}
		from, to, active := rule.Window(now)
		if !active {
			u.PeriodStart = rule.Start
			if rule.End != nil && *rule.End > 0 {
				u.PeriodEnd = rule.End
			}
			out = append(out, u)
			continue
		}
		u.Active = true
		start := from.Unix()
		u.PeriodStart = &start
		if to != nil {
			end := to.Unix()
			u.PeriodEnd = &end
		}
		var (
			used int64
			err  error
		)
		if unit == UnitCost {
			used, err = store.SumUsageCostMicrosInWindow(
				ctx, model.UsageFilter{UserID: userID, GroupID: groupID}, from, to,
			)
			if err != nil {
				return nil, fmt.Errorf("sum user cost for rule %d: %w", i, err)
			}
		} else {
			used, err = store.SumUserTokensInWindow(ctx, userID, groupID, from, to)
			if err != nil {
				return nil, fmt.Errorf("sum user tokens for rule %d: %w", i, err)
			}
		}
		u.Used = used
		out = append(out, u)
	}
	return out, nil
}
