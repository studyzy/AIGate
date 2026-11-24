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

// 本文件是模型路由的装配层：把「匹配 router → 跑策略 → 兜底」串起来，
// 并实现策略可用的外部能力（决策模型调用、按 router 隔离的熔断器、记账）。
//
// 不变量：除「没有生效 router / 没有候选」这两类配置型错误外，任何失败都回退到
// 密钥 primary，请求照常继续。路由不能把一条正常请求变成错误。

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"aigate/internal/model"
	"aigate/internal/provider"
)

// autoModelName 是触发路由的伪模型名（大小写不敏感）。
const autoModelName = "auto"

// autoRoutedHeader 在路由生效时携带实际使用的上游模型名。
const autoRoutedHeader = "X-AIGate-Routed-Model"

// routerIDHeader 携带命中的路由规则 ID，便于调用方与运维定位。
const routerIDHeader = "X-AIGate-Router-Id"

// autoCause 是一次路由的判定原因，用于日志与排障。
type autoCause string

const (
	causeSingleCandidate autoCause = "single_candidate"
	causeShortCircuit    autoCause = "short_circuit"
	causeLLMClassifier   autoCause = "llm_classifier"
	causeFallbackPrimary autoCause = "fallback_primary"
)

// SetAutoPriceSource 装配路由取价来源（与记账共用同一个 billing.StoreSource）。
func (h *Handler) SetAutoPriceSource(src PriceSource) {
	h.autoPrices = src
}

// autoRouterAvailable 报告 auto 伪模型当前是否可用（存在对任意密钥生效的启用 router）。
//
// 这里只做一次廉价的全局判断（是否有任何启用的 router），供 /v1/models 使用；
// 精确到某个密钥的命中在请求路径上由 MatchModelRouter 决定。
func (h *Handler) autoRouterAvailable(ctx context.Context) bool {
	routers, err := h.store.ListModelRouters(ctx)
	if err != nil {
		slog.Error("list model routers for availability failed", "error", err)
		return false
	}
	for i := range routers {
		if routers[i].Enabled {
			return true
		}
	}
	return false
}

// autoRoutedModelID 返回本次请求由路由选定的模型 ID（0 表示未发生路由）。
type autoRoutedKey struct{}

// withAutoRouted 把路由结果写进请求上下文，供响应头使用。
func withAutoRouted(ctx context.Context, modelID uint) context.Context {
	return context.WithValue(ctx, autoRoutedKey{}, modelID)
}

// autoRoutedFrom 读取上下文里的路由结果（0 表示未发生）。
func autoRoutedFrom(ctx context.Context) uint {
	if v, ok := ctx.Value(autoRoutedKey{}).(uint); ok {
		return v
	}
	return 0
}

// keyBindsRealAuto 报告该密钥是否真的绑定了一个 model_id 为 auto 的模型。
//
// 伪模型与真实模型同名时，显式配置优先：用户把某个上游模型命名为 auto 是明确的意图，
// 不能被网关的内置路由悄悄顶掉。/v1/models 与请求解析两处共用这一个判断。
func (h *Handler) keyBindsRealAuto(ctx context.Context, keyID uint) bool {
	infos, err := h.store.ListKeyModels(ctx, keyID, true)
	if err != nil {
		slog.Error("list key models for auto shadowing check failed", "error", err, "key_id", keyID)
		// 出错时按「存在真实绑定」处理：此刻宁可禁用伪模型，也不误顶掉真实模型。
		return true
	}
	return hasRealAutoBinding(infos)
}

// hasRealAutoBinding 报告绑定列表中是否存在 model_id 为 auto 的模型。
func hasRealAutoBinding(infos []model.BindingInfo) bool {
	for _, m := range infos {
		if isAutoModelName(m.ModelID) {
			return true
		}
	}
	return false
}

// strategyDeps 组装策略可用的外部能力。
func (h *Handler) strategyDeps() StrategyDeps {
	return StrategyDeps{
		Prices:  h.autoPrices,
		Decide:  h,
		Breaker: h,
	}
}

