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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"aigate/internal/billing"
	"aigate/internal/httpx"
	"aigate/internal/model"
)

// modelResponse 是模型的对外表示：绝不包含 api_key。
type modelResponse struct {
	ID                uint              `json:"id"`
	Provider          string            `json:"provider"`
	ModelID           string            `json:"model_id"`
	ModelName         string            `json:"model_name"`
	URL               string            `json:"url"`
	CredentialID      uint              `json:"credential_id"`
	ModelType         string            `json:"model_type"`
	BillingMode       string            `json:"billing_mode"`
	CustomHTTPHeaders map[string]string `json:"custom_http_headers"`
	QuotaDay          int               `json:"quota_day"`
	Enabled           bool              `json:"enabled"`
	// UserKeyRequired 用户自备 Key 模式：不保存共享密钥，转发密钥只取密钥绑定行；创建后不可变更。
	UserKeyRequired bool `json:"user_key_required"`
	// Currency 模型计价币种（ISO 4217）；空串表示未配置，不参与金额记账。
	Currency string `json:"currency"`
	// EnablePeakPricing 是否启用峰谷定价（时段配置在价目快照里）。
	EnablePeakPricing bool `json:"enable_peak_pricing"`
	// UseUpstreamCost 是否优先采信上游自报的 usage.cost。
	UseUpstreamCost bool `json:"use_upstream_cost"`
	// CurrentPrice 当前生效的价目快照；无价目时为 null。
	CurrentPrice *priceResponse `json:"current_price"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

// modelCreateRequest 是创建请求；quota_day / enabled 缺省时分别取 -1 / true。
// use_upstream_cost 缺省为 true（与列默认值一致），enable_peak_pricing 缺省为 false。
type modelCreateRequest struct {
	Provider          string           `json:"provider"`
	ModelID           string           `json:"model_id"`
	ModelName         string           `json:"model_name"`
	URL               string           `json:"url"`
	APIKey            string           `json:"api_key"`
	CredentialID      *uint            `json:"credential_id"`
	ModelType         string           `json:"model_type"`
	BillingMode       string           `json:"billing_mode"`
	CustomHTTPHeaders *json.RawMessage `json:"custom_http_headers"`
	QuotaDay          *int             `json:"quota_day"`
	Enabled           *bool            `json:"enabled"`
	UserKeyRequired   *bool            `json:"user_key_required"`
	Currency          string           `json:"currency"`
	EnablePeakPricing *bool            `json:"enable_peak_pricing"`
	UseUpstreamCost   *bool            `json:"use_upstream_cost"`
}

// modelUpdateRequest 是局部更新请求：指针字段用于区分「未传」与「零值」。
type modelUpdateRequest struct {
	Provider          *string          `json:"provider"`
	ModelID           *string          `json:"model_id"`
	ModelName         *string          `json:"model_name"`
	URL               *string          `json:"url"`
	APIKey            *string          `json:"api_key"`
	CredentialID      *uint            `json:"credential_id"`
	ModelType         *string          `json:"model_type"`
	BillingMode       *string          `json:"billing_mode"`
	CustomHTTPHeaders *json.RawMessage `json:"custom_http_headers"`
	QuotaDay          *int             `json:"quota_day"`
	Enabled           *bool            `json:"enabled"`
	UserKeyRequired   *bool            `json:"user_key_required"`
	Currency          *string          `json:"currency"`
	EnablePeakPricing *bool            `json:"enable_peak_pricing"`
	UseUpstreamCost   *bool            `json:"use_upstream_cost"`
}

// handleCreateModel 创建模型；成功后失效该模型的计价缓存（币种/开关即刻对数据面生效）。
func handleCreateModel(store *model.Store, src *billing.StoreSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req modelCreateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if msg := validateModelCreate(&req); msg != "" {
			httpx.WriteError(w, http.StatusBadRequest, msg)
			return
		}
		headers, _, err := resolveCustomHeaders(req.CustomHTTPHeaders)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.CredentialID != nil && *req.CredentialID > 0 {
			ok, err := credentialExists(r.Context(), store, *req.CredentialID)
			if err != nil {
				writeInternalError(w, err)
				return
			}
			if !ok {
				httpx.WriteError(w, http.StatusBadRequest, fmt.Sprintf("credential %d not found", *req.CredentialID))
				return
			}
		}
		// 自备 Key 模式标记（入参校验见 validateModelCreate）。
		userKeyRequired := req.UserKeyRequired != nil && *req.UserKeyRequired

		currency, ok := normalizeCurrency(req.Currency)
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "currency must be a 3-letter ISO 4217 code")
			return
		}

		billingMode, msg := resolveBillingMode(req.ModelType, req.BillingMode)
		if msg != "" {
			httpx.WriteError(w, http.StatusBadRequest, msg)
			return
		}
		m := &model.AIModel{
			Provider:          req.Provider,
			ModelID:           req.ModelID,
			ModelName:         req.ModelName,
			URL:               req.URL,
			APIKey:            req.APIKey,
			ModelType:         req.ModelType,
			BillingMode:       billingMode,
			CustomHTTPHeaders: headers,
			QuotaDay:          -1,
			Enabled:           true,
			Currency:          currency,
			// 缺省与列默认值一致：采信上游自报成本，不启用峰谷定价。
			UseUpstreamCost:   req.UseUpstreamCost == nil || *req.UseUpstreamCost,
			EnablePeakPricing: req.EnablePeakPricing != nil && *req.EnablePeakPricing,
		}
		if req.CredentialID != nil {
			m.CredentialID = *req.CredentialID
		}
		if req.QuotaDay != nil {
			m.QuotaDay = *req.QuotaDay
		}
		if req.Enabled != nil {
			m.Enabled = *req.Enabled
		}
		m.UserKeyRequired = userKeyRequired
		if err := store.CreateAIModel(r.Context(), m); err != nil {
			writeInternalError(w, err)
			return
		}
		// 带 default 标签的零值字段在 INSERT 时会被 GORM 跳过（enabled=false / quota_day=0）：
		// 显式给出这些零值的请求补一次定向更新，避免落成库默认值（use_upstream_cost=false 同理）。
		if patch := modelZeroValuePatch(req); len(patch) > 0 {
			updated, err := store.UpdateAIModel(r.Context(), m.ID, patch)
			if err != nil {
				writeInternalError(w, err)
				return
			}
			m = updated
		}
		// 新模型的计价配置即刻对数据面生效（没有自动失效钩子）。
		invalidateModel(src, m.ID)
		httpx.WriteJSON(w, http.StatusCreated, toModelResponse(m))
	}
}

func handleListModels(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := store.ListAIModels(r.Context())
		if err != nil {
			writeInternalError(w, err)
			return
		}
		out := make([]modelResponse, 0, len(list))
		for i := range list {
			out = append(out, toModelResponse(&list[i]))
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"models": out})
	}
}

func handleGetModel(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid model id")
			return
		}
		m, err := store.GetAIModel(r.Context(), id)
		if err != nil {
			writeLookupError(w, err, "model")
			return
		}
		resp, err := toModelResponseWithPrice(r, store, m)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, resp)
	}
}

// handleUpdateModel 局部更新模型；改动计价相关字段（currency / 两个开关）后失效该模型的计价缓存。
func handleUpdateModel(store *model.Store, src *billing.StoreSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid model id")
			return
		}
		var req modelUpdateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		before, err := store.GetAIModel(r.Context(), id)
		if err != nil {
			writeLookupError(w, err, "model")
			return
		}
		if msg := validateModelUpdate(&req, before); msg != "" {
			httpx.WriteError(w, http.StatusBadRequest, msg)
			return
		}
		// 跨族变更（chat ↔ 决策）且模型仍被密钥绑定时拒绝：改动会让线上已绑定的调用立刻 400。
		if req.ModelType != nil && IsDecisionModelType(*req.ModelType) != IsDecisionModelType(before.ModelType) {
			bound, err := store.CountAIModelBindings(r.Context(), id)
			if err != nil {
				writeInternalError(w, err)
				return
			}
			if bound > 0 {
				httpx.WriteError(w, http.StatusBadRequest,
					fmt.Sprintf("model is bound to %d key(s); unbind them before changing model_type family", bound))
				return
			}
		}

		fields, msg := modelUpdateFields(&req)
		if msg != "" {
			httpx.WriteError(w, http.StatusBadRequest, msg)
			return
		}
		if req.CredentialID != nil && *req.CredentialID > 0 {
			ok, err := credentialExists(r.Context(), store, *req.CredentialID)
			if err != nil {
				writeInternalError(w, err)
				return
			}
			if !ok {
				httpx.WriteError(
					w,
					http.StatusBadRequest,
					fmt.Sprintf("credential %d not found", *req.CredentialID),
				)
				return
			}
		}
		headers, present, err := resolveCustomHeaders(req.CustomHTTPHeaders)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if present {
			fields["custom_http_headers"] = headers
		}

		m, err := store.UpdateAIModel(r.Context(), id, fields)
		if err != nil {
			writeLookupError(w, err, "model")
			return
		}
		// 任何字段变更都失效该模型的计价缓存（幂等：未改计价字段时只是多读一次库）。
		invalidateModel(src, id)
		resp, err := toModelResponseWithPrice(r, store, m)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, resp)
	}
}

func handleDeleteModel(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid model id")
			return
		}
		if _, err := store.GetAIModel(r.Context(), id); err != nil {
			writeLookupError(w, err, "model")
			return
		}
		if err := store.DeleteAIModel(r.Context(), id); err != nil {
			writeInternalError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// toModelResponse 将模型实体转为对外表示（丢弃 api_key）。
func toModelResponse(m *model.AIModel) modelResponse {
	return modelResponse{
		ID:                m.ID,
		Provider:          m.Provider,
		ModelID:           m.ModelID,
		ModelName:         m.ModelName,
		URL:               m.URL,
		CredentialID:      m.CredentialID,
		ModelType:         m.ModelType,
		BillingMode:       billingModeOrDefault(m.BillingMode),
		CustomHTTPHeaders: m.GetCustomHTTPHeaders(),
		QuotaDay:          m.QuotaDay,
		Enabled:           m.Enabled,
		UserKeyRequired:   m.UserKeyRequired,
		Currency:          m.Currency,
		EnablePeakPricing: m.EnablePeakPricing,
		UseUpstreamCost:   m.UseUpstreamCost,
		CreatedAt:         m.CreatedAt,
		UpdatedAt:         m.UpdatedAt,
	}
}

// toModelResponseWithPrice 在模型对外表示上附带当前生效价目（无价目为 null）。
func toModelResponseWithPrice(r *http.Request, store *model.Store, m *model.AIModel) (modelResponse, error) {
	resp := toModelResponse(m)
	price, err := currentPrice(r, store, m.ID)
	if err != nil {
		return modelResponse{}, err
	}
	resp.CurrentPrice = price
	return resp, nil
}

// validateModelCreate 校验模型创建请求的纯字段约束，返回错误消息（空串表示通过）。
// 依赖存储的校验（凭据存在性等）留在 handler 内执行。
func validateModelCreate(req *modelCreateRequest) string {
	if strings.TrimSpace(req.ModelID) == "" {
		return "model_id is required"
	}
	if strings.TrimSpace(req.URL) == "" {
		return "url is required"
	}
	if !validModelType(req.ModelType) {
		return "model_type must be one of: " + modelTypeList()
	}
	// 自备 Key 模式：不保存共享密钥（照搬 hatchery：UserKeyRequired=true 时 api_key 必须为空）。
	if req.UserKeyRequired != nil && *req.UserKeyRequired {
		if strings.TrimSpace(req.APIKey) != "" {
			return "user_key_required models must not carry a shared api_key"
		}
		if req.CredentialID != nil && *req.CredentialID > 0 {
			return "user_key_required models must not reference a credential"
		}
	}
	return ""
}

// validateModelUpdate 校验模型更新请求中与既有状态相关的纯约束，返回错误消息（空串表示通过）。
func validateModelUpdate(req *modelUpdateRequest, before *model.AIModel) string {
	// 自备 Key 模式创建后不可变更（照搬 hatchery「创建时确定，编辑时不可转换」）。
	if req.UserKeyRequired != nil && *req.UserKeyRequired != before.UserKeyRequired {
		return "user_key_required cannot be changed after creation"
	}
	// 自备 Key 模型不保存共享密钥：拒绝携带 api_key 或凭据引用。
	if before.UserKeyRequired {
		if req.APIKey != nil && strings.TrimSpace(*req.APIKey) != "" {
			return "user_key_required models must not carry a shared api_key"
		}
		if req.CredentialID != nil && *req.CredentialID > 0 {
			return "user_key_required models must not reference a credential"
		}
	}
	return ""
}

// modelUpdateFields 汇总局部更新的字段集；入场校验（空值、类型）失败时返回错误消息。
func modelUpdateFields(req *modelUpdateRequest) (map[string]any, string) {
	fields := make(map[string]any)
	if req.Provider != nil {
		fields["provider"] = *req.Provider
	}
	if req.ModelID != nil {
		if strings.TrimSpace(*req.ModelID) == "" {
			return nil, "model_id must not be empty"
		}
		fields["model_id"] = *req.ModelID
	}
	if req.ModelName != nil {
		fields["model_name"] = *req.ModelName
	}
	if req.URL != nil {
		if strings.TrimSpace(*req.URL) == "" {
			return nil, "url must not be empty"
		}
		fields["url"] = *req.URL
	}
	if req.ModelType != nil {
		if !validModelType(*req.ModelType) {
			return nil, "model_type must be one of: " + modelTypeList()
		}
		fields["model_type"] = *req.ModelType
	}
	if req.BillingMode != nil {
		if !model.ValidBillingMode(*req.BillingMode) {
			return nil, "billing_mode must be per_token or input_only"
		}
		fields["billing_mode"] = *req.BillingMode
	}
	if req.APIKey != nil && *req.APIKey != "" {
		fields["api_key"] = *req.APIKey
	}
	if req.CredentialID != nil {
		fields["credential_id"] = *req.CredentialID
	}
	if req.QuotaDay != nil {
		fields["quota_day"] = *req.QuotaDay
	}
	if req.Enabled != nil {
		fields["enabled"] = *req.Enabled
	}
	if req.Currency != nil {
		currency, ok := normalizeCurrency(*req.Currency)
		if !ok {
			return nil, "currency must be a 3-letter ISO 4217 code"
		}
		fields["currency"] = currency
	}
	if req.EnablePeakPricing != nil {
		fields["enable_peak_pricing"] = *req.EnablePeakPricing
	}
	if req.UseUpstreamCost != nil {
		fields["use_upstream_cost"] = *req.UseUpstreamCost
	}
	return fields, ""
}

// validModelType 校验模型接口类型取值。
func validModelType(t string) bool {
	switch t {
	case model.ModelTypeOpenAI, model.ModelTypeAnthropic, model.ModelTypeResponses,
		model.ModelTypeDecisions, model.ModelTypeSystemOne:
		return true
	default:
		return false
	}
}

// modelTypeList 是 model_type 合法取值的文案（四处错误消息共用）。
func modelTypeList() string {
	return model.ModelTypeOpenAI + ", " + model.ModelTypeAnthropic + ", " + model.ModelTypeResponses +
		", " + model.ModelTypeDecisions + " or " + model.ModelTypeSystemOne
}

// billingModeOrDefault 把空值按 per_token 回显（加列前的存量行没有该字段）。
func billingModeOrDefault(m string) string {
	if m == "" {
		return model.BillingModePerToken
	}
	return m
}

// IsDecisionModelType 报告 model_type 是否是决策协议；与 provider 侧白名单同口径，
// admin 包不 import provider，故在此重复一次（取值仍来自 internal/model）。
func IsDecisionModelType(t string) bool {
	return t == model.ModelTypeDecisions || t == model.ModelTypeSystemOne
}

// resolveBillingMode 决定创建模型时的计费口径：
//   - 显式给出：必须是合法值；
//   - 未给出：决策模型默认 input_only（决策的输出通常免费），其余默认 per_token。
func resolveBillingMode(modelType, requested string) (string, string) {
	if requested == "" {
		if IsDecisionModelType(modelType) {
			return model.BillingModeInputOnly, ""
		}
		return model.BillingModePerToken, ""
	}
	if !model.ValidBillingMode(requested) {
		return "", "billing_mode must be per_token or input_only"
	}
	return requested, ""
}

// modelZeroValuePatch 返回创建后需要补写的零值字段（见 handleCreateModel 中说明）。
func modelZeroValuePatch(req modelCreateRequest) map[string]any {
	patch := make(map[string]any)
	if req.Enabled != nil && !*req.Enabled {
		patch["enabled"] = false
	}
	if req.QuotaDay != nil && *req.QuotaDay == 0 {
		patch["quota_day"] = 0
	}
	// use_upstream_cost 列默认 true：显式传 false 时 Create 会跳过零值，需补写。
	if req.UseUpstreamCost != nil && !*req.UseUpstreamCost {
		patch["use_upstream_cost"] = false
	}
	return patch
}

// resolveCustomHeaders 归一化 custom_http_headers：
// 返回值 (存储用 JSON 字符串, 请求中是否出现, 错误)。未出现时返回 ("", false, nil)。
func resolveCustomHeaders(raw *json.RawMessage) (string, bool, error) {
	if raw == nil {
		return "", false, nil
	}
	if strings.TrimSpace(string(*raw)) == "null" {
		return "", true, nil
	}
	var headers map[string]string
	if err := json.Unmarshal(*raw, &headers); err != nil {
		return "", false, fmt.Errorf("custom_http_headers must be a JSON object of string values")
	}
	encoded, err := json.Marshal(headers)
	if err != nil {
		return "", false, fmt.Errorf("custom_http_headers cannot be encoded")
	}
	return string(encoded), true, nil
}

// writeLookupError 统一处理「查找/更新目标」错误：不存在 → 404，其余 → 500。
func writeLookupError(w http.ResponseWriter, err error, name string) {
	if model.IsNotFound(err) {
		httpx.WriteError(w, http.StatusNotFound, name+" not found")
		return
	}
	writeInternalError(w, err)
}
