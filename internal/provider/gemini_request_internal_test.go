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
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// buildGeminiRequestBody 是测试助手：构造请求并解回 map 便于断言。
func buildGeminiRequestBody(t *testing.T, model, body string) map[string]any {
	t.Helper()
	out, err := buildGeminiRequest(model, []byte(body))
	if err != nil {
		t.Fatalf("buildGeminiRequest: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	return parsed
}

func TestBuildGeminiRequest_SystemInstruction(t *testing.T) {
	got := buildGeminiRequestBody(t, "gemini-2.5-flash", `{
		"messages":[
			{"role":"system","content":"be brief"},
			{"role":"system","content":"be kind"},
			{"role":"user","content":"hi"}]}`)

	si, ok := got["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("缺 systemInstruction: %v", got)
	}
	parts := si["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("两条 system 应各自成 part，got %d", len(parts))
	}
	contents := got["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("system 不应进 contents，got %d 条", len(contents))
	}
}

// 只有一条 system 消息时没有非 system 轮，contents 不能为空（上游会 400）。
func TestBuildGeminiRequest_SystemOnlyBecomesUser(t *testing.T) {
	got := buildGeminiRequestBody(t, "gemini-2.5-flash", `{"messages":[{"role":"system","content":"be brief"}]}`)
	contents := got["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents 长度 = %d, want 1", len(contents))
	}
	if role := contents[0].(map[string]any)["role"]; role != "user" {
		t.Fatalf("role = %v, want user", role)
	}
}

func TestBuildGeminiRequest_MergesConsecutiveSameRole(t *testing.T) {
	got := buildGeminiRequestBody(t, "gemini-2.5-flash", `{
		"messages":[
			{"role":"user","content":"a"},
			{"role":"user","content":"b"},
			{"role":"assistant","content":"c"},
			{"role":"assistant","content":"d"}]}`)
	contents := got["contents"].([]any)
	if len(contents) != 2 {
		t.Fatalf("连续同角色应合并，got %d 条", len(contents))
	}
	if role := contents[1].(map[string]any)["role"]; role != "model" {
		t.Fatalf("assistant 应映射为 model，got %v", role)
	}
}

// 用户消息没有 text part 时补空格，否则上游报 "must have a text parameter"。
func TestBuildGeminiRequest_BlankTextWhenNoTextPart(t *testing.T) {
	got := buildGeminiRequestBody(t, "gemini-2.5-flash", `{
		"messages":[{"role":"user","content":[
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	contents := got["contents"].([]any)
	parts := contents[0].(map[string]any)["parts"].([]any)
	hasText := false
	for _, p := range parts {
		if text, ok := p.(map[string]any)["text"].(string); ok {
			hasText = text != ""
		}
	}
	if !hasText {
		t.Fatalf("缺 text part，上游会 400: %v", parts)
	}
}

// 并行工具调用的多个 tool 结果必须合并成一条只含 functionResponse 的 user 消息。
func TestBuildGeminiRequest_MergesParallelToolResponses(t *testing.T) {
	got := buildGeminiRequestBody(t, "gemini-2.5-flash", `{
		"messages":[
			{"role":"user","content":"weather?"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"w","arguments":"{\"city\":\"A\"}"}},
				{"id":"call_2","type":"function","function":{"name":"w","arguments":"{\"city\":\"B\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"{\"t\":1}"},
			{"role":"tool","tool_call_id":"call_2","content":"{\"t\":2}"}]}`)

	contents := got["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents 长度 = %d, want 3（user / model / user）: %v", len(contents), contents)
	}
	last := contents[2].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("functionResponse 轮 role = %v, want user", last["role"])
	}
	parts := last["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("两个 functionResponse 应在同一条消息里，got %d", len(parts))
	}
	for _, p := range parts {
		pm := p.(map[string]any)
		if _, has := pm["text"]; has {
			t.Fatalf("functionResponse 轮不应混入 text part: %v", pm)
		}
	}
}

// functionCall.args 必须是 JSON 对象，不能是字符串。
func TestBuildGeminiRequest_ToolCallArgsIsObject(t *testing.T) {
	got := buildGeminiRequestBody(t, "gemini-2.5-flash", `{
		"messages":[
			{"role":"user","content":"x"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"w","arguments":"{\"city\":\"A\"}"}}]}]}`)

	parts := got["contents"].([]any)[1].(map[string]any)["parts"].([]any)
	fc, ok := parts[0].(map[string]any)["functionCall"].(map[string]any)
	if !ok {
		t.Fatalf("缺 functionCall: %v", parts)
	}
	if _, isObj := fc["args"].(map[string]any); !isObj {
		t.Fatalf("args 必须是对象，got %T = %v", fc["args"], fc["args"])
	}
}

// assistant 回传的思考文本要作为 thought part 带回去（不能丢掉历史里的思考块）。
func TestBuildGeminiRequest_ReasoningContentBecomesThoughtPart(t *testing.T) {
	got := buildGeminiRequestBody(t, "gemini-2.5-flash", `{
		"messages":[
			{"role":"user","content":"x"},
			{"role":"assistant","reasoning_content":"let me think","content":"the answer"},
			{"role":"user","content":"next"}]}`)

	parts := got["contents"].([]any)[1].(map[string]any)["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("期望 thought + text 两个 part，got %d", len(parts))
	}
	thought := parts[0].(map[string]any)
	if thought["thought"] != true || thought["text"] != "let me think" {
		t.Fatalf("thought part = %v", thought)
	}
	// text 必须排在 functionCall 之前；这里没有 functionCall，只校验 text 在后。
	if parts[1].(map[string]any)["text"] != "the answer" {
		t.Fatalf("text part = %v", parts[1])
	}
}

// functionResponse.response 必须是 Struct：合法 JSON 对象原样用，其余包一层 content。
func TestBuildGeminiRequest_FunctionResponseIsStruct(t *testing.T) {
	cases := []struct {
		name, toolContent string
		assert            func(t *testing.T, resp map[string]any)
	}{
		{
			name:        "json object kept",
			toolContent: `{\"temp\":21}`,
			assert: func(t *testing.T, resp map[string]any) {
				if resp["temp"] != float64(21) {
					t.Fatalf("JSON 对象应原样用，got %v", resp)
				}
			},
		},
		{
			name:        "plain text wrapped",
			toolContent: `sunny`,
			assert: func(t *testing.T, resp map[string]any) {
				if resp["content"] != "sunny" {
					t.Fatalf("非 JSON 应包成 content，got %v", resp)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildGeminiRequestBody(t, "gemini-2.5-flash", `{
				"messages":[
					{"role":"user","content":"x"},
					{"role":"assistant","content":null,"tool_calls":[
						{"id":"call_1","type":"function","function":{"name":"w","arguments":"{}"}}]},
					{"role":"tool","tool_call_id":"call_1","content":"`+c.toolContent+`"}]}`)
			parts := got["contents"].([]any)[2].(map[string]any)["parts"].([]any)
			resp := parts[0].(map[string]any)["functionResponse"].(map[string]any)["response"].(map[string]any)
			c.assert(t, resp)
		})
	}
}