// resolveAutoRoute 在 model="auto" 时挑出真正的目标模型。
//
// 返回：
//   - target 非 nil：转发到该模型；
//   - status 非空：配置型错误，调用方应据此写出错误响应；
//   - status 为 0 且 target 为 nil：回退 primary。
func (h *Handler) resolveAutoRoute(
	ctx context.Context,
	key *model.APIKey,
	reqBody map[string]any,
) (target *model.Resolved, router *model.ModelRouter, cause autoCause, status int, message string) {
	router, err := h.store.MatchModelRouter(ctx, key)
	if err != nil {
		slog.Error("match model router failed", "error", err, "key_id", key.ID)
		return nil, nil, causeFallbackPrimary, 0, ""
	}
	if router == nil {
		return nil, nil, causeFallbackPrimary, 400, msgAutoUnavailable
	}

	strategy, err := StrategyByName(router.Strategy)
	if err != nil {
		slog.Error("router references an unknown strategy",
			"error", err, "router_id", router.ID, "strategy", router.Strategy)
		return nil, router, causeFallbackPrimary, 0, ""
	}

	pool, err := h.store.ListKeyCandidates(ctx, key.ID)
	if err != nil {
		slog.Error("list router candidates failed", "error", err, "key_id", key.ID, "router_id", router.ID)
		return nil, router, causeFallbackPrimary, 0, ""
	}
	candidates := autoCandidates(pool)
	if len(candidates) == 0 {
		return nil, router, causeFallbackPrimary, 500, msgNoResolvableModel
	}

	result, err := strategy.Select(ctx, jsonRaw(router.Config), RouteInput{
		Router:     router,
		Key:        key,
		Candidates: candidates,
		Request:    requestShape(reqBody),
		Now:        autoNow(),
	}, h.strategyDeps())
	if err != nil {
		slog.Warn("routing strategy could not decide, falling back",
			"error", err, "key_id", key.ID, "router_id", router.ID, "strategy", router.Strategy)
		return nil, router, causeFallbackPrimary, 0, ""
	}

	res, rerr := h.store.ResolveAIModelByID(ctx, result.AIModelID)
	if rerr != nil {
		slog.Error("resolve routed candidate failed", "error", rerr,
			"key_id", key.ID, "ai_model_id", result.AIModelID)
		return nil, router, causeFallbackPrimary, 0, ""
	}
	return res, router, autoCause(result.Cause), 0, ""
}

// jsonRaw 把落库的配置字符串转成 json.RawMessage（空串按空对象处理）。
func jsonRaw(s string) []byte {
	if s == "" {
		return []byte("{}")
	}
	return []byte(s)
}

// ---------------------------------------------------------------------------
// DecisionCaller 实现：调用决策模型
// ---------------------------------------------------------------------------

