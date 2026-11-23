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

package provider

// 本文件定义决策模型的内部中间表示（IR）与错误类型。
// 决策协议有两条入口（OpenAI Decisions / TypeSafe System One）与两种上游格式，
// 两两之间需要转换；IR 是唯一中转点，避免 N×M 的组合在本包里重写一遍。
// 与 chat 向 Provider 的契约（恒吐 OpenAI-chat 形状）不同，决策路径恒吐 IR。

import (
	"encoding/json"
	"fmt"
)

// DecisionQuestionType 是 IR 里统一后的提问类型，取值沿用 OpenAI Decisions 的命名。
// System One 的 noul 在此归为 predicate（两者都问「概率是多少」）。
type DecisionQuestionType string

// 决策提问类型。
const (
	DecisionPredicate DecisionQuestionType = "predicate"
	DecisionPick      DecisionQuestionType = "choice"
	DecisionScore     DecisionQuestionType = "score"
	DecisionFrame     DecisionQuestionType = "box"
)

// DecisionOption 是一道 choice 题的一个选项。
// System One 的选项固定是字符串 key；OpenAI 允许 string 或 bool，
// 所以 bool 值的原样信息存在 BoolValue 里（nil 表示字符串选项）。
type DecisionOption struct {
	Value       string
	Description string
	BoolValue   *bool
}

// DecisionLevel 是一道 score 题的一个档位（最低档在前）。
type DecisionLevel struct {
	Label       string
	Description string
}

// DecisionQuestion 是一道提问。Name 为答案回显与跨协议映射的键。
type DecisionQuestion struct {
	Name         string
	Type         DecisionQuestionType
	Instructions string
	Choices      []DecisionOption
	Levels       []DecisionLevel
}

// DecisionMessage 是 OpenAI 入口 message 数组里的一条，已拍平成「角色 + 文本 + 图片」。
type DecisionMessage struct {
	Role   string
	Text   string
	Images []string
}

// DecisionRequest 是一次决策请求的 IR。State 与 Messages 二选一：
// System One 用 State，OpenAI 入口若给的是 string 也用 State，给 message 数组则填 Messages。
type DecisionRequest struct {
	Model     string
	State     json.RawMessage
	Messages  []DecisionMessage
	Questions []DecisionQuestion

	Images   []string                   // data URL 形式
	Assets   map[string]json.RawMessage // 仅 System One：按占位符索引的素材
	SafetyID string                     // 仅 OpenAI
}

// DecisionProbability 是 choice 答案里某个取值的概率。
type DecisionProbability struct {
	Value       string
	Probability float64
}

// DecisionScoreProbability 是 score 答案里某个档位的概率。
type DecisionScoreProbability struct {
	Value       int
	Probability float64
}

// DecisionRegion 是 box 答案在图片上的矩形（相对坐标）。
type DecisionRegion struct {
	Top, Bottom, Left, Right float64
}

// DecisionAnswer 是一道提问的答案。字段按题类型选用，未用的保持零值。
type DecisionAnswer struct {
	Type          DecisionQuestionType
	Name          string
	Probability   *float64                   // predicate
	Noul          *float64                   // System One 的 noul（与 Probability 同义，保留原名便于回写）
	Choice        string                     // choice：命中值的字符串形式
	ChoiceBool    *bool                      // choice：上游若给的是 bool 则原样保留
	Probabilities []DecisionProbability      // choice
	Score         *float64                   // score：按概率加权的档位下标
	Levels        []DecisionScoreProbability // score
	Legend        []string                   // score：下标 → label
	Box           *DecisionRegion            // box
	Confidence    *float64                   // choice / score / box
	Refused       bool                       // 上游拒答（OpenAI 的 type:"refusal"）
}

// DecisionResponse 是一次决策响应的 IR。Raw 保留上游未被识别字段，原样回写客户端。
type DecisionResponse struct {
	Model   string
	Answers []DecisionAnswer
	Usage   *Usage
	Raw     map[string]json.RawMessage
}

// DecisionBadRequest 表示入口请求体不合法（由网关返回 400，文案英文）。
type DecisionBadRequest struct {
	Field   string
	Message string
}

// Error 实现 error；Field 非空时前置在消息里，便于客户端定位。
func (e *DecisionBadRequest) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// badRequest 是构造 DecisionBadRequest 的简写。
func badRequest(field, format string, args ...any) *DecisionBadRequest {
	return &DecisionBadRequest{Field: field, Message: fmt.Sprintf(format, args...)}
}
