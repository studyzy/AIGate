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

package proxy

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"aigate/internal/model"
)

// 本文件是直通 SSE 扫描器（responsesPassthroughStream）的白盒用例。
// 黑盒路径无法可靠制造跨 Read 的边界（httptest chunked 编码会合并碎片），
// 而「跨边界的行缓冲」正是扫描器最容易写错的地方，故直接驱动 Write 验证。

// syncFlusher 是最小 http.Flusher 实现。
type syncFlusher struct{}

func (syncFlusher) Flush() {}

// newPassthroughStream 构造一个写入 httptest.ResponseRecorder 的扫描器。
func newPassthroughStream(gatewayID string) (*responsesPassthroughStream, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	return &responsesPassthroughStream{
		w:         rec,
		flusher:   syncFlusher{},
		gatewayID: gatewayID,
	}, rec
}

// terminalDataLine 取出流中 response.completed 事件块的 data 行。
func terminalDataLine(t *testing.T, stream string) string {
	t.Helper()
	for _, block := range strings.Split(stream, "\n\n") {
		if !strings.HasPrefix(block, "event: response.completed") {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data:") {
				return line
			}
		}
	}
	t.Fatalf("no terminal data line in stream:\n%s", stream)
	return ""
}

// TestResponsesPassthroughStream_FragmentedWrites：同一事件被拆成多个 Write 时，
// 行与事件的边界必须靠缓冲还原，终态 data 行的 id 仍要改写、usage 仍要抓到。
func TestResponsesPassthroughStream_FragmentedWrites(t *testing.T) {
	fragments := []string{
		"event: response.cre",
		"ated\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_up\"}}\n\n",
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"respo",
		"nse\":{\"id\":\"resp_up\",\"status\":\"completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3,",
		"\"total_tokens\":5}}}\n\n",
	}

	s, rec := newPassthroughStream("resp_gateway")
	for _, fragment := range fragments {
		if _, err := s.Write([]byte(fragment)); err != nil {
			t.Fatalf("write fragment: %v", err)
		}
	}
	s.close()

	out := rec.Body.String()
	if !strings.Contains(out, "event: response.created\n") || !strings.Contains(out, "event: response.completed\n") {
		t.Fatalf("event: lines must be passed through verbatim, got:\n%s", out)
	}
	// 只看终态事件的 data 行：response.created 里的上游 id 属正常透传，不在改写范围。
	terminalData := terminalDataLine(t, out)
	if strings.Contains(terminalData, `"id":"resp_up"`) {
		t.Fatalf("terminal event id must be rewritten to the gateway id, got:\n%s", terminalData)
	}
	if !strings.Contains(terminalData, `"id":"resp_gateway"`) {
		t.Fatalf("terminal event should carry the gateway id, got:\n%s", terminalData)
	}
	if s.usage == nil || s.usage.PromptTokens != 2 || s.usage.CompletionTokens != 3 || s.usage.TotalTokens != 5 {
		t.Fatalf("usage = %+v, want prompt=2 completion=3 total=5", s.usage)
	}
}

// TestResponsesPassthroughStream_LastLineWithoutNewline：上游末行不带换行时，
// close() 仍要把它写出去（否则终态事件整体丢失）。
func TestResponsesPassthroughStream_LastLineWithoutNewline(t *testing.T) {
	s, rec := newPassthroughStream("resp_gateway")
	_, _ = s.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\"," +
		"\"response\":{\"id\":\"resp_up\"}}"))
	if strings.Contains(rec.Body.String(), "resp_gateway") {
		t.Fatalf("terminal line must not be emitted before close(), got %q", rec.Body.String())
	}
	s.close()

	out := rec.Body.String()
	if !strings.Contains(out, `"id":"resp_gateway"`) {
		t.Fatalf("close() must flush the pending last line, got %q", out)
	}
}

// TestResponsesPassthroughStream_RewritesModelToRequestedName：终态事件里的 model
// 回显为客户端请求的模型名（上游收到的是绑定的原始 ID）。
func TestResponsesPassthroughStream_RewritesModelToRequestedName(t *testing.T) {
	s, rec := newPassthroughStream("resp_gateway")
	s.requestedName = "client-model"
	_, _ = s.Write([]byte("event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_up\",\"model\":\"upstream-id\",\"status\":\"completed\"}}\n\n"))

	if !strings.Contains(rec.Body.String(), `"model":"client-model"`) {
		t.Fatalf("terminal event model should echo the requested name, got %q", rec.Body.String())
	}
}

