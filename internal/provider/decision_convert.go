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

// 本文件实现决策协议两种线上格式与 IR 的双向编解码：
//   /v1/decisions  ↔ IR ↔ systemone 上游
//   /v1/systemone  ↔ IR ↔ openai-decisions 上游
// 同格式走一遍也经过这里（规范化入站体，剥掉调用方多塞的字段）。
//
// 有损转换一律显式报错（ErrNotSupportedField），不静默丢弃：跨协议时
// assets / safety_identifier / 非 user 角色消息都在上游格式里无处安放。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// ErrNotSupportedField 表示某个字段无法被目标协议表达。调用方据此返回 400。
var ErrNotSupportedField = errors.New("field is not supported by upstream protocol")

// ---------------------------------------------------------------------------
// 有序对象解码
// ---------------------------------------------------------------------------

// decodeOrderedObject 按 JSON 文本里的出现顺序返回对象 key 与各值。
// System One 的 questions / answers 是有序对象，Go 的 map 会丢顺序，
// 而 /v1/decisions 入口要求答案顺序与提问顺序一致，故这里手工保序。
func decodeOrderedObject(data []byte) ([]string, map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, errors.New("expected a JSON object")
	}
	var keys []string
	values := make(map[string]json.RawMessage)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, nil, errors.New("expected an object key")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, nil, err
		}
		keys = append(keys, key)
		values[key] = raw
	}
	if _, err := dec.Token(); err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, err
	}
	return keys, values, nil
}

// ---------------------------------------------------------------------------
// 上限
// ---------------------------------------------------------------------------

const (
	// maxDecisionQuestions 是单次请求允许的提问数（两个协议都按 128 取交集）。
	maxDecisionQuestions = 128
	// maxOpenAIChoices / maxSystemOneChoices 是 choice 选项数上限（下限不同：2 / 1）。
	maxOpenAIChoices    = 255
	maxSystemOneChoices = 255
	// maxScoreLevels 是 score 档位数上限（两个协议都是 10）。
	maxScoreLevels = 10
)

// ---------------------------------------------------------------------------
// 入口 → IR
// ---------------------------------------------------------------------------

// 两条入口接受的顶层字段白名单。决策协议没有生成参数，任何未列出的键
// （stream 的任意真值写法、max_tokens / temperature / top_p / tools …）一律 400：
// 静默丢弃会让客户端以为参数生效，属于 spec §4 的反目标。
var (
	decisionsTopFields = map[string]struct{}{
		"model": {}, "input": {}, "questions": {}, "safety_identifier": {},
	}
	systemOneTopFields = map[string]struct{}{
		"model": {}, "state": {}, "questions": {}, "images": {}, "assets": {},
	}
)

// rejectUnknownFields 对未在白名单内的顶层键返回 400，消息里点名第一个未知键。
func rejectUnknownFields(top map[string]json.RawMessage, allowed map[string]struct{}, style string) error {
	unknown := make([]string, 0, len(top))
	for key := range top {
		if _, ok := allowed[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return badRequest(unknown[0],
		"%s requests do not accept %q (unsupported field)", style, unknown[0])
}

// DecodeDecisionsRequest 把 OpenAI Decisions 入口请求体解析成 IR。
func DecodeDecisionsRequest(body []byte) (*DecisionRequest, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, badRequest("", "invalid json body")
	}
	if raw, ok := top["stream"]; ok {
		var stream bool
		if err := json.Unmarshal(raw, &stream); err == nil && stream {
			return nil, badRequest("stream", "streaming is not supported on decision models")
		}
		return nil, badRequest("stream", "streaming is not supported on decision models")
	}
	if err := rejectUnknownFields(top, decisionsTopFields, "decisions"); err != nil {
		return nil, err
	}
	req := &DecisionRequest{}
	if err := json.Unmarshal(top["model"], &req.Model); err != nil || strings.TrimSpace(req.Model) == "" {
		return nil, badRequest("model", "model is required")
	}
	rawInput, ok := top["input"]
	if !ok {
		return nil, badRequest("input", "input is required")
	}
	if err := decodeOpenAIInput(rawInput, req); err != nil {
		return nil, err
	}
	if err := decodeOpenAIQuestions(top["questions"], req); err != nil {
		return nil, err
	}
	if raw, ok := top["safety_identifier"]; ok {
		_ = json.Unmarshal(raw, &req.SafetyID)
	}
	return req, nil
}

// decodeOpenAIInput 解析 input：string 直接进 State，message 数组拍平成 Messages。
func decodeOpenAIInput(raw json.RawMessage, req *DecisionRequest) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return badRequest("input", "input is required")
	}
	if trimmed[0] == '"' {
		req.State = append(json.RawMessage(nil), trimmed...)
		return nil
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(trimmed, &messages); err != nil {
		return badRequest("input", "input must be a string or an array of messages")
	}
	for i, rawMsg := range messages {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rawMsg, &m); err != nil {
			return badRequest(fmt.Sprintf("input[%d]", i), "message must be an object")
		}
		var role string
		_ = json.Unmarshal(m["role"], &role)
		if role != "user" && role != "system" && role != "developer" {
			return fmt.Errorf("%w: input[%d].role %q", ErrNotSupportedField, i, role)
		}
		msg := DecisionMessage{Role: role}
		if rawContent, ok := m["content"]; ok {
			text, images, err := decodeOpenAIContent(rawContent)
			if err != nil {
				return err
			}
			msg.Text, msg.Images = text, images
		}
		req.Messages = append(req.Messages, msg)
	}
	if len(req.Messages) == 0 {
		return badRequest("input", "input must contain at least one message")
	}
	return nil
}

