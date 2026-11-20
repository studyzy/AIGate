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

// 本文件实现 Responses API 入口侧的协议转换（纯函数，不涉及 HTTP 编排）：
//   - responsesRequestToChat：Responses 请求体 → OpenAI chat completions 请求体（spec §5.3.1）；
//   - chatResponseToResponses：chat 响应体 → Responses 响应体（spec §5.3.2）；
//   - responsesStreamTranslator：chat SSE 增量 → Responses 事件序列（spec §5.5）。
//
// 与 provider/responses.go（§5.3.3，Responses 上游 → chat 形状）方向相反，不可混用。
// 转换是有损的：Responses 专有结构（reasoning item、encrypted_content、内置工具输出、
// annotations 等）在转换路径上被丢弃。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const (
	// respObjectResponse 是 Responses 响应的 object 取值。
	respObjectResponse = "response"
	// respStatusCompleted 是终态「正常完成」。
	respStatusCompleted = "completed"
	// respStatusIncomplete 是终态「未完成」（如触达 max_output_tokens）。
	respStatusIncomplete = "incomplete"
	// respStatusInProgress 是流式过程中的状态。
	respStatusInProgress = "in_progress"
)

// --- 请求方向：responses → chat ---

// responsesRequestToChat 把 Responses 请求体转换为 OpenAI chat completions 请求体。
// 校验：model 必填（缺失返回英文错误）。
func responsesRequestToChat(body []byte) (map[string]any, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, errors.New("invalid request body")
	}
	if req == nil {
		return nil, errors.New("invalid request body")
	}
	model, _ := req["model"].(string)
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("missing model")
	}

	out := map[string]any{"model": model}

	// instructions 置于最前，作为首条 system 消息。
	messages := make([]any, 0, 4)
	if instructions, _ := req["instructions"].(string); instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	// input：字符串即单条 user 消息；数组逐项转换。
	switch input := req["input"].(type) {
	case string:
		messages = append(messages, map[string]any{"role": "user", "content": input})
	case []any:
		messages = append(messages, responsesItemsToMessages(input)...)
	}
	if len(messages) > 0 {
		out["messages"] = messages
	}

	// 标量字段透传（存在才写，避免改变无关字段语义）。
	for _, key := range []string{"temperature", "top_p", "parallel_tool_calls", "metadata", "tool_choice"} {
		if v, exists := req[key]; exists {
			out[key] = v
		}
	}
	// max_output_tokens → max_tokens。
	if v, exists := req["max_output_tokens"]; exists {
		out["max_tokens"] = v
	}
	// text.format → response_format。
	if text, ok := req["text"].(map[string]any); ok {
		if format, exists := text["format"]; exists && format != nil {
			out["response_format"] = format
		}
	}
	// 扁平 tool → 嵌套 {type:function, function:{...}}。
	if tools := responsesToolsToChat(req["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	// 流式：让上游在末片带上 usage，供记账与终态事件使用。
	if stream, _ := req["stream"].(bool); stream {
		out["stream"] = true
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	return out, nil
}

// responsesItemsToMessages 把 Responses input item 数组转换为 chat messages。
//
//   - item 带 role（user/system/developer/assistant）→ 同 role 的 message；
//   - function_call → assistant 消息的 tool_calls（连续的相邻调用合并进同一条消息）；
//   - function_call_output → 独立 role=tool 消息；
//   - 其余 Responses 专有 item（reasoning、内置工具输出等）丢弃。
func responsesItemsToMessages(items []any) []any {
	var out []any
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		itemType, _ := item["type"].(string)
		switch {
		case itemType == "function_call":
			call := map[string]any{
				"id":   item["call_id"],
				"type": "function",
				"function": map[string]any{
					"name":      item["name"],
					"arguments": responsesArguments(item["arguments"]),
				},
			}
			// 连续调用合并进前一条 assistant tool_calls 消息，与 OpenAI 形态一致。
			if n := len(out); n > 0 {
				if prev, ok := out[n-1].(map[string]any); ok && prev["role"] == "assistant" {
					if calls, ok := prev["tool_calls"].([]any); ok {
						prev["tool_calls"] = append(calls, call)
						continue
					}
				}
			}
			out = append(out, map[string]any{
				"role":       "assistant",
				"content":    nil,
				"tool_calls": []any{call},
			})
		case itemType == "function_call_output":
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": item["call_id"],
				"content":      responsesToolOutput(item["output"]),
			})
		case itemType == "" || itemType == "message":
			if role, _ := item["role"].(string); role != "" {
				out = append(out, map[string]any{
					"role":    role,
					"content": responsesContentToChat(item["content"]),
				})
			}
		}
	}
	return out
}

