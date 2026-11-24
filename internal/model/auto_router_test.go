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
	"testing"

	"aigate/internal/model"
)

// ListKeyCandidates 的候选口径必须与 /v1/models 的可见集一致（见 design §7.1）：
// 只含已启用、且 user_key_required 模型确实带绑定密钥的绑定，按 sort_order 升序。
func TestListKeyCandidates_MatchesVisibleSet(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	owner := mustCreateUser(t, store, "owner")

	primary := mustCreateAIModel(t, store, "primary-model")
	cheap := mustCreateAIModel(t, store, "cheap-model")

	// 已禁用：两侧都不该出现。
	disabled := mustCreateAIModel(t, store, "disabled-model")
	if _, err := store.UpdateAIModel(ctx, disabled.ID, map[string]any{"enabled": false}); err != nil {
		t.Fatalf("disable model: %v", err)
	}

	// user_key_required 且绑定行无密钥：可被 ResolveModelForKey 匹配到，但解析必然
	// 失败（ErrBindingKeyMissing）。候选池必须跳过它，否则决策模型会选中一个随后报错的目标。
	needsKey := newTestAIModel("needs-key-model")
	needsKey.UserKeyRequired = true
	if err := store.CreateAIModel(ctx, needsKey); err != nil {
		t.Fatalf("create user_key_required model: %v", err)
	}

	key, _ := mustCreateAPIKey(t, store, owner.ID,
		binding(cheap.ID, model.ModelRoleFallback, 5),
		binding(primary.ID, model.ModelRolePrimary, 0),
		binding(disabled.ID, model.ModelRoleFallback, 9),
		binding(needsKey.ID, model.ModelRoleFallback, 6),
	)

	cands, err := store.ListKeyCandidates(ctx, key.ID)
	if err != nil {
		t.Fatalf("ListKeyCandidates: %v", err)
	}

	var got []string
	for _, c := range cands {
		got = append(got, c.ModelID)
	}
	want := []string{"primary-model", "cheap-model"} // 按 sort_order：0 然后 5
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
	}

	// 与 /v1/models 的可见集逐条对齐（这是本函数存在的理由）。
	infos, err := store.ListKeyModels(ctx, key.ID, true)
	if err != nil {
		t.Fatalf("ListKeyModels: %v", err)
	}
	if len(infos) != len(cands) {
		t.Fatalf("ListKeyModels returned %d, ListKeyCandidates returned %d; pools must agree",
			len(infos), len(cands))
	}
	for i := range infos {
		if infos[i].AIModelID != cands[i].AIModelID {
			t.Fatalf("pool mismatch at %d: models api %d, candidates %d",
				i, infos[i].AIModelID, cands[i].AIModelID)
		}
	}

	// 候选必须带上比价与协议判断所需的字段。
	if cands[0].ModelType == "" {
		t.Fatal("candidate ModelType should be populated")
	}
}

// user_key_required 且绑定行**有**密钥的模型应当进候选池（它确实可解析）。
func TestListKeyCandidates_IncludesUserKeyRequiredWithKey(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	owner := mustCreateUser(t, store, "owner")
	primary := mustCreateAIModel(t, store, "primary-model")

	needsKey := newTestAIModel("needs-key-model")
	needsKey.UserKeyRequired = true
	if err := store.CreateAIModel(ctx, needsKey); err != nil {
		t.Fatalf("create user_key_required model: %v", err)
	}

	withKey := binding(needsKey.ID, model.ModelRoleFallback, 3)
	withKey.APIKey = "sk-user-provided"
	key, _ := mustCreateAPIKey(t, store, owner.ID,
		binding(primary.ID, model.ModelRolePrimary, 0),
		withKey,
	)

	cands, err := store.ListKeyCandidates(ctx, key.ID)
	if err != nil {
		t.Fatalf("ListKeyCandidates: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2", len(cands))
	}
}

// 兜底必须显式解析 primary：ResolveModelForKey 收到 "auto" 会返回 ErrModelNotBound，
// 不能用请求里的模型名去兜底（见 design §7.5）。
func TestResolvePrimaryForKey_IgnoresRequestedName(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()

	owner := mustCreateUser(t, store, "owner")
	primary := mustCreateAIModel(t, store, "primary-model")
	cheap := mustCreateAIModel(t, store, "cheap-model")
	key, _ := mustCreateAPIKey(t, store, owner.ID,
		binding(primary.ID, model.ModelRolePrimary, 0),
		binding(cheap.ID, model.ModelRoleFallback, 5),
	)

	// 前置：用请求名 "auto" 直接解析会失败，所以兜底不能走这条路。
	if _, err := store.ResolveModelForKey(ctx, key, "auto"); err == nil {
		t.Fatal("resolving the literal name auto should fail, making it unusable as a fallback")
	}

	got, err := store.ResolvePrimaryForKey(ctx, key)
	if err != nil {
		t.Fatalf("ResolvePrimaryForKey: %v", err)
	}
	if got.ModelID != "primary-model" {
		t.Fatalf("ModelID = %q, want primary-model", got.ModelID)
	}
}