// decodeOpenAIContent 解析 message.content：string 或 input_text / input_image part 数组。
func decodeOpenAIContent(raw json.RawMessage) (text string, images []string, err error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return "", nil, badRequest("content", "content must be a string")
		}
		return text, nil, nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return "", nil, badRequest("content", "content must be a string or an array of parts")
	}
	var texts []string
	for _, part := range parts {
		var typ string
		_ = json.Unmarshal(part["type"], &typ)
		switch typ {
		case "input_text", "text":
			var t string
			if err := json.Unmarshal(part["text"], &t); err != nil {
				return "", nil, badRequest("content", "input_text part requires text")
			}
			texts = append(texts, t)
		case "input_image", "image_url":
			var u string
			if err := json.Unmarshal(part["image_url"], &u); err != nil || u == "" {
				var obj struct {
					URL string `json:"url"`
				}
				if err := json.Unmarshal(part["image_url"], &obj); err != nil || obj.URL == "" {
					return "", nil, badRequest("content", "input_image part requires image_url")
				}
				u = obj.URL
			}
			images = append(images, u)
		default:
			return "", nil, fmt.Errorf("%w: content part %q", ErrNotSupportedField, typ)
		}
	}
	return strings.Join(texts, "\n"), images, nil
}

// decodeOpenAIQuestions 解析 questions 数组。
func decodeOpenAIQuestions(raw json.RawMessage, req *DecisionRequest) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return badRequest("questions", "questions is required")
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return badRequest("questions", "questions must be an array")
	}
	if len(items) == 0 || len(items) > maxDecisionQuestions {
		return badRequest("questions", "questions must contain 1 to %d entries", maxDecisionQuestions)
	}
	for i, item := range items {
		q := DecisionQuestion{}
		field := fmt.Sprintf("questions[%d]", i)
		_ = json.Unmarshal(item["name"], &q.Name)
		if q.Name == "" {
			q.Name = fmt.Sprintf("%d", i)
		}
		var typ string
		_ = json.Unmarshal(item["type"], &typ)
		switch typ {
		case "predicate":
			q.Type = DecisionPredicate
		case "choice":
			q.Type = DecisionPick
		case "score":
			q.Type = DecisionScore
		case "box":
			q.Type = DecisionFrame
		case "noul":
			return badRequest(field+".type", "noul is a System One question type; use predicate on this endpoint")
		default:
			return badRequest(field+".type", "type must be predicate, choice, score or box")
		}
		_ = json.Unmarshal(item["instructions"], &q.Instructions)
		switch q.Type {
		case DecisionPick:
			choices, err := decodeOpenAIChoices(item["choices"], field)
			if err != nil {
				return err
			}
			q.Choices = choices
		case DecisionScore:
			levels, err := decodeOpenAILevels(item["levels"], field)
			if err != nil {
				return err
			}
			q.Levels = levels
		}
		req.Questions = append(req.Questions, q)
	}
	return nil
}

