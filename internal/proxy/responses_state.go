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

// 本文件实现 POST /v1/responses 的有状态路径（design §5.6「请求侧行为」表）：
//   - store 取舍：缺省 true；站点开关关闭时全部按 false 处理、显式 true 一律 400；
//   - previous_response_id：取父行 items 作为历史前缀，拼本轮 input；
//   - conversation：取会话最新 completed 行的 items 作为历史前缀，新行写入同一会话；
//   - 归属校验：按 owner_user_id 判定，失配与「不存在」返回**同一个** 404（防存在性枚举）；
//   - 落库时机：响应完成后（流式则在流结束后），一次性写入终态 items 与完整响应体。
//
// 历史拼接的位置是「§5.4.1 第 3 步」：在 prepareResponsesTokens（第 4 步）之前。
// 原因有两条：
//   1. 第 4 步的 enforceMaxTokens 靠请求体形态判定协议（有 max_output_tokens、无 messages），
//      历史作为 input item 数组拼进去不会改变形态；但若把拼接放到转换之后，
//      拼的就不再是 Responses 形态，形状也就没人保证了；
//   2. 第 5 步的配额/模型解析必须读到最终要转发的请求（含历史），否则「先放行再放大请求」。
// 结论：拼接只动 input，不动 max_output_tokens / messages，顺序契约不被破坏。
//
// 装配（站点开关、清理循环）在 responses_retention.go。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"aigate/internal/httpx"
	"aigate/internal/model"
	"aigate/internal/provider"

	"gorm.io/gorm"
)

// responsesStatefulInput 是一次有状态请求在第 3 步解析出的全部上下文。
type responsesStatefulInput struct {
	// store 表示本轮是否落库（已综合客户端取值、站点开关与是否是可续接的会话）。
	store bool
	// conversationID 是本轮要写入的会话 ID（空串 = 未加入会话）。
	conversationID string
	// previousID 是本轮基于的上一轮 ID（空串 = 对话起点）。
	previousID string
	// history 是父行 / 会话最新行的累积 items（nil = 无历史）。
	history []any
}

// prepareResponsesState 是 §5.4.1 第 3 步：解析并应用有状态字段。
//
// 成功时返回本轮上下文；失败时已写出响应（400/404）并返回 ok=false，调用方直接返回。
// 本轮请求体在成功时会被就地改写：input 前置父行 items，store 归一为布尔值。
func (h *Handler) prepareResponsesState(
	w http.ResponseWriter,
	r *http.Request,
	key *model.APIKey,
	reqBody map[string]any,
) (*responsesStatefulInput, bool) {
	// background 属于 Task 9；本轮出现即 400（与站点开关无关：它根本还没被实现）。
	if v, exists := reqBody["background"]; exists && v != nil {
		httpx.WriteError(w, http.StatusBadRequest, msgResponsesBackground)
		return nil, false
	}
	// store 的缺省值是 true（OpenAI 语义），因此这里用「是否显式给了字段」而不是「取值是否为真」
	// 判定，否则站点关闭存储时，任何不带 store 的请求都会被当成显式 store:true 而 400。
	explicitStore, hasStore := responsesStoreFlag(reqBody)
	store := true
	requestedStore := false
	if hasStore && explicitStore != nil {
		b, ok := explicitStore.(bool)
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, msgResponsesInvalidStore)
			return nil, false
		}
		store = b
		requestedStore = b
	}
	previousID, conversationID := responsesContinuationFields(reqBody)
	stateful := previousID != "" || conversationID != ""

	// 站点开关一票否决：有状态能力整体不可用，显式开启存储也明确拒绝（不静默改写）。
	// 判定顺序：先看有状态字段（它们在任何取值下都不可用），再看显式 store:true。
	// store 的**类型**合法性已在上面判过，因此站点关闭时 "store":"yes" 报的仍是
	// invalid store 而不是「站点已关闭存储」——参数非法与站点策略是两件事，不能互相掩盖。
	if !h.allowResponseStorage {
		if stateful {
			httpx.WriteError(w, http.StatusBadRequest, msgResponsesStatefulDisabled)
			return nil, false
		}
		// 只有**显式**要求存储才拒绝；缺省的 store 缺省值虽是 true，但那是站点默认，
		// 不是客户端的显式诉求——这里恰恰是要把站点默认压成 false。
		if requestedStore {
			httpx.WriteError(w, http.StatusBadRequest, msgResponsesStorageDisabled)
			return nil, false
		}
		return &responsesStatefulInput{store: false}, true
	}
	// 两者互斥（OpenAI 语义）；先于任何查库判定，避免把「参数冲突」伪装成 404。
	if previousID != "" && conversationID != "" {
		httpx.WriteError(w, http.StatusBadRequest, msgResponsesContinuationConflict)
		return nil, false
	}

	state := &responsesStatefulInput{store: store, conversationID: conversationID, previousID: previousID}
	if stateful {
		parent, ok := h.loadResponsesParent(w, r, key, state)
		if !ok {
			return nil, false
		}
		if parent != nil {
			if history, ok := decodeResponsesItems(parent.Items); ok {
				state.history = history
			}
		}
	}
	applyResponsesHistory(reqBody, state.history)
	reqBody["store"] = state.store
	return state, true
}

