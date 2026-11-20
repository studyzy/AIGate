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

// 白盒单测：Responses ↔ Chat Completions 协议转换（responses_convert.go）。
// 断言以「字段值 / 结构 / 事件序列」为准，不逐字节比对 golden。

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 通用辅助
// ---------------------------------------------------------------------------

// respConvMapFromBytes 解析 JSON 字节为 map（并断言可解析）。
func respConvMapFromBytes(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", string(b), err)
	}
	return m
}

// respConvRequest 执行请求转换并断言成功。
func respConvRequest(t *testing.T, body string) map[string]any {
	t.Helper()
	out, err := responsesRequestToChat([]byte(body))
	if err != nil {
		t.Fatalf("responsesRequestToChat(%s) returned an error: %v", body, err)
	}
	return out
}

// respConvMessages 取转换后的 messages 数组。
func respConvMessages(t *testing.T, out map[string]any) []any {
	t.Helper()
	msgs, ok := out["messages"].([]any)
	if !ok {
		t.Fatalf("messages = %#v, want an array", out["messages"])
	}
	return msgs
}

// respConvMessageAt 取第 i 条 message（并断言是对象）。
func respConvMessageAt(t *testing.T, msgs []any, i int) map[string]any {
	t.Helper()
	if i >= len(msgs) {
		t.Fatalf("messages has %d entries, want index %d", len(msgs), i)
	}
	m, ok := msgs[i].(map[string]any)
	if !ok {
		t.Fatalf("messages[%d] = %#v, want an object", i, msgs[i])
	}
	return m
}

// respConvFloatEqual 比较期望的整数值与 JSON 解码后的数值。
func respConvFloatEqual(t *testing.T, got any, want float64) {
	t.Helper()
	n, ok := got.(float64)
	if !ok {
		t.Fatalf("value = %#v, want the number %v", got, want)
	}
	if n != want {
		t.Fatalf("value = %v, want %v", n, want)
	}
}

// ---------------------------------------------------------------------------
// 请求方向：responses → chat
// ---------------------------------------------------------------------------

// TestResponsesRequestToChat_InputString 断言 input 字符串 → 单条 user 文本消息。
func TestResponsesRequestToChat_InputString(t *testing.T) {
	out := respConvRequest(t, `{"model":"m","input":"hi"}`)

	if out["model"] != "m" {
		t.Fatalf("model = %#v, want \"m\"", out["model"])
	}
	msgs := respConvMessages(t, out)
	if len(msgs) != 1 {
		t.Fatalf("messages = %#v, want exactly 1 entry", msgs)
	}
	msg := respConvMessageAt(t, msgs, 0)
	if msg["role"] != "user" {
		t.Fatalf("messages[0].role = %#v, want \"user\"", msg["role"])
	}
	if msg["content"] != "hi" {
		t.Fatalf("messages[0].content = %#v, want \"hi\"", msg["content"])
	}
	// input 本体不得继续出现在 chat 请求里（上游不认识）。
	if _, exists := out["input"]; exists {
		t.Fatalf("converted request still carries the responses-only field \"input\"")
	}
}

// TestResponsesRequestToChat_ItemsAndInstructions 断言 instructions 变首条 system，
// function_call / function_call_output item 转成 tool_calls / tool 消息。
func TestResponsesRequestToChat_ItemsAndInstructions(t *testing.T) {
	out := respConvRequest(t, `{
		"model":"m",
		"instructions":"be nice",
		"input":[
			{"role":"developer","content":[{"type":"input_text","text":"dev rule"}]},
			{"role":"user","content":[{"type":"input_text","text":"weather?"}]},
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"SZ\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"sunny"}
		]
	}`)

	msgs := respConvMessages(t, out)
	// system(instructions) / developer / user / assistant(tool_calls) / tool
	if len(msgs) != 5 {
		t.Fatalf("messages = %#v, want 5 entries", msgs)
	}

	// 0：instructions → 首条 system
	sys := respConvMessageAt(t, msgs, 0)
	if sys["role"] != "system" || sys["content"] != "be nice" {
		t.Fatalf("messages[0] = %#v, want system message \"be nice\"", sys)
	}

	// 1：developer item 原样保留 role
	dev := respConvMessageAt(t, msgs, 1)
	if dev["role"] != "developer" {
		t.Fatalf("messages[1].role = %#v, want \"developer\"", dev["role"])
	}
	devParts, ok := dev["content"].([]any)
	if !ok || len(devParts) != 1 {
		t.Fatalf("messages[1].content = %#v, want one part", dev["content"])
	}
	devPart, _ := devParts[0].(map[string]any)
	if devPart["type"] != "text" || devPart["text"] != "dev rule" {
		t.Fatalf("messages[1].content[0] = %#v, want {type:text,text:\"dev rule\"}", devPart)
	}

	// 2：user item，input_text → text part
	user := respConvMessageAt(t, msgs, 2)
	if user["role"] != "user" {
		t.Fatalf("messages[2].role = %#v, want \"user\"", user["role"])
	}
	userParts, ok := user["content"].([]any)
	if !ok || len(userParts) != 1 {
		t.Fatalf("messages[2].content = %#v, want one part", user["content"])
	}
	userPart, _ := userParts[0].(map[string]any)
	if userPart["type"] != "text" || userPart["text"] != "weather?" {
		t.Fatalf("messages[2].content[0] = %#v, want {type:text,text:\"weather?\"}", userPart)
	}

	// 3：function_call → assistant tool_calls（连续调用合并进同一条 assistant 消息）
	asst := respConvMessageAt(t, msgs, 3)
	if asst["role"] != "assistant" {
		t.Fatalf("messages[3].role = %#v, want \"assistant\"", asst["role"])
	}
	calls, ok := asst["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("messages[3].tool_calls = %#v, want one call", asst["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	if call["id"] != "call_1" || call["type"] != "function" {
		t.Fatalf("tool_calls[0] = %#v, want {id:call_1,type:function}", call)
	}
	fn, _ := call["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"SZ"}` {
		t.Fatalf("tool_calls[0].function = %#v, want name/arguments preserved", fn)
	}
}

// TestResponsesRequestToChat_FunctionCallOutput 单独断言 function_call_output 一定是
// 独立 role=tool 消息（tool_call_id 对应 call_id），且不会与 assistant 消息合并。
func TestResponsesRequestToChat_FunctionCallOutput(t *testing.T) {
	out := respConvRequest(t, `{
		"model":"m",
		"input":[
			{"role":"user","content":[{"type":"input_text","text":"weather?"}]},
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"sunny"},
			{"role":"user","content":[{"type":"input_text","text":"and tomorrow?"}]}
		]
	}`)

	msgs := respConvMessages(t, out)
	if len(msgs) != 4 {
		t.Fatalf("messages = %#v, want 4 entries", msgs)
	}
	tool := respConvMessageAt(t, msgs, 2)
	if tool["role"] != "tool" {
		t.Fatalf("messages[2].role = %#v, want \"tool\"", tool["role"])
	}
	if tool["tool_call_id"] != "call_1" {
		t.Fatalf("messages[2].tool_call_id = %#v, want \"call_1\"", tool["tool_call_id"])
	}
	if tool["content"] != "sunny" {
		t.Fatalf("messages[2].content = %#v, want \"sunny\"", tool["content"])
	}
}

// TestResponsesRequestToChat_ContentParts 断言 input_image / input_file 的 part 类型映射。
func TestResponsesRequestToChat_ContentParts(t *testing.T) {
	out := respConvRequest(t, `{
		"model":"m",
		"input":[{"role":"user","content":[
			{"type":"input_text","text":"look"},
			{"type":"input_image","image_url":"https://example.com/a.png"},
			{"type":"input_file","file_id":"file_1"}
		]}]
	}`)

	parts, ok := respConvMessageAt(t, respConvMessages(t, out), 0)["content"].([]any)
	if !ok || len(parts) != 3 {
		t.Fatalf("content = %#v, want 3 parts", parts)
	}
	image, _ := parts[1].(map[string]any)
	if image["type"] != "image_url" {
		t.Fatalf("content[1].type = %#v, want \"image_url\"", image["type"])
	}
	if _, ok := image["image_url"].(map[string]any); !ok {
		t.Fatalf("content[1].image_url = %#v, want a nested object", image["image_url"])
	}
	file, _ := parts[2].(map[string]any)
	if file["type"] != "file" {
		t.Fatalf("content[2].type = %#v, want \"file\"", file["type"])
	}
}

