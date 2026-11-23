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

// Package proxy 实现数据面转发管道：鉴权 → 模型解析 → 参数强制 → 上游转发 → 用量记账。
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"aigate/internal/event"
	"aigate/internal/httpx"
	"aigate/internal/model"
	"aigate/internal/provider"
	"aigate/internal/quota"
)

// Handler 数据面处理器。
type Handler struct {
	store               *model.Store
	usage               *UsageWriter
	quotaExceededStatus int // 配额超限状态码（429 或 403，默认 429）

	// allowResponseStorage 是 Responses 站点级存储开关（-allow-response-storage，默认 true）。
	// false 时所有请求按 store:false 处理、显式 store:true 与有状态字段一律 400。
	// 由 SetResponseStorage 在装配阶段设置，运行期只读，故不需要加锁。
	allowResponseStorage bool

	// pruneStartOnce / pruneCancel 是保留期清理循环的启停句柄（StartResponseRetention 注册，
	// StopResponseRetention 取消；未启动时 pruneCancel 为 nil）。
	pruneStartOnce sync.Once
	pruneCancel    context.CancelFunc

	// responseBackgroundTimeout 是 background:true 后台执行的总时长上限
	// （-response-background-timeout，默认 responseBackgroundDefaultTimeout）。
	// 与 allowResponseStorage 同口径：装配阶段由 SetResponseBackgroundTimeout 设置，
	// 运行期只读（后台 goroutine 每个任务读一次），因此不需要加锁。
	responseBackgroundTimeout time.Duration

	// backgroundRuns 登记在途后台任务的可取消 ctx（键为 respID），
	// 由 registerBackgroundRun / releaseBackgroundRun 维护，cancel 时据此中断上游请求
	// （见 responses_background.go 顶部的「在途后台任务的注册表」一节）。
	backgroundMu   sync.Mutex
	backgroundRuns map[string]context.CancelFunc
}

// NewHandler 创建数据面处理器。quotaExceededStatus 非 429/403 时回退 429。
//
// Responses 存储默认开启（与 -allow-response-storage 的默认值一致）；
// 站点关闭存储与保留期清理分别由 SetResponseStorage / StartResponseRetention 装配。
func NewHandler(store *model.Store, usage *UsageWriter, quotaExceededStatus int) *Handler {
	if quotaExceededStatus != 429 && quotaExceededStatus != 403 {
		quotaExceededStatus = http.StatusTooManyRequests
	}
	return &Handler{
		store:                     store,
		usage:                     usage,
		quotaExceededStatus:       quotaExceededStatus,
		allowResponseStorage:      true,
		responseBackgroundTimeout: responseBackgroundDefaultTimeout,
	}
}

// Register 注册数据面路由。
//
// Responses 的四条路由：创建 + 三个生命周期端点（design §5.6）。生命周期端点必须带 {id}，
// 与 POST /v1/responses 是不同模式，互不影响（Ruling 3：后加的路由只追加，不重写本函数）。
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", h.handleChatCompletions)
	mux.HandleFunc("POST /v1/messages", h.handleAnthropicMessages)
	mux.HandleFunc("POST /v1/responses", h.handleResponses)
	mux.HandleFunc("GET /v1/responses/{id}", h.handleGetResponse)
	mux.HandleFunc("DELETE /v1/responses/{id}", h.handleDeleteResponse)
	mux.HandleFunc("POST /v1/responses/{id}/cancel", h.handleCancelResponse)
	// 决策模型入口（spec §7.2）：两个协议各一条，外加无版本别名以适配
	// TypeSafe SDK（base_url 只写 origin）与 OpenAI SDK（base_url 含 /v1）两种配置习惯。
	mux.HandleFunc("POST /v1/decisions", h.handleDecisions)
	mux.HandleFunc("POST /decisions", h.handleDecisions)
	mux.HandleFunc("POST /v1/systemone", h.handleSystemOne)
	mux.HandleFunc("POST /systemone", h.handleSystemOne)
	mux.HandleFunc("GET /v1/models", h.handleModels)
}

