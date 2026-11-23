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

package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"aigate/internal/billing"
	"aigate/internal/httpx"
	"aigate/internal/model"
)

// 管理面的计费数据：模型价目快照、计费日历、汇率与平台结算币种。
//
// 三张快照表都是「按 effective_at / 日期追加」的不可变集合，数据面把它们缓存在
// billing.StoreSource 里。缓存没有任何自动失效钩子，因此**每个写操作成功后必须调用
// 对应的 Invalidate* 方法**（见各 handler 内的 invalidate* 辅助），否则新价目/新汇率/
// 新日历要等到进程重启才对已运行的数据面可见——那段时间里所有请求都在按旧数据记账。
//
// validatePeakHours 在写入前把时段校验到「可评估或为空」：无法评估的时段会让数据面
// fail-closed 取高峰价，等于系统性多收钱，故在写入口直接拒绝；数据面的 fail-closed
// 规则保留为历史脏数据的兜底。

// dateLayout 是计费日历的日期格式（业务时区日期标签，与每日用量聚合同口径）。
const dateLayout = "2006-01-02"

// maxRateMicros 是汇率上限：1 单位 from 至多等于 1000 单位 to。
// 与 billing.ConvertMicros 的调用约定一致（超限会让折算结果溢出 int64，故在写入侧拒绝）。
const maxRateMicros int64 = 1_000_000_000

// ---------------------------------------------------------------------------
// 当前生效价目
// ---------------------------------------------------------------------------

// currentPrice 返回模型在 now 时刻生效的价目（取 effective_at 不晚于 now 的最新一条）。
// 无生效价目时返回 nil；读取失败时返回错误——模型响应宁可报错，也不静默漏掉价目。
//
// model.Store 没有单行取价接口，这里复用唯一的数据读取入口 ListAIModelPrices
// （按 effective_at 倒序）。管理面是低频读写路径，整表读取某模型的价目代价可接受。
func currentPrice(r *http.Request, store *model.Store, modelID uint) (*priceResponse, error) {
	rows, err := store.ListAIModelPrices(r.Context(), modelID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for i := range rows {
		if rows[i].EffectiveAt.After(now) {
			continue
		}
		out := toPriceResponse(&rows[i])
		return &out, nil
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// 价目
// ---------------------------------------------------------------------------

// priceResponse 是价目快照的对外表示；micros 字段均为「每百万 token 的微元数」。
type priceResponse struct {
	ID          uint   `json:"id"`
	AIModelID   uint   `json:"ai_model_id"`
	Currency    string `json:"currency"`
	EffectiveAt string `json:"effective_at"`
	// PeakHours 原样回显存储的 JSON 文本（便于直接核对厂商价目页），无时段时为 "{}"。
	PeakHours              string `json:"peak_hours"`
	PeakInputHitMicros     int64  `json:"peak_input_hit_micros"`
	PeakInputMissMicros    int64  `json:"peak_input_miss_micros"`
	PeakOutputMicros       int64  `json:"peak_output_micros"`
	OffpeakInputHitMicros  int64  `json:"offpeak_input_hit_micros"`
	OffpeakInputMissMicros int64  `json:"offpeak_input_miss_micros"`
	OffpeakOutputMicros    int64  `json:"offpeak_output_micros"`
	CreatedAt              string `json:"created_at"`
}

// priceCreateRequest 是追加价目快照的请求体；currency 缺省取模型当前币种。
type priceCreateRequest struct {
	Currency               string `json:"currency"`
	EffectiveAt            string `json:"effective_at"`
	PeakHours              string `json:"peak_hours"`
	PeakInputHitMicros     int64  `json:"peak_input_hit_micros"`
	PeakInputMissMicros    int64  `json:"peak_input_miss_micros"`
	PeakOutputMicros       int64  `json:"peak_output_micros"`
	OffpeakInputHitMicros  int64  `json:"offpeak_input_hit_micros"`
	OffpeakInputMissMicros int64  `json:"offpeak_input_miss_micros"`
	OffpeakOutputMicros    int64  `json:"offpeak_output_micros"`
}

// handleListModelPrices 实现 `GET /admin/models/{id}/prices`：列出全部价目快照（新快照在前）。
func handleListModelPrices(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid model id")
			return
		}
		if _, err := store.GetAIModel(r.Context(), id); err != nil {
			writeLookupError(w, err, "model")
			return
		}
		rows, err := store.ListAIModelPrices(r.Context(), id)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		out := make([]priceResponse, 0, len(rows))
		for i := range rows {
			out = append(out, toPriceResponse(&rows[i]))
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"prices": out})
	}
}

