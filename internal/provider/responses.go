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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 本文件实现 OpenAI Responses API（/responses）上游适配器。
// Provider 接口的契约是「恒吐 OpenAI-chat 形状」，故方向是
// chat 请求 → Responses 请求（design §5.3.1）、Responses 响应/流 → chat（design §5.3.3）。
// 不要与 proxy/responses_convert.go 的入口侧转换（§5.3.1/§5.3.2）混用。

const (
	// responsesUpstreamTimeout 是非流式上游超时。
	responsesUpstreamTimeout = 5 * time.Minute
	// responsesStreamTimeout 是流式上游超时（与 anthropic 侧对齐）。
	responsesStreamTimeout = 30 * time.Minute

	// responsesPath 是 Responses 上游路径（apiBase 已含 /v1）。
	responsesPath = "/responses"

	// responsesSSE 单行上限：工具参数等大块 delta 会超过 bufio 默认 64 KiB。
	responsesSSEMaxLineBytes = 1024 * 1024
)

// OpenAIResponsesProvider 适配 OpenAI Responses API（/responses）。
type OpenAIResponsesProvider struct{}

var _ Provider = (*OpenAIResponsesProvider)(nil)

func (p *OpenAIResponsesProvider) baseURL(apiBase string) string {
	return strings.TrimRight(apiBase, "/")
}

// ChatCompletion 发送非流式请求，并把上游 Responses 响应转换为 chat 形状。
func (p *OpenAIResponsesProvider) ChatCompletion(
	ctx context.Context,
	apiKey, apiBase, model string,
	reqBody []byte,
	customHeaders map[string]string,
) (result *CompletionResult, statusCode int, err error) {
	body, err := buildResponsesRequest(model, reqBody, false)
	if err != nil {
		return nil, 0, err
	}
	httpReq, err := newUpstreamRequest(ctx, p.baseURL(apiBase)+responsesPath, body, func(h http.Header) {
		h.Set("Authorization", "Bearer "+apiKey)
	}, customHeaders)
	if err != nil {
		return nil, 0, err
	}
	resp, err := doUpstream(httpReq, responsesUpstreamTimeout)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := ReadUpstreamBody(resp.Body)
	if err != nil {
		return nil, 0, richErrorf(err, "failed to read upstream response")
	}

	// 非 200 原样返回 status + body，与 OpenAIProvider 一致（不解析）。
	if resp.StatusCode != http.StatusOK {
		return &CompletionResult{Body: respBody}, resp.StatusCode, nil
	}

	chatBody, err := responsesToChatResponse(respBody)
	if err != nil {
		return &CompletionResult{Body: respBody}, resp.StatusCode, nil
	}
	return &CompletionResult{Body: chatBody, Usage: extractResponsesUsage(string(respBody), nil)}, http.StatusOK, nil
}

// ChatCompletionStream 发送流式请求，把 Responses SSE 译为 chat chunk 并补 data: [DONE]。
func (p *OpenAIResponsesProvider) ChatCompletionStream(
	ctx context.Context,
	apiKey, apiBase, model string,
	reqBody []byte,
	w http.ResponseWriter,
	flusher http.Flusher,
	customHeaders map[string]string,
) (result *StreamResult, statusCode int, err error) {
	body, err := buildResponsesRequest(model, reqBody, true)
	if err != nil {
		return nil, 0, err
	}
	httpReq, err := newUpstreamRequest(ctx, p.baseURL(apiBase)+responsesPath, body, func(h http.Header) {
		h.Set("Authorization", "Bearer "+apiKey)
	}, customHeaders)
	if err != nil {
		return nil, 0, err
	}
	resp, err := doUpstream(httpReq, responsesStreamTimeout)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		forwardUpstreamError(w, resp)
		return nil, resp.StatusCode, nil
	}

	setSSEHeaders(w)
	// 这条路径的客户端是 chat 协议，故结尾必须补 data: [DONE]
	// （与 design §4.3「Responses 入口不发 [DONE]」不矛盾，两者客户端不同）。
	usage, err := responsesStreamToChat(w, flusher, resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return &StreamResult{Usage: usage}, http.StatusOK, nil
}

