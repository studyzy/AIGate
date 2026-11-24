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

package proxy

// 本文件实现第一个路由策略：decision —— 用决策模型在候选里挑最便宜且能胜任的模型。
//
// 行为与先前站点级 auto 伪模型完全一致（短路、题面、兜底语义逐条保留），
// 差别只在于配置来源：从 router.config 读，而不是 settings.auto_router_* 列。
// 这样同一套策略可以被多条 router（不同作用域/优先级）复用。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"aigate/internal/billing"
	"aigate/internal/model"
)

// strategyDecision 是决策选型策略的名字。
const strategyDecision = "decision"

func init() { registerStrategy(decisionStrategy{}) }

// decisionConfig 是 decision 策略的配置（落库为 JSON）。
type decisionConfig struct {
	DecisionModelID     uint `json:"decision_model_id"`
	MaxCandidates       int  `json:"max_candidates"`
	MaxShortTokens      int  `json:"max_short_tokens"`
	MaxContextTokens    int  `json:"max_context_tokens"`
	MaxChars            int  `json:"max_chars"`
	TimeoutMs           int  `json:"timeout_ms"`
	CooldownSeconds     int  `json:"cooldown_seconds"`
	ShortCircuitEnabled bool `json:"short_circuit_enabled"`
}

// decisionDefaults 与配置字段的默认值保持一致。
var decisionDefaults = decisionConfig{
	MaxCandidates:       model.DefaultAutoRouterMaxCandidates,
	MaxShortTokens:      model.DefaultAutoRouterMaxShortTokens,
	MaxContextTokens:    model.DefaultAutoRouterMaxContextTokens,
	MaxChars:            model.DefaultAutoRouterMaxChars,
	TimeoutMs:           model.DefaultAutoRouterTimeoutMs,
	CooldownSeconds:     model.DefaultAutoRouterCooldownSeconds,
	ShortCircuitEnabled: true,
}

// parseDecisionConfig 解析配置并对缺省字段补默认值（落库前已校验过，这里只做容错读取）。
func parseDecisionConfig(raw json.RawMessage) decisionConfig {
	cfg := decisionDefaults
	if len(raw) == 0 {
		return cfg
	}
	_ = json.Unmarshal(raw, &cfg)
	if cfg.MaxCandidates <= 0 {
		cfg.MaxCandidates = decisionDefaults.MaxCandidates
	}
	if cfg.MaxShortTokens <= 0 {
		cfg.MaxShortTokens = decisionDefaults.MaxShortTokens
	}
	if cfg.MaxContextTokens <= 0 {
		cfg.MaxContextTokens = decisionDefaults.MaxContextTokens
	}
	if cfg.MaxChars <= 0 {
		cfg.MaxChars = decisionDefaults.MaxChars
	}
	if cfg.TimeoutMs <= 0 {
		cfg.TimeoutMs = decisionDefaults.TimeoutMs
	}
	if cfg.CooldownSeconds <= 0 {
		cfg.CooldownSeconds = decisionDefaults.CooldownSeconds
	}
	return cfg
}

// decisionStrategy 是 Strategy 的 decision 实现。
type decisionStrategy struct{}

// Name 实现 Strategy。
func (decisionStrategy) Name() string { return strategyDecision }

// Describe 实现 Strategy：字段表驱动管理面与 WebUI 表单。
func (decisionStrategy) Describe() StrategyDescriptor {
	min1 := 1
	return StrategyDescriptor{
		Name:        strategyDecision,
		Title:       "Decision model routing",
		Description: "Asks a decision model which candidate is the cheapest that can still handle the request.",
		Fields: []StrategyConfigField{
			{Key: "decision_model_id", Type: "model", Title: "Decision model",
				Description: "An ai_models row whose model_type is a decision protocol.", Required: true},
			{Key: "max_candidates", Type: "int", Title: "Max candidates",
				Description: "Candidates beyond this are truncated by ascending price.", Default: decisionDefaults.MaxCandidates, Min: &min1},
			{Key: "short_circuit_enabled", Type: "bool", Title: "Short-circuit simple requests",
				Description: "Skip the decision call for short single-turn requests that hit a simple-task signal.",
				Default:     decisionDefaults.ShortCircuitEnabled},
			{Key: "max_short_tokens", Type: "int", Title: "Short-circuit token limit",
				Description: "Longest estimated prompt that may be short-circuited.", Default: decisionDefaults.MaxShortTokens, Min: &min1},
			{Key: "max_context_tokens", Type: "int", Title: "Short-circuit context limit",
				Description: "Conversations longer than this always ask the decision model.", Default: decisionDefaults.MaxContextTokens, Min: &min1},
			{Key: "max_chars", Type: "int", Title: "Prompt budget (chars)",
				Description: "Longest conversation text sent to the decision model.", Default: decisionDefaults.MaxChars, Min: &min1},
			{Key: "timeout_ms", Type: "int", Title: "Decision timeout (ms)",
				Description: "Hard deadline for one decision call; on expiry the request falls back to the key's primary model.",
				Default:     decisionDefaults.TimeoutMs, Min: &min1},
			{Key: "cooldown_seconds", Type: "int", Title: "Circuit cooldown (s)",
				Description: "After a failed decision call, routing falls back without calling it for this long.",
				Default:     decisionDefaults.CooldownSeconds, Min: &min1},
		},
	}
}