// handleCreateModelPrice 实现 `POST /admin/models/{id}/prices`：追加一份价目快照。
// 同一模型同一 effective_at 重复追加返回 409。
func handleCreateModelPrice(store *model.Store, src *billing.StoreSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid model id")
			return
		}
		var req priceCreateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		m, err := store.GetAIModel(r.Context(), id)
		if err != nil {
			writeLookupError(w, err, "model")
			return
		}
		currency, ok := resolvePriceCurrency(req.Currency, m.Currency)
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest,
				"currency must be a 3-letter ISO 4217 code (or set one on the model first)")
			return
		}
		effectiveAt, err := time.Parse(time.RFC3339, strings.TrimSpace(req.EffectiveAt))
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "effective_at must be an RFC3339 timestamp")
			return
		}
		peakHours, msg := normalizePeakHours(req.PeakHours)
		if msg != "" {
			httpx.WriteError(w, http.StatusBadRequest, msg)
			return
		}
		p := &model.AIModelPrice{
			AIModelID:              id,
			Currency:               currency,
			EffectiveAt:            effectiveAt,
			PeakHours:              peakHours,
			PeakInputHitMicros:     req.PeakInputHitMicros,
			PeakInputMissMicros:    req.PeakInputMissMicros,
			PeakOutputMicros:       req.PeakOutputMicros,
			OffpeakInputHitMicros:  req.OffpeakInputHitMicros,
			OffpeakInputMissMicros: req.OffpeakInputMissMicros,
			OffpeakOutputMicros:    req.OffpeakOutputMicros,
		}
		if err := store.CreateAIModelPrice(r.Context(), p); err != nil {
			if model.IsNotFound(err) {
				writeLookupError(w, err, "model")
				return
			}
			if isDuplicateWriteError(err) {
				httpx.WriteError(w, http.StatusConflict,
					fmt.Sprintf("a price for model %d already exists at %s", id, effectiveAt.UTC().Format(time.RFC3339)))
				return
			}
			writeInternalError(w, err)
			return
		}
		invalidateModel(src, id)
		httpx.WriteJSON(w, http.StatusCreated, toPriceResponse(p))
	}
}

// handleDeleteModelPrice 实现 `DELETE /admin/model-prices/{id}`。
func handleDeleteModelPrice(store *model.Store, src *billing.StoreSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid price id")
			return
		}
		// 删除要按 (模型, 价目) 双键失效，先定位价目所属模型；不存在即为 404。
		modelID, found, err := findPriceOwner(r, store, id)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if !found {
			httpx.WriteError(w, http.StatusNotFound, "price not found")
			return
		}
		if err := store.DeleteAIModelPrice(r.Context(), id); err != nil {
			writeInternalError(w, err)
			return
		}
		invalidateModel(src, modelID)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// findPriceOwner 在全量价目里定位某条价目的所属模型。
//
// Store 只提供「按模型列出价目」，没有单行取价接口；管理面是低频写路径，
// 逐模型扫描的代价可以接受，且不必为本模块给数据层新增查询方法。
func findPriceOwner(r *http.Request, store *model.Store, priceID uint) (uint, bool, error) {
	models, err := store.ListAIModels(r.Context())
	if err != nil {
		return 0, false, err
	}
	for i := range models {
		rows, err := store.ListAIModelPrices(r.Context(), models[i].ID)
		if err != nil {
			return 0, false, err
		}
		for j := range rows {
			if rows[j].ID == priceID {
				return models[i].ID, true, nil
			}
		}
	}
	return 0, false, nil
}