// CheckConnectivityWithChat 以最小 Responses 请求探测上游连通性与模型可用性。
func (p *OpenAIResponsesProvider) CheckConnectivityWithChat(
	ctx context.Context, apiKey, apiBase, model string,
) (time.Duration, ProbeOutcome, error) {
	body, err := json.Marshal(map[string]interface{}{
		"model":             model,
		"input":             probePrompt,
		"max_output_tokens": probeMaxTokens,
		"store":             false,
	})
	if err != nil {
		return 0, ProbeOutcome{}, &ConnectivityError{Kind: ErrNetworkUnreachable, Cause: err}
	}
	httpReq, err := newProbeRequest(ctx, p.baseURL(apiBase)+responsesPath, body, func(h http.Header) {
		if apiKey != "" {
			h.Set("Authorization", "Bearer "+apiKey)
		}
	})
	if err != nil {
		return 0, ProbeOutcome{}, err
	}
	return runConnectivityProbe(httpReq, responsesProbeReply)
}

// responsesProbeReply 从 Responses 回包中提取探测结果。
// 与 openAIProbeReply 的区别：正文在 output[].content[] 里、type 为 output_text，
// 且推理模型可能只产出 reasoning 而正文为空。
func responsesProbeReply(body []byte) (ProbeOutcome, error) {
	var parsed struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ProbeOutcome{}, errors.New("response is not a responses payload")
	}
	var parts []string
	for _, item := range parsed.Output {
		if item.Type != "message" {
			continue
		}
		for _, c := range item.Content {
			if strings.TrimSpace(c.Text) != "" {
				parts = append(parts, c.Text)
			}
		}
	}
	reply := strings.TrimSpace(strings.Join(parts, " "))
	if reply == "" {
		return ProbeOutcome{}, errors.New("response has no output text")
	}
	return ProbeOutcome{
		Reply:         truncateReply(reply),
		UpstreamID:    parsed.ID,
		UpstreamModel: parsed.Model,
	}, nil
}

// buildResponsesRequest 把 chat 请求体转换为 Responses 请求体（design §5.3.1）。
//
// 映射：messages → input（system/developer 提升为 instructions，其余转 item）、
// max_tokens/max_completion_tokens → max_output_tokens、response_format → text.format、
// 嵌套 tool → 扁平 tool。Responses 不需要 stream_options；store 恒为 false。
func buildResponsesRequest(model string, reqBody []byte, stream bool) ([]byte, error) {
	var chat map[string]interface{}
	if err := json.Unmarshal(reqBody, &chat); err != nil {
		return nil, richErrorf(err, "failed to decode request body")
	}

	req := make(map[string]interface{}, len(chat)+4)
	for k, v := range chat {
		req[k] = v
	}
	req["model"] = model

	// messages → instructions + input items。
	instructions, items := responsesInputFromMessages(chat["messages"])
	delete(req, "messages")
	if len(instructions) > 0 {
		req["instructions"] = strings.Join(instructions, "\n\n")
	}
	if items == nil {
		items = []interface{}{}
	}
	req["input"] = items

	// max_tokens / max_completion_tokens → max_output_tokens。
	if v, ok := chat["max_tokens"]; ok {
		req["max_output_tokens"] = v
	} else if v, ok := chat["max_completion_tokens"]; ok {
		req["max_output_tokens"] = v
	}
	delete(req, "max_tokens")
	delete(req, "max_completion_tokens")

	// response_format → text.format（合并到已有 text 对象，避免覆盖 verbosity 等字段）。
	if v, ok := chat["response_format"]; ok {
		text, _ := req["text"].(map[string]interface{})
		if text == nil {
			text = map[string]interface{}{}
		}
		text["format"] = v
		req["text"] = text
		delete(req, "response_format")
	}

	// 嵌套 tool → 扁平 tool。
	if v, ok := chat["tools"]; ok {
		req["tools"] = flattenResponsesTools(v)
	}

	// Responses 不需要 stream_options；流式只下发 stream。
	delete(req, "stream_options")
	if stream {
		req["stream"] = true
	}

	// 安全红线：恒关闭上游存储，忽略入站 store。
	req["store"] = false

	body, err := json.Marshal(req)
	if err != nil {
		return nil, richErrorf(err, "failed to encode request body")
	}
	return body, nil
}