func decodeOpenAIChoices(raw json.RawMessage, field string) ([]DecisionOption, error) {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, badRequest(field+".choices", "choices is required")
	}
	if len(items) < 2 || len(items) > maxOpenAIChoices {
		return nil, badRequest(field+".choices", "choices must contain 2 to %d entries", maxOpenAIChoices)
	}
	seen := make(map[string]struct{}, len(items))
	out := make([]DecisionOption, 0, len(items))
	for _, item := range items {
		var c DecisionOption
		var boolValue bool
		rawValue := bytes.TrimSpace(item["value"])
		switch {
		case len(rawValue) > 0 && rawValue[0] == '"':
			if err := json.Unmarshal(rawValue, &c.Value); err != nil {
				return nil, badRequest(field+".choices", "choice value is invalid")
			}
		case string(rawValue) == "true" || string(rawValue) == "false":
			if err := json.Unmarshal(rawValue, &boolValue); err != nil {
				return nil, badRequest(field+".choices", "choice value is invalid")
			}
			c.BoolValue = &boolValue
			c.Value = fmt.Sprintf("%t", boolValue)
		default:
			return nil, badRequest(field+".choices", "choice value must be a string or a boolean")
		}
		_ = json.Unmarshal(item["description"], &c.Description)
		if _, dup := seen[c.Value]; dup {
			return nil, badRequest(field+".choices", "choice values must be unique: %q", c.Value)
		}
		seen[c.Value] = struct{}{}
		out = append(out, c)
	}
	return out, nil
}

func decodeOpenAILevels(raw json.RawMessage, field string) ([]DecisionLevel, error) {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, badRequest(field+".levels", "levels is required")
	}
	if len(items) < 2 || len(items) > maxScoreLevels {
		return nil, badRequest(field+".levels", "levels must contain 2 to %d entries", maxScoreLevels)
	}
	out := make([]DecisionLevel, 0, len(items))
	for _, item := range items {
		var l DecisionLevel
		if err := json.Unmarshal(item["label"], &l.Label); err != nil || l.Label == "" {
			return nil, badRequest(field+".levels", "each level requires a label")
		}
		_ = json.Unmarshal(item["description"], &l.Description)
		out = append(out, l)
	}
	return out, nil
}

// DecodeSystemOneRequest 把 System One 入口请求体解析成 IR。questions 的顺序被保留。
func DecodeSystemOneRequest(body []byte) (*DecisionRequest, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, badRequest("", "invalid json body")
	}
	if raw, ok := top["stream"]; ok {
		var stream bool
		if err := json.Unmarshal(raw, &stream); err == nil && stream {
			return nil, badRequest("stream", "streaming is not supported on decision models")
		}
		return nil, badRequest("stream", "streaming is not supported on decision models")
	}
	if err := rejectUnknownFields(top, systemOneTopFields, "system one"); err != nil {
		return nil, err
	}
	req := &DecisionRequest{}
	if err := json.Unmarshal(top["model"], &req.Model); err != nil || strings.TrimSpace(req.Model) == "" {
		return nil, badRequest("model", "model is required")
	}
	rawState, ok := top["state"]
	if !ok || len(bytes.TrimSpace(rawState)) == 0 {
		return nil, badRequest("state", "state is required")
	}
	req.State = append(json.RawMessage(nil), bytes.TrimSpace(rawState)...)
	if raw, ok := top["assets"]; ok && len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		if err := json.Unmarshal(raw, &req.Assets); err != nil {
			return nil, badRequest("assets", "assets must be an object")
		}
	}
	if raw, ok := top["images"]; ok && len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		if err := json.Unmarshal(raw, &req.Images); err != nil {
			return nil, badRequest("images", "images must be an array of image references")
		}
	}
	rawQuestions, ok := top["questions"]
	if !ok || len(bytes.TrimSpace(rawQuestions)) == 0 {
		return nil, badRequest("questions", "questions is required")
	}
	keys, values, err := decodeOrderedObject(rawQuestions)
	if err != nil {
		return nil, badRequest("questions", "questions must be an object")
	}
	if len(keys) == 0 || len(keys) > maxDecisionQuestions {
		return nil, badRequest("questions", "questions must contain 1 to %d entries", maxDecisionQuestions)
	}
	for _, name := range keys {
		q, err := decodeSystemOneQuestion(name, values[name])
		if err != nil {
			return nil, err
		}
		req.Questions = append(req.Questions, q)
	}
	return req, nil
}

