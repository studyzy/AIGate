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

// 本文件钉住 design §5.4 的「9 格矩阵」：3 个数据面入口 × 3 种上游协议（9 格全覆盖）。
//
//	| 入口                 | 上游 model_type     | §5.4 表格指定路径                                      |
//	|----------------------|---------------------|--------------------------------------------------------|
//	| /v1/chat/completions | openai-completions  | 直通（格 1）                                           |
//	| /v1/chat/completions | anthropic-messages  | GetProvider → AnthropicProvider 内部转换（格 2）        |
//	| /v1/chat/completions | openai-responses    | GetProvider → OpenAIResponsesProvider 内部转换（格 3）  |
//	| /v1/messages         | anthropic-messages  | forwardAnthropicPassthrough 直通（格 4）                |
//	| /v1/messages         | openai-completions  | forwardViaOpenAIConversion（格 5）                      |
//	| /v1/messages         | openai-responses    | 同格 5，GetProvider 自动落到新 provider（格 6）         |
//	| /v1/responses        | openai-responses    | forwardResponsesPassthrough 直通（格 7）                |
//	| /v1/responses        | openai-completions  | forwardViaChatConversion（格 8）                        |
//	| /v1/responses        | anthropic-messages  | 同格 8，GetProvider 落到 AnthropicProvider（格 9）       |
//
// 每格的判别力来自四处，任何一处不成立该格立刻变红：
//  1. mock 上游**按收到的路径**决定返回哪种协议的响应——某格打到非预期路径时上游按另一种
//     协议回包，客户端的形状断言随即失败，而不是被一段「三协议通吃」的万用 body 掩盖；
//  2. 断言上游收到的请求体是**上游协议自己的形状**（chat 带 messages / anthropic 带 messages
//     与 max_tokens / responses 带 input 且无 messages），证明转换确实发生在 Provider 这
//     一唯一边界上，而不是把入站 body 原样塞给上游；
//  3. 断言上游恰好收到一次请求；
//  4. 断言用量记账拿到非零 token（形状对了但用量丢失同样算矩阵没走通）。
//
// allowPrivateUpstreams 是包级全局开关，本文件所有用例串行执行，禁止 t.Parallel。

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"aigate/internal/model"
)

// 入口标识。
const (
	matrixIngressChat      = "chat"
	matrixIngressMessages  = "messages"
	matrixIngressResponses = "responses"
)

// 上游协议标识（取值即 ai_models.model_type，见 internal/protocol）。
const (
	matrixUpstreamChat      = model.ModelTypeOpenAI
	matrixUpstreamAnthropic = model.ModelTypeAnthropic
	matrixUpstreamResponses = model.ModelTypeResponses
)

