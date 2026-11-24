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

// 本文件把 Gemini generateContent 响应转成 OpenAI-chat 形状，
// 并统一用量口径（design §4.4 / §4.5）。

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// geminiFinishReasons 把 Gemini 的 finishReason 映射为 OpenAI 的 finish_reason。
// 与 litellm _FINISH_REASON_MAP 同口径；未收录的值回落 "stop"。
var geminiFinishReasons = map[string]string{
	"STOP":                      "stop",
	"MAX_TOKENS":                "length",
	"CONTINUATION":              "length",
	"SAFETY":                    "content_filter",
	"RECITATION":                "content_filter",
	"BLOCKLIST":                 "content_filter",
	"PROHIBITED_CONTENT":        "content_filter",
	"SPII":                      "content_filter",
	"LANGUAGE":                  "content_filter",
	"OTHER":                     "content_filter",
	"IMAGE_SAFETY":              "content_filter",
	"IMAGE_PROHIBITED_CONTENT":  "content_filter",
	"IMAGE_RECITATION":          "content_filter",
	"IMAGE_OTHER":               "content_filter",
	"NO_IMAGE":                  "content_filter",
	"FINISH_REASON_UNSPECIFIED": "stop",
	"MALFORMED_FUNCTION_CALL":   "stop",
	"MALFORMED_RESPONSE":        "stop",
	"TOO_MANY_TOOL_CALLS":       "stop",
	"UNEXPECTED_TOOL_CALL":      "stop",
}

// geminiMapFinishReason 查表；未收录返回 "stop"。
func geminiMapFinishReason(reason string) string {
	if mapped, ok := geminiFinishReasons[reason]; ok {
		return mapped
	}
	return "stop"
}

// NewGeminiUsage 组装用量。thoughtsTokenCount 在 candidatesTokenCount 之外，
// 必须加上（design §4.5，与 Bifrost 同口径）。流式与非流式共用，避免口径漂移。
func NewGeminiUsage(promptTokens, candidatesTokens, thoughtsTokens, cachedTokens, totalTokens int) *Usage {
	completion := candidatesTokens + thoughtsTokens
	if totalTokens <= 0 {
		totalTokens = promptTokens + completion
	}
	return &Usage{
		PromptTokens:           promptTokens,
		CompletionTokens:       completion,
		TotalTokens:            totalTokens,
		PromptCacheReadTokens:  cachedTokens,
		PromptCacheWriteTokens: 0,
	}
}

// geminiPart 是 Gemini content 里的一个 part（只取本协议用到的字段）。
type geminiPart struct {
	Text             string              `json:"text,omitempty"`
	Thought          bool                `json:"thought,omitempty"`
	ThoughtSignature string              `json:"thoughtSignature,omitempty"`
	FunctionCall     *geminiFunctionCall `json:"functionCall,omitempty"`
}

// geminiFunctionCall 是模型发起的一次工具调用。
type geminiFunctionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// geminiResponse 是非流式响应体（只取本协议用到的字段）。
type geminiResponse struct {
	ResponseID string `json:"responseId"`
	Candidates []struct {
		Index        int            `json:"index"`
		Content      *geminiContent `json:"content,omitempty"`
		FinishReason string         `json:"finishReason,omitempty"`
	} `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason,omitempty"`
	} `json:"promptFeedback,omitempty"`
	UsageMetadata *geminiUsageMetadata `json:"usageMetadata,omitempty"`
}

// geminiContent 是一轮候选内容。
type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts,omitempty"`
}

// geminiUsageMetadata 是上游用量。
type geminiUsageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
}

// usage 按统一口径折算。
func (u *geminiUsageMetadata) usage() *Usage {
	if u == nil {
		return nil
	}
	return NewGeminiUsage(u.PromptTokenCount, u.CandidatesTokenCount,
		u.ThoughtsTokenCount, u.CachedContentTokenCount, u.TotalTokenCount)
}

// geminiToOpenAI 把 Gemini 非流式响应转成 chat 形状。
func geminiToOpenAI(resp []byte) ([]byte, *Usage, error) {
	var gr geminiResponse
	if err := json.Unmarshal(resp, &gr); err != nil {
		return nil, nil, richErrorf(err, "failed to decode upstream response")
	}

	message := map[string]any{"role": "assistant", "content": ""}
	finishReason := "stop"

	switch {
	case gr.PromptFeedback != nil && gr.PromptFeedback.BlockReason != "":
		// 输入被拦截：报 content_filter，而不是空响应。
		finishReason = "content_filter"
	case len(gr.Candidates) > 0:
		cand := gr.Candidates[0]
		finishReason = geminiMapFinishReason(cand.FinishReason)
		content, reasoning, toolCalls := geminiExtractParts(cand.Content)
		message["content"] = content
		if reasoning != "" {
			message["reasoning_content"] = reasoning
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}
	}

	oai := map[string]any{
		"id":      gr.ResponseID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if u := gr.UsageMetadata.usage(); u != nil {
		oai["usage"] = map[string]any{
			"prompt_tokens":     u.PromptTokens,
			"completion_tokens": u.CompletionTokens,
			"total_tokens":      u.TotalTokens,
		}
	}

	body, err := json.Marshal(oai)
	if err != nil {
		return nil, nil, richErrorf(err, "failed to encode response body")
	}
	return body, gr.UsageMetadata.usage(), nil
}

// geminiExtractParts 从候选内容里拆出正文、思考文本与工具调用。
// thought part 的文本进 reasoning_content；functionCall 的签名编进 tool call id。
func geminiExtractParts(c *geminiContent) (content, reasoning string, toolCalls []any) {
	if c == nil {
		return "", "", nil
	}
	var textParts, thoughtParts []string
	for _, p := range c.Parts {
		switch {
		case p.FunctionCall != nil:
			args := "{}"
			if len(p.FunctionCall.Args) > 0 {
				args = string(p.FunctionCall.Args)
			}
			id := p.FunctionCall.ID
			if sig := geminiDecodeSignature(p.ThoughtSignature); sig != nil {
				id = encodeGeminiCallID(id, sig)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      p.FunctionCall.Name,
					"arguments": args,
				},
			})
		case p.Text != "" && p.Thought:
			thoughtParts = append(thoughtParts, p.Text)
		case p.Text != "":
			textParts = append(textParts, p.Text)
		}
	}
	return strings.Join(textParts, ""), strings.Join(thoughtParts, ""), toolCalls
}

// geminiDecodeSignature 把 part 上的 thoughtSignature 解成原始字节；
// 失败时返回 nil。签名是 base64（标准或 URL-safe 均可，Gemini 用标准编码）。
func geminiDecodeSignature(sig string) []byte {
	if sig == "" {
		return nil
	}
	if raw, err := base64.StdEncoding.DecodeString(sig); err == nil {
		return raw
	}
	if raw, err := base64.RawURLEncoding.DecodeString(sig); err == nil {
		return raw
	}
	return nil
}