func decodeSystemOneQuestion(name string, raw json.RawMessage) (DecisionQuestion, error) {
	q := DecisionQuestion{Name: name}
	field := fmt.Sprintf("questions.%s", name)
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil {
		return q, badRequest(field, "question must be an object")
	}
	var typ string
	_ = json.Unmarshal(item["type"], &typ)
	switch typ {
	case "noul":
		q.Type = DecisionPredicate
	case "choice":
		q.Type = DecisionPick
	case "score":
		q.Type = DecisionScore
	case "box":
		q.Type = DecisionFrame
	case "predicate":
		return q, badRequest(field+".type", "predicate is an OpenAI Decisions question type; use noul on this endpoint")
	default:
		return q, badRequest(field+".type", "type must be noul, choice, score or box")
	}
	_ = json.Unmarshal(item["instructions"], &q.Instructions)
	switch q.Type {
	case DecisionPick:
		var criteria map[string]string
		if err := json.Unmarshal(item["criteria"], &criteria); err != nil {
			return q, badRequest(field+".criteria", "criteria is required")
		}
		if len(criteria) == 0 || len(criteria) > maxSystemOneChoices {
			return q, badRequest(field+".criteria", "criteria must contain 1 to %d entries", maxSystemOneChoices)
		}
		keys, _, err := decodeOrderedObject(item["criteria"])
		if err != nil {
			return q, badRequest(field+".criteria", "criteria must be an object")
		}
		for _, k := range keys {
			q.Choices = append(q.Choices, DecisionOption{Value: k, Description: criteria[k]})
		}
	case DecisionScore:
		var criteria []string
		if err := json.Unmarshal(item["criteria"], &criteria); err != nil {
			return q, badRequest(field+".criteria", "criteria is required")
		}
		if len(criteria) == 0 || len(criteria) > maxScoreLevels {
			return q, badRequest(field+".criteria", "criteria must contain 1 to %d entries", maxScoreLevels)
		}
		for _, label := range criteria {
			q.Levels = append(q.Levels, DecisionLevel{Label: label})
		}
	}
	return q, nil
}

// ---------------------------------------------------------------------------
// IR → 上游请求体
// ---------------------------------------------------------------------------

// EncodeOpenAIDecisionsRequest 把 IR 编成 OpenAI Decisions 请求体。
func EncodeOpenAIDecisionsRequest(req *DecisionRequest) ([]byte, error) {
	if len(req.Assets) > 0 {
		return nil, fmt.Errorf("%w: assets", ErrNotSupportedField)
	}
	type choicePayload struct {
		Value       any    `json:"value"`
		Description string `json:"description,omitempty"`
	}
	type levelPayload struct {
		Label       string `json:"label"`
		Description string `json:"description,omitempty"`
	}
	type questionPayload struct {
		Type         string          `json:"type"`
		Name         string          `json:"name"`
		Instructions string          `json:"instructions"`
		Choices      []choicePayload `json:"choices,omitempty"`
		Levels       []levelPayload  `json:"levels,omitempty"`
	}
	out := map[string]any{
		"model":     req.Model,
		"questions": make([]questionPayload, 0, len(req.Questions)),
	}
	input, images, err := openAIInput(req)
	if err != nil {
		return nil, err
	}
	out["input"] = input
	if len(images) > 0 {
		out["input"] = imageMessage(images)
	}
	if req.SafetyID != "" {
		out["safety_identifier"] = req.SafetyID
	}
	questions := out["questions"].([]questionPayload)
	for _, q := range req.Questions {
		p := questionPayload{Type: string(q.Type), Name: q.Name, Instructions: q.Instructions}
		switch q.Type {
		case DecisionPick:
			for _, c := range q.Choices {
				var value any = c.Value
				if c.BoolValue != nil {
					value = *c.BoolValue
				}
				p.Choices = append(p.Choices, choicePayload{Value: value, Description: c.Description})
			}
		case DecisionScore:
			for _, l := range q.Levels {
				p.Levels = append(p.Levels, levelPayload{Label: l.Label, Description: l.Description})
			}
		}
		questions = append(questions, p)
	}
	out["questions"] = questions
	return json.Marshal(out)
}

