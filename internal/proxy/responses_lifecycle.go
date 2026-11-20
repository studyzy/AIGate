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

// 本文件实现 Responses 的三个生命周期端点（design §5.6）：
//
//	GET    /v1/responses/{id}         点查当前 response_body
//	DELETE /v1/responses/{id}         删除**已终态**的行，返回 {"id","object","deleted":true}
//	POST   /v1/responses/{id}/cancel  把 queued / in_progress 置为 cancelled
//
// 三条共同的规矩：
//  1. 先鉴权（Bearer），再查行；
//  2. 归属只看 owner_user_id：失配与「行不存在」返回**同一个** 404，状态码与响应体逐字节一致
//     （msgResponsesNotFound），不给存在性枚举留缝隙；
//  3. 行状态由后台任务与保留期清理并发改写，因此状态判断一律在读到的行上做，写回用条件更新。

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"aigate/internal/httpx"
	"aigate/internal/model"
)

// 生命周期端点的错误文案（恒英文）。
const (
	// msgResponsesDeleteInFlight：在途（queued / in_progress）行不允许删除，要先 cancel。
	msgResponsesDeleteInFlight = "response is still running, cancel it before deleting"
	// msgResponsesCancelFinished：终态行不可取消，响应里带回当前对象供客户端取结果。
	msgResponsesCancelFinished = "response already finished and cannot be cancelled"
)

// responsesDeletedObject 是删除响应的 object 取值（OpenAI 口径）。
const responsesDeletedObject = "response.deleted"

// handleGetResponse 处理 GET /v1/responses/{id}。
func (h *Handler) handleGetResponse(w http.ResponseWriter, r *http.Request) {
	key := h.authKey(w, r)
	if key == nil {
		return
	}
	row, ok := h.loadOwnedResponseState(w, r, key)
	if !ok {
		return
	}
	body := row.ResponseBody
	if len(body) == 0 {
		body = []byte("{}")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleDeleteResponse 处理 DELETE /v1/responses/{id}：只允许删已终态的行。
//
// 为什么在途行不能删：删除会让正在运行的后台任务在下一次写回时命中 0 行并静默返回 nil
// （存储层契约），运维既看不到结果也看不到失败原因；先 cancel 再删，才能保证
// 「删掉的行一定有明确终态」。
func (h *Handler) handleDeleteResponse(w http.ResponseWriter, r *http.Request) {
	key := h.authKey(w, r)
	if key == nil {
		return
	}
	row, ok := h.loadOwnedResponseState(w, r, key)
	if !ok {
		return
	}
	if !isTerminalResponseStatus(row.Status) {
		httpx.WriteError(w, http.StatusBadRequest, msgResponsesDeleteInFlight)
		return
	}
	err := h.store.DeleteResponseState(r.Context(), row.ID)
	switch {
	case err == nil:
	case errors.Is(err, model.ErrResponseStateNotFound):
		// 读到与删除之间被取消了（或已被清理）：与「不存在」同一个 404。
		httpx.WriteError(w, http.StatusNotFound, msgResponsesNotFound)
		return
	default:
		slog.Error("delete response state failed", "error", err, "response_id", row.ID)
		httpx.WriteError(w, http.StatusInternalServerError, msgInternalError)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id":      row.ID,
		"object":  responsesDeletedObject,
		"deleted": true,
	})
}

// handleCancelResponse 处理 POST /v1/responses/{id}/cancel。
//
// 契约（规格 §5.6）：
//   - queued / in_progress → 置 cancelled，返回取消后的响应对象（200）；
//   - 已是终态 → 400，并附当前响应对象（客户端据此拿到结果或失败原因）；
//   - 行不存在 / 不属于本用户 → 404（与不存在逐字节一致）。
//
// 取消只改状态，不改 response_body：后台任务若在取消前刚好写完终态，那个终态已被条件更新
// 挡下（不会覆盖 cancelled），但 GET 仍会返回它——这正是「取消与完成竞争时以先到者为准」：
// 结果已经产生，取消只表示「不再等待」。
func (h *Handler) handleCancelResponse(w http.ResponseWriter, r *http.Request) {
	key := h.authKey(w, r)
	if key == nil {
		return
	}
	row, ok := h.loadOwnedResponseState(w, r, key)
	if !ok {
		return
	}
	cancelled, current, err := h.cancelResponseState(r.Context(), row.ID)
	switch {
	case err == nil:
	case errors.Is(err, model.ErrResponseStateNotFound):
		httpx.WriteError(w, http.StatusNotFound, msgResponsesNotFound)
		return
	default:
		slog.Error("cancel response state failed", "error", err, "response_id", row.ID)
		httpx.WriteError(w, http.StatusInternalServerError, msgInternalError)
		return
	}
	if !cancelled {
		httpx.WriteJSON(w, http.StatusBadRequest, cancelConflictBody(current, msgResponsesCancelFinished))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, cancelResponseObject(current, row))
}

// loadOwnedResponseState 是三个端点共用的「取路径 id → 查行 → 校验归属」。
// 失败时已写出 404/500（归属失配与不存在同文同码）并返回 ok=false。
func (h *Handler) loadOwnedResponseState(
	w http.ResponseWriter,
	r *http.Request,
	key *model.APIKey,
) (*model.ResponseState, bool) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		httpx.WriteError(w, http.StatusNotFound, msgResponsesNotFound)
		return nil, false
	}
	row, err := h.store.GetResponseState(r.Context(), id)
	switch {
	case err == nil:
	case errors.Is(err, model.ErrResponseStateNotFound):
		httpx.WriteError(w, http.StatusNotFound, msgResponsesNotFound)
		return nil, false
	default:
		slog.Error("load response state failed", "error", err, "response_id", id)
		httpx.WriteError(w, http.StatusInternalServerError, msgInternalError)
		return nil, false
	}
	// 归属失配走与「不存在」相同的出口；日志不记录该行属于谁（避免运维侧出现影子信息）。
	if row.OwnerUserID != key.UserID {
		slog.Warn("response state owner mismatch", "response_id", row.ID, "api_key_id", key.ID,
			"endpoint", r.Method+" "+r.URL.Path)
		httpx.WriteError(w, http.StatusNotFound, msgResponsesNotFound)
		return nil, false
	}
	return row, true
}

