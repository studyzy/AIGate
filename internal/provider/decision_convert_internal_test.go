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

// 本文件钉住决策协议两种线上格式与 IR 的双向编解码：
// 入站校验（两道入口各自的规则）、跨协议映射、以及有损字段必须显式报错。

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const decisionsBody = `{
  "model": "gpt-6-luna",
  "input": "Customer wrote: I was charged twice for order 4411.",
  "questions": [
    {"type": "predicate", "name": "is_refund", "instructions": "Is the customer asking for a refund?"},
    {"type": "choice", "name": "dept", "instructions": "Which team should handle this",
     "choices": [{"value": "billing", "description": "payment issues"}, {"value": "technical"}]},
    {"type": "score", "name": "frustration", "instructions": "How frustrated",
     "levels": [{"label": "calm"}, {"label": "angry", "description": "shouting"}]},
    {"type": "box", "name": "region", "instructions": "Where is the receipt"}
  ]
}`

const systemOneBody = `{
  "model": "jev-latest",
  "state": "Customer wrote: I was charged twice for order 4411.",
  "questions": {
    "c_first": {"type": "noul", "instructions": "Is the customer asking for a refund?"},
    "a_second": {"type": "choice", "instructions": "Which team",
      "criteria": {"billing": "payment issues", "technical": "bugs"}},
    "b_third": {"type": "score", "instructions": "How frustrated",
      "criteria": ["calm", "angry"]}
  }
}`

func TestDecodeDecisionsRequest_AllQuestionTypes(t *testing.T) {
	req, err := DecodeDecisionsRequest([]byte(decisionsBody))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Model != "gpt-6-luna" {
		t.Fatalf("model = %q", req.Model)
	}
	if string(req.State) != `"Customer wrote: I was charged twice for order 4411."` {
		t.Fatalf("state = %s", req.State)
	}
	if len(req.Messages) != 0 {
		t.Fatalf("string input must not produce messages, got %d", len(req.Messages))
	}
	want := []DecisionQuestionType{DecisionPredicate, DecisionPick, DecisionScore, DecisionFrame}
	if len(req.Questions) != len(want) {
		t.Fatalf("questions = %d, want %d", len(req.Questions), len(want))
	}
	for i, w := range want {
		if req.Questions[i].Type != w {
			t.Fatalf("question %d type = %q, want %q", i, req.Questions[i].Type, w)
		}
	}
	if len(req.Questions[1].Choices) != 2 || req.Questions[1].Choices[0].Description != "payment issues" {
		t.Fatalf("choices = %+v", req.Questions[1].Choices)
	}
	if len(req.Questions[2].Levels) != 2 || req.Questions[2].Levels[1].Description != "shouting" {
		t.Fatalf("levels = %+v", req.Questions[2].Levels)
	}
}

func TestDecodeDecisionsRequest_Invalid(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{"no questions", `{"model":"m","input":"x"}`, "questions"},
		{"empty questions", `{"model":"m","input":"x","questions":[]}`, "questions"},
		{"systemone type", `{"model":"m","input":"x","questions":[{"type":"noul","instructions":"?"}]}`, "questions"},
		{"one choice", `{"model":"m","input":"x","questions":[{"type":"choice","instructions":"?","choices":[{"value":"a"}]}]}`, "choices"},
		{"duplicate choice", `{"model":"m","input":"x","questions":[{"type":"choice","instructions":"?","choices":[{"value":"a"},{"value":"a"}]}]}`, "choices"},
		{"eleven levels", `{"model":"m","input":"x","questions":[{"type":"score","instructions":"?","levels":[{"label":"0"},{"label":"1"},{"label":"2"},{"label":"3"},{"label":"4"},{"label":"5"},{"label":"6"},{"label":"7"},{"label":"8"},{"label":"9"},{"label":"10"}]}]}`, "levels"},
		{"stream", `{"model":"m","input":"x","stream":true,"questions":[{"type":"predicate","instructions":"?"}]}`, "stream"},
		{"missing model", `{"input":"x","questions":[{"type":"predicate","instructions":"?"}]}`, "model"},
		{"missing input", `{"model":"m","questions":[{"type":"predicate","instructions":"?"}]}`, "input"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeDecisionsRequest([]byte(tc.body))
			var bad *DecisionBadRequest
			if !errors.As(err, &bad) {
				t.Fatalf("err = %v, want DecisionBadRequest", err)
			}
			if !strings.Contains(bad.Field, tc.field) {
				t.Fatalf("field = %q, want it to mention %q", bad.Field, tc.field)
			}
		})
	}
}

