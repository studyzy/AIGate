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

// 本文件实现管理面注入的两个能力：策略目录（驱动前端表单）与路由预览。
//
// 它们放在 proxy 是因为策略是数据面的概念；管理面（internal/admin）只持有接口，
// 由 cmd/aigate 在装配时注入，避免管理面依赖数据面的内部结构。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"aigate/internal/dto"
)

// List 实现 admin.RouterStrategies：返回全部可用策略。
func (h *Handler) List() []dto.RouterStrategy {
	out := make([]dto.RouterStrategy, 0, 4)
	for _, d := range ListStrategies() {
		fields := make([]dto.RouterStrategyField, 0, len(d.Fields))
		for _, f := range d.Fields {
			fields = append(fields, dto.RouterStrategyField(f))
		}
		out = append(out, dto.RouterStrategy{
			Name:        d.Name,
			Title:       d.Title,
			Description: d.Description,
			Fields:      fields,
		})
	}
	return out
}

// ValidateConfig 实现 admin.RouterStrategies：委托给具体策略；策略不存在返回 ErrUnknownRoutingStrategy。
func (h *Handler) ValidateConfig(ctx context.Context, strategy, raw string) (json.RawMessage, error) {
	s, err := StrategyByName(strategy)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownRoutingStrategy, strategy)
	}
	return s.ValidateConfig(ctx, raw)
}

// ErrUnknownRoutingStrategy 表示策略名未注册（admin 侧据此返回 400）。
var ErrUnknownRoutingStrategy = errors.New("unknown routing strategy")

// Preview 实现 admin.RouterPreviewer：对指定规则跑一次策略判断，不发出转发请求。
//
// 决策策略在预览时会真实调用一次决策模型（否则预览结果与线上不一致）；
// 调用成本照常记账，与线上路径同口径。
func (h *Handler) Preview(
	ctx context.Context, routerID, keyID uint, reqBody map[string]any,
) (dto.RouterPreviewResult, error) {
	key, err := h.store.GetAPIKeyByID(ctx, keyID)
	if err != nil {
		return dto.RouterPreviewResult{}, fmt.Errorf("api key %d not found", keyID)
	}
	router, err := h.store.GetModelRouter(ctx, routerID)
	if err != nil {
		return dto.RouterPreviewResult{}, fmt.Errorf("router %d not found", routerID)
	}
	strategy, err := StrategyByName(router.Strategy)
	if err != nil {
		return dto.RouterPreviewResult{}, err
	}
	pool, err := h.store.ListKeyCandidates(ctx, key.ID)
	if err != nil {
		return dto.RouterPreviewResult{}, fmt.Errorf("list candidates: %w", err)
	}
	candidates := autoCandidates(pool)
	if len(candidates) == 0 {
		return dto.RouterPreviewResult{}, errors.New("this key has no routable candidates")
	}

	result, selErr := strategy.Select(ctx, jsonRaw(router.Config), RouteInput{
		Router:     router,
		Key:        key,
		Candidates: candidates,
		Request:    requestShape(reqBody),
		Now:        autoNow(),
	}, h.strategyDeps())

	out := dto.RouterPreviewResult{RouterID: router.ID, Strategy: router.Strategy}
	if selErr != nil {
		// 预览到兜底也是有用信息：告诉管理员「这段请求会回退到 primary」。
		primary, perr := h.store.ResolvePrimaryForKey(ctx, key)
		if perr != nil {
			return dto.RouterPreviewResult{}, fmt.Errorf("strategy failed (%v) and no primary model is available", selErr)
		}
		out.Fallback = true
		out.Cause = string(causeFallbackPrimary)
		out.Detail = map[string]any{"reason": selErr.Error()}
		out.AIModelID = primary.AIModelID
		out.ModelID = primary.ModelID
		out.ModelName = primary.ModelName
		return out, nil
	}

	resolved, err := h.store.ResolveAIModelByID(ctx, result.AIModelID)
	if err != nil {
		return dto.RouterPreviewResult{}, fmt.Errorf("resolve routed model: %w", err)
	}
	out.AIModelID = resolved.AIModelID
	out.ModelID = resolved.ModelID
	out.ModelName = resolved.ModelName
	out.Cause = result.Cause
	out.Detail = result.Detail
	return out, nil
}
