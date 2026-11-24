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

package proxy_test

// 本文件验证 gemini-generate-content 上游与三个数据面入口的端到端接线。
// 断言的是**网关发给 Gemini 的形状**，而不是客户端拿到什么：只看客户端响应的
// 测试会漏掉「签名漏给上游」这类错误（spec §7）。

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"aigate/internal/model"
)

// geminiEnv 起一个网关 + 一个假 Gemini 上游。
type geminiEnv struct {
	*env
	modelType string
	// token 是该模型数据面密钥的明文。
	token string
}

// startGeminiEnv 创建 gemini-generate-content 模型并绑定密钥。
// replies 按调用次序返回（用尽后重复最后一个）。
func startGeminiEnv(t *testing.T, replies ...string) *geminiEnv {
	t.Helper()
	var mu sync.Mutex
	idx := 0
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reply := replies[len(replies)-1]
		if idx < len(replies) {
			reply = replies[idx]
		}
		idx++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	})

	m := newAIModel("gemini-2.5-flash", e.upstream.URL+"/v1beta")
	m.ModelType = model.ModelTypeGemini
	mustCreateAIModel(t, e.store, m)
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	return &geminiEnv{env: e, modelType: model.ModelTypeGemini, token: token}
}

// post 向指定路径发一次请求。
func (g *geminiEnv) post(t *testing.T, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, g.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// readAll 读取并关闭响应体。
func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// contents 返回上游收到请求的 contents 数组。
func contentsOf(t *testing.T, body map[string]any) []any {
	t.Helper()
	c, ok := body["contents"].([]any)
	if !ok {
		t.Fatalf("上游请求缺 contents: %v", body)
	}
	return c
}

// partsOf 返回第 i 条轮次的 parts。
func partsOf(t *testing.T, body map[string]any, i int) []any {
	t.Helper()
	c := contentsOf(t, body)
	if i >= len(c) {
		t.Fatalf("contents 只有 %d 条，要取第 %d 条", len(c), i)
	}
	return c[i].(map[string]any)["parts"].([]any)
}

const geminiHello = `{"responseId":"r1","candidates":[{"content":{"role":"model",` +
	`"parts":[{"text":"hello"}]},"finishReason":"STOP"}],` +
	`"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`

// 1. chat 入口纯文本往返。
func TestGemini_ChatCompletions_TextRoundTrip(t *testing.T) {
	g := startGeminiEnv(t, geminiHello)
	resp := g.post(t, "/v1/chat/completions",
		`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "hello") {
		t.Fatalf("客户端未拿到 hello: %s", body)
	}
	parts := partsOf(t, g.rec.Body(), 0)
	if parts[0].(map[string]any)["text"] != "hi" {
		t.Fatalf("上游 contents[0].parts[0] = %v", parts[0])
	}
	if got := g.rec.Header("x-goog-api-key"); got != "upstream-secret" {
		t.Fatalf("x-goog-api-key = %q", got)
	}
}

// 2. chat 入口流式以 [DONE] 结束。
func TestGemini_ChatCompletions_Stream(t *testing.T) {
	g := startGeminiEnv(t,
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"STOP\"}]}\n\n")
	resp := g.post(t, "/v1/chat/completions",
		`{"model":"gemini-2.5-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("流应以 [DONE] 结束: %s", body)
	}
	if !strings.Contains(body, `"content":"hi"`) {
		t.Fatalf("流里应含正文分片: %s", body)
	}
}

// 3. Anthropic 入口落到 Gemini 上游，system 进 systemInstruction。
func TestGemini_AnthropicIngress_ConvertsToGemini(t *testing.T) {
	g := startGeminiEnv(t, geminiHello)
	resp := g.post(t, "/v1/messages", `{"model":"gemini-2.5-flash","max_tokens":64,
		"system":"be brief","messages":[{"role":"user","content":"hi"}]}`)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	up := g.rec.Body()
	si, ok := up["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("缺 systemInstruction: %v", up)
	}
	if si["parts"].([]any)[0].(map[string]any)["text"] != "be brief" {
		t.Fatalf("systemInstruction = %v", si)
	}
}

// 4. Responses 入口落到 Gemini 上游。
func TestGemini_ResponsesIngress_ConvertsToGemini(t *testing.T) {
	g := startGeminiEnv(t, geminiHello)
	resp := g.post(t, "/v1/responses",
		`{"model":"gemini-2.5-flash","input":"hi"}`)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	parts := partsOf(t, g.rec.Body(), 0)
	if parts[0].(map[string]any)["text"] != "hi" {
		t.Fatalf("上游 contents[0].parts[0] = %v", parts[0])
	}
}

// 5. 工具调用往返：签名进 id 回到客户端，再发上游时必须剥成 base id。
func TestGemini_ToolCallRoundTrip_StripsSignature(t *testing.T) {
	const toolReply = `{"responseId":"r1","candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"id":"call_1","name":"get_weather","args":{"city":"A"}},
		 "thoughtSignature":"AQID"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`
	g := startGeminiEnv(t, toolReply, geminiHello)

	resp := g.post(t, "/v1/chat/completions", `{"model":"gemini-2.5-flash",
		"messages":[{"role":"user","content":"weather?"}],
		"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "_ts_") {
		t.Fatalf("客户端拿到的 tool call id 应含签名后缀: %s", body)
	}

	var first struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &first); err != nil {
		t.Fatalf("解析首次响应: %v", err)
	}
	callID := first.Choices[0].Message.ToolCalls[0].ID

	// 客户端原样回传 id 与工具结果。
	second := `{"model":"gemini-2.5-flash","messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"` + callID + `","type":"function",
		 "function":{"name":"get_weather","arguments":"{\"city\":\"A\"}"}}]},
		{"role":"tool","tool_call_id":"` + callID + `","content":"{\"temp\":21}"}],
		"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`
	resp2 := g.post(t, "/v1/chat/completions", second)
	_ = readAll(t, resp2)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("第二次 status = %d", resp2.StatusCode)
	}

	up := g.rec.Body()
	var found bool
	for _, raw := range contentsOf(t, up) {
		for _, p := range raw.(map[string]any)["parts"].([]any) {
			pm := p.(map[string]any)
			fc, ok := pm["functionCall"].(map[string]any)
			if !ok {
				continue
			}
			found = true
			if id, _ := fc["id"].(string); strings.Contains(id, "_ts_") {
				t.Fatalf("functionCall.id 不能带签名发上游: %q", id)
			}
			if fc["id"] != "call_1" {
				t.Fatalf("functionCall.id = %v, want call_1", fc["id"])
			}
			if pm["thoughtSignature"] != "AQID" {
				t.Fatalf("thoughtSignature = %v, want AQID", pm["thoughtSignature"])
			}
		}
	}
	if !found {
		t.Fatalf("上游请求里没有 functionCall: %v", up)
	}
}