func TestDecodeSystemOneRequest_OrderPreserved(t *testing.T) {
	req, err := DecodeSystemOneRequest([]byte(systemOneBody))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := []string{req.Questions[0].Name, req.Questions[1].Name, req.Questions[2].Name}
	want := []string{"c_first", "a_second", "b_third"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("question order = %v, want %v", got, want)
		}
	}
	if req.Questions[0].Type != DecisionPredicate {
		t.Fatalf("noul must map to predicate, got %q", req.Questions[0].Type)
	}
	if len(req.Questions[1].Choices) != 2 || req.Questions[1].Choices[1].Value != "technical" {
		t.Fatalf("criteria = %+v", req.Questions[1].Choices)
	}
}

func TestDecodeSystemOneRequest_ChoiceMinOne(t *testing.T) {
	body := `{"model":"m","state":"x","questions":{"q":{"type":"choice","criteria":{"only":"single"}}}}`
	if _, err := DecodeSystemOneRequest([]byte(body)); err != nil {
		t.Fatalf("single choice key must be accepted: %v", err)
	}
}

func TestDecodeSystemOneRequest_RejectsOpenAIType(t *testing.T) {
	body := `{"model":"m","state":"x","questions":{"q":{"type":"predicate"}}}`
	_, err := DecodeSystemOneRequest([]byte(body))
	var bad *DecisionBadRequest
	if !errors.As(err, &bad) || !strings.Contains(bad.Message, "noul") {
		t.Fatalf("err = %v, want a hint to use noul", err)
	}
}