// TestResponsesPassthroughStream_LeavesNonTerminalEventsAlone：非终态事件的 data 行必须逐字节透传。
func TestResponsesPassthroughStream_LeavesNonTerminalEventsAlone(t *testing.T) {
	const delta = `data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"hi","pad":"   "}`
	s, rec := newPassthroughStream("resp_gateway")
	_, _ = s.Write([]byte("event: response.output_text.delta\n" + delta + "\n\n"))
	s.close()

	if got := rec.Body.String(); got != "event: response.output_text.delta\n"+delta+"\n\n" {
		t.Fatalf("non-terminal event must be passed through verbatim:\n got %q\nwant %q",
			got, "event: response.output_text.delta\n"+delta+"\n\n")
	}
	if s.usage != nil {
		t.Fatalf("usage should stay nil when no terminal event carries it, got %+v", s.usage)
	}
}

// TestNewResponsesID_Shape：ID 必须是 resp_ + 32 位十六进制，且每次不同。
func TestNewResponsesID_Shape(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 32; i++ {
		id := newResponsesID()
		if len(id) != len(responsesIDPrefix)+32 {
			t.Fatalf("id = %q, want %d chars", id, len(responsesIDPrefix)+32)
		}
		if !strings.HasPrefix(id, responsesIDPrefix) {
			t.Fatalf("id = %q, want %q prefix", id, responsesIDPrefix)
		}
		if _, err := hex.DecodeString(id[len(responsesIDPrefix):]); err != nil {
			t.Fatalf("id = %q: suffix is not hex: %v", id, err)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id generated: %q", id)
		}
		seen[id] = struct{}{}
	}
}

// TestNewResponsesID_ErrorReturnsEmpty：熵源失败时必须返回空串，交给调用方按 500 处理；
// 绝不能返回裸的 "resp_"（那会成为一个看起来合法、实则可能重复的 ID）。
func TestNewResponsesID_ErrorReturnsEmpty(t *testing.T) {
	restore := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("entropy unavailable") }
	t.Cleanup(func() { randRead = restore })

	if id := newResponsesID(); id != "" {
		t.Fatalf("id on entropy failure = %q, want empty string", id)
	}
}

// TestResponsesConversionMessage_Sentinels：入站错误映射必须基于哨兵而非字符串比对。
// 用 fmt.Errorf 包一层哨兵，模拟「转换器改文案但保留哨兵」的情形——映射仍要正确。
func TestResponsesConversionMessage_Sentinels(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("wrapped: %w", errResponsesInvalidInput), msgResponsesInvalidInput},
		{fmt.Errorf("wrapped: %w", errResponsesInvalidTools), msgResponsesInvalidTools},
		{fmt.Errorf("wrapped: %w", errResponsesMissingModel), msgModelRequired},
		{errResponsesInvalidBody, msgInvalidJSON},
		{errors.New("something else"), msgInvalidJSON},
	}
	for _, c := range cases {
		if got := responsesConversionMessage(c.err); got != c.want {
			t.Fatalf("responsesConversionMessage(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// TestPrepareResponsesTokens_OnlyHandlesResponsesShape：钉住 §5.4.1 的机制前提——
// prepareResponsesTokens 只对 **Responses 形态**（带 max_output_tokens、无 messages）生效；
// 传入已转换的 chat 形态时它必须什么都不做。
//
// 为什么必须钉住这条：如果它悄悄「兼容」了 chat 形态，转换路径的 chat 压制（第二道）
// 就会变成冗余，而第 4 步是否真的执行则永远观测不到——顺序契约重新失去判别力。
// 反向验证：把 max_output_tokens 当 chat 形态也压制，本用例即变红。
func TestPrepareResponsesTokens_OnlyHandlesResponsesShape(t *testing.T) {
	const limit = 16

	t.Run("responses shape is capped", func(t *testing.T) {
		body := map[string]any{"model": "m", "input": "hi", "max_output_tokens": float64(9000)}
		prepareResponsesTokens(&model.APIKey{MaxTokens: limit}, body)
		if got := body["max_output_tokens"]; got != float64(limit) {
			t.Fatalf("max_output_tokens = %v, want %d", got, limit)
		}
	})

	t.Run("chat shape is left untouched by this step", func(t *testing.T) {
		// 已转换的 chat 形态：带 messages。max_output_tokens 不是 chat 协议字段，
		// 第 4 步不该碰它（chat 的上限由转换路径的 enforceMaxTokens 经 max_tokens 施加）。
		body := map[string]any{
			"model":    "m",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}
		prepareResponsesTokens(&model.APIKey{MaxTokens: limit}, body)
		if _, injected := body["max_output_tokens"]; injected {
			t.Fatalf("chat shape must not gain max_output_tokens, got %v", body)
		}
		if _, injected := body["max_tokens"]; injected {
			t.Fatalf("this step must not inject chat max_tokens, got %v", body)
		}
	})
}
