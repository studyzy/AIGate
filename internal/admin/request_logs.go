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
	"errors"
	"net/http"
	"time"

	"aigate/internal/httpx"
	"aigate/internal/model"
	"aigate/internal/queryparams"
	"aigate/internal/requestlog"
)

// requestLogRow 是请求日志列表行的对外表示（显式 snake_case 字段名，不含正文与头列）。
// requestlog.Row 是插件契约里的普通结构体、没有 json tag，故此处单独定义 DTO 逐一映射（R26）。
type requestLogRow struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Model      string    `json:"model"`
	APIKeyHash string    `json:"api_key_hash"`
	StatusCode int       `json:"status_code"`
	LatencyMS  int       `json:"latency_ms"`
	Stream     bool      `json:"stream"`
	Error      string    `json:"error"`
	Truncated  bool      `json:"truncated"`
}

// requestLogDetail 在列表行之上补出两个正文列与两个头列，供详情接口返回完整 Entry。
type requestLogDetail struct {
	requestLogRow
	RequestBody     string `json:"request_body"`
	ResponseBody    string `json:"response_body"`
	RequestHeaders  string `json:"request_headers"`
	ResponseHeaders string `json:"response_headers"`
}

// requestLogListResponse 是列表接口的响应体；Logs 恒为非 nil 切片，空结果序列化为 []。
type requestLogListResponse struct {
	Logs     []requestLogRow `json:"logs"`
	Total    int64           `json:"total"`
	Page     int             `json:"page"`
	PageSize int             `json:"page_size"`
}

// toRequestLogRow 把插件契约行映射为对外 DTO。
func toRequestLogRow(r *requestlog.Row) requestLogRow {
	return requestLogRow{
		ID:         r.ID,
		CreatedAt:  r.CreatedAt,
		Method:     r.Method,
		Path:       r.Path,
		Model:      r.Model,
		APIKeyHash: r.APIKeyHash,
		StatusCode: r.StatusCode,
		LatencyMS:  r.LatencyMS,
		Stream:     r.Stream,
		Error:      r.Error,
		Truncated:  r.Truncated,
	}
}

// toRequestLogDetail 把完整 Entry 映射为对外 DTO（含正文与头）。
func toRequestLogDetail(e *requestlog.Entry) requestLogDetail {
	return requestLogDetail{
		requestLogRow: requestLogRow{
			ID:         e.ID,
			CreatedAt:  e.CreatedAt,
			Method:     e.Method,
			Path:       e.Path,
			Model:      e.Model,
			APIKeyHash: e.APIKeyHash,
			StatusCode: e.StatusCode,
			LatencyMS:  e.LatencyMS,
			Stream:     e.Stream,
			Error:      e.Error,
			Truncated:  e.Truncated,
		},
		RequestBody:     e.RequestBody,
		ResponseBody:    e.ResponseBody,
		RequestHeaders:  e.RequestHeaders,
		ResponseHeaders: e.ResponseHeaders,
	}
}

// handleRequestLogs 实现 `GET /admin/request-logs`：解析过滤参数 → 翻译 api_key_id → 查询 → 组装列表响应。
func handleRequestLogs(store *model.Store, logs requestlog.Reader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := queryparams.ParseRequestLogFilter(r)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		q := requestlog.Query{
			Start:        f.Start,
			End:          f.End,
			APIKeyHash:   f.APIKeyHash,
			Model:        f.Model,
			Path:         f.Path,
			StatusCode:   f.StatusCode,
			Stream:       f.Stream,
			MinLatencyMS: f.MinLatencyMS,
			MaxLatencyMS: f.MaxLatencyMS,
			Q:            f.Q,
			Page:         f.Page,
			PageSize:     f.PageSize,
		}
		// api_key_id 优先：经 model 层把密钥 ID 翻译成采集时的 proxy_token_hash；
		// 与 api_key_hash 同时给出时以 id 的翻译结果为准。
		if f.APIKeyID > 0 {
			key, err := store.GetAPIKeyByID(r.Context(), f.APIKeyID)
			if err != nil {
				if model.IsNotFound(err) {
					httpx.WriteError(w, http.StatusNotFound, "api key not found")
					return
				}
				writeInternalError(w, err)
				return
			}
			q.APIKeyHash = key.ProxyTokenHash
		}
		page, err := logs.Search(r.Context(), q)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		rows := make([]requestLogRow, 0, len(page.Rows))
		for i := range page.Rows {
			rows = append(rows, toRequestLogRow(&page.Rows[i]))
		}
		// 分页回显取解析后已归一化的值（与 handleUsageLogs 一致），不依赖 Reader 是否填写 Page.Page/PageSize。
		httpx.WriteJSON(w, http.StatusOK, requestLogListResponse{
			Logs:     rows,
			Total:    page.Total,
			Page:     f.Page,
			PageSize: f.PageSize,
		})
	}
}

// handleRequestLogDetail 实现 `GET /admin/request-logs/{id}`：返回完整 Entry（含正文与白名单头）。
func handleRequestLogDetail(logs requestlog.Reader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			httpx.WriteError(w, http.StatusBadRequest, "empty request log id")
			return
		}
		e, err := logs.Get(r.Context(), id)
		if err != nil {
			if errors.Is(err, requestlog.ErrNotFound) {
				httpx.WriteError(w, http.StatusNotFound, "request log not found")
				return
			}
			writeInternalError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, toRequestLogDetail(e))
	}
}