func (h *Handler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// 1. 密钥鉴权（哈希查询）。
	key := h.authKey(w, r)
	if key == nil {
		return
	}

	// 2. 读取请求体并解析目标模型。
	body, err := httpx.ReadBody(r)
	if err != nil {
		if errors.Is(err, httpx.ErrBodyTooLarge) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, msgBodyTooLarge)
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, msgReadRequestBody)
		return
	}
	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, msgInvalidJSON)
		return
	}

	// 3. 模型解析 → 配额检查 → max_tokens 强制 → body 重写（与 /v1/messages 共用）。
	resolved, modifiedBody, ok := h.prepareForward(w, r, key, reqBody)
	if !ok {
		return
	}
	isStreaming, _ := reqBody["stream"].(bool)

	// 4. 按模型类型分派到上游 provider。
	p := provider.GetProvider(resolved.ModelType)
	customHeaders := extractCustomHeaders(r, resolved.CustomHTTPHeaders)
	ctx := r.Context()

	if isStreaming {
		flusher, ok := w.(http.Flusher)
		if !ok {
			httpx.WriteError(w, http.StatusInternalServerError, msgStreamNotSupported)
			return
		}
		sw := &streamWriter{ResponseWriter: w, flusher: flusher}
		result, statusCode, err := p.ChatCompletionStream(
			ctx,
			resolved.APIKey,
			resolved.URL,
			resolved.ModelID,
			modifiedBody,
			sw,
			flusher,
			customHeaders,
		)
		if err != nil {
			slog.Error("upstream stream error", "error", err, "model", resolved.ModelID)
			// 未写出任何数据（如连接失败）：返回 502，与非流式语义一致；
			// 错误细节只进日志，不回显给调用方（避免泄漏内网地址/拓扑）。
			if !sw.wrote {
				httpx.WriteError(w, http.StatusBadGateway, msgUpstreamConnect)
			}
			return
		}
		var usage *provider.Usage
		if result != nil {
			usage = result.Usage
		}
		if result != nil || statusCode != 0 {
			h.logUsage(key, resolved, usage, statusCode, start)
		}
		return
	}

	result, statusCode, err := p.ChatCompletion(
		ctx,
		resolved.APIKey,
		resolved.URL,
		resolved.ModelID,
		modifiedBody,
		customHeaders,
	)
	if err != nil {
		// 错误细节只进日志，不回显给调用方（避免泄漏内网地址/拓扑）。
		slog.Error("upstream request error", "error", err, "model", resolved.ModelID)
		// 记账完整性：上游已回包但响应体读取中断（截断）时，这次请求确实被消耗掉了，必须落库。
		// provider 在该情形返回 result=nil、状态码 0、token 数不可得，故金额为 0，但行不能丢。
		// 完全无响应（连接/DNS 失败）没有任何计费依据，不记。
		if isUpstreamBodyReadError(err) {
			h.logUsage(key, resolved, nil, http.StatusInternalServerError, start)
		}
		httpx.WriteError(w, http.StatusBadGateway, msgUpstreamConnect)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = w.Write(result.Body)
	h.logUsage(key, resolved, result.Usage, statusCode, start)
}

// isUpstreamBodyReadError 判断错误是否来自「上游已回包、但响应体读取中断」。
// provider 对读体失败统一用 richErrorf 包装底层错误（如 io.ErrUnexpectedEOF），errors.Is 可穿透；
// 连接/拨号阶段的失败发生在拿到响应头之前，不会被判为 true。
func isUpstreamBodyReadError(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF)
}

// prepareForward 完成「模型解析 → 配额检查 → max_tokens 强制 → body 重写」；
// 失败时已写出错误响应并返回 ok=false。
//
// 返回的 body 是「model 替换为上游原始 ID、max_tokens 已施加密钥上限」后的请求体字节。
// 错误响应风格按当前路由选择（见 writeIngressError / writeIngressQuotaExceeded）。
func (h *Handler) prepareForward(
	w http.ResponseWriter,
	r *http.Request,
	key *model.APIKey,
	reqBody map[string]any,
) (resolved *model.Resolved, body []byte, ok bool) {
	// 解析模型 + 配额检查。
	res, ok := h.resolveAndCheckQuota(w, r, key, reqBody)
	if !ok {
		return nil, nil, false
	}

	// 协议守卫：chat 系入口不接受决策模型。GetProvider 对未知类型会兜底到 chat，
	// 决策模型若漏到这里会被当成 chat 发给上游（静默错发），因此在分派前拒绝。
	if provider.IsDecisionModelType(res.ModelType) {
		h.writeIngressError(w, r, http.StatusBadRequest, fmt.Sprintf(msgDecisionOnChatRoute, res.ModelType))
		return nil, nil, false
	}

	// 强制 max_tokens 上限，并把上游 model 名替换为绑定的原始 ID。
	enforceMaxTokens(reqBody, key.MaxTokens)
	reqBody["model"] = res.ModelID
	modifiedBody, err := json.Marshal(reqBody)
	if err != nil {
		h.writeIngressError(w, r, http.StatusInternalServerError, msgInvalidJSON)
		return nil, nil, false
	}
	return res, modifiedBody, true
}

