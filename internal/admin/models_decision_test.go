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

package admin_test

// 本文件覆盖决策模型在管理面的契约：新 model_type 的校验、billing_mode 的默认与校验、
// 跨族修改 model_type 的守卫，以及决策模型的连通性探测口径。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"aigate/internal/model"
	"aigate/internal/provider"
)

// decisionProbeUpstream 是一个回答 noul 问句的 mock 决策上游。
type decisionProbeUpstream struct {
	mu    sync.Mutex
	path  string
	calls int
}

func startDecisionProbeUpstream(t *testing.T) (*httptest.Server, *decisionProbeUpstream) {
	t.Helper()
	rec := &decisionProbeUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.path = r.URL.Path
		rec.calls++
		rec.mu.Unlock()
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w,
			`{"model":"jev-1.14.0","answers":{"reachable":{"type":"noul","noul":0.97}},`+
				`"usage":{"input_tokens":20,"output_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestCreateModel_DecisionTypeDefaultsInputOnly(t *testing.T) {
	srv, _, token := newAdminServer(t)

	status, body := doRequest(t, srv, http.MethodPost, "/admin/models", token, map[string]any{
		"provider": "typesafe", "model_id": "jev-latest", "model_name": "Jev",
		"url": "https://api.typesafe.ai/v1", "api_key": "ts-key", "model_type": "systemone",
	})
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", status, body)
	}
	var created struct {
		ModelType   string `json:"model_type"`
		BillingMode string `json:"billing_mode"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.ModelType != "systemone" || created.BillingMode != model.BillingModeInputOnly {
		t.Fatalf("created = %+v", created)
	}
}

func TestCreateModel_ChatTypeDefaultsPerToken(t *testing.T) {
	srv, _, token := newAdminServer(t)

	status, body := doRequest(t, srv, http.MethodPost, "/admin/models", token, map[string]any{
		"model_id": "gpt-x", "url": "https://api.example.com/v1", "api_key": "k",
		"model_type": "openai-completions",
	})
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", status, body)
	}
	var created struct {
		BillingMode string `json:"billing_mode"`
	}
	_ = json.Unmarshal(body, &created)
	if created.BillingMode != model.BillingModePerToken {
		t.Fatalf("billing_mode = %q", created.BillingMode)
	}
}

func TestCreateModel_InvalidBillingMode(t *testing.T) {
	srv, _, token := newAdminServer(t)

	status, body := doRequest(t, srv, http.MethodPost, "/admin/models", token, map[string]any{
		"model_id": "m", "url": "https://api.example.com/v1", "api_key": "k",
		"model_type": "systemone", "billing_mode": "tokens",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", status, body)
	}
	if !strings.Contains(string(body), "billing_mode") {
		t.Fatalf("body = %s", body)
	}
}

func TestCreateModel_AliasTypeRejected(t *testing.T) {
	srv, _, token := newAdminServer(t)

	status, body := doRequest(t, srv, http.MethodPost, "/admin/models", token, map[string]any{
		"model_id": "m", "url": "https://api.example.com/v1", "api_key": "k",
		"model_type": "openai-decisions-alias",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", status, body)
	}
	if !strings.Contains(string(body), "openai-decisions") || !strings.Contains(string(body), "systemone") {
		t.Fatalf("error must list the decision types: %s", body)
	}
}

// 已绑定密钥时禁止跨族改 model_type；解绑后放行。
func TestUpdateModel_CrossFamilyRejectedWhenBound(t *testing.T) {
	srv, store, token := newAdminServer(t)
	ctx := context.Background()

	m := &model.AIModel{Provider: "p", ModelID: "jev", ModelName: "Jev",
		URL: "https://api.typesafe.ai/v1", APIKey: "k", ModelType: model.ModelTypeSystemOne,
		BillingMode: model.BillingModeInputOnly, QuotaDay: -1, Enabled: true}
	if err := store.CreateAIModel(ctx, m); err != nil {
		t.Fatalf("create model: %v", err)
	}
	ownerID := seedOwner(t, store)
	seedKey := seedKeyFor(t, store, ownerID)
	if err := store.CreateAPIKey(ctx, seedKey, []model.APIKeyModel{primaryBinding(m.ID)}); err != nil {
		t.Fatalf("bind key: %v", err)
	}

	status, body := doRequest(t, srv, http.MethodPut, "/admin/models/"+itoa(m.ID), token,
		map[string]any{"model_type": "openai-completions"})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", status, body)
	}
	if !strings.Contains(string(body), "unbind") {
		t.Fatalf("body = %s", body)
	}
}

// seedKeyFor 构造一把最小可用密钥（哈希与显示前缀已生成）。
func seedKeyFor(t *testing.T, _ *model.Store, ownerID uint) *model.APIKey {
	t.Helper()
	_, hash, prefix, err := model.GenerateToken(model.TokenPrefixKey)
	if err != nil {
		t.Fatalf("generate key token: %v", err)
	}
	return &model.APIKey{Name: "binding-key", UserID: ownerID, ProxyTokenHash: hash, ProxyTokenPrefix: prefix}
}

func primaryBinding(id uint) model.APIKeyModel {
	return model.APIKeyModel{AIModelID: id, Role: model.ModelRolePrimary}
}

// 决策模型的探测发一道 noul 问句，回执是形式如 yes=0.97 的概率。
func TestConnectivity_DecisionModel(t *testing.T) {
	provider.SetAllowPrivateUpstreams(true)
	t.Cleanup(func() { provider.SetAllowPrivateUpstreams(false) })
	upstream, rec := startDecisionProbeUpstream(t)
	srv, store, token := newAdminServer(t)
	ctx := context.Background()

	m := &model.AIModel{Provider: "typesafe", ModelID: "jev-latest", ModelName: "Jev",
		URL: upstream.URL + "/v1", APIKey: "k", ModelType: model.ModelTypeSystemOne,
		BillingMode: model.BillingModeInputOnly, QuotaDay: -1, Enabled: true}
	if err := store.CreateAIModel(ctx, m); err != nil {
		t.Fatalf("create model: %v", err)
	}

	status, body := doRequest(t, srv, http.MethodPost, "/admin/models/"+itoa(m.ID)+"/connectivity", token, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s", status, body)
	}
	var probe struct {
		OK    bool   `json:"ok"`
		Reply string `json:"reply"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !probe.OK || probe.Reply != "yes=0.97" {
		t.Fatalf("probe = %+v", probe)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.path != "/v1/systemone" {
		t.Fatalf("upstream path = %q", rec.path)
	}
}
