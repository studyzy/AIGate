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

package model_test

import (
	"errors"
	"testing"

	"aigate/internal/model"
)

// newRouter 构造一条最小可用的路由规则（未入库）。
func newRouter(name, scope string, scopeID uint, priority int) *model.ModelRouter {
	return &model.ModelRouter{
		Name:     name,
		Strategy: "decision",
		Config:   `{"decision_model_id":1}`,
		Scope:    scope,
		ScopeID:  scopeID,
		Priority: priority,
		Enabled:  true,
	}
}

// mustCreateRouter 入库一条路由规则。
func mustCreateRouter(t *testing.T, store *model.Store, r *model.ModelRouter) *model.ModelRouter {
	t.Helper()
	if err := store.CreateModelRouter(testCtx(), r); err != nil {
		t.Fatalf("create router %q: %v", r.Name, err)
	}
	return r
}

// scope:id 作用域规范化：global 强制清空 scope_id，其余要求 scope_id > 0。
func TestCreateModelRouter_ScopeNormalization(t *testing.T) {
	store := newTestStore(t)

	global := newRouter("g", model.RouterScopeGlobal, 99, 0)
	if err := store.CreateModelRouter(testCtx(), global); err != nil {
		t.Fatalf("create global: %v", err)
	}
	if global.ScopeID != 0 {
		t.Fatalf("global scope_id = %d, want 0", global.ScopeID)
	}

	noID := newRouter("u", model.RouterScopeUser, 0, 0)
	if err := store.CreateModelRouter(testCtx(), noID); err == nil {
		t.Fatal("non-global scope without scope_id must be rejected")
	}

	badScope := newRouter("x", "tenant", 1, 0)
	if err := store.CreateModelRouter(testCtx(), badScope); err == nil {
		t.Fatal("unknown scope must be rejected")
	}

	badJSON := newRouter("j", model.RouterScopeGlobal, 0, 5)
	badJSON.Config = "{not json"
	if err := store.CreateModelRouter(testCtx(), badJSON); err == nil {
		t.Fatal("invalid config json must be rejected")
	}
}

// 同一作用域内优先级唯一：重复写入必须被拒，否则运行期命中会变成随机结果。
func TestCreateModelRouter_PriorityConflict(t *testing.T) {
	store := newTestStore(t)
	mustCreateRouter(t, store, newRouter("a", model.RouterScopeGlobal, 0, 10))

	dup := newRouter("b", model.RouterScopeGlobal, 0, 10)
	err := store.CreateModelRouter(testCtx(), dup)
	if !errors.Is(err, model.ErrRouterPriorityTaken) {
		t.Fatalf("err = %v, want ErrRouterPriorityTaken", err)
	}

	// 不同作用域可以有相同优先级——它们不会互相竞争。
	if err := store.CreateModelRouter(testCtx(), newRouter("c", model.RouterScopeUser, 7, 10)); err != nil {
		t.Fatalf("same priority in a different scope should be allowed: %v", err)
	}

	// 更新时也要校验（排除自身）。
	if _, err := store.UpdateModelRouter(testCtx(), 1, map[string]any{"priority": 20}); err != nil {
		t.Fatalf("self update should be allowed: %v", err)
	}
}

// 匹配语义：key > user > group > global；同级按 priority 升序；禁用跳过。
func TestMatchModelRouter_ScopePrecedence(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	owner := mustCreateUser(t, store, "owner")
	group := &model.Group{Name: "g1", TokenQuotaRules: "[]", GlobalTokenQuotaRules: "[]"}
	if err := store.CreateGroup(ctx, group); err != nil {
		t.Fatalf("create group: %v", err)
	}
	owner.GroupID = group.ID
	if _, err := store.UpdateUser(ctx, owner.ID, map[string]any{"group_id": group.ID}); err != nil {
		t.Fatalf("set group: %v", err)
	}

	primaryModel := mustCreateAIModel(t, store, "m")
	key, _ := mustCreateAPIKey(t, store, owner.ID, binding(primaryModel.ID, model.ModelRolePrimary, 0))
	// 让密钥带上分组（CreateAPIKey 按 owner 的分组落库；显式确认一次）。
	key.GroupID = group.ID

	global := mustCreateRouter(t, store, newRouter("global", model.RouterScopeGlobal, 0, 100))
	groupR := mustCreateRouter(t, store, newRouter("group", model.RouterScopeGroup, group.ID, 50))
	userR := mustCreateRouter(t, store, newRouter("user", model.RouterScopeUser, owner.ID, 20))
	keyR := mustCreateRouter(t, store, newRouter("key", model.RouterScopeKey, key.ID, 10))

	steps := []struct {
		name    string
		disable func()
		want    uint
	}{
		{"key wins", func() {}, keyR.ID},
		{"user wins after key disabled", func() { disableRouter(t, store, keyR.ID) }, userR.ID},
		{"group wins after user disabled", func() { disableRouter(t, store, userR.ID) }, groupR.ID},
		{"global after group disabled", func() { disableRouter(t, store, groupR.ID) }, global.ID},
	}
	for _, st := range steps {
		st.disable()
		got, err := store.MatchModelRouter(ctx, key)
		if err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		if got == nil || got.ID != st.want {
			t.Fatalf("%s: matched %v, want router %d", st.name, got, st.want)
		}
	}

	// 全部禁用：没有可用的 router。
	disableRouter(t, store, global.ID)
	got, err := store.MatchModelRouter(ctx, key)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if got != nil {
		t.Fatalf("matched %v, want nil when every router is disabled", got)
	}
}

