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

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"aigate/internal/admin"
	"aigate/internal/dto"
	"aigate/internal/model"
)

// stubStrategies 是测试用的策略能力：只认 decision，并拒绝未知字段。
type stubStrategies struct{}

func (stubStrategies) List() []dto.RouterStrategy {
	return []dto.RouterStrategy{{Name: "decision", Title: "Decision", Fields: []dto.RouterStrategyField{
		{Key: "decision_model_id", Type: "model", Title: "Decision model", Required: true},
	}}}
}

func (stubStrategies) ValidateConfig(ctx context.Context, strategy, raw string) (json.RawMessage, error) {
	if strategy != "decision" {
		return nil, errors.New("unknown routing strategy: " + strategy)
	}
	var cfg map[string]any
	if raw == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, err
	}
	known := map[string]bool{"decision_model_id": true, "max_candidates": true, "short_circuit_enabled": true}
	for k := range cfg {
		if !known[k] {
			return nil, errors.New("invalid config: unknown field " + k)
		}
	}
	if _, ok := cfg["decision_model_id"]; !ok {
		return nil, errors.New("invalid config: decision_model_id is required")
	}
	return json.RawMessage(raw), nil
}

// withStrategies 注入测试用策略能力，并在用例结束后清空（全局状态，用例间必须隔离）。
func withStrategies(t *testing.T) {
	t.Helper()
	admin.SetRouterStrategies(stubStrategies{})
	t.Cleanup(func() { admin.SetRouterStrategies(nil) })
}

// routerBody 构造一份合法的新建请求（decision 策略的最小配置）。
func routerBody(name, scope string, scopeID uint, priority int) map[string]any {
	return map[string]any{
		"name":     name,
		"strategy": "decision",
		"config":   `{"decision_model_id":1,"max_candidates":8,"short_circuit_enabled":true}`,
		"scope":    scope,
		"scope_id": scopeID,
		"priority": priority,
	}
}

// 路由 CRUD 全流程：创建 → 列表 → 详情 → 更新 → 删除。
func TestRouterCRUD(t *testing.T) {
	withStrategies(t)
	srv, _, token := newAdminServer(t)

	code, body := doRequest(t, srv, http.MethodPost, "/admin/model-routers", token,
		routerBody("r1", "global", 0, 10))
	if code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body=%s)", code, body)
	}
	var created struct {
		ID       uint   `json:"id"`
		Scope    string `json:"scope"`
		Enabled  bool   `json:"enabled"`
		Strategy string `json:"strategy"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.ID == 0 || created.Scope != "global" || !created.Enabled || created.Strategy != "decision" {
		t.Fatalf("created = %+v", created)
	}

	code, body = doRequest(t, srv, http.MethodGet, "/admin/model-routers", token, nil)
	if code != http.StatusOK {
		t.Fatalf("list: status = %d (body=%s)", code, body)
	}
	var list struct {
		Routers []map[string]any `json:"routers"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Routers) != 1 {
		t.Fatalf("list size = %d, want 1", len(list.Routers))
	}

	code, body = doRequest(t, srv, http.MethodGet, "/admin/model-routers/1", token, nil)
	if code != http.StatusOK {
		t.Fatalf("get: status = %d (body=%s)", code, body)
	}

	code, body = doRequest(t, srv, http.MethodPut, "/admin/model-routers/1", token,
		map[string]any{"name": "renamed", "enabled": false})
	if code != http.StatusOK {
		t.Fatalf("update: status = %d (body=%s)", code, body)
	}
	var updated struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal(body, &updated); err != nil {
		t.Fatalf("decode updated: %v", err)
	}
	if updated.Name != "renamed" || updated.Enabled {
		t.Fatalf("updated = %+v, want renamed and disabled", updated)
	}

	code, _ = doRequest(t, srv, http.MethodDelete, "/admin/model-routers/1", token, nil)
	if code != http.StatusNoContent {
		t.Fatalf("delete: status = %d, want 204", code)
	}
	code, _ = doRequest(t, srv, http.MethodGet, "/admin/model-routers/1", token, nil)
	if code != http.StatusNotFound {
		t.Fatalf("get after delete: status = %d, want 404", code)
	}
}

// 校验：缺名字、缺策略、非法作用域、未知策略、非法配置 JSON，都必须是 400 而非 500。
func TestRouterValidation(t *testing.T) {
	withStrategies(t)
	srv, _, token := newAdminServer(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing name", func() map[string]any { b := routerBody("", "global", 0, 0); return b }()},
		{"missing strategy", func() map[string]any {
			b := routerBody("x", "global", 0, 0)
			delete(b, "strategy")
			return b
		}()},
		{"bad scope", routerBody("x", "tenant", 1, 0)},
		{"non-global without scope id", routerBody("x", "user", 0, 0)},
		{"unknown strategy", func() map[string]any {
			b := routerBody("x", "global", 0, 0)
			b["strategy"] = "does-not-exist"
			return b
		}()},
		{"bad config json", func() map[string]any {
			b := routerBody("x", "global", 0, 0)
			b["config"] = "{not json"
			return b
		}()},
		{"config unknown field", func() map[string]any {
			b := routerBody("x", "global", 0, 0)
			b["config"] = `{"decision_model_id":1,"max_candidtes":8}`
			return b
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doRequest(t, srv, http.MethodPost, "/admin/model-routers", token, tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", code, body)
			}
		})
	}
}

// 同作用域同优先级重复：400，且错误信息点明冲突。
func TestRouterPriorityConflictIsRejected(t *testing.T) {
	withStrategies(t)
	srv, _, token := newAdminServer(t)

	if code, body := doRequest(t, srv, http.MethodPost, "/admin/model-routers", token,
		routerBody("a", "global", 0, 5)); code != http.StatusCreated {
		t.Fatalf("first create: %d %s", code, body)
	}
	code, body := doRequest(t, srv, http.MethodPost, "/admin/model-routers", token,
		routerBody("b", "global", 0, 5))
	if code != http.StatusBadRequest {
		t.Fatalf("duplicate priority: status = %d, want 400 (body=%s)", code, body)
	}
	if decodeError(t, body).Error.Message == "" {
		t.Fatal("error message should explain the conflict")
	}
}

// strategies 端点：返回可用策略及其字段（驱动前端表单）；未装配时明确 503。
func TestRouterStrategiesEndpoint(t *testing.T) {
	srv, _, token := newAdminServer(t)

	code, body := doRequest(t, srv, http.MethodGet, "/admin/model-routers/strategies", token, nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 without injected strategies (body=%s)", code, body)
	}

	withStrategies(t)
	code, body = doRequest(t, srv, http.MethodGet, "/admin/model-routers/strategies", token, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", code, body)
	}
	var out struct {
		Strategies []dto.RouterStrategy `json:"strategies"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Strategies) != 1 || out.Strategies[0].Name != "decision" {
		t.Fatalf("strategies = %+v, want one decision strategy", out.Strategies)
	}
	if len(out.Strategies[0].Fields) == 0 {
		t.Fatal("strategy should publish its config fields for the console to render")
	}
}

// 认证：无 token 一律 401，普通用户 403。
func TestRouterRequiresAdmin(t *testing.T) {
	srv, store, _ := newAdminServer(t)

	if code, _ := doRequest(t, srv, http.MethodGet, "/admin/model-routers", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", code)
	}
	_, userToken := seedUser(t, store, "plain", model.RoleUser, true)
	if code, _ := doRequest(t, srv, http.MethodGet, "/admin/model-routers", userToken, nil); code != http.StatusForbidden {
		t.Fatalf("non-admin: status = %d, want 403", code)
	}
}
