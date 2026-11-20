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

package proxy_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aigate/internal/model"
	"aigate/internal/proxy"

	"gorm.io/gorm"
)

// 本文件是 POST /v1/responses 有状态能力（Task 8）的黑盒用例：
// store 落库与回显、previous_response_id 续接、conversation 会话、站点开关与保留期。
//
// 注意：上一轮历史经「input item 数组」拼进本轮请求（Responses 语义下历史与 input 同构），
// 因此断言一律落在「上游收到的 messages 包含了第 N 轮内容」上，而不是某个专有字段名。

// responsesNotFoundMessage 是「会话行不存在」与「不属于本用户」共用的 404 文案（避免存在性枚举）。
//
// 三处文案与生产代码的常量逐字一致（生产侧常量未导出，黑盒用例按字面断言）。
const (
	responsesNotFoundMessage          = "previous response not found"
	responsesStorageDisabledMessage   = "response storage is disabled on this gateway"
	responsesMutuallyExclusiveMessage = "previous_response_id and conversation are mutually exclusive"
	responsesStatefulDisabledMessage  = "response storage is disabled on this gateway, " +
		"previous_response_id and conversation are unavailable"
)

// waitStoredResponses 等待会话状态行落库完成（落库发生在响应写出之后，与客户端收包存在竞争）。
func waitStoredResponses(t *testing.T, e *env, want int) []model.ResponseState {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows := storedResponseRows(t, e)
		if want <= 0 || len(rows) >= want {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("response_states rows = %d, want %d (timed out)", len(rows), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// storedResponseRows 从表侧读取全部会话状态行（按创建时间升序）。
func storedResponseRows(t *testing.T, e *env) []model.ResponseState {
	t.Helper()
	var rows []model.ResponseState
	if err := openReadDB(t, e.dsn).Model(&model.ResponseState{}).
		Order("created_at ASC, id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("read response_states: %v", err)
	}
	return rows
}

// upstreamMessagesJSON 取上游收到的 messages 的 JSON 文本，用于断言历史内容是否被拼入。
func upstreamMessagesJSON(t *testing.T, body map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(body["messages"])
	if err != nil {
		t.Fatalf("marshal upstream messages: %v", err)
	}
	return string(raw)
}

// assertUpstreamHasContent 断言上游收到的 messages 中包含 want 这段文本。
func assertUpstreamHasContent(t *testing.T, body map[string]any, want, ctxDesc string) {
	t.Helper()
	if got := upstreamMessagesJSON(t, body); !strings.Contains(got, want) {
		t.Fatalf("%s: upstream messages %s does not contain %q", ctxDesc, got, want)
	}
}

// responsesAnswerBody 让 mock chat 上游回一句可判别的话（历史里据此确认第 N 轮内容）。
func responsesAnswerBody(text string) string {
	return `{"id":"chatcmpl-1","object":"chat.completion","model":"mock-a","choices":[` +
		`{"index":0,"message":{"role":"assistant","content":"` + text + `"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
}

// newSteerableEnv 用可配置的 Handler 重建被测网关（供站点开关用例使用）：
// 复用 newEnv 的隔离 store / usage / 上游，只替换对外暴露的 HTTP 服务。
func newSteerableEnv(
	t *testing.T,
	respond func(w http.ResponseWriter, r *http.Request),
	configure func(*proxy.Handler),
) *env {
	t.Helper()
	e := newEnv(t, respond)
	// 关掉 newEnv 注册的服务，改用它自己的 Handler 重建（同一 store / usage / 上游）。
	e.server.Close()
	h := proxy.NewHandler(e.store, e.usage, 429)
	configure(h)
	mux := http.NewServeMux()
	h.Register(mux)
	e.server = httptest.NewServer(mux)
	t.Cleanup(e.server.Close)
	e.client = e.server.Client()
	return e
}

// ---------------------------------------------------------------------------
// 1. store 缺省为 true（★ Review Focus 1）
// ---------------------------------------------------------------------------

// TestResponseState_StoreDefaultIsTrue：请求体不含 store 字段时按 OpenAI 语义缺省 true——
// 必须落库一行，且响应回显 "store": true。
func TestResponseState_StoreDefaultIsTrue(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postResponses(t, token, `{"model":"mock-a","input":"hello default store"}`)
	assertStatus(t, resp, http.StatusOK)
	out := decodeResponsesBody(t, readBody(t, resp))

	id, _ := out["id"].(string)
	if !strings.HasPrefix(id, "resp_") {
		t.Fatalf("id = %q, want a resp_ id", id)
	}
	if got, ok := out["store"].(bool); !ok || !got {
		t.Fatalf("response.store = %v (present=%v), want true", out["store"], ok)
	}

	rows := waitStoredResponses(t, e, 1)
	if len(rows) != 1 {
		t.Fatalf("response_states rows = %d, want 1 (default store must persist)", len(rows))
	}
	if rows[0].ID != id {
		t.Fatalf("stored row id = %q, want the response id %q", rows[0].ID, id)
	}
	if rows[0].OwnerUserID != e.user.ID {
		t.Fatalf("owner_user_id = %d, want %d", rows[0].OwnerUserID, e.user.ID)
	}
	if rows[0].Status != model.ResponseStateStatusCompleted {
		t.Fatalf("status = %q, want %q", rows[0].Status, model.ResponseStateStatusCompleted)
	}
	if len(rows[0].Items) == 0 || !strings.Contains(string(rows[0].Items), "hello default store") {
		t.Fatalf("items snapshot must contain the request input, got %s", rows[0].Items)
	}
	if !strings.Contains(string(rows[0].ResponseBody), id) {
		t.Fatalf("response_body must be the full response (id %q), got %s", id, rows[0].ResponseBody)
	}
}

// TestResponseState_StoreFalseDoesNotPersist：store:false 不落库，回显 false。
func TestResponseState_StoreFalseDoesNotPersist(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postResponses(t, token, `{"model":"mock-a","input":"hi","store":false}`)
	assertStatus(t, resp, http.StatusOK)
	out := decodeResponsesBody(t, readBody(t, resp))
	if got, ok := out["store"].(bool); !ok || got {
		t.Fatalf("response.store = %v (present=%v), want false", out["store"], ok)
	}
	if rows := storedResponseRows(t, e); len(rows) != 0 {
		t.Fatalf("response_states rows = %d, want 0 for store:false", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 2. 站点级开关（★ Review Focus 1）
// ---------------------------------------------------------------------------

// TestResponseState_StorageDisabledByConfig：SetResponseStorage(false) 后
// 缺省请求按 store:false 处理（回显 false、不落库），显式 store:true 直接 400。
func TestResponseState_StorageDisabledByConfig(t *testing.T) {
	e := newSteerableEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody),
		func(h *proxy.Handler) {
			if err := h.SetResponseStorage(false); err != nil {
				t.Fatalf("SetResponseStorage(false): %v", err)
			}
		})
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	t.Run("default store becomes false and is not persisted", func(t *testing.T) {
		resp := e.postResponses(t, token, `{"model":"mock-a","input":"hi"}`)
		assertStatus(t, resp, http.StatusOK)
		out := decodeResponsesBody(t, readBody(t, resp))
		if got, ok := out["store"].(bool); !ok || got {
			t.Fatalf("response.store = %v (present=%v), want false when storage is disabled", out["store"], ok)
		}
		if rows := storedResponseRows(t, e); len(rows) != 0 {
			t.Fatalf("response_states rows = %d, want 0 when storage is disabled", len(rows))
		}
	})

	t.Run("explicit store:true is rejected with 400", func(t *testing.T) {
		resp := e.postResponses(t, token, `{"model":"mock-a","input":"hi","store":true}`)
		assertErrorResponse(t, resp, http.StatusBadRequest, responsesStorageDisabledMessage)
	})

	t.Run("stateful fields are rejected while disabled", func(t *testing.T) {
		for _, body := range []string{
			`{"model":"mock-a","input":"hi","previous_response_id":"resp_deadbeef"}`,
			`{"model":"mock-a","input":"hi","conversation":"conv_1"}`,
		} {
			resp := e.postResponses(t, token, body)
			assertErrorResponse(t, resp, http.StatusBadRequest, responsesStatefulDisabledMessage)
		}
	})

	if calls := e.rec.Calls(); calls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (only the accepted request may be forwarded)", calls)
	}
}

// TestResponseState_InvalidStoreRejected（M-2）：store 必须是布尔值。
// 与站点开关无关：非法类型永远是「参数非法」，不能被站点开关的文案掩盖。
func TestResponseState_InvalidStoreRejected(t *testing.T) {
	const invalidStoreMessage = "invalid store"
	for _, tc := range []struct {
		name  string
		store bool
	}{{"storage enabled", true}, {"storage disabled", false}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSteerableEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody),
				func(h *proxy.Handler) {
					if err := h.SetResponseStorage(tc.store); err != nil {
						t.Fatalf("SetResponseStorage: %v", err)
					}
				})
			m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
			_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

			for _, body := range []string{
				`{"model":"mock-a","input":"hi","store":"yes"}`,
				`{"model":"mock-a","input":"hi","store":1}`,
			} {
				resp := e.postResponses(t, token, body)
				assertErrorResponse(t, resp, http.StatusBadRequest, invalidStoreMessage)
			}
			if calls := e.rec.Calls(); calls != 0 {
				t.Fatalf("upstream calls = %d, want 0", calls)
			}
		})
	}
}

// TestResponseState_StorageNullIsDefault：JSON null 视同「未设置」（缺省 true），
// 站点关闭时按 store:false 处理而不是 400——null 不是客户端的显式诉求。
func TestResponseState_StorageNullIsDefault(t *testing.T) {
	e := newSteerableEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody),
		func(h *proxy.Handler) {
			if err := h.SetResponseStorage(false); err != nil {
				t.Fatalf("SetResponseStorage(false): %v", err)
			}
		})
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postResponses(t, token, `{"model":"mock-a","input":"hi","store":null}`)
	assertStatus(t, resp, http.StatusOK)
	out := decodeResponsesBody(t, readBody(t, resp))
	if got, ok := out["store"].(bool); !ok || got {
		t.Fatalf("response.store = %v (present=%v), want false", out["store"], ok)
	}
}

// ---------------------------------------------------------------------------
// 3. previous_response_id 续接
// ---------------------------------------------------------------------------

// TestResponseState_PreviousResponseIDContinues：第二轮带 previous_response_id 时，
// 上游收到的 messages 必须同时包含第一轮的 input 与第一轮的 assistant 输出。
func TestResponseState_PreviousResponseIDContinues(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesAnswerBody("first answer")))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	first := e.postResponses(t, token, `{"model":"mock-a","input":"turn one question"}`)
	assertStatus(t, first, http.StatusOK)
	firstID, _ := decodeResponsesBody(t, readBody(t, first))["id"].(string)
	if firstID == "" {
		t.Fatal("first response has no id")
	}

	second := e.postResponses(t, token,
		`{"model":"mock-a","input":"turn two question","previous_response_id":"`+firstID+`"}`)
	assertStatus(t, second, http.StatusOK)
	_ = readBody(t, second)
	up := waitUpstreamBody(t, e, 2)

	assertUpstreamHasContent(t, up, "turn one question", "second turn")
	assertUpstreamHasContent(t, up, "first answer", "second turn")
	assertUpstreamHasContent(t, up, "turn two question", "second turn")

	rows := waitStoredResponses(t, e, 2)
	if len(rows) != 2 {
		t.Fatalf("response_states rows = %d, want 2 (each stored turn)", len(rows))
	}
	if rows[1].PreviousResponseID != firstID {
		t.Fatalf("previous_response_id = %q, want %q", rows[1].PreviousResponseID, firstID)
	}
	if !strings.Contains(string(rows[1].Items), "turn one question") ||
		!strings.Contains(string(rows[1].Items), "turn two question") {
		t.Fatalf("second row must snapshot the whole conversation, got %s", rows[1].Items)
	}
}

// TestResponseState_PreviousIDWithStoreFalseStillReadsParent：store:false 时仍读父行拼历史，
// 但不产生新行（design §5.6「请求侧行为」表；与 OpenAI 语义一致）。
func TestResponseState_PreviousIDWithStoreFalseStillReadsParent(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesAnswerBody("first answer")))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	first := e.postResponses(t, token, `{"model":"mock-a","input":"turn one question"}`)
	assertStatus(t, first, http.StatusOK)
	firstID, _ := decodeResponsesBody(t, readBody(t, first))["id"].(string)
	waitStoredResponses(t, e, 1)

	second := e.postResponses(t, token,
		`{"model":"mock-a","store":false,"input":"turn two question","previous_response_id":"`+firstID+`"}`)
	assertStatus(t, second, http.StatusOK)
	out := decodeResponsesBody(t, readBody(t, second))
	if got, ok := out["store"].(bool); !ok || got {
		t.Fatalf("response.store = %v (present=%v), want false", out["store"], ok)
	}

	up := waitUpstreamBody(t, e, 2)
	assertUpstreamHasContent(t, up, "turn one question", "store:false continuation")

	if rows := storedResponseRows(t, e); len(rows) != 1 {
		t.Fatalf("response_states rows = %d, want 1 (store:false must not create a row)", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 4. conversation 会话
// ---------------------------------------------------------------------------

// TestResponseState_ConversationContinues：两轮共用同一 conversation 时，
// 第二轮带上第一轮的历史，且两行落在同一 conversation_id 上。
func TestResponseState_ConversationContinues(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesAnswerBody("first answer")))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	first := e.postResponses(t, token,
		`{"model":"mock-a","input":"conv one question","conversation":"conv_abc"}`)
	assertStatus(t, first, http.StatusOK)
	_ = readBody(t, first)

	second := e.postResponses(t, token,
		`{"model":"mock-a","input":"conv two question","conversation":"conv_abc"}`)
	assertStatus(t, second, http.StatusOK)
	_ = readBody(t, second)
	up := waitUpstreamBody(t, e, 2)

	assertUpstreamHasContent(t, up, "conv one question", "second conversation turn")
	assertUpstreamHasContent(t, up, "first answer", "second conversation turn")
	assertUpstreamHasContent(t, up, "conv two question", "second conversation turn")

	rows := waitStoredResponses(t, e, 2)
	if len(rows) != 2 {
		t.Fatalf("response_states rows = %d, want 2", len(rows))
	}
	for _, row := range rows {
		if row.ConversationID != "conv_abc" {
			t.Fatalf("conversation_id = %q, want %q", row.ConversationID, "conv_abc")
		}
	}
}

// ---------------------------------------------------------------------------
// 5. 互斥与 404
// ---------------------------------------------------------------------------

// TestResponseState_BothIDAndConversationRejected：两者同时出现 → 400（OpenAI 语义：互斥）。
func TestResponseState_BothIDAndConversationRejected(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postResponses(t, token,
		`{"model":"mock-a","input":"hi","previous_response_id":"resp_x","conversation":"conv_1"}`)
	assertErrorResponse(t, resp, http.StatusBadRequest, responsesMutuallyExclusiveMessage)

	if calls := e.rec.Calls(); calls != 0 {
		t.Fatalf("upstream calls = %d, want 0 (rejected request must not be forwarded)", calls)
	}
}

// TestResponseState_ExpiredIs404：行被按保留期清理后，再用该 id 必须 404。
func TestResponseState_ExpiredIs404(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	first := e.postResponses(t, token, `{"model":"mock-a","input":"old turn"}`)
	assertStatus(t, first, http.StatusOK)
	firstID, _ := decodeResponsesBody(t, readBody(t, first))["id"].(string)

	// 落库发生在响应写出之后：先等该行出现，再回拨创建时间。
	if _, err := e.store.GetResponseState(ctx(), firstID); err != nil {
		waitStoredResponses(t, e, 1)
	}

	// 把该行 CreatedAt 挪到保留期之外，再跑一次清理（口径与清理循环一致：保留 7 天）。
	readDB := openReadDB(t, e.dsn)
	createdAgo := time.Now().UTC().Add(-30 * 24 * time.Hour)
	if err := readDB.Model(&model.ResponseState{}).Where("id = ?", firstID).
		UpdateColumn("created_at", createdAgo).Error; err != nil {
		t.Fatalf("backdate created_at: %v", err)
	}
	pruned, err := e.store.PruneResponseStates(ctx(), time.Now().UTC().Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("prune response states: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned rows = %d, want 1", pruned)
	}
	if _, err := e.store.GetResponseState(ctx(), firstID); !errors.Is(err, model.ErrResponseStateNotFound) {
		t.Fatalf("GetResponseState after prune: err = %v, want ErrResponseStateNotFound", err)
	}
	// 表侧复查：被清理的行确实不在表里了（防止上面读到的只是另一条连接的旧快照）。
	waitStoredResponses(t, e, 0)
	if rows := storedResponseRows(t, e); len(rows) != 0 {
		t.Fatalf("response_states rows after prune = %d, want 0", len(rows))
	}

	// previous_response_id 点查已清理的行 → 404。
	resp := e.postResponses(t, token, `{"model":"mock-a","input":"hi","previous_response_id":"`+firstID+`"}`)
	assertErrorResponse(t, resp, http.StatusNotFound, responsesNotFoundMessage)
	// 会话没有可用行不等于错误：这是一段新会话，正常放行（本任务不提供建会话端点）。
	fresh := e.postResponses(t, token, `{"model":"mock-a","input":"hi","conversation":"conv-never-existed"}`)
	assertStatus(t, fresh, http.StatusOK)
	_ = readBody(t, fresh)
	if calls := e.rec.Calls(); calls != 2 {
		t.Fatalf("upstream calls = %d, want 2 (the 404 must not reach upstream)", calls)
	}
}

// TestResponseState_RetentionLoopPrunes 钉住装配层的清理循环本身：
// StartResponseRetention 必须**启动即清一次**（覆盖停机期间累积的旧行），
// 且保留期 ≤ 0 时不启动（= 永不清理）。
func TestResponseState_RetentionLoopPrunes(t *testing.T) {
	store, _ := newTestStore(t)
	usage := proxy.NewUsageWriter(store, 0)
	usage.Start()
	t.Cleanup(func() { usage.Stop(context.Background()) })

	seed := func(id string) {
		if err := store.CreateResponseState(ctx(), &model.ResponseState{
			ID:           id,
			Status:       model.ResponseStateStatusCompleted,
			CreatedAt:    time.Now().UTC().Add(-30 * 24 * time.Hour),
			UpdatedAt:    time.Now().UTC().Add(-30 * 24 * time.Hour),
			Items:        []byte("[]"),
			ResponseBody: []byte("{}"),
		}); err != nil {
			t.Fatalf("seed response state: %v", err)
		}
	}

	t.Run("retention wins", func(t *testing.T) {
		seed("resp_old_retention")
		h := proxy.NewHandler(store, usage, 429)
		h.StartResponseRetention(7 * 24 * time.Hour)
		t.Cleanup(h.StopResponseRetention)
		waitGone(t, store, "resp_old_retention")
	})

	t.Run("zero retention keeps rows", func(t *testing.T) {
		if _, err := store.PruneResponseStates(ctx(), time.Now().UTC().Add(time.Hour)); err != nil {
			t.Fatalf("clean leftovers: %v", err)
		}
		seed("resp_old_keep")
		h := proxy.NewHandler(store, usage, 429)
		h.StartResponseRetention(0)
		t.Cleanup(h.StopResponseRetention)
		time.Sleep(50 * time.Millisecond)
		if _, err := store.GetResponseState(ctx(), "resp_old_keep"); err != nil {
			t.Fatalf("row must survive when retention is 0: %v", err)
		}
	})
}

// TestResponseState_DuplicatePersistIsNotAnError（M-5）：同一 ID 重复落库（重试/并发写入）
// 不是故障——唯一键冲突应降级为 Warn，而不是让运维按 ERROR 排查一场并不存在的故障。
// 这里直接验证存储层行为：第二次 CreateResponseState 返回的错误能被 ErrDuplicatedKey 判定。
func TestResponseState_DuplicatePersistIsNotAnError(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody))
	row := &model.ResponseState{ID: "resp_dup_1", Status: model.ResponseStateStatusCompleted}
	if err := e.store.CreateResponseState(ctx(), row); err != nil {
		t.Fatalf("first create: %v", err)
	}
	err := e.store.CreateResponseState(ctx(), &model.ResponseState{
		ID: "resp_dup_1", Status: model.ResponseStateStatusCompleted,
	})
	if err == nil {
		t.Fatal("duplicate id must surface an error from the store")
	}
	// 生产装配（model.Open）与测试夹具（model.Open）同一条路径，故这里的判定口径即生产口径。
	// 【依赖】该判定依赖 model.Open 的 gorm.Config.TranslateError: true；关掉它，这里与
	// responses_state.go 的降级分支会同时失效——本用例是那条依赖的回归哨兵，不要删。
	if !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Fatalf("duplicate create error = %v, want it to satisfy errors.Is(gorm.ErrDuplicatedKey); "+
			"without TranslateError the caller cannot tell a benign duplicate from a real failure", err)
	}
}

// waitGone 轮询等待会话状态行消失（清理循环在独立 goroutine 里执行）。
func waitGone(t *testing.T, store *model.Store, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := store.GetResponseState(ctx(), id)
		if errors.Is(err, model.ErrResponseStateNotFound) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("response state %q was not pruned (err=%v)", id, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestResponseState_CrossUserIsolation（T6 推迟到本任务）：
// 用密钥 A 创建 response，再用**另一个用户**的密钥带该 previous_response_id 请求，
// 必须 404；且响应体与「id 根本不存在」逐字一致，不透露该 id 是否存在。
func TestResponseState_CrossUserIsolation(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, responsesOKChatBody))
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, tokenA := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	owner := e.postResponses(t, tokenA, `{"model":"mock-a","input":"secret question"}`)
	assertStatus(t, owner, http.StatusOK)
	ownerID, _ := decodeResponsesBody(t, readBody(t, owner))["id"].(string)

	userB := mustCreateUser(t, e.store, "intruder")
	_, tokenB := mustCreateKey(t, e.store, userB.ID, 0, primary(m.ID, 0))
	// 同一用户、另一把密钥：归属按 owner_user_id 校验，应当允许续接。
	_, tokenSameUserOtherKey := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	foreign := e.postResponses(t, tokenB,
		`{"model":"mock-a","input":"steal","previous_response_id":"`+ownerID+`"}`)
	foreignBody := readBody(t, foreign)
	if foreign.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-user status = %d, want 404 (body=%s)", foreign.StatusCode, foreignBody)
	}

	missing := e.postResponses(t, tokenB,
		`{"model":"mock-a","input":"steal","previous_response_id":"resp_00000000000000000000000000000000"}`)
	missingBody := readBody(t, missing)
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing-id status = %d, want 404 (body=%s)", missing.StatusCode, missingBody)
	}
	if foreignBody != missingBody {
		t.Fatalf("404 bodies must be indistinguishable:\n foreign=%s\n missing=%s", foreignBody, missingBody)
	}
	if strings.Contains(foreignBody, ownerID) {
		t.Fatalf("404 body leaks the response id %q: %s", ownerID, foreignBody)
	}

	// 同一用户的另一把密钥可以续接（归属校验只看 owner_user_id）。
	sameUser := e.postResponses(t, tokenSameUserOtherKey,
		`{"model":"mock-a","input":"second","previous_response_id":"`+ownerID+`"}`)
	assertStatus(t, sameUser, http.StatusOK)
	_ = readBody(t, sameUser)

	if calls := e.rec.Calls(); calls != 2 {
		t.Fatalf("upstream calls = %d, want 2 (owner + same-user continuation only)", calls)
	}
}

// ---------------------------------------------------------------------------
// 6. 流式落库
// ---------------------------------------------------------------------------

// TestResponseState_StreamingPersists：流式路径在流结束后落一行，items 含本轮问与答。
func TestResponseState_StreamingPersists(t *testing.T) {
	e := newEnv(t, respondChatSSE())
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postResponses(t, token, `{"model":"mock-a","stream":true,"input":"stream question"}`)
	assertStatus(t, resp, http.StatusOK)
	stream := readBody(t, resp)
	if !strings.Contains(stream, "response.completed") {
		t.Fatalf("stream missing response.completed: %s", stream)
	}

	rows := waitStoredResponses(t, e, 1)
	if len(rows) != 1 {
		t.Fatalf("response_states rows = %d, want 1", len(rows))
	}
	if rows[0].Status != model.ResponseStateStatusCompleted {
		t.Fatalf("status = %q, want completed", rows[0].Status)
	}
	items := string(rows[0].Items)
	if !strings.Contains(items, "stream question") || !strings.Contains(items, "Hello") {
		t.Fatalf("items snapshot must hold the turn's input and output, got %s", items)
	}
	if !strings.Contains(string(rows[0].ResponseBody), `"output"`) {
		t.Fatalf("response_body must be the terminal response object, got %s", rows[0].ResponseBody)
	}
	if rows[0].TotalTokens != 9 {
		t.Fatalf("total_tokens = %d, want 9", rows[0].TotalTokens)
	}
	_ = e.rec
}

// TestResponseState_StreamingPassthroughKeepsFirstTerminal（I-1）：
// 落库内容必须等于客户端所见。上游在终态之后再补一个内容不同的 response.completed 时，
// 客户端收到的是**第一个**终态事件，落库的 response_body 与 items 快照也必须取自那一个。
//
// 判别力：守卫（仅首个终态事件可赋值 + 消费后置 nil）被删掉时，落库会变成**最后一个**，
// 本用例打红。这条守卫同时是异常上游「用后续终态改写下一轮重放内容」的唯一闸门。
func TestResponseState_StreamingPassthroughKeepsFirstTerminal(t *testing.T) {
	const secondOutput = "INJECTED-SECOND-TERMINAL"
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(responsesSSEStream +
			"event: response.completed\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_up_1\",\"object\":\"response\"," +
			"\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\"," +
			`"content":[{"type":"output_text","text":"` + secondOutput + `"}]}],` +
			"\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
		w.(http.Flusher).Flush()
	})
	m := mustCreateAIModel(t, e.store, newResponsesAIModel("mock-responses", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postResponses(t, token, `{"model":"mock-responses","input":"hi","stream":true}`)
	assertStatus(t, resp, http.StatusOK)
	stream := readBody(t, resp)

	// 前提：客户端确实收到了两个终态事件，且两者内容不同（否则本用例没有判别力）。
	if n := strings.Count(stream, "event: response.completed"); n != 2 {
		t.Fatalf("client must see both terminal events, got %d:\n%s", n, stream)
	}
	if !strings.Contains(stream, secondOutput) {
		t.Fatalf("second terminal event must be forwarded to the client verbatim, got:\n%s", stream)
	}

	rows := waitStoredResponses(t, e, 1)
	body := string(rows[0].ResponseBody)
	if strings.Contains(body, secondOutput) {
		t.Fatalf("stored response_body came from the LAST terminal event; it must be the FIRST one:\n%s", body)
	}
	if !strings.Contains(body, `"status":"completed"`) {
		t.Fatalf("stored response_body is not the first terminal response object:\n%s", body)
	}
	if items := string(rows[0].Items); strings.Contains(items, secondOutput) {
		t.Fatalf("stored items snapshot came from the LAST terminal event:\n%s", items)
	}

	// 「等于客户端所见」：落库的 response_body 必须是客户端收到的第一个终态 response 对象
	// （把网关回显的 store 除外——那是网关口径，不是上游内容）。
	var stored map[string]any
	if err := json.Unmarshal(rows[0].ResponseBody, &stored); err != nil {
		t.Fatalf("decode stored body: %v", err)
	}
	delete(stored, "store")
	firstClient := firstTerminalResponse(t, stream)
	delete(firstClient, "store")
	want, _ := json.Marshal(firstClient)
	got, _ := json.Marshal(stored)
	if string(want) != string(got) {
		t.Fatalf("stored body != first terminal event seen by the client:\n stored=%s\n client=%s", got, want)
	}
}

// firstTerminalResponse 取客户端 SSE 流里**第一个**终态事件的 response 对象。
func firstTerminalResponse(t *testing.T, stream string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(stream, "\n") {
		rest, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(rest)), &ev) != nil {
			continue
		}
		if typ, _ := ev["type"].(string); !strings.HasPrefix(typ, "response.completed") &&
			typ != "response.incomplete" && typ != "response.failed" {
			continue
		}
		target, ok := ev["response"].(map[string]any)
		if !ok {
			target = ev
		}
		return target
	}
	t.Fatalf("no terminal event in client stream")
	return nil
}

// TestResponseState_StreamingInterruptedDoesNotPersist（I-2）：
// 上游只发部分 chunk 就断开（没有终态事件）时，**不落库**——半截对话不应成为下一轮的父行。
// 这条覆盖转换路径：终态事件由翻译器在流结束时合成，一旦翻译器的收尾逻辑变化就会静默回归。
func TestResponseState_StreamingInterruptedDoesNotPersist(t *testing.T) {
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 一个内容分片 + 一个只带 finish_reason 的分片：都没有 [DONE]，也没有 usage 分片。
		_, _ = w.Write([]byte(
			`data: {"id":"chatcmpl-x","choices":[{"index":0,"delta":{"role":"assistant","content":"par"},"finish_reason":null}]}` + "\n\n" +
				`data: {"id":"chatcmpl-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
		w.(http.Flusher).Flush()
		// 直接返回：连接结束，网关读到的 body 到此为止。
	})
	m := mustCreateAIModel(t, e.store, newAIModel("mock-a", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postResponses(t, token, `{"model":"mock-a","stream":true,"input":"interrupted question"}`)
	assertStatus(t, resp, http.StatusOK)
	stream := readBody(t, resp)

	// 前提：网关确实**补发**了 response.completed（这是 Finish 的既定行为），
	// 所以「不落库」不能由「没有终态事件」来解释，必须是明确的策略。
	if !strings.Contains(stream, "event: response.completed") {
		t.Fatalf("Finish must still emit a terminal event to the client, got:\n%s", stream)
	}

	// 给落库一个明确的观察窗（它发生在响应写出之后）。
	time.Sleep(100 * time.Millisecond)
	if rows := storedResponseRows(t, e); len(rows) != 0 {
		t.Fatalf("response_states rows = %d, want 0 for an interrupted upstream stream", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 7. 上游 store 强制 false
// ---------------------------------------------------------------------------

// TestResponseState_UpstreamStoreForcedFalse：客户端传 store:true，网关落库，但转发给
// openai-responses 上游的 body 里 store 必须是 false（数据面最小化）。
func TestResponseState_UpstreamStoreForcedFalse(t *testing.T) {
	e := newEnv(t, respondConstantJSON(http.StatusOK, okResponsesBody))
	m := mustCreateAIModel(t, e.store, newResponsesAIModel("mock-responses", e.upstream.URL))
	_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))

	resp := e.postResponses(t, token, `{"model":"mock-responses","input":"hi","store":true}`)
	assertStatus(t, resp, http.StatusOK)
	out := decodeResponsesBody(t, readBody(t, resp))
	if got, ok := out["store"].(bool); !ok || !got {
		t.Fatalf("response.store = %v (present=%v), want true (gateway stores)", out["store"], ok)
	}

	up := waitUpstreamBody(t, e, 1)
	if store, ok := up["store"]; !ok || store != false {
		t.Fatalf("upstream body.store = %v (present=%v), want forced false", store, ok)
	}
	if rows := waitStoredResponses(t, e, 1); len(rows) != 1 {
		t.Fatalf("response_states rows = %d, want 1", len(rows))
	}
}