// 三种协议的 mock 上游响应体。
const (
	matrixChatReply = `{"id":"chatcmpl-matrix","object":"chat.completion","model":"up-chat",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"matrix chat answer"},` +
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`

	matrixAnthropicReply = `{"id":"msg_matrix","type":"message","role":"assistant",` +
		`"content":[{"type":"text","text":"matrix anthropic answer"}],"model":"up-anthropic",` +
		`"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":5}}`

	matrixResponsesReply = `{"id":"resp_matrix_upstream","object":"response","model":"up-responses",` +
		`"status":"completed","output":[{"type":"message","role":"assistant","id":"msg_1",` +
		`"content":[{"type":"output_text","text":"matrix responses answer","annotations":[]}]}],` +
		`"usage":{"input_tokens":6,"output_tokens":7,"total_tokens":13}}`
)

// matrixIngress 描述一个数据面入口：客户端路径、请求体模板、响应形状断言。
type matrixIngress struct {
	name    string
	path    string
	bodyFmt string // %s 为密钥绑定的上游模型 ID
	// assertShape 断言客户端拿到的是本入口协议的响应形状。
	assertShape func(t *testing.T, cell string, resp map[string]any)
}

// matrixUpstream 描述一种上游协议：mock 的路径、URL 约定、请求体形状校验、成功响应体。
type matrixUpstream struct {
	name string
	// paths 是本协议在 mock 上落地的路径；请求打到别的路径即判定为矩阵错位。
	paths []string
	// apiModel 把 mock 地址换算成该协议在 ai_models.url 里应填的值。
	//
	// 关键约定：Provider 一律在「配置的 URL」后**直接拼接**自己的路径
	// （provider 各 baseURL 的 TrimRight(apiBase, "/") + 路径），URL 不会被补 /v1；
	// 而 ai_models.model_type 的常量注释约定「OpenAI 兼容端点 URL 含 /v1」，
	// 即配置值本来就带 /v1。故这里从 mock 地址剥掉「/v1 + 协议路径」得到配置值，
	// 拼回去正好命中 mock。写错约定（例如多塞一个 /v1）会让请求落到 /v1/v1/...，
	// mock 回 404，该格立刻变红——这正是本用例要暴露的路径错位。
	apiModel func(mockURL string) string
	// assertRequestBody 断言上游收到的是本协议自己的请求体形状。
	assertRequestBody func(t *testing.T, cell string, body map[string]any)
	reply             string
	// assertShapeIndependent 标出「本格响应形状不能证明直通路径」的场景（见 Test 内注释）。
	assertShapeIndependent bool
}

// matrixIngresses 是 3 个入口的固定顺序（先入口、后上游：格号与 §5.4 表格前 3 行对齐）。
var matrixIngresses = []matrixIngress{
	{
		name:    matrixIngressChat,
		path:    "/v1/chat/completions",
		bodyFmt: `{"model":"%s","messages":[{"role":"user","content":"hi"}]}`,
		assertShape: func(t *testing.T, cell string, resp map[string]any) {
			t.Helper()
			choices, ok := resp["choices"].([]any)
			if !ok || len(choices) == 0 {
				t.Fatalf("%s: chat ingress response missing choices[]: %v", cell, resp)
			}
			first, _ := choices[0].(map[string]any)
			msg, ok := first["message"].(map[string]any)
			if !ok {
				t.Fatalf("%s: chat ingress response missing choices[0].message: %v", cell, first)
			}
			if text, _ := msg["content"].(string); text == "" {
				t.Fatalf("%s: chat ingress response choices[0].message.content is empty: %v", cell, msg)
			}
		},
	},
	{
		name:    matrixIngressMessages,
		path:    "/v1/messages",
		bodyFmt: `{"model":"%s","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
		assertShape: func(t *testing.T, cell string, resp map[string]any) {
			t.Helper()
			content, ok := resp["content"].([]any)
			if !ok || len(content) == 0 {
				t.Fatalf("%s: messages ingress response missing content[]: %v", cell, resp)
			}
			first, _ := content[0].(map[string]any)
			if typ, _ := first["type"].(string); typ != "text" {
				t.Fatalf("%s: messages ingress content[0].type = %q, want \"text\" (%v)", cell, typ, first)
			}
			if text, _ := first["text"].(string); text == "" {
				t.Fatalf("%s: messages ingress content[0].text is empty: %v", cell, first)
			}
		},
	},
	{
		name:    matrixIngressResponses,
		path:    responsesPath,
		bodyFmt: `{"model":"%s","input":"hi"}`,
		assertShape: func(t *testing.T, cell string, resp map[string]any) {
			t.Helper()
			if _, ok := resp["output"].([]any); !ok {
				t.Fatalf("%s: responses ingress response missing output[]: %v", cell, resp)
			}
			if text := responsesText(t, resp); text == "" {
				t.Fatalf("%s: responses ingress output_text is empty: %v", cell, resp)
			}
		},
	},
}