// 6. Responses 入口的签名已知降级为哨兵（spec §5.2.4）。
func TestGemini_ResponsesIngress_MissingSignatureUsesSentinel(t *testing.T) {
	g := startGeminiEnv(t, geminiHello)
	resp := g.post(t, "/v1/responses", `{"model":"gemini-2.5-flash","input":[
		{"type":"message","role":"user","content":"weather?"},
		{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"{}"}],
		"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`)
	_ = readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	up := g.rec.Body()
	var found bool
	for _, raw := range contentsOf(t, up) {
		for _, p := range raw.(map[string]any)["parts"].([]any) {
			pm := p.(map[string]any)
			if _, ok := pm["functionCall"]; ok {
				found = true
				if pm["thoughtSignature"] != "skip_thought_signature_validator" {
					t.Fatalf("thoughtSignature = %v, want 哨兵", pm["thoughtSignature"])
				}
			}
		}
	}
	if !found {
		t.Fatalf("上游请求里没有 functionCall: %v", up)
	}
}

// 7. https 图片走 fileData，且网关从不向图片地址发起连接。
func TestGemini_HTTPSImage_NeverFetched(t *testing.T) {
	// 真实 listener：若网关真去下载，这里会数到连接。
	var conns int32
	var mu sync.Mutex
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conns++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(imgSrv.Close)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if conns != 0 {
			t.Fatalf("网关向图片地址发起了 %d 次连接，违规", conns)
		}
	})

	imgURL := strings.Replace(imgSrv.URL, "http://", "https://", 1) + "/cat.png"
	g := startGeminiEnv(t, geminiHello)
	resp := g.post(t, "/v1/chat/completions", `{"model":"gemini-2.5-flash","messages":[
		{"role":"user","content":[
			{"type":"text","text":"what is this"},
			{"type":"image_url","image_url":{"url":"`+imgURL+`"}}]}]}`)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var fileURI string
	for _, p := range partsOf(t, g.rec.Body(), 0) {
		if fd, ok := p.(map[string]any)["fileData"].(map[string]any); ok {
			fileURI, _ = fd["fileUri"].(string)
			if fd["mimeType"] != "image/png" {
				t.Fatalf("mimeType = %v, want image/png", fd["mimeType"])
			}
		}
	}
	if fileURI != imgURL {
		t.Fatalf("fileUri = %q, want %q", fileURI, imgURL)
	}
}

