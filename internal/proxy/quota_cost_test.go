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
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"aigate/internal/model"
)

// quotaErrorDetail 是 429 响应里 error 对象的扩展视图：除统一错误体的三键外，
// 金额配额还要带出计量单位（unit）与结算币种（currency）。
type quotaErrorDetail struct {
	Message  string `json:"message"`
	Type     string `json:"type"`
	Code     int    `json:"code"`
	Unit     string `json:"unit"`
	Currency string `json:"currency"`
}

// assertQuotaExceededUnit 断言 429 响应体带出正确的计量单位与币种。
func assertQuotaExceededUnit(
	t *testing.T,
	resp *http.Response,
	wantUnit, wantCurrency string,
) {
	t.Helper()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body=%s)", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	var got struct {
		Error quotaErrorDetail `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	if got.Error.Unit != wantUnit {
		t.Fatalf("error.unit = %q, want %q (body=%s)", got.Error.Unit, wantUnit, body)
	}
	if got.Error.Currency != wantCurrency {
		t.Fatalf("error.currency = %q, want %q (body=%s)", got.Error.Currency, wantCurrency, body)
	}
}

// TestProxy_CostQuotaExceededReturnsUnitAndCurrency 覆盖金额配额的 429 响应：
// 密钥规则 unit=cost、限额 1（结算币种整单位），窗口内已有 2 元用量 → 拒绝并带
// unit=cost 与平台结算币种（默认 CNY）。
func TestProxy_CostQuotaExceededReturnsUnitAndCurrency(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	key, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	if _, err := e.store.UpdateAPIKey(ctx(), key.ID, map[string]any{
		"token_quota_rules": `[{"mode":"day","limit":1,"unit":"cost"}]`,
	}); err != nil {
		t.Fatalf("update key rules: %v", err)
	}
	// 2 元 = 2_000_000 微元，超过 1 元的日限额。
	if err := e.store.InsertUsageLog(ctx(), &model.LLMUsageLog{
		APIKeyID:           key.ID,
		UserID:             e.user.ID,
		TotalTokens:        10,
		CostPlatformMicros: 2_000_000,
		CreatedAt:          time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert usage: %v", err)
	}

	resp := e.postChat(t, token, reqWithModel("mock-a"), nil)
	assertQuotaExceededUnit(t, resp, "cost", model.DefaultPlatformCurrency)
	if got := e.rec.Calls(); got != 0 {
		t.Fatalf("quota-exceeded request must not reach upstream, got %d calls", got)
	}
}

// TestProxy_TokenQuotaReportsTokenUnit 覆盖旧规则的兼容性：规则 JSON 无 unit 时
// 行为与升级前一致，但 429 响应仍须明确告诉调用方计量单位是 token（币种为空）。
func TestProxy_TokenQuotaReportsTokenUnit(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	key, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	if _, err := e.store.UpdateAPIKey(ctx(), key.ID, map[string]any{
		"token_quota_rules": `[{"mode":"day","limit":1}]`,
	}); err != nil {
		t.Fatalf("update key rules: %v", err)
	}
	if err := e.store.InsertUsageLog(ctx(), &model.LLMUsageLog{
		APIKeyID:    key.ID,
		UserID:      e.user.ID,
		TotalTokens: 5,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert usage: %v", err)
	}

	resp := e.postChat(t, token, reqWithModel("mock-a"), nil)
	assertQuotaExceededUnit(t, resp, "token", "")
	if got := e.rec.Calls(); got != 0 {
		t.Fatalf("quota-exceeded request must not reach upstream, got %d calls", got)
	}
}