// matrixUpstreams 是 3 种上游协议的固定顺序。
var matrixUpstreams = []matrixUpstream{
	{
		name:     matrixUpstreamChat,
		paths:    []string{"/chat/completions"},
		apiModel: func(mockURL string) string { return strings.TrimSuffix(mockURL, "/v1/chat/completions") },
		reply:    matrixChatReply,
		assertRequestBody: func(t *testing.T, cell string, body map[string]any) {
			t.Helper()
			if _, ok := body["messages"]; !ok {
				t.Fatalf("%s: chat upstream request body missing messages: %v", cell, body)
			}
			if _, ok := body["input"]; ok {
				t.Fatalf("%s: chat upstream request body must not carry responses-shaped input: %v", cell, body)
			}
		},
	},
	{
		name:     matrixUpstreamAnthropic,
		paths:    []string{"/v1/messages"},
		apiModel: func(mockURL string) string { return strings.TrimSuffix(mockURL, "/v1/messages") },
		reply:    matrixAnthropicReply,
		assertRequestBody: func(t *testing.T, cell string, body map[string]any) {
			t.Helper()
			if _, ok := body["messages"]; !ok {
				t.Fatalf("%s: anthropic upstream request body missing messages: %v", cell, body)
			}
			if _, ok := body["input"]; ok {
				t.Fatalf("%s: anthropic upstream request body must not carry responses-shaped input: %v", cell, body)
			}
			if n, ok := bodyNumber(t, body, "max_tokens"); !ok || n <= 0 {
				t.Fatalf("%s: anthropic upstream request body max_tokens = %v (present=%v), want > 0", cell, n, ok)
			}
		},
	},
	{
		name:     matrixUpstreamResponses,
		paths:    []string{"/responses"},
		apiModel: func(mockURL string) string { return strings.TrimSuffix(mockURL, "/v1/responses") },
		reply:    matrixResponsesReply,
		// responses 入口 × responses 上游（格 7）的响应形状**没有**路径判别力：
		// 该上游的回复本身就是 Responses 形状，直通与「转 chat 再转回 Responses」
		// 两种实现都会产出合法 output[]（实测变异：把直通分支强制关掉，本格仍 PASS）。
		// 直通是否真的生效要靠现有 TestResponsesIngress_PassthroughToResponsesUpstream
		// 断言「上游收到 input 且无 messages、store 被强制 false」来钉住；本用例只保证
		// 该格可达且形状合法。此处显式标记，避免把 PASS 误读成「直通已被覆盖」。
		assertShapeIndependent: true,
		assertRequestBody: func(t *testing.T, cell string, body map[string]any) {
			t.Helper()
			if _, ok := body["input"]; !ok {
				t.Fatalf("%s: responses upstream request body missing input: %v", cell, body)
			}
			if _, ok := body["messages"]; ok {
				t.Fatalf("%s: responses upstream request body must not carry chat messages: %v", cell, body)
			}
		},
	},
}

// matrixCall 是一次上游请求的快照。
type matrixCall struct {
	path      string
	xAPIKey   string
	authToken string
	body      map[string]any
}

// matrixUpstreamServer 是矩阵用例专用的 mock 上游：按请求路径返回对应协议的响应，
// 并记录每次请求（路径、鉴权头、请求体）。
//
// 为什么不用共享夹具 startUpstream（httptest.NewServer）：在本仓库的工具链环境下，
// httptest 的 mock 上游收到的是**零字节 body**（Content-Length 与请求头都在，body 读
// 出来是 0 字节）——这一点对三个入口、三种上游全部一致，与本用例无关但与「按请求体
// 形状断言」直接冲突。换用 net.Listen + http.Server 自建监听后 body 正常到达，
// 已用「同一条请求分别打到 httptest 与自建监听」对照确认。
// 该现象同时意味着**任何依赖上游请求体的既有黑盒用例在本机都会读到空 body**，
// 需要在独立任务里核实（本任务不修改共享夹具）。
type matrixUpstreamServer struct {
	url    string
	server *http.Server

	mu      sync.Mutex
	byPath  map[string][]matrixCall
	misrout []string
}