// TestResponsesRequestToChat_TextFormat 断言 text.format → response_format，max_output_tokens →
// max_tokens，扁平 tool → 嵌套 tool，流式追加 stream_options.include_usage。
func TestResponsesRequestToChat_TextFormat(t *testing.T) {
	out := respConvRequest(t, `{
		"model":"m",
		"input":"hi",
		"max_output_tokens":128,
		"temperature":0.2,
		"top_p":0.9,
		"parallel_tool_calls":true,
		"metadata":{"k":"v"},
		"stream":true,
		"text":{"format":{"type":"json_schema","name":"out","schema":{"type":"object"},"strict":true}},
		"tools":[{"type":"function","name":"get_weather","description":"lookup","parameters":{"type":"object","properties":{"city":{"type":"string"}}},"strict":true}],
		"tool_choice":"auto"
	}`)

	// text.format → response_format（原样透传 json_schema 结构）
	rf, ok := out["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format = %#v, want an object", out["response_format"])
	}
	if rf["type"] != "json_schema" || rf["name"] != "out" {
		t.Fatalf("response_format = %#v, want json_schema \"out\"", rf)
	}
	if _, ok := rf["schema"].(map[string]any); !ok {
		t.Fatalf("response_format.schema = %#v, want the object schema", rf["schema"])
	}

	// max_output_tokens → max_tokens
	respConvFloatEqual(t, out["max_tokens"], 128)
	if _, exists := out["max_output_tokens"]; exists {
		t.Fatalf("converted request still carries max_output_tokens")
	}

	// 同名透传字段
	if out["temperature"] != 0.2 || out["top_p"] != 0.9 || out["parallel_tool_calls"] != true {
		t.Fatalf("passthrough fields = %#v/%#v/%#v, want 0.2/0.9/true",
			out["temperature"], out["top_p"], out["parallel_tool_calls"])
	}
	if meta, ok := out["metadata"].(map[string]any); !ok || meta["k"] != "v" {
		t.Fatalf("metadata = %#v, want {k:v}", out["metadata"])
	}
	if out["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %#v, want \"auto\"", out["tool_choice"])
	}

	// 扁平 tool → 嵌套 tool
	tools, ok := out["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v, want one tool", out["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Fatalf("tools[0].type = %#v, want \"function\"", tool["type"])
	}
	if _, exists := tool["name"]; exists {
		t.Fatalf("tools[0] kept the flat name field: %#v", tool)
	}
	fn, ok := tool["function"].(map[string]any)
	if !ok {
		t.Fatalf("tools[0].function = %#v, want an object", tool["function"])
	}
	if fn["name"] != "get_weather" || fn["description"] != "lookup" || fn["strict"] != true {
		t.Fatalf("tools[0].function = %#v, want name/description/strict preserved", fn)
	}
	if _, ok := fn["parameters"].(map[string]any); !ok {
		t.Fatalf("tools[0].function.parameters = %#v, want the schema", fn["parameters"])
	}

	// stream 为真 → 追加 stream_options.include_usage
	opts, ok := out["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Fatalf("stream_options = %#v, want {include_usage:true}", out["stream_options"])
	}
}

// TestResponsesRequestToChat_NoStreamOptions 断言非流式请求不追加 stream_options。
func TestResponsesRequestToChat_NoStreamOptions(t *testing.T) {
	out := respConvRequest(t, `{"model":"m","input":"hi"}`)
	if _, exists := out["stream_options"]; exists {
		t.Fatalf("stream_options = %#v, want it absent for a non-stream request", out["stream_options"])
	}
}

// TestResponsesRequestToChat_Invalid 断言缺 model 与非法 JSON 都返回英文错误。
func TestResponsesRequestToChat_Invalid(t *testing.T) {
	if _, err := responsesRequestToChat([]byte(`{"input":"hi"}`)); err == nil {
		t.Fatalf("missing model: expected an error")
	}
	if _, err := responsesRequestToChat([]byte(`not json`)); err == nil {
		t.Fatalf("invalid JSON: expected an error")
	}
}

// ---------------------------------------------------------------------------
// 响应方向：chat → responses
// ---------------------------------------------------------------------------

// TestChatResponseToResponses_RoundTrip 断言 chat 响应转 responses 后
// output[0].content[0].text 正确、usage.input_tokens 来自 prompt_tokens。
func TestChatResponseToResponses_RoundTrip(t *testing.T) {
	chat := `{
		"id":"chatcmpl-1",
		"object":"chat.completion",
		"model":"upstream-model",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hello there"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,
			"prompt_tokens_details":{"cached_tokens":3},
			"completion_tokens_details":{"reasoning_tokens":2}}
	}`

	raw, err := chatResponseToResponses([]byte(chat), "client-model", "resp_abc")
	if err != nil {
		t.Fatalf("chatResponseToResponses returned an error: %v", err)
	}
	out := respConvMapFromBytes(t, raw)

	if out["id"] != "resp_abc" {
		t.Fatalf("id = %#v, want \"resp_abc\"", out["id"])
	}
	if out["object"] != "response" {
		t.Fatalf("object = %#v, want \"response\"", out["object"])
	}
	if out["model"] != "client-model" {
		t.Fatalf("model = %#v, want the requested model", out["model"])
	}
	if out["status"] != "completed" {
		t.Fatalf("status = %#v, want \"completed\"", out["status"])
	}

	output, ok := out["output"].([]any)
	if !ok || len(output) != 1 {
		t.Fatalf("output = %#v, want one message item", out["output"])
	}
	item, ok := output[0].(map[string]any)
	if !ok {
		t.Fatalf("output[0] = %#v, want an object", output[0])
	}
	if item["type"] != "message" || item["role"] != "assistant" {
		t.Fatalf("output[0] = %#v, want a message item with role assistant", item)
	}
	content, ok := item["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("output[0].content = %#v, want one part", item["content"])
	}
	part, _ := content[0].(map[string]any)
	if part["type"] != "output_text" {
		t.Fatalf("output[0].content[0].type = %#v, want \"output_text\"", part["type"])
	}
	if part["text"] != "hello there" {
		t.Fatalf("output[0].content[0].text = %#v, want \"hello there\"", part["text"])
	}
	if annotations, ok := part["annotations"].([]any); !ok || len(annotations) != 0 {
		t.Fatalf("output[0].content[0].annotations = %#v, want []", part["annotations"])
	}

	usage, ok := out["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage = %#v, want an object", out["usage"])
	}
	respConvFloatEqual(t, usage["input_tokens"], 11)
	respConvFloatEqual(t, usage["output_tokens"], 7)
	respConvFloatEqual(t, usage["total_tokens"], 18)
	inDetails, _ := usage["input_tokens_details"].(map[string]any)
	if inDetails == nil {
		t.Fatalf("usage.input_tokens_details = %#v, want cached_tokens", usage["input_tokens_details"])
	}
	respConvFloatEqual(t, inDetails["cached_tokens"], 3)
	outDetails, _ := usage["output_tokens_details"].(map[string]any)
	if outDetails == nil {
		t.Fatalf("usage.output_tokens_details = %#v, want reasoning_tokens", usage["output_tokens_details"])
	}
	respConvFloatEqual(t, outDetails["reasoning_tokens"], 2)
}

// TestChatResponseToResponses_ToolCallsAndLength 断言 tool_calls → function_call item，
// finish_reason="length" → status=incomplete + incomplete_details.reason=max_output_tokens。
func TestChatResponseToResponses_ToolCallsAndLength(t *testing.T) {
	chat := `{
		"id":"chatcmpl-2",
		"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_9","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SZ\"}"}}
		]},"finish_reason":"length"}],
		"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}
	}`

	raw, err := chatResponseToResponses([]byte(chat), "m", "resp_x")
	if err != nil {
		t.Fatalf("chatResponseToResponses returned an error: %v", err)
	}
	out := respConvMapFromBytes(t, raw)

	if out["status"] != "incomplete" {
		t.Fatalf("status = %#v, want \"incomplete\"", out["status"])
	}
	details, ok := out["incomplete_details"].(map[string]any)
	if !ok || details["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete_details = %#v, want reason max_output_tokens", out["incomplete_details"])
	}

	output, ok := out["output"].([]any)
	if !ok || len(output) != 1 {
		t.Fatalf("output = %#v, want exactly the tool call item (no empty text message)", out["output"])
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "function_call" {
		t.Fatalf("output[0].type = %#v, want \"function_call\"", item["type"])
	}
	if item["call_id"] != "call_9" || item["name"] != "get_weather" {
		t.Fatalf("output[0] = %#v, want call_id/name preserved", item)
	}
	if item["arguments"] != `{"city":"SZ"}` {
		t.Fatalf("output[0].arguments = %#v, want the raw arguments string", item["arguments"])
	}
}

// TestChatResponseToResponses_Invalid 断言非 chat 响应返回错误。
func TestChatResponseToResponses_Invalid(t *testing.T) {
	if _, err := chatResponseToResponses([]byte(`not json`), "m", "resp_x"); err == nil {
		t.Fatalf("invalid JSON: expected an error")
	}
	if _, err := chatResponseToResponses([]byte(`{"id":"x","choices":[]}`), "m", "resp_x"); err == nil {
		t.Fatalf("empty choices: expected an error")
	}
}

// ---------------------------------------------------------------------------
// 流式方向：chat chunk → Responses 事件序列
// ---------------------------------------------------------------------------

// respStreamNew 构造被测翻译器；httptest.ResponseRecorder 自带 http.Flusher。
func respStreamNew(t *testing.T, model string) (*responsesStreamTranslator, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	return newResponsesStreamTranslator(rec, rec, model, "resp_stream"), rec
}

// respStreamSSE 组装一个 chat 形态的 SSE 事件（data 行 + 空行结束）。
func respStreamSSE(data string) string { return "data: " + data + "\n\n" }