// 签名必须剥离后发给上游：模型看到长 id 会把 base64 当文本复述（Bifrost e2e #272）。
func TestBuildGeminiRequest_StripsSignatureFromIDs(t *testing.T) {
	got := buildGeminiRequestBody(t, "gemini-2.5-flash", `{
		"messages":[
			{"role":"user","content":"x"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1_ts_AQID","type":"function","function":{"name":"w","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1_ts_AQID","content":"ok"}]}`)

	contents := got["contents"].([]any)
	callPart := contents[1].(map[string]any)["parts"].([]any)[0].(map[string]any)
	fc := callPart["functionCall"].(map[string]any)
	if fc["id"] != "call_1" {
		t.Fatalf("functionCall.id = %v, want call_1（签名必须剥离）", fc["id"])
	}
	// thoughtSignature 挂在 part 上，不在 functionCall 里。
	if callPart["thoughtSignature"] != "AQID" {
		t.Fatalf("thoughtSignature = %v, want AQID", callPart["thoughtSignature"])
	}
	fr := contents[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if fr["id"] != "call_1" {
		t.Fatalf("functionResponse.id = %v, want call_1", fr["id"])
	}
}

// 客户端回传的 id 里没有签名时，用 Gemini 认可的哨兵降级，而不是报 400。
func TestBuildGeminiRequest_MissingSignatureUsesSentinel(t *testing.T) {
	got := buildGeminiRequestBody(t, "gemini-2.5-flash", `{
		"messages":[
			{"role":"user","content":"x"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"w","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}]}`)

	callPart := got["contents"].([]any)[1].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if callPart["thoughtSignature"] != "skip_thought_signature_validator" {
		t.Fatalf("thoughtSignature = %v, want 哨兵", callPart["thoughtSignature"])
	}
}

// tool 结果后紧跟 user 文本时保持两条消息，不能把 functionResponse 和 text 混进一条。
func TestBuildGeminiRequest_ToolResultThenUserTextStaySeparate(t *testing.T) {
	got := buildGeminiRequestBody(t, "gemini-2.5-flash", `{
		"messages":[
			{"role":"user","content":"x"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"w","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"},
			{"role":"user","content":"and then?"}]}`)

	contents := got["contents"].([]any)
	if len(contents) != 4 {
		t.Fatalf("contents 长度 = %d, want 4: %v", len(contents), contents)
	}
	if _, has := contents[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"]; !has {
		t.Fatalf("第 3 条应是 functionResponse")
	}
	if _, has := contents[3].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"]; !has {
		t.Fatalf("第 4 条应是纯文本")
	}
}

