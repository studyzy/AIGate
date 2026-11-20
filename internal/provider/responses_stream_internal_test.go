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

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件覆盖 responses 流式翻译中工具调用相关的回归用例（review 指出的 C1/C2/I1/I2/I3/M3/M4/M7）。
// 关键背景：Responses 上游在 `response.output_item.added` 之后再逐块发
// `response.function_call_arguments.delta`，两者并存是正常序列，不是边角情况。

// streamChunk 是解析后的一条 chat 流式分片。
type streamChunk struct {
	Object string `json:"object"`
	Error  *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// parseChatStream 解析 chat 流式输出；返回全部分片、是否以 [DONE] 收尾、以及各 tool 的累计参数。
func parseChatStream(t *testing.T, raw string) (chunks []streamChunk, sawDone bool, toolArgs map[int]string, toolNames map[int]string) {
	t.Helper()
	toolArgs = map[int]string{}
	toolNames = map[int]string{}
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			sawDone = true
			continue
		}
		var c streamChunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("分片不是 JSON: %v (%s)", err, data)
		}
		chunks = append(chunks, c)
		for _, ch := range c.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				toolArgs[tc.Index] += tc.Function.Arguments
				if tc.Function.Name != "" {
					toolNames[tc.Index] = tc.Function.Name
				}
			}
		}
	}
	return chunks, sawDone, toolArgs, toolNames
}

// responsesSSE 用 data 行拼装 Responses SSE（事件名与 data 行都写出，贴近真实上游）。
func responsesSSE(pairs ...string) string {
	var b strings.Builder
	for _, p := range pairs {
		b.WriteString("data: ")
		b.WriteString(p)
		b.WriteString("\n\n")
	}
	return b.String()
}

// TestResponsesStreamToolCall_AddedAndDeltaNotDuplicated（C1）
// added 事件已带完整 arguments、随后上游**仍然**发 arguments.delta 时，参数必须只出现一次；
// 否则累加结果会是 `{"a":1}{"a":1}` 这种非法 JSON。
func TestResponsesStreamToolCall_AddedAndDeltaNotDuplicated(t *testing.T) {
	const sse = `data: {"type":"response.output_item.added","output_index":0,` +
		`"item":{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":""}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"a\":"}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"1}"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}` + "\n\n"

	rec := httptest.NewRecorder()
	usage, err := responsesStreamToChat(rec, rec, strings.NewReader(sse))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage == nil || usage.CompletionTokens != 5 {
		t.Fatalf("usage = %+v, want completion 5", usage)
	}

	_, sawDone, toolArgs, toolNames := parseChatStream(t, rec.Body.String())
	if !sawDone {
		t.Fatalf("未以 [DONE] 收尾:\n%s", rec.Body.String())
	}
	if got := toolArgs[0]; got != `{"a":1}` {
		t.Fatalf("tool index 0 arguments = %q, want %q（不得重复拼接）", got, `{"a":1}`)
	}
	if toolNames[0] != "get_weather" {
		t.Fatalf("tool index 0 name = %q, want get_weather", toolNames[0])
	}
}

// TestResponsesStreamToolCall_AddedCarriesFullArguments（C1 变体）
// 兼容「上游只在 added 里给完整参数、之后再无任何 arguments.delta」的上游实现。
func TestResponsesStreamToolCall_AddedCarriesFullArguments(t *testing.T) {
	const sse = `data: {"type":"response.output_item.added","output_index":0,` +
		`"item":{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"a\":1}"}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"

	rec := httptest.NewRecorder()
	if _, err := responsesStreamToChat(rec, rec, strings.NewReader(sse)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, _, toolArgs, _ := parseChatStream(t, rec.Body.String())
	if got := toolArgs[0]; got != `{"a":1}` {
		t.Fatalf("tool index 0 arguments = %q, want %q", got, `{"a":1}`)
	}
}

// TestResponsesStreamToolCall_ParallelOutOfOrder（C2 / M7）
// 两个并行工具、事件乱序（工具1 的 delta 先于其 added 到达）时，
// 参数不得串到错误工具、index 不得被复用。
func TestResponsesStreamToolCall_ParallelOutOfOrder(t *testing.T) {
	const sse = `data: {"type":"response.output_item.added","output_index":0,` +
		`"item":{"type":"function_call","call_id":"call_0","name":"alpha","arguments":""}}` + "\n\n" +
		// 工具1 的 delta 先到（added 还没来）——不得臆造出与工具0 相同的 index。
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"b\":"}` + "\n\n" +
		`data: {"type":"response.output_item.added","output_index":1,` +
		`"item":{"type":"function_call","call_id":"call_1","name":"beta","arguments":""}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"a\":"}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"2}"}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"1}"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}` + "\n\n"

	rec := httptest.NewRecorder()
	if _, err := responsesStreamToChat(rec, rec, strings.NewReader(sse)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chunks, sawDone, toolArgs, toolNames := parseChatStream(t, rec.Body.String())
	if !sawDone {
		t.Fatalf("未以 [DONE] 收尾")
	}

	// 两个工具必须落在两个不同 index 上。
	if len(toolArgs) != 2 {
		t.Fatalf("tool 分片 index 数 = %d (%v), want 2 个不同 index", len(toolArgs), toolArgs)
	}
	byName := map[string]string{}
	for idx, name := range toolNames {
		byName[name] = toolArgs[idx]
	}
	if byName["alpha"] != `{"a":1}` {
		t.Fatalf("alpha 参数 = %q, want %q（全部 toolArgs=%v names=%v）", byName["alpha"], `{"a":1}`, toolArgs, toolNames)
	}
	if byName["beta"] != `{"b":2}` {
		t.Fatalf("beta 参数 = %q, want %q（全部 toolArgs=%v names=%v）", byName["beta"], `{"b":2}`, toolArgs, toolNames)
	}

	// 收尾分片必须是 tool_calls。
	last := chunks[len(chunks)-1]
	if len(last.Choices) == 0 || last.Choices[0].FinishReason == nil || *last.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %+v, want tool_calls", last.Choices)
	}
}