// responsesContentToChat 转换 message item 的 content：
// 字符串原样；part 数组逐项映射（input_text→text、input_image→image_url、input_file→file）。
func responsesContentToChat(v any) any {
	switch content := v.(type) {
	case string:
		return content
	case []any:
		parts := make([]any, 0, len(content))
		for _, raw := range content {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			partType, _ := part["type"].(string)
			switch partType {
			case "input_text", "output_text", "text":
				parts = append(parts, map[string]any{"type": "text", "text": part["text"]})
			case "input_image":
				imageURL := part["image_url"]
				if imageURL == nil {
					imageURL = part["file_id"]
				}
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": imageURL},
				})
			case "input_file":
				file := map[string]any{"type": "file"}
				for _, key := range []string{"file_id", "filename", "file_data"} {
					if value, exists := part[key]; exists {
						file[key] = value
					}
				}
				parts = append(parts, file)
			default:
				// 未知 part：保留原始类型与文本，交给上游判断。
				parts = append(parts, part)
			}
		}
		return parts
	default:
		return v
	}
}

// responsesArguments 取 function_call 的 arguments 字符串：
// 字符串原样；其余形态（如对象）序列化为 JSON；缺失时为 "{}"。
func responsesArguments(v any) string {
	switch args := v.(type) {
	case nil:
		return "{}"
	case string:
		if args == "" {
			return "{}"
		}
		return args
	default:
		b, err := json.Marshal(args)
		if err != nil {
			return "{}"
		}
		return string(b)
	}
}