// ValidateConfig 实现 Strategy：校验字段并写出规范化后的 JSON（补齐默认值）。
func (decisionStrategy) ValidateConfig(ctx context.Context, raw string) (json.RawMessage, error) {
	var in map[string]any
	if raw == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	// 从默认值出发再反序列化：bool 字段无法用「== 0」判断是否缺省，
	// 从零值开始会让 short_circuit_enabled 的声明默认值（true）被静默改成 false。
	cfg := decisionDefaults
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	if cfg.DecisionModelID == 0 {
		return nil, errors.New("invalid config: decision_model_id is required")
	}
	// 未知字段一律拒绝：拼错的键静默失效比报错更难查。
	known := map[string]bool{
		"decision_model_id": true, "max_candidates": true, "max_short_tokens": true,
		"max_context_tokens": true, "max_chars": true, "timeout_ms": true,
		"cooldown_seconds": true, "short_circuit_enabled": true,
	}
	for k := range in {
		if !known[k] {
			return nil, fmt.Errorf("invalid config: unknown field %q", k)
		}
	}
	for _, p := range []struct {
		name string
		v    int
	}{
		{"max_candidates", cfg.MaxCandidates}, {"max_short_tokens", cfg.MaxShortTokens},
		{"max_context_tokens", cfg.MaxContextTokens}, {"max_chars", cfg.MaxChars},
		{"timeout_ms", cfg.TimeoutMs}, {"cooldown_seconds", cfg.CooldownSeconds},
	} {
		if p.v < 0 {
			return nil, fmt.Errorf("invalid config: %s must not be negative", p.name)
		}
	}
	// 补齐默认值，使落库配置自解释（读取侧仍做容错）。
	if cfg.MaxCandidates == 0 {
		cfg.MaxCandidates = decisionDefaults.MaxCandidates
	}
	if cfg.MaxShortTokens == 0 {
		cfg.MaxShortTokens = decisionDefaults.MaxShortTokens
	}
	if cfg.MaxContextTokens == 0 {
		cfg.MaxContextTokens = decisionDefaults.MaxContextTokens
	}
	if cfg.MaxChars == 0 {
		cfg.MaxChars = decisionDefaults.MaxChars
	}
	if cfg.TimeoutMs == 0 {
		cfg.TimeoutMs = decisionDefaults.TimeoutMs
	}
	if cfg.CooldownSeconds == 0 {
		cfg.CooldownSeconds = decisionDefaults.CooldownSeconds
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return b, nil
}

// pricedCandidate 是带比价标量的候选，仅用于排序。
type pricedCandidate struct {
	candidate model.Candidate
	// micros 是比价标量；ok=false 表示无价目或无法折算，排在已定价候选之后。
	micros int64
	ok     bool
}

// Select 实现 Strategy：短路 → 决策模型 → 结论。
//
// 返回 error 表示本策略无法给出结论（未配置决策模型、决策失败、未知选择等），
// 调用方统一回退 primary。
func (decisionStrategy) Select(ctx context.Context, cfg json.RawMessage, in RouteInput, deps StrategyDeps) (RouteResult, error) {
	c := parseDecisionConfig(cfg)

	// 候选只有一个：直接用它，不为一次没有选择的决定付决策费。
	if len(in.Candidates) == 1 {
		return RouteResult{AIModelID: in.Candidates[0].AIModelID, Cause: string(causeSingleCandidate)}, nil
	}

	priced, err := priceCandidates(ctx, in.Candidates, in.Now, deps)
	if err != nil {
		return RouteResult{}, err
	}
	if limit := c.MaxCandidates; limit > 0 && len(priced) > limit {
		priced = priced[:limit]
	}

	if c.ShortCircuitEnabled && shouldShortCircuit(c, in.Request) {
		chosen := pickCheapest(priced)
		return RouteResult{AIModelID: chosen.AIModelID, Cause: string(causeShortCircuit),
			Detail: map[string]any{"reason": "short_circuit"}}, nil
	}

	choice, err := askDecisionModel(ctx, c, in, priced, deps)
	if err != nil {
		return RouteResult{}, err
	}
	options := buildChoices(priced)
	chosen, found := resolveAutoChoice(priced, options, choice)
	if !found {
		return RouteResult{}, fmt.Errorf("decision model returned unknown choice %q", choice)
	}
	return RouteResult{AIModelID: chosen.AIModelID, Cause: string(causeLLMClassifier)}, nil
}

// askDecisionModel 询问决策模型：熔断检查 → 题面构造 → 调用 → 熔断记账。
//
// 决策模型本身是候选池外的站点级配置：它不参与路由候选，但它的调用是一次真实上游消耗，
// 由 deps.Decide（Handler）负责记账。
func askDecisionModel(
	ctx context.Context,
	cfg decisionConfig,
	in RouteInput,
	priced []pricedCandidate,
	deps StrategyDeps,
) (string, error) {
	routerID := in.Router.ID
	if deps.Breaker != nil && !deps.Breaker.Allow(routerID) {
		return "", errors.New("router circuit is open")
	}
	if deps.Decide == nil {
		return "", errors.New("decision caller is not wired")
	}

	options := buildChoices(priced)
	domOpts := make([]DecisionOption, 0, len(options))
	for _, o := range options {
		domOpts = append(domOpts, DecisionOption{Value: o.Value, Description: o.Description})
	}

	state := in.Request.SystemPrompt
	if in.Request.LastUser != "" {
		if state != "" {
			state += "\n\n"
		}
		state += in.Request.LastUser
	}
	if maxChars := cfg.MaxChars; maxChars > 0 && len([]rune(state)) > maxChars {
		state = string([]rune(state)[:maxChars])
	}

	cooldown := time.Duration(cfg.CooldownSeconds) * time.Second
	choice, err := deps.Decide.Decide(ctx, routerID, cfg.DecisionModelID, state,
		domOpts, time.Duration(cfg.TimeoutMs)*time.Millisecond)
	if err != nil {
		if deps.Breaker != nil {
			deps.Breaker.Fail(routerID, cooldown)
		}
		return "", err
	}
	if deps.Breaker != nil {
		deps.Breaker.Succeed(routerID)
	}
	return choice, nil
}

// priceCandidates 给每个候选算比价标量，并按「已定价在前、价格升序」排序。
//
// 取价失败不阻断路由：该候选标为未知价格，排在已定价候选之后。
func priceCandidates(ctx context.Context, candidates []model.Candidate, now time.Time, deps StrategyDeps) ([]pricedCandidate, error) {
	out := make([]pricedCandidate, 0, len(candidates))
	for _, c := range candidates {
		pc := pricedCandidate{candidate: c}
		if deps.Prices != nil {
			prices, err := deps.Prices.Prices(ctx, c.AIModelID)
			if err != nil {
				slog.Warn("load router candidate price failed", "error", err, "ai_model_id", c.AIModelID)
			} else if micros, ok := billing.ComparablePrice(prices, now, false); ok {
				pc.micros, pc.ok = micros, true
			}
		}
		out = append(out, pc)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ok != out[j].ok {
			return out[i].ok
		}
		if !out[i].ok {
			return false
		}
		return out[i].micros < out[j].micros
	})
	return out, nil
}

// shouldShortCircuit 判定是否可以跳过决策模型、直接选候选池里最便宜的一个。
//
// 六个条件全满足才短路（design §7.3）。第 6 条要求「至少命中一个简单信号」，
// 这是刻意保守：litellm 明确警示过，什么都不命中的 prompt 会被「默认」判为最简档
// 而非「有证据地」判为最简档，约一半通用流量属此类——那正是要靠决策模型兜住的一半。
func shouldShortCircuit(cfg decisionConfig, req RequestShape) bool {
	if req.LastUser == "" || req.MessageCount != 1 {
		return false
	}
	if req.SystemPrompt != "" {
		// 有角色设定时任务通常不是「翻译一句话」那么明摆着简单。
		return false
	}
	if req.HasTools || req.HasComplexShape {
		return false
	}
	if estimateTokens(req.LastUser) > cfg.MaxShortTokens {
		return false
	}
	if req.TotalChars > cfg.MaxContextTokens {
		return false
	}
	return hasSimpleSignal(req.LastUser)
}