// responsesStoreFlag 返回 store 字段的原始取值与是否显式出现。
// 缺省 / null 视为 true（OpenAI 语义）；**是不是显式给的**由第二个返回值区分，
// 站点开关关闭时要靠它区分「客户端要求存储」与「客户端什么都没说」。
// 非布尔值（整数 0/1、字符串等）不猜测约定，由调用方按 400 拒绝。
func responsesStoreFlag(reqBody map[string]any) (any, bool) {
	raw, exists := reqBody["store"]
	if !exists {
		return nil, false
	}
	return raw, true
}

// responsesContinuationFields 取 previous_response_id / conversation。
// conversation 既可以是字符串，也可以是 {"id": "..."} 对象（两种 OpenAI 客户端形态都接受）；
// 其余类型按「未设置」处理——非字符串值不可能命中任何一行，最终落到与不存在相同的 404。
func responsesContinuationFields(reqBody map[string]any) (previousID, conversationID string) {
	previousID, _ = reqBody["previous_response_id"].(string)
	previousID = strings.TrimSpace(previousID)
	switch v := reqBody["conversation"].(type) {
	case string:
		conversationID = strings.TrimSpace(v)
	case map[string]any:
		conversationID, _ = v["id"].(string)
		conversationID = strings.TrimSpace(conversationID)
	}
	return previousID, conversationID
}

// loadResponsesParent 取历史父行：previous_response_id 直接点查；conversation 取会话最新完成行。
//
// 归属校验只看 owner_user_id（owner_api_key_id 只记录不校验）：同一用户的另一把密钥可以续接，
// 不同用户一律按「不存在」返回同一个 404——文案、状态码与响应体逐字一致，不给枚举留任何缝隙。
// 会话没有可用行时返回 (nil, true)：此时按新会话处理（本任务不提供建会话端点）。
//
// 返回 (nil, false) 表示已写出 404/500，调用方直接返回。
func (h *Handler) loadResponsesParent(
	w http.ResponseWriter,
	r *http.Request,
	key *model.APIKey,
	state *responsesStatefulInput,
) (*model.ResponseState, bool) {
	ctx := r.Context()
	var (
		row *model.ResponseState
		err error
	)
	if state.previousID != "" {
		row, err = h.store.GetResponseState(ctx, state.previousID)
	} else {
		row, err = h.store.LatestResponseStateInConversation(ctx, state.conversationID)
	}
	switch {
	case err == nil:
	case errors.Is(err, model.ErrResponseStateNotFound):
		if state.previousID != "" {
			httpx.WriteError(w, http.StatusNotFound, msgResponsesNotFound)
			return nil, false
		}
		return nil, true // 会话尚无可用行：按新会话处理
	default:
		slog.Error("load response state failed", "error", err, "previous_response_id", state.previousID,
			"conversation", state.conversationID)
		httpx.WriteError(w, http.StatusInternalServerError, msgInternalError)
		return nil, false
	}

	// 归属校验：失配与不存在走同一个出口（同一个文案常量）。
	// 日志里也不记录「该行属于谁」，避免运维侧出现可被社工的影子信息。
	if row.OwnerUserID != key.UserID {
		slog.Warn("response state owner mismatch", "response_id", row.ID, "api_key_id", key.ID)
		httpx.WriteError(w, http.StatusNotFound, msgResponsesNotFound)
		return nil, false
	}
	return row, true
}

// decodeResponsesItems 解析累积快照；损坏时返回 ok=false 并由调用方按「无历史」处理
// （历史只影响请求形状，把它降级成新对话比让请求整条失败更可预测）。
func decodeResponsesItems(raw []byte) ([]any, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false
	}
	var items []any
	if err := json.Unmarshal(raw, &items); err != nil {
		slog.Warn("decode stored response items failed", "error", err)
		return nil, false
	}
	// JSON null 解码成功但语义上是「没有历史」：与解码失败同口径返回 false，
	// 避免把「解码成功」与「有历史」混为一谈（调用方只关心后者）。
	if items == nil {
		return nil, false
	}
	return items, true
}