// responsesToolOutput 把 function_call_output 的 output 字符串化：
// 字符串原样；part 数组拼接 text 块；其余形态 JSON 序列化。
func responsesToolOutput(v any) string {
	switch output := v.(type) {
	case nil:
		return ""
	case string:
		return output
	case []any:
		var parts []string
		for _, raw := range output {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
		return responsesJSONString(output)
	default:
		return responsesJSONString(output)
	}
}

// responsesToolsToChat 把扁平的 Responses function 工具转为 chat 嵌套形态。
// 非 function 类型（内置工具）在 chat 上游没有对应物，丢弃。
func responsesToolsToChat(v any) []any {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []any
	for _, raw := range items {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if toolType, _ := tool["type"].(string); toolType != "function" {
			continue
		}
		name, _ := tool["name"].(string)
		if name == "" {
			continue
		}
		fn := map[string]any{"name": name}
		if desc, ok := tool["description"].(string); ok && desc != "" {
			fn["description"] = desc
		}
		if schema, exists := tool["parameters"]; exists && schema != nil {
			fn["parameters"] = schema
		} else {
			fn["parameters"] = map[string]any{"type": "object"}
		}
		if strict, ok := tool["strict"]; ok {
			fn["strict"] = strict
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// --- 响应方向：chat → responses ---

// chatResponseToResponses 把 chat completion 响应体转换为 Responses 响应体。
// requestedModel 是客户端请求的模型名（回显）；respID 是网关生成的 resp_... ID。
func chatResponseToResponses(chatBody []byte, requestedModel, respID string) ([]byte, error) {
	choice, usage, err := responsesChatChoice(chatBody)
	if err != nil {
		return nil, err
	}

	finishReason, _ := choice["finish_reason"].(string)
	out := map[string]any{
		"id":     respID,
		"object": respObjectResponse,
		"model":  requestedModel,
		"status": respStatusCompleted,
		"output": []any{},
	}
	// finish_reason="length" → incomplete（截断必须显式告知，不静默当作成功）。
	if finishReason == "length" {
		out["status"] = respStatusIncomplete
		out["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}

	message, _ := choice["message"].(map[string]any)
	if message != nil {
		output := []any{}
		if text := responsesMessageText(message["content"]); text != "" {
			output = append(output, responsesMessageItem(text))
		}
		toolCalls, _ := message["tool_calls"].([]any)
		for _, raw := range toolCalls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			item := map[string]any{"type": "function_call", "call_id": call["id"]}
			if fn, ok := call["function"].(map[string]any); ok {
				item["name"] = fn["name"]
				item["arguments"] = responsesArguments(fn["arguments"])
			}
			output = append(output, item)
		}
		out["output"] = output
	}
	if usage != nil {
		out["usage"] = responsesUsageFromChat(usage)
	}
	return json.Marshal(out)
}

// responsesChatChoice 解析 chat 响应并取首个 choice。
func responsesChatChoice(chatBody []byte) (map[string]any, map[string]any, error) {
	var chat map[string]any
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		return nil, nil, errors.New("invalid upstream response")
	}
	choices, ok := chat["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil, nil, errors.New("invalid upstream response")
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return nil, nil, errors.New("invalid upstream response")
	}
	usage, _ := chat["usage"].(map[string]any)
	return choice, usage, nil
}

// responsesMessageText 从 chat message.content 提取文本（字符串或 parts 数组）。
func responsesMessageText(v any) string {
	switch content := v.(type) {
	case string:
		return content
	case []any:
		var parts []string
		for _, raw := range content {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "")
	default:
		return ""
	}
}

// responsesMessageItem 构造一个 assistant message output item。
func responsesMessageItem(text string) map[string]any {
	return map[string]any{
		"type":    "message",
		"role":    "assistant",
		"content": responsesMessageItemContent(text),
	}
}

// responsesUsageFromChat 映射用量（spec §5.3.2）。
// 与 chat 侧 sanitizeUsage 的区别：这里的空值是「字段缺失」而非 null，因此显式补 0。
func responsesUsageFromChat(usage map[string]any) map[string]any {
	out := map[string]any{
		"input_tokens":          responsesInt(usage, "prompt_tokens"),
		"output_tokens":         responsesInt(usage, "completion_tokens"),
		"total_tokens":          responsesInt(usage, "total_tokens"),
		"input_tokens_details":  map[string]any{"cached_tokens": 0},
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
	}
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		out["input_tokens_details"] = map[string]any{"cached_tokens": responsesInt(details, "cached_tokens")}
	}
	if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
		out["output_tokens_details"] = map[string]any{"reasoning_tokens": responsesInt(details, "reasoning_tokens")}
	}
	return out
}

// responsesInt 取对象中的整数字段，缺失或非数值时返回 0。
func responsesInt(obj map[string]any, key string) int {
	if n, ok := anthJSONInt(obj[key]); ok {
		return n
	}
	return 0
}

// --- 流式方向：chat chunk → Responses 事件序列 ---

// responsesStreamTranslator 把 chat 形状的 SSE 增量合成为 Responses 事件序列并写入目标 writer。
//
// 纯文本输出的事件序列（spec §5.5）：
//
//	response.created → response.in_progress → response.output_item.added
//	→ response.content_part.added → response.output_text.delta* → response.output_text.done
//	→ response.content_part.done → response.output_item.done → response.completed
//
// 工具调用（delta.tool_calls[]，spec §5.5.1）为每个工具额外合成：
//
//	response.output_item.added（function_call）→ response.function_call_arguments.delta*
//	→ response.function_call_arguments.done → response.output_item.done
//
// 一个响应可同时包含一个 message item 与多个 function_call item；output_index 按 item
// 首次出现的顺序递增，message 与各 function_call 互不共享。每个 item 独立累积自己的内容。
//
// 与 chat 流不同，Responses 流以 response.completed 为界，绝不写 "[DONE]"。
// 收尾事件由上游 [DONE]、调用方 Finish() 触发。
type responsesStreamTranslator struct {
	w       http.ResponseWriter
	flusher http.Flusher
	model   string
	respID  string

	// buf 缓存尚未构成完整事件（未遇到空行边界）的字节。
	buf []byte

	// wrote 标记是否已向客户端写出过任何字节（供调用方判断「未写出即失败 → 502」）。
	wrote bool

	// opened 标记 created/in_progress 是否已发出；
	// closed 标记收尾是否已完成；failed 标记是否已输出错误事件。
	opened bool
	closed bool
	failed bool

	// items 是按 output_index 排列的已开启 item（按首次出现顺序追加）。
	items []*responsesStreamItem

	// textItem 是 message item（首次出现文本增量时创建，之后复用）。
	textItem *responsesStreamItem
	// callItems 把 chat tool_calls[].index 映射到对应的 function_call item。
	// index 缺失时使用 responsesMissingToolIndex 合成键，避免多个工具串号。
	callItems map[int]*responsesStreamItem
	// openCalls 保存尚未收尾（未发 output_item.done）的 function_call item，按开启顺序。
	openCalls []*responsesStreamItem

	// 收尾用的结束原因与用量。
	finishReason string
	usage        map[string]any
}

// responsesStreamItem 是一个流式 output item 的累积状态。
type responsesStreamItem struct {
	index int
	// kind 为 "message" 或 "function_call"。
	kind string
	id   string

	// message 专有：累积的文本与 content_part 是否已开启。
	text     strings.Builder
	partOpen bool

	// function_call 专有：call_id / name / 累积参数。
	callID    string
	name      string
	arguments strings.Builder
	// closed 标记 output_item.done 是否已发出。
	closed bool
}

// responsesMissingToolIndex 是「chat tool_calls[] 未带 index」时使用的合成键（真实 index 恒 >= 0）。
const responsesMissingToolIndex = -1

// newResponsesStreamTranslator 创建流式翻译器；model 为客户端请求的模型名，respID 为网关生成的响应 ID。
func newResponsesStreamTranslator(
	w http.ResponseWriter,
	flusher http.Flusher,
	model, respID string,
) *responsesStreamTranslator {
	return &responsesStreamTranslator{
		w:         w,
		flusher:   flusher,
		model:     model,
		respID:    respID,
		callItems: map[int]*responsesStreamItem{},
	}
}

// Write 接收上游（chat 形状）SSE 字节；内部按事件边界解析并翻译输出。始终返回 (len(p), nil)。
func (t *responsesStreamTranslator) Write(p []byte) (int, error) {
	// 已收尾/已失败：忽略后续输入（避免在终态之后追加事件）。
	if t.closed || t.failed {
		return len(p), nil
	}
	t.buf = append(t.buf, p...)
	for !t.closed && !t.failed {
		end, delimLen := anthropicStreamEventBoundary(t.buf)
		if end < 0 {
			break
		}
		block := string(t.buf[:end])
		t.buf = t.buf[end+delimLen:]
		t.processEvent(block)
	}
	if t.closed || t.failed {
		t.buf = nil
	}
	return len(p), nil
}

// Wrote 返回是否已向客户端写出过任何字节（调用方用于「未写出即失败 → 502」判断）。
func (t *responsesStreamTranslator) Wrote() bool { return t.wrote }

// Finish 幂等收尾：若已开始且未结束，补发剩余事件直到 response.completed。
func (t *responsesStreamTranslator) Finish() {
	if !t.closed && !t.failed {
		t.finalize()
	}
}

// processEvent 处理一个完整 SSE 事件块（多行 data: 会被拼接）。
func (t *responsesStreamTranslator) processEvent(block string) {
	data := anthropicSSEDataOf(block)
	if data == "" {
		return
	}
	if data == "[DONE]" {
		t.finalize()
		return
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		// 上游分片无法解析：容忍并丢弃（不中断整条流）。
		return
	}
	// 流中错误：以 event: error 输出并停止（不再追加 [DONE]，也不再补完成事件）。
	if errValue, exists := chunk["error"]; exists && errValue != nil {
		t.emitError(errValue)
		return
	}
	t.handleChunk(chunk)
}

// handleChunk 处理单个 chat 流式分片：提取文本增量、工具调用增量、用量与 finish_reason。
func (t *responsesStreamTranslator) handleChunk(chunk map[string]any) {
	// 用量可能出现在任意分片（stream_options.include_usage 的末片）。
	if usage, ok := chunk["usage"].(map[string]any); ok {
		t.usage = usage
	}

	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		return
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return
	}

	// 只有真正出现内容增量时才开 sequence（避免空 delta 触发提前发头）。
	if delta, ok := choice["delta"].(map[string]any); ok {
		if text, ok := delta["content"].(string); ok && text != "" {
			t.handleTextDelta(text)
		}
		if calls, ok := delta["tool_calls"].([]any); ok {
			for _, raw := range calls {
				call, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				t.handleToolCallDelta(call)
			}
		}
	}

	if t.closed || t.failed {
		return
	}
	// finish_reason 只记录不收尾：include_usage 会在其后追加仅含 usage 的分片，
	// 提前收尾会丢弃它，导致 response.completed 的 usage 恒为 0。
	if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
		t.finishReason = reason
	}
}