// toPriceResponse 将价目实体转为对外表示。
func toPriceResponse(p *model.AIModelPrice) priceResponse {
	peakHours := strings.TrimSpace(p.PeakHours)
	if peakHours == "" {
		peakHours = "{}"
	}
	return priceResponse{
		ID:                     p.ID,
		AIModelID:              p.AIModelID,
		Currency:               p.Currency,
		EffectiveAt:            p.EffectiveAt.UTC().Format(time.RFC3339),
		PeakHours:              peakHours,
		PeakInputHitMicros:     p.PeakInputHitMicros,
		PeakInputMissMicros:    p.PeakInputMissMicros,
		PeakOutputMicros:       p.PeakOutputMicros,
		OffpeakInputHitMicros:  p.OffpeakInputHitMicros,
		OffpeakInputMissMicros: p.OffpeakInputMissMicros,
		OffpeakOutputMicros:    p.OffpeakOutputMicros,
		CreatedAt:              p.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// resolvePriceCurrency 归一化价目币种：缺省取模型当前币种。
//
// 两处都不合法时返回错误——价目的币种是快照，落空串等于这份价目永远不参与金额记账，
// 那是静默漏记，必须在写入口拒绝。
func resolvePriceCurrency(requested, modelCurrency string) (string, bool) {
	raw := requested
	if strings.TrimSpace(raw) == "" {
		raw = modelCurrency
	}
	cur, ok := normalizeCurrency(raw)
	if !ok || cur == "" {
		return "", false
	}
	return cur, true
}

// normalizeCurrency 把币种归一为大写三位字母；不合法时返回 false。
// 空串归一为 ""（调用方按语义决定是否合法）。
func normalizeCurrency(raw string) (string, bool) {
	cur := strings.ToUpper(strings.TrimSpace(raw))
	if cur == "" {
		return "", true
	}
	// 恰好三位 ASCII 大写字母（不用正则：避免为一行判断引入一个包级正则）。
	if len(cur) != 3 {
		return "", false
	}
	for i := 0; i < len(cur); i++ {
		if cur[i] < 'A' || cur[i] > 'Z' {
			return "", false
		}
	}
	return cur, true
}

// normalizePeakHours 校验并规范化 peak_hours，返回可落库的 JSON 文本。
//
// 空串与 "{}"（以及 JSON null）表示无时段配置，落 "{}"；其余必须通过 billing.ParsePeakSchedule，
// 且时区可解析、每个窗口的 days 至少有一个合法值（0..6）、start/end 均为合法 HH:MM——
// 任何一条不满足都返回错误：那样的时段在运行期不可评估，数据面会 fail-closed 取高峰价。
func normalizePeakHours(raw string) (string, string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return "{}", ""
	}
	sched, err := billing.ParsePeakSchedule(trimmed)
	if err != nil {
		return "", "peak_hours must be a JSON object with timezone and windows"
	}
	if sched.Timezone == "" && len(sched.Windows) == 0 {
		return "{}", ""
	}
	if sched.Timezone == "" {
		return "", "peak_hours.timezone is required when windows are configured"
	}
	if _, err := time.LoadLocation(sched.Timezone); err != nil {
		return "", "peak_hours.timezone must be a valid IANA time zone name"
	}
	if len(sched.Windows) == 0 {
		return "", "peak_hours.windows must not be empty when timezone is configured"
	}
	for i, win := range sched.Windows {
		validDay := false
		for _, d := range win.Days {
			if d >= 0 && d <= 6 {
				validDay = true
				break
			}
		}
		if !validDay {
			return "", fmt.Sprintf("peak_hours.windows[%d].days must include at least one day in 0..6", i)
		}
		if !validClock(win.Start) || !validClock(win.End) {
			return "", fmt.Sprintf("peak_hours.windows[%d] start and end must be HH:MM", i)
		}
	}
	// 统一序列化：调用方传的原文本可能带多余空白，落库后回显保持稳定。
	encoded, err := json.Marshal(sched)
	if err != nil {
		return "", "peak_hours cannot be encoded"
	}
	return string(encoded), ""
}

// validClock 校验 "HH:MM"：两位小时（00..23 或 24）与两位分钟（00..59，24 点只允许 24:00）。
// 口径与 billing 包解析峰谷时段时一致，"24:00" 作为当日结束时刻合法。
func validClock(s string) bool {
	if len(s) != 5 || s[2] != ':' {
		return false
	}
	digits := []int{0, 1, 3, 4}
	for _, i := range digits {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	if m > 59 {
		return false
	}
	if h == 24 {
		return m == 0
	}
	return h < 24
}

// ---------------------------------------------------------------------------
// 计费日历
// ---------------------------------------------------------------------------

// calendarDayResponse 是日历一天的对外表示。
type calendarDayResponse struct {
	Date      string `json:"date"`
	IsWorkday bool   `json:"is_workday"`
	Note      string `json:"note"`
}

// calendarDayRequest 是日历登记的一天；is_workday 用指针区分「未传」与「false」。
type calendarDayRequest struct {
	Date      string `json:"date"`
	IsWorkday *bool  `json:"is_workday"`
	Note      string `json:"note"`
}

// calendarUpsertRequest 是 `PUT /admin/billing-calendar` 的请求体（批量覆盖写）。
type calendarUpsertRequest struct {
	Days []calendarDayRequest `json:"days"`
}

// handleListBillingCalendar 实现 `GET /admin/billing-calendar?year=2026[&month=10]`：
// 未给 year 时取业务时区的当年；给了 month（1..12）时只返回该月。
func handleListBillingCalendar(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().In(model.BusinessLocation())
		year, month := now.Year(), 0
		if raw := strings.TrimSpace(r.URL.Query().Get("year")); raw != "" {
			v, err := parseQueryInt(raw)
			if err != nil || v < 1 || v > 9999 {
				httpx.WriteError(w, http.StatusBadRequest, "year must be a 4-digit year")
				return
			}
			year = v
		}
		if raw := strings.TrimSpace(r.URL.Query().Get("month")); raw != "" {
			v, err := parseQueryInt(raw)
			if err != nil || v < 1 || v > 12 {
				httpx.WriteError(w, http.StatusBadRequest, "month must be between 1 and 12")
				return
			}
			month = v
		}
		from := time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC)
		to := time.Date(year, time.December, 31, 0, 0, 0, 0, time.UTC)
		if month != 0 {
			from = time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
			to = from.AddDate(0, 1, -1)
		}
		rows, err := store.ListBillingCalendarDays(r.Context(), from, to)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		days := make([]calendarDayResponse, 0, len(rows))
		for i := range rows {
			days = append(days, calendarDayResponse{
				Date:      rows[i].Date.Format(dateLayout),
				IsWorkday: rows[i].IsWorkday,
				Note:      rows[i].Note,
			})
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"year": year, "days": days})
	}
}