// respStreamFeed 按 size 字节一段喂入，模拟上游任意切分（含在事件中间切断）。
func respStreamFeed(t *testing.T, tr *responsesStreamTranslator, payload string, size int) {
	t.Helper()
	if size <= 0 {
		size = 1
	}
	for i := 0; i < len(payload); i += size {
		end := i + size
		if end > len(payload) {
			end = len(payload)
		}
		p := []byte(payload[i:end])
		n, err := tr.Write(p)
		if err != nil {
			t.Fatalf("Write returned an error: %v", err)
		}
		if n != len(p) {
			t.Fatalf("Write returned n = %d, want %d", n, len(p))
		}
	}
}

// respStreamEvents 按出现顺序返回全部事件 (name, data)。
func respStreamEvents(t *testing.T, raw string) [][2]string {
	t.Helper()
	var out [][2]string
	for _, block := range strings.Split(raw, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			if rest, ok := strings.CutPrefix(line, "event: "); ok {
				name = strings.TrimSpace(rest)
				continue
			}
			if rest, ok := strings.CutPrefix(line, "data: "); ok {
				data = rest
			}
		}
		if name == "" {
			t.Fatalf("output contains a block without an event line: %q", block)
		}
		out = append(out, [2]string{name, data})
	}
	return out
}

// respStreamDataOf 解析指定第 i 个同类型事件的 data 载荷为 map。
func respStreamDataOf(t *testing.T, events [][2]string, eventName string, i int) map[string]any {
	t.Helper()
	seen := 0
	for _, ev := range events {
		if ev[0] != eventName {
			continue
		}
		if seen == i {
			var m map[string]any
			if err := json.Unmarshal([]byte(ev[1]), &m); err != nil {
				t.Fatalf("unmarshal %s payload %q: %v", eventName, ev[1], err)
			}
			return m
		}
		seen++
	}
	t.Fatalf("event %q #%d not found in %v", eventName, i, events)
	return nil
}

