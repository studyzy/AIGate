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

package proxy

// 本文件实现 auto 伪模型的路由解析：客户端发 model="auto" 时，从该密钥的候选池里
// 挑一个「最便宜且能胜任」的模型（见 docs/superpowers/specs/2026-11-24-auto-routing-design.md）。
//
// 分两层：
//   - decideAuto（纯函数）：候选池 + 请求体 → 选定的 ai_model_id 与判定原因。
//     不访问数据库、不发网络，全部输入由调用方注入，便于穷举测试。
//   - resolveAuto（装配层）：取价目、调用决策模型、兜底。任何失败都回退到 primary。
//
// 兜底是本设计的核心不变量：auto 不能把一条正常请求变成错误。

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"aigate/internal/model"
	"aigate/internal/provider"
)

// autoCandidates 从候选池里剔除决策协议模型后返回可用候选。
//
// 决策模型必须剔除：它是分类器，不是对话模型，路由过去会被下游入口的协议守卫拒绝。
func autoCandidates(pool []model.Candidate) []model.Candidate {
	out := make([]model.Candidate, 0, len(pool))
	for _, c := range pool {
		if provider.IsDecisionModelType(c.ModelType) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// isAutoModelName 判定请求里的模型名是否为 auto 伪模型（大小写不敏感）。
func isAutoModelName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), autoModelName)
}

// estimateTokens 是请求规模的粗略估算：按 UTF-8 字符数除以 4。
//
// 仓库没有 tokenizer，也不引入依赖；这个比例只用于「要不要短路」的阈值判断，
// 不影响记账（记账一律采信上游返回的 usage）。集中在此处，日后替换只需改这一处。
func estimateTokens(text string) int {
	n := len([]rune(text))
	return (n + 3) / 4
}

// extractUserText 抽出用于估算与题面的对话文本。
//
// 返回 state 是喂给决策模型的文本（系统提示 + 最近对话），lastUser 是最后一条用户消息，
// total 是全部文本的字符数。
//
// 三条入口的任务背景位置不同，必须都覆盖：chat 在 messages 里（role=system/developer），
// Anthropic 在顶层 system（字符串或 content 块数组），Responses 在顶层 instructions。
// 只看 messages 会让非 chat 入口在「看不见角色设定」的情况下被迫路由——
// 角色设定恰恰决定任务难度（例如「你是资深代码审查员」加一句「谢谢」）。
func extractUserText(reqBody map[string]any) (state, lastUser string, total int) {
	var parts []string

	// 顶层系统提示（Responses 的 instructions、Anthropic 的 system）。
	if s, ok := reqBody["instructions"].(string); ok && s != "" {
		parts = append(parts, s)
		total += len([]rune(s))
	}
	if sys, ok := reqBody["system"]; ok {
		if s := blockText(sys); s != "" {
			parts = append(parts, s)
			total += len([]rune(s))
		}
	}

	msgs, _ := reqBody["messages"].([]any)
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		text := blockText(msg["content"])
		total += len([]rune(text))
		switch role, _ := msg["role"].(string); role {
		case "system", "developer":
			if text != "" {
				parts = append(parts, text)
			}
		case "user":
			lastUser = text
			if text != "" {
				parts = append(parts, text)
			}
		}
	}

	return strings.Join(parts, "\n\n"), lastUser, total
}

// blockText 把 content / system 的两种形态（字符串、content 块数组）都抽成纯文本。
func blockText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, block := range t {
			bm, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if s, isStr := bm["text"].(string); isStr {
				b.WriteString(s)
			}
		}
		return b.String()
	}
	return ""
}

// hasComplexShape 报告请求中是否存在「不适合短路」的形态：
// 工具调用、图片等多模态输入、或结构化输出要求。
func hasComplexShape(reqBody map[string]any) bool {
	if _, ok := reqBody["tools"]; ok {
		return true
	}
	if _, ok := reqBody["tool_choice"]; ok {
		return true
	}
	if _, ok := reqBody["functions"]; ok {
		return true
	}
	if tc, ok := reqBody["response_format"]; ok {
		if m, isMap := tc.(map[string]any); isMap && len(m) > 0 {
			return true
		}
	}
	// 多模态 content：任何一条消息的 content 是数组即视为复杂。
	msgs, ok := reqBody["messages"].([]any)
	if !ok {
		return false
	}
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, isArr := msg["content"].([]any); isArr {
			return true
		}
	}
	return false
}

