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

// 按金额计费的端到端链路：模型币种 + 价目快照 + 汇率 + 平台结算币种 → 数据面记账 →
// 明细表 / 日聚合表 / 报表 / 金额配额。
//
// 时钟口径：e2e 不控制时钟。峰谷判定依赖「记账时刻落在哪个时段」，一旦在 e2e 里伪造时间，
// 网关进程、数据库时间与请求时刻就不再是同一个事实源，断言会变成对测试脚手架的断言。
// 因此高峰档与跨午夜窗口的边界只由 internal/billing 的单元测试覆盖（可注入时刻）；
// 这里只覆盖空闲价路径（enable_peak_pricing=false → 恒取 offpeak_* 三列）与上游自报成本路径。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// 本文件用到的金额（微元，1 单位 = 1e6 微元）。
//
// 口径来自设计文档「计价口径」：单价 *_micros 是「每百万 token 的微元数」，
// 金额 = token × 单价 / 1e6。据此：
//
//	输出单价 4 元/百万 = 4_000_000，500 个输出 token → 500 × 4_000_000 / 1e6 = 2_000 微元 = 0.002 元
//	按 1 元 = 0.14 美元的汇率折算 → 2_000 × 0.14 = 280 微元
//
// 注意：实现计划 Task 14 的示例期望 2_000_000 / 280_000，与同一份设计文档的公式相差 1000 倍
// （该数量级只有 token 数为整百万时才成立，如 internal/billing 的用例都用 1_000_000 token）。
// 这里以文档公式、billing 包与 engine/pricing 单测三者一致的 2_000 / 280 为准。
const (
	// priceOutputMicrosPerMtok 是 4 元/百万 token（CNY）。
	priceOutputMicrosPerMtok int64 = 4_000_000
	// mockCompletionTokens 是本用例 mock 上游回报的输出 token 数。
	mockCompletionTokens int64 = 500
	// wantVendorMicros 是模型币种金额：500 × 4 元/百万 = 0.002 元 = 2_000 微元。
	wantVendorMicros int64 = 2_000
	// rateCNYToUSDMicros 是 1 CNY = 0.14 USD。
	rateCNYToUSDMicros int64 = 140_000
	// wantPlatformMicros 是折算后的结算金额：2_000 × 0.14 = 280 微元。
	wantPlatformMicros int64 = 280
	// costQuotaLimitUnits 是密钥级金额配额：月窗口 1 USD。
	costQuotaLimitUnits int64 = 1
	// wantCostQuotaWindowMicros 是配额窗口内应攒到的结算金额：1 USD = 1e6 微元。
	wantCostQuotaWindowMicros int64 = costQuotaLimitUnits * 1_000_000
	// costQuotaOutputMicros 是金额配额专用模型的输出单价：1000 元/百万 token。
	// 配合 mock 的 500 输出 token 与 0.14 的折算率，单笔结算金额 70_000 微元（0.07 USD），
	// 十几笔即可越过 1 USD，无需为了攒额度打出几千次请求。
	costQuotaOutputMicros   int64 = 1_000_000_000
	costQuotaVendorMicros   int64 = mockCompletionTokens * costQuotaOutputMicros / 1_000_000
	costQuotaPlatformMicros int64 = costQuotaVendorMicros * rateCNYToUSDMicros / 1_000_000
	// maxCostQuotaAttempts 是配额用例的请求上限：超出仍未 429 即判定失败，避免无界循环。
	maxCostQuotaAttempts = 40
)

// newBillingModel 创建一把只用于计费用的模型：CNY 计价，绑定 mock 上游。
// 各自带唯一 model_id，互不依赖、可任意顺序执行。
func newBillingModel(t *testing.T, baseURL, mockURL, modelID string, extra map[string]any) int64 {
	t.Helper()

	payload := map[string]any{
		"provider":   "mock",
		"model_id":   modelID,
		"model_name": modelID,
		"url":        mockURL + "/v1",
		"api_key":    upstreamAPIKey,
		"model_type": "openai-completions",
		"quota_day":  -1,
		"enabled":    true,
		"currency":   "CNY",
	}
	for k, v := range extra {
		payload[k] = v
	}
	return createModel(t, baseURL, adminToken, payload)
}

