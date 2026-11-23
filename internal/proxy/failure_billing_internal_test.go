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
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"aigate/internal/model"
	"aigate/internal/provider"
)

// 本文件是「记账完整性」漏记路径 2 的白盒用例：
// /v1/chat/completions 非流式分支在 provider 返回 error 时，若 result 非空
// （上游已回包，状态码与 usage 都在），仍须记一笔账。
//
// 为什么白盒：「已回包却返回 error」在 providers 中只出现在响应体读取中断
// （OpenAIProvider.ChatCompletion 的 ReadUpstreamBody 分支），要求上游声明
// Content-Length 后提前断开——本仓库尚无该夹具，且 providers 按模型类型从全局注册表
// 取到无状态单例，无法只替换其中一种实现制造该错误。
// 因此这里以「上游 500 + usage」驱动真实的数据面入口（handleChatCompletions），
// 覆盖从鉴权到记账的完整链路；流式中断路径由黑盒用例覆盖（failure_billing_test.go）。

// TestProxy_UpstreamErrorWithUsageIsBilled 覆盖非流式上游错误分支的记账：
// 上游已回包（声明长度后截断，provider 返回 error 且 result 为 nil）时，
// 调用方按既有语义拿到 502，但这次请求仍须落库一条记录（token 已不可得，金额为 0），
// 状态码记为上游状态码——「出错不等于没有成本」，不能连行都没有。
func TestProxy_UpstreamErrorWithUsageIsBilled(t *testing.T) {
	// 上游落在 127.0.0.1，须先放行私网/回环（与黑盒夹具同口径），用例结束还原严格模式。
	provider.SetAllowPrivateUpstreams(true)
	t.Cleanup(func() { provider.SetAllowPrivateUpstreams(false) })

	const upstreamBody = `{"error":{"message":"upstream exploded","type":"server_error"},` +
		`"usage":{"prompt_tokens":1000,"completion_tokens":2000,"total_tokens":3000}}`
	// 声明完整长度后只写一半并断开：provider 读取响应体时拿到 unexpected EOF，
	// 于是在已回包（状态码 + 部分响应体）的情况下返回 error——正是本分支的真实触发条件。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(upstreamBody)))
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, upstreamBody[:len(upstreamBody)/2])
		w.(http.Flusher).Flush()
		if hijacker, ok := w.(http.Hijacker); ok {
			if conn, _, err := hijacker.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	}))
	defer upstream.Close()

	store, err := model.Open(model.DBTypeSQLite, filepath.Join(t.TempDir(), "billing-internal.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.AutoMigrate(); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	aiModel := &model.AIModel{
		Provider:  "mock-provider",
		ModelID:   "mock-a",
		ModelName: "name-mock-a",
		URL:       upstream.URL,
		APIKey:    "upstream-secret",
		ModelType: model.ModelTypeOpenAI,
		QuotaDay:  -1,
		Enabled:   true,
	}
	if err := store.CreateAIModel(ctx, aiModel); err != nil {
		t.Fatalf("create ai model: %v", err)
	}
	plain, hash, prefix, err := model.GenerateToken(model.TokenPrefixKey)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	owner := &model.User{Username: "tester"}
	if err := store.CreateUser(ctx, owner); err != nil {
		t.Fatalf("create user: %v", err)
	}
	key := &model.APIKey{
		Name:             "test-key",
		UserID:           owner.ID,
		ProxyTokenHash:   hash,
		ProxyTokenPrefix: prefix,
		Enabled:          true,
	}
	bindings := []model.APIKeyModel{{AIModelID: aiModel.ID, Role: model.ModelRolePrimary, SortOrder: 0}}
	if err := store.CreateAPIKey(ctx, key, bindings); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	usage := NewUsageWriter(store, 4, nil)
	usage.Start()
	h := &Handler{store: store, usage: usage, quotaExceededStatus: http.StatusTooManyRequests}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock-a","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+plain)
	req.Header.Set("Content-Type", "application/json")
	h.handleChatCompletions(rec, req)
	usage.Stop(context.Background())

	// 调用方仍按既有语义拿到 502（上游错误细节不外泄）。
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("client status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}

	logs, _, err := store.ListUsageLogs(ctx, model.UsageFilter{}, 1, 10)
	if err != nil {
		t.Fatalf("list usage logs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("usage rows = %d, want 1 (upstream error with usage must be billed)", len(logs))
	}
	row := logs[0]
	if row.APIKeyID != key.ID || row.AIModelID != aiModel.ID {
		t.Fatalf("row identity = %+v, want key=%d model=%d", row, key.ID, aiModel.ID)
	}
	if row.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status_code = %d, want upstream status 500", row.StatusCode)
	}
	// 响应体被截断，usage 未读完：token 与金额只能为 0，但行必须在。
	if row.PromptTokens != 0 || row.CompletionTokens != 0 || row.TotalTokens != 0 {
		t.Fatalf("tokens = %+v, want 0 (usage could not be read)", row)
	}
	if row.CostVendorMicros != 0 || row.CostPlatformMicros != 0 {
		t.Fatalf("costs = %+v, want 0", row)
	}
}
