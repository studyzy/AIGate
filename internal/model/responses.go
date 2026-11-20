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

package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// Responses 会话状态（status 列取值）。
const (
	// ResponseStateStatusQueued 已受理，等待后台执行（background 模式）。
	ResponseStateStatusQueued = "queued"
	// ResponseStateStatusInProgress 后台执行中。
	ResponseStateStatusInProgress = "in_progress"
	// ResponseStateStatusCompleted 正常结束。
	ResponseStateStatusCompleted = "completed"
	// ResponseStateStatusFailed 失败（error 列记录原因）。
	ResponseStateStatusFailed = "failed"
	// ResponseStateStatusIncomplete 被 token/内容策略截断的结束。
	ResponseStateStatusIncomplete = "incomplete"
	// ResponseStateStatusCancelled 用户主动取消；终态，后台任务不得覆盖。
	ResponseStateStatusCancelled = "cancelled"
)

// ErrResponseStateNotFound 表示 Responses 会话状态行不存在（已被清理或 ID 非法）。
var ErrResponseStateNotFound = errors.New("response state not found")

// ResponseState 是 OpenAI Responses API 的一次响应状态：items 为累积的对话快照，
// 供 previous_response_id / conversation 拼接历史；response_body 为 GET /v1/responses/{id} 的原样回包。
// 归属用 (owner_user_id, owner_api_key_id) 双键校验，失配按不存在处理。
type ResponseState struct {
	ID                 string    `gorm:"size:64;primaryKey"`
	ConversationID     string    `gorm:"size:64;not null;default:'';index:idx_response_states_conversation"`
	OwnerUserID        uint      `gorm:"not null;default:0;index:idx_response_states_owner,priority:1"`
	OwnerAPIKeyID      uint      `gorm:"not null;default:0;index:idx_response_states_owner,priority:2"`
	RequestModel       string    `gorm:"size:191;not null;default:''"`
	PreviousResponseID string    `gorm:"size:64;not null;default:''"`
	Status             string    `gorm:"size:16;not null;default:'queued'"`
	Items              []byte    `gorm:"type:longtext;not null"`
	ResponseBody       []byte    `gorm:"type:longtext;not null"`
	InputTokens        int       `gorm:"not null;default:0"`
	OutputTokens       int       `gorm:"not null;default:0"`
	TotalTokens        int       `gorm:"not null;default:0"`
	ReasoningTokens    int       `gorm:"not null;default:0"`
	Error              string    `gorm:"type:text;not null"`
	CreatedAt          time.Time `gorm:"not null;index:idx_response_states_created_at"`
	UpdatedAt          time.Time `gorm:"not null"`
}

// CreateResponseState 落一行会话状态；调用方负责生成 ID 与填写归属字段。
// CreatedAt/UpdatedAt 为空时取当前时间。
func (s *Store) CreateResponseState(ctx context.Context, st *ResponseState) error {
	if st.CreatedAt.IsZero() {
		st.CreatedAt = time.Now().UTC()
	}
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = st.CreatedAt
	}
	if st.Items == nil {
		st.Items = []byte("[]")
	}
	if st.ResponseBody == nil {
		st.ResponseBody = []byte("{}")
	}
	if err := s.db.WithContext(ctx).Create(st).Error; err != nil {
		return fmt.Errorf("create response state: %w", err)
	}
	return nil
}

// GetResponseState 按 ID 查询会话状态；不存在返回 ErrResponseStateNotFound。
func (s *Store) GetResponseState(ctx context.Context, id string) (*ResponseState, error) {
	return s.getResponseState(s.db.WithContext(ctx).Where("id = ?", id))
}

// UpdateResponseStateStatus 条件更新状态与终态载荷：已取消（cancelled）的行不会被覆盖。
// errMsg 与 items/body 按「非空才写」处理，避免后台任务在中间态跃迁时清空已有内容。
// 行不存在（含已取消）时静默返回 nil——后台任务据此判定「已被取消/清理」即可，
// 无需区分两种原因；调用方要区分时用 GetResponseState。
func (s *Store) UpdateResponseStateStatus(
	ctx context.Context,
	id, status, errMsg string,
	items, body []byte,
) error {
	fields := map[string]any{
		"status":     status,
		"updated_at": time.Now().UTC(),
	}
	if errMsg != "" {
		fields["error"] = errMsg
	}
	if len(items) > 0 {
		fields["items"] = items
	}
	if len(body) > 0 {
		fields["response_body"] = body
	}
	err := s.db.WithContext(ctx).Model(&ResponseState{}).
		Where("id = ? AND status <> ?", id, ResponseStateStatusCancelled).
		Updates(fields).Error
	if err != nil {
		return fmt.Errorf("update response state status: %w", err)
	}
	return nil
}

// PruneResponseStates 删除创建时间早于 before 的行，返回删除行数。
// 后台清理循环（保留期以天计，默认 30 天）只需按返回值记录日志。
func (s *Store) PruneResponseStates(ctx context.Context, before time.Time) (int64, error) {
	res := s.db.WithContext(ctx).Where("created_at < ?", before.UTC()).Delete(&ResponseState{})
	if res.Error != nil {
		return 0, fmt.Errorf("prune response states: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// LatestResponseStateInConversation 返回会话内最新一条已完成（completed）的行；
// 会话无可用行时返回 ErrResponseStateNotFound。
// 按 created_at DESC, id DESC 稳定排序，id 作为同一毫秒内的确定性兜底。
func (s *Store) LatestResponseStateInConversation(ctx context.Context, conversationID string) (*ResponseState, error) {
	if conversationID == "" {
		return nil, ErrResponseStateNotFound
	}
	return s.getResponseState(
		s.db.WithContext(ctx).
			Where("conversation_id = ? AND status = ?", conversationID, ResponseStateStatusCompleted).
			Order("created_at DESC, id DESC").
			Limit(1),
	)
}

// getResponseState 执行查询并把「无行」统一映射为 ErrResponseStateNotFound。
// 映射走哨兵错误而非 IsNotFound(gorm.ErrRecordNotFound)：需要同时包住 id 与状态条件两类落到「无行」的场景，
// 且让调用方在任何一层 errors.Is 都能判定。
func (s *Store) getResponseState(q *gorm.DB) (*ResponseState, error) {
	var st ResponseState
	if err := q.First(&st).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrResponseStateNotFound
		}
		return nil, fmt.Errorf("get response state: %w", err)
	}
	return &st, nil
}
