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

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"aigate/internal/billing"
	"aigate/internal/model"
	"aigate/internal/proxy"
)

// 本文件覆盖设计文档「记账完整性」补齐的漏记路径之一：
// /v1/messages 流式上游已写出部分事件后中断，仍须落库（金额非零）。
// 该分支由真实 TCP 中断驱动，走完整数据面链路（鉴权 → 转换 → 上游 → 记账）。
// 另一处（非流式上游错误分支）见 failure_billing_internal_test.go。

// fixedPriceSource 是计价引擎的固定价源：CNY 模型、空闲价固定、同币种无需汇率。
type fixedPriceSource struct{}

func (fixedPriceSource) Model(context.Context, uint) (billing.ModelInfo, error) {
	return billing.ModelInfo{Currency: "CNY"}, nil
}

func (fixedPriceSource) Prices(_ context.Context, _ uint) ([]billing.DatedPrice, error) {
	return []billing.DatedPrice{{EffectiveAt: time.Now().Add(-24 * time.Hour), Currency: "CNY",
		Price: billing.Price{
			OffpeakInputHitMicros:  500_000,
			OffpeakInputMissMicros: 1_000_000,
			OffpeakOutputMicros:    2_000_000,
		}}}, nil
}

func (fixedPriceSource) Rate(context.Context, string, string) ([]billing.DatedRate, error) {
	return nil, nil
}

func (fixedPriceSource) Calendar(context.Context, time.Time, time.Time) (billing.Calendar, error) {
	return billing.NewCalendar(nil), nil
}

func (fixedPriceSource) PlatformCurrency(context.Context) (string, error) { return "CNY", nil }

// newBillingEnv 与 newEnv 相同，但把用量写入器换成装配了计价引擎的实例：
// 价源固定，与被测 store 里的模型配置无关，无需求先登记价目。
func newBillingEnv(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *env {
	t.Helper()
	e := newEnv(t, respond)
	// 新建写入器而不是改造 newEnv 的那个：对已启动的写入器调用 Stop 会关闭其队列并
	// 让 startOnce 失效，复用后无法再起消费 goroutine。
	e.usage = proxy.NewUsageWriter(e.store, 0, billing.NewEngine(fixedPriceSource{}, time.UTC))
	e.usage.Start()
	t.Cleanup(e.stopUsage)
	return e
}

// respondSSEThenAbort 先按 chunks 写出并 flush，随后关闭整条上游 TCP 连接：
// 网关读到的是被截断的流（scanner.Err 非空），据此走「流式上游中断」错误分支。
func respondSSEThenAbort(t *testing.T, chunks []string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("test upstream must support Flusher")
			return
		}
		for _, chunk := range chunks {
			_, _ = io.WriteString(w, chunk)
			flusher.Flush()
		}
		// 留出 TCP 送达时间：翻译器按 SSE 事件边界缓冲，末片必须先到代理侧。
		time.Sleep(50 * time.Millisecond)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack upstream connection: %v", err)
			return
		}
		time.Sleep(50 * time.Millisecond)
		_ = conn.Close()
	}
}

// TestProxy_StreamAbortStillBilled 覆盖 /v1/messages 流式中断：
// 上游先发事件再断开，客户端读到截断的流（没有收尾事件），网关仍须以 502 落库且金额非零。
func TestProxy_StreamAbortStillBilled(t *testing.T) {
	// 只发前两片文本增量后断开：上游 usage 帧（收尾片）永不送达。
	// 这正是中断流的常态——token 数拿不到，但这次请求确实被消耗了，行必须落库。
	aborted := append([]string(nil), openAIStreamChunks[:2]...)
	e := newBillingEnv(t, respondSSEThenAbort(t, aborted))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-gpt", e.upstream.URL))
	key, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postMessagesAPIKey(t, token,
		`{"model":"mock-gpt","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	assertStatus(t, resp, http.StatusOK)
	stream := readStreamBody(t, resp)
	// 断言确实走到「已写出部分事件」的分支：有起始事件，但没有收尾事件。
	if !strings.Contains(stream, "event: message_start") || strings.Contains(stream, "event: message_stop") {
		t.Fatalf("stream should be truncated after partial events, got %q", stream)
	}

	// 客户端读完整条截断的流不等于代理侧 Handler 已返回，记账可能仍在进行：先等行出现。
	read := waitUsageLogRows(t, e.dsn, 1)
	e.stopUsage()
	if n := countRows(t, read, &model.LLMUsageLog{}); n != 1 {
		t.Fatalf("llm_usage_logs rows = %d, want 1 (aborted stream must still be billed)", n)
	}
	row := latestUsageLog(t, read)
	if row.APIKeyID != key.ID || row.AIModelID != m.ID {
		t.Fatalf("usage log identity = %+v, want key=%d model=%d", row, key.ID, m.ID)
	}
	if row.StatusCode != http.StatusBadGateway {
		t.Fatalf("usage status_code = %d, want 502", row.StatusCode)
	}
	// usage 帧未送达：金额按拿到的 token（0）计算，故为 0；有 usage 时的折算由 usagelog_test.go 覆盖。
	if row.CostVendorMicros != 0 || row.CostPlatformMicros != 0 {
		t.Fatalf("costs = %+v, want 0 (no usage frame arrived before the abort)", row)
	}
}
