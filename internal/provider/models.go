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

package provider

// 本文件实现上游模型列表查询：管理面「添加供应商」时用凭据里的密钥向厂商
// 拉取可用模型供勾选，避免手工抄写 model_id。出站同样走 SSRF 校验与统一错误分类。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"aigate/internal/protocol"
)

// listModelsTimeout 是查询上游模型列表的超时（列表接口通常很快）。
const listModelsTimeout = 15 * time.Second

// modelsResponse 是上游 /models 的响应体（OpenAI 兼容与 Anthropic 均为 data[].id）。
type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// ListModels 用给定密钥向上游查询可用模型 ID 列表。
//
// url 语义与转发一致：openai-completions 是带 /v1 的 base，anthropic-messages 是 host。
// 返回的错误为 *ConnectivityError，便于调用方复用既有 error_kind 分类。
func ListModels(ctx context.Context, modelType, apiKey, apiBase string) ([]string, error) {
	if strings.TrimSpace(apiBase) == "" {
		return nil, &ConnectivityError{Kind: ErrUpstreamClient, Cause: errors.New("empty upstream url")}
	}
	base := strings.TrimRight(apiBase, "/")
	target := base + "/models"
	auth := func(h http.Header) { h.Set("Authorization", "Bearer "+apiKey) }
	if modelType == protocol.TypeAnthropicMessages {
		target = base + "/v1/models"
		auth = func(h http.Header) {
			h.Set("x-api-key", apiKey)
			h.Set("anthropic-version", anthropicVersion)
		}
	}
	if modelType == protocol.TypeGeminiGenerateContent {
		// url 已含 /v1beta，列表接口就在它的 /models 下。
		auth = func(h http.Header) {
			h.Set("x-goog-api-key", apiKey)
			h.Del("Authorization")
		}
	}

	if err := validateOutboundURL(ctx, target); err != nil {
		return nil, &ConnectivityError{Kind: ErrNetworkUnreachable, Cause: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, &ConnectivityError{Kind: ErrNetworkUnreachable, Cause: err}
	}
	auth(req.Header)
	req.Header.Set("Accept", "application/json")

	resp, err := doUpstream(req, listModelsTimeout)
	if err != nil {
		return nil, &ConnectivityError{Kind: ErrNetworkUnreachable, Cause: err}
	}
	defer resp.Body.Close()

	body, err := ReadUpstreamBody(resp.Body)
	if err != nil {
		return nil, &ConnectivityError{Kind: ErrNetworkUnreachable, Cause: err}
	}
	if kind := classifyHTTPStatus(resp.StatusCode); kind != nil {
		return nil, &ConnectivityError{Kind: kind, StatusCode: resp.StatusCode, Snippet: snippetOf(string(body))}
	}

	if modelType == protocol.TypeGeminiGenerateContent {
		return geminiModelIDs(body, resp.StatusCode)
	}

	var parsed modelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, &ConnectivityError{Kind: ErrUpstreamClient, StatusCode: resp.StatusCode, Snippet: snippetOf(string(body))}
	}
	ids := make([]string, 0, len(parsed.Data))
	for _, item := range parsed.Data {
		if strings.TrimSpace(item.ID) != "" {
			ids = append(ids, item.ID)
		}
	}
	return ids, nil
}

// geminiModelsResponse 是 Gemini 列表接口的响应体：models[].name 而不是 data[].id。
type geminiModelsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// geminiModelIDs 解析 Gemini 模型列表并去掉 models/ 前缀。
func geminiModelIDs(body []byte, status int) ([]string, error) {
	var parsed geminiModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, &ConnectivityError{Kind: ErrUpstreamClient, StatusCode: status, Snippet: snippetOf(string(body))}
	}
	ids := make([]string, 0, len(parsed.Models))
	for _, item := range parsed.Models {
		name := strings.TrimPrefix(strings.TrimSpace(item.Name), "models/")
		if name != "" {
			ids = append(ids, name)
		}
	}
	return ids, nil
}

// probeReplyLimit 是回传给管理面的探测回复长度上限（推理模型可能返回长文本）。
const probeReplyLimit = 80

// truncateReply 截断探测回复，保证响应体紧凑。
func truncateReply(s string) string {
	r := []rune(s)
	if len(r) <= probeReplyLimit {
		return string(r)
	}
	return string(r[:probeReplyLimit]) + "…"
}

// snippetOf 截断上游响应片段，避免把整段 body 塞进错误信息。
func snippetOf(body string) string {
	r := []rune(strings.TrimSpace(body))
	if len(r) <= 200 {
		return string(r)
	}
	return string(r[:200]) + "…"
}