func TestEncodeSystemOneRequest_FromDecisionsIR(t *testing.T) {
	req, err := DecodeDecisionsRequest([]byte(decisionsBody))
	if err != nil {
		t.Fatal(err)
	}
	body, err := EncodeSystemOneRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var out struct {
		State     string `json:"state"`
		Questions map[string]struct {
			Type     string          `json:"type"`
			Criteria json.RawMessage `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode encoded body: %v", err)
	}
	if out.State != "Customer wrote: I was charged twice for order 4411." {
		t.Fatalf("state = %q", out.State)
	}
	if out.Questions["is_refund"].Type != "noul" {
		t.Fatalf("predicate must become noul, got %q", out.Questions["is_refund"].Type)
	}
	var criteria []string
	if err := json.Unmarshal(out.Questions["frustration"].Criteria, &criteria); err != nil || len(criteria) != 2 {
		t.Fatalf("levels must become a criteria array: %s", out.Questions["frustration"].Criteria)
	}
}

func TestEncodeSystemOneRequest_BoolChoiceKeys(t *testing.T) {
	req := &DecisionRequest{
		Model: "m",
		State: json.RawMessage(`"x"`),
		Questions: []DecisionQuestion{{
			Name: "needs_human", Type: DecisionPick,
			Choices: []DecisionOption{{Value: "true", Description: "yes"}},
		}},
	}
	body, err := EncodeSystemOneRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"criteria":{"true":"yes"}`) {
		t.Fatalf("bool choice key not rendered: %s", body)
	}
}

func TestEncodeSystemOneRequest_RejectsSafetyID(t *testing.T) {
	req, err := DecodeDecisionsRequest([]byte(strings.Replace(decisionsBody, `"model"`, `"safety_identifier":"u1","model"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = EncodeSystemOneRequest(req)
	if !errors.Is(err, ErrNotSupportedField) || !strings.Contains(err.Error(), "safety_identifier") {
		t.Fatalf("err = %v, want ErrNotSupportedField naming safety_identifier", err)
	}
}

func TestEncodeOpenAIDecisionsRequest_RejectsAssets(t *testing.T) {
	req := &DecisionRequest{
		Model:  "m",
		State:  json.RawMessage(`"x"`),
		Assets: map[string]json.RawMessage{"img": json.RawMessage(`{}`)},
	}
	_, err := EncodeOpenAIDecisionsRequest(req)
	if !errors.Is(err, ErrNotSupportedField) || !strings.Contains(err.Error(), "assets") {
		t.Fatalf("err = %v, want ErrNotSupportedField naming assets", err)
	}
}

func TestEncodeOpenAIDecisionsRequest_FromSystemOneIR(t *testing.T) {
	req, err := DecodeSystemOneRequest([]byte(systemOneBody))
	if err != nil {
		t.Fatal(err)
	}
	body, err := EncodeOpenAIDecisionsRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var out struct {
		Input     string `json:"input"`
		Questions []struct {
			Type    string `json:"type"`
			Name    string `json:"name"`
			Choices []struct {
				Value any `json:"value"`
			} `json:"choices"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Input != "Customer wrote: I was charged twice for order 4411." {
		t.Fatalf("input = %q", out.Input)
	}
	if len(out.Questions) != 3 || out.Questions[0].Type != "predicate" || out.Questions[0].Name != "c_first" {
		t.Fatalf("questions = %+v", out.Questions)
	}
	if out.Questions[1].Choices[0].Value != "billing" {
		t.Fatalf("choice value = %v", out.Questions[1].Choices[0].Value)
	}
}

func TestParseSystemOneResponse_OutOfOrder(t *testing.T) {
	req, err := DecodeSystemOneRequest([]byte(systemOneBody))
	if err != nil {
		t.Fatal(err)
	}
	// 上游按 a/b/c 的字典序返回，与请求顺序（c/a/b）不同。
	upstream := `{"model":"jev-1.14.0","answers":{
	  "a_second":{"type":"choice","choice":"billing","probabilities":{"billing":0.9,"technical":0.1},"confidence":0.8},
	  "b_third":{"type":"score","score":1.0,"legend":{"0":"calm","1":"angry"},"probabilities":{"0":0.0,"1":1.0},"confidence":1.0},
	  "c_first":{"type":"noul","noul":0.97}},
	  "usage":{"input_tokens":412,"output_tokens":67}}`
	resp, err := ParseSystemOneResponse([]byte(upstream), req)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	names := []string{resp.Answers[0].Name, resp.Answers[1].Name, resp.Answers[2].Name}
	want := []string{"c_first", "a_second", "b_third"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("answer order = %v, want request order %v", names, want)
		}
	}
	if resp.Answers[0].Probability == nil || *resp.Answers[0].Probability != 0.97 {
		t.Fatalf("noul probability = %v", resp.Answers[0].Probability)
	}
	if resp.Answers[2].Legend[1] != "angry" {
		t.Fatalf("legend = %+v", resp.Answers[2].Legend)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 412 || resp.Usage.TotalTokens != 479 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestEncodeDecisionsResponse_Shape(t *testing.T) {
	p := 0.97
	req, err := DecodeSystemOneRequest([]byte(systemOneBody))
	if err != nil {
		t.Fatal(err)
	}
	resp := &DecisionResponse{
		Model: "jev-1.14.0",
		Answers: []DecisionAnswer{
			{Type: DecisionPredicate, Name: "c_first", Probability: &p, Noul: &p},
			{Type: DecisionPick, Name: "a_second", Choice: "billing"},
			{Type: DecisionScore, Name: "b_third", Legend: []string{"calm", "angry"}},
		},
	}
	_ = req
	body, err := EncodeDecisionsResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Answers []struct {
			Type        string   `json:"type"`
			Name        string   `json:"name"`
			Probability *float64 `json:"probability"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Answers) != 3 || out.Answers[0].Type != "predicate" || out.Answers[0].Name != "c_first" {
		t.Fatalf("answers = %+v", out.Answers)
	}
	if out.Answers[0].Probability == nil || *out.Answers[0].Probability != 0.97 {
		t.Fatalf("probability = %v", out.Answers[0].Probability)
	}
}

func TestEncodeSystemOneResponse_Shape(t *testing.T) {
	p := 0.9
	resp := &DecisionResponse{
		Model:   "gpt-6-luna",
		Answers: []DecisionAnswer{{Type: DecisionPredicate, Name: "is_refund", Probability: &p}},
	}
	body, err := EncodeSystemOneResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"noul":0.9`) {
		t.Fatalf("body = %s", body)
	}
}

func TestRoundTripAllTypes(t *testing.T) {
	req, err := DecodeDecisionsRequest([]byte(decisionsBody))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeOpenAIDecisionsRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) == 0 {
		t.Fatal("empty encoded body")
	}
	upstream := `{"model":"gpt-6-luna","answers":[
	  {"type":"predicate","name":"is_refund","probability":1.0},
	  {"type":"choice","name":"dept","choice":"billing","probabilities":[{"value":"billing","probability":0.93},{"value":"technical","probability":0.07}],"confidence":0.86},
	  {"type":"score","name":"frustration","score":0.74,"probabilities":[{"value":0,"label":"calm","probability":0.26},{"value":1,"label":"angry","probability":0.74}],"confidence":0.61},
	  {"type":"box","name":"region","box":{"top":0.1,"bottom":0.2,"left":0.3,"right":0.4},"confidence":0.8}],
	  "usage":{"input_tokens":401,"output_tokens":0,"total_tokens":401}}`
	resp, err := ParseOpenAIDecisionsResponse([]byte(upstream), req)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.Answers) != 4 {
		t.Fatalf("answers = %d", len(resp.Answers))
	}
	if resp.Answers[1].Choice != "billing" || len(resp.Answers[1].Probabilities) != 2 {
		t.Fatalf("choice answer = %+v", resp.Answers[1])
	}
	if resp.Answers[2].Score == nil || *resp.Answers[2].Score != 0.74 {
		t.Fatalf("score answer = %+v", resp.Answers[2])
	}
	if resp.Answers[3].Box == nil || resp.Answers[3].Box.Right != 0.4 {
		t.Fatalf("box answer = %+v", resp.Answers[3])
	}
}

func TestDecodeDecisionsRequest_MessageInput(t *testing.T) {
	body := `{"model":"m","questions":[{"type":"predicate","instructions":"?"}],
	  "input":[{"type":"message","role":"system","content":[{"type":"input_text","text":"be terse"}]},
	           {"type":"message","role":"user","content":[{"type":"input_text","text":"hello"},{"type":"input_image","image_url":"data:image/png;base64,AAA"}]}]}`
	req, err := DecodeDecisionsRequest([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.Messages) != 2 || req.Messages[1].Text != "hello" {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if len(req.Messages[1].Images) != 1 || req.Messages[1].Images[0] != "data:image/png;base64,AAA" {
		t.Fatalf("images = %+v", req.Messages[1].Images)
	}
	// 拍平进 System One 时，两条消息以空行分隔。
	encoded, err := EncodeSystemOneRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"state":"be terse\n\nhello"`) {
		t.Fatalf("state = %s", encoded)
	}
}

func TestDecodeDecisionsRequest_RejectsAssistantRole(t *testing.T) {
	body := `{"model":"m","questions":[{"type":"predicate","instructions":"?"}],
	  "input":[{"type":"message","role":"assistant","content":"earlier turn"}]}`
	_, err := DecodeDecisionsRequest([]byte(body))
	if !errors.Is(err, ErrNotSupportedField) {
		t.Fatalf("err = %v, want ErrNotSupportedField", err)
	}
}
