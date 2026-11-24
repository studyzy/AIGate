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

// 本文件把 Gemini streamGenerateContent 的 SSE 帧翻译成 OpenAI chat 分片。
// 每帧形如 data: {"candidates":[...],"usageMetadata":{...}}，
// 帧内可能嵌 {"error":{...}}，必须转成错误而不是当空帧跳过（design §4.6）。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// geminiStreamTranslator 逐帧转换并写出分片。
type geminiStreamTranslator struct {
	w             http.ResponseWriter
	flusher       http.Flusher
	messageID     string
	chunkIndex    int
	toolCallIdx   int
	usage         *Usage
	wroteRolePart bool
}

// newGeminiStreamTranslator 创建翻译器（工具序号从 -1 起计数）。
func newGeminiStreamTranslator(w http.ResponseWriter, flusher http.Flusher) *geminiStreamTranslator {
	return &geminiStreamTranslator{w: w, flusher: flusher, toolCallIdx: -1}
}

// writeChunk 写出一个 OpenAI 分片并递增序号。
func (t *geminiStreamTranslator) writeChunk(chunk map[string]any) {
	writeSSEChunk(t.w, t.flusher, chunk)
	t.chunkIndex++
}

// handleFrame 处理一帧 SSE 数据；帧内嵌 error 时返回错误。
func (t *geminiStreamTranslator) handleFrame(data string) error {
	var frame struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
		geminiResponse
	}
	if err := json.Unmarshal([]byte(data), &frame); err != nil {
		// 单帧解析失败不致命：跳过该帧，继续读后续内容。
		return nil
	}
	if frame.Error != nil {
		return richErrorf(fmt.Errorf("upstream stream error"),
			"gemini stream error (status %d): %s %s",
			frame.Error.Code, frame.Error.Status, frame.Error.Message)
	}

	if frame.ResponseID != "" {
		t.messageID = frame.ResponseID
	}
	if u := frame.UsageMetadata.usage(); u != nil {
		t.usage = u
	}
	if len(frame.Candidates) == 0 {
		return nil
	}

	content, reasoning, toolCalls := geminiExtractParts(frame.Candidates[0].Content)
	if !t.wroteRolePart {
		t.writeChunk(buildOpenAIStreamChunk(t.messageID, t.chunkIndex, "assistant", "", ""))
		t.wroteRolePart = true
	}
	if reasoning != "" {
		t.writeChunk(buildReasoningStreamChunk(t.messageID, t.chunkIndex, reasoning))
	}
	if content != "" {
		t.writeChunk(buildOpenAIStreamChunk(t.messageID, t.chunkIndex, "", content, ""))
	}
	for _, raw := range toolCalls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		t.toolCallIdx++
		fn, _ := call["function"].(map[string]any)
		id, _ := call["id"].(string)
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		// Gemini 的 functionCall 是整块到达，直接给出完整 arguments。
		t.writeChunk(map[string]any{
			"id":      t.messageID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{"tool_calls": []any{toolCallChunk(t.toolCallIdx, id, name, args)}},
				"finish_reason": nil,
			}},
		})
	}

	if finish := geminiMapFinishReason(frame.Candidates[0].FinishReason); frame.Candidates[0].FinishReason != "" {
		// 有工具调用时必须以 tool_calls 收尾，否则客户端会以为只是普通结束。
		if t.toolCallIdx >= 0 {
			finish = "tool_calls"
		}
		usage := t.usage
		if usage == nil {
			usage = &Usage{}
		}
		t.writeChunk(buildOpenAIStreamChunkWithUsage(t.messageID, t.chunkIndex, finish, usage))
	}
	return nil
}

// toolCallChunk 构造 OpenAI 分片里的单个工具调用项。
func toolCallChunk(idx int, id, name, args string) map[string]any {
	return map[string]any{
		"index": idx,
		"id":    id,
		"type":  "function",
		"function": map[string]any{
			"name":      name,
			"arguments": args,
		},
	}
}
