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

package requestlog

// 本文件实现数据面的边缘采集中间件：只拦推理入口，读请求体前缀后还原完整 body，
// 包装 ResponseWriter 累积响应，最后组装一条 Entry 投递给 Sink。
// 采集与投递失败都不影响数据面响应；契约文件保持零内部依赖，本文件同样只用标准库。

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// MaxCapturedBodyBytes 是单侧（请求或响应）正文的采集上限，与 httpx.MaxBodyBytes 一致。
const MaxCapturedBodyBytes = 1 << 20

// RequestIDHeader 是回给客户端、并与日志行 id 对齐的响应头。
const RequestIDHeader = "X-AIGate-Request-Id"

// Sink 是采集结果的下游出口（由异步 Writer 实现），Submit 必须非阻塞。
type Sink interface {
	Submit(e Entry)
}

// capturedPaths 是唯一会被采集的推理入口（POST + 精确路径），其余一律直通。
var capturedPaths = map[string]struct{}{
	"/v1/chat/completions": {},
	"/v1/messages":         {},
	"/v1/responses":        {},
	"/v1/decisions":        {},
	"/decisions":           {},
	"/v1/systemone":        {},
	"/systemone":           {},
}

// Middleware 包装 next：命中推理入口时采集一次请求/响应并投递给 sink，否则原样直通。
func Middleware(next http.Handler, sink Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !shouldCapture(r) {
			next.ServeHTTP(w, r)
			return
		}

		// 请求 ID 在调用 next 之前写入响应头，保证客户端能拿到并与日志行对齐。
		id := newRequestID()
		w.Header().Set(RequestIDHeader, id)

		// 身份只取哈希：与数据面一致，x-api-key 优先、其次 Authorization: Bearer。
		// 请求头在进入 next 之前取，代表边缘看到的原始请求。
		apiKeyHash := credentialHash(r)
		requestHeaders := filterHeaders(r.Header)

		// 读请求体前缀后必须还原完整 body：prefix + 未读的 r.Body 拼回原始字节流，
		// 这样超过 1 MiB 的请求仍会被 httpx.ReadBody 判为 413，语义不变。
		original := r.Body
		prefix, err := io.ReadAll(io.LimitReader(original, MaxCapturedBodyBytes+1))
		if err != nil {
			slog.Warn("request log: read request body prefix failed", "error", err)
		}
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(prefix), original))

		model, stream := parseBodyMeta(prefix)

		cw := &captureWriter{w: w, statusCode: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(cw, r)
		latencyMS := int(time.Since(start).Milliseconds())

		requestBody := prefix
		truncated := cw.truncated
		if len(prefix) > MaxCapturedBodyBytes {
			requestBody = prefix[:MaxCapturedBodyBytes]
			truncated = true
		}

		e := Entry{
			ID:              id,
			CreatedAt:       time.Now().UTC(),
			Method:          r.Method,
			Path:            r.URL.Path,
			Model:           model,
			APIKeyHash:      apiKeyHash,
			StatusCode:      cw.statusCode,
			LatencyMS:       latencyMS,
			Stream:          stream,
			RequestBody:     string(requestBody),
			ResponseBody:    cw.buf.String(),
			RequestHeaders:  requestHeaders,
			ResponseHeaders: filterHeaders(cw.Header()),
			Error:           statusClass(cw.statusCode),
			Truncated:       truncated,
		}
		// Submit 由契约保证非阻塞；同步调用使投递在 ServeHTTP 返回前完成，不等待落库。
		sink.Submit(e)
	})
}

// shouldCapture 判断是否为「POST + 七条推理入口」。
func shouldCapture(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	_, ok := capturedPaths[r.URL.Path]
	return ok
}

// newRequestID 生成 16 随机字节的 32 位十六进制请求 ID。
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 在 Go 1.24 下实际不会返回错误；万一发生仍返回合法 32 位 hex。
		slog.Warn("request log: generate request id failed", "error", err)
	}
	return hex.EncodeToString(b[:])
}

