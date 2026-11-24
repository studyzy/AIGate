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

// 本文件把 OpenAI-chat 请求体转换成 Gemini generateContent 请求体。
// 规则见 design §4.2 / §4.2.1 / §4.3 / §5.3：
//   - system/developer → systemInstruction.parts
//   - 连续 user 合并、连续 assistant 合并（assistant → role:"model"）
//   - tool 结果累积后一次性 flush 成一条只含 functionResponse 的 user 消息
//   - model 轮内 thought part、text part、functionCall part 依次排列
//   - 签名从 call id 拆出放进 thoughtSignature；发上游前 id 剥回 base id

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
)

// geminiModelName 去掉可选的 models/ 前缀：管理员常把列表接口返回的
// "models/gemini-2.5-flash" 直接粘进 model_id，不去掉会拼出 /models/models/。
func geminiModelName(model string) string {
	return strings.TrimPrefix(model, "models/")
}

// buildGeminiRequest 把 OpenAI-chat 请求体转换成 Gemini generateContent 请求体。
func buildGeminiRequest(model string, reqBody []byte) ([]byte, error) {
	var oai oaiChatRequest
	if err := json.Unmarshal(reqBody, &oai); err != nil {
		return nil, richErrorf(err, "failed to decode request body")
	}
	if err := checkUnsupportedChatParams(reqBody); err != nil {
		return nil, err
	}

	gr := map[string]any{}
	if err := buildGeminiMessages(&oai, gr); err != nil {
		return nil, err
	}

	// model 由调用方在 URL 路径里拼，不放进请求体。
	if tools := buildGeminiTools(&oai); len(tools) > 0 {
		gr["tools"] = []any{map[string]any{"functionDeclarations": tools}}
		if tc := buildGeminiToolConfig(&oai); tc != nil {
			gr["toolConfig"] = tc
		}
	}
	if gc := buildGeminiGenerationConfig(model, reqBody, &oai); len(gc) > 0 {
		gr["generationConfig"] = gc
	}
	return json.Marshal(gr)
}

// checkUnsupportedChatParams 拒绝 n>1：响应侧只读 candidates[0]，
// 放行会让多出来的候选被静默丢弃（design §4.3.2）。
// n 用 json.Number 读出，畸形的 n（字符串、小数、布尔）归为调用方请求错误，
// 而不是解码失败被上层当成上游故障返回 502。
func checkUnsupportedChatParams(reqBody []byte) error {
	var probe struct {
		N json.Number `json:"n"`
	}
	if err := json.Unmarshal(reqBody, &probe); err != nil {
		return &geminiRequestError{msg: "invalid n: " + err.Error()}
	}
	if probe.N == "" {
		return nil
	}
	n, err := probe.N.Float64()
	if err != nil {
		return &geminiRequestError{msg: "invalid n: " + probe.N.String()}
	}
	if n > 1 {
		return &geminiRequestError{msg: "gemini-generate-content upstream supports a single candidate"}
	}
	return nil
}

