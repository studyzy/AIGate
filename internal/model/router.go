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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// 路由作用域。与配额引擎的四个层级同构：越具体越优先。
const (
	RouterScopeGlobal = "global"
	RouterScopeGroup  = "group"
	RouterScopeUser   = "user"
	RouterScopeKey    = "key"
)

// ValidRouterScope 报告作用域是否合法。
func ValidRouterScope(s string) bool {
	switch s {
	case RouterScopeGlobal, RouterScopeGroup, RouterScopeUser, RouterScopeKey:
		return true
	}
	return false
}

// routerScopeRank 返回作用域的具体程度（大者更具体），用于匹配时的优先级比较。
// 与 priority 的关系：先比作用域具体程度，同作用域再比 priority（小者优先）。
func routerScopeRank(scope string) int {
	switch scope {
	case RouterScopeKey:
		return 4
	case RouterScopeUser:
		return 3
	case RouterScopeGroup:
		return 2
	case RouterScopeGlobal:
		return 1
	}
	return 0
}

// ModelRouter 是一条路由规则：一个名字 + 一种策略 + 一份策略配置 + 作用域 + 优先级。
//
// Strategy 与 Config 的关系是「策略私有的」：表结构不随策略增加而变化，
// 配置的形状与校验由策略自身负责（见 internal/proxy 的 Strategy 接口）。
type ModelRouter struct {
	ID          uint   `gorm:"primaryKey"`
	Name        string `gorm:"size:128;not null;default:''"`
	Description string `gorm:"size:512;not null;default:''"`
	// Strategy 是策略名（落库字面量），必须在策略注册表中。
	Strategy string `gorm:"size:32;not null;default:''"`
	// Config 是策略私有配置 JSON；由策略的 ValidateConfig 校验后写入。
	Config string `gorm:"type:text;not null"`
	// Scope/ScopeID 决定这条规则对谁生效；Scope=global 时 ScopeID 恒为 0。
	Scope   string `gorm:"size:16;not null;default:'global'"`
	ScopeID uint   `gorm:"not null;default:0;index"`
	// Priority 是同一作用域内（含作用域 ID）的排序，小者优先。
	Priority  int       `gorm:"not null;default:0"`
	Enabled   bool      `gorm:"not null;default:true"`
	CreatedAt time.Time `gorm:"not null"`
	UpdatedAt time.Time `gorm:"not null"`
}

// TableName 显式指定表名（与 sql/init.sql 一致）。
func (ModelRouter) TableName() string { return "model_routers" }

var (
	// ErrRouterPriorityTaken 表示同一作用域内该优先级已被占用。
	ErrRouterPriorityTaken = errors.New("router priority already used in this scope")
	// ErrRouterNotFound 表示路由规则不存在。
	ErrRouterNotFound = errors.New("router not found")
)

// NormalizeRouterScope 规范化作用域：global 清空 ScopeID，其余要求 ScopeID > 0。
func NormalizeRouterScope(scope string, scopeID uint) (string, uint, error) {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		scope = RouterScopeGlobal
	}
	if !ValidRouterScope(scope) {
		return "", 0, fmt.Errorf("invalid router scope %q", scope)
	}
	if scope == RouterScopeGlobal {
		return scope, 0, nil
	}
	if scopeID == 0 {
		return "", 0, fmt.Errorf("router scope %q requires a scope id", scope)
	}
	return scope, scopeID, nil
}

// CreateModelRouter 创建路由规则；校验作用域规范化与同作用域优先级唯一。
func (s *Store) CreateModelRouter(ctx context.Context, r *ModelRouter) error {
	scope, scopeID, err := NormalizeRouterScope(r.Scope, r.ScopeID)
	if err != nil {
		return err
	}
	r.Scope, r.ScopeID = scope, scopeID
	if strings.TrimSpace(r.Strategy) == "" {
		return errors.New("router strategy is required")
	}
	if r.Config == "" {
		r.Config = "{}"
	}
	if !json.Valid([]byte(r.Config)) {
		return errors.New("router config must be valid json")
	}
	if err := s.checkRouterPriorityUnique(ctx, 0, r); err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).Create(r).Error; err != nil {
		return fmt.Errorf("create model router: %w", err)
	}
	return nil
}

// UpdateModelRouter 更新路由规则（部分字段）；优先级变更同样校验唯一性。
func (s *Store) UpdateModelRouter(ctx context.Context, id uint, fields map[string]any) (*ModelRouter, error) {
	existing, err := s.GetModelRouter(ctx, id)
	if err != nil {
		return nil, err
	}
	merged := *existing
	if v, ok := fields["scope"].(string); ok {
		merged.Scope = v
	}
	if v, ok := fields["scope_id"].(uint); ok {
		merged.ScopeID = v
	}
	if v, ok := fields["priority"].(int); ok {
		merged.Priority = v
	}
	scope, scopeID, err := NormalizeRouterScope(merged.Scope, merged.ScopeID)
	if err != nil {
		return nil, err
	}
	fields["scope"], fields["scope_id"] = scope, scopeID
	if v, ok := fields["config"].(string); ok && !json.Valid([]byte(v)) {
		return nil, errors.New("router config must be valid json")
	}
	if err := s.checkRouterPriorityUnique(ctx, id, &merged); err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Model(&ModelRouter{}).Where("id = ?", id).Updates(fields).Error; err != nil {
		return nil, fmt.Errorf("update model router: %w", err)
	}
	return s.GetModelRouter(ctx, id)
}