// handleUpsertBillingCalendar 实现 `PUT /admin/billing-calendar`：按日期批量覆盖写。
func handleUpsertBillingCalendar(store *model.Store, src *billing.StoreSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req calendarUpsertRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(req.Days) == 0 {
			httpx.WriteError(w, http.StatusBadRequest, "days must not be empty")
			return
		}
		days := make([]model.BillingCalendarDay, 0, len(req.Days))
		seen := make(map[string]struct{}, len(req.Days))
		for i, d := range req.Days {
			date, ok := parseDateLabel(d.Date)
			if !ok {
				httpx.WriteError(w, http.StatusBadRequest, fmt.Sprintf("days[%d].date must be YYYY-MM-DD", i))
				return
			}
			// is_workday 必须显式给出：省略后按 false 落库会把工作日悄悄变成节假日。
			if d.IsWorkday == nil {
				httpx.WriteError(w, http.StatusBadRequest, fmt.Sprintf("days[%d].is_workday is required", i))
				return
			}
			label := date.Format(dateLayout)
			if _, dup := seen[label]; dup {
				httpx.WriteError(w, http.StatusBadRequest, fmt.Sprintf("days[%d].date duplicates %s", i, label))
				return
			}
			seen[label] = struct{}{}
			days = append(days, model.BillingCalendarDay{Date: date, IsWorkday: *d.IsWorkday, Note: d.Note})
		}
		if err := store.UpsertBillingCalendarDays(r.Context(), days); err != nil {
			writeMutationError(w, err)
			return
		}
		invalidateCalendar(src)
		out := make([]calendarDayResponse, 0, len(days))
		for _, d := range days {
			out = append(out, calendarDayResponse{
				Date:      d.Date.Format(dateLayout),
				IsWorkday: d.IsWorkday,
				Note:      d.Note,
			})
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"days": out})
	}
}