// TestResponsesStreamToolCall_MissingOutputIndex（C2 根因）
// output_index 缺失时不能退化成 0 与真实 index 0 冲突：两条不同 call_id 的工具必须分开。
func TestResponsesStreamToolCall_MissingOutputIndex(t *testing.T) {
	const sse = `data: {"type":"response.output_item.added",` +
		`"item":{"type":"function_call","call_id":"call_a","name":"alpha","arguments":""}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","delta":"{\"a\":1}"}` + "\n\n" +
		`data: {"type":"response.output_item.added",` +
		`"item":{"type":"function_call","call_id":"call_b","name":"beta","arguments":""}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","delta":"{\"b\":2}"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"

	rec := httptest.NewRecorder()
	if _, err := responsesStreamToChat(rec, rec, strings.NewReader(sse)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, _, toolArgs, toolNames := parseChatStream(t, rec.Body.String())
	if len(toolArgs) != 2 {
		t.Fatalf("tool index 数 = %d (%v names=%v), want 2（缺 output_index 不得让两个工具共享 index）", len(toolArgs), toolArgs, toolNames)
	}
}

// TestResponsesStreamToolCall_NoCrossContamination（M7 单工具流式）
// 单工具流式：所有参数分片必须归入同一个 index，且拼出的 JSON 合法。
func TestResponsesStreamToolCall_NoCrossContamination(t *testing.T) {
	sse := "" +
		`data: {"type":"response.output_item.added","output_index":0,` +
		`"item":{"type":"function_call","call_id":"call_x","name":"do_it","arguments":""}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"k\":"}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"[1,"}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"2]}"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":2,"output_tokens":7,"total_tokens":9}}}` + "\n\n"

	rec := httptest.NewRecorder()
	if _, err := responsesStreamToChat(rec, rec, strings.NewReader(sse)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, _, toolArgs, _ := parseChatStream(t, rec.Body.String())
	if len(toolArgs) != 1 {
		t.Fatalf("tool index 数 = %d, want 1（%v）", len(toolArgs), toolArgs)
	}
	if !json.Valid([]byte(toolArgs[0])) {
		t.Fatalf("拼接后的 arguments 不是合法 JSON: %q", toolArgs[0])
	}
}

