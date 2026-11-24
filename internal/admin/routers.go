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

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"aigate/internal/dto"
	"aigate/internal/httpx"
	"aigate/internal/model"
)

// 管理面的模型路由 API：路由规则本身（model_routers）的增删改查，外加两个只读辅助接口。
//
// 策略是数据面的概念（internal/proxy），本包不 import 它——那样管理面就会依赖数据面的
// 内部结构。策略能力由装配方（cmd/aigate）经 SetRouterStrategies / SetRouterPreviewer
// 注入；未注入时，策略相关端点返回 503，其余 CRUD 照常可用（配置仍是合法 JSON）。
//
// 校验分工：
//   - 本包负责「结构性」校验：作用域规范化、优先级唯一、策略名存在、配置是合法 JSON；
//   - 策略的 ValidateConfig 负责「语义」校验（字段类型、必填项、取值范围）。
// 两者都过才落库。

// RouterStrategies 是可注入的策略能力集合。
type RouterStrategies interface {
	// List 返回全部可用策略（按名字排序）。
	List() []dto.RouterStrategy
	// ValidateConfig 校验并规范化配置；策略不存在时返回 ErrUnknownStrategy。
	ValidateConfig(ctx context.Context, strategy, raw string) (json.RawMessage, error)
}

// ErrUnknownStrategy 表示策略名未注册。
var ErrUnknownStrategy = errors.New("unknown routing strategy")

// RouterPreviewer 让管理面在不发真实请求的前提下跑一次路由判断。
type RouterPreviewer interface {
	// Preview 选出一条规则并执行其策略；返回选定的模型与该策略给出的原因。
	Preview(ctx context.Context, routerID uint, keyID uint, reqBody map[string]any) (dto.RouterPreviewResult, error)
}

// PreviewResult 是一次路由预览的结果。
type PreviewResult struct {
	RouterID   uint           `json:"router_id"`
	Strategy   string         `json:"strategy"`
	AIModelID  uint           `json:"ai_model_id"`
	ModelID    string         `json:"model_id"`
	ModelName  string         `json:"model_name,omitempty"`
	Cause      string         `json:"cause"`
	Detail     map[string]any `json:"detail,omitempty"`
	Fallback   bool           `json:"fallback"`              // true 表示策略未能决定，回退到了 primary
	FallbackOf []string       `json:"fallback_of,omitempty"` // 参与比较的候选（便于排障）
}

var (
	routerStrategies RouterStrategies
	routerPreviewer  RouterPreviewer
)

// SetRouterStrategies 装配策略能力（由 cmd/aigate 在 Register 之前调用）。
func SetRouterStrategies(s RouterStrategies) { routerStrategies = s }

// SetRouterPreviewer 装配路由预览能力（由 cmd/aigate 在 Register 之前调用）。
func SetRouterPreviewer(p RouterPreviewer) { routerPreviewer = p }

// routerResponse 是路由规则的对外表示。
type routerResponse struct {
	ID          uint   `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Strategy    string `json:"strategy"`
	Config      string `json:"config"`
	Scope       string `json:"scope"`
	ScopeID     uint   `json:"scope_id"`
	Priority    int    `json:"priority"`
	Enabled     bool   `json:"enabled"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func toRouterResponse(r *model.ModelRouter) routerResponse {
	return routerResponse{
		ID:          r.ID,
		Name:        r.Name,
		Description: r.Description,
		Strategy:    r.Strategy,
		Config:      r.Config,
		Scope:       r.Scope,
		ScopeID:     r.ScopeID,
		Priority:    r.Priority,
		Enabled:     r.Enabled,
		CreatedAt:   r.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   r.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// routerCreateRequest 是新建路由的请求体。
type routerCreateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Strategy    string `json:"strategy"`
	Config      string `json:"config"`
	Scope       string `json:"scope"`
	ScopeID     uint   `json:"scope_id"`
	Priority    int    `json:"priority"`
	// Enabled 缺省为 true（新建默认启用）。
	Enabled *bool `json:"enabled"`
}

// routerUpdateRequest 是更新路由的请求体；nil 字段表示不变。
type routerUpdateRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Strategy    *string `json:"strategy"`
	Config      *string `json:"config"`
	Scope       *string `json:"scope"`
	ScopeID     *uint   `json:"scope_id"`
	Priority    *int    `json:"priority"`
	Enabled     *bool   `json:"enabled"`
}

// handleListRouterStrategies 实现 GET /admin/model-routers/strategies：
// 返回可用策略及其配置字段，供前端渲染表单。
func handleListRouterStrategies() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if routerStrategies == nil {
			httpx.WriteError(w, http.StatusServiceUnavailable, "routing strategies are not available")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"strategies": routerStrategies.List()})
	}
}