// checkRouterPriorityUnique 校验同作用域内优先级未被其他规则占用（自身除外）。
func (s *Store) checkRouterPriorityUnique(ctx context.Context, selfID uint, r *ModelRouter) error {
	var n int64
	q := s.db.WithContext(ctx).Model(&ModelRouter{}).
		Where("scope = ? AND scope_id = ? AND priority = ?", r.Scope, r.ScopeID, r.Priority)
	if selfID != 0 {
		q = q.Where("id <> ?", selfID)
	}
	if err := q.Count(&n).Error; err != nil {
		return fmt.Errorf("check router priority: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("%w: priority %d in scope %q", ErrRouterPriorityTaken, r.Priority, r.Scope)
	}
	return nil
}

// GetModelRouter 按 ID 读取路由规则。
func (s *Store) GetModelRouter(ctx context.Context, id uint) (*ModelRouter, error) {
	var r ModelRouter
	if err := s.db.WithContext(ctx).First(&r, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrRouterNotFound
		}
		return nil, fmt.Errorf("get model router: %w", err)
	}
	return &r, nil
}

// ListModelRouters 列出全部路由规则（按作用域具体程度倒序，再按优先级升序）。
func (s *Store) ListModelRouters(ctx context.Context) ([]ModelRouter, error) {
	var rows []ModelRouter
	if err := s.db.WithContext(ctx).Order("priority ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list model routers: %w", err)
	}
	// 排序在内存里做：作用域的具体程度是业务语义，不宜写进 SQL 的 CASE。
	sortRoutersByMatchOrder(rows)
	return rows, nil
}

// sortRoutersByMatchOrder 按匹配顺序排序：作用域越具体越靠前，同级按 priority 升序。
func sortRoutersByMatchOrder(rows []ModelRouter) {
	// 插入排序：规则数量很少（个位数到几十），避免为一次排序引入 sort 包的比较器闭包。
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && routerLess(rows[j], rows[j-1]); j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}

// routerLess 报告 a 是否应排在 b 之前。
func routerLess(a, b ModelRouter) bool {
	if ra, rb := routerScopeRank(a.Scope), routerScopeRank(b.Scope); ra != rb {
		return ra > rb
	}
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	return a.ID < b.ID
}

// DeleteModelRouter 删除路由规则。
func (s *Store) DeleteModelRouter(ctx context.Context, id uint) error {
	res := s.db.WithContext(ctx).Where("id = ?", id).Delete(&ModelRouter{})
	if res.Error != nil {
		return fmt.Errorf("delete model router: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrRouterNotFound
	}
	return nil
}

// MatchModelRouter 为一次请求挑选生效的路由规则：取最具体作用域下、优先级最小且启用的规则。
//
// 返回 (nil, nil) 表示没有规则适用——调用方据此判定 auto 不可用，而不是当成错误。
// 匹配顺序：key > user > group > global；同一作用域内按 priority 升序（小者优先）。
func (s *Store) MatchModelRouter(ctx context.Context, key *APIKey) (*ModelRouter, error) {
	rows, err := s.ListModelRouters(ctx)
	if err != nil {
		return nil, err
	}
	// 按作用域分档：只有同一档内命中的才参与竞争，避免「某个 key 级规则不匹配」
	// 却被更宽的作用域抢先返回。rows 已按 routerLess 排序，各档内 priority 升序。
	for _, scope := range []string{RouterScopeKey, RouterScopeUser, RouterScopeGroup, RouterScopeGlobal} {
		for i := range rows {
			r := &rows[i]
			if !r.Enabled || r.Scope != scope {
				continue
			}
			if routerMatchesScope(r, key) {
				return r, nil
			}
		}
	}
	return nil, nil
}

// routerMatchesScope 报告该规则的作用域是否命中此密钥。
func routerMatchesScope(r *ModelRouter, key *APIKey) bool {
	switch r.Scope {
	case RouterScopeKey:
		return r.ScopeID == key.ID
	case RouterScopeUser:
		return r.ScopeID == key.UserID
	case RouterScopeGroup:
		// 未分组（GroupID = 0）的密钥不匹配任何 group 级规则。
		return key.GroupID != 0 && r.ScopeID == key.GroupID
	case RouterScopeGlobal:
		return true
	}
	return false
}