// respStreamChatChunks 是事件序列测试的输入：两条内容增量 + 带 usage/finish_reason 的末块。
func respStreamChatChunks() string {
	return respStreamSSE(`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"he"},"finish_reason":null}]}`) +
		respStreamSSE(`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":null}]}`) +
		respStreamSSE(`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
}

// respStreamWantOrder 是 §5.5 规定的严格事件类型序列。
var respStreamWantOrder = []string{
	"response.created",
	"response.in_progress",
	"response.output_item.added",
	"response.content_part.added",
	"response.output_text.delta",
	"response.output_text.delta",
	"response.output_text.done",
	"response.content_part.done",
	"response.output_item.done",
	"response.completed",
}

// TestResponsesStreamTranslator_EventOrder（Review Focus 5）断言：
//   - 事件 type 序列严格等于 §5.5 的列表（不多不少、顺序一致）；
//   - response.created 与 response.completed 的 id 相同且以 resp_ 开头；
//   - 输出不含 [DONE]；
//   - response.completed 携带累积文本与最终 usage。
func TestResponsesStreamTranslator_EventOrder(t *testing.T) {
	tr, rec := respStreamNew(t, "client-model")
	respStreamFeed(t, tr, respStreamChatChunks(), 0)
	tr.Finish()

	if !tr.Wrote() {
		t.Fatalf("Wrote() = false after a full stream, want true")
	}

	raw := rec.Body.String()
	if strings.Contains(raw, "[DONE]") {
		t.Fatalf("output contains [DONE]:\n%s", raw)
	}

	events := respStreamEvents(t, raw)
	gotNames := make([]string, 0, len(events))
	for _, ev := range events {
		gotNames = append(gotNames, ev[0])
	}
	if len(gotNames) != len(respStreamWantOrder) {
		t.Fatalf("event types = %v, want exactly %v", gotNames, respStreamWantOrder)
	}
	for i := range respStreamWantOrder {
		if gotNames[i] != respStreamWantOrder[i] {
			t.Fatalf("event types = %v, want exactly %v", gotNames, respStreamWantOrder)
		}
	}

	// 每个事件的 data.type 必须与 event: 行一致。
	for _, ev := range events {
		payload := respStreamDataOf(t, events, ev[0], 0)
		if payload["type"] != ev[0] {
			t.Fatalf("event %q carries data.type = %#v", ev[0], payload["type"])
		}
	}

	created := respStreamDataOf(t, events, "response.created", 0)
	completed := respStreamDataOf(t, events, "response.completed", 0)
	createdResp, _ := created["response"].(map[string]any)
	completedResp, _ := completed["response"].(map[string]any)
	if createdResp == nil || completedResp == nil {
		t.Fatalf("created/completed payload lacks a response object: %v / %v", created, completed)
	}
	createdID, _ := createdResp["id"].(string)
	completedID, _ := completedResp["id"].(string)
	if createdID == "" || completedID == "" {
		t.Fatalf("created id = %q, completed id = %q, want non-empty", createdID, completedID)
	}
	if createdID != completedID {
		t.Fatalf("created id = %q but completed id = %q, want the same id", createdID, completedID)
	}
	if !strings.HasPrefix(createdID, "resp_") {
		t.Fatalf("id = %q, want it to start with \"resp_\"", createdID)
	}

	// 增量与终态文本
	d0 := respStreamDataOf(t, events, "response.output_text.delta", 0)
	d1 := respStreamDataOf(t, events, "response.output_text.delta", 1)
	if d0["delta"] != "he" || d1["delta"] != "llo" {
		t.Fatalf("deltas = %#v/%#v, want \"he\"/\"llo\"", d0["delta"], d1["delta"])
	}
	done := respStreamDataOf(t, events, "response.output_text.done", 0)
	if done["text"] != "hello" {
		t.Fatalf("output_text.done text = %#v, want \"hello\"", done["text"])
	}

	// 终态响应：status 与 usage 必须与上游末块一致。
	if completedResp["status"] != "completed" {
		t.Fatalf("completed status = %#v, want \"completed\"", completedResp["status"])
	}
	if completedResp["model"] != "client-model" {
		t.Fatalf("completed model = %#v, want the requested model", completedResp["model"])
	}
	usage, ok := completedResp["usage"].(map[string]any)
	if !ok {
		t.Fatalf("completed usage = %#v, want an object", completedResp["usage"])
	}
	respConvFloatEqual(t, usage["input_tokens"], 5)
	respConvFloatEqual(t, usage["output_tokens"], 2)
	respConvFloatEqual(t, usage["total_tokens"], 7)

	// 终态响应里应带上已合成的完整 output。
	completedOut, ok := completedResp["output"].([]any)
	if !ok || len(completedOut) != 1 {
		t.Fatalf("completed output = %#v, want one synthesized message item", completedResp["output"])
	}
	createdOut, ok := createdResp["output"].([]any)
	if !ok || len(createdOut) != 0 {
		t.Fatalf("created output = %#v, want []", createdResp["output"])
	}
}

// TestResponsesStreamTranslator_ArbitraryChunking 断言上游任意字节切分（含事件中间切断、
// 带 [DONE] 且末块缺 usage）下事件序列不变。
func TestResponsesStreamTranslator_ArbitraryChunking(t *testing.T) {
	payload := respStreamChatChunks() + "data: [DONE]\n\n"
	for _, size := range []int{1, 3, 7, 64} {
		tr, rec := respStreamNew(t, "m")
		respStreamFeed(t, tr, payload, size)
		tr.Finish()

		raw := rec.Body.String()
		if strings.Contains(raw, "[DONE]") {
			t.Fatalf("size=%d: output contains [DONE]", size)
		}
		events := respStreamEvents(t, raw)
		gotNames := make([]string, 0, len(events))
		for _, ev := range events {
			gotNames = append(gotNames, ev[0])
		}
		if strings.Join(gotNames, ",") != strings.Join(respStreamWantOrder, ",") {
			t.Fatalf("size=%d: event types = %v, want exactly %v", size, gotNames, respStreamWantOrder)
		}
	}
}

// TestResponsesStreamTranslator_EmptyStream 断言上游 200 但无任何增量时仍产出合法事件序列，
// 且不出现 content_part / output_text 事件（没有内容就没有 part）。
func TestResponsesStreamTranslator_EmptyStream(t *testing.T) {
	tr, rec := respStreamNew(t, "m")
	tr.Finish()

	events := respStreamEvents(t, rec.Body.String())
	gotNames := make([]string, 0, len(events))
	for _, ev := range events {
		gotNames = append(gotNames, ev[0])
	}
	want := []string{"response.created", "response.in_progress", "response.completed"}
	if strings.Join(gotNames, ",") != strings.Join(want, ",") {
		t.Fatalf("event types = %v, want exactly %v", gotNames, want)
	}
}

// TestResponsesStreamTranslator_FinishIdempotent 断言重复 Finish 与收尾后的 Write 不会追加事件。
func TestResponsesStreamTranslator_FinishIdempotent(t *testing.T) {
	tr, rec := respStreamNew(t, "m")
	respStreamFeed(t, tr, respStreamChatChunks(), 0)
	tr.Finish()
	first := rec.Body.String()
	tr.Finish()
	if _, err := tr.Write([]byte(respStreamSSE(`{"choices":[{"delta":{"content":"late"}}]}`))); err != nil {
		t.Fatalf("Write after Finish returned an error: %v", err)
	}
	if rec.Body.String() != first {
		t.Fatalf("output changed after a second Finish / post-finalize Write")
	}
}

// TestResponsesStreamTranslator_ErrorEvent 断言流中错误以 event: error 输出、不带 [DONE]。
func TestResponsesStreamTranslator_ErrorEvent(t *testing.T) {
	tr, rec := respStreamNew(t, "m")
	tr.Write([]byte(respStreamSSE(`{"choices":[{"delta":{"content":"he"}}]}`)))
	tr.Write([]byte(`data: {"error":{"message":"upstream exploded","type":"server_error"}}` + "\n\n"))
	tr.Finish()

	raw := rec.Body.String()
	if strings.Contains(raw, "[DONE]") {
		t.Fatalf("output contains [DONE]")
	}
	events := respStreamEvents(t, raw)
	// C-2：error 之后仍须补出 response.failed 终态，故 error 不再是最后一条。
	if got := respStreamLastTerminal(t, events); got != "response.failed" {
		t.Fatalf("terminal event = %q, want response.failed", got)
	}
	if n := len(respStreamAllOf(events, "error")); n != 1 {
		t.Fatalf("error event count = %d, want 1", n)
	}
	errPayload := respStreamDataOf(t, events, "error", 0)
	errObj, ok := errPayload["error"].(map[string]any)
	if !ok || errObj["message"] != "upstream exploded" {
		t.Fatalf("error payload = %#v, want the upstream message", errPayload)
	}
}

// ---------------------------------------------------------------------------
// 流式方向：工具调用事件合成（spec §5.5.1）
// ---------------------------------------------------------------------------

// respStreamChatToolChunk 组装一个带 tool_calls 增量的 chat 分片。
// args 为空串时不写 arguments 字段（模拟只带 id/name 的首片）。
func respStreamChatToolChunk(index int, id, name, args string) string {
	fn := map[string]any{"name": name, "arguments": args}
	call := map[string]any{"index": index, "function": fn}
	if id != "" {
		call["id"] = id
		call["type"] = "function"
	}
	chunk := map[string]any{
		"id":      "c1",
		"object":  "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{call}}}},
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return ""
	}
	return respStreamSSE(string(b))
}

// respStreamChatTextChunk 组装一个只带文本增量的 chat 分片。
func respStreamChatTextChunk(text string) string {
	chunk := map[string]any{
		"id":      "c1",
		"object":  "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}}},
	}
	b, _ := json.Marshal(chunk)
	return respStreamSSE(string(b))
}

// respStreamChatFinishChunk 组装带 finish_reason 与 usage 的末片。
func respStreamChatFinishChunk(finishReason string) string {
	chunk := map[string]any{
		"id":      "c1",
		"object":  "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}},
		"usage":   map[string]any{"prompt_tokens": 9, "completion_tokens": 4, "total_tokens": 13},
	}
	b, _ := json.Marshal(chunk)
	return respStreamSSE(string(b))
}

// respStreamEventNames 返回事件名序列。
func respStreamEventNames(events [][2]string) []string {
	names := make([]string, 0, len(events))
	for _, ev := range events {
		names = append(names, ev[0])
	}
	return names
}

// respStreamAllOf 返回指定类型的全部事件（按出现顺序）。
func respStreamAllOf(events [][2]string, eventName string) [][2]string {
	var out [][2]string
	for _, ev := range events {
		if ev[0] == eventName {
			out = append(out, ev)
		}
	}
	return out
}

// respStreamCompleted 返回 response.completed 的 response 对象。
func respStreamCompleted(t *testing.T, events [][2]string) map[string]any {
	t.Helper()
	payload := respStreamDataOf(t, events, "response.completed", 0)
	resp, ok := payload["response"].(map[string]any)
	if !ok {
		t.Fatalf("response.completed payload lacks a response object: %v", payload)
	}
	return resp
}

// respStreamOutput 返回终态 response 的 output 数组。
func respStreamOutput(t *testing.T, resp map[string]any) []any {
	t.Helper()
	output, ok := resp["output"].([]any)
	if !ok {
		t.Fatalf("response.output = %#v, want an array", resp["output"])
	}
	return output
}

// respStreamItemAt 取 output[i] 并断言是对象。
func respStreamItemAt(t *testing.T, output []any, i int) map[string]any {
	t.Helper()
	if i >= len(output) {
		t.Fatalf("output has %d items, want index %d: %#v", len(output), i, output)
	}
	item, ok := output[i].(map[string]any)
	if !ok {
		t.Fatalf("output[%d] = %#v, want an object", i, output[i])
	}
	return item
}

// TestResponsesStreamTranslator_FunctionCallSingle 单工具流式：两个参数增量分片
// → 事件序列含 function_call_arguments.delta ×2 与 .done，且终态 item 的 arguments 是完整合法 JSON。
func TestResponsesStreamTranslator_FunctionCallSingle(t *testing.T) {
	tr, rec := respStreamNew(t, "m")
	respStreamFeed(t, tr, respStreamChatToolChunk(0, "call_1", "get_weather", ""), 0)
	respStreamFeed(t, tr, respStreamChatToolChunk(0, "", "", `{"city":`), 0)
	respStreamFeed(t, tr, respStreamChatToolChunk(0, "", "", `"SZ"}`), 0)
	respStreamFeed(t, tr, respStreamChatFinishChunk("tool_calls"), 0)
	tr.Finish()

	raw := rec.Body.String()
	if strings.Contains(raw, "[DONE]") {
		t.Fatalf("output contains [DONE]")
	}
	events := respStreamEvents(t, raw)

	// 参数增量：恰好两条，内容与顺序与上游分片一致。
	deltas := respStreamAllOf(events, "response.function_call_arguments.delta")
	if len(deltas) != 2 {
		t.Fatalf("function_call_arguments.delta count = %d, want 2: %v", len(deltas), respStreamEventNames(events))
	}
	for i, want := range []string{`{"city":`, `"SZ"}`} {
		payload := respStreamDataOf(t, events, "response.function_call_arguments.delta", i)
		if payload["delta"] != want {
			t.Fatalf("delta #%d = %#v, want %q", i, payload["delta"], want)
		}
	}

	// 参数收齐：done 恰好一条，带完整 arguments。
	argDone := respStreamDataOf(t, events, "response.function_call_arguments.done", 0)
	if argDone["arguments"] != `{"city":"SZ"}` {
		t.Fatalf("function_call_arguments.done arguments = %#v, want the full JSON", argDone["arguments"])
	}

	// output_item.added 的 item 形状。
	added := respStreamDataOf(t, events, "response.output_item.added", 0)
	addedItem, ok := added["item"].(map[string]any)
	if !ok {
		t.Fatalf("output_item.added item = %#v, want an object", added["item"])
	}
	if addedItem["type"] != "function_call" || addedItem["call_id"] != "call_1" ||
		addedItem["name"] != "get_weather" || addedItem["arguments"] != "" {
		t.Fatalf("output_item.added item = %#v, want function_call with empty arguments", addedItem)
	}

	// 终态 output：唯一 item 是 function_call，arguments 为完整合法 JSON。
	resp := respStreamCompleted(t, events)
	if resp["status"] != "completed" {
		t.Fatalf("status = %#v, want \"completed\" for finish_reason=tool_calls", resp["status"])
	}
	output := respStreamOutput(t, resp)
	if len(output) != 1 {
		t.Fatalf("output = %#v, want exactly 1 function_call item", output)
	}
	item := respStreamItemAt(t, output, 0)
	if item["type"] != "function_call" || item["call_id"] != "call_1" || item["name"] != "get_weather" {
		t.Fatalf("output[0] = %#v, want the function_call item", item)
	}
	args, _ := item["arguments"].(string)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("output[0].arguments = %q is not valid JSON: %v", args, err)
	}
	if parsed["city"] != "SZ" {
		t.Fatalf("output[0].arguments = %q, want city=SZ", args)
	}

	// 无文本输出：不应出现 message 级事件。
	for _, name := range []string{"response.content_part.added", "response.output_text.delta"} {
		if len(respStreamAllOf(events, name)) != 0 {
			t.Fatalf("unexpected %q event in a tool-only stream: %v", name, respStreamEventNames(events))
		}
	}
}

// TestResponsesStreamTranslator_FunctionCallParallel 多工具并行：两个工具交错增量
// → 各自 output_index 不同、参数不互相污染、output[] 有 2 个 function_call item。
func TestResponsesStreamTranslator_FunctionCallParallel(t *testing.T) {
	tr, rec := respStreamNew(t, "m")
	// 交错：A 首片 → B 首片 → A 参数 → B 参数 → A 参数
	for _, chunk := range []string{
		respStreamChatToolChunk(0, "call_a", "alpha", ""),
		respStreamChatToolChunk(1, "call_b", "beta", ""),
		respStreamChatToolChunk(0, "", "", `{"a":1`),
		respStreamChatToolChunk(1, "", "", `{"b":2`),
		respStreamChatToolChunk(0, "", "", `}`),
		respStreamChatToolChunk(1, "", "", `}`),
		respStreamChatFinishChunk("tool_calls"),
	} {
		respStreamFeed(t, tr, chunk, 0)
	}
	tr.Finish()
	events := respStreamEvents(t, rec.Body.String())

	// 两个 item 的 output_index 必须不同。
	addedA := respStreamDataOf(t, events, "response.output_item.added", 0)
	addedB := respStreamDataOf(t, events, "response.output_item.added", 1)
	idxA, idxB := addedA["output_index"], addedB["output_index"]
	if idxA == nil || idxB == nil || idxA == idxB {
		t.Fatalf("output_index = %v / %v, want two distinct integers", idxA, idxB)
	}

	// 每条参数增量必须落到各自 item 的 output_index 上（不串号）。
	deltas := respStreamAllOf(events, "response.function_call_arguments.delta")
	if len(deltas) != 4 {
		t.Fatalf("delta count = %d, want 4: %v", len(deltas), respStreamEventNames(events))
	}
	itemOf := map[any]string{idxA: "call_a", idxB: "call_b"}
	got := map[string]string{}
	for i := range deltas {
		payload := respStreamDataOf(t, events, "response.function_call_arguments.delta", i)
		callID, ok := itemOf[payload["output_index"]]
		if !ok {
			t.Fatalf("delta #%d carries unknown output_index %#v", i, payload["output_index"])
		}
		got[callID] += payload["delta"].(string)
	}
	if got["call_a"] != `{"a":1}` || got["call_b"] != `{"b":2}` {
		t.Fatalf("accumulated arguments = %#v, want call_a {\"a\":1} / call_b {\"b\":2}", got)
	}

	argDone := respStreamAllOf(events, "response.function_call_arguments.done")
	if len(argDone) != 2 {
		t.Fatalf("function_call_arguments.done count = %d, want 2", len(argDone))
	}
	doneArgs := map[any]string{}
	for i := range argDone {
		payload := respStreamDataOf(t, events, "response.function_call_arguments.done", i)
		doneArgs[payload["output_index"]] = payload["arguments"].(string)
	}
	if doneArgs[idxA] != `{"a":1}` || doneArgs[idxB] != `{"b":2}` {
		t.Fatalf("done arguments = %#v, want per-tool complete JSON", doneArgs)
	}

	// 终态 output：2 个 function_call item，参数各自完整。
	resp := respStreamCompleted(t, events)
	output := respStreamOutput(t, resp)
	if len(output) != 2 {
		t.Fatalf("output = %#v, want 2 function_call items", output)
	}
	calls := map[string]string{}
	for i := range output {
		item := respStreamItemAt(t, output, i)
		if item["type"] != "function_call" {
			t.Fatalf("output[%d].type = %#v, want function_call", i, item["type"])
		}
		callID, _ := item["call_id"].(string)
		args, _ := item["arguments"].(string)
		calls[callID] = args
	}
	if calls["call_a"] != `{"a":1}` || calls["call_b"] != `{"b":2}` {
		t.Fatalf("output arguments = %#v, want call_a {\"a\":1} / call_b {\"b\":2}", calls)
	}
}

// TestResponsesStreamTranslator_TextThenTool 文本 + 工具混合：
// output[] 同时含 message 与 function_call，顺序与出现顺序一致，output_index 不共享。
func TestResponsesStreamTranslator_TextThenTool(t *testing.T) {
	tr, rec := respStreamNew(t, "m")
	for _, chunk := range []string{
		respStreamChatTextChunk("let me check"),
		respStreamChatToolChunk(0, "call_1", "get_weather", `{"city":"SZ"}`),
		respStreamChatFinishChunk("tool_calls"),
	} {
		respStreamFeed(t, tr, chunk, 0)
	}
	tr.Finish()
	events := respStreamEvents(t, rec.Body.String())

	// 两个 item 的 added 事件必须分配不同 output_index。
	addedMsg := respStreamDataOf(t, events, "response.output_item.added", 0)
	addedCall := respStreamDataOf(t, events, "response.output_item.added", 1)
	if addedMsg["output_index"] == addedCall["output_index"] {
		t.Fatalf("message and function_call share output_index %v", addedMsg["output_index"])
	}
	msgItem, _ := addedMsg["item"].(map[string]any)
	if msgItem["type"] != "message" {
		t.Fatalf("first added item = %#v, want the message item first", msgItem)
	}
	callIdx, ok := addedCall["output_index"].(float64)
	if !ok || callIdx <= addedMsg["output_index"].(float64) {
		t.Fatalf("function_call output_index = %#v, want it to follow the message index", addedCall["output_index"])
	}

	// content_part 事件必须带 message 的 output_index（而非写死 0 之外的值）。
	partAdded := respStreamDataOf(t, events, "response.content_part.added", 0)
	if partAdded["output_index"] != addedMsg["output_index"] {
		t.Fatalf("content_part.added output_index = %#v, want %#v", partAdded["output_index"], addedMsg["output_index"])
	}
	argDelta := respStreamDataOf(t, events, "response.function_call_arguments.delta", 0)
	if argDelta["output_index"] != addedCall["output_index"] {
		t.Fatalf("arguments.delta output_index = %#v, want %#v", argDelta["output_index"], addedCall["output_index"])
	}

	// 终态 output：message 在前，function_call 在后。
	resp := respStreamCompleted(t, events)
	output := respStreamOutput(t, resp)
	if len(output) != 2 {
		t.Fatalf("output = %#v, want message + function_call", output)
	}
	msg := respStreamItemAt(t, output, 0)
	if msg["type"] != "message" {
		t.Fatalf("output[0].type = %#v, want message first", msg["type"])
	}
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("output[0].content = %#v, want one output_text part", msg["content"])
	}
	if part, _ := content[0].(map[string]any); part["text"] != "let me check" {
		t.Fatalf("output[0].content[0].text = %#v, want \"let me check\"", content[0])
	}
	call := respStreamItemAt(t, output, 1)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["arguments"] != `{"city":"SZ"}` {
		t.Fatalf("output[1] = %#v, want the function_call item", call)
	}
}

// TestResponsesStreamTranslator_FunctionCallInterleavedWithText 工具与文本第二次交错：
// 一条工具调用后再来文本增量，必须复用同一个 message item（不新开），output 仍为 2 项。
func TestResponsesStreamTranslator_FunctionCallInterleavedWithText(t *testing.T) {
	tr, rec := respStreamNew(t, "m")
	for _, chunk := range []string{
		respStreamChatTextChunk("first "),
		respStreamChatToolChunk(0, "call_1", "get_weather", `{}`),
		respStreamChatTextChunk("second"),
		respStreamChatFinishChunk("tool_calls"),
	} {
		respStreamFeed(t, tr, chunk, 0)
	}
	tr.Finish()
	events := respStreamEvents(t, rec.Body.String())

	// message item 只 added 一次。
	if n := len(respStreamAllOf(events, "response.output_item.added")); n != 2 {
		t.Fatalf("output_item.added count = %d, want 2 (message + function_call): %v",
			n, respStreamEventNames(events))
	}
	resp := respStreamCompleted(t, events)
	output := respStreamOutput(t, resp)
	if len(output) != 2 {
		t.Fatalf("output = %#v, want message + function_call", output)
	}
	msg := respStreamItemAt(t, output, 0)
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("output[0].content = %#v, want one part", msg["content"])
	}
	part, _ := content[0].(map[string]any)
	if part["text"] != "first second" {
		t.Fatalf("message text = %#v, want both deltas in one item", part["text"])
	}
}

// TestResponsesStreamTranslator_TextOnlyNoRegression 断言纯文本流的事件序列
// 仍严格等于 §5.5 原序列（工具调用支持不得改变 message 路径）。
func TestResponsesStreamTranslator_TextOnlyNoRegression(t *testing.T) {
	tr, rec := respStreamNew(t, "m")
	respStreamFeed(t, tr, respStreamChatChunks(), 0)
	tr.Finish()

	gotNames := respStreamEventNames(respStreamEvents(t, rec.Body.String()))
	if strings.Join(gotNames, ",") != strings.Join(respStreamWantOrder, ",") {
		t.Fatalf("event types = %v, want exactly %v", gotNames, respStreamWantOrder)
	}
}

// TestResponsesStreamTranslator_FunctionCallClosure 断言收尾时每个未收齐的工具
// 都会补齐 arguments.done → output_item.done（即便上游未发 [DONE] / 末块）。
func TestResponsesStreamTranslator_FunctionCallClosure(t *testing.T) {
	tr, rec := respStreamNew(t, "m")
	for _, chunk := range []string{
		respStreamChatTextChunk("hi"),
		respStreamChatToolChunk(0, "call_1", "alpha", `{"a":1}`),
		respStreamChatToolChunk(1, "call_2", "beta", `{"b":2}`),
	} {
		respStreamFeed(t, tr, chunk, 0)
	}
	// 不给 [DONE]，但上游给了 finish_reason（已结束）：Finish() 应走正常收尾而非判定中断。
	respStreamFeed(t, tr, respStreamChatFinishChunk("tool_calls"), 0)
	tr.Finish()
	events := respStreamEvents(t, rec.Body.String())

	if n := len(respStreamAllOf(events, "response.function_call_arguments.done")); n != 2 {
		t.Fatalf("function_call_arguments.done count = %d, want 2: %v", n, respStreamEventNames(events))
	}
	// output_item.done 必须覆盖全部 3 个 item。
	doneItems := respStreamAllOf(events, "response.output_item.done")
	if len(doneItems) != 3 {
		t.Fatalf("output_item.done count = %d, want 3", len(doneItems))
	}
	// 收尾顺序：所有 item 级事件都必须在 response.completed 之前。
	names := respStreamEventNames(events)
	if names[len(names)-1] != "response.completed" {
		t.Fatalf("last event = %q, want response.completed", names[len(names)-1])
	}
	// 每个 item 的 added 必须早于其 done（按 output_index 配对）。
	type span struct{ added, done int }
	spans := map[int]*span{}
	for j := range events {
		name := events[j][0]
		if name != "response.output_item.added" && name != "response.output_item.done" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(events[j][1]), &payload); err != nil {
			t.Fatalf("unmarshal %s payload: %v", name, err)
		}
		idx := int(payload["output_index"].(float64))
		if spans[idx] == nil {
			spans[idx] = &span{added: -1, done: -1}
		}
		if name == "response.output_item.added" {
			spans[idx].added = j
		} else {
			spans[idx].done = j
		}
	}
	for idx := 0; idx < 3; idx++ {
		sp := spans[idx]
		if sp == nil || sp.added < 0 || sp.done < 0 || sp.added > sp.done {
			t.Fatalf("item %d: span = %#v, want added before done", idx, sp)
		}
	}
}

// ---------------------------------------------------------------------------
// Fix Round 2：契约加固（I-1 / I-2 / I-3 / I-5 / I-6）
// ---------------------------------------------------------------------------

// TestResponsesRequestToChat_RequiresMessages（I-1）断言无法产出 messages 的 input 一律报错，
// 不允许发出「只有 model、没有 messages」的上游请求（§2 反目标：转换丢字段却不报错）。
func TestResponsesRequestToChat_RequiresMessages(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"input 是对象", `{"model":"m","input":{"role":"user","content":"hi"}}`},
		{"数组里全是被丢弃的 item", `{"model":"m","input":[{"type":"reasoning"}]}`},
		{"input 是非字符串标量", `{"model":"m","input":123}`},
		{"完全没有 input", `{"model":"m"}`},
		{"input 为 null", `{"model":"m","input":null}`},
		{"input 是空数组", `{"model":"m","input":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := responsesRequestToChat([]byte(tc.body))
			if err == nil {
				t.Fatalf("responsesRequestToChat(%s) = %v, want an error (no messages would be sent upstream)", tc.body, out)
			}
			if !strings.Contains(err.Error(), "invalid input") {
				t.Fatalf("error = %q, want it to mention \"invalid input\"", err.Error())
			}
		})
	}
}