// Decide 实现 DecisionCaller：按 router 的配置调用决策模型并把选项作为 choice 题下发。
//
// 决策调用是一次真实的上游消耗：成本照记，但不计入用户配额（见 logDecisionCall）。
func (h *Handler) Decide(
	ctx context.Context,
	routerID, decisionModelID uint,
	state string,
	options []DecisionOption,
	timeout time.Duration,
) (string, error) {
	res, err := h.store.ResolveAIModelByID(ctx, decisionModelID)
	if err != nil {
		return "", err
	}
	decider, err := provider.GetDecisionProvider(res.ModelType)
	if err != nil {
		return "", err
	}

	choices := make([]provider.DecisionOption, 0, len(options))
	for _, o := range options {
		choices = append(choices, provider.DecisionOption{Value: o.Value, Description: o.Description})
	}
	req := &provider.DecisionRequest{
		Model: res.ModelID,
		State: marshalAutoState(state),
		Questions: []provider.DecisionQuestion{{
			Name:         autoModelName,
			Type:         provider.DecisionPick,
			Instructions: autoInstructions,
			Choices:      choices,
		}},
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()

	// 自定义头传 nil：extractCustomHeaders 的契约是「把入站请求确实带着的头回传给上游」，
	// 用于多租户隔离；路由的决策调用没有对应的入站请求，硬塞配置值会绕过该契约。
	result, derr := decider.Decide(cctx, res.APIKey, res.URL, res.ModelID, req, nil)
	if derr != nil || result == nil || result.StatusCode != 200 {
		status := 0
		if result != nil {
			status = result.StatusCode
		}
		h.logDecisionCall(res, nil, status, start, routerID)
		if derr != nil {
			return "", derr
		}
		return "", errDecisionFailed
	}

	h.logDecisionCall(res, usageOf(result.Resp), result.StatusCode, start, routerID)
	choice := firstChoice(result.Resp)
	if choice == "" {
		return "", errDecisionFailed
	}
	return choice, nil
}

// logDecisionCall 记一笔路由决策调用：记账但不占用户配额。
func (h *Handler) logDecisionCall(
	deciderModel *model.Resolved,
	usage *provider.Usage,
	statusCode int,
	start time.Time,
	routerID uint,
) {
	if h.usage == nil {
		return
	}
	entry := model.LLMUsageLog{
		AIModelID:  deciderModel.AIModelID,
		Model:      deciderModel.ModelID,
		Provider:   deciderModel.Provider,
		StatusCode: statusCode,
		Latency:    int(time.Since(start).Milliseconds()),
		CreatedAt:  time.Now().UTC(),
		RouterID:   routerID,
	}
	if usage != nil {
		entry.PromptTokens = usage.PromptTokens
		entry.CompletionTokens = usage.CompletionTokens
		entry.TotalTokens = usage.TotalTokens
		entry.PromptCacheReadTokens = usage.PromptCacheReadTokens
		entry.PromptCacheWriteTokens = usage.PromptCacheWriteTokens
		entry.ReportedCost = usage.ReportedCost
	}
	// 与 /v1/decisions 入口同口径：input_only 模型的输出通常免费。
	if deciderModel.BillingMode == model.BillingModeInputOnly {
		entry.CompletionTokens = 0
		entry.TotalTokens = entry.PromptTokens
	}
	// 网关内部开销：记账但不占用户配额。
	entry.SetCountTowardQuota(false)
	h.usage.Submit(entry)
}

// usageOf 取出决策响应里的用量（可能为 nil）。
func usageOf(resp *provider.DecisionResponse) *provider.Usage {
	if resp == nil {
		return nil
	}
	return resp.Usage
}

// firstChoice 取出第一个 choice 答案的值（决策协议可能返回多题，路由只有一个）。
func firstChoice(resp *provider.DecisionResponse) string {
	if resp == nil {
		return ""
	}
	for _, a := range resp.Answers {
		if a.Choice != "" {
			return a.Choice
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// BreakerSource 实现：按 router 隔离的进程内熔断器
// ---------------------------------------------------------------------------

// routerBreaker 是单个 router 的熔断状态。
type routerBreaker struct {
	openTill time.Time
}

// breakerStates 是按 router ID 索引的熔断表（由 Handler 持有）。
type breakerStates struct {
	mu     sync.Mutex
	states map[uint]*routerBreaker
}

// Allow 实现 BreakerSource。
func (b *breakerStates) Allow(routerID uint) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.states[routerID]
	return !ok || time.Now().After(st.openTill)
}

// Fail 实现 BreakerSource。
func (b *breakerStates) Fail(routerID uint, cooldown time.Duration) {
	if cooldown <= 0 {
		cooldown = time.Duration(model.DefaultAutoRouterCooldownSeconds) * time.Second
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.states == nil {
		b.states = make(map[uint]*routerBreaker)
	}
	b.states[routerID] = &routerBreaker{openTill: time.Now().Add(cooldown)}
}

// Succeed 实现 BreakerSource。
func (b *breakerStates) Succeed(routerID uint) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.states != nil {
		delete(b.states, routerID)
	}
}

// Allow 实现 BreakerSource（Handler 委托给按 router 的熔断表）。
func (h *Handler) Allow(routerID uint) bool { return h.routerBreakers.Allow(routerID) }

// Fail 实现 BreakerSource。
func (h *Handler) Fail(routerID uint, cooldown time.Duration) {
	h.routerBreakers.Fail(routerID, cooldown)
}

// Succeed 实现 BreakerSource。
func (h *Handler) Succeed(routerID uint) { h.routerBreakers.Succeed(routerID) }

// errDecisionFailed 表示决策调用未给出可用结论。
var errDecisionFailed = errDecisionFailedErr{}

type errDecisionFailedErr struct{}

func (errDecisionFailedErr) Error() string { return "decision model gave no usable answer" }