// responsesInputFromMessages 把 chat messages 拆成 instructions（system/developer）与 input items。
// 非 system 消息按 design §5.3.1 的逆映射转为 Responses input item。
func responsesInputFromMessages(raw interface{}) (instructions []string, items []interface{}) {
	messages, _ := raw.([]interface{})
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "system", "developer":
			if text := contentToString(msg["content"]); text != "" {
				instructions = append(instructions, text)
			}
		case "tool":
			callID, _ := msg["tool_call_id"].(string)
			items = append(items, map[string]interface{}{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  contentToString(msg["content"]),
			})
		case "assistant":
			items = append(items, responsesAssistantItems(msg)...)
		default:
			items = append(items, map[string]interface{}{
				"role":    role,
				"content": responsesContentParts(msg["content"]),
			})
		}
	}
	return instructions, items
}

// responsesAssistantItems 把 assistant 消息转为 message item + 若干 function_call item。
func responsesAssistantItems(msg map[string]interface{}) []interface{} {
	var items []interface{}
	if parts := responsesContentParts(msg["content"]); len(parts) > 0 {
		items = append(items, map[string]interface{}{
			"role":    "assistant",
			"content": parts,
		})
	}
	toolCalls, _ := msg["tool_calls"].([]interface{})
	for _, tc := range toolCalls {
		call, ok := tc.(map[string]interface{})
		if !ok {
			continue
		}
		fn, _ := call["function"].(map[string]interface{})
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		id, _ := call["id"].(string)
		items = append(items, map[string]interface{}{
			"type":      "function_call",
			"call_id":   id,
			"name":      name,
			"arguments": args,
		})
	}
	return items
}

// responsesContentParts 把 chat content（字符串或块数组）转为 Responses 内容块数组。
func responsesContentParts(raw interface{}) []interface{} {
	parts := []interface{}{}
	switch v := raw.(type) {
	case string:
		if v != "" {
			parts = append(parts, map[string]interface{}{"type": "input_text", "text": v})
		}
	case []interface{}:
		for _, item := range v {
			block, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			switch block["type"] {
			case "text", "input_text":
				parts = append(parts, map[string]interface{}{"type": "input_text", "text": block["text"]})
			case "image_url", "input_image":
				url := ""
				switch iv := block["image_url"].(type) {
				case string:
					url = iv
				case map[string]interface{}:
					url, _ = iv["url"].(string)
				}
				if url == "" {
					url, _ = block["image_url"].(string)
				}
				if url != "" {
					parts = append(parts, map[string]interface{}{"type": "input_image", "image_url": url})
				}
			}
		}
	}
	return parts
}

// flattenResponsesTools 把 chat 的嵌套 tool（{type,function:{...}}）转为 Responses 扁平 tool。
func flattenResponsesTools(raw interface{}) []interface{} {
	tools, _ := raw.([]interface{})
	out := make([]interface{}, 0, len(tools))
	for _, t := range tools {
		tool, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]interface{})
		if !ok {
			// 已是扁平形状（或内置工具），原样保留。
			out = append(out, tool)
			continue
		}
		flat := map[string]interface{}{"type": "function"}
		for _, k := range []string{"name", "description", "parameters", "strict"} {
			if v, ok := fn[k]; ok {
				flat[k] = v
			}
		}
		out = append(out, flat)
	}
	return out
}