func TestBuildGeminiRequest_Params(t *testing.T) {
	got := buildGeminiRequestBody(t, "gemini-2.5-flash", `{
		"messages":[{"role":"user","content":"x"}],
		"temperature":0.3,"top_p":0.9,"max_tokens":256,"stop":"END","n":1,
		"presence_penalty":0.1,"frequency_penalty":0.2}`)

	gc, ok := got["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("缺 generationConfig: %v", got)
	}
	if gc["temperature"] != 0.3 || gc["topP"] != 0.9 {
		t.Fatalf("temperature/topP = %v/%v", gc["temperature"], gc["topP"])
	}
	if gc["maxOutputTokens"] != float64(256) {
		t.Fatalf("maxOutputTokens = %v, want 256", gc["maxOutputTokens"])
	}
	stops, ok := gc["stopSequences"].([]any)
	if !ok || len(stops) != 1 || stops[0] != "END" {
		t.Fatalf("stop 字符串应包装成数组，got %v", gc["stopSequences"])
	}
	if gc["presencePenalty"] != 0.1 || gc["frequencyPenalty"] != 0.2 {
		t.Fatalf("penalty 映射错误: %v", gc)
	}
}

// 空 stop 数组不能发出 "stopSequences": []，严格上游会拒绝。
func TestBuildGeminiRequest_EmptyStopOmitted(t *testing.T) {
	got := buildGeminiRequestBody(t, "gemini-2.5-flash", `{
		"messages":[{"role":"user","content":"x"}],"stop":[]}`)
	if gc, ok := got["generationConfig"].(map[string]any); ok {
		if _, has := gc["stopSequences"]; has {
			t.Fatalf("空 stop 数组应省略，got %v", gc["stopSequences"])
		}
	}
}

// n>1 直接拒绝：响应侧只读 candidates[0]，放行会静默丢弃候选（design §4.3.2）。
func TestBuildGeminiRequest_RejectsN(t *testing.T) {
	_, err := buildGeminiRequest("gemini-2.5-flash", []byte(`{
		"messages":[{"role":"user","content":"x"}],"n":2}`))
	if err == nil {
		t.Fatalf("n=2 应被拒绝")
	}
	if !strings.Contains(err.Error(), "single candidate") {
		t.Fatalf("错误文案应说明只支持单候选，got %v", err)
	}
}