// simpleSignals 是「确实简单」的本地信号（中英双语）。
//
// 这是刻意保守的一组：命中才认为任务简单。litellm 明确警示过——什么都不命中的 prompt
// 会被「默认」判为最简档而非「有证据地」判为最简档，约一半通用流量属此类，
// 正是要靠决策模型兜住的那一半。所以这里要求「未见信号就不短路」。
var simpleSignals = []string{
	// 中文
	"翻译", "润色", "改写", "总结", "摘要", "格式化", "提取", "抽取", "分类", "打标签",
	"排序", "转成", "简称", "缩写", "纠错", "改错", "校对",
	// English
	"translate", "proofread", "rewrite", "summarize", "summarise", "format",
	"extract", "classify", "categorize", "label", "sort", "convert", "fix grammar",
	"shorten", "abbreviate", "spell check",
}

// hasSimpleSignal 报告文本是否命中任一条简单信号。
func hasSimpleSignal(text string) bool {
	lower := strings.ToLower(text)
	for _, s := range simpleSignals {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// buildChoices 把候选列表转成决策模型的 choice 选项（值 = 唯一候选键，描述 = 价格档与来源）。
//
// 只写相对档位、不写绝对数字：决策模型对绝对单价没有校准能力，
// 但它能把「最便宜/中等/最贵」和「这次任务够不够用」直接比较。
// 选项顺序与 priced 一致（已按价格升序）。
func buildChoices(priced []pricedCandidate) []provider.DecisionOption {
	keys := disambiguateKeys(priced)
	var known []int64
	for _, p := range priced {
		if p.ok {
			known = append(known, p.micros)
		}
	}
	cheapest, priciest := int64(-1), int64(-1)
	for _, v := range known {
		if cheapest < 0 || v < cheapest {
			cheapest = v
		}
		if v > priciest {
			priciest = v
		}
	}
	out := make([]provider.DecisionOption, 0, len(priced))
	for _, p := range priced {
		out = append(out, provider.DecisionOption{
			Value:       keys[p.candidate.AIModelID],
			Description: describeCandidate(p, cheapest, priciest),
		})
	}
	return out
}

// candidateKey 是决策模型回传时用于识别候选的键，必须在候选集内**唯一**。
//
// 优先用 model_id（决策模型对语义名更稳）；同名绑定（同一 model_id 来自不同 provider）
// 追加 provider 消歧。绝不用 ModelName：它只是展示名，允许重复，用作 map 键会让同名
// 候选互相覆盖，决策模型也就永远选不到被覆盖的那个。
func candidateKey(c model.Candidate) string {
	base := c.ModelID
	if base == "" {
		base = c.ModelName
	}
	if base == "" {
		return fmt.Sprintf("model-%d", c.AIModelID)
	}
	return base
}

// disambiguateKeys 在发现键冲突时为冲突项追加 provider 后缀，保证每个候选都能被单独选中。
func disambiguateKeys(priced []pricedCandidate) map[uint]string {
	count := make(map[string]int, len(priced))
	for _, p := range priced {
		count[candidateKey(p.candidate)]++
	}
	out := make(map[uint]string, len(priced))
	for _, p := range priced {
		k := candidateKey(p.candidate)
		if count[k] > 1 {
			k = k + "@" + p.candidate.Provider
		}
		out[p.candidate.AIModelID] = k
	}
	return out
}

// describeCandidate 生成单个候选的描述：价格档 + 上下文/能力提示。
func describeCandidate(p pricedCandidate, cheapest, priciest int64) string {
	var b strings.Builder
	switch {
	case !p.ok:
		// 价格未知优先判定：否则「只有一个候选有价」时会被误描述成「唯一已定价候选」，
		// 决策模型就看不到「其余候选价格不明」这一信息。
		b.WriteString("price unknown")
	case cheapest >= 0 && cheapest == priciest:
		b.WriteString("only priced candidate")
	case p.micros == cheapest:
		b.WriteString("cheapest available")
	case p.micros == priciest:
		b.WriteString("most expensive")
	default:
		b.WriteString("medium price")
	}
	if p.candidate.Provider != "" {
		b.WriteString("; provider " + p.candidate.Provider)
	}
	return b.String()
}

// pickCheapest 返回已定价候选中比价标量最小者；无已定价候选时退回第一个（按 sort_order）。
//
// 这是短路的落点：本地已判定任务简单，选最便宜的即可，不需要问决策模型。
func pickCheapest(priced []pricedCandidate) model.Candidate {
	var best *pricedCandidate
	for i := range priced {
		p := &priced[i]
		if !p.ok {
			continue
		}
		if best == nil || p.micros < best.micros {
			best = p
		}
	}
	if best != nil {
		return best.candidate
	}
	return priced[0].candidate
}

// resolveAutoChoice 把决策模型返回的候选键解析回候选；未命中返回 ok=false。
//
// 以随题面下发的 options 为准（键已消歧），再退化到裸 model_id：少数模型会回
// 更短的语义名，只要在候选集内唯一仍可接受。
func resolveAutoChoice(priced []pricedCandidate, options []provider.DecisionOption, choice string) (model.Candidate, bool) {
	choice = strings.TrimSpace(choice)
	if choice == "" {
		return model.Candidate{}, false
	}
	// 第一轮：按题面下发的键精确匹配（消歧后的键优先）。
	keys := disambiguateKeys(priced)
	for i, o := range options {
		if strings.EqualFold(o.Value, choice) && i < len(priced) {
			return priced[i].candidate, true
		}
	}
	for _, p := range priced {
		if strings.EqualFold(keys[p.candidate.AIModelID], choice) {
			return p.candidate, true
		}
	}
	// 第二轮：裸 model_id，仅在候选集内唯一时采信。
	var target model.Candidate
	hits := 0
	for _, p := range priced {
		if strings.EqualFold(p.candidate.ModelID, choice) {
			target = p.candidate
			hits++
		}
	}
	if hits == 1 {
		return target, true
	}
	return model.Candidate{}, false
}

// marshalAutoState 把对话文本编成决策协议的 state（JSON 字符串）。
func marshalAutoState(text string) json.RawMessage {
	b, _ := json.Marshal(text)
	return json.RawMessage(b)
}

// autoNow 是时钟注入点：取价与熔断都按它判定当前时刻，测试可替换。
var autoNow = time.Now

// requestShape 把某个入口的请求体归一成策略可见的形态。
//
// 三条入口的字段布局不同（chat 用 messages，Anthropic 用顶层 system，
// Responses 用 instructions），这里一次性抹平，策略只面对 RequestShape。
func requestShape(reqBody map[string]any) RequestShape {
	_, lastUser, total := extractUserText(reqBody)
	msgCount := 0
	if msgs, ok := reqBody["messages"].([]any); ok {
		msgCount = len(msgs)
	}
	return RequestShape{
		SystemPrompt:    systemPromptText(reqBody),
		LastUser:        lastUser,
		TotalChars:      total,
		MessageCount:    msgCount,
		HasTools:        hasTools(reqBody),
		HasComplexShape: hasComplexShape(reqBody),
	}
}

// systemPromptText 抽取系统提示。
//
// 两个来源都要看：Responses 的 instructions 与 Anthropic 的顶层 system 是入口原生位置；
// 而 Anthropic 入口会在路由之前把 system 转成 messages 里的 role=system 条目
// （anthropicRequestToOpenAI），所以 messages 里的 system/developer 也算系统提示。
func systemPromptText(reqBody map[string]any) string {
	if s, ok := reqBody["instructions"].(string); ok && s != "" {
		return s
	}
	if sys, ok := reqBody["system"]; ok {
		if s := blockText(sys); s != "" {
			return s
		}
	}
	var parts []string
	if msgs, ok := reqBody["messages"].([]any); ok {
		for _, raw := range msgs {
			msg, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			if role != "system" && role != "developer" {
				continue
			}
			if s := blockText(msg["content"]); s != "" {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// hasTools 报告请求是否携带工具定义。
func hasTools(reqBody map[string]any) bool {
	for _, k := range []string{"tools", "tool_choice", "functions", "function_call"} {
		if _, ok := reqBody[k]; ok {
			return true
		}
	}
	return false
}