// applyResponsesHistory 把历史 items 前置于本轮 input（Responses 语义下历史与 input 同构，
// 都是 item 数组，转换器 responsesItemsToMessages 会把两段按顺序展开成 messages）。
//
// 不同 input 形状的处理：
//   - 缺省 / null / 空数组：本轮只有历史，原样写入历史（空历史时保持原样，由转换器按
//     invalid input 拒绝一个既无历史也无输入的请求）；
//   - 字符串：等价于一条 user message，包成 {"role":"user","content":...} 后接在历史之后；
//   - 数组：直接拼成「历史 + 本轮 items」；
//   - 其它（对象/数字）：保持原样不动——不是合法 input，交给转换器按 invalid input 拒绝。
//
// 注意：只动 input，绝不动 max_output_tokens / messages（§5.4.1 第 4 步靠请求体形态判定协议）。
func applyResponsesHistory(reqBody map[string]any, history []any) {
	if len(history) == 0 {
		return
	}
	combined := make([]any, 0, len(history)+1)
	combined = append(combined, history...)
	switch input := reqBody["input"].(type) {
	case nil:
	case string:
		combined = append(combined, map[string]any{"role": "user", "content": input})
	case []any:
		combined = append(combined, input...)
	default:
		return
	}
	reqBody["input"] = combined
}

// buildResponsesItemsSnapshot 生成本行的累积 items：原有历史 + 本轮 input（不含 instructions）
// + 本轮 output items，供下一轮续接。
//
// 不含 instructions 的理由：它在 Responses 协议里是「本请求的系统提示」，语义上不属于对话历史；
// 转换器每轮都会把它作为首条 system 消息重新发出。若存进历史，续接时会重复注入上一轮的 system。
func buildResponsesItemsSnapshot(history []any, reqBody map[string]any, output []any) []any {
	snapshot := make([]any, 0, len(history)+len(output)+1)
	snapshot = append(snapshot, history...)
	switch input := reqBody["input"].(type) {
	case string:
		snapshot = append(snapshot, map[string]any{"role": "user", "content": input})
	case []any:
		snapshot = append(snapshot, input...)
	}
	snapshot = append(snapshot, output...)
	return snapshot
}

// responsesStatusOf 把响应体的 status 映射为行状态：只保留终态；非终态（含缺省）
// 一律记为 completed——本路径落库时响应确实已经结束，带 queued/in_progress 的响应体
// 只可能来自上游的异常回包，按失败处理才是安全选择。
func responsesStatusOf(resp map[string]any) string {
	switch status, _ := resp["status"].(string); status {
	case model.ResponseStateStatusIncomplete:
		return model.ResponseStateStatusIncomplete
	case model.ResponseStateStatusFailed, model.ResponseStateStatusCancelled:
		return model.ResponseStateStatusFailed
	default:
		return model.ResponseStateStatusCompleted
	}
}

// responsesUsageFields 从响应体的 usage 抽取四列用量（缺失时全 0）。
func responsesUsageFields(resp map[string]any) (input, output, total, reasoning int) {
	usage, ok := resp["usage"].(map[string]any)
	if !ok {
		return 0, 0, 0, 0
	}
	input = responsesInt(usage, "input_tokens")
	output = responsesInt(usage, "output_tokens")
	total = responsesInt(usage, "total_tokens")
	if details, ok := usage["output_tokens_details"].(map[string]any); ok {
		reasoning = responsesInt(details, "reasoning_tokens")
	}
	return input, output, total, reasoning
}

