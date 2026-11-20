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
	"encoding/json"
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
	last := events[len(events)-1]
	if last[0] != "error" {
		t.Fatalf("last event = %q, want \"error\"", last[0])
	}
	errPayload := respStreamDataOf(t, events, "error", 0)
	errObj, ok := errPayload["error"].(map[string]any)
	if !ok || errObj["message"] != "upstream exploded" {
		t.Fatalf("error payload = %#v, want the upstream message", errPayload)
	}
}