// openAIInput 为 OpenAI 请求体准备 input 与需要单独带上的图片。
func openAIInput(req *DecisionRequest) (any, []string, error) {
	var images []string
	images = append(images, req.Images...)
	if len(req.Messages) > 0 {
		type part struct {
			Type     string `json:"type"`
			Text     string `json:"text,omitempty"`
			ImageURL string `json:"image_url,omitempty"`
		}
		type message struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []part `json:"content"`
		}
		out := make([]message, 0, len(req.Messages))
		for _, m := range req.Messages {
			content := []part{}
			if m.Text != "" {
				content = append(content, part{Type: "input_text", Text: m.Text})
			}
			for _, img := range m.Images {
				content = append(content, part{Type: "input_image", ImageURL: img})
			}
			out = append(out, message{Type: "message", Role: m.Role, Content: content})
		}
		return out, nil, nil
	}
	state := bytes.TrimSpace(req.State)
	if len(state) == 0 {
		return "", nil, badRequest("input", "input is required")
	}
	if state[0] == '"' {
		var text string
		if err := json.Unmarshal(state, &text); err != nil {
			return nil, nil, badRequest("state", "state must be valid JSON")
		}
		return text, images, nil
	}
	return string(state), images, nil
}

// imageMessage 把纯图片列表包成一条 user message 的 content part 数组。
func imageMessage(images []string) []map[string]any {
	content := make([]map[string]any, 0, len(images))
	for _, img := range images {
		content = append(content, map[string]any{"type": "input_image", "image_url": img})
	}
	return []map[string]any{{"type": "message", "role": "user", "content": content}}
}

// EncodeSystemOneRequest 把 IR 编成 System One 请求体。
// safety_identifier 是 OpenAI 专有字段，System One 无处安放，跨协议时显式报错。
func EncodeSystemOneRequest(req *DecisionRequest) ([]byte, error) {
	if req.SafetyID != "" {
		return nil, fmt.Errorf("%w: safety_identifier", ErrNotSupportedField)
	}
	state, images, err := systemOneState(req)
	if err != nil {
		return nil, err
	}
	type questionPayload struct {
		Type         string `json:"type"`
		Instructions string `json:"instructions"`
		Criteria     any    `json:"criteria,omitempty"`
	}
	questions := make(map[string]questionPayload, len(req.Questions))
	for _, q := range req.Questions {
		p := questionPayload{Type: string(systemOneType(q.Type)), Instructions: q.Instructions}
		switch q.Type {
		case DecisionPick:
			criteria := make(map[string]string, len(q.Choices))
			for _, c := range q.Choices {
				criteria[c.Value] = c.Description
			}
			p.Criteria = criteria
		case DecisionScore:
			labels := make([]string, 0, len(q.Levels))
			for _, l := range q.Levels {
				labels = append(labels, l.Label)
			}
			p.Criteria = labels
		}
		questions[q.Name] = p
	}
	out := map[string]any{
		"model":     req.Model,
		"state":     state,
		"questions": questions,
	}
	if len(images) > 0 {
		out["images"] = images
	}
	if len(req.Images) > 0 {
		out["images"] = append(out["images"].([]string), req.Images...)
	}
	if len(req.Assets) > 0 {
		out["assets"] = req.Assets
	}
	return json.Marshal(out)
}

// systemOneState 为 System One 请求体准备 state 与随行的图片。
func systemOneState(req *DecisionRequest) (any, []string, error) {
	if len(req.Messages) > 0 {
		var b strings.Builder
		var images []string
		for i, m := range req.Messages {
			if i > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(m.Text)
			images = append(images, m.Images...)
		}
		return b.String(), images, nil
	}
	state := bytes.TrimSpace(req.State)
	if len(state) == 0 {
		return nil, nil, badRequest("state", "state is required")
	}
	if state[0] == '"' {
		var text string
		if err := json.Unmarshal(state, &text); err != nil {
			return nil, nil, badRequest("state", "state must be valid JSON")
		}
		return text, nil, nil
	}
	var generic any
	if err := json.Unmarshal(state, &generic); err != nil {
		return nil, nil, badRequest("state", "state must be valid JSON")
	}
	return generic, nil, nil
}

// ---------------------------------------------------------------------------
// 上游响应体 → IR
// ---------------------------------------------------------------------------