// buildGeminiMessages 填充 gr 的 contents 与 systemInstruction。
//
// 工具结果的落位是这里最容易出错的地方：Gemini 要求 functionResponse 紧跟
// 发起它的 model 轮，而 OpenAI 的 tool 消息可以出现在任何位置。做法是记下
// 每个 assistant(tool_calls) 轮的下标，tool 结果到了以后插到"对应调用轮之后
// 的第一个位置"——若下一轮已经是 user，就插在它前面。
func buildGeminiMessages(oai *oaiChatRequest, gr map[string]any) error {
	var systemParts []any
	var contents []any
	var pendingToolParts []any
	// callID → 工具名。键用完整 id（客户端回传的就是完整 id）。
	callNames := map[string]string{}
	// callID → 该 functionCall 所在 contents 下标。
	callTurns := map[string]int{}
	// pendingAfter 是待落地工具结果应插入的下标；-1 表示追加到末尾。
	pendingAfter := -1
	seenNonSystem := false

	flushTools := func() {
		if len(pendingToolParts) == 0 {
			return
		}
		turn := map[string]any{"role": "user", "parts": pendingToolParts}
		pendingToolParts = nil
		at := pendingAfter
		pendingAfter = -1
		if at < 0 || at > len(contents) {
			contents = append(contents, turn)
			return
		}
		contents = append(contents, nil)
		copy(contents[at+1:], contents[at:])
		contents[at] = turn
		// 插入后，已记录的调用轮下标要顺延。
		for id, idx := range callTurns {
			if idx >= at {
				callTurns[id] = idx + 1
			}
		}
	}
	appendTurn := func(role string, parts []any) {
		if len(parts) == 0 {
			return
		}
		if n := len(contents); n > 0 {
			prev := contents[n-1].(map[string]any)
			if prev["role"] == role && !hasFunctionResponse(prev) {
				prev["parts"] = append(prev["parts"].([]any), parts...)
				return
			}
		}
		contents = append(contents, map[string]any{"role": role, "parts": parts})
	}

	for _, m := range oai.Messages {
		switch m.Role {
		case "system", "developer":
			text := contentToString(m.Content)
			if text == "" {
				continue
			}
			if seenNonSystem {
				flushTools()
				appendTurn("user", []any{map[string]any{"text": text}})
				continue
			}
			systemParts = append(systemParts, map[string]any{"text": text})
		case "tool":
			pendingToolParts = append(pendingToolParts, geminiFunctionResponse(m, callNames))
			if pendingAfter < 0 {
				// 定位到对应调用轮之后、且在任何新轮之前。
				if idx, ok := callTurns[stripGeminiCallID(m.ToolCallID)]; ok {
					pendingAfter = idx + 1
				} else if idx, ok := callTurns[m.ToolCallID]; ok {
					pendingAfter = idx + 1
				}
			}
		default:
			seenNonSystem = true
			flushTools()
			if m.Role == "assistant" {
				before := len(contents)
				appendTurn("model", geminiAssistantParts(m, callNames))
				if len(contents) > before {
					for _, tc := range m.ToolCalls {
						callTurns[stripGeminiCallID(tc.ID)] = len(contents) - 1
					}
				}
				continue
			}
			parts, err := geminiUserParts(m.Content)
			if err != nil {
				return err
			}
			appendTurn("user", parts)
		}
	}
	flushTools()

	if len(systemParts) > 0 {
		gr["systemInstruction"] = map[string]any{"parts": systemParts}
	}
	if len(contents) == 0 {
		// 上游要求 contents 非空。
		contents = append(contents, map[string]any{"role": "user", "parts": []any{map[string]any{"text": " "}}})
	}
	gr["contents"] = contents
	return nil
}

// hasFunctionResponse 判断一条轮次是否含 functionResponse part。
func hasFunctionResponse(turn map[string]any) bool {
	parts, ok := turn["parts"].([]any)
	if !ok {
		return false
	}
	for _, p := range parts {
		if m, ok := p.(map[string]any); ok {
			if _, has := m["functionResponse"]; has {
				return true
			}
		}
	}
	return false
}

// geminiAssistantParts 构造 model 轮。顺序契约：thought part、text part、
// functionCall part 依次排列（functionCall 必须收尾 model 轮）。
func geminiAssistantParts(m oaiInputMessage, callNames map[string]string) []any {
	var parts []any
	if rc := strings.TrimSpace(m.ReasoningContent); rc != "" {
		parts = append(parts, map[string]any{"text": rc, "thought": true})
	}
	if text := contentToString(m.Content); text != "" {
		parts = append(parts, map[string]any{"text": text})
	}
	for _, tc := range m.ToolCalls {
		baseID, sig := splitGeminiCallID(tc.ID)
		call := map[string]any{
			"name": tc.Function.Name,
			"args": geminiArgsObject(tc.Function.Arguments),
		}
		if baseID != "" {
			call["id"] = baseID
		}
		part := map[string]any{"functionCall": call}
		switch {
		case len(sig) > 0:
			part["thoughtSignature"] = base64.RawURLEncoding.EncodeToString(sig)
		case tc.ID != "":
			// 没有签名时填哨兵，避免上游因缺签名 400。
			part["thoughtSignature"] = geminiSkipSignatureValidator
		}
		parts = append(parts, part)
		callNames[tc.ID] = tc.Function.Name
	}
	return parts
}

