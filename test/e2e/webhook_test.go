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

//go:build e2e

package e2e

// webhook 用例：订阅、签名投递、事件覆盖与投递记录。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// webhookSecretE2E 是 M4 订阅的固定 secret（用于复算 HMAC 签名）。
const webhookSecretE2E = "e2e-webhook-secret"

// webhookDelivery 是 mock 接收端记录的一次投递。
type webhookDelivery struct {
	EventType  string
	DeliveryID string
	Signature  string
	Body       []byte
}

// webhookReceiver 是进程内的假 webhook 接收端，记录收到的投递并恒返回 200。
type webhookReceiver struct {
	server *httptest.Server

	mu  sync.Mutex
	got []webhookDelivery
}

func newWebhookReceiver() *webhookReceiver {
	r := &webhookReceiver{}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.got = append(r.got, webhookDelivery{
			EventType:  req.Header.Get("X-Gateway-Event"),
			DeliveryID: req.Header.Get("X-Gateway-Delivery"),
			Signature:  req.Header.Get("X-Gateway-Signature"),
			Body:       body,
		})
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return r
}

func (r *webhookReceiver) URL() string { return r.server.URL }

func (r *webhookReceiver) Close() { r.server.Close() }

// snapshot 返回已收到的投递副本。
func (r *webhookReceiver) snapshot() []webhookDelivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]webhookDelivery, len(r.got))
	copy(out, r.got)
	return out
}

// waitEvent 轮询等待指定类型的投递到达，返回该投递。
func (r *webhookReceiver) waitEvent(t *testing.T, eventType string) webhookDelivery {
	t.Helper()
	var found webhookDelivery
	waitFor(t, fmt.Sprintf("the webhook received the %s event", eventType), webhookPollTimeout, func() error {
		got := r.snapshot()
		for _, d := range got {
			if d.EventType == eventType {
				found = d
				return nil
			}
		}
		return fmt.Errorf("no %s received yet (currently %d deliveries received)", eventType, len(got))
	})
	return found
}

// e2eSign 复算投递签名（黑盒实现，语义与网关一致：sha256=<hex(hmac-sha256(secret, body))>）。
func e2eSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// e2eEnvelope 是事件信封（仅取用例需要断言的字段）。
type e2eEnvelope struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	CreatedAt string         `json:"created_at"`
	Data      map[string]any `json:"data"`
}

// decodeEnvelope 解析投递 body 为事件信封。
func decodeEnvelope(t *testing.T, body []byte) e2eEnvelope {
	t.Helper()
	var env e2eEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("failed to parse the event envelope: %v, body=%s", err, body)
	}
	return env
}

