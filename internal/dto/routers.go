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

package dto

// 模型路由的对外契约：策略描述（驱动前端表单）与路由预览结果。
//
// 放在 dto 是因为两侧都要用：管理面（internal/admin）把它序列化给前端，
// 数据面（internal/proxy）实现策略目录与预览并在装配时注入。
// 放在任何一侧都会让另一侧反向依赖它。

// RouterStrategyField 是策略配置里的一个字段。
type RouterStrategyField struct {
	Key         string   `json:"key"`
	Type        string   `json:"type"` // string | int | bool | enum | model
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Default     any      `json:"default,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Min         *int     `json:"min,omitempty"`
	Max         *int     `json:"max,omitempty"`
	Required    bool     `json:"required,omitempty"`
}

// RouterStrategy 是对外描述的一种可用策略。
type RouterStrategy struct {
	Name        string                `json:"name"`
	Title       string                `json:"title"`
	Description string                `json:"description,omitempty"`
	Fields      []RouterStrategyField `json:"fields"`
}

// RouterPreviewResult 是一次路由预览的结果。
type RouterPreviewResult struct {
	RouterID  uint           `json:"router_id"`
	Strategy  string         `json:"strategy"`
	AIModelID uint           `json:"ai_model_id"`
	ModelID   string         `json:"model_id"`
	ModelName string         `json:"model_name,omitempty"`
	Cause     string         `json:"cause"`
	Detail    map[string]any `json:"detail,omitempty"`
	// Fallback 为 true 表示策略未能决定，预览的是「会回退到 primary」这一结果。
	Fallback bool `json:"fallback"`
}