// TestResponsesStream_ResponseFailedEmitsError（I1）
// response.failed 时必须以 chat 客户端能识别的 error 分片透出上游错误信息，
// 否则客户端无法区分「成功但无输出」与「上游失败」。
func TestResponsesStream_ResponseFailedEmitsError(t *testing.T) {
	const sse = `data: {"type":"response.failed","response":{"status":"failed",` +
		`"error":{"code":"server_error","message":"upstream exploded"}}}` + "\n\n"

	rec := httptest.NewRecorder()
	if _, err := responsesStreamToChat(rec, rec, strings.NewReader(sse)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chunks, sawDone, _, _ := parseChatStream(t, rec.Body.String())
	if !sawDone {
		t.Fatalf("仍须以 [DONE] 收尾")
	}

	found := false
	for _, c := range chunks {
		if c.Error != nil {
			found = true
			if !strings.Contains(c.Error.Message, "upstream exploded") {
				t.Fatalf("error.message = %q, want 含上游原文", c.Error.Message)
			}
		}
	}
	if !found {
		t.Fatalf("缺少 error 分片:\n%s", rec.Body.String())
	}
}

// TestResponsesStream_MissingCompletedWarns（I2 / I3）
// 上游流没有终止事件时不得静默当成功：必须记一条带结构化字段的 slog.Warn。
func TestResponsesStream_MissingCompletedWarns(t *testing.T) {
	logs := captureLogs(t)

	const sse = `data: {"type":"response.created","response":{"id":"resp_x"}}` + "\n\n" +
		`data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n"

	rec := httptest.NewRecorder()
	if _, err := responsesStreamToChat(rec, rec, strings.NewReader(sse)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	warn := logs.findWarn(t, "responses upstream stream ended without terminal event")
	if warn == nil {
		t.Fatalf("缺少终止事件告警:\n%s", logs.buf.String())
	}
	if warn["message_id"] != "resp_x" {
		t.Fatalf("warn message_id = %v, want resp_x", warn["message_id"])
	}
	if _, ok := warn["usage_seen"]; !ok {
		t.Fatalf("warn 缺少结构化字段 usage_seen: %v", warn)
	}
}

// TestResponsesStream_CompletedDoesNotWarn 确认正常路径不产生该告警（避免噪音）。
func TestResponsesStream_CompletedDoesNotWarn(t *testing.T) {
	logs := captureLogs(t)

	const sse = `data: {"type":"response.completed","response":{"status":"completed",` +
		`"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"

	rec := httptest.NewRecorder()
	if _, err := responsesStreamToChat(rec, rec, strings.NewReader(sse)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if warn := logs.findWarn(t, "responses upstream stream ended without terminal event"); warn != nil {
		t.Fatalf("正常完成的流不应告警: %v", warn)
	}
}

// TestResponsesStream_DataWithoutSpace（M1）
// 兼容 `data:{...}`（冒号后无空格）的行；当前实现会整行丢弃。
func TestResponsesStream_DataWithoutSpace(t *testing.T) {
	const sse = `data:{"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
		`data:{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}` + "\n\n"

	rec := httptest.NewRecorder()
	usage, err := responsesStreamToChat(rec, rec, strings.NewReader(sse))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage == nil || usage.PromptTokens != 2 {
		t.Fatalf("usage = %+v, want prompt 2（data: 无空格的行也被处理）", usage)
	}
	var content strings.Builder
	chunks, sawDone, _, _ := parseChatStream(t, rec.Body.String())
	for _, c := range chunks {
		for _, ch := range c.Choices {
			content.WriteString(ch.Delta.Content)
		}
	}
	if content.String() != "hi" {
		t.Fatalf("content = %q, want hi", content.String())
	}
	if !sawDone {
		t.Fatalf("未以 [DONE] 收尾")
	}
}

// TestChatUsageFromResponses_MatchesUsageRenderer（M4）
// chatUsageFromResponses 必须复用 chatUsageFromResponsesUsage 的渲染，避免两份口径漂移。
func TestChatUsageFromResponses_MatchesUsageRenderer(t *testing.T) {
	data := `{"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,` +
		`"input_tokens_details":{"cached_tokens":4,"cache_write_tokens":6}}}`
	got, want := chatUsageFromResponses(data), chatUsageFromResponsesUsage(extractResponsesUsage(data, nil))
	if len(got) != len(want) {
		t.Fatalf("字段数不同: got %v want %v", got, want)
	}
	for k, wv := range want {
		gv, ok := got[k]
		if !ok {
			t.Fatalf("缺字段 %q", k)
		}
		if !jsonEqual(gv, wv) {
			t.Fatalf("字段 %q: got %v want %v", k, gv, wv)
		}
	}
}

// TestIntField_ReportsPresence（M3）
// intField 必须用 (int, bool) 区分「值为 0」与「字段缺失」，这是 C2 的根因。
func TestIntField_ReportsPresence(t *testing.T) {
	m := map[string]interface{}{"zero": float64(0), "two": float64(2), "str": "x"}
	if v, ok := intField(m, "zero"); v != 0 || !ok {
		t.Fatalf("zero: got (%d,%v), want (0,true)", v, ok)
	}
	if v, ok := intField(m, "two"); v != 2 || !ok {
		t.Fatalf("two: got (%d,%v), want (2,true)", v, ok)
	}
	if v, ok := intField(m, "missing"); v != 0 || ok {
		t.Fatalf("missing: got (%d,%v), want (0,false)", v, ok)
	}
	if v, ok := intField(m, "str"); v != 0 || ok {
		t.Fatalf("str: got (%d,%v), want (0,false)", v, ok)
	}
}

// --- 测试辅助 ---

// logCapture 捕获 slog.Default() 的输出，供告警类用例断言。
type logCapture struct{ buf *bytes.Buffer }

// captureLogs 把默认 slog handler 换成内存缓冲（JSON 行），返回捕获器。
func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logCapture{buf: buf}
}

// records 把捕获到的 JSON 日志行解析为扁平 map 列表。
func (c *logCapture) records(t *testing.T) []map[string]interface{} {
	t.Helper()
	var out []map[string]interface{}
	for _, line := range strings.Split(strings.TrimSpace(c.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("日志行不是 JSON: %v (%s)", err, line)
		}
		out = append(out, rec)
	}
	return out
}

// findWarn 查找指定 message 的首条 WARN 记录，找不到返回 nil。
func (c *logCapture) findWarn(t *testing.T, msg string) map[string]interface{} {
	t.Helper()
	for _, rec := range c.records(t) {
		if rec["level"] == "WARN" && rec["msg"] == msg {
			return rec
		}
	}
	return nil
}

// jsonEqual 比较两个值序列化后是否等价（用于 map/slice 值比较）。
func jsonEqual(a, b interface{}) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}
