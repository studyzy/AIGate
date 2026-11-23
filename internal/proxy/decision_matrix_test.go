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

package proxy_test

// 本文件钉住决策模型的两格矩阵：2 个入口 × 2 种上游协议。
//
//   | 入口           | 上游 model_type    | 期望                                      |
//   |----------------|--------------------|-------------------------------------------|
//   | /v1/decisions  | openai-decisions   | 同协议，上游收到 input + questions 数组   |
//   | /v1/decisions  | systemone          | 跨协议，上游收到 state + questions 对象   |
//   | /v1/systemone  | systemone          | 同协议，上游收到 state + questions 对象   |
//   | /v1/systemone  | openai-decisions   | 跨协议，上游收到 input + questions 数组   |
//
// 判别力来自三处：上游按收到的路径回本协议的响应体（打到错的路径会解析失败）；
// 断言上游请求体是上游协议的形状；断言响应是入口协议的形状（answers 数组 vs 对象）。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"aigate/internal/model"
)

const matrixDecisionsReply = `{"model":"gpt-6-luna","answers":[
  {"type":"predicate","name":"is_refund","probability":0.97},
  {"type":"choice","name":"dept","choice":"billing",
   "probabilities":[{"value":"billing","probability":0.9},{"value":"technical","probability":0.1}],"confidence":0.8},
  {"type":"score","name":"frustration","score":0.5,
   "probabilities":[{"value":0,"label":"calm","probability":0.5},{"value":1,"label":"angry","probability":0.5}],"confidence":0.1},
  {"type":"box","name":"region","box":{"top":0.1,"bottom":0.2,"left":0.3,"right":0.4},"confidence":0.9}],
  "usage":{"input_tokens":100,"output_tokens":0,"total_tokens":100}}`

const matrixSystemOneReply = `{"model":"jev-1.14.0","answers":{
  "is_refund":{"type":"noul","noul":0.97},
  "dept":{"type":"choice","choice":"billing","probabilities":{"billing":0.9,"technical":0.1},"confidence":0.8},
  "frustration":{"type":"score","score":0.5,"legend":{"0":"calm","1":"angry"},"probabilities":{"0":0.5,"1":0.5},"confidence":0.1},
  "region":{"type":"box","box":{"top":0.1,"bottom":0.2,"left":0.3,"right":0.4},"confidence":0.9}},
  "usage":{"input_tokens":100,"output_tokens":5}}`

// decisionsIngressBody 是 OpenAI 入口的请求体（四种题都在）。
const decisionsIngressBody = `{"model":"%s","input":"ticket text","questions":[
  {"type":"predicate","name":"is_refund","instructions":"refund?"},
  {"type":"choice","name":"dept","instructions":"team?","choices":[{"value":"billing"},{"value":"technical"}]},
  {"type":"score","name":"frustration","instructions":"how angry?","levels":[{"label":"calm"},{"label":"angry"}]},
  {"type":"box","name":"region","instructions":"where?"}]}`

// systemOneIngressBody 是 System One 入口的请求体（四种题都在，key 与上面同名）。
const systemOneIngressBody = `{"model":"%s","state":"ticket text","questions":{
  "is_refund":{"type":"noul","instructions":"refund?"},
  "dept":{"type":"choice","instructions":"team?","criteria":{"billing":"","technical":""}},
  "frustration":{"type":"score","instructions":"how angry?","criteria":["calm","angry"]},
  "region":{"type":"box","instructions":"where?"}}}`

type matrixDecisionCase struct {
	name    string
	path    string
	bodyFmt string
	engine  string
	// assertUpstream 断言上游收到的请求体是本协议形状。
	assertUpstream func(t *testing.T, cell string, body map[string]any)
	// assertResponse 断言响应是入口协议形状。
	assertResponse func(t *testing.T, cell string, out map[string]any)
}