// TestResponsesRequestToChat_EmptyStringInput 空字符串 input 产生一条空 user 消息，按合法处理。
func TestResponsesRequestToChat_EmptyStringInput(t *testing.T) {
	out, err := responsesRequestToChat([]byte(`{"model":"m","input":""}`))
	if err != nil {
		t.Fatalf("empty string input: unexpected error: %v", err)
	}
	msgs, ok := out["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages = %#v, want exactly one empty user message", out["messages"])
	}
	msg, _ := msgs[0].(map[string]any)
	if msg["role"] != "user" || msg["content"] != "" {
		t.Fatalf("messages[0] = %#v, want {role:user,content:\"\"}", msg)
	}
}

// TestResponsesRequestToChat_ToolCallOnlyAssistant（I-1 伴生）断言 content:null 的
// assistant item 只要带 tool_calls 就仍算有效输入（不能因为「没有文本」被误判成 invalid input）。
func TestResponsesRequestToChat_ToolCallOnlyAssistant(t *testing.T) {
	out, err := responsesRequestToChat([]byte(`{"model":"m","input":[
		{"type":"message","role":"assistant","content":null,
		 "tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}
	]}`))
	if err != nil {
		t.Fatalf("tool_calls-only assistant item: unexpected error: %v", err)
	}
	msgs, ok := out["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages = %#v, want one assistant message", out["messages"])
	}
	msg, _ := msgs[0].(map[string]any)
	calls, ok := msg["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("messages[0].tool_calls = %#v, want the incoming tool_calls preserved", msg["tool_calls"])
	}
}