// ParseOpenAIDecisionsResponse 解析 OpenAI Decisions 上游响应。
func ParseOpenAIDecisionsResponse(body []byte, req *DecisionRequest) (*DecisionResponse, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid upstream response: %w", err)
	}
	resp := &DecisionResponse{Raw: raw}
	_ = json.Unmarshal(raw["model"], &resp.Model)
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw["answers"], &items); err != nil {
		return resp, fmt.Errorf("invalid upstream response: answers: %w", err)
	}
	for i, item := range items {
		answer, err := parseOpenAIAnswer(item)
		if err != nil {
			return resp, fmt.Errorf("invalid upstream response: answers[%d]: %w", i, err)
		}
		resp.Answers = append(resp.Answers, answer)
	}
	resp.Usage = parseDecisionUsage(raw["usage"])
	return resp, nil
}

func parseOpenAIAnswer(item map[string]json.RawMessage) (DecisionAnswer, error) {
	var a DecisionAnswer
	var typ string
	_ = json.Unmarshal(item["type"], &typ)
	a.Type = DecisionQuestionType(typ)
	_ = json.Unmarshal(item["name"], &a.Name)
	if typ == "refusal" {
		a.Refused = true
		return a, nil
	}
	switch a.Type {
	case DecisionPredicate:
		var p float64
		if err := json.Unmarshal(item["probability"], &p); err != nil {
			return a, errors.New("predicate answer requires probability")
		}
		a.Probability = &p
	case DecisionPick:
		choice := bytes.TrimSpace(item["choice"])
		switch {
		case len(choice) > 0 && (string(choice) == "true" || string(choice) == "false"):
			var b bool
			_ = json.Unmarshal(choice, &b)
			a.ChoiceBool = &b
			a.Choice = fmt.Sprintf("%t", b)
		case len(choice) > 0:
			_ = json.Unmarshal(choice, &a.Choice)
		}
		var probs []map[string]json.RawMessage
		if err := json.Unmarshal(item["probabilities"], &probs); err == nil {
			for _, p := range probs {
				var item DecisionProbability
				value := bytes.TrimSpace(p["value"])
				if len(value) > 0 && value[0] == '"' {
					_ = json.Unmarshal(value, &item.Value)
				} else {
					var b bool
					if json.Unmarshal(value, &b) == nil {
						item.Value = fmt.Sprintf("%t", b)
					}
				}
				_ = json.Unmarshal(p["probability"], &item.Probability)
				a.Probabilities = append(a.Probabilities, item)
			}
		}
		a.Confidence = parseOptionalFloat(item["confidence"])
	case DecisionScore:
		a.Score = parseOptionalFloat(item["score"])
		a.Confidence = parseOptionalFloat(item["confidence"])
		a.Legend, a.Levels = parseScoreProbabilities(item["probabilities"])
	case DecisionFrame:
		a.Confidence = parseOptionalFloat(item["confidence"])
		if len(bytes.TrimSpace(item["box"])) > 0 {
			var box ComparisonBox
			if err := json.Unmarshal(item["box"], &box); err == nil {
				a.Box = &DecisionRegion{Top: box.Top, Bottom: box.Bottom, Left: box.Left, Right: box.Right}
			}
		}
	}
	return a, nil
}

// ComparisonBox 是 box 答案的线上形状（两个协议同名）。
type ComparisonBox struct {
	Top    float64 `json:"top"`
	Bottom float64 `json:"bottom"`
	Left   float64 `json:"left"`
	Right  float64 `json:"right"`
}

func parseScoreProbabilities(raw json.RawMessage) (legend []string, levels []DecisionScoreProbability) {
	var probs []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probs); err != nil {
		return nil, nil
	}
	for _, p := range probs {
		var item DecisionScoreProbability
		_ = json.Unmarshal(p["value"], &item.Value)
		_ = json.Unmarshal(p["probability"], &item.Probability)
		var label string
		_ = json.Unmarshal(p["label"], &label)
		for len(legend) <= item.Value {
			legend = append(legend, "")
		}
		legend[item.Value] = label
		levels = append(levels, item)
	}
	return legend, levels
}

func parseOptionalFloat(raw json.RawMessage) *float64 {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return &v
}

