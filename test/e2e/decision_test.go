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

//go:build e2e

package e2e

// 决策模型端到端：真实二进制 + 真实数据库 + mock 决策上游，覆盖
//   1. 两条入口 × 两种上游协议的四个组合（同协议直通与跨协议翻译）；
//   2. 无 /v1 别名；
//   3. 决策入口拒绝流式与 chat 专有字段；
//   4. chat 入口拒绝决策模型；
//   5. input_only 计费落库（completion=0、total=prompt），且响应体保留上游 usage。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	decisionDecisionsModelID = "e2e-decision-decisions"
	decisionSystemOneModelID = "e2e-decision-systemone"

	decisionDecisionsReply = `{"model":"gpt-6-luna","answers":[` +
		`{"type":"predicate","name":"is_refund","probability":1.0},` +
		`{"type":"choice","name":"dept","choice":"billing",` +
		`"probabilities":[{"value":"billing","probability":0.93},{"value":"technical","probability":0.07}],` +
		`"confidence":0.86}],"usage":{"input_tokens":401,"output_tokens":0,"total_tokens":401}}`

	decisionSystemOneReply = `{"model":"jev-1.14.0","answers":{` +
		`"is_refund":{"type":"noul","noul":1.0},` +
		`"dept":{"type":"choice","choice":"billing",` +
		`"probabilities":{"billing":0.93,"technical":0.07},"confidence":0.86}},` +
		`"usage":{"input_tokens":412,"output_tokens":67}}`
)

// decisionUpstreamMock 是按路径区分协议的假决策上游：/v1/decisions 回 OpenAI 形状，
// /v1/systemone 回 System One 形状；打到别的路径直接 404，让「发错协议」的 bug 立刻可见。
type decisionUpstreamMock struct {
	server *httptest.Server
	mu     chan struct{} // 容量 1 的互斥：记录最后一次请求
	last   []byte
	path   string
}