// handleListModelRouters 实现 GET /admin/model-routers。
func handleListModelRouters(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := store.ListModelRouters(r.Context())
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "failed to list routers")
			return
		}
		out := make([]routerResponse, 0, len(rows))
		for i := range rows {
			out = append(out, toRouterResponse(&rows[i]))
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"routers": out})
	}
}

// handleGetModelRouter 实现 GET /admin/model-routers/{id}。
func handleGetModelRouter(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseRouterID(w, r)
		if !ok {
			return
		}
		row, err := store.GetModelRouter(r.Context(), id)
		if err != nil {
			writeRouterError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, toRouterResponse(row))
	}
}

// handleCreateModelRouter 实现 POST /admin/model-routers。
func handleCreateModelRouter(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req routerCreateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			writeDecodeError(w, err)
			return
		}
		router := &model.ModelRouter{
			Name:        strings.TrimSpace(req.Name),
			Description: strings.TrimSpace(req.Description),
			Strategy:    strings.TrimSpace(req.Strategy),
			Config:      req.Config,
			Scope:       req.Scope,
			ScopeID:     req.ScopeID,
			Priority:    req.Priority,
			Enabled:     true,
		}
		if req.Enabled != nil {
			router.Enabled = *req.Enabled
		}
		if !validateRouterFields(w, r, router) {
			return
		}
		if err := store.CreateModelRouter(r.Context(), router); err != nil {
			writeRouterError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusCreated, toRouterResponse(router))
	}
}

// handleUpdateModelRouter 实现 PUT /admin/model-routers/{id}。
func handleUpdateModelRouter(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseRouterID(w, r)
		if !ok {
			return
		}
		var req routerUpdateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			writeDecodeError(w, err)
			return
		}
		// 先读出既有行：策略校验需要知道最终生效的策略名与配置。
		current, err := store.GetModelRouter(r.Context(), id)
		if err != nil {
			writeRouterError(w, err)
			return
		}
		merged := *current
		fields := map[string]any{}
		if req.Name != nil {
			merged.Name = strings.TrimSpace(*req.Name)
			fields["name"] = merged.Name
		}
		if req.Description != nil {
			merged.Description = strings.TrimSpace(*req.Description)
			fields["description"] = merged.Description
		}
		if req.Strategy != nil {
			merged.Strategy = strings.TrimSpace(*req.Strategy)
			fields["strategy"] = merged.Strategy
		}
		if req.Config != nil {
			merged.Config = *req.Config
			fields["config"] = merged.Config
		}
		if req.Scope != nil {
			merged.Scope = *req.Scope
			fields["scope"] = merged.Scope
		}
		if req.ScopeID != nil {
			merged.ScopeID = *req.ScopeID
			fields["scope_id"] = merged.ScopeID
		}
		if req.Priority != nil {
			merged.Priority = *req.Priority
			fields["priority"] = merged.Priority
		}
		if req.Enabled != nil {
			merged.Enabled = *req.Enabled
			fields["enabled"] = merged.Enabled
		}
		if !validateRouterFields(w, r, &merged) {
			return
		}
		// 策略可能规范化了配置：把校验后的值写回。
		fields["config"] = merged.Config
		updated, err := store.UpdateModelRouter(r.Context(), id, fields)
		if err != nil {
			writeRouterError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, toRouterResponse(updated))
	}
}