func TestBuildGeminiRequest_ToolChoice(t *testing.T) {
	cases := map[string]string{
		`"auto"`:     "AUTO",
		`"required"`: "ANY",
		`"none"`:     "NONE",
	}
	for choice, wantMode := range cases {
		got := buildGeminiRequestBody(t, "gemini-2.5-flash", `{
			"messages":[{"role":"user","content":"x"}],
			"tools":[{"type":"function","function":{"name":"w","parameters":{"type":"object"}}}],
			"tool_choice":`+choice+`}`)
		tc, ok := got["toolConfig"].(map[string]any)
		if !ok {
			t.Fatalf("choice=%s: 缺 toolConfig: %v", choice, got)
		}
		fcc := tc["functionCallingConfig"].(map[string]any)
		if fcc["mode"] != wantMode {
			t.Fatalf("choice=%s: mode = %v, want %s", choice, fcc["mode"], wantMode)
		}
	}

	got := buildGeminiRequestBody(t, "gemini-2.5-flash", `{
		"messages":[{"role":"user","content":"x"}],
		"tools":[{"type":"function","function":{"name":"w","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"w"}}}`)
	fcc := got["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)
	if fcc["mode"] != "ANY" {
		t.Fatalf("指定函数时 mode = %v, want ANY", fcc["mode"])
	}
	names := fcc["allowedFunctionNames"].([]any)
	if len(names) != 1 || names[0] != "w" {
		t.Fatalf("allowedFunctionNames = %v", names)
	}
}

// Gemini 的 responseSchema 不接受 additionalProperties。
func TestBuildGeminiRequest_ResponseSchemaStripsAdditionalProperties(t *testing.T) {
	got := buildGeminiRequestBody(t, "gemini-2.5-flash", `{
		"messages":[{"role":"user","content":"x"}],
		"response_format":{"type":"json_schema","json_schema":{
			"name":"out","strict":true,
			"schema":{"type":"object","additionalProperties":false,
				"properties":{"a":{"type":"string","additionalProperties":false}}}}}}`)

	gc := got["generationConfig"].(map[string]any)
	if gc["responseMimeType"] != "application/json" {
		t.Fatalf("responseMimeType = %v", gc["responseMimeType"])
	}
	schema, ok := gc["responseSchema"].(map[string]any)
	if !ok {
		t.Fatalf("缺 responseSchema: %v", gc)
	}
	if _, has := schema["additionalProperties"]; has {
		t.Fatalf("顶层不应有 additionalProperties: %v", schema)
	}
	props := schema["properties"].(map[string]any)
	if _, has := props["a"].(map[string]any)["additionalProperties"]; has {
		t.Fatalf("嵌套也不应保留 additionalProperties: %v", props)
	}
}

func TestBuildGeminiRequest_ReasoningEffort(t *testing.T) {
	got := buildGeminiRequestBody(t, "gemini-2.5-flash", `{
		"messages":[{"role":"user","content":"x"}],"reasoning_effort":"low","max_tokens":8192}`)
	tc, ok := got["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("缺 thinkingConfig: %v", got)
	}
	if tc["thinkingBudget"] != float64(1024) {
		t.Fatalf("thinkingBudget = %v, want 1024", tc["thinkingBudget"])
	}
}

// 未给 max_tokens 时用 defaultMaxTokens(4096)，与 anthropic 侧口径一致。
func TestBuildGeminiRequest_DefaultMaxOutputTokens(t *testing.T) {
	got := buildGeminiRequestBody(t, "gemini-2.5-flash", `{"messages":[{"role":"user","content":"x"}]}`)
	gc := got["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != float64(4096) {
		t.Fatalf("maxOutputTokens = %v, want 4096", gc["maxOutputTokens"])
	}
}

func TestBuildGeminiRequest_ImageParts(t *testing.T) {
	got := buildGeminiRequestBody(t, "gemini-2.5-flash", `{
		"messages":[{"role":"user","content":[
			{"type":"text","text":"what is this"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},
			{"type":"image_url","image_url":{"url":"https://e.com/a.jpg"}}]}]}`)

	parts := got["contents"].([]any)[0].(map[string]any)["parts"].([]any)
	var inline, file bool
	for _, p := range parts {
		pm := p.(map[string]any)
		if _, has := pm["inlineData"]; has {
			inline = true
		}
		if _, has := pm["fileData"]; has {
			file = true
		}
	}
	if !inline || !file {
		t.Fatalf("inlineData=%v fileData=%v, parts=%v", inline, file, parts)
	}
}

// models/ 前缀若不去掉，会拼出 /models/models/xxx。
func TestGeminiModelName(t *testing.T) {
	cases := map[string]string{
		"gemini-2.5-flash":        "gemini-2.5-flash",
		"models/gemini-2.5-flash": "gemini-2.5-flash",
		"models/models/x":         "models/x",
	}
	for in, want := range cases {
		if got := geminiModelName(in); got != want {
			t.Fatalf("geminiModelName(%q) = %q, want %q", in, got, want)
		}
	}
}

// 畸形 n（字符串、小数）应报「请求不合法」，而不是解码失败被当成上游故障 502。
func TestBuildGeminiRequest_MalformedNIsRequestError(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"x"}],"n":"2"}`,
		`{"messages":[{"role":"user","content":"x"}],"n":2.5}`,
		`{"messages":[{"role":"user","content":"x"}],"n":true}`,
	} {
		_, err := buildGeminiRequest("gemini-2.5-flash", []byte(body))
		if err == nil {
			t.Fatalf("body=%s 应报错", body)
		}
		var reqErr *geminiRequestError
		if !errors.As(err, &reqErr) {
			t.Fatalf("body=%s 应归为请求不合法（400），got %T = %v", body, err, err)
		}
	}
}

// 工具结果必须紧跟发起它的 model 轮：若其间夹了一条 user 文本，
// functionResponse 仍要先于该文本落到上游，否则 Gemini 关联不上调用。
func TestBuildGeminiRequest_ToolResponsePrecedesTrailingUserText(t *testing.T) {
	got := buildGeminiRequestBody(t, "gemini-2.5-flash", `{
		"messages":[
			{"role":"user","content":"x"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"w","arguments":"{}"}}]},
			{"role":"user","content":"note"},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}]}`)

	contents := got["contents"].([]any)
	// 期望顺序：user / model(functionCall) / user(functionResponse) / user(text)
	if len(contents) < 3 {
		t.Fatalf("contents = %v", contents)
	}
	third := contents[2].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if _, ok := third["functionResponse"]; !ok {
		t.Fatalf("第 3 条应是 functionResponse，got %v", contents)
	}
}