// startMatrixUpstream 在 127.0.0.1 上起一个按路径分派协议的 mock 上游。
func startMatrixUpstream(t *testing.T) *matrixUpstreamServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen matrix upstream: %v", err)
	}
	replies := map[string]string{
		"/chat/completions": matrixChatReply,
		"/v1/messages":      matrixAnthropicReply,
		"/responses":        matrixResponsesReply,
	}
	up := &matrixUpstreamServer{
		url:    "http://" + ln.Addr().String(),
		byPath: make(map[string][]matrixCall, len(replies)),
	}
	up.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &parsed)
		}
		reply, known := replies[r.URL.Path]

		// 记录在返回响应**之前**提交：客户端读到响应体时，本次请求一定已在快照里。
		up.mu.Lock()
		up.byPath[r.URL.Path] = append(up.byPath[r.URL.Path], matrixCall{
			path:      r.URL.Path,
			xAPIKey:   r.Header.Get("x-api-key"),
			authToken: r.Header.Get("Authorization"),
			body:      parsed,
		})
		if !known {
			up.misrout = append(up.misrout, r.URL.Path)
		}
		up.mu.Unlock()

		if !known {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"unknown upstream path"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, reply)
	})}
	go func() { _ = up.server.Serve(ln) }()
	t.Cleanup(func() { _ = up.server.Close() })
	return up
}

// snapshot 拷贝当前状态。
func (u *matrixUpstreamServer) snapshot() (map[string][]matrixCall, []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make(map[string][]matrixCall, len(u.byPath))
	for path, calls := range u.byPath {
		out[path] = append([]matrixCall(nil), calls...)
	}
	return out, append([]string(nil), u.misrout...)
}

// callsFor 返回该协议预期路径收到的请求（按接收顺序）与全局错位记录。
func (u *matrixUpstreamServer) callsFor(t *testing.T, cell string, want matrixUpstream) []matrixCall {
	t.Helper()
	all, misrouted := u.snapshot()
	if len(misrouted) > 0 {
		t.Fatalf("%s: upstream received requests on unexpected paths %v (want %v; all paths: %s)",
			cell, misrouted, want.paths, matrixSummarize(all))
	}
	calls := make([]matrixCall, 0)
	for _, path := range want.paths {
		calls = append(calls, all[path]...)
	}
	return calls
}

// matrixSummarize 把「路径 → 次数」压成一行便于失败时定位。
func matrixSummarize(all map[string][]matrixCall) string {
	paths := make([]string, 0, len(all))
	for path := range all {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	parts := make([]string, 0, len(paths))
	for _, path := range paths {
		parts = append(parts, fmt.Sprintf("%s×%d", path, len(all[path])))
	}
	return strings.Join(parts, " ")
}

// matrixPost 向被测网关发出一次请求。
func matrixPost(t *testing.T, e *env, path, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build %s request: %v", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("do %s request: %v", path, err)
	}
	return resp
}

// matrixAssertAuth 断言上游收到该协议的鉴权形态：
// anthropic-messages 用 x-api-key，其余走 Authorization: Bearer。
func matrixAssertAuth(t *testing.T, cell string, up matrixUpstream, call matrixCall) {
	t.Helper()
	if up.name == matrixUpstreamAnthropic {
		if call.xAPIKey == "" {
			t.Fatalf("%s: anthropic upstream must receive x-api-key", cell)
		}
		return
	}
	if !strings.HasPrefix(call.authToken, "Bearer ") {
		t.Fatalf("%s: upstream Authorization = %q, want Bearer", cell, call.authToken)
	}
}

