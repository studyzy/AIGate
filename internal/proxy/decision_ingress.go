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

// 本文件实现决策模型的两条入站管线：POST /v1/decisions（OpenAI Decisions）与
// POST /v1/systemone（TypeSafe System One），以及无版本别名 /decisions、/systemone。
//
// 执行顺序（与 spec §7.6 一致）：
//  1. 密钥鉴权；
//  2. 读 body + 入口协议解码（非法形状 400，不触达上游）；
//  3. 模型解析 + 配额（每条路径恰好一次，走 resolveAndCheckQuota，不做 max_tokens 压制）；
//  4. 协议白名单守卫：命中模型的 model_type 必须是决策协议；
//  5. 按 model_type 选 DecisionProvider，IR → 上游格式 → 发送；
//  6. 上游非 200 原样透传；200 则按**入口格式**编回响应；
//  7. 用量记账（input_only 模型把 completion 归零）。
//
// 决策协议没有流式：stream:true 在解码阶段就被拒绝。

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"aigate/internal/httpx"
	"aigate/internal/model"
	"aigate/internal/provider"
)

// handleDecisions 处理 OpenAI Decisions 入口。
func (h *Handler) handleDecisions(w http.ResponseWriter, r *http.Request) {
	h.handleDecision(w, r, ingressDecisions)
}

// handleSystemOne 处理 TypeSafe System One 入口。
func (h *Handler) handleSystemOne(w http.ResponseWriter, r *http.Request) {
	h.handleDecision(w, r, ingressSystemOne)
}

// decodeDecisionRequest 按入口格式把请求体解码成 IR 与原始 map（后者供模型解析）。
func decodeDecisionRequest(kind ingressKind, body []byte) (*provider.DecisionRequest, map[string]any, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, nil, &provider.DecisionBadRequest{Message: msgInvalidJSON}
	}
	var (
		ir  *provider.DecisionRequest
		err error
	)
	if kind == ingressDecisions {
		ir, err = provider.DecodeDecisionsRequest(body)
	} else {
		ir, err = provider.DecodeSystemOneRequest(body)
	}
	if err != nil {
		return nil, nil, err
	}
	return ir, req, nil
}

func (h *Handler) handleDecision(w http.ResponseWriter, r *http.Request, kind ingressKind) {
	start := time.Now()

	// 1. 密钥鉴权。
	key := h.authKey(w, r)
	if key == nil {
		return
	}

	// 2. 读 body 并按入口协议解码。
	body, err := httpx.ReadBody(r)
	if err != nil {
		if errors.Is(err, httpx.ErrBodyTooLarge) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, msgBodyTooLarge)
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, msgReadRequestBody)
		return
	}
	ir, reqBody, err := decodeDecisionRequest(kind, body)
	if err != nil {
		h.writeDecisionBadRequest(w, kind, err)
		return
	}

	// 3. 模型解析 + 配额（不做 max_tokens 压制：决策协议没有这个概念）。
	resolved, ok := h.resolveAndCheckQuota(w, r, key, reqBody)
	if !ok {
		return
	}

	// 4. 协议白名单守卫：决策入口只接受决策协议的上游模型。
	if !provider.IsDecisionModelType(resolved.ModelType) {
		h.writeIngressError(w, r, http.StatusBadRequest, fmt.Sprintf(msgNotDecisionModel, resolved.ModelType))
		return
	}
	decider, err := provider.GetDecisionProvider(resolved.ModelType)
	if err != nil {
		h.writeIngressError(w, r, http.StatusBadRequest, fmt.Sprintf(msgNotDecisionModel, resolved.ModelType))
		return
	}

	// 5. 发送。
	result, err := decider.Decide(
		r.Context(),
		resolved.APIKey,
		resolved.URL,
		resolved.ModelID,
		ir,
		extractCustomHeaders(r, resolved.CustomHTTPHeaders),
	)
	if err != nil {
		slog.Error("decision upstream error", "error", err, "model", resolved.ModelID)
		h.writeDecisionBadRequest(w, kind, err)
		h.logDecisionUsage(key, resolved, nil, http.StatusBadGateway, start)
		return
	}

	// 6. 上游错误原样透传；成功则按入口格式编回。
	if result.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(result.StatusCode)
		_, _ = w.Write(result.Body)
		h.logDecisionUsage(key, resolved, result.Resp, result.StatusCode, start)
		return
	}
	out, err := encodeDecisionResponse(kind, result.Resp)
	if err != nil {
		slog.Error("encode decision response failed", "error", err, "model", resolved.ModelID)
		httpx.WriteError(w, http.StatusBadGateway, msgUpstreamConnect)
		h.logDecisionUsage(key, resolved, result.Resp, http.StatusBadGateway, start)
		return
	}

	// 7. 回包 + 用量。
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	h.logDecisionUsage(key, resolved, result.Resp, http.StatusOK, start)
}

// encodeDecisionResponse 按入口格式把 IR 编成响应体。
func encodeDecisionResponse(kind ingressKind, resp *provider.DecisionResponse) ([]byte, error) {
	if kind == ingressDecisions {
		return provider.EncodeDecisionsResponse(resp)
	}
	return provider.EncodeSystemOneResponse(resp)
}

// writeDecisionError 按入口格式写出错误（两条决策入口都用 OpenAI 风格信封）。
func (h *Handler) writeDecisionError(w http.ResponseWriter, kind ingressKind, status int, message string) {
	httpx.WriteError(w, status, message)
}

// writeDecisionBadRequest 把解码/转换失败写成 400：
// 错误的入口形状与无法跨协议的字段都走这里，文案带上具体字段名。
func (h *Handler) writeDecisionBadRequest(w http.ResponseWriter, kind ingressKind, err error) {
	var bad *provider.DecisionBadRequest
	if errors.As(err, &bad) {
		h.writeDecisionError(w, kind, http.StatusBadRequest, fmt.Sprintf(decisionBadRequestFormat(kind), bad.Error()))
		return
	}
	if errors.Is(err, provider.ErrNotSupportedField) {
		h.writeDecisionError(w, kind, http.StatusBadRequest, err.Error())
		return
	}
	// 其余（含上游 200 但形状不认识、上游 body 过大）：属于上游侧失败。
	h.writeDecisionError(w, kind, http.StatusBadGateway, msgUpstreamConnect)
}

// decisionBadRequestFormat 返回入口对应的 400 文案模板。
func decisionBadRequestFormat(kind ingressKind) string {
	if kind == ingressDecisions {
		return msgInvalidDecisionInput
	}
	return msgInvalidSystemOneInput
}

// logDecisionUsage 记账一次决策请求的用量。
//
// input_only 模型（决策模型默认）没有可计费输出：completion 记 0，total 必须等于
// prompt——UpsertDailyUsage 在 total_tokens<=0 时直接跳过，只置 0 而不改 total
// 会让当日聚合缺行。响应体里的 usage 保持上游原值，不受此处影响。
func (h *Handler) logDecisionUsage(
	key *model.APIKey,
	resolved *model.Resolved,
	resp *provider.DecisionResponse,
	statusCode int,
	start time.Time,
) {
	var usage *provider.Usage
	if statusCode == http.StatusOK && resp != nil {
		usage = resp.Usage
		if usage != nil && resolved.BillingMode == model.BillingModeInputOnly {
			usage.CompletionTokens = 0
			usage.TotalTokens = usage.PromptTokens
		}
	}
	h.logUsage(key, resolved, usage, statusCode, start)
}