// handleDeleteModelRouter 实现 DELETE /admin/model-routers/{id}。
func handleDeleteModelRouter(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseRouterID(w, r)
		if !ok {
			return
		}
		if err := store.DeleteModelRouter(r.Context(), id); err != nil {
			writeRouterError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handlePreviewModelRouter 实现 POST /admin/model-routers/{id}/preview：
// 用一条规则对一个真实密钥与一段请求体跑一次路由，不发出上游请求。
func handlePreviewModelRouter() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if routerPreviewer == nil {
			httpx.WriteError(w, http.StatusServiceUnavailable, "router preview is not available")
			return
		}
		id, ok := parseRouterID(w, r)
		if !ok {
			return
		}
		var req struct {
			APIKeyID uint           `json:"api_key_id"`
			Request  map[string]any `json:"request"`
		}
		if err := httpx.DecodeJSON(r, &req); err != nil {
			writeDecodeError(w, err)
			return
		}
		if req.APIKeyID == 0 {
			httpx.WriteError(w, http.StatusBadRequest, "api_key_id is required")
			return
		}
		if req.Request == nil {
			req.Request = map[string]any{}
		}
		result, err := routerPreviewer.Preview(r.Context(), id, req.APIKeyID, req.Request)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		httpx.WriteJSON(w, http.StatusOK, result)
	}
}

// validateRouterFields 做结构性校验并调用策略的语义校验；失败时已写出 400。
func validateRouterFields(w http.ResponseWriter, r *http.Request, router *model.ModelRouter) bool {
	if router.Name == "" {
		httpx.WriteError(w, http.StatusBadRequest, "name is required")
		return false
	}
	if router.Strategy == "" {
		httpx.WriteError(w, http.StatusBadRequest, "strategy is required")
		return false
	}
	scope, scopeID, err := model.NormalizeRouterScope(router.Scope, router.ScopeID)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return false
	}
	router.Scope, router.ScopeID = scope, scopeID

	if routerStrategies == nil {
		// 未装配策略能力（仅测试环境可能）：只校验 JSON 合法。
		if router.Config != "" && !json.Valid([]byte(router.Config)) {
			httpx.WriteError(w, http.StatusBadRequest, "config must be valid json")
			return false
		}
		return true
	}
	normalized, err := routerStrategies.ValidateConfig(r.Context(), router.Strategy, router.Config)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, ErrUnknownStrategy) {
			httpx.WriteError(w, http.StatusBadRequest, msg)
			return false
		}
		httpx.WriteError(w, http.StatusBadRequest, msg)
		return false
	}
	router.Config = string(normalized)
	return true
}

// writeRouterError 把 model 层错误映射成 HTTP 响应。
func writeRouterError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, model.ErrRouterNotFound):
		httpx.WriteError(w, http.StatusNotFound, "router not found")
	case errors.Is(err, model.ErrRouterPriorityTaken):
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
	default:
		httpx.WriteError(w, http.StatusInternalServerError, "routing request failed")
	}
}

// parseRouterID 解析路径里的路由 ID；非法时写出 400。
func parseRouterID(w http.ResponseWriter, r *http.Request) (uint, bool) {
	id, ok := httpx.ParseUintID(r, "id")
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, "invalid router id")
		return 0, false
	}
	return id, true
}

// writeDecodeError 把请求体解码错误映射成 400/413。
func writeDecodeError(w http.ResponseWriter, err error) {
	if errors.Is(err, httpx.ErrBodyTooLarge) {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	httpx.WriteError(w, http.StatusBadRequest, "invalid json body")
}