// TestMatrix_AllNineCells 逐格验证 3 个入口 × 3 种上游 = 9 格全部可用。
//
// 「handler 不 panic」不显式断言：http.ServeMux/net/http 会 recover panic 并中止连接，
// 客户端拿到的是 EOF 而非 200，下面第一条 assertStatus 就必然失败。
func TestMatrix_AllNineCells(t *testing.T) {
	// 净 Handler + 真实 HTTP：网关自己仍用 httptest（它的 role 是「被调用方」，
	// 不受上面记录的 body 读取问题影响）。
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	upstream := startMatrixUpstream(t)

	// 每种上游协议一个模型 + 一把密钥（密钥 MaxTokens=0 不限、不建分组）：
	// 矩阵用例只关心可达性，不让 pay-as-you-go 配额在格间累积干扰。
	tokens := make(map[string]string, len(matrixUpstreams))
	modelIDs := make(map[string]string, len(matrixUpstreams))
	for _, up := range matrixUpstreams {
		modelID := "up-" + up.name
		m := newAIModel(modelID, up.apiModel(upstream.url))
		m.ModelType = up.name
		mustCreateAIModel(t, e.store, m)
		_, token := mustCreateKey(t, e.store, e.user.ID, 0, primary(m.ID, 0))
		modelIDs[up.name] = modelID
		tokens[up.name] = token
	}

	// 每格调用一次，故上游各路径的累计计数在格间递增；用基线差值判断「本格恰好一次」。
	served := make(map[string]int, len(matrixUpstreams))
	total := 0
	for _, ing := range matrixIngresses {
		for _, up := range matrixUpstreams {
			total++
			cell := fmt.Sprintf("cell%d/%s-ingress/%s-upstream", total, ing.name, up.name)
			t.Run(cell, func(t *testing.T) {
				body := fmt.Sprintf(ing.bodyFmt, modelIDs[up.name])

				resp := matrixPost(t, e, ing.path, tokens[up.name], body)
				assertStatus(t, resp, http.StatusOK)
				respBody := readBody(t, resp)

				var out map[string]any
				if err := json.Unmarshal([]byte(respBody), &out); err != nil {
					t.Fatalf("%s: decode %s response %q: %v", cell, ing.name, respBody, err)
				}
				if _, isErr := out["error"]; isErr {
					t.Fatalf("%s: 200 must not carry an error body: %v", cell, out)
				}
				ing.assertShape(t, cell, out)
				if up.assertShapeIndependent {
					t.Logf("%s: response shape is path-agnostic for this cell; passthrough is pinned by TestResponsesIngress_PassthroughToResponsesUpstream", cell)
				}

				// 上游侧：本格恰好新增一次请求、落在本协议路径上、body 为本协议形状、鉴权形态正确。
				calls := upstream.callsFor(t, cell, up)
				got := calls[served[up.name]:]
				if len(got) != 1 {
					all, _ := upstream.snapshot()
					t.Fatalf("%s: upstream %s expected exactly 1 new request on %v, got %d (all paths: %s)",
						cell, up.name, up.paths, len(got), matrixSummarize(all))
				}
				served[up.name] = len(calls)
				if got[0].path != up.paths[0] {
					t.Fatalf("%s: upstream path = %q, want %q", cell, got[0].path, up.paths[0])
				}
				up.assertRequestBody(t, cell, got[0].body)
				matrixAssertAuth(t, cell, up, got[0])

				// 用量记账必须拿到非零 token：形状对了但用量丢失同样是矩阵没走通。
				// responsesLatestUsage 内部会排空用量写入器，顺带保证本格观测已完成。
				if total == len(matrixIngresses)*len(matrixUpstreams) {
					row := responsesLatestUsage(t, e)
					if row.TotalTokens <= 0 {
						t.Fatalf("%s: usage log total_tokens = %d, want > 0 (row=%+v)", cell, row.TotalTokens, row)
					}
				}
			})
		}
	}
	if total != 9 {
		t.Fatalf("matrix must cover 9 cells, table declares %d", total)
	}
}