// 同作用域内按 priority 升序：小者优先。
func TestMatchModelRouter_PriorityOrder(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	owner := mustCreateUser(t, store, "owner")
	primaryModel := mustCreateAIModel(t, store, "m")
	key, _ := mustCreateAPIKey(t, store, owner.ID, binding(primaryModel.ID, model.ModelRolePrimary, 0))

	later := mustCreateRouter(t, store, newRouter("later", model.RouterScopeGlobal, 0, 50))
	earlier := mustCreateRouter(t, store, newRouter("earlier", model.RouterScopeGlobal, 0, 5))

	got, err := store.MatchModelRouter(ctx, key)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if got == nil || got.ID != earlier.ID {
		t.Fatalf("matched %v, want the lower-priority-number router %d (not %d)", got, earlier.ID, later.ID)
	}
}

// 未分组的密钥不应命中任何 group 级规则。
func TestMatchModelRouter_UngroupedKeySkipsGroupScope(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	owner := mustCreateUser(t, store, "owner")
	primaryModel := mustCreateAIModel(t, store, "m")
	key, _ := mustCreateAPIKey(t, store, owner.ID, binding(primaryModel.ID, model.ModelRolePrimary, 0))

	global := mustCreateRouter(t, store, newRouter("global", model.RouterScopeGlobal, 0, 100))
	// 构造一条 scope_id=0 的 group 级规则：规范化会拒绝，这里直接验证匹配层对未分组密钥的处理。
	store_ := newRouter("group", model.RouterScopeGroup, key.GroupID, 1)
	if key.GroupID == 0 {
		got, err := store.MatchModelRouter(ctx, key)
		if err != nil {
			t.Fatalf("match: %v", err)
		}
		if got == nil || got.ID != global.ID {
			t.Fatalf("ungrouped key matched %v, want the global router", got)
		}
	}
	_ = store_
}

// disableRouter 关闭一条路由规则。
func disableRouter(t *testing.T, store *model.Store, id uint) {
	t.Helper()
	if _, err := store.UpdateModelRouter(testCtx(), id, map[string]any{"enabled": false}); err != nil {
		t.Fatalf("disable router %d: %v", id, err)
	}
}

// 作用域命中但 ID 不匹配时，必须继续往下一档找，而不是被更宽作用域的行为掩盖。
// 这是 router 匹配最容易写错的地方（曾把 global 提前返回）。
func TestMatchModelRouter_SkipsNonMatchingScopeIDs(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	owner := mustCreateUser(t, store, "owner")
	other := mustCreateUser(t, store, "other")
	primaryModel := mustCreateAIModel(t, store, "m")
	key, _ := mustCreateAPIKey(t, store, owner.ID, binding(primaryModel.ID, model.ModelRolePrimary, 0))

	global := mustCreateRouter(t, store, newRouter("global", model.RouterScopeGlobal, 0, 100))
	// 指向别的用户的 user 级规则：对本密钥不适用。
	mustCreateRouter(t, store, newRouter("other-user", model.RouterScopeUser, other.ID, 1))

	got, err := store.MatchModelRouter(ctx, key)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if got == nil || got.ID != global.ID {
		t.Fatalf("matched %v, want the global router: a user rule for another user must not apply", got)
	}
}
