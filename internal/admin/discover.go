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

// 本文件实现「拉取上游模型列表」：管理面添加供应商时，用凭据密钥向上游查询
// 可用模型供用户勾选，免去手工抄写 model_id。

import (
	"net/http"
	"strings"

	"aigate/internal/httpx"
	"aigate/internal/model"
	"aigate/internal/provider"
)

// discoverRequest 是探测请求：api_key 优先（尚未落库的新凭据），否则用 credential_id 解析。
type discoverRequest struct {
	ModelType    string `json:"model_type"`
	URL          string `json:"url"`
	CredentialID uint   `json:"credential_id"`
	APIKey       string `json:"api_key"`
}

// discoverResponse 与连通性探测同口径：一律 200，成功/失败体现在 ok 字段。
type discoverResponse struct {
	OK        bool     `json:"ok"`
	Models    []string `json:"models,omitempty"`
	ErrorKind string   `json:"error_kind,omitempty"`
	Message   string   `json:"message,omitempty"`
}

// handleDiscoverModels 实现 `POST /admin/models/discover`：列出上游可用模型 ID。
func handleDiscoverModels(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req discoverRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !validModelType(req.ModelType) {
			httpx.WriteError(w, http.StatusBadRequest, "model_type must be openai-completions, anthropic-messages or openai-responses")
			return
		}
		if strings.TrimSpace(req.URL) == "" {
			httpx.WriteError(w, http.StatusBadRequest, "url is required")
			return
		}

		apiKey := strings.TrimSpace(req.APIKey)
		if apiKey == "" {
			if req.CredentialID == 0 {
				httpx.WriteError(w, http.StatusBadRequest, "api_key or credential_id is required")
				return
			}
			cred, err := store.GetCredentialByID(r.Context(), req.CredentialID)
			if err != nil {
				writeLookupError(w, err, "credential")
				return
			}
			secret, err := cred.FirstHeaderSecretValue()
			if err != nil {
				writeInternalError(w, err)
				return
			}
			if secret == "" {
				httpx.WriteError(w, http.StatusBadRequest, "credential has no header secret to use")
				return
			}
			apiKey = secret
		}

		ids, err := provider.ListModels(r.Context(), req.ModelType, apiKey, req.URL)
		if err != nil {
			httpx.WriteJSON(w, http.StatusOK, discoverResponse{
				OK:        false,
				ErrorKind: classifyConnectivityError(err),
				Message:   err.Error(),
			})
			return
		}
		if ids == nil {
			ids = []string{}
		}
		httpx.WriteJSON(w, http.StatusOK, discoverResponse{OK: true, Models: ids})
	}
}