// geminiUserParts 构造 user 轮；没有 text part 时补一个空格。
// 图片 URL 不可用时返回错误而不是静默丢图。
func geminiUserParts(content any) ([]any, error) {
	var parts []any
	hasText := false
	appendText := func(text string) {
		if text == "" {
			return
		}
		parts = append(parts, map[string]any{"text": text})
		hasText = true
	}

	switch v := content.(type) {
	case string:
		appendText(v)
	case []any:
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch m["type"] {
			case "text":
				text, _ := m["text"].(string)
				appendText(text)
			case "image_url":
				var rawURL string
				switch iu := m["image_url"].(type) {
				case string:
					rawURL = iu
				case map[string]any:
					rawURL, _ = iu["url"].(string)
				}
				if rawURL == "" {
					continue
				}
				part, err := geminiImageURLToPart(rawURL)
				if err != nil {
					return nil, err
				}
				parts = append(parts, part)
			}
		}
	}

	if len(parts) > 0 && !hasText {
		parts = append(parts, map[string]any{"text": " "})
	}
	return parts, nil
}

// geminiFunctionResponse 构造 functionResponse part。
// name 用 callNames[完整 id] 查，查不到回落 base id。
func geminiFunctionResponse(m oaiInputMessage, callNames map[string]string) map[string]any {
	name := callNames[m.ToolCallID]
	if name == "" {
		// 客户端只发了最近几轮、或从 previous_response_id 续接时拿不到前序 tool_calls，
		// 只能回落 base id。记一条 warn，否则工具调用降级会无迹可循（design §4.2 规则 4）。
		name = stripGeminiCallID(m.ToolCallID)
		slog.Warn("gemini functionResponse name unresolved, falling back to base id",
			"tool_call_id", m.ToolCallID)
	}
	fr := map[string]any{
		"name":     name,
		"response": geminiResponseObject(contentToString(m.Content)),
	}
	if baseID := stripGeminiCallID(m.ToolCallID); baseID != "" {
		fr["id"] = baseID
	}
	return map[string]any{"functionResponse": fr}
}

// geminiArgsObject 把 arguments 字符串转成 JSON 对象；空串/非法 JSON 返回空对象。
func geminiArgsObject(args string) any {
	if strings.TrimSpace(args) == "" {
		return map[string]any{}
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(args), &obj); err != nil {
		return map[string]any{}
	}
	return obj
}

// geminiResponseObject 把工具结果转成 Struct：合法 JSON 对象原样用，
// 其余包成 {"content": ...}——Gemini 要求它是 Struct。
func geminiResponseObject(content string) any {
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "{") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
			return obj
		}
	}
	return map[string]any{"content": content}
}

// buildGeminiTools 把 OpenAI tools 转成 functionDeclarations。
func buildGeminiTools(oai *oaiChatRequest) []any {
	var decls []any
	for _, t := range oai.Tools {
		d := map[string]any{"name": t.Function.Name}
		if t.Function.Description != "" {
			d["description"] = t.Function.Description
		}
		if len(t.Function.Parameters) > 0 {
			var params map[string]any
			if err := json.Unmarshal(t.Function.Parameters, &params); err == nil {
				d["parameters"] = params
			}
		}
		decls = append(decls, d)
	}
	return decls
}

// buildGeminiToolConfig 把 tool_choice 转成 toolConfig。
func buildGeminiToolConfig(oai *oaiChatRequest) map[string]any {
	if oai.ToolChoice == nil {
		return nil
	}
	switch tc := oai.ToolChoice.(type) {
	case string:
		mode := ""
		switch tc {
		case "auto":
			mode = "AUTO"
		case "required":
			mode = "ANY"
		case "none":
			mode = "NONE"
		}
		if mode == "" {
			return nil
		}
		return map[string]any{"functionCallingConfig": map[string]any{"mode": mode}}
	case map[string]any:
		fn, ok := tc["function"].(map[string]any)
		if !ok {
			return nil
		}
		name, _ := fn["name"].(string)
		if name == "" {
			return nil
		}
		return map[string]any{"functionCallingConfig": map[string]any{
			"mode":                 "ANY",
			"allowedFunctionNames": []any{name},
		}}
	}
	return nil
}