// resolveAndCheckQuota 只完成 prepareForward 的前半段：「模型解析 → 配额检查」；
// 不做 max_tokens 压制与 body 重写。失败时已写出错误响应并返回 ok=false。
//
// 拆成两段是为了让调用方能「只做解析与配额、不碰请求体」：Responses 入口用它拿 resolved
// 再按 ModelType 分派，避免把 Responses 形态 body 交给 prepareForward 的改写步骤
// （那会二次压制并掩盖 §5.4.1 第 4 步）。每条路径对它的调用恰好一次。
func (h *Handler) resolveAndCheckQuota(
	w http.ResponseWriter,
	r *http.Request,
	key *model.APIKey,
	reqBody map[string]any,
) (*model.Resolved, bool) {
	// 解析密钥绑定的目标模型（显式未绑定 → 400；未指定 → primary）。
	reqModelName := extractRequestedModel(reqBody)
	res, err := h.store.ResolveModelForKey(r.Context(), key, reqModelName)
	if err != nil {
		switch {
		case errors.Is(err, model.ErrModelNotBound):
			h.writeIngressError(w, r, http.StatusBadRequest, msgModelNotBound)
		case errors.Is(err, model.ErrNoResolvableModel):
			h.writeIngressError(w, r, http.StatusBadRequest, msgNoResolvableModel)
		case errors.Is(err, model.ErrBindingKeyMissing):
			h.writeIngressError(w, r, http.StatusBadRequest, msgBindingKeyMissing)
		default:
			slog.Error("resolve model failed", "error", err, "key_id", key.ID)
			h.writeIngressError(w, r, http.StatusInternalServerError, msgInternalError)
		}
		return nil, false
	}

	// 配额检查（全局 → 模型日配额 → 用户 → 密钥；首个超限即拒绝，带 Retry-After）。
	if st := quota.CheckRequest(r.Context(), h.store, key, res, time.Now()); st.Exceeded {
		h.writeIngressQuotaExceeded(w, r, key, res, st)
		return nil, false
	}
	return res, true
}