// handleTextDelta 把一条文本增量并入 message item（按需开启 item 与 content_part）。
func (t *responsesStreamTranslator) handleTextDelta(text string) {
	t.ensureOpened()
	item := t.ensureTextItem()
	if !item.partOpen {
		item.partOpen = true
		t.emit("response.content_part.added", map[string]any{
			"item_id":       item.id,
			"output_index":  item.index,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	item.text.WriteString(text)
	t.emit("response.output_text.delta", map[string]any{
		"item_id":       item.id,
		"output_index":  item.index,
		"content_index": 0,
		"delta":         text,
	})
}

// handleToolCallDelta 处理一条 chat tool_calls[] 增量：首片开启 function_call item，
// 同一工具后续分片按 index 归并到同一 item 上（不同工具互不串号）。
func (t *responsesStreamTranslator) handleToolCallDelta(call map[string]any) {
	t.ensureOpened()
	index := responsesMissingToolIndex
	if n, ok := anthJSONInt(call["index"]); ok {
		index = n
	}
	id, _ := call["id"].(string)
	name, args := "", ""
	if fn, ok := call["function"].(map[string]any); ok {
		name, _ = fn["name"].(string)
		args, _ = fn["arguments"].(string)
	}

	item, exists := t.callItems[index]
	if !exists {
		if id == "" {
			id = t.respID + "_call_" + strconv.Itoa(len(t.items))
		}
		item = t.newItem("function_call", id)
		item.callID = id
		item.name = name
		t.callItems[index] = item
		t.openCalls = append(t.openCalls, item)
		t.emit("response.output_item.added", map[string]any{
			"output_index": item.index,
			"item": map[string]any{
				"id":        item.id,
				"type":      "function_call",
				"call_id":   item.callID,
				"name":      item.name,
				"arguments": "",
				"status":    "in_progress",
			},
		})
	} else if item.name == "" && name != "" {
		// 迟到的 name（首片只带 id 的少见形态）：补记，供终态 item 使用。
		item.name = name
	}
	if item.closed || args == "" {
		return
	}
	item.arguments.WriteString(args)
	t.emit("response.function_call_arguments.delta", map[string]any{
		"item_id":      item.id,
		"output_index": item.index,
		"delta":        args,
	})
}

// newItem 新建并登记一个 item（output_index 按登记顺序分配）。
func (t *responsesStreamTranslator) newItem(kind, id string) *responsesStreamItem {
	item := &responsesStreamItem{index: len(t.items), kind: kind, id: id}
	t.items = append(t.items, item)
	return item
}

// ensureOpened 首次有增量输出时发出 created / in_progress。
func (t *responsesStreamTranslator) ensureOpened() {
	if t.opened {
		return
	}
	t.opened = true
	t.emit("response.created", map[string]any{"response": t.responseBody(respStatusInProgress, []any{})})
	t.emit("response.in_progress", map[string]any{"response": t.responseBody(respStatusInProgress, []any{})})
}

// ensureTextItem 返回 message item，不存在时创建并发 output_item.added。
func (t *responsesStreamTranslator) ensureTextItem() *responsesStreamItem {
	if t.textItem != nil {
		return t.textItem
	}
	item := t.newItem("message", t.respID+"_msg")
	t.textItem = item
	t.emit("response.output_item.added", map[string]any{
		"output_index": item.index,
		"item":         map[string]any{"id": item.id, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}},
	})
	return item
}

// finalize 收尾：按开启顺序补齐每个未收尾 item 的 done 级事件，再以 response.completed 结束。幂等。
// 上游 200 但从未产生增量输出（空流）时也补发最小合法事件序列，
// 避免客户端拿到「200 + 空 body」的非法 SSE 流。
func (t *responsesStreamTranslator) finalize() {
	if t.closed || t.failed {
		return
	}
	t.closed = true
	t.ensureOpened()
	for _, item := range t.openCalls {
		t.closeCallItem(item)
	}
	if item := t.textItem; item != nil && !item.closed {
		full := item.text.String()
		t.emit("response.output_text.done", map[string]any{
			"item_id":       item.id,
			"output_index":  item.index,
			"content_index": 0,
			"text":          full,
		})
		t.emit("response.content_part.done", map[string]any{
			"item_id":       item.id,
			"output_index":  item.index,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": full, "annotations": []any{}},
		})
		t.emit("response.output_item.done", map[string]any{
			"output_index": item.index,
			"item":         responsesMessageItemFull(item.id, full, t.finalStatus()),
		})
		item.closed = true
	}
	t.emit("response.completed", map[string]any{"response": t.responseBody(t.finalStatus(), t.finalOutput())})
}

// closeCallItem 收尾一个 function_call item：parameters.done → output_item.done。幂等。
func (t *responsesStreamTranslator) closeCallItem(item *responsesStreamItem) {
	if item.closed {
		return
	}
	item.closed = true
	args := item.arguments.String()
	t.emit("response.function_call_arguments.done", map[string]any{
		"item_id":      item.id,
		"output_index": item.index,
		"name":         item.name,
		"arguments":    args,
	})
	t.emit("response.output_item.done", map[string]any{
		"output_index": item.index,
		"item":         responsesFunctionCallItemFull(item.id, item.callID, item.name, args),
	})
}

// finalStatus 返回终态：finish_reason="length" → incomplete，其余（含 tool_calls）→ completed。
func (t *responsesStreamTranslator) finalStatus() string {
	if t.finishReason == "length" {
		return respStatusIncomplete
	}
	return respStatusCompleted
}

// finalOutput 按 output_index 顺序返回终态 output 数组（无 item 时为空数组）。
func (t *responsesStreamTranslator) finalOutput() []any {
	output := make([]any, 0, len(t.items))
	for _, item := range t.items {
		switch item.kind {
		case "message":
			output = append(output, responsesMessageItemFull(item.id, item.text.String(), t.finalStatus()))
		case "function_call":
			output = append(output, responsesFunctionCallItemFull(item.id, item.callID, item.name, item.arguments.String()))
		}
	}
	return output
}

// responsesMessageItemFull 构造终态 message item（带 status）。
func responsesMessageItemFull(id, text, status string) map[string]any {
	return map[string]any{
		"id":      id,
		"type":    "message",
		"role":    "assistant",
		"status":  status,
		"content": responsesMessageItemContent(text),
	}
}

// responsesFunctionCallItemFull 构造终态 function_call item。
func responsesFunctionCallItemFull(id, callID, name, args string) map[string]any {
	return map[string]any{
		"id":        id,
		"type":      "function_call",
		"call_id":   callID,
		"name":      name,
		"arguments": args,
		"status":    respStatusCompleted,
	}
}

// responseBody 构造事件载荷里的 response 对象。
func (t *responsesStreamTranslator) responseBody(status string, output []any) map[string]any {
	body := map[string]any{
		"id":     t.respID,
		"object": respObjectResponse,
		"model":  t.model,
		"status": status,
		"output": output,
	}
	if status == respStatusIncomplete {
		body["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	// usage 只在终态可用（上游末片才有），进行中事件不携带。
	if t.usage != nil {
		body["usage"] = responsesUsageFromChat(t.usage)
	}
	return body
}

// emitError 输出 event: error 并停止后续处理（不追加 [DONE]，不再补完成事件）。
func (t *responsesStreamTranslator) emitError(errValue any) {
	t.failed = true
	t.buf = nil
	t.wrote = true
	t.ensureSSEHeaders()
	data, err := json.Marshal(map[string]any{"type": "error", "error": errValue})
	if err != nil {
		return
	}
	fmt.Fprintf(t.w, "event: error\ndata: %s\n\n", data)
	if t.flusher != nil {
		t.flusher.Flush()
	}
}

// ensureSSEHeaders 在首次写出前设置 SSE 响应头（若响应头尚未提交）。
func (t *responsesStreamTranslator) ensureSSEHeaders() {
	if h := t.w.Header(); h.Get("Content-Type") == "" {
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
	}
}

// emit 写出一个 Responses SSE 事件并立即 flush。
func (t *responsesStreamTranslator) emit(eventType string, payload map[string]any) {
	payload["type"] = eventType
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	t.ensureSSEHeaders()
	fmt.Fprintf(t.w, "event: %s\ndata: %s\n\n", eventType, data)
	t.wrote = true
	if t.flusher != nil {
		t.flusher.Flush()
	}
}

// responsesMessageItemContent 构造 message item 的 content 数组。
func responsesMessageItemContent(text string) []any {
	return []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}
}

// responsesJSONString 尽力把任意值序列化为 JSON 字符串，失败返回空串。
func responsesJSONString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