// ParseSystemOneResponse 解析 System One 上游响应；answers 按 key 顺序回填到请求顺序。
func ParseSystemOneResponse(body []byte, req *DecisionRequest) (*DecisionResponse, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid upstream response: %w", err)
	}
	resp := &DecisionResponse{Raw: raw}
	_ = json.Unmarshal(raw["model"], &resp.Model)
	resp.Usage = parseDecisionUsage(raw["usage"])
	if len(bytes.TrimSpace(raw["answers"])) == 0 {
		return resp, nil
	}
	keys, values, err := decodeOrderedObject(raw["answers"])
	if err != nil {
		return resp, fmt.Errorf("invalid upstream response: answers: %w", err)
	}
	byName := make(map[string]DecisionAnswer, len(keys))
	for _, k := range keys {
		answer, err := parseSystemOneAnswer(k, values[k])
		if err != nil {
			return resp, fmt.Errorf("invalid upstream response: answers.%s: %w", k, err)
		}
		byName[k] = answer
	}
	// 按请求顺序回填，上游漏答的题按拒答处理（OpenAI 入口的 refusal 语义）。
	for _, q := range req.Questions {
		answer, ok := byName[q.Name]
		if !ok {
			resp.Answers = append(resp.Answers, DecisionAnswer{Type: q.Type, Name: q.Name, Refused: true})
			continue
		}
		resp.Answers = append(resp.Answers, answer)
	}
	return resp, nil
}

func parseSystemOneAnswer(name string, raw json.RawMessage) (DecisionAnswer, error) {
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil {
		return DecisionAnswer{Name: name}, errors.New("answer must be an object")
	}
	var typ string
	_ = json.Unmarshal(item["type"], &typ)
	a := DecisionAnswer{Name: name}
	switch typ {
	case "noul":
		a.Type = DecisionPredicate
		p := parseOptionalFloat(item["noul"])
		if p == nil {
			p = parseOptionalFloat(item["probability"])
		}
		if p == nil {
			return a, errors.New("noul answer requires noul")
		}
		a.Noul = p
		a.Probability = p
	case "choice":
		a.Type = DecisionPick
		_ = json.Unmarshal(item["choice"], &a.Choice)
		var probs map[string]float64
		if err := json.Unmarshal(item["probabilities"], &probs); err == nil {
			keys, _, _ := decodeOrderedObject(item["probabilities"])
			for _, k := range keys {
				a.Probabilities = append(a.Probabilities, DecisionProbability{Value: k, Probability: probs[k]})
			}
		}
		a.Confidence = parseOptionalFloat(item["confidence"])
	case "score":
		a.Type = DecisionScore
		a.Score = parseOptionalFloat(item["score"])
		a.Confidence = parseOptionalFloat(item["confidence"])
		var probs map[string]float64
		if err := json.Unmarshal(item["probabilities"], &probs); err != nil {
			break
		}
		var legend map[string]string
		_ = json.Unmarshal(item["legend"], &legend)
		keys, _, _ := decodeOrderedObject(item["probabilities"])
		for _, k := range keys {
			var idx int
			if _, err := fmt.Sscanf(k, "%d", &idx); err != nil {
				continue
			}
			for len(a.Legend) <= idx {
				a.Legend = append(a.Legend, "")
			}
			a.Legend[idx] = legend[k]
			a.Levels = append(a.Levels, DecisionScoreProbability{Value: idx, Probability: probs[k]})
		}
	case "box":
		a.Type = DecisionFrame
		a.Confidence = parseOptionalFloat(item["confidence"])
		if len(bytes.TrimSpace(item["box"])) > 0 {
			var box ComparisonBox
			if err := json.Unmarshal(item["box"], &box); err == nil {
				a.Box = &DecisionRegion{Top: box.Top, Bottom: box.Bottom, Left: box.Left, Right: box.Right}
			}
		}
	case "refusal":
		a.Refused = true
	default:
		return a, fmt.Errorf("unknown answer type %q", typ)
	}
	return a, nil
}

// parseDecisionUsage 解析两协议同形的 usage 对象。
func parseDecisionUsage(raw json.RawMessage) *Usage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var u struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil
	}
	usage := &Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
	if u.InputTokensDetails != nil {
		usage.PromptCacheReadTokens = u.InputTokensDetails.CachedTokens
	}
	return usage
}

// ---------------------------------------------------------------------------
// IR → 入口响应体
// ---------------------------------------------------------------------------