// billingCosts 是一次请求落库后的金额快照（微元）。
type billingCosts struct {
	CostVendorMicros   int64
	CostPlatformMicros int64
	CompletionTokens   int64
}

// waitForKeyUsageCosts 等待某把密钥出现 n 条用量明细（异步落库），返回它们按主键倒序的金额。
// 金额一起读，保证「行数已达 n」与「金额已算完」是同一个快照。
func waitForKeyUsageCosts(t *testing.T, db *sql.DB, apiKeyID int64, n int) []billingCosts {
	t.Helper()

	var got []billingCosts
	waitFor(t, fmt.Sprintf("usage rows of api_key_id=%d reach %d with costs billed", apiKeyID, n),
		usagePollTimeout, func() error {
			rows, err := db.Query(
				// 明细表不冗余币种：币种是模型行的属性（cost_vendor_micros 即模型币种金额）。
				`SELECT cost_vendor_micros, cost_platform_micros, completion_tokens
				   FROM llm_usage_logs WHERE api_key_id = ? ORDER BY id DESC`, apiKeyID)
			if err != nil {
				return err
			}
			defer rows.Close()

			var out []billingCosts
			for rows.Next() {
				var c billingCosts
				if err := rows.Scan(&c.CostVendorMicros, &c.CostPlatformMicros, &c.CompletionTokens); err != nil {
					return err
				}
				out = append(out, c)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			got = out
			if len(out) < n {
				return fmt.Errorf("currently %d rows, expected >= %d", len(out), n)
			}
			return nil
		})
	return got
}

// setPlatformCurrency 经 aigatectl 设置平台结算币种。
func setPlatformCurrency(t *testing.T, baseURL, currency string) {
	t.Helper()
	adminCLI(t, requireEnv(t, envCLIBinKey), baseURL, nil,
		"settings", "update", "--data", "@"+cliDataFile(t, "settings.json", map[string]any{
			"platform_currency": currency,
		}))
}

// TestE2E_BillingCostLoop 覆盖一条完整链路：CNY 计价模型 + 空闲价目 + CNY→USD 汇率，
// 一次请求的金额要同时出现在明细表、日聚合表、报表合计，并驱动金额配额。
func TestE2E_BillingCostLoop(t *testing.T) {
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	mock := newUpstreamMock(kindOpenAI)
	// 上游固定报 completion_tokens=500：金额断言就落在 500 × 单价 这个整值上。
	// 只覆盖非流式报文；流式分片保持默认，本用例也只发非流式请求。
	mock.setNonStreamBody(`{"id":"chatcmpl-billing","object":"chat.completion","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"hello from mock"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":0,"completion_tokens":500,"total_tokens":500}}`)
	t.Cleanup(mock.Close)

	gw := startGateway(t, bin, dsn)
	db := openE2EDB(t, dsn)
	waitFor(t, "database reachable", usagePollTimeout, db.Ping)

	// 报价时刻取「从现在起一小时前」：价目与其生效时刻都落在本进程内，不受宿主机时钟精度影响
	// （MySQL DATETIME 只保留到秒，取整会落在同一秒内，仍可选中）。
	effectiveAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)

	// 金额配额（第 7 步）的密钥要在第 8 步按窗口复核，故提到外层。
	var (
		costModelID    int64
		costQuotaKeyID int64
	)

	t.Run("1. a CNY model with an off-peak price and a CNY→USD rate", func(t *testing.T) {
		// 平台结算币种必须是 USD，否则 CNY 模型恒按恒等汇率记账，折算路径不会被走到。
		status, body := doRequest(t, http.MethodGet, gw.baseURL+"/admin/settings", adminToken, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /admin/settings expected 200, got %d, response: %s", status, body)
		}
		var before struct {
			PlatformCurrency string `json:"platform_currency"`
		}
		if err := json.Unmarshal(body, &before); err != nil {
			t.Fatalf("failed to parse settings: %v, response: %s", err, body)
		}
		if before.PlatformCurrency == "CNY" {
			// 默认结算币种就是 CNY：改为 USD 后数据面按 CNY→USD 折算。
			setPlatformCurrency(t, gw.baseURL, "USD")
		}

		costModelID = newBillingModel(t, gw.baseURL, mock.URL(), "billing-cost-loop", map[string]any{
			"enable_peak_pricing": false,
			"use_upstream_cost":   false,
		})

		// 1 CNY = 0.14 USD：rate_micros 即 0.14 × 1e6。
		createRate(t, gw.baseURL, "CNY", "USD", rateCNYToUSDMicros, effectiveAt)

		// 只配空闲输出单价（4 元/百万 token），高峰三列留空：未启用峰谷的模型取这三列。
		createPrice(t, gw.baseURL, costModelID, map[string]any{
			"effective_at":              effectiveAt,
			"offpeak_output_micros":     priceOutputMicrosPerMtok,
			"offpeak_input_hit_micros":  0,
			"offpeak_input_miss_micros": 0,
		})
	})

	t.Run("2. one request is billed 2000 CNY micros and converted to 280 USD micros", func(t *testing.T) {
		created := createKey(t, gw.baseURL, map[string]any{
			"name": "billing-loop", "user_id": 1, "group_id": 0,
			"bindings": []map[string]any{{"ai_model_id": costModelID, "role": "primary"}},
		})
		if created.Token == "" {
			t.Fatal("keys create response is missing the one-time plaintext token")
		}
		costQuotaKeyID = created.ID

		status, body := postChatCompletion(t, gw.baseURL, created.Token, map[string]any{
			"model":    "billing-cost-loop",
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
		if status != http.StatusOK {
			t.Fatalf("the first billed request expected 200, got %d, response: %s", status, body)
		}

		rows := waitForKeyUsageCosts(t, db, created.ID, 1)
		row := rows[0]
		// 上游 mock 固定回 completion_tokens=500：500 × 4 元/百万 = 0.002 元 = 2_000 微元；
		// 按 0.14 折算 = 280 微元（美元）。
		if row.CostVendorMicros != wantVendorMicros || row.CostPlatformMicros != wantPlatformMicros {
			t.Fatalf("llm_usage_logs row = %+v, want vendor %d / platform %d",
				row, wantVendorMicros, wantPlatformMicros)
		}
		if row.CompletionTokens != mockCompletionTokens {
			t.Fatalf("completion_tokens = %d, want %d from the mock upstream", row.CompletionTokens, mockCompletionTokens)
		}
	})

	t.Run("3. daily_usage_summaries agrees for the same dimension", func(t *testing.T) {
		// 日聚合是 UpsertDailyUsage 的累加结果；密钥是本次新建的，所以它的行只有上面这一笔。
		var (
			vendor    int64
			platform  int64
			currency  string
			rowsCount int
		)
		if err := db.QueryRow(
			`SELECT COUNT(*), COALESCE(SUM(cost_vendor_micros), 0), COALESCE(SUM(cost_platform_micros), 0),
			        COALESCE(MAX(currency), '')
			   FROM daily_usage_summaries WHERE api_key_id = ? AND ai_model_id = ?`,
			costQuotaKeyID, costModelID,
		).Scan(&rowsCount, &vendor, &platform, &currency); err != nil {
			t.Fatalf("read daily_usage_summaries: %v", err)
		}
		if rowsCount != 1 {
			t.Fatalf("daily_usage_summaries has %d rows for this key/model, want 1", rowsCount)
		}
		if vendor != wantVendorMicros || platform != wantPlatformMicros || currency != "CNY" {
			t.Errorf("daily_usage_summaries = vendor %d / platform %d / %q, want %d / %d / CNY",
				vendor, platform, currency, wantVendorMicros, wantPlatformMicros)
		}
	})

	t.Run("4. usage summary by model reports the settlement currency and the converted total", func(t *testing.T) {
		type summaryRow struct {
			AIModelID          int64  `json:"ai_model_id"`
			Currency           string `json:"currency"`
			CostVendorMicros   int64  `json:"cost_vendor_micros"`
			CostPlatformMicros int64  `json:"cost_platform_micros"`
		}
		var summary struct {
			GroupBy                 string       `json:"group_by"`
			Rows                    []summaryRow `json:"rows"`
			TotalCostPlatformMicros int64        `json:"total_cost_platform_micros"`
			PlatformCurrency        string       `json:"platform_currency"`
		}
		adminCLI(t, requireEnv(t, envCLIBinKey), gw.baseURL, &summary, "usage", "summary", "--group-by", "model")

		if summary.GroupBy != "model" {
			t.Errorf("group_by echoed as %q, want model", summary.GroupBy)
		}
		if summary.PlatformCurrency == "CNY" {
			t.Errorf("platform_currency = %q, want the configured settlement currency (not the default CNY)",
				summary.PlatformCurrency)
		}
		// 平台的成本列是模型币种金额，跨模型不可相加；合计走结算币种列，必须含这一笔。
		var found bool
		for _, r := range summary.Rows {
			if r.AIModelID != costModelID {
				continue
			}
			found = true
			if r.Currency != "CNY" || r.CostVendorMicros != wantVendorMicros || r.CostPlatformMicros != wantPlatformMicros {
				t.Errorf("summary row = %+v, want CNY %d / %d", r, wantVendorMicros, wantPlatformMicros)
			}
		}
		if !found {
			t.Errorf("group_by=model is missing model %d: %+v", costModelID, summary.Rows)
		}
		// 合计是本库全部行的折算金额之和，只断言「至少含本用例这一笔」。
		if summary.TotalCostPlatformMicros < wantPlatformMicros {
			t.Errorf("total_cost_platform_micros = %d, want >= %d",
				summary.TotalCostPlatformMicros, wantPlatformMicros)
		}
	})

	t.Run("5. CLI round-trip: prices list --id and rates list", func(t *testing.T) {
		var prices struct {
			Prices []struct {
				AIModelID           int64  `json:"ai_model_id"`
				Currency            string `json:"currency"`
				PeakHours           string `json:"peak_hours"`
				OffpeakOutputMicros int64  `json:"offpeak_output_micros"`
				PeakOutputMicros    int64  `json:"peak_output_micros"`
			} `json:"prices"`
		}
		adminCLI(t, requireEnv(t, envCLIBinKey), gw.baseURL, &prices,
			"prices", "list", "--id", fmt.Sprintf("%d", costModelID))
		if len(prices.Prices) == 0 {
			t.Fatalf("prices list --id %d returned no price", costModelID)
		}
		p := prices.Prices[0]
		// 币种缺省取模型币种；未给峰谷时段时 peak_hours 回显 "{}"，高峰三列原样为 0。
		if p.AIModelID != costModelID || p.Currency != "CNY" || p.OffpeakOutputMicros != priceOutputMicrosPerMtok {
			t.Errorf("prices list returned %+v, want CNY / %d for model %d", p, priceOutputMicrosPerMtok, costModelID)
		}
		if p.PeakOutputMicros != 0 {
			t.Errorf("peak_output_micros = %d, want 0 (no peak columns were configured)", p.PeakOutputMicros)
		}

		var rates struct {
			FromCurrency string `json:"from_currency"`
			ToCurrency   string `json:"to_currency"`
			Rates        []struct {
				RateMicros  int64  `json:"rate_micros"`
				EffectiveAt string `json:"effective_at"`
			} `json:"rates"`
		}
		adminCLI(t, requireEnv(t, envCLIBinKey), gw.baseURL, &rates, "rates", "list", "--from", "CNY", "--to", "USD")
		if rates.FromCurrency != "CNY" || rates.ToCurrency != "USD" {
			t.Fatalf("rates list echoed %s/%s, want CNY/USD", rates.FromCurrency, rates.ToCurrency)
		}
		var rateFound bool
		for _, r := range rates.Rates {
			if r.RateMicros == rateCNYToUSDMicros {
				rateFound = true
			}
		}
		if !rateFound {
			t.Errorf("rates list is missing the %d CNY/USD rate this test created: %+v", rateCNYToUSDMicros, rates.Rates)
		}
	})

	t.Run("6. billing calendar: CLI calendar set then list --year", func(t *testing.T) {
		// 网关以 -timezone Asia/Shanghai 启动，列表按该业务时区的年份取值。
		year := time.Now().In(time.FixedZone("CST", 8*3600)).Year()
		// 只登记一天：列表按年返回全部登记日，不因其它用例的数据而受影响地断言具体天数。
		day := fmt.Sprintf("%d-10-01", year)
		setCalendar(t, gw.baseURL, []map[string]any{
			{"date": day, "is_workday": false, "note": "billing e2e holiday"},
		})

		var list struct {
			Year int `json:"year"`
			Days []struct {
				Date      string `json:"date"`
				IsWorkday bool   `json:"is_workday"`
				Note      string `json:"note"`
			} `json:"days"`
		}
		adminCLI(t, requireEnv(t, envCLIBinKey), gw.baseURL, &list,
			"calendar", "list", "--year", fmt.Sprintf("%d", year))
		if list.Year != year {
			t.Fatalf("calendar list --year echoed year %d, want %d", list.Year, year)
		}
		var found bool
		for _, d := range list.Days {
			if d.Date != day {
				continue
			}
			found = true
			if d.IsWorkday || d.Note != "billing e2e holiday" {
				t.Errorf("calendar day %s = %+v, want is_workday=false with the note", day, d)
			}
		}
		if !found {
			t.Errorf("calendar list --year %d is missing %s: %+v", year, day, list.Days)
		}
	})

	t.Run("7. a key-level month cost quota of 1 USD rejects with unit=cost", func(t *testing.T) {
		// 第 2 步那个模型单笔只记 280 微元，攒满 1 USD 要上千次请求；
		// 这里专门建一个高价模型，让十几次请求就跨过 1 USD（汇率复用第 1 步的 CNY→USD）。
		quotaModelID := newBillingModel(t, gw.baseURL, mock.URL(), "billing-cost-quota", map[string]any{
			"enable_peak_pricing": false,
			"use_upstream_cost":   false,
		})
		createPrice(t, gw.baseURL, quotaModelID, map[string]any{
			"effective_at":          effectiveAt,
			"offpeak_output_micros": 1_000_000_000,
		})

		// 新密钥的窗口用量为 0，低于 1 USD 限额：第一次放行。
		created := createKey(t, gw.baseURL, map[string]any{
			"name": "billing-loop-quota", "user_id": 1, "group_id": 0,
			"token_quota_rules": fmt.Sprintf(`[{"mode":"month","limit":%d,"unit":"cost"}]`, costQuotaLimitUnits),
			"bindings":          []map[string]any{{"ai_model_id": quotaModelID, "role": "primary"}},
		})
		if created.Token == "" {
			t.Fatal("keys create response is missing the one-time plaintext token")
		}
		costQuotaKeyID = created.ID

		// 配额读的是「已落库」的金额明细，而落库是异步的：每发一笔都先等它落库再发下一笔，
		// 否则请求跑在落库前面，窗口长期小于 1 USD，永远等不到 429。
		// 这一步也因此断言了「金额配额按入库金额生效」这一时序语义。
		var rejected bool
		for i := 1; i <= maxCostQuotaAttempts; i++ {
			status, body := postChatCompletion(t, gw.baseURL, created.Token, map[string]any{
				"model":    "billing-cost-quota",
				"messages": []map[string]string{{"role": "user", "content": fmt.Sprintf("attempt %d", i)}},
			})
			if status == http.StatusTooManyRequests {
				rejected = true
				var errResp struct {
					Error struct {
						Message  string `json:"message"`
						Unit     string `json:"unit"`
						Currency string `json:"currency"`
					} `json:"error"`
				}
				if err := json.Unmarshal(body, &errResp); err != nil {
					t.Fatalf("failed to parse the 429 body: %v, response: %s", err, body)
				}
				// 金额口径的拒绝必须自报单位与结算币种：调用方据此判断 used/limit 是微元而非 token。
				if errResp.Error.Unit != "cost" {
					t.Errorf("the 429 body error.unit = %q, want cost, response: %s", errResp.Error.Unit, body)
				}
				if errResp.Error.Currency == "" {
					t.Errorf("a cost quota rejection must carry error.currency, response: %s", body)
				}
				break
			}
			if status != http.StatusOK {
				t.Fatalf("attempt %d expected 200 or 429, got %d, response: %s", i, status, body)
			}
			// 等本笔落库后再发下一笔（窗口用量随后由下一笔的配额检查读取）。
			waitForKeyUsageCosts(t, db, created.ID, i)
		}
		if !rejected {
			t.Fatalf("no 429 after %d requests under a 1 USD month cost quota", maxCostQuotaAttempts)
		}
	})

	t.Run("8. the rejection compared converted settlement amounts, not a token count", func(t *testing.T) {
		// 与第 7 步的窗口对齐：同一把密钥、同一个业务时区自然月，只截断到窗口起点之后。
		windowStart := billingMonthStart(time.Now().In(time.FixedZone("CST", 8*3600)))

		var used int64
		if err := db.QueryRow(
			`SELECT COALESCE(SUM(cost_platform_micros), 0) FROM llm_usage_logs
			  WHERE api_key_id = ? AND created_at >= ?`, costQuotaKeyID, windowStart.UTC(),
		).Scan(&used); err != nil {
			t.Fatalf("sum the cost quota window: %v", err)
		}
		// 该密钥每笔记 costQuotaPlatformMicros（0.07 USD）：被拒之前窗口里必然已攒到 1 USD 以上。
		// 这也证明配额比较的是折算后的结算金额，而不是 token 数（500 token 远谈不上任何 token 限额）。
		if used < wantCostQuotaWindowMicros {
			t.Errorf("the cost quota window sees %d platform micros, want >= %d (1 USD): "+
				"the key was rejected on converted amounts, not on tokens", used, wantCostQuotaWindowMicros)
		}

		// 交叉核对：该密钥「计入配额」的明细行数与日聚合累计的请求数一致
		// （两表由同一次记账写入；count_toward_quota = 0 的行只留明细、不进日聚合）。
		// 记账是异步的，末尾几笔可能尚未消费完，故允许日聚合略少于明细；
		// 反过来（日聚合大于明细）则说明重复累加，必须报错。
		var logs, requests int64
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM llm_usage_logs WHERE api_key_id = ? AND count_toward_quota = 1`, costQuotaKeyID).
			Scan(&logs); err != nil {
			t.Fatalf("count llm_usage_logs: %v", err)
		}
		if err := db.QueryRow(`SELECT COALESCE(SUM(request_count), 0) FROM daily_usage_summaries WHERE api_key_id = ?`,
			costQuotaKeyID).Scan(&requests); err != nil {
			t.Fatalf("sum daily_usage_summaries.request_count: %v", err)
		}
		if logs < 2 || requests > logs || logs-requests > 3 {
			t.Errorf("daily_usage_summaries request_count = %d, want it to match the %d quota-counted usage rows "+
				"(async writer may lag by a few rows)", requests, logs)
		}
	})

}

// billingMonthStart 返回业务时区自然月的起点（窗口口径见 quota.Rule.Window）。
func billingMonthStart(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
}

// TestE2E_BillingUpstreamReportedCost 覆盖上游自报成本优先：use_upstream_cost=true 时，
// 上游 usage 里带回的 cost 必须胜过任何已配置价目。
func TestE2E_BillingUpstreamReportedCost(t *testing.T) {
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	// 单独的 mock：它上报的 usage 带 cost，且 completion_tokens 与固定 mock 不同，
	// 使「按价目自算」与「采信上游」得到两个截然不同的数字。
	mock := newUpstreamMock(kindOpenAI)
	mock.setNonStreamBody(`{"id":"chatcmpl-cost","object":"chat.completion","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"hello from mock"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":0,"completion_tokens":100,"total_tokens":100,"cost":0.0035}}`)
	t.Cleanup(mock.Close)

	gw := startGateway(t, bin, dsn)
	db := openE2EDB(t, dsn)
	waitFor(t, "database reachable", usagePollTimeout, db.Ping)

	effectiveAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)

	modelRowID := newBillingModel(t, gw.baseURL, mock.URL(), "billing-upstream-cost", map[string]any{
		"enable_peak_pricing": false,
		"use_upstream_cost":   true,
	})

	// 故意配一份很贵的价目：若采信价目，100 token 会算出 100 × 20 元/百万 = 0.002 元 = 2_000 微元，
	// 与上游自报的 3_500 微元（0.0035 元）不同，据此可区分两条取价路径。
	createPrice(t, gw.baseURL, modelRowID, map[string]any{
		"effective_at":          effectiveAt,
		"offpeak_output_micros": 20_000_000,
	})

	created := createKey(t, gw.baseURL, map[string]any{
		"name": "billing-upstream-cost", "user_id": 1, "group_id": 0,
		"bindings": []map[string]any{{"ai_model_id": modelRowID, "role": "primary"}},
	})

	status, body := postChatCompletion(t, gw.baseURL, created.Token, map[string]any{
		"model":    "billing-upstream-cost",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if status != http.StatusOK {
		t.Fatalf("the request expected 200, got %d, response: %s", status, body)
	}

	rows := waitForKeyUsageCosts(t, db, created.ID, 1)
	row := rows[0]
	// cost=0.0035 → 3500 微元；模型币种为 CNY，平台是否也是 CNY 由设置决定，
	// 故只断言模型币种金额，平台金额按同一口径（CNY→CNY 为恒等）。
	if row.CostVendorMicros != 3500 {
		t.Fatalf("cost_vendor_micros = %d, want 3500 (the upstream-reported cost), row: %+v",
			row.CostVendorMicros, row)
	}
	if row.CostPlatformMicros != row.CostVendorMicros && row.CostPlatformMicros == 0 {
		t.Errorf("cost_platform_micros = %d, a reported cost must also produce a platform amount", row.CostPlatformMicros)
	}
	if row.CompletionTokens != 100 {
		t.Errorf("completion_tokens = %d, want 100 (the usage of this mock)", row.CompletionTokens)
	}
}

// TestE2E_Billing_ModelCapabilitiesInModelList 验证四个能力字段能经管理面往返，
// 并在数据面 GET /v1/models 下发给下游 Agent。
//
// 这些字段是纯能力声明：网关不据此做任何逻辑处理——因此断言的重点是「如实下发」，
// 而不是任何转发行为变化。未配置时下发 null / false / null（区别于 0 与空数组）。
func TestE2E_Billing_ModelCapabilitiesInModelList(t *testing.T) {
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	openAIMock := newUpstreamMock(kindOpenAI)
	t.Cleanup(openAIMock.Close)
	gw := startGateway(t, bin, dsn)

	// 配了能力的模型。
	richID := createModel(t, gw.baseURL, adminToken, map[string]any{
		"provider":          "mock",
		"model_id":          "cap-rich",
		"model_name":        "rich",
		"url":               openAIMock.URL() + "/v1",
		"api_key":           upstreamAPIKey,
		"model_type":        "openai-completions",
		"context_window":    200000,
		"max_output_tokens": 64000,
		"supports_image":    true,
		"effort_levels":     []string{"medium", "low", "high"},
	})
	// 一项能力都没配的模型：用于钉住「未声明」的下发形状。
	bareID := createModel(t, gw.baseURL, adminToken, map[string]any{
		"provider":   "mock",
		"model_id":   "cap-bare",
		"model_name": "bare",
		"url":        openAIMock.URL() + "/v1",
		"api_key":    upstreamAPIKey,
		"model_type": "openai-completions",
	})

	// 管理面往返：effort 由服务端按阶梯升序归一化，而不是照抄入参顺序。
	status, raw := adminJSON(t, gw.baseURL, http.MethodGet, fmt.Sprintf("/admin/models/%d", richID), nil)
	if status != http.StatusOK {
		t.Fatalf("get model expected 200, got %d: %s", status, raw)
	}
	var model struct {
		ContextWindow   int      `json:"context_window"`
		MaxOutputTokens int      `json:"max_output_tokens"`
		SupportsImage   bool     `json:"supports_image"`
		EffortLevels    []string `json:"effort_levels"`
	}
	if err := json.Unmarshal(raw, &model); err != nil {
		t.Fatalf("decode model: %v", err)
	}
	if model.ContextWindow != 200000 || model.MaxOutputTokens != 64000 || !model.SupportsImage {
		t.Fatalf("model capabilities did not round-trip: %s", raw)
	}
	if len(model.EffortLevels) != 3 || model.EffortLevels[0] != "low" || model.EffortLevels[2] != "high" {
		t.Fatalf("effort_levels = %v, want [low medium high]", model.EffortLevels)
	}

	owner := createUser(t, gw.baseURL, "caps-owner", 0)
	key := createKey(t, gw.baseURL, map[string]any{
		"name":    "caps-key",
		"user_id": owner.ID,
		"bindings": []map[string]any{
			{"ai_model_id": richID, "role": "primary"},
			{"ai_model_id": bareID, "role": "fallback"},
		},
	})

	models := listModels(t, gw.baseURL, key.Token)
	byID := make(map[string]map[string]any, len(models))
	for _, m := range models {
		if id, ok := m["id"].(string); ok {
			byID[id] = m
		}
	}

	rich, ok := byID["cap-rich"]
	if !ok {
		t.Fatalf("/v1/models is missing cap-rich: %v", models)
	}
	if rich["context_window"] != float64(200000) || rich["max_output_tokens"] != float64(64000) {
		t.Fatalf("cap-rich limits = %v / %v", rich["context_window"], rich["max_output_tokens"])
	}
	if rich["supports_image"] != true {
		t.Fatalf("cap-rich supports_image = %v, want true", rich["supports_image"])
	}
	levels, ok := rich["effort_levels"].([]any)
	if !ok || len(levels) != 3 || levels[0] != "low" || levels[1] != "medium" || levels[2] != "high" {
		t.Fatalf("cap-rich effort_levels = %v, want [low medium high]", rich["effort_levels"])
	}

	bare, ok := byID["cap-bare"]
	if !ok {
		t.Fatalf("/v1/models is missing cap-bare: %v", models)
	}
	for _, field := range []string{"context_window", "max_output_tokens", "effort_levels"} {
		if v, present := bare[field]; !present || v != nil {
			t.Fatalf("cap-bare %s = %v (present=%v), want an explicit null", field, v, present)
		}
	}
	if bare["supports_image"] != false {
		t.Fatalf("cap-bare supports_image = %v, want false", bare["supports_image"])
	}
}

// adminJSON 以管理员身份发一次管理面 HTTP 请求，返回状态码与响应体。
// 只用于「读回形状」与「故意失败」的契约断言；写操作仍优先走 aigatectl。
func adminJSON(t *testing.T, baseURL, method, path string, body map[string]any) (int, []byte) {
	t.Helper()
	var raw []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s %s body: %v", method, path, err)
		}
		raw = encoded
	}
	return doRequest(t, method, baseURL+path, adminToken, raw)
}

// listModels 以数据面密钥调用 GET /v1/models，返回 data 数组。
func listModels(t *testing.T, baseURL, token string) []map[string]any {
	t.Helper()
	status, raw := doRequest(t, http.MethodGet, baseURL+"/v1/models", token, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/models expected 200, got %d: %s", status, raw)
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode /v1/models: %v", err)
	}
	return out.Data
}
