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
	"log/slog"
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

// 请求方向转换器的哨兵错误：入站处理器用 errors.Is 判定，再映射为对外 400 文案。
// 文案本身就是对外 API 契约的一部分（T5 测试按字面断言），因此哨兵化不改变对外行为。
var (
	// errResponsesInvalidBody 表示请求体不是合法 JSON 对象。
	errResponsesInvalidBody = errors.New("invalid request body")
	// errResponsesMissingModel 表示请求体缺少 model。
	errResponsesMissingModel = errors.New("missing model")
	// errResponsesInvalidInput 表示 input 无法产出任何 chat message。
	errResponsesInvalidInput = errors.New("invalid input")
	// errResponsesInvalidTools 表示 tools 不是数组。
	errResponsesInvalidTools = errors.New("invalid tools")
	// errResponsesInvalidUpstream 表示上游 chat 响应无法解析。
	errResponsesInvalidUpstream = errors.New("invalid upstream response")
)

// --- 请求方向：responses → chat ---

// responsesRequestToChat 把 Responses 请求体转换为 OpenAI chat completions 请求体。
//
// 校验（均返回英文错误）：
//   - model 必填；
//   - 必须能产出至少一条 messages：input 缺失/为 null/为空数组、或数组里全是被丢弃的
//     Responses 专有 item 时，返回 "invalid input"。绝不发出「只有 model、没有 messages」
//     的上游请求——那属于 §2 反目标「转换丢字段却不报错」。
//     注意空字符串 input 是合法的：它产生一条空的 user 消息。
func responsesRequestToChat(body []byte) (map[string]any, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, errResponsesInvalidBody
	}
	if req == nil {
		return nil, errResponsesInvalidBody
	}
	model, _ := req["model"].(string)
	if strings.TrimSpace(model) == "" {
		return nil, errResponsesMissingModel
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
	// input 无法产出任何 message 时直接拒绝，绝不向上游发一条没有 messages 的请求。
	if len(messages) == 0 {
		return nil, errResponsesInvalidInput
	}
	out["messages"] = messages

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
	// tools：显式传入的数组（含空数组）原样保留——部分上游见到 tool_choice 却没有 tools 会 400。
	// 存在但非数组（对象/字符串/数字等）是客户端错误：静默丢弃同样会造出
	// 「tool_choice 有值、tools 缺失」的组合，因此与 invalid input 一样直接拒绝。
	if rawTools, exists := req["tools"]; exists && rawTools != nil {
		if _, ok := rawTools.([]any); !ok {
			return nil, errResponsesInvalidTools
		}
		out["tools"] = responsesToolsToChat(rawTools)
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
			role, _ := item["role"].(string)
			if role == "" {
				continue
			}
			message := map[string]any{"role": role, "content": responsesContentToChat(item["content"])}
			// Responses 允许把工具调用挂在 message item 上（content 为 null）；不搬过去就等于丢字段。
			if calls, ok := responsesToolCallsOf(item); ok {
				message["tool_calls"] = calls
			}
			if message["content"] == nil && message["tool_calls"] == nil {
				// 既无内容也无工具调用：按丢弃处理，避免产生空消息。
				continue
			}
			out = append(out, message)
		}
	}
	return out
}