// EncodeDecisionsResponse 把 IR 编成 /v1/decisions 的响应体（answers 数组 + usage）。
func EncodeDecisionsResponse(resp *DecisionResponse) ([]byte, error) {
	answers := make([]map[string]any, 0, len(resp.Answers))
	for _, a := range resp.Answers {
		item := map[string]any{"type": string(a.Type), "name": a.Name}
		if a.Refused {
			answers = append(answers, map[string]any{"type": "refusal", "name": a.Name})
			continue
		}
		switch a.Type {
		case DecisionPredicate:
			item["probability"] = floatOrZero(a.Probability)
		case DecisionPick:
			if a.ChoiceBool != nil {
				item["choice"] = *a.ChoiceBool
			} else {
				item["choice"] = a.Choice
			}
			probs := make([]map[string]any, 0, len(a.Probabilities))
			for _, p := range a.Probabilities {
				probs = append(probs, map[string]any{"value": p.Value, "probability": p.Probability})
			}
			item["probabilities"] = probs
			if a.Confidence != nil {
				item["confidence"] = *a.Confidence
			}
		case DecisionScore:
			item["score"] = floatOrZero(a.Score)
			probs := make([]map[string]any, 0, len(a.Levels))
			for _, p := range a.Levels {
				entry := map[string]any{"value": p.Value, "probability": p.Probability}
				if p.Value < len(a.Legend) && a.Legend[p.Value] != "" {
					entry["label"] = a.Legend[p.Value]
				}
				probs = append(probs, entry)
			}
			item["probabilities"] = probs
			if a.Confidence != nil {
				item["confidence"] = *a.Confidence
			}
		case DecisionFrame:
			if a.Box != nil {
				item["box"] = ComparisonBox{Top: a.Box.Top, Bottom: a.Box.Bottom, Left: a.Box.Left, Right: a.Box.Right}
			}
			if a.Confidence != nil {
				item["confidence"] = *a.Confidence
			}
		}
		answers = append(answers, item)
	}
	out := map[string]any{"model": resp.Model, "answers": answers}
	if usage := encodeDecisionUsage(resp.Usage); usage != nil {
		out["usage"] = usage
	}
	return json.Marshal(out)
}

// EncodeSystemOneResponse 把 IR 编成 /v1/systemone 的响应体（answers 对象 + usage）。
func EncodeSystemOneResponse(resp *DecisionResponse) ([]byte, error) {
	answers := make(map[string]any, len(resp.Answers))
	for _, a := range resp.Answers {
		if a.Refused {
			answers[a.Name] = map[string]any{"type": "refusal"}
			continue
		}
		item := map[string]any{"type": string(systemOneType(a.Type))}
		switch a.Type {
		case DecisionPredicate:
			item["noul"] = floatOrZero(a.Probability)
		case DecisionPick:
			item["choice"] = a.Choice
			probs := make(map[string]float64, len(a.Probabilities))
			for _, p := range a.Probabilities {
				probs[p.Value] = p.Probability
			}
			item["probabilities"] = probs
			if a.Confidence != nil {
				item["confidence"] = *a.Confidence
			}
		case DecisionScore:
			item["score"] = floatOrZero(a.Score)
			probs := make(map[string]float64, len(a.Levels))
			legend := make(map[string]string, len(a.Legend))
			for _, p := range a.Levels {
				key := fmt.Sprintf("%d", p.Value)
				probs[key] = p.Probability
				if p.Value < len(a.Legend) && a.Legend[p.Value] != "" {
					legend[key] = a.Legend[p.Value]
				}
			}
			item["probabilities"] = probs
			if len(legend) > 0 {
				item["legend"] = legend
			}
			if a.Confidence != nil {
				item["confidence"] = *a.Confidence
			}
		case DecisionFrame:
			if a.Box != nil {
				item["box"] = ComparisonBox{Top: a.Box.Top, Bottom: a.Box.Bottom, Left: a.Box.Left, Right: a.Box.Right}
			}
			if a.Confidence != nil {
				item["confidence"] = *a.Confidence
			}
		}
		answers[a.Name] = item
	}
	out := map[string]any{"model": resp.Model, "answers": answers}
	if usage := encodeDecisionUsage(resp.Usage); usage != nil {
		out["usage"] = usage
	}
	return json.Marshal(out)
}

// systemOneType 把 IR 类型映射回 System One 的线上类型名。
func systemOneType(t DecisionQuestionType) DecisionQuestionType {
	if t == DecisionPredicate {
		return "noul"
	}
	return t
}

func encodeDecisionUsage(u *Usage) map[string]any {
	if u == nil {
		return nil
	}
	return map[string]any{
		"input_tokens":  u.PromptTokens,
		"output_tokens": u.CompletionTokens,
		"total_tokens":  u.TotalTokens,
	}
}

func floatOrZero(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