func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	key := h.authKey(w, r)
	if key == nil {
		return
	}
	infos, err := h.store.ListKeyModels(r.Context(), key.ID, true)
	if err != nil {
		slog.Error("list key models failed", "error", err, "key_id", key.ID)
		httpx.WriteError(w, http.StatusInternalServerError, msgInternalError)
		return
	}
	seen := make(map[string]struct{}, len(infos))
	data := make([]map[string]any, 0, len(infos))
	created := time.Now().Unix()
	for _, m := range infos {
		id := strings.ToLower(m.ModelID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		data = append(data, map[string]any{
			"id":       id,
			"object":   "model",
			"created":  created,
			"owned_by": m.Provider,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// writeQuotaExceeded 写出配额超限响应：按配置的状态码（默认 429）+ Retry-After 头；
// 响应后发出 quota.exceeded 事件（key/resolved 提供事件维度字段）。
//
// 响应体在统一错误体之外补充 unit / currency：unit 为 token 或 cost，让调用方知道
// used / limit 是 token 还是金额；cost 规则另带结算币种，token 规则币种为空（省略）。
// 注意 Status.Unit 对 token 规则是空串（保持既有结果的零值约定），
// "token" 这个字面量只在此 JSON 输出边界补齐。
func (h *Handler) writeQuotaExceeded(
	w http.ResponseWriter,
	key *model.APIKey,
	resolved *model.Resolved,
	st quota.Status,
) {
	msg := msgUserQuotaExceeded
	switch st.Kind {
	case "global":
		msg = msgGlobalQuotaExceeded
	case "model":
		msg = msgModelQuotaExceeded
	case "key":
		msg = msgKeyQuotaExceeded
	}
	unit := st.Unit
	if unit == "" {
		unit = quota.UnitToken
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(st.RetryAfter)))
	httpx.WriteErrorDetail(w, h.quotaExceededStatus, httpx.ErrorDetail{
		Message:  msg,
		Unit:     unit,
		Currency: st.Currency,
	})
	emitQuotaExceeded(key, resolved, st)
}

// emitQuotaExceeded 发出 quota.exceeded 事件。
// 两个配额响应写出口（OpenAI 风格与 Anthropic 风格）互斥调用，事件不会重复发出。
func emitQuotaExceeded(key *model.APIKey, resolved *model.Resolved, st quota.Status) {
	event.Emit(event.TypeQuotaExceeded, map[string]any{
		"api_key_id": key.ID,
		"user_id":    key.UserID,
		"group_id":   key.GroupID,
		"model":      resolved.ModelID,
		"kind":       st.Kind,
		"limit":      st.Limit,
		"used":       st.Used,
	})
}

// retryAfterSeconds 把重试间隔换算为秒（无法推算时回退 3600s，下限 1s）。
func retryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 3600
	}
	secs := int(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	return secs
}

// authKey 解析 Bearer 密钥并校验启用状态与有效期；校验失败时直接写出 401 响应并返回 nil。
// 缺失凭据与无效凭据使用不同文案（missing / invalid），与 OpenAI 生态习惯一致；
// 已过期密钥按「无效凭据」口径拒绝（不暴露过期状态，管理员经事件与字段感知）。
func (h *Handler) authKey(w http.ResponseWriter, r *http.Request) *model.APIKey {
	token := extractBearerToken(r)
	if token == "" {
		httpx.WriteError(w, http.StatusUnauthorized, msgMissingAPIKey)
		return nil
	}
	key, err := h.store.GetAPIKeyByTokenHash(r.Context(), model.HashToken(token))
	if err != nil || !key.Enabled {
		httpx.WriteError(w, http.StatusUnauthorized, msgInvalidAPIKey)
		return nil
	}
	if rejectExpiredKey(r.Context(), h.store, key, time.Now()) {
		httpx.WriteError(w, http.StatusUnauthorized, msgInvalidAPIKey)
		return nil
	}
	return key
}

// streamWriter 记录流式路径是否已写出数据（用于判断失败时能否返回 502）。
type streamWriter struct {
	http.ResponseWriter
	flusher http.Flusher
	wrote   bool
}

func (s *streamWriter) Write(p []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(p)
}

// Flush 透传到底层 flusher。
func (s *streamWriter) Flush() {
	s.flusher.Flush()
}

// logUsage 投递用量日志（异步、不阻塞）并发出 request.completed 事件。
// 事件 data 的 usage 与用量记账口径一致（流式路径同样在流结束后调用本函数）。
func (h *Handler) logUsage(
	key *model.APIKey,
	resolved *model.Resolved,
	usage *provider.Usage,
	statusCode int,
	start time.Time,
) {
	entry := model.LLMUsageLog{
		APIKeyID:   key.ID,
		UserID:     key.UserID,
		GroupID:    key.GroupID,
		AIModelID:  resolved.AIModelID,
		Model:      resolved.ModelID,
		Provider:   resolved.Provider,
		StatusCode: statusCode,
		Latency:    int(time.Since(start).Milliseconds()),
		CreatedAt:  time.Now().UTC(),
	}
	if usage != nil {
		entry.PromptTokens = usage.PromptTokens
		entry.CompletionTokens = usage.CompletionTokens
		entry.TotalTokens = usage.TotalTokens
		entry.PromptCacheReadTokens = usage.PromptCacheReadTokens
		entry.PromptCacheWriteTokens = usage.PromptCacheWriteTokens
		// 上游自报成本只做传输：由 UsageWriter 消费时按 use_upstream_cost 决定是否采信，不落库。
		entry.ReportedCost = usage.ReportedCost
	}
	h.usage.Submit(entry)

	event.Emit(event.TypeRequestCompleted, map[string]any{
		"api_key_id":  key.ID,
		"user_id":     key.UserID,
		"group_id":    key.GroupID,
		"model":       resolved.ModelID,
		"provider":    resolved.Provider,
		"status_code": statusCode,
		"latency_ms":  entry.Latency,
		"usage": map[string]any{
			"prompt_tokens":             entry.PromptTokens,
			"completion_tokens":         entry.CompletionTokens,
			"total_tokens":              entry.TotalTokens,
			"prompt_cache_read_tokens":  entry.PromptCacheReadTokens,
			"prompt_cache_write_tokens": entry.PromptCacheWriteTokens,
		},
	})
}

// extractBearerToken 从 Authorization 头提取 Bearer token。
func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
}

// extractRequestedModel 读取请求体中的 model 字段（空串表示未指定）。
func extractRequestedModel(reqBody map[string]any) string {
	if v, ok := reqBody["model"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// maxTokensChatFields 是 chat 协议的输出长度字段（缺少时以 max_tokens 注入上限）。
// 用数组而非切片：包级变量不可被误改写。
var maxTokensChatFields = [...]string{"max_tokens", "max_completion_tokens"}

// maxTokensCappedFields 是全部受密钥上限压制的输出长度字段：
// chat 协议的 max_tokens / max_completion_tokens，以及 Responses 协议的 max_output_tokens。
var maxTokensCappedFields = [...]string{"max_tokens", "max_completion_tokens", "max_output_tokens"}

// maxTokensFieldsProvided 报告请求是否在某组字段上给出了有效数值。
//
// 只有 float64 才算「有效数值」，与压制循环的判定一致：
//   - JSON null 经 json.Unmarshal 得到 nil 但 key 保留，实质等同于「未提供」；
//   - 字符串 "9999"、布尔 true 等非数值同样不构成上限（逐字约束：非数值忽略）。
//
// 三种情况都不算客户端自己给了上限，否则会留下「有字段、无上限、也不注入」的空窗，
// 让上游收到无上限的输出预算。
func maxTokensFieldsProvided(reqBody map[string]any, fields []string) bool {
	for _, key := range fields {
		if _, ok := reqBody[key].(float64); ok {
			return true
		}
	}
	return false
}

// isResponsesShaped 判断请求体是否 Responses 形态：带 max_output_tokens 且没有 chat 的 messages。
//
// 协议完全由请求体自身的形态判定，不依赖 model_type（签名拿不到，见 design §5.4.1）。
//
// 顺序契约（design §5.4.1）：调用方必须**在把 Responses 请求转成 chat 形状之前**调用
// enforceMaxTokens。Responses 入口的既定流程是
// 「对 Responses 形态 body 先调 enforceMaxTokens → 再 responsesRequestToChat 转 chat 形状
// → 再走 prepareForward 的其余步骤」，转换后得到的 chat 字段 max_tokens 已是压制后的值。
//
// 顺序颠倒（先转 chat 再压制）会让 Responses 请求退化为 chat 形态（带 messages），
// max_output_tokens 随之失去密钥上限保护——这是明令禁止的执行顺序，不是设计意图。
func isResponsesShaped(reqBody map[string]any) bool {
	if _, ok := reqBody["max_output_tokens"]; !ok {
		return false
	}
	_, hasMessages := reqBody["messages"]
	return !hasMessages
}

// enforceMaxTokens 对请求体的输出长度字段施加密钥上限（三个数据面入口共用）。
// 数值字段超限、非正数（部分后端把 -1 视作「不限」）时统一收敛为上限。
//
// 哪些请求形态会把 max_output_tokens 转为数值下发：
//   - Responses 形态（带 max_output_tokens、无 messages）：字段超限/非正数时收敛为上限；
//     没有有效数值（null / 字符串 / 布尔）时改写为上限；此时不注入 chat 专有的 max_tokens。
//   - chat 形态（带 messages）：max_output_tokens 是非标准字段，逐字透传——不压制、不改写、不删除，
//     上限一律经 chat 字段按既有语义施加，避免严格校验的上游因多出的字段 400。
func enforceMaxTokens(reqBody map[string]any, limit int) {
	if limit <= 0 {
		return
	}
	cappedFields := maxTokensChatFields[:]
	outputTokensIsResponses := isResponsesShaped(reqBody)
	if outputTokensIsResponses {
		cappedFields = maxTokensCappedFields[:]
	}
	for _, key := range cappedFields {
		v, ok := reqBody[key]
		if !ok {
			continue
		}
		f, ok := v.(float64)
		if !ok {
			continue
		}
		// 用浮点比较而非 int(f)：避免超大值在 float→int 转换上的实现定义行为（可绕过上限）。
		if f <= 0 || f > float64(limit) {
			reqBody[key] = float64(limit)
		}
	}
	if maxTokensFieldsProvided(reqBody, cappedFields) {
		return
	}
	// Responses 形态且没有有效数值（null / 非数值）：补 max_output_tokens，
	// 别注入 chat 专有的 max_tokens，避免污染 Responses 请求体。
	if outputTokensIsResponses {
		reqBody["max_output_tokens"] = float64(limit)
		return
	}
	reqBody["max_tokens"] = float64(limit)
}

// extractCustomHeaders 从入站请求提取需透传给上游的自定义头。
// 语义：任一配置头的值与请求不一致时整体不透传并告警；全部一致时按配置键透传。
func extractCustomHeaders(r *http.Request, configured map[string]string) map[string]string {
	if len(configured) == 0 {
		return nil
	}
	result := make(map[string]string, len(configured))
	for key := range configured {
		if v := r.Header.Get(key); v != "" && v == configured[key] {
			result[http.CanonicalHeaderKey(key)] = v
		} else {
			slog.Warn("custom header value mismatch, passthrough disabled", "header", key)
			return nil
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