// 8. n>1 直接 400。
func TestGemini_NGreaterThanOneRejected(t *testing.T) {
	g := startGeminiEnv(t, geminiHello)
	resp := g.post(t, "/v1/chat/completions",
		`{"model":"gemini-2.5-flash","n":2,"messages":[{"role":"user","content":"hi"}]}`)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", resp.StatusCode, body)
	}
	if g.rec.Calls() != 0 {
		t.Fatalf("被拒绝的请求不应到达上游，calls = %d", g.rec.Calls())
	}
}

// 9. Anthropic 入口的签名保真：tool_use.id 带回签名，第二次请求必须剥成 base id。
func TestGemini_AnthropicIngress_ToolCallStripsSignature(t *testing.T) {
	const toolReply = `{"responseId":"r1","candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"id":"call_1","name":"get_weather","args":{"city":"A"}},
		 "thoughtSignature":"AQID"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`
	g := startGeminiEnv(t, toolReply, geminiHello)

	resp := g.post(t, "/v1/messages", `{"model":"gemini-2.5-flash","max_tokens":64,
		"messages":[{"role":"user","content":"weather?"}],
		"tools":[{"name":"get_weather","input_schema":{"type":"object"}}]}`)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "_ts_") {
		t.Fatalf("tool_use.id 应含签名后缀: %s", body)
	}
	var first struct {
		Content []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &first); err != nil {
		t.Fatalf("解析首次响应: %v", err)
	}
	var callID string
	for _, blk := range first.Content {
		if blk.Type == "tool_use" {
			callID = blk.ID
		}
	}
	if callID == "" {
		t.Fatalf("响应里没有 tool_use: %s", body)
	}

	second := `{"model":"gemini-2.5-flash","max_tokens":64,
		"messages":[{"role":"user","content":"weather?"},
		{"role":"assistant","content":[{"type":"tool_use","id":"` + callID + `","name":"get_weather","input":{"city":"A"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + callID + `","content":"{\"temp\":21}"}]}],
		"tools":[{"name":"get_weather","input_schema":{"type":"object"}}]}`
	resp2 := g.post(t, "/v1/messages", second)
	_ = readAll(t, resp2)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("第二次 status = %d", resp2.StatusCode)
	}
	up := g.rec.Body()
	var found bool
	for _, raw := range contentsOf(t, up) {
		for _, p := range raw.(map[string]any)["parts"].([]any) {
			pm := p.(map[string]any)
			fc, ok := pm["functionCall"].(map[string]any)
			if !ok {
				continue
			}
			found = true
			if fc["id"] != "call_1" {
				t.Fatalf("functionCall.id = %v, want call_1（签名必须剥离）", fc["id"])
			}
			if pm["thoughtSignature"] != "AQID" {
				t.Fatalf("thoughtSignature = %v, want AQID", pm["thoughtSignature"])
			}
		}
	}
	if !found {
		t.Fatalf("上游请求里没有 functionCall: %v", up)
	}
}

// 10. Responses 入口流式：SSE 以完成事件收尾。
func TestGemini_ResponsesIngress_Stream(t *testing.T) {
	g := startGeminiEnv(t,
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"STOP\"}]}\n\n")
	resp := g.post(t, "/v1/responses",
		`{"model":"gemini-2.5-flash","stream":true,"input":"hi"}`)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "response.completed") {
		t.Fatalf("Responses 流应以 response.completed 收尾: %s", body)
	}
}

// 11. Anthropic 入口流式：带 message_stop 收尾。
func TestGemini_AnthropicIngress_Stream(t *testing.T) {
	g := startGeminiEnv(t,
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"STOP\"}]}\n\n")
	resp := g.post(t, "/v1/messages",
		`{"model":"gemini-2.5-flash","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("Anthropic 流应以 message_stop 收尾: %s", body)
	}
}

// 说明：Anthropic 与 Responses 入口的转换器都不保留 `n`（两种协议本身没有该字段），
// 因此 n>1 的拒绝只在 /v1/chat/completions 上可观测，由
// TestGemini_NGreaterThanOneRejected 覆盖。

var _ = net.Dial // 保留 net 导入以便将来扩展连接断言