func TestDecisionMatrix_FourCells(t *testing.T) {
	// mock 上游按收到的路径回本协议的响应体：某格打到错的路径时，响应形状断言立刻变红，
	// 而不是被一段「两协议通吃」的万用 body 掩盖。
	upstream := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/decisions":
			_, _ = w.Write([]byte(matrixDecisionsReply))
		case "/v1/systemone":
			_, _ = w.Write([]byte(matrixSystemOneReply))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"unexpected path"}}`))
		}
	}

	assertDecisionsUpstream := func(t *testing.T, cell string, body map[string]any) {
		t.Helper()
		if _, ok := body["input"]; !ok {
			t.Fatalf("%s: upstream must receive input, got %v", cell, body)
		}
		questions, ok := body["questions"].([]any)
		if !ok {
			t.Fatalf("%s: upstream questions must be an array, got %T", cell, body["questions"])
		}
		types := make([]string, 0, len(questions))
		for _, q := range questions {
			m := q.(map[string]any)
			types = append(types, fmt.Sprint(m["type"]))
		}
		if strings.Join(types, ",") != "predicate,choice,score,box" {
			t.Fatalf("%s: upstream question types = %v", cell, types)
		}
	}

	assertSystemOneUpstream := func(t *testing.T, cell string, body map[string]any) {
		t.Helper()
		if _, ok := body["state"]; !ok {
			t.Fatalf("%s: upstream must receive state, got %v", cell, body)
		}
		questions, ok := body["questions"].(map[string]any)
		if !ok {
			t.Fatalf("%s: upstream questions must be an object, got %T", cell, body["questions"])
		}
		if fmt.Sprint(questions["is_refund"].(map[string]any)["type"]) != "noul" {
			t.Fatalf("%s: predicate must travel as noul, got %v", cell, questions["is_refund"])
		}
	}

	assertDecisionsResponse := func(t *testing.T, cell string, out map[string]any) {
		t.Helper()
		answers, ok := out["answers"].([]any)
		if !ok {
			t.Fatalf("%s: response answers must be an array, got %T", cell, out["answers"])
		}
		names := make([]string, 0, len(answers))
		for _, a := range answers {
			names = append(names, fmt.Sprint(a.(map[string]any)["name"]))
		}
		if strings.Join(names, ",") != "is_refund,dept,frustration,region" {
			t.Fatalf("%s: answer order = %v", cell, names)
		}
		if answers[0].(map[string]any)["probability"] != 0.97 {
			t.Fatalf("%s: predicate probability lost: %v", cell, answers[0])
		}
	}

	assertSystemOneResponse := func(t *testing.T, cell string, out map[string]any) {
		t.Helper()
		answers, ok := out["answers"].(map[string]any)
		if !ok {
			t.Fatalf("%s: response answers must be an object, got %T", cell, out["answers"])
		}
		for _, name := range []string{"is_refund", "dept", "frustration", "region"} {
			if _, ok := answers[name]; !ok {
				t.Fatalf("%s: missing answer %q in %v", cell, name, answers)
			}
		}
		if answers["is_refund"].(map[string]any)["noul"] != 0.97 {
			t.Fatalf("%s: noul probability lost: %v", cell, answers["is_refund"])
		}
	}

	cases := []struct {
		upstreamType string
		upstreamPath string
		ingress      matrixDecisionCase
	}{
		{model.ModelTypeDecisions, "/v1/decisions", matrixDecisionCase{
			name: "decisions-ingress/decisions-upstream", path: "/v1/decisions", bodyFmt: decisionsIngressBody,
			assertUpstream: assertDecisionsUpstream, assertResponse: assertDecisionsResponse}},
		{model.ModelTypeSystemOne, "/v1/systemone", matrixDecisionCase{
			name: "decisions-ingress/systemone-upstream", path: "/v1/decisions", bodyFmt: decisionsIngressBody,
			assertUpstream: assertSystemOneUpstream, assertResponse: assertDecisionsResponse}},
		{model.ModelTypeSystemOne, "/v1/systemone", matrixDecisionCase{
			name: "systemone-ingress/systemone-upstream", path: "/v1/systemone", bodyFmt: systemOneIngressBody,
			assertUpstream: assertSystemOneUpstream, assertResponse: assertSystemOneResponse}},
		{model.ModelTypeDecisions, "/v1/decisions", matrixDecisionCase{
			name: "systemone-ingress/decisions-upstream", path: "/v1/systemone", bodyFmt: systemOneIngressBody,
			assertUpstream: assertDecisionsUpstream, assertResponse: assertSystemOneResponse}},
	}

	for _, tc := range cases {
		t.Run(tc.ingress.name, func(t *testing.T) {
			e := newEnv(t, upstream)
			m := newAIModel("up-"+tc.upstreamType, e.upstream.URL+"/v1")
			m.ModelType = tc.upstreamType
			m.BillingMode = model.BillingModeInputOnly
			mustCreateAIModel(t, e.store, m)
			_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

			resp := e.postDecision(t, tc.ingress.path, token, fmt.Sprintf(tc.ingress.bodyFmt, m.ModelID))
			assertStatus(t, resp, http.StatusOK)
			var out map[string]any
			if err := json.Unmarshal([]byte(readBody(t, resp)), &out); err != nil {
				t.Fatalf("%s: decode response: %v", tc.ingress.name, err)
			}
			if _, isErr := out["error"]; isErr {
				t.Fatalf("%s: 200 must not carry an error body: %v", tc.ingress.name, out)
			}

			if got := e.rec.Path(); got != tc.upstreamPath {
				t.Fatalf("%s: upstream path = %q, want %q", tc.ingress.name, got, tc.upstreamPath)
			}
			if got := e.rec.Calls(); got != 1 {
				t.Fatalf("%s: upstream calls = %d, want 1", tc.ingress.name, got)
			}
			tc.ingress.assertUpstream(t, tc.ingress.name, e.rec.Body())
			tc.ingress.assertResponse(t, tc.ingress.name, out)
		})
	}
}
