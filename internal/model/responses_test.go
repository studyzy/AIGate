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

// Responses 会话状态（response_states）单测：读写一致、不存在哨兵错误、
// 取消态不被后台终态覆盖、保留期清理、会话内取最新一条。

import (
	"errors"
	"testing"
	"time"

	"aigate/internal/model"
)

// newTestResponseState 构造一条会话状态行（未入库）。
func newTestResponseState(id, conversationID string, created time.Time) *model.ResponseState {
	return &model.ResponseState{
		ID:                 id,
		ConversationID:     conversationID,
		OwnerUserID:        7,
		OwnerAPIKeyID:      9,
		RequestModel:       "gpt-4o-mini",
		PreviousResponseID: "",
		Status:             model.ResponseStateStatusCompleted,
		Items:              []byte(`[{"type":"message","role":"user","content":[]}]`),
		ResponseBody:       []byte(`{"id":"` + id + `","object":"response"}`),
		CreatedAt:          created,
		UpdatedAt:          created,
	}
}

// TestCreateAndGetResponseState 覆盖创建后按 ID 读回，items / response_body 原样保留。
func TestCreateAndGetResponseState(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()
	now := time.Now().UTC().Truncate(time.Millisecond)
	want := newTestResponseState("resp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "conv_1", now)
	want.Status = model.ResponseStateStatusQueued

	if err := store.CreateResponseState(ctx, want); err != nil {
		t.Fatalf("CreateResponseState: %v", err)
	}
	got, err := store.GetResponseState(ctx, want.ID)
	if err != nil {
		t.Fatalf("GetResponseState: %v", err)
	}
	if got.ID != want.ID || got.ConversationID != want.ConversationID {
		t.Fatalf("identity = %q/%q, want %q/%q", got.ID, got.ConversationID, want.ID, want.ConversationID)
	}
	if string(got.Items) != string(want.Items) {
		t.Fatalf("Items = %q, want %q", got.Items, want.Items)
	}
	if string(got.ResponseBody) != string(want.ResponseBody) {
		t.Fatalf("ResponseBody = %q, want %q", got.ResponseBody, want.ResponseBody)
	}
	if got.Status != model.ResponseStateStatusQueued {
		t.Fatalf("Status = %q, want %q", got.Status, model.ResponseStateStatusQueued)
	}
}

// TestGetResponseState_NotFound 覆盖不存在的 ID 返回哨兵错误。
func TestGetResponseState_NotFound(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.GetResponseState(testCtx(), "resp_ffffffffffffffffffffffffffffffff"); !errors.Is(err, model.ErrResponseStateNotFound) {
		t.Fatalf("err = %v, want ErrResponseStateNotFound", err)
	}
}

// TestUpdateResponseStateStatus_CancelledNotOverwritten 钉住条件更新：
// 已取消的行不会被后台任务写回的终态覆盖。
func TestUpdateResponseStateStatus_CancelledNotOverwritten(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()
	now := time.Now().UTC().Truncate(time.Millisecond)
	st := newTestResponseState("resp_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "conv_1", now)
	st.Status = model.ResponseStateStatusInProgress
	if err := store.CreateResponseState(ctx, st); err != nil {
		t.Fatalf("CreateResponseState: %v", err)
	}

	// 用户取消。
	if err := store.UpdateResponseStateStatus(ctx, st.ID, model.ResponseStateStatusCancelled, "", nil, nil); err != nil {
		t.Fatalf("UpdateResponseStateStatus(cancelled): %v", err)
	}
	// 后台任务随后写回终态——必须被条件更新拦下。
	if err := store.UpdateResponseStateStatus(ctx, st.ID, model.ResponseStateStatusCompleted, "", []byte(`[]`), []byte(`{}`)); err != nil {
		t.Fatalf("UpdateResponseStateStatus(completed after cancel): %v", err)
	}

	got, err := store.GetResponseState(ctx, st.ID)
	if err != nil {
		t.Fatalf("GetResponseState: %v", err)
	}
	if got.Status != model.ResponseStateStatusCancelled {
		t.Fatalf("Status = %q, want %q (cancelled must not be overwritten)", got.Status, model.ResponseStateStatusCancelled)
	}
	if string(got.Items) != string(st.Items) || string(got.ResponseBody) != string(st.ResponseBody) {
		t.Fatalf("cancelled row payload changed: items=%q body=%q", got.Items, got.ResponseBody)
	}
}

// TestUpdateResponseStateStatus_WritesTerminal 覆盖正常状态跃迁会写入终态与载荷。
func TestUpdateResponseStateStatus_WritesTerminal(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()
	now := time.Now().UTC().Truncate(time.Millisecond)
	st := newTestResponseState("resp_cccccccccccccccccccccccccccccccc", "conv_1", now)
	st.Status = model.ResponseStateStatusQueued
	if err := store.CreateResponseState(ctx, st); err != nil {
		t.Fatalf("CreateResponseState: %v", err)
	}

	items := []byte(`[{"type":"message","role":"assistant"}]`)
	body := []byte(`{"id":"resp_c","status":"completed"}`)
	if err := store.UpdateResponseStateStatus(ctx, st.ID, model.ResponseStateStatusCompleted, "", items, body); err != nil {
		t.Fatalf("UpdateResponseStateStatus(completed): %v", err)
	}
	got, err := store.GetResponseState(ctx, st.ID)
	if err != nil {
		t.Fatalf("GetResponseState: %v", err)
	}
	if got.Status != model.ResponseStateStatusCompleted {
		t.Fatalf("Status = %q, want %q", got.Status, model.ResponseStateStatusCompleted)
	}
	if string(got.Items) != string(items) || string(got.ResponseBody) != string(body) {
		t.Fatalf("Items/ResponseBody = %q/%q, want %q/%q", got.Items, got.ResponseBody, items, body)
	}

	// 空载荷不应清空已有内容（后台只在终态一次性写回）。
	if err := store.UpdateResponseStateStatus(ctx, st.ID, model.ResponseStateStatusFailed, "upstream 500", nil, nil); err != nil {
		t.Fatalf("UpdateResponseStateStatus(failed): %v", err)
	}
	got, err = store.GetResponseState(ctx, st.ID)
	if err != nil {
		t.Fatalf("GetResponseState: %v", err)
	}
	if got.Status != model.ResponseStateStatusFailed || got.Error != "upstream 500" {
		t.Fatalf("Status/Error = %q/%q, want failed/upstream 500", got.Status, got.Error)
	}
	if string(got.Items) != string(items) || string(got.ResponseBody) != string(body) {
		t.Fatalf("nil payloads overwrote stored ones: items=%q body=%q", got.Items, got.ResponseBody)
	}
}

// TestPruneResponseStates 覆盖保留期清理：早于 cutoff 的行删除，其余保留。
func TestPruneResponseStates(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()
	now := time.Now().UTC().Truncate(time.Millisecond)

	old := newTestResponseState("resp_dddddddddddddddddddddddddddddddd", "conv_1", now.Add(-10*24*time.Hour))
	fresh := newTestResponseState("resp_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "conv_1", now)
	for _, st := range []*model.ResponseState{old, fresh} {
		if err := store.CreateResponseState(ctx, st); err != nil {
			t.Fatalf("CreateResponseState(%s): %v", st.ID, err)
		}
	}

	n, err := store.PruneResponseStates(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("PruneResponseStates: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned = %d, want 1", n)
	}
	if _, err := store.GetResponseState(ctx, old.ID); !errors.Is(err, model.ErrResponseStateNotFound) {
		t.Fatalf("old row err = %v, want ErrResponseStateNotFound", err)
	}
	if _, err := store.GetResponseState(ctx, fresh.ID); err != nil {
		t.Fatalf("fresh row must survive prune: %v", err)
	}

	// 边界：created_at 恰好等于 cutoff 的保留（严格小于判定）。
	n, err = store.PruneResponseStates(ctx, fresh.CreatedAt)
	if err != nil {
		t.Fatalf("PruneResponseStates(boundary): %v", err)
	}
	if n != 0 {
		t.Fatalf("boundary prune removed %d rows, want 0", n)
	}
}

// TestLatestResponseStateInConversation 覆盖会话内取最新一条，且不受其他会话干扰；
// 未完成 / 已取消的行不作为历史前缀（与 Spec §5.6「取最新一条 completed 行」一致）。
func TestLatestResponseStateInConversation(t *testing.T) {
	store := newTestStore(t)
	ctx := testCtx()
	now := time.Now().UTC().Truncate(time.Millisecond)

	older := newTestResponseState("resp_11111111111111111111111111111111", "conv_1", now.Add(-2*time.Hour))
	newer := newTestResponseState("resp_22222222222222222222222222222222", "conv_1", now)
	other := newTestResponseState("resp_33333333333333333333333333333333", "conv_2", now.Add(time.Hour))
	// 更晚但未完成的行：不应被当作会话最新历史。
	pending := newTestResponseState("resp_44444444444444444444444444444444", "conv_1", now.Add(time.Hour))
	pending.Status = model.ResponseStateStatusQueued
	cancelled := newTestResponseState("resp_55555555555555555555555555555555", "conv_1", now.Add(2*time.Hour))
	cancelled.Status = model.ResponseStateStatusCancelled
	for _, st := range []*model.ResponseState{newer, older, other, pending, cancelled} {
		if err := store.CreateResponseState(ctx, st); err != nil {
			t.Fatalf("CreateResponseState(%s): %v", st.ID, err)
		}
	}

	got, err := store.LatestResponseStateInConversation(ctx, "conv_1")
	if err != nil {
		t.Fatalf("LatestResponseStateInConversation: %v", err)
	}
	if got.ID != newer.ID {
		t.Fatalf("latest = %q, want %q", got.ID, newer.ID)
	}

	if _, err := store.LatestResponseStateInConversation(ctx, "conv_missing"); !errors.Is(err, model.ErrResponseStateNotFound) {
		t.Fatalf("missing conversation err = %v, want ErrResponseStateNotFound", err)
	}
}