// responsesToChatResponse 把 Responses 响应体转换为 chat 响应体（design §5.3.3）。
// 输出必须是纯 chat JSON：夹带 responses 专有字段会破坏 chat 入口契约。
func responsesToChatResponse(body []byte) ([]byte, error) {
	var resp struct {
		ID                string `json:"id"`
		Status            string `json:"status"`
		Model             string `json:"model"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
		Usage *json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, richErrorf(err, "failed to decode upstream response")
	}

	var textParts []string
	var toolCalls []map[string]interface{}
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" && c.Text != "" {
					textParts = append(textParts, c.Text)
				}
			}
		case "function_call":
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   item.CallID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      item.Name,
					"arguments": item.Arguments,
				},
			})
		}
	}

	message := map[string]interface{}{
		"role":    "assistant",
		"content": strings.Join(textParts, ""),
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	chat := map[string]interface{}{
		"id":      resp.ID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   resp.Model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       message,
				"finish_reason": responsesFinishReason(resp.Status, resp.IncompleteDetails.Reason, len(toolCalls) > 0),
			},
		},
	}
	if resp.Usage != nil {
		chat["usage"] = chatUsageFromResponses(string(body))
	}
	return json.Marshal(chat)
}

// responsesFinishReason 按 status / incomplete_details.reason 决定 chat 的 finish_reason。
func responsesFinishReason(status, incompleteReason string, hasToolCall bool) string {
	if status == "incomplete" {
		switch incompleteReason {
		case "max_output_tokens":
			return "length"
		case "content_filter":
			return "content_filter"
		default:
			return "length"
		}
	}
	if hasToolCall {
		return "tool_calls"
	}
	return "stop"
}

// responsesUsage 是 Responses 用量对象。
// 只取 input_tokens / output_tokens / total_tokens：
// output_tokens 已含 reasoning_tokens（子集），再读会重复累加。
type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
}

// responsesUsageEnvelope 用于从响应体 / SSE data 中取出 usage。
type responsesUsageEnvelope struct {
	Usage    *responsesUsage `json:"usage"`
	Response *struct {
		Usage *responsesUsage `json:"usage"`
	} `json:"response"`
}

// toUsage 把上游 Responses 用量映射为内部 Usage。
func (u *responsesUsage) toUsage() *Usage {
	return &Usage{
		PromptTokens:           u.InputTokens,
		CompletionTokens:       u.OutputTokens,
		TotalTokens:            u.TotalTokens,
		PromptCacheReadTokens:  u.InputTokensDetails.CachedTokens,
		PromptCacheWriteTokens: u.InputTokensDetails.CacheWriteTokens,
	}
}

// extractResponsesUsage 从 Responses 响应体或 SSE data 中提取用量。
// 不能复用 extractOpenAIUsage：它读 prompt_tokens/completion_tokens，会把 Responses 静默记成 0。
// 无 usage 字段时返回 prev（流式中间事件依赖此语义保留已有用量）。
func extractResponsesUsage(data string, prev *Usage) *Usage {
	var env responsesUsageEnvelope
	if json.Unmarshal([]byte(data), &env) != nil {
		return prev
	}
	if env.Usage != nil {
		return env.Usage.toUsage()
	}
	if env.Response != nil && env.Response.Usage != nil {
		return env.Response.Usage.toUsage()
	}
	return prev
}

// chatUsageFromResponses 构造 chat 形状的 usage 对象（Responses → chat，§5.3.3）。
func chatUsageFromResponses(data string) map[string]interface{} {
	u := extractResponsesUsage(data, nil)
	if u == nil {
		return map[string]interface{}{}
	}
	return map[string]interface{}{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
		"prompt_tokens_details": map[string]interface{}{
			"cached_tokens":      u.PromptCacheReadTokens,
			"cache_write_tokens": u.PromptCacheWriteTokens,
		},
	}
}

// responsesStreamToChat 逐行读取 Responses SSE，译为 chat chunk 写往 w，
// 结束时补 data: [DONE]（这条路径的客户端是 chat 协议）。返回累计用量。
func responsesStreamToChat(w http.ResponseWriter, flusher http.Flusher, r io.Reader) (*Usage, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), responsesSSEMaxLineBytes)

	state := &responsesStreamState{w: w, flusher: flusher, toolIndex: map[int]int{}}
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		state.handleData(strings.TrimPrefix(line, "data: "))
	}
	// 读取中断（连接重置、单行超限等）必须显式报错：否则调用方会把截断当作正常结束。
	if err := scanner.Err(); err != nil {
		return nil, richErrorf(err, "failed to read upstream stream")
	}

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
	return state.usage, nil
}

// responsesStreamState 是 Responses SSE → chat chunk 的翻译状态。
type responsesStreamState struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	messageID string
	usage     *Usage
	usageSeen bool
	// toolIndex 记录 output_index → chat tool_calls[].index，保证序号连续。
	toolIndex map[int]int
}

// handleData 解析单条 SSE data 并按事件类型产出 chat chunk。
// 非法 JSON 与未知事件静默跳过，与 anthropicStreamTranslator 一致。
func (s *responsesStreamState) handleData(data string) {
	var event map[string]interface{}
	if json.Unmarshal([]byte(data), &event) != nil {
		return
	}
	eventType, _ := event["type"].(string)
	if s.usage = extractResponsesUsage(data, s.usage); s.usage != nil {
		s.usageSeen = true
	}

	switch eventType {
	case "response.created", "response.in_progress":
		s.captureID(event)
	case "response.output_text.delta":
		if delta, _ := event["delta"].(string); delta != "" {
			s.writeChunk(newOpenAIChunk(s.messageID, map[string]interface{}{"content": delta}, nil))
		}
	case "response.output_item.added":
		s.handleOutputItemAdded(event)
	case "response.function_call_arguments.delta":
		s.handleArgumentsDelta(event)
	case "response.completed", "response.incomplete", "response.failed":
		s.handleTerminal(eventType, event)
	}
}

// captureID 记录上游响应 ID，作为 chat chunk 的 id。
func (s *responsesStreamState) captureID(event map[string]interface{}) {
	resp, ok := event["response"].(map[string]interface{})
	if !ok {
		return
	}
	if id, ok := resp["id"].(string); ok && id != "" {
		s.messageID = id
	}
}

// handleOutputItemAdded 在 function_call 项出现时产出带 name 的首个 tool_call 分片。
func (s *responsesStreamState) handleOutputItemAdded(event map[string]interface{}) {
	item, ok := event["item"].(map[string]interface{})
	if !ok || item["type"] != "function_call" {
		return
	}
	outputIndex := intField(event, "output_index")
	idx := len(s.toolIndex)
	s.toolIndex[outputIndex] = idx

	callID, _ := item["call_id"].(string)
	name, _ := item["name"].(string)
	s.writeChunk(newOpenAIChunk(s.messageID, map[string]interface{}{
		"tool_calls": []map[string]interface{}{
			{
				"index":    idx,
				"id":       callID,
				"type":     "function",
				"function": map[string]interface{}{"name": name, "arguments": ""},
			},
		},
	}, nil))

	// 部分上游在 added 事件里直接带完整 arguments。
	if args, _ := item["arguments"].(string); args != "" {
		s.writeArgumentsDelta(idx, args)
	}
}

// handleArgumentsDelta 把工具参数增量译为 chat 的 tool_calls arguments 分片。
func (s *responsesStreamState) handleArgumentsDelta(event map[string]interface{}) {
	delta, _ := event["delta"].(string)
	if delta == "" {
		return
	}
	idx, ok := s.toolIndex[intField(event, "output_index")]
	if !ok {
		idx = len(s.toolIndex)
	}
	s.writeArgumentsDelta(idx, delta)
}

// writeArgumentsDelta 写出一条只含 tool_calls arguments 的 chat 分片。
func (s *responsesStreamState) writeArgumentsDelta(idx int, args string) {
	s.writeChunk(newOpenAIChunk(s.messageID, map[string]interface{}{
		"tool_calls": []map[string]interface{}{
			{"index": idx, "function": map[string]interface{}{"arguments": args}},
		},
	}, nil))
}

// handleTerminal 处理终止事件：补发带 finish_reason 与 usage 的收尾分片。
func (s *responsesStreamState) handleTerminal(eventType string, event map[string]interface{}) {
	finishReason := ""
	hasToolCall := len(s.toolIndex) > 0
	switch eventType {
	case "response.completed":
		finishReason = "stop"
		if hasToolCall {
			finishReason = "tool_calls"
		}
	case "response.incomplete":
		reason := ""
		if resp, ok := event["response"].(map[string]interface{}); ok {
			if details, ok := resp["incomplete_details"].(map[string]interface{}); ok {
				reason, _ = details["reason"].(string)
			}
		}
		finishReason = responsesFinishReason("incomplete", reason, hasToolCall)
	default: // response.failed
		s.writeChunk(newOpenAIChunk(s.messageID, map[string]interface{}{}, nil))
		return
	}

	chunk := newOpenAIChunk(s.messageID, map[string]interface{}{}, finishReason)
	if s.usage != nil {
		chunk["usage"] = chatUsageFromResponsesUsage(s.usage)
	}
	s.writeChunk(chunk)
}

// writeChunk 写出一个 chat 分片并立即 flush。
func (s *responsesStreamState) writeChunk(chunk map[string]interface{}) {
	writeSSEChunk(s.w, s.flusher, chunk)
}

// chatUsageFromResponsesUsage 把内部 Usage 渲染为 chat 形状的 usage 对象。
func chatUsageFromResponsesUsage(u *Usage) map[string]interface{} {
	return map[string]interface{}{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
		"prompt_tokens_details": map[string]interface{}{
			"cached_tokens":      u.PromptCacheReadTokens,
			"cache_write_tokens": u.PromptCacheWriteTokens,
		},
	}
}

// intField 读取 JSON 数字字段（float64 → int），缺失时返回 0。
func intField(m map[string]interface{}, key string) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return 0
}