// TestResponsesRequestToChat_EmptyToolsPreserved（I-2）断言显式 tools:[] 原样保留，
// 避免「tool_choice 存在但 tools 缺失」被部分上游判为 400。
func TestResponsesRequestToChat_EmptyToolsPreserved(t *testing.T) {
	out := respConvRequest(t, `{"model":"m","input":"hi","tools":[],"tool_choice":"auto"}`)
	tools, ok := out["tools"].([]any)
	if !ok {
		t.Fatalf("tools = %#v, want an explicit empty array (presence matters)", out["tools"])
	}
	if len(tools) != 0 {
		t.Fatalf("tools = %#v, want empty", tools)
	}
	if out["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %#v, want \"auto\"", out["tool_choice"])
	}
}

// TestResponsesStreamTranslator_ErrorBeforeFirstDelta（I-3）断言首个增量之前出错时
// 仍先发 response.created / in_progress，再发 error（SDK 依赖 created 建立 response 对象）。
func TestResponsesStreamTranslator_ErrorBeforeFirstDelta(t *testing.T) {
	tr, rec := respStreamNew(t, "m")
	tr.Write([]byte(`data: {"error":{"message":"boom","type":"server_error"}}` + "\n\n"))
	tr.Finish()

	events := respStreamEvents(t, rec.Body.String())
	names := respStreamEventNames(events)
	want := []string{"response.created", "response.in_progress", "error", "response.failed"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("event types = %v, want exactly %v", names, want)
	}
	created := respStreamDataOf(t, events, "response.created", 0)
	resp, _ := created["response"].(map[string]any)
	if resp["id"] != "resp_stream" {
		t.Fatalf("response.created id = %#v, want resp_stream", resp["id"])
	}
	// 流中错误算「已写出」：已经开流，无法回退成 JSON 502。
	if !tr.Wrote() {
		t.Fatalf("Wrote() = false after an in-stream error, want true")
	}
}

// TestResponsesStreamTranslator_EmptyStreamWroteFalse（I-5）断言上游 200 但零增量时，
// Finish() 后 Wrote() 必须仍为 false，Task 6 才能据此回退成 502。
func TestResponsesStreamTranslator_EmptyStreamWroteFalse(t *testing.T) {
	tr, _ := respStreamNew(t, "m")
	if tr.Wrote() {
		t.Fatalf("Wrote() = true before any input")
	}
	tr.Finish()
	if tr.Wrote() {
		t.Fatalf("Wrote() = true after Finish() on an empty stream, want false (would break the 502 fallback)")
	}
}

// TestResponsesStreamTranslator_WroteAfterFirstEvent 断言 Wrote() 只由真实写出驱动。
func TestResponsesStreamTranslator_WroteAfterFirstEvent(t *testing.T) {
	tr, _ := respStreamNew(t, "m")
	payload := respStreamChatChunks()
	// 只喂入第一个事件但**不含结束空行**的前缀：缓冲未构成完整事件，尚未写出任何字节。
	firstEnd := strings.Index(payload, "\n\n")
	if firstEnd < 0 {
		t.Fatalf("fixture has no event boundary")
	}
	tr.Write([]byte(payload[:firstEnd+1]))
	if tr.Wrote() {
		t.Fatalf("Wrote() = true after an incomplete event prefix, want false")
	}
	respStreamFeed(t, tr, payload, 0)
	if !tr.Wrote() {
		t.Fatalf("Wrote() = false after a full stream, want true")
	}
}

// TestResponsesStreamTranslator_ToolCallsButNoItemWarns（I-6）断言 finish_reason="tool_calls"
// 但没有任何 function_call item 时会记 Warn（防御上游漏发 tool_calls 增量）。
func TestResponsesStreamTranslator_ToolCallsButNoItemWarns(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	tr, rec := respStreamNew(t, "m")
	respStreamFeed(t, tr, respStreamChatTextChunk("just text"), 0)
	respStreamFeed(t, tr, respStreamChatFinishChunk("tool_calls"), 0)
	tr.Finish()

	if !strings.Contains(logs.String(), "tool_calls") || !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("logs = %q, want a WARN about tool_calls without any function_call item", logs.String())
	}
	// 事件序列本身不应被日志影响。
	names := respStreamEventNames(respStreamEvents(t, rec.Body.String()))
	if names[len(names)-1] != "response.completed" {
		t.Fatalf("last event = %q, want response.completed", names[len(names)-1])
	}
}

// TestResponsesStreamTranslator_ToolCallsWithItemNoWarn 断言正常工具流不产生该 Warn。
func TestResponsesStreamTranslator_ToolCallsWithItemNoWarn(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	tr, _ := respStreamNew(t, "m")
	respStreamFeed(t, tr, respStreamChatToolChunk(0, "call_1", "alpha", `{}`), 0)
	respStreamFeed(t, tr, respStreamChatFinishChunk("tool_calls"), 0)
	tr.Finish()

	if logs.Len() != 0 {
		t.Fatalf("logs = %q, want no warning for a normal tool-call stream", logs.String())
	}
}

// TestResponsesStreamTranslator_FinishKeepsWroteFromDelta（I-5）断言 Wrote() 只反映增量驱动的
// 真实输出：即便上游没有 [DONE]、收尾事件全由 Finish() 补发，Wrote() 仍为 true。
func TestResponsesStreamTranslator_FinishKeepsWroteFromDelta(t *testing.T) {
	tr, rec := respStreamNew(t, "m")
	// 不给 [DONE]，但上游给了 finish_reason（已结束）：收尾事件由 Finish() 补发。
	respStreamFeed(t, tr, respStreamChatTextChunk("hello"), 0)
	respStreamFeed(t, tr, respStreamChatFinishChunk("stop"), 0)
	if !tr.Wrote() {
		t.Fatalf("Wrote() = false after a text delta, want true")
	}
	tr.Finish()
	if !tr.Wrote() {
		t.Fatalf("Wrote() = false after Finish(), want the flag to stay true")
	}
	names := respStreamEventNames(respStreamEvents(t, rec.Body.String()))
	if names[len(names)-1] != "response.completed" {
		t.Fatalf("last event = %q, want response.completed", names[len(names)-1])
	}
}