func newDecisionUpstreamMock() *decisionUpstreamMock {
	m := &decisionUpstreamMock{mu: make(chan struct{}, 1)}
	m.mu <- struct{}{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 0, 512)
		buf := bytes.NewBuffer(body)
		_, _ = buf.ReadFrom(r.Body)
		<-m.mu
		m.last = buf.Bytes()
		m.path = r.URL.Path
		m.mu <- struct{}{}

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/decisions":
			_, _ = w.Write([]byte(decisionDecisionsReply))
		case "/v1/systemone":
			_, _ = w.Write([]byte(decisionSystemOneReply))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"error":{"message":"unexpected path %s"}}`, r.URL.Path)
		}
	}))
	return m
}

func (m *decisionUpstreamMock) URL() string { return m.server.URL }

func (m *decisionUpstreamMock) Close() { m.server.Close() }

func (m *decisionUpstreamMock) lastRequest() (path string, body []byte) {
	<-m.mu
	path, body = m.path, append([]byte(nil), m.last...)
	m.mu <- struct{}{}
	return path, body
}

// postDecision 向数据面发一次决策请求，返回状态码与响应体。
func postDecision(t *testing.T, baseURL, path, apiKey string, payload any) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(mustJSON(t, payload)))
	if err != nil {
		t.Fatalf("failed to build the %s request: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := e2eHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("request %s failed: %v", path, err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func TestE2E_DecisionModels_BothIngests(t *testing.T) {
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	upstream := newDecisionUpstreamMock()
	t.Cleanup(upstream.Close)

	gw := startGateway(t, bin, dsn)
	adminAuth := adminToken

	var (
		decisionsKey string
		systemOneKey string
	)

	t.Run("1. create one model per upstream protocol, one key each", func(t *testing.T) {
		// 密钥必须有属主用户；各用例共用同一个 e2e 库，用户按名区分互不干扰。
		owner := createUser(t, gw.baseURL, "e2e-decision-owner", 0)
		decisionsModelRowID := createModel(t, gw.baseURL, adminAuth, map[string]any{
			"provider":   "mock",
			"model_id":   decisionDecisionsModelID,
			"model_name": decisionDecisionsModelID,
			"url":        upstream.URL() + "/v1",
			"api_key":    upstreamAPIKey,
			"model_type": "openai-decisions",
			"quota_day":  -1,
			"enabled":    true,
		})
		systemOneModelRowID := createModel(t, gw.baseURL, adminAuth, map[string]any{
			"provider":   "mock",
			"model_id":   decisionSystemOneModelID,
			"model_name": decisionSystemOneModelID,
			"url":        upstream.URL() + "/v1",
			"api_key":    upstreamAPIKey,
			"model_type": "systemone",
			"quota_day":  -1,
			"enabled":    true,
		})

		decisionsKey = createKey(t, gw.baseURL, map[string]any{
			"name":     "e2e-decision-decisions-key",
			"user_id":  owner.ID,
			"bindings": []map[string]any{{"ai_model_id": decisionsModelRowID, "role": "primary"}},
		}).Token
		systemOneKey = createKey(t, gw.baseURL, map[string]any{
			"name":     "e2e-decision-systemone-key",
			"user_id":  owner.ID,
			"bindings": []map[string]any{{"ai_model_id": systemOneModelRowID, "role": "primary"}},
		}).Token
	})

	// 每个组合一个子用例：入口协议由请求路径决定，期望的响应形状与上游收到的协议也一并断言。
	t.Run("2. four ingress x upstream combinations", func(t *testing.T) {
		cases := []struct {
			name           string
			path           string
			key            string
			payload        map[string]any
			wantUpstream   string
			wantAnswersKey string
		}{
			{
				name: "decisions ingress to decisions upstream", path: "/v1/decisions", key: decisionsKey,
				wantUpstream: "/v1/decisions", wantAnswersKey: "",
				payload: map[string]any{
					"model": decisionDecisionsModelID, "input": "ticket text",
					"questions": []map[string]any{
						{"type": "predicate", "name": "is_refund", "instructions": "refund?"},
						{"type": "choice", "name": "dept", "instructions": "team?",
							"choices": []map[string]any{{"value": "billing"}, {"value": "technical"}}},
					},
				},
			},
			{
				name: "decisions ingress to systemone upstream", path: "/v1/decisions", key: systemOneKey,
				wantUpstream: "/v1/systemone", wantAnswersKey: "",
				payload: map[string]any{
					"model": decisionSystemOneModelID, "input": "ticket text",
					"questions": []map[string]any{
						{"type": "predicate", "name": "is_refund", "instructions": "refund?"},
						{"type": "choice", "name": "dept", "instructions": "team?",
							"choices": []map[string]any{{"value": "billing"}, {"value": "technical"}}},
					},
				},
			},
			{
				name: "systemone ingress to systemone upstream", path: "/v1/systemone", key: systemOneKey,
				wantUpstream: "/v1/systemone", wantAnswersKey: "is_refund",
				payload: map[string]any{
					"model": decisionSystemOneModelID, "state": "ticket text",
					"questions": map[string]any{
						"is_refund": map[string]any{"type": "noul", "instructions": "refund?"},
						"dept": map[string]any{"type": "choice", "instructions": "team?",
							"criteria": map[string]string{"billing": "", "technical": ""}},
					},
				},
			},
			{
				name: "systemone ingress to decisions upstream", path: "/v1/systemone", key: decisionsKey,
				wantUpstream: "/v1/decisions", wantAnswersKey: "is_refund",
				payload: map[string]any{
					"model": decisionDecisionsModelID, "state": "ticket text",
					"questions": map[string]any{
						"is_refund": map[string]any{"type": "noul", "instructions": "refund?"},
						"dept": map[string]any{"type": "choice", "instructions": "team?",
							"criteria": map[string]string{"billing": "", "technical": ""}},
					},
				},
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				status, body := postDecision(t, gw.baseURL, tc.path, tc.key, tc.payload)
				if status != http.StatusOK {
					t.Fatalf("status = %d, body = %s", status, body)
				}

				var got struct {
					Answers json.RawMessage `json:"answers"`
				}
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatalf("failed to decode the decision response: %v, body = %s", err, body)
				}
				if tc.wantAnswersKey == "" {
					var arr []map[string]any
					if err := json.Unmarshal(got.Answers, &arr); err != nil {
						t.Fatalf("decisions ingress must answer with an array, got %s", got.Answers)
					}
					if len(arr) != 2 || arr[0]["name"] != "is_refund" {
						t.Fatalf("answers = %s", got.Answers)
					}
				} else {
					var obj map[string]any
					if err := json.Unmarshal(got.Answers, &obj); err != nil {
						t.Fatalf("systemone ingress must answer with an object, got %s", got.Answers)
					}
					if _, ok := obj[tc.wantAnswersKey]; !ok {
						t.Fatalf("answer %q missing in %s", tc.wantAnswersKey, got.Answers)
					}
				}

				upstreamPath, upstreamBody := upstream.lastRequest()
				if upstreamPath != tc.wantUpstream {
					t.Fatalf("upstream path = %q, want %q", upstreamPath, tc.wantUpstream)
				}
				// 上游必须收到本协议的线上形状，而不是入口原样 body。
				var sent map[string]any
				if err := json.Unmarshal(upstreamBody, &sent); err != nil {
					t.Fatalf("upstream body is not JSON: %v", err)
				}
				if tc.wantUpstream == "/v1/systemone" {
					if _, ok := sent["state"]; !ok {
						t.Fatalf("systemone upstream must receive state, got %s", upstreamBody)
					}
					if _, ok := sent["questions"].(map[string]any); !ok {
						t.Fatalf("systemone upstream questions must be an object: %s", upstreamBody)
					}
				} else {
					if _, ok := sent["input"]; !ok {
						t.Fatalf("decisions upstream must receive input, got %s", upstreamBody)
					}
					if _, ok := sent["questions"].([]any); !ok {
						t.Fatalf("decisions upstream questions must be an array: %s", upstreamBody)
					}
				}
			})
		}
	})

	t.Run("3. /v1-less aliases behave identically", func(t *testing.T) {
		payload := map[string]any{
			"model": decisionSystemOneModelID, "state": "alias",
			"questions": map[string]any{"is_refund": map[string]any{"type": "noul", "instructions": "refund?"}},
		}
		versionedStatus, versionedBody := postDecision(t, gw.baseURL, "/v1/systemone", systemOneKey, payload)
		aliasStatus, aliasBody := postDecision(t, gw.baseURL, "/systemone", systemOneKey, payload)
		if versionedStatus != http.StatusOK || aliasStatus != http.StatusOK {
			t.Fatalf("status = %d / %d, bodies = %s / %s", versionedStatus, aliasStatus, versionedBody, aliasBody)
		}

		decisionsPayload := map[string]any{
			"model": decisionDecisionsModelID, "input": "alias",
			"questions": []map[string]any{{"type": "predicate", "name": "is_refund", "instructions": "refund?"}},
		}
		if status, _ := postDecision(t, gw.baseURL, "/decisions", decisionsKey, decisionsPayload); status != http.StatusOK {
			t.Fatalf("/decisions alias status = %d", status)
		}
	})

	t.Run("4. decision ingresses reject streaming and chat-only fields", func(t *testing.T) {
		payload := map[string]any{
			"model": decisionSystemOneModelID, "state": "x", "stream": true,
			"questions": map[string]any{"is_refund": map[string]any{"type": "noul", "instructions": "?"}},
		}
		if status, body := postDecision(t, gw.baseURL, "/v1/systemone", systemOneKey, payload); status != http.StatusBadRequest ||
			!strings.Contains(string(body), "streaming is not supported") {
			t.Fatalf("stream:true must be 400, got %d %s", status, body)
		}

		payload = map[string]any{
			"model": decisionSystemOneModelID, "state": "x", "temperature": 0.2,
			"questions": map[string]any{"is_refund": map[string]any{"type": "noul", "instructions": "?"}},
		}
		if status, body := postDecision(t, gw.baseURL, "/v1/systemone", systemOneKey, payload); status != http.StatusBadRequest ||
			!strings.Contains(string(body), "temperature") {
			t.Fatalf("chat-only field must be 400 and name the field, got %d %s", status, body)
		}
	})

	t.Run("5. chat ingress rejects decision models", func(t *testing.T) {
		status, body := postChatCompletion(t, gw.baseURL, systemOneKey, map[string]any{
			"model":    decisionSystemOneModelID,
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusBadRequest || !strings.Contains(string(body), "does not support chat completions") {
			t.Fatalf("chat ingress must reject decision models, got %d %s", status, body)
		}
	})

	t.Run("6. input_only billing records completion=0", func(t *testing.T) {
		// System One 上游返回 output_tokens=67（免费但非零）；input_only 下必须记 0。
		waitFor(t, "decision usage row to land", 10*time.Second, func() error {
			db := openE2EDB(t, dsn)
			var prompt, completion, total int
			err := db.QueryRow(
				"SELECT prompt_tokens, completion_tokens, total_tokens FROM llm_usage_logs "+
					"WHERE model = ? ORDER BY id DESC LIMIT 1", decisionSystemOneModelID).
				Scan(&prompt, &completion, &total)
			if err != nil {
				return err
			}
			if prompt != 412 || completion != 0 || total != 412 {
				return fmt.Errorf("usage row = prompt %d completion %d total %d, want 412/0/412",
					prompt, completion, total)
			}
			return nil
		})
	})
}