// isTerminalResponseStatus 报告状态是否已终态（不可再取消、可以删除）。
func isTerminalResponseStatus(status string) bool {
	switch status {
	case model.ResponseStateStatusCompleted, model.ResponseStateStatusFailed,
		model.ResponseStateStatusIncomplete, model.ResponseStateStatusCancelled:
		return true
	default:
		return false
	}
}

// cancelResponseObject 构造取消成功的响应体：以当前响应体为基础，把 status 改写为 cancelled。
//
// 行里此刻的 response_body 通常是 queued（后台任务尚未写回终态），但兼容 "{}" 与任何
// 部分载荷：保留已有字段、只改写 status，这样 POST /cancel 的回包与随后的 GET 一致。
func cancelResponseObject(current, fallback *model.ResponseState) map[string]any {
	row := current
	if row == nil {
		row = fallback
	}
	out := map[string]any{}
	if len(row.ResponseBody) > 0 {
		if err := decodeJSONObject(row.ResponseBody, &out); err != nil {
			out = map[string]any{}
		}
	}
	out["id"] = row.ID
	out["object"] = respObjectResponse
	out["status"] = model.ResponseStateStatusCancelled
	if _, exists := out["output"]; !exists {
		out["output"] = []any{}
	}
	// 取消的行一定是后台方式创建的行，store 恒为 true。
	out["store"] = true
	if row.RequestModel != "" {
		out["model"] = row.RequestModel
	}
	return out
}

// cancelConflictBody 构造「终态不可取消」的 400 响应体：错误信封 + 当前响应对象。
// 规格要求「返回当前对象或 400」——这里两者都给：错误说明为什么拒绝，response 字段让客户端不必
// 再发一次 GET 就能拿到结果。
func cancelConflictBody(row *model.ResponseState, message string) map[string]any {
	body := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "invalid_request_error",
		},
	}
	if row == nil {
		return body
	}
	current := map[string]any{}
	if len(row.ResponseBody) > 0 {
		_ = decodeJSONObject(row.ResponseBody, &current)
	}
	current["id"] = row.ID
	current["object"] = respObjectResponse
	if _, exists := current["status"]; !exists {
		current["status"] = row.Status
	}
	body["response"] = current
	return body
}

// decodeJSONObject 把 JSON 字节解码进 map；失败返回错误（调用方按空对象处理）。
func decodeJSONObject(raw []byte, dst *map[string]any) error {
	if len(raw) == 0 {
		return errResponsesInvalidBody
	}
	return json.Unmarshal(raw, dst)
}