// credentialHash 按数据面取键顺序解析凭据，只返回 hex(sha256(token))，缺失时为 ""。
// 该值与 model.HashToken / api_keys.proxy_token_hash 完全一致，但本包不 import internal/model。
func credentialHash(r *http.Request) string {
	token := strings.TrimSpace(r.Header.Get("x-api-key"))
	if token == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// headerAllowed 实现采集头白名单：仅允许四类固定头与 X-AIGate-*（响应侧网关自加的头）。
// 任何 *-key / *-token 头一律丢弃（优先于前缀白名单），其余（凭据头、框架头等）自然排除。
func headerAllowed(key string) bool {
	k := strings.ToLower(key)
	if strings.HasSuffix(k, "-key") || strings.HasSuffix(k, "-token") {
		return false
	}
	switch k {
	case "content-type", "accept", "user-agent", "x-request-id":
		return true
	default:
		return strings.HasPrefix(k, "x-aigate-")
	}
}

// filterHeaders 把白名单头序列化为 JSON 对象字符串；无命中时为 "{}"。
func filterHeaders(h http.Header) string {
	kept := make(http.Header, len(h))
	for k, v := range h {
		if headerAllowed(k) {
			kept[k] = v
		}
	}
	b, err := json.Marshal(kept)
	if err != nil {
		// map[string][]string 的 json.Marshal 不会失败；保底返回空对象，不影响数据面。
		slog.Warn("request log: marshal headers failed", "error", err)
		return "{}"
	}
	return string(b)
}

// parseBodyMeta 尽力解析请求体中的 model / stream 字段，失败时返回零值。
func parseBodyMeta(body []byte) (model string, stream bool) {
	var v struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", false
	}
	return strings.TrimSpace(v.Model), v.Stream
}

// statusClass 把 >=400 的状态码归类为一段英文状态文本；成功请求返回空串。
func statusClass(code int) string {
	if code < http.StatusBadRequest {
		return ""
	}
	if text := http.StatusText(code); text != "" {
		return text
	}
	// 非标准状态码兜底，保证 error 非空。
	if code < http.StatusInternalServerError {
		return "client error"
	}
	return "server error"
}

// captureWriter 包装 ResponseWriter：透传语义的同时累积响应正文到上限。
type captureWriter struct {
	w           http.ResponseWriter
	statusCode  int
	wroteHeader bool
	buf         bytes.Buffer
	truncated   bool
}

func (cw *captureWriter) Header() http.Header { return cw.w.Header() }

func (cw *captureWriter) WriteHeader(code int) {
	if cw.wroteHeader {
		return
	}
	cw.wroteHeader = true
	cw.statusCode = code
	cw.w.WriteHeader(code)
}

func (cw *captureWriter) Write(p []byte) (int, error) {
	if !cw.wroteHeader {
		cw.WriteHeader(http.StatusOK)
	}
	// 先累积 handler 试图写出的字节；底层 Write 的错误原样透传给调用方，
	// 但既不 panic 也不阻止之后组装并投递 Entry。
	cw.accumulate(p)
	return cw.w.Write(p)
}

// Flush 透传 http.Flusher 语义给底层（流式 SSE 需要）。
func (cw *captureWriter) Flush() {
	if f, ok := cw.w.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 供 http.ResponseController 取得底层 ResponseWriter。
func (cw *captureWriter) Unwrap() http.ResponseWriter { return cw.w }

// accumulate 累积到上限后只置 truncated、不再追加；空写入不改变状态。
func (cw *captureWriter) accumulate(p []byte) {
	if cw.truncated || len(p) == 0 {
		return
	}
	remaining := MaxCapturedBodyBytes - cw.buf.Len()
	if len(p) > remaining {
		cw.buf.Write(p[:remaining])
		cw.truncated = true
		return
	}
	cw.buf.Write(p)
}
