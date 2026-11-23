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

// 上游 mock：在测试进程内起 httptest 服务模拟 OpenAI 与 Anthropic 两种上游协议，
// 记录收到的请求体以便断言网关发出的内容；固定响应体也放在这里。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

const openAINonStreamBody = `{"id":"chatcmpl-mock","object":"chat.completion","choices":[{"index":0,` +
	`"message":{"role":"assistant","content":"hello from mock"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,` +
	`"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3}}}`

const anthropicNonStreamBody = `{"id":"msg_mock","type":"message","role":"assistant","content":[{"type":"text",` +
	`"text":"hello from anthropic"}],"model":"mock","stop_reason":"end_turn","usage":{"input_tokens":9,` +
	`"output_tokens":5}}`

// openAIStreamChunks 是 OpenAI 型 SSE 的 3 个 data 分片：2 个 content + 1 个带 usage。
var openAIStreamChunks = []string{
	`{"id":"chatcmpl-mock","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant",` +
		`"content":"hello "},"finish_reason":null}]}`,
	`{"id":"chatcmpl-mock","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"from mock"},` +
		`"finish_reason":null}]}`,
	`{"id":"chatcmpl-mock","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3}}}`,
}

// anthropicSSEEvents 是 Anthropic 型 SSE 事件序列（event + data 成对出现）。
var anthropicSSEEvents = [][2]string{
	{
		"message_start",
		`{"type":"message_start","message":{"id":"msg_mock","type":"message","role":"assistant","model":"mock",` +
			`"content":[],"stop_reason":null,"usage":{"input_tokens":9,"output_tokens":0}}}`,
	},
	{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
	{
		"content_block_delta",
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello from anthropic"}}`,
	},
	{"content_block_stop", `{"type":"content_block_stop","index":0}`},
	{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`},
	{"message_stop", `{"type":"message_stop"}`},
}

type upstreamKind string

const (
	kindOpenAI    upstreamKind = "openai"
	kindAnthropic upstreamKind = "anthropic"
)

// recordedRequest 是 mock 上游最后一次收到的请求快照。
type recordedRequest struct {
	Method  string
	Path    string
	Body    []byte
	Headers http.Header
}

// upstreamMock 是进程内的假上游：按 kind 提供 OpenAI 或 Anthropic 路由，
// 并以互斥保护记录最后一次收到的请求，供用例断言 model 名与请求体格式。
type upstreamMock struct {
	server *httptest.Server
	kind   upstreamKind

	mu   sync.Mutex
	last recordedRequest
}

func newUpstreamMock(kind upstreamKind) *upstreamMock {
	m := &upstreamMock{kind: kind}
	mux := http.NewServeMux()
	switch kind {
	case kindOpenAI:
		mux.HandleFunc("/v1/chat/completions", m.handleOpenAI)
	case kindAnthropic:
		mux.HandleFunc("/v1/messages", m.handleAnthropic)
	}
	m.server = httptest.NewServer(mux)
	return m
}

func (m *upstreamMock) URL() string { return m.server.URL }

func (m *upstreamMock) Close() { m.server.Close() }

func (m *upstreamMock) record(r *http.Request, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last = recordedRequest{Method: r.Method, Path: r.URL.Path, Body: body, Headers: r.Header.Clone()}
}

func (m *upstreamMock) lastRequest() recordedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

func (m *upstreamMock) handleOpenAI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	m.record(r, body)

	var payload struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &payload)

	if payload.Stream {
		writeSSEHeaders(w)
		flusher, _ := w.(http.Flusher)
		for _, chunk := range openAIStreamChunks {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			flush(flusher)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flush(flusher)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, openAINonStreamBody)
}

func (m *upstreamMock) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	m.record(r, body)

	var payload struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &payload)

	if payload.Stream {
		writeSSEHeaders(w)
		flusher, _ := w.(http.Flusher)
		for _, ev := range anthropicSSEEvents {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev[0], ev[1])
			flush(flusher)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, anthropicNonStreamBody)
}

func writeSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
}

func flush(f http.Flusher) {
	if f != nil {
		f.Flush()
	}
}