// responsesToolCallsOf 把 Responses message item 上的 tool_calls 转成 chat 形态。
// Responses 的 item 可能把工具调用挂在 message 上（此时 content 为 null）；
// 这些条目在 chat 协议里必须落在 assistant 的 tool_calls 里，否则转换即丢字段。
func responsesToolCallsOf(item map[string]any) ([]any, bool) {
	rawCalls, ok := item["tool_calls"].([]any)
	if !ok || len(rawCalls) == 0 {
		return nil, false
	}
	calls := make([]any, 0, len(rawCalls))
	for _, raw := range rawCalls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		converted := map[string]any{"type": "function"}
		if fn, ok := call["function"].(map[string]any); ok {
			// 已是 chat 嵌套形态：原样保留。
			converted["id"] = call["id"]
			converted["function"] = map[string]any{
				"name":      fn["name"],
				"arguments": responsesArguments(fn["arguments"]),
			}
			calls = append(calls, converted)
			continue
		}
		// 扁平 Responses 形态：{id?, call_id?, name, arguments}。
		id, _ := call["id"].(string)
		if id == "" {
			id, _ = call["call_id"].(string)
		}
		converted["id"] = id
		converted["function"] = map[string]any{
			"name":      call["name"],
			"arguments": responsesArguments(call["arguments"]),
		}
		calls = append(calls, converted)
	}
	if len(calls) == 0 {
		return nil, false
	}
	return calls, true
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
// 调用方保证入参是数组（非数组已在 responsesRequestToChat 里拒绝为 "invalid tools"）。
func responsesToolsToChat(v any) []any {
	items, _ := v.([]any)
	out := make([]any, 0, len(items))
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
		return nil, nil, errResponsesInvalidUpstream
	}
	choices, ok := chat["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil, nil, errResponsesInvalidUpstream
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return nil, nil, errResponsesInvalidUpstream
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
//
// 收尾只由两种输入触发：上游的 "[DONE]" 事件，或调用方 Finish()。
// 带 finish_reason 的末块**只记录结束原因、不触发收尾**——因为 include_usage 模式会在它
// 之后追加一个仅含 usage 的分片，提前收尾会把终态 usage 丢掉。
//
// 上游约定（I-4）：收到 "[DONE]" 即视为流已结束，之后到达的字节一律忽略；
// 终态 usage 以 "[DONE]" 之前最后一个完整事件为准。
type responsesStreamTranslator struct {
	w       http.ResponseWriter
	flusher http.Flusher
	model   string
	respID  string

	// buf 缓存尚未构成完整事件（未遇到空行边界）的字节。
	buf []byte

	// wrote 标记是否已向客户端写出过任何字节（供调用方判断「未写出即失败 → 502」）。
	//
	// 语义边界（I-3 / I-5）：
	//   - 只由真实「上游增量驱动的输出」置位：文本/工具调用增量事件，以及流中的 error 事件。
	//     流中错误说明已经开流，无法回退成 JSON 502，因此算已写出。
	//   - 收尾事件由 Finish() 补发时**不**置位。
	//   - 【重要】Wrote()==false 只表示「没有由增量驱动过输出」，**不**表示「没向 w 写过字节」：
	//     零增量时 Finish() 仍会把 created/in_progress/completed 真实写进 w（见 Finish 注释），
	//     上游只发 "[DONE]" 时同样如此。因此**不要**用「Wrote() 为假」单独决定回退 502——
	//     见 Wrote 方法注释里的回退判定建议。
	wrote bool

	// opened 标记 created/in_progress 是否已发出；
	// finalizing 标记已进入收尾阶段（此后写出的事件不置 wrote）；
	// closed 标记收尾是否已完成；failed 标记是否已输出错误事件。
	opened     bool
	finalizing bool
	closed     bool
	failed     bool

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

// Wrote 报告是否已有「增量驱动」的输出写到客户端，用于区分「上游 200 但什么也没产出」。
//
// 回退 502 的正确判定（Task 6 契约）：**以「上游状态码异常」为主要依据**，而不是「Wrote() 为假」。
// 原因：Finish() 在零增量场景下也会把 created/in_progress/completed 真实写进 w，
// 若此时只凭 Wrote()==false 回退成 502，就会写出「既有 SSE 事件体、又是 502」的自相矛盾响应。
// 推荐顺序：上游状态码异常 → 直接 502（此时代未调用过翻译器）；
// 上游 2xx → 一律按 SSE 收尾，最多依据 Wrote() 记录「空响应」的日志/用量，不改状态码。
//
// 另注意：本方法必须在调用 Finish() **之前**读取（Finish 之后它的值不再变化，但此前写入的
// 收尾事件已经落盘，见 Finish 注释）。
func (t *responsesStreamTranslator) Wrote() bool { return t.wrote }

// Finish 幂等收尾：补发剩余事件直到 response.completed。
//
// 【调用顺序契约 —— 必须遵守】Finish() 会把收尾事件**真实写进 w**，即使在零增量场景下
// （上游 200 无任何 chunk、或只发了 "[DONE]"）。因此调用方**必须先判断 Wrote() 再调用
// Finish()**；先 Finish 再判断会让已写出的 body 落盘、502 回退失效。
// 正确先例见 anthropic_ingress.go 的 `if !tw.Wrote() { 502 }` 之后再 `tw.Finish()`。
//
// Finish() **不改变** Wrote() 的语义（收尾阶段写出的事件不计入 wrote）：上游 200 但零增量时
// Wrote() 保持 false，供调用方识别「空响应」；但如 Wrote 注释所述，回退 502 的依据应当是
// 上游状态码异常，而不是 Wrote()==false。
func (t *responsesStreamTranslator) Finish() { t.finalize() }

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
//
// 两个「写了字节但 Wrote()==false」的场景（Task 6 必须知道）：
//   - 上游 2xx 且零 chunk，随后调用方 Finish()；
//   - 上游只发 "[DONE]"（同样零增量），由 [DONE] 触发 finalize。
//
// 两者都会真实写出 created/in_progress/completed。因此 Wrote()==false **不代表** w 未被写入。
func (t *responsesStreamTranslator) finalize() {
	if t.closed || t.failed {
		return
	}
	t.finalizing = true
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
	output := t.finalOutput()
	// 防御：上游声明 tool_calls 结束却一个 function_call item 都没给（漏发 tool_calls 增量），
	// 客户端会看到「完成但没有任何工具调用」。记 Warn 便于排查，不改写事件序列。
	if t.finishReason == "tool_calls" && !t.hasFunctionCallItem() {
		slog.Warn("responses stream: finish_reason=tool_calls but no function_call item was synthesized",
			"response_id", t.respID, "model", t.model)
	}
	t.emit("response.completed", map[string]any{"response": t.responseBody(t.finalStatus(), output)})
}

// hasFunctionCallItem 报告是否有任何已合成的 function_call item。
func (t *responsesStreamTranslator) hasFunctionCallItem() bool {
	for _, item := range t.items {
		if item.kind == "function_call" {
			return true
		}
	}
	return false
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
//
// 先 ensureOpened() 补发 response.created / response.in_progress：OpenAI SDK 依赖
// response.created 建立 response 对象，首个增量之前就失败时也必须先给（I-3）。
// 流中错误算「已写出」（见 wrote 字段注释），因此这里显式置位。
func (t *responsesStreamTranslator) emitError(errValue any) {
	t.ensureOpened()
	t.failed = true
	t.buf = nil
	t.writeEvent("error", map[string]any{"type": "error", "error": errValue}, true)
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
// 收尾阶段补发的事件不计入 Wrote()，见 wrote 字段注释。
func (t *responsesStreamTranslator) emit(eventType string, payload map[string]any) {
	payload["type"] = eventType
	t.writeEvent(eventType, payload, !t.finalizing)
}

// writeEvent 是唯一的事件写出路径：设响应头 → 写 event/data 行 → flush → 按需置 wrote。
func (t *responsesStreamTranslator) writeEvent(eventType string, payload map[string]any, markWrote bool) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	t.ensureSSEHeaders()
	fmt.Fprintf(t.w, "event: %s\ndata: %s\n\n", eventType, data)
	if markWrote {
		t.wrote = true
	}
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