// handleDeleteBillingCalendarDay 实现 `DELETE /admin/billing-calendar/{date}`（{date} 为 YYYY-MM-DD）。
func handleDeleteBillingCalendarDay(store *model.Store, src *billing.StoreSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		date, ok := parseDateLabel(r.PathValue("date"))
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
			return
		}
		if err := store.DeleteBillingCalendarDay(r.Context(), date); err != nil {
			writeInternalError(w, err)
			return
		}
		invalidateCalendar(src)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// parseDateLabel 解析 YYYY-MM-DD 为业务时区的日期标签（当日 00:00 的 UTC 表示）。
func parseDateLabel(raw string) (time.Time, bool) {
	d, err := time.Parse(dateLayout, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, false
	}
	return d, true
}

// ---------------------------------------------------------------------------
// 汇率
// ---------------------------------------------------------------------------

// rateResponse 是汇率快照的对外表示：1 单位 from = rate_micros/1e6 单位 to。
type rateResponse struct {
	ID           uint   `json:"id"`
	FromCurrency string `json:"from_currency"`
	ToCurrency   string `json:"to_currency"`
	EffectiveAt  string `json:"effective_at"`
	RateMicros   int64  `json:"rate_micros"`
	CreatedAt    string `json:"created_at"`
}

// rateCreateRequest 是追加汇率快照的请求体。
type rateCreateRequest struct {
	FromCurrency string `json:"from_currency"`
	ToCurrency   string `json:"to_currency"`
	EffectiveAt  string `json:"effective_at"`
	RateMicros   int64  `json:"rate_micros"`
}

// handleListCurrencyRates 实现 `GET /admin/currency-rates?from=&to=`（币种对必填）。
func handleListCurrencyRates(store *model.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		from, ok := normalizeCurrency(r.URL.Query().Get("from"))
		if !ok || from == "" {
			httpx.WriteError(w, http.StatusBadRequest, "from must be a 3-letter ISO 4217 code")
			return
		}
		to, ok := normalizeCurrency(r.URL.Query().Get("to"))
		if !ok || to == "" {
			httpx.WriteError(w, http.StatusBadRequest, "to must be a 3-letter ISO 4217 code")
			return
		}
		rows, err := store.ListCurrencyRates(r.Context(), from, to)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		out := make([]rateResponse, 0, len(rows))
		for i := range rows {
			out = append(out, toRateResponse(&rows[i]))
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"from_currency": from,
			"to_currency":   to,
			"rates":         out,
		})
	}
}