func TestE2E_Webhook_Delivery(t *testing.T) {
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	openAIMock := newUpstreamMock(kindOpenAI)
	t.Cleanup(openAIMock.Close)

	gw := startGateway(t, bin, dsn)
	adminAuth := adminToken

	db := openE2EDB(t, dsn)
	waitFor(t, "database reachable", usagePollTimeout, db.Ping)

	receiver := newWebhookReceiver()
	t.Cleanup(receiver.Close)

	var (
		webhookID int64
		modelID   int64

		limitedGroup int64
		userID       int64
		limitedKeyID int64
		limitedKey   string
	)

	t.Run("1. create the subscription (secret is masked, event_types subscribes to all)", func(t *testing.T) {
		modelID = createModel(t, gw.baseURL, adminAuth, map[string]any{
			"provider":   "mock",
			"model_id":   openAIModelID,
			"model_name": openAIModelID,
			"url":        openAIMock.URL() + "/v1",
			"api_key":    upstreamAPIKey,
			"model_type": "openai-completions",
			"quota_day":  -1,
			"enabled":    true,
		})

		var resp struct {
			ID         int64    `json:"id"`
			Secret     string   `json:"secret"`
			EventTypes []string `json:"event_types"`
		}
		adminCLI(t, requireEnv(t, envCLIBinKey), gw.baseURL, &resp,
			"webhooks", "create", "--data", "@"+cliDataFile(t, "webhook.json", map[string]any{
				"name":        "e2e-hook",
				"url":         receiver.URL(),
				"secret":      webhookSecretE2E,
				"event_types": []string{},
			}))
		if resp.ID == 0 {
			t.Fatal("webhooks create response is missing id")
		}
		if resp.Secret == "" {
			t.Fatal("the creation response should return a masked secret")
		}
		if resp.Secret == webhookSecretE2E {
			t.Fatalf("the creation response echoed the secret plaintext: %q", resp.Secret)
		}
		if len(resp.EventTypes) != 0 {
			t.Errorf("empty event_types should return an empty array (subscribe to all), got %+v", resp.EventTypes)
		}
		webhookID = resp.ID
		t.Logf("webhook id=%d url=%s", webhookID, receiver.URL())
	})

	t.Run("2. a normal forward -> request.completed (signature is verifiable)", func(t *testing.T) {
		_, key := m2CreateKey(t, gw.baseURL, adminAuth, 1, 0, modelID)
		status, body := postChatCompletion(t, gw.baseURL, key, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("the non-streaming forward expected 200, got %d, response: %s", status, body)
		}

		got := receiver.waitEvent(t, "request.completed")
		if got.DeliveryID == "" {
			t.Error("the X-Gateway-Delivery header is missing")
		}
		if want := e2eSign(webhookSecretE2E, got.Body); got.Signature != want {
			t.Errorf("signature verification failed: expected %q, got %q", want, got.Signature)
		}

		env := decodeEnvelope(t, got.Body)
		if env.Type != "request.completed" || env.ID != got.DeliveryID || env.CreatedAt == "" {
			t.Errorf("unexpected envelope fields: %+v (delivery ID header=%q)", env, got.DeliveryID)
		}
		if env.Data["model"] != openAIModelID {
			t.Errorf("data.model expected %q, got %v", openAIModelID, env.Data["model"])
		}
		usage, ok := env.Data["usage"].(map[string]any)
		if !ok {
			t.Fatalf("data.usage should be an object, got %#v", env.Data["usage"])
		}
		if total, _ := usage["total_tokens"].(float64); total <= 0 {
			t.Errorf("data.usage.total_tokens should be > 0, got %#v", usage)
		}
	})

	t.Run("3. quota exceeded -> quota.exceeded (kind=user)", func(t *testing.T) {
		limitedGroup = m2CreateGroup(t, gw.baseURL, adminAuth, "m4-limited", `[{"mode":"day","limit":5}]`)

		u := createUser(t, gw.baseURL, "m4-user", limitedGroup)
		userID = u.ID

		limitedKeyID, limitedKey = m2CreateKey(t, gw.baseURL, adminAuth, userID, limitedGroup, modelID)

		status, _, body := m2PostChatWithHeader(t, gw.baseURL, limitedKey, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("the first request from the limited group expected 200, got %d, response: %s", status, body)
		}
		// 日限 5，本次记入 18 tokens；等异步落库后再发第二次触发超限。
		waitFor(t, "the limited-group usage is persisted", usagePollTimeout, func() error {
			st, b := doRequest(
				t,
				http.MethodGet,
				fmt.Sprintf("%s/admin/usage/logs?group_id=%d", gw.baseURL, limitedGroup),
				adminAuth,
				nil,
			)
			if st != http.StatusOK {
				return fmt.Errorf("GET /admin/usage/logs returned %d: %s", st, b)
			}
			var resp struct {
				Total int64 `json:"total"`
			}
			if err := json.Unmarshal(b, &resp); err != nil {
				return err
			}
			if resp.Total < 1 {
				return fmt.Errorf("the usage of this group is not persisted yet (total=%d)", resp.Total)
			}
			return nil
		})

		status, _, body = m2PostChatWithHeader(t, gw.baseURL, limitedKey, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi again"}},
		})
		if status != http.StatusTooManyRequests {
			t.Fatalf("the exceeding request expected 429, got %d, response: %s", status, body)
		}

		got := receiver.waitEvent(t, "quota.exceeded")
		if want := e2eSign(webhookSecretE2E, got.Body); got.Signature != want {
			t.Errorf("quota.exceeded signature verification failed: expected %q, got %q", want, got.Signature)
		}
		env := decodeEnvelope(t, got.Body)
		if env.Data["kind"] != "user" {
			t.Errorf("data.kind expected user, got %v", env.Data["kind"])
		}
		if id, _ := env.Data["api_key_id"].(float64); int64(id) != limitedKeyID {
			t.Errorf("data.api_key_id expected %d, got %v", limitedKeyID, env.Data["api_key_id"])
		}
		if env.Data["model"] != openAIModelID {
			t.Errorf("data.model expected %q, got %v", openAIModelID, env.Data["model"])
		}
		if limit, _ := env.Data["limit"].(float64); limit <= 0 {
			t.Errorf("data.limit should be > 0, got %v", env.Data["limit"])
		}
	})

	t.Run("4. disabling the key -> key.disabled (reason=disabled)", func(t *testing.T) {
		adminCLI(t, requireEnv(t, envCLIBinKey), gw.baseURL, nil,
			"keys", "update", "--id", strconv.FormatInt(limitedKeyID, 10),
			"--data", "@"+cliDataFile(t, "disable.json", map[string]any{"enabled": false}))

		got := receiver.waitEvent(t, "key.disabled")
		env := decodeEnvelope(t, got.Body)
		if id, _ := env.Data["key_id"].(float64); int64(id) != limitedKeyID {
			t.Errorf("data.key_id expected %d, got %v", limitedKeyID, env.Data["key_id"])
		}
		if env.Data["reason"] != "disabled" {
			t.Errorf("data.reason expected disabled, got %v", env.Data["reason"])
		}
		if owner, _ := env.Data["user_id"].(float64); int64(owner) != userID {
			t.Errorf("data.user_id expected %d, got %v", userID, env.Data["user_id"])
		}
	})

	t.Run("5. delivery records are queryable (total>=3 and at least one success)", func(t *testing.T) {
		var resp struct {
			Deliveries []struct {
				ID         int64  `json:"id"`
				WebhookID  int64  `json:"webhook_id"`
				EventType  string `json:"event_type"`
				Status     string `json:"status"`
				Attempts   int    `json:"attempts"`
				LastStatus int    `json:"last_status"`
				CreatedAt  string `json:"created_at"`
			} `json:"deliveries"`
			Total    int64 `json:"total"`
			Page     int   `json:"page"`
			PageSize int   `json:"page_size"`
		}
		adminCLI(t, requireEnv(t, envCLIBinKey), gw.baseURL, &resp,
			"webhooks", "deliveries", "--id", strconv.FormatInt(webhookID, 10),
			"--page", "1", "--page-size", "50")
		if resp.Total < 3 {
			t.Fatalf("delivery records expected >= 3, got %d", resp.Total)
		}
		if resp.Page != 1 || resp.PageSize != 50 || len(resp.Deliveries) == 0 {
			t.Errorf("unexpected pagination fields: %+v", resp)
		}

		successes := 0
		for i, d := range resp.Deliveries {
			if d.WebhookID != webhookID {
				t.Errorf("the delivery record[%d] webhook_id expected %d, got %d", i, webhookID, d.WebhookID)
			}
			if d.CreatedAt == "" {
				t.Errorf("the delivery record[%d] is missing created_at", i)
			}
			if d.Status == "success" {
				successes++
				if d.Attempts < 1 || d.LastStatus != http.StatusOK {
					t.Errorf("the successful delivery[%d] should have attempts>=1 and last_status=200, got %+v", i, d)
				}
			}
		}
		if successes == 0 {
			t.Errorf("there should be a delivery record with status=success, got %+v", resp.Deliveries)
		}

		// 直连数据库复核投递记录确实落库。
		var rows int
		if err := db.QueryRow(`SELECT COUNT(*) FROM webhook_deliveries WHERE webhook_id = `+
			`?`, webhookID).Scan(&rows); err != nil {
			t.Fatalf("failed to read webhook_deliveries directly: %v", err)
		}
		if rows < 3 {
			t.Errorf("webhook_deliveries expected >= 3 rows, got %d", rows)
		}
	})

	t.Run("6. an expired key -> key.expired (delivered once per validity window)", func(t *testing.T) {
		expiredKey := createKey(t, gw.baseURL, map[string]any{
			"name": "m4-expired-key", "user_id": 1,
			"expires_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
			"bindings":   []map[string]any{{"ai_model_id": modelID, "role": "primary"}},
		})
		if expiredKey.Token == "" {
			t.Fatal("keys create response is missing the one-time plaintext token")
		}
		key := expiredKey

		// 首次使用：401 + 一次 key.expired。
		status, respBody := postChatCompletion(t, gw.baseURL, key.Token, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusUnauthorized {
			t.Fatalf("an expired key expected 401, got %d, response: %s", status, respBody)
		}
		got := receiver.waitEvent(t, "key.expired")
		env := decodeEnvelope(t, got.Body)
		if env.Data["key_id"] == nil || env.Data["name"] == nil {
			t.Errorf("key.expired data should carry key_id/name, got %#v", env.Data)
		}
		if want := e2eSign(webhookSecretE2E, got.Body); got.Signature != want {
			t.Errorf("signature verification failed for key.expired: expected %q, got %q", want, got.Signature)
		}

		// 再次使用：仍 401，但不重复投递。
		postChatCompletion(t, gw.baseURL, key.Token, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		time.Sleep(700 * time.Millisecond)
		count := 0
		for _, d := range receiver.snapshot() {
			if d.EventType == "key.expired" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("key.expired deliveries = %d, want 1 (once per validity window)", count)
		}
	})
}