// persistResponsesState 落一行已完成（或已完成但被截断）的会话状态。
//
// 失败只记 Error 日志、不回给调用方：此时响应的正文已经写出（甚至在流式路径上早已落盘），
// 没有可回退的状态码；下一轮续接会以 404 结束，客户端据此重试即可。
func (h *Handler) persistResponsesState(
	ctx context.Context,
	key *model.APIKey,
	respID string,
	state *responsesStatefulInput,
	requestedModel string,
	items []any,
	body []byte,
) {
	resp := map[string]any{}
	if err := json.Unmarshal(body, &resp); err != nil || resp == nil {
		resp = map[string]any{}
	}
	snapshot, err := json.Marshal(items)
	if err != nil {
		slog.Error("marshal response state items failed", "error", err, "response_id", respID)
		return
	}
	inputTokens, outputTokens, totalTokens, reasoningTokens := responsesUsageFields(resp)
	row := &model.ResponseState{
		ID:                 respID,
		ConversationID:     state.conversationID,
		OwnerUserID:        key.UserID,
		OwnerAPIKeyID:      key.ID,
		RequestModel:       requestedModel,
		PreviousResponseID: state.previousID,
		Status:             responsesStatusOf(resp),
		Items:              snapshot,
		ResponseBody:       body,
		InputTokens:        inputTokens,
		OutputTokens:       outputTokens,
		TotalTokens:        totalTokens,
		ReasoningTokens:    reasoningTokens,
	}
	err = h.store.CreateResponseState(ctx, row)
	switch {
	case err == nil:
	case errors.Is(err, gorm.ErrDuplicatedKey):
		// 【依赖】本判定成立的前提是 model.Open 开启了 gorm.Config.TranslateError: true。
		// 关掉它，驱动只会回裸的 "UNIQUE constraint failed: ..." 文本错误，errors.Is 恒为 false，
		// 这个 case 会**静默失效**——重复落库退回 ERROR 分支（不丢数据、也不报错，但可观测性变差）。
		// 改动 model.Open 的该开关或升级 GORM 时，请同步复核这里的判定是否仍然成立。
		//
		// 同一 ID 重复落库（重试、或同一响应被两条路径各写一次）：内容早已存在，
		// 不是故障，降到 Warn——但也不能静默吞掉，重复本身仍是需要观测的现象。
		slog.Warn("response state already stored, skipping duplicate", "response_id", respID,
			"model", requestedModel)
	default:
		slog.Error("persist response state failed", "error", err, "response_id", respID, "model", requestedModel)
	}
}

// persistResponsesStreamState 是流式路径的落库包装：终态事件（response.completed /
// incomplete / failed）携带完整 response 对象，以其为响应体；没有终态事件（上游中断）
// 时不落库——半截对话不应成为下一轮的父行，否则续接会重放客户端从未真正收到的内容。
// 完整论证（为什么这是有意的不对称、为什么不能改回「落库合成终态」）见
// persistResponsesStream 的函数注释，改动此处前请先读那一段。
func (h *Handler) persistResponsesStreamState(
	ctx context.Context,
	key *model.APIKey,
	respID string,
	state *responsesStatefulInput,
	requestedModel string,
	items []any,
	terminal []byte,
) {
	if len(terminal) == 0 {
		return
	}
	var ev map[string]any
	if err := json.Unmarshal(terminal, &ev); err != nil {
		slog.Warn("decode terminal responses event failed", "error", err, "response_id", respID)
		return
	}
	raw, exists := ev["response"]
	if !exists {
		raw = ev
	}
	body, err := json.Marshal(raw)
	if err != nil {
		slog.Error("marshal terminal responses body failed", "error", err, "response_id", respID)
		return
	}
	h.persistResponsesState(ctx, key, respID, state, requestedModel, items, body)
}

// buildResponsesBody 是直通路径的响应改写：把上游 id 换成网关 id、回显客户端请求名与 store，
// 并返回解析结果供落库（id 与 store 已写回 body，两者保持一致）。
func buildResponsesBody(respBody []byte, respID, requestedModel string, store bool) ([]byte, map[string]any) {
	var resp map[string]any
	if err := json.Unmarshal(respBody, &resp); err != nil || resp == nil {
		return respBody, nil
	}
	resp["id"] = respID
	if _, exists := resp["model"]; exists && requestedModel != "" {
		resp["model"] = requestedModel
	}
	resp["store"] = store
	out, err := json.Marshal(resp)
	if err != nil {
		return respBody, nil
	}
	return out, resp
}

// normalizeResponsesStore 给转换路径的响应补上 store 回显（该路径的响应体由网关构造，
// 不含 store 字段）。解析失败时原样返回。
func normalizeResponsesStore(respBody []byte, store bool) []byte {
	var resp map[string]any
	if err := json.Unmarshal(respBody, &resp); err != nil || resp == nil {
		return respBody
	}
	resp["store"] = store
	out, err := json.Marshal(resp)
	if err != nil {
		return respBody
	}
	return out
}

// responsesUsageFromBody 解析响应体里的 usage，用于用量记账（直通路径写回 store 之后调用）。
func responsesUsageFromBody(body []byte) *provider.Usage {
	return providerUsageFromResponsesBody(body)
}

// responsesOutputItems 取响应体的 output items；缺失或类型不符时返回空切片。
func responsesOutputItems(resp map[string]any) []any {
	output, _ := resp["output"].([]any)
	return output
}