// TestResponsesRequestToChat_InvalidTools（复审 Important 1）断言 tools 存在且非 null、
// 但不是数组时报错，而不是静默丢弃后让 tool_choice 单独透传（部分上游会 400）。
func TestResponsesRequestToChat_InvalidTools(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"tools 是对象", `{"model":"m","input":"hi","tools":{"type":"x"},"tool_choice":"auto"}`},
		{"tools 是字符串", `{"model":"m","input":"hi","tools":"oops"}`},
		{"tools 是数字", `{"model":"m","input":"hi","tools":42}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := responsesRequestToChat([]byte(tc.body))
			if err == nil {
				t.Fatalf("responsesRequestToChat(%s) = %v, want an error", tc.body, out)
			}
			if !strings.Contains(err.Error(), "invalid tools") {
				t.Fatalf("error = %q, want it to mention \"invalid tools\"", err.Error())
			}
		})
	}
}

// TestResponsesRequestToChat_ToolsNullOrAbsent 断言 tools 缺失或为 null 时不报错也不写入。
func TestResponsesRequestToChat_ToolsNullOrAbsent(t *testing.T) {
	for _, body := range []string{`{"model":"m","input":"hi"}`, `{"model":"m","input":"hi","tools":null}`} {
		out, err := responsesRequestToChat([]byte(body))
		if err != nil {
			t.Fatalf("responsesRequestToChat(%s): unexpected error: %v", body, err)
		}
		if _, exists := out["tools"]; exists {
			t.Fatalf("responsesRequestToChat(%s): tools = %#v, want it absent", body, out["tools"])
		}
	}
}

// ---------------------------------------------------------------------------
// 终态快照（有状态路径落库用）：只由上游驱动，且消费即清
// ---------------------------------------------------------------------------

// TestResponsesStreamTranslator_TerminalIsUpstreamDrivenOnly：终态快照只在**上游**给出收尾
// （[DONE]）时产生；Finish() 补发的收尾是网关凭空合成的，上游并未结束，快照必须保持为空。
//
// 这条正是有状态路径「上游中断不落库」的依据：黑盒用例只能看到结果，这里钉住成因。
func TestResponsesStreamTranslator_TerminalIsUpstreamDrivenOnly(t *testing.T) {
	t.Run("upstream [DONE] produces a terminal snapshot", func(t *testing.T) {
		tr, _ := respStreamNew(t, "m")
		respStreamFeed(t, tr, respStreamSSE(`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`), 7)
		respStreamFeed(t, tr, respStreamSSE("[DONE]"), 7)
		if len(tr.TakeTerminal()) == 0 {
			t.Fatal("terminal snapshot must exist when the upstream closes the stream itself")
		}
	})

	t.Run("Finish-synthesized completion produces none", func(t *testing.T) {
		tr, rec := respStreamNew(t, "m")
		respStreamFeed(t, tr, respStreamSSE(`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`), 7)
		tr.Finish()
		if stream := rec.Body.String(); !strings.Contains(stream, "response.completed") {
			t.Fatalf("Finish must still emit response.completed to the client, got %q", stream)
		}
		if len(tr.TakeTerminal()) != 0 {
			t.Fatal("a Finish-synthesized completion must never become the stored result")
		}
	})
}

// TestResponsesStreamTranslator_TakeTerminalConsumesOnce：取走即清空，重复取为空。
func TestResponsesStreamTranslator_TakeTerminalConsumesOnce(t *testing.T) {
	tr, _ := respStreamNew(t, "m")
	respStreamFeed(t, tr, respStreamSSE("[DONE]"), 5)
	if len(tr.TakeTerminal()) == 0 {
		t.Fatal("first TakeTerminal must return the snapshot")
	}
	if got := tr.TakeTerminal(); got != nil {
		t.Fatalf("second TakeTerminal = %q, want nil (consume-once)", got)
	}
}

// TestResponsesStreamTranslator_FinishAfterUpstreamDoneIsNotSynthesized：上游已用 [DONE]
// 自行收尾后，调用方**照常**再调一次 Finish()（调用方无从判断上游是否已收尾），
// 这一轮仍是「上游驱动的正常轮」，不得被标记成合成收尾。
//
// 今天这没有可见后果（finalize 因 closed 早返回），但一旦 finalize 变成可重入、
// 或 terminal 改为延后写入，被误标的正常轮就会静默不落库。本用例把「只有真正补发才置位」
// 钉成不变量：去掉 Finish 的 closed/failed 前置，这里立刻变红。
func TestResponsesStreamTranslator_FinishAfterUpstreamDoneIsNotSynthesized(t *testing.T) {
	tr, _ := respStreamNew(t, "m")
	respStreamFeed(t, tr, respStreamSSE(`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`), 7)
	respStreamFeed(t, tr, respStreamSSE("[DONE]"), 7)

	tr.Finish() // 正常调用：调用方不知道上游已经收尾了

	if len(tr.TakeTerminal()) == 0 {
		t.Fatal("an upstream-closed round must keep its terminal snapshot after a redundant Finish")
	}

	// 失败流同理：已 failed 的流不再补发，也不该被标记为合成收尾。
	tr2, _ := respStreamNew(t, "m")
	respStreamFeed(t, tr2, respStreamSSE(`{"error":{"message":"boom"}}`), 5)
	tr2.Finish()
	if tr2.finishSynthesized {
		t.Fatal("Finish on an already-failed stream must not synthesize a completion")
	}
}

// ---------------------------------------------------------------------------
// Fix Round 4：流收尾状态机（C-2 / I-2 / C-1）
// ---------------------------------------------------------------------------

// respStreamLastFailure 断言流以一个失败类终态收尾（response.failed 或 error），返回事件名。
func respStreamLastTerminal(t *testing.T, events [][2]string) string {
	t.Helper()
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i][0] {
		case "response.completed", "response.failed", "error":
			return events[i][0]
		}
	}
	t.Fatalf("stream has no terminal event: %v", respStreamEventNames(events))
	return ""
}

// TestResponsesStreamTranslator_FailedStreamEmitsTerminalWithoutDone（C-2）：
// emitError 之后上游没有 [DONE]，Finish() 必须补出 response.failed，否则客户端永远等不到终态。
func TestResponsesStreamTranslator_FailedStreamEmitsTerminalWithoutDone(t *testing.T) {
	tr, rec := respStreamNew(t, "m")
	respStreamFeed(t, tr, respStreamChatTextChunk("half"), 0)
	tr.Write([]byte(`data: {"error":{"message":"upstream exploded","type":"server_error"}}` + "\n\n"))
	tr.Finish()

	events := respStreamEvents(t, rec.Body.String())
	if got := respStreamLastTerminal(t, events); got != "response.failed" {
		t.Fatalf("terminal event = %q, want response.failed (client would otherwise wait forever)", got)
	}
	failed := respStreamDataOf(t, events, "response.failed", 0)
	resp, ok := failed["response"].(map[string]any)
	if !ok {
		t.Fatalf("response.failed payload lacks a response object: %v", failed)
	}
	if resp["status"] != "failed" {
		t.Fatalf("response.failed status = %#v, want \"failed\"", resp["status"])
	}
	if resp["id"] != "resp_stream" {
		t.Fatalf("response.failed id = %#v, want the same resp_ id", resp["id"])
	}
}

// TestResponsesStreamTranslator_FailedStreamTerminalEmittedOnce（C-2）：
// error 之后上游又发了 [DONE]，终态仍只能出现一次（不重复）。
func TestResponsesStreamTranslator_FailedStreamTerminalEmittedOnce(t *testing.T) {
	tr, rec := respStreamNew(t, "m")
	respStreamFeed(t, tr, respStreamChatTextChunk("half"), 0)
	tr.Write([]byte(`data: {"error":{"message":"boom"}}` + "\n\n"))
	tr.Write([]byte("data: [DONE]\n\n"))
	tr.Finish()
	tr.Finish()

	events := respStreamEvents(t, rec.Body.String())
	if n := len(respStreamAllOf(events, "response.failed")); n > 1 {
		t.Fatalf("response.failed emitted %d times, want at most 1: %v", n, respStreamEventNames(events))
	}
	if n := len(respStreamAllOf(events, "error")); n != 1 {
		t.Fatalf("error event count = %d, want 1", n)
	}
	if got := respStreamLastTerminal(t, events); got == "response.completed" {
		t.Fatalf("failed stream must not end with response.completed: %v", respStreamEventNames(events))
	}
}

// TestResponsesStreamTranslator_FailedStreamNoCompleted：失败流绝不能发 response.completed。
func TestResponsesStreamTranslator_FailedStreamNoCompleted(t *testing.T) {
	tr, rec := respStreamNew(t, "m")
	tr.Write([]byte(`data: {"error":{"message":"boom"}}` + "\n\n"))
	tr.Finish()

	events := respStreamEvents(t, rec.Body.String())
	if n := len(respStreamAllOf(events, "response.completed")); n != 0 {
		t.Fatalf("response.completed emitted %d times on a failed stream: %v", n, respStreamEventNames(events))
	}
}

// TestResponsesStreamTranslator_CleanEOFAfterContentIsCompleted（I-2 修正）：
// 上游把内容发完就直接关连接（既不发 [DONE] 也不发 finish_reason）属于**干净 EOF**，
// 是合理的流结束 —— 必须判完成，不能误伤正常用户（很多 chat 兼容上游就是这样收尾的）。
func TestResponsesStreamTranslator_CleanEOFAfterContentIsCompleted(t *testing.T) {
	tr, rec := respStreamNew(t, "m")
	respStreamFeed(t, tr, respStreamChatTextChunk("hello"), 0)
	// 上游复述到此为止：没有 [DONE]，没有 finish_reason，也没有读错误。
	tr.Finish()

	events := respStreamEvents(t, rec.Body.String())
	if got := respStreamLastTerminal(t, events); got != "response.completed" {
		t.Fatalf("terminal event = %q, want response.completed for a clean EOF", got)
	}
	if n := len(respStreamAllOf(events, "response.failed")); n != 0 {
		t.Fatalf("response.failed emitted %d times on a clean EOF: %v", n, respStreamEventNames(events))
	}
	resp := respStreamCompleted(t, events)
	if resp["status"] != "completed" {
		t.Fatalf("status = %#v, want \"completed\"", resp["status"])
	}
}

// TestResponsesStreamTranslator_ReadErrorAfterContentIsFailed（I-2）：读取出错（连接重置、
// 单行超限、超时）说明内容可能不完整 —— 必须以 response.failed 收尾，不能谎报 completed。
func TestResponsesStreamTranslator_ReadErrorAfterContentIsFailed(t *testing.T) {
	tr, rec := respStreamNew(t, "m")
	respStreamFeed(t, tr, respStreamChatTextChunk("half"), 0)
	// 调用方（ingress）在 ChatCompletionStream 返回 err != nil 时走 Abort。
	tr.Abort("upstream stream read error: token too long")

	events := respStreamEvents(t, rec.Body.String())
	if n := len(respStreamAllOf(events, "response.completed")); n != 0 {
		t.Fatalf("read error reported response.completed %d times: %v", n, respStreamEventNames(events))
	}
	if got := respStreamLastTerminal(t, events); got != "response.failed" {
		t.Fatalf("terminal event = %q, want response.failed after a read error", got)
	}
	failed := respStreamDataOf(t, events, "response.failed", 0)
	resp, _ := failed["response"].(map[string]any)
	if resp["status"] != "failed" {
		t.Fatalf("status = %#v, want \"failed\"", resp["status"])
	}
	// 已产出的内容仍要保留在终态里（客户端能看到自己收到了多少）。
	if output, _ := resp["output"].([]any); len(output) == 0 {
		t.Fatalf("response.failed output = %#v, want the partial content retained", resp["output"])
	}
}

// TestResponsesStreamTranslator_AbortIdempotent：Abort 之后 Finish() 不得再补终态，
// 重复 Abort 也不得重复发（终态至多一次）。
func TestResponsesStreamTranslator_AbortIdempotent(t *testing.T) {
	tr, rec := respStreamNew(t, "m")
	respStreamFeed(t, tr, respStreamChatTextChunk("half"), 0)
	tr.Abort("boom")
	tr.Abort("boom again")
	tr.Finish()

	events := respStreamEvents(t, rec.Body.String())
	if n := len(respStreamAllOf(events, "response.failed")); n != 1 {
		t.Fatalf("response.failed emitted %d times, want exactly 1: %v", n, respStreamEventNames(events))
	}
	if n := len(respStreamAllOf(events, "response.completed")); n != 0 {
		t.Fatalf("response.completed emitted after Abort: %v", respStreamEventNames(events))
	}
}

// TestResponsesStreamTranslator_AbortAfterNormalFinishIsNoop：上游已正常收尾后再报读错误
// （例如收尾后才关连接）不得改写终态。
func TestResponsesStreamTranslator_AbortAfterNormalFinishIsNoop(t *testing.T) {
	tr, rec := respStreamNew(t, "m")
	respStreamFeed(t, tr, respStreamChatTextChunk("hello"), 0)
	tr.Finish()
	tr.Abort("late read error")

	events := respStreamEvents(t, rec.Body.String())
	if got := respStreamLastTerminal(t, events); got != "response.completed" {
		t.Fatalf("terminal event = %q, want the earlier response.completed", got)
	}
}

// TestResponsesStreamTranslator_InterruptedBeforeContentStaysMinimal（I-2 边界）：
// 零增量 + Finish() 仍走最小合法序列（completed），不因为「中断」就把空流报成 failed。
func TestResponsesStreamTranslator_InterruptedBeforeContentStaysMinimal(t *testing.T) {
	tr, rec := respStreamNew(t, "m")
	tr.Finish()

	names := respStreamEventNames(respStreamEvents(t, rec.Body.String()))
	want := "response.created,response.in_progress,response.completed"
	if strings.Join(names, ",") != want {
		t.Fatalf("event types = %v, want %s", names, want)
	}
}

// TestResponsesStreamTranslator_TerminalFailures 逐个钉住终态事件「编码/写出失败」的三条路径。
//
// json.Marshal 对 map[string]any 几乎只会在「含无法序列化的 Go 值」时失败（NaN/Inf 会被静默换成
// null），所以这里直接用不可序列化值构造 response 对象，覆盖原来只能靠真实事故触发的分支：
//  1. 编码失败 → 记 Error 且标记 finishSynthesized（C-1：不静默、不当作正常轮落库）；
//  2. 编码失败 → finalize 仍要补出终态事件（C-2：失败流不能没有终态）；
//  3. 写出阶段编码失败 → 同样记 Error，且退化为只带错误信息的终态（客户端仍有终态可等）。
func TestResponsesStreamTranslator_TerminalFailures(t *testing.T) {
	t.Run("终态 response 编码失败记 Error 且不当作正常轮", func(t *testing.T) {
		var logs bytes.Buffer
		restore := captureSlog(&logs)
		defer restore()

		tr, _ := respStreamNew(t, "m")
		// 让 usage 进入终态，再把 usage 本身换成不可序列化的值：单元级函数（func）无法被
		// 外层 map 挡住，保证失败发生在「response 原文编码」这一步而不是被 JSON 省略。
		tr.usage = map[string]any{}
		tr.ensureOpened()
		tr.handleTextDelta("hi")
		tr.usage = map[string]any{"input_tokens_details": func() {}}
		// 自指的 response 对象不可序列化（json 检测到循环引用）→ 事件体被降级，但 response 的
		// 原文必须记 Error 且判为不可信（不落库），否则这一轮会静默变成父子链上的 404。
		unencodable := map[string]any{"output": make(chan int)} // chan 无法被 JSON 编码
		tr.emitTerminal("response.completed", unencodable)

		if !strings.Contains(logs.String(), "level=ERROR") {
			t.Fatalf("logs = %q, want an ERROR for the unencodable response body", logs.String())
		}
		if !tr.finishSynthesized {
			t.Fatalf("finishSynthesized = false, want true so the round is not persisted as a normal one")
		}
		if tr.terminal != nil {
			t.Fatalf("terminal = %q, want nil for an unencodable response body", string(tr.terminal))
		}
	})

	t.Run("编码失败时失败流仍有终态", func(t *testing.T) {
		var logs bytes.Buffer
		restore := captureSlog(&logs)
		defer restore()

		tr, rec := respStreamNew(t, "m")
		tr.ensureOpened()
		tr.handleTextDelta("half")
		tr.terminal = nil
		// 让终态承载不可序列化的错误对象：emitError 会把它原样放进 response.failed。
		tr.failure = func() {}
		tr.fail("upstream returned an error event")

		if got := respStreamLastTerminal(t, respStreamEvents(t, rec.Body.String())); got != "response.failed" {
			t.Fatalf("terminal event = %q, want response.failed", got)
		}
		if !strings.Contains(logs.String(), "level=ERROR") {
			t.Fatalf("logs = %q, want an ERROR for the unencodable terminal", logs.String())
		}
	})

	t.Run("写出阶段编码失败仍给终态", func(t *testing.T) {
		var logs bytes.Buffer
		restore := captureSlog(&logs)
		defer restore()

		tr, rec := respStreamNew(t, "m")
		tr.ensureOpened()
		tr.handleTextDelta("half")
		tr.emitTerminal("response.completed", map[string]any{"bad": func() {}})

		if !strings.Contains(logs.String(), "level=ERROR") {
			t.Fatalf("logs = %q, want an ERROR when the event itself cannot be encoded", logs.String())
		}
		var sawCompleted bool
		for _, ev := range respStreamEvents(t, rec.Body.String()) {
			if ev[0] == "response.completed" {
				sawCompleted = true
			}
		}
		if !sawCompleted {
			t.Fatalf("no terminal event written after an encode failure: %s", rec.Body.String())
		}
	})
}

// captureSlog 把默认 logger 临时重定向到 buf，返回恢复函数。
func captureSlog(buf *bytes.Buffer) func() {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelError})))
	return func() { slog.SetDefault(prev) }
}