// handleCreateCurrencyRate 实现 `POST /admin/currency-rates`：追加一条汇率快照。
// 同一币种对同一 effective_at 重复追加返回 409。
func handleCreateCurrencyRate(store *model.Store, src *billing.StoreSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req rateCreateRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		from, ok := normalizeCurrency(req.FromCurrency)
		if !ok || from == "" {
			httpx.WriteError(w, http.StatusBadRequest, "from_currency must be a 3-letter ISO 4217 code")
			return
		}
		to, ok := normalizeCurrency(req.ToCurrency)
		if !ok || to == "" {
			httpx.WriteError(w, http.StatusBadRequest, "to_currency must be a 3-letter ISO 4217 code")
			return
		}
		// 同币种折算恒为 1，不落行（记账时直接按恒等汇率处理）。
		if from == to {
			httpx.WriteError(w, http.StatusBadRequest, "from_currency and to_currency must differ")
			return
		}
		effectiveAt, err := time.Parse(time.RFC3339, strings.TrimSpace(req.EffectiveAt))
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "effective_at must be an RFC3339 timestamp")
			return
		}
		// 汇率上限：1 单位 from 至多等于 1000 单位 to（billing.ConvertMicros 的调用约定）。
		if req.RateMicros <= 0 || req.RateMicros > maxRateMicros {
			httpx.WriteError(w, http.StatusBadRequest,
				fmt.Sprintf("rate_micros must be between 1 and %d", maxRateMicros))
			return
		}
		rate := &model.CurrencyRate{
			FromCurrency: from,
			ToCurrency:   to,
			EffectiveAt:  effectiveAt,
			RateMicros:   req.RateMicros,
		}
		if err := store.CreateCurrencyRate(r.Context(), rate); err != nil {
			if isDuplicateWriteError(err) {
				httpx.WriteError(w, http.StatusConflict,
					fmt.Sprintf("a rate for %s/%s already exists at %s",
						from, to, effectiveAt.UTC().Format(time.RFC3339)))
				return
			}
			writeInternalError(w, err)
			return
		}
		invalidateRates(src)
		httpx.WriteJSON(w, http.StatusCreated, toRateResponse(rate))
	}
}

// handleDeleteCurrencyRate 实现 `DELETE /admin/currency-rates/{id}`。
func handleDeleteCurrencyRate(store *model.Store, src *billing.StoreSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := httpx.ParseUintID(r, "id")
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "invalid rate id")
			return
		}
		if err := store.DeleteCurrencyRate(r.Context(), id); err != nil {
			writeInternalError(w, err)
			return
		}
		invalidateRates(src)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// toRateResponse 将汇率实体转为对外表示。
func toRateResponse(c *model.CurrencyRate) rateResponse {
	return rateResponse{
		ID:           c.ID,
		FromCurrency: c.FromCurrency,
		ToCurrency:   c.ToCurrency,
		EffectiveAt:  c.EffectiveAt.UTC().Format(time.RFC3339),
		RateMicros:   c.RateMicros,
		CreatedAt:    c.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// ---------------------------------------------------------------------------
// 缓存失效
// ---------------------------------------------------------------------------

// invalidateModel 清掉某模型的计价配置与价目缓存。
func invalidateModel(src *billing.StoreSource, modelID uint) {
	if src != nil {
		src.InvalidateModel(modelID)
	}
}

// invalidateCalendar 清掉日历缓存。
func invalidateCalendar(src *billing.StoreSource) {
	if src != nil {
		src.InvalidateCalendar()
	}
}

// invalidateRates 清掉全部汇率缓存。
func invalidateRates(src *billing.StoreSource) {
	if src != nil {
		src.InvalidateRates()
	}
}

// invalidatePlatformCurrency 清掉平台结算币种缓存。
func invalidatePlatformCurrency(src *billing.StoreSource) {
	if src != nil {
		src.InvalidatePlatformCurrency()
	}
}

// isDuplicateWriteError 判定唯一键冲突（映射为 409）。
//
// 【依赖】该判定成立的前提是 model.Open 开启了 gorm.Config.TranslateError: true
// （它把驱动错误翻译成 gorm.ErrDuplicatedKey）。关掉那个开关后，驱动只会回裸的
// "UNIQUE constraint failed: ..." / "Duplicate entry ..." 文本，errors.Is 恒为 false，
// 重复写入会退回 500——这里保留一条文本兜底，使两代驱动错误都能被判为 409。
// 管理面其余代码不 import gorm；本文件只用它的哨兵值，不触达任何数据库会话。
func isDuplicateWriteError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "Duplicate entry") ||
		strings.Contains(msg, "Error 1062")
}

// parseQueryInt 解析查询参数里的十进制整数（非法或为空返回错误）。
func parseQueryInt(raw string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(raw))
}