// geminiMaxStopSequences 是 Gemini 允许的 stop 序列条数上限。
const geminiMaxStopSequences = 5

// buildGeminiGenerationConfig 组装 generationConfig。
func buildGeminiGenerationConfig(model string, reqBody []byte, oai *oaiChatRequest) map[string]any {
	var raw map[string]any
	_ = json.Unmarshal(reqBody, &raw)
	gc := map[string]any{}

	for _, k := range []string{"temperature", "top_p", "seed", "frequency_penalty", "presence_penalty"} {
		if v, ok := raw[k].(float64); ok {
			gc[geminiParamNames[k]] = v
		}
	}

	maxOutput := defaultMaxTokens
	switch {
	case oai.MaxCompletionTokens != nil && *oai.MaxCompletionTokens > 0:
		maxOutput = *oai.MaxCompletionTokens
	case oai.MaxTokens != nil && *oai.MaxTokens > 0:
		maxOutput = *oai.MaxTokens
	}
	gc["maxOutputTokens"] = maxOutput

	if stops := geminiStopSequences(raw["stop"]); len(stops) > 0 {
		gc["stopSequences"] = stops
	}

	applyGeminiResponseFormat(raw["response_format"], gc)

	if tc := geminiThinkingConfig(model, oai.ReasoningEffort, maxOutput); tc != nil {
		gc["thinkingConfig"] = tc
	}
	return gc
}

// geminiParamNames 是直通参数在 generationConfig 里的名字。
var geminiParamNames = map[string]string{
	"temperature":       "temperature",
	"top_p":             "topP",
	"seed":              "seed",
	"frequency_penalty": "frequencyPenalty",
	"presence_penalty":  "presencePenalty",
}

// geminiStopSequences 把 stop（字符串或数组）转成 stopSequences；空/无效返回 nil。
func geminiStopSequences(v any) []any {
	var out []any
	switch s := v.(type) {
	case string:
		if s != "" {
			out = append(out, s)
		}
	case []any:
		for _, item := range s {
			text, ok := item.(string)
			if !ok || text == "" {
				continue
			}
			out = append(out, text)
			if len(out) == geminiMaxStopSequences {
				break
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// applyGeminiResponseFormat 把 response_format 映射为 responseMimeType/responseSchema。
// Gemini 的 responseSchema 不接受 additionalProperties，需递归剥除（design §4.3.3）。
func applyGeminiResponseFormat(v any, gc map[string]any) {
	rf, ok := v.(map[string]any)
	if !ok {
		return
	}
	switch rf["type"] {
	case "json_object":
		gc["responseMimeType"] = "application/json"
	case "json_schema":
		gc["responseMimeType"] = "application/json"
		js, ok := rf["json_schema"].(map[string]any)
		if !ok {
			return
		}
		schema, ok := js["schema"].(map[string]any)
		if !ok {
			return
		}
		gc["responseSchema"] = stripAdditionalProperties(schema)
	}
}

// stripAdditionalProperties 递归删除 additionalProperties 键。
func stripAdditionalProperties(schema map[string]any) map[string]any {
	out := make(map[string]any, len(schema))
	for k, v := range schema {
		if k == "additionalProperties" {
			continue
		}
		out[k] = stripAdditionalPropertiesValue(v)
	}
	return out
}

// stripAdditionalPropertiesValue 对任意 JSON 值递归应用剥除。
func stripAdditionalPropertiesValue(v any) any {
	switch node := v.(type) {
	case map[string]any:
		return stripAdditionalProperties(node)
	case []any:
		out := make([]any, len(node))
		for i, item := range node {
			out[i] = stripAdditionalPropertiesValue(item)
		}
		return out
	default:
		return v
	}
}
