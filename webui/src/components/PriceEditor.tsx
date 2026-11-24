import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import type { AIModelPrice, AIGateModel, PriceInput } from "@/api/types";
import { ConfirmDialog, Drawer } from "@/components/ui/Overlay";
import { QueryError } from "@/components/ui/QueryError";
import { useToast } from "@/components/ui/Toast";
import { Badge, Button, EmptyState, ErrorText, Field, Input, Select, Spinner } from "@/components/ui/primitives";
import { useI18n } from "@/i18n";
import type { MessageKey } from "@/i18n/en";
import { api } from "@/lib/api";
import { formatDateTime, toLocalInput } from "@/lib/format";
import { inputToMicros, microsToInput } from "@/lib/micros";

import { PeakHoursEditor, parsePeakHours } from "./PeakHoursEditor";

// PRICE_FIELDS 是六档单价的字段名：峰三列 / 谷三列，都是「每百万 token 的微元数」。
const PEAK_FIELDS = ["peak_input_hit_micros", "peak_input_miss_micros", "peak_output_micros"] as const;
const OFFPEAK_FIELDS = ["offpeak_input_hit_micros", "offpeak_input_miss_micros", "offpeak_output_micros"] as const;
type PriceField = (typeof PEAK_FIELDS)[number] | (typeof OFFPEAK_FIELDS)[number];

type PriceDraft = Record<PriceField, string>;

// CURRENCY_OPTIONS 是计价币种下拉的常见选项；下拉只是便利，服务端只校验三位大写。
const CURRENCY_OPTIONS = ["CNY", "USD", "HKD", "EUR", "GBP", "JPY", "SGD", "AUD", "CAD", "KRW"];

/** CURRENCY_CUSTOM 是币种下拉里「手输」的哨兵值，不是合法币种。 */
const CURRENCY_CUSTOM = "__custom";

const ZERO_DRAFT: PriceDraft = {
  peak_input_hit_micros: "0",
  peak_input_miss_micros: "0",
  peak_output_micros: "0",
  offpeak_input_hit_micros: "0",
  offpeak_input_miss_micros: "0",
  offpeak_output_micros: "0",
};

function draftFrom(price: AIModelPrice | null): PriceDraft {
  if (!price) return ZERO_DRAFT;
  return {
    peak_input_hit_micros: microsToInput(price.peak_input_hit_micros),
    peak_input_miss_micros: microsToInput(price.peak_input_miss_micros),
    peak_output_micros: microsToInput(price.peak_output_micros),
    offpeak_input_hit_micros: microsToInput(price.offpeak_input_hit_micros),
    offpeak_input_miss_micros: microsToInput(price.offpeak_input_miss_micros),
    offpeak_output_micros: microsToInput(price.offpeak_output_micros),
  };
}

/** timestampOf 把 datetime-local 文本解析成毫秒；空文本或非法值返回 NaN。 */
function timestampOf(local: string): number {
  return local ? new Date(local).getTime() : Number.NaN;
}

/**
 * PriceEditor 是一个模型的价目快照抽屉：上方按生效时刻倒序列出版本，下方追加新快照。
 * 单价以币种单位录入（如 1.5 元/百万 token），提交前换算为整数微元；回显时反向换算。
 */
export function PriceEditor({ model, onClose }: { model: AIGateModel; onClose: () => void }) {
  const { t } = useI18n();
  const toast = useToast();
  const client = useQueryClient();
  const query = useQuery({
    queryKey: ["model-prices", model.id],
    queryFn: () => api<{ prices: AIModelPrice[] }>("/admin/models/" + model.id + "/prices"),
  });

  const prices = query.data?.prices ?? [];
  const latest = prices[0] ?? null;
  const [draft, setDraft] = useState<PriceDraft>(ZERO_DRAFT);
  const [currency, setCurrency] = useState(model.currency);
  // 已有币种不在常见选项里时，下拉切到「手输」并保留原值。
  const [customCurrency, setCustomCurrency] = useState(
    model.currency !== "" && !CURRENCY_OPTIONS.includes(model.currency),
  );
  // 两个开关是模型级配置（api_models 列），随本抽屉的「保存设置」提交，不随价目快照变化。
  const [peakPricing, setPeakPricing] = useState(model.enable_peak_pricing);
  const [upstreamCost, setUpstreamCost] = useState(model.use_upstream_cost);
  const [savingSettings, setSavingSettings] = useState(false);
  const [settingsError, setSettingsError] = useState("");
  const [effectiveAt, setEffectiveAt] = useState(() => toLocalInput(new Date()));
  const [peakHours, setPeakHours] = useState("{}");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [deleting, setDeleting] = useState<AIModelPrice | null>(null);

  // 用最新版本预填表单，管理员改一个数就能生成新快照；只在取到数据后一次性回填。
  const [hydrated, setHydrated] = useState(false);
  if (!hydrated && latest) {
    setDraft(draftFrom(latest));
    setPeakHours(latest.peak_hours || "{}");
    if (!model.currency) setCurrency(latest.currency);
    setHydrated(true);
  }

  const peakEnabled = peakPricing;

  // saveSettings 把币种与两个开关一起写回模型：币种决定价目单位，必须在追加价目之前落地。
  async function saveSettings() {
    setSettingsError("");
    const code = currency.trim().toUpperCase();
    setSavingSettings(true);
    try {
      await api("/admin/models/" + model.id, {
        method: "PUT",
        body: JSON.stringify({
          currency: code,
          enable_peak_pricing: peakPricing,
          use_upstream_cost: upstreamCost,
        }),
      });
      await client.invalidateQueries({ queryKey: ["models"] });
      await client.invalidateQueries({ queryKey: ["model-prices", model.id] });
      toast(t("common.saved"), "success");
    } catch (err) {
      setSettingsError(err instanceof Error ? err.message : String(err));
    } finally {
      setSavingSettings(false);
    }
  }

  async function submit() {
    setError("");
    const currencyCode = currency.trim().toUpperCase();
    if (currencyCode === "") {
      setError(t("price.currencyRequired"));
      return;
    }
    const when = timestampOf(effectiveAt);
    if (Number.isNaN(when)) {
      setError(t("price.effectiveRequired"));
      return;
    }
    if (latest && when === timestampOf(toLocalInput(new Date(latest.effective_at)))) {
      // 服务端对同一模型同一 effective_at 返回 409；这里先拦下来，省一次往返。
      setError(t("price.duplicateEffective"));
      return;
    }
    const payload: Partial<Record<PriceField, number>> = {};
    const fields = peakEnabled ? [...PEAK_FIELDS, ...OFFPEAK_FIELDS] : [...OFFPEAK_FIELDS];
    for (const field of fields) {
      const micros = inputToMicros(draft[field]);
      if (micros === null) {
        setError(t("price.invalidAmount", { field: t(FIELD_LABELS[field]) }));
        return;
      }
      payload[field] = micros;
    }
    const schedule = parsePeakHours(peakHours);
    if (peakEnabled && schedule.windows.length > 0 && schedule.timezone === "") {
      setError(t("peak.timezoneRequired"));
      return;
    }

    setBusy(true);
    try {
      const body: PriceInput = {
        currency: currencyCode,
        effective_at: new Date(when).toISOString(),
        peak_hours: peakEnabled ? peakHours : "{}",
        peak_input_hit_micros: payload.peak_input_hit_micros ?? 0,
        peak_input_miss_micros: payload.peak_input_miss_micros ?? 0,
        peak_output_micros: payload.peak_output_micros ?? 0,
        offpeak_input_hit_micros: payload.offpeak_input_hit_micros ?? 0,
        offpeak_input_miss_micros: payload.offpeak_input_miss_micros ?? 0,
        offpeak_output_micros: payload.offpeak_output_micros ?? 0,
      };
      await api("/admin/models/" + model.id + "/prices", { method: "POST", body: JSON.stringify(body) });
      await client.invalidateQueries({ queryKey: ["model-prices", model.id] });
      await client.invalidateQueries({ queryKey: ["models"] });
      toast(t("common.saved"), "success");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function remove(price: AIModelPrice) {
    setBusy(true);
    try {
      await api("/admin/model-prices/" + price.id, { method: "DELETE" });
      await client.invalidateQueries({ queryKey: ["model-prices", model.id] });
      await client.invalidateQueries({ queryKey: ["models"] });
      toast(t("common.saved"), "success");
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
      setDeleting(null);
    }
  }

  return (
    <Drawer
      open
      title={t("price.title", { model: model.model_name })}
      onClose={onClose}
      footer={<Button variant="secondary" onClick={onClose}>{t("common.close")}</Button>}
    >
      {query.isError ? (
        <QueryError error={query.error} onRetry={() => void query.refetch()} />
      ) : query.isLoading ? (
        <Spinner label={t("common.loading")} />
      ) : (
        <div className="space-y-4">
          <section className="space-y-3 rounded-lg border border-slate-200 bg-slate-50/60 p-3">
            <div className="text-sm font-medium text-slate-700">{t("price.settings")}</div>
            <Field label={t("price.currency")} hint={t("price.currencyHint")}>
              <Select
                value={customCurrency ? CURRENCY_CUSTOM : currency}
                onChange={(e) => {
                  if (e.target.value === CURRENCY_CUSTOM) {
                    setCustomCurrency(true);
                    return;
                  }
                  setCustomCurrency(false);
                  setCurrency(e.target.value);
                }}
                data-testid="price-currency-select"
              >
                <option value="">{t("price.currency.none")}</option>
                {CURRENCY_OPTIONS.map((code) => (
                  <option key={code} value={code}>
                    {code}
                  </option>
                ))}
                <option value={CURRENCY_CUSTOM}>{t("price.currency.custom")}</option>
              </Select>
            </Field>
            {customCurrency ? (
              <Field label={t("price.currencyCode")}>
                <Input
                  value={currency}
                  onChange={(e) => setCurrency(e.target.value)}
                  placeholder="CNY"
                  maxLength={3}
                  data-testid="price-currency-input"
                />
              </Field>
            ) : null}
            <label className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={peakPricing}
                onChange={(e) => setPeakPricing(e.target.checked)}
                data-testid="price-peak-pricing"
              />
              {t("price.enablePeakPricing")}
            </label>
            <p className="text-xs text-slate-500">{t("price.enablePeakPricingHint")}</p>
            <label className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={upstreamCost}
                onChange={(e) => setUpstreamCost(e.target.checked)}
                data-testid="price-upstream-cost"
              />
              {t("price.useUpstreamCost")}
            </label>
            <p className="text-xs text-slate-500">{t("price.useUpstreamCostHint")}</p>
            <p className="text-xs text-slate-500">{t("price.settingsHint")}</p>
            {settingsError ? <ErrorText>{settingsError}</ErrorText> : null}
            <Button variant="secondary" onClick={() => void saveSettings()} disabled={savingSettings}>
              {t("price.saveSettings")}
            </Button>
          </section>

          <section>
            <div className="mb-2 text-sm font-medium text-slate-700">{t("price.versions")}</div>
            {prices.length === 0 ? (
              <EmptyState title={t("price.empty")} description={t("price.emptyHint")} />
            ) : (
              <div className="space-y-2" data-testid="price-versions">
                {prices.map((price, index) => (
                  <div key={price.id} className="rounded-lg border border-slate-200 p-3 text-sm" data-testid="price-version">
                    <div className="flex items-center justify-between gap-2">
                      <div className="flex items-center gap-2">
                        {index === 0 ? <Badge tone="green">{t("price.current")}</Badge> : null}
                        <span className="font-medium">{formatDateTime(price.effective_at)}</span>
                        <Badge>{price.currency}</Badge>
                      </div>
                      <Button variant="ghost" className="text-red-600" onClick={() => setDeleting(price)} disabled={busy}>
                        {t("common.delete")}
                      </Button>
                    </div>
                    <dl className="mt-2 grid grid-cols-2 gap-x-4 gap-y-1 text-xs text-slate-600">
                      <PriceLine prefix="peak" price={price} />
                      <PriceLine prefix="offpeak" price={price} />
                    </dl>
                  </div>
                ))}
              </div>
            )}
          </section>

          <section className="space-y-3 rounded-lg border border-slate-200 p-3">
            <div className="text-sm font-medium text-slate-700">{t("price.addTitle")}</div>
            <div className="flex items-center gap-2 text-xs text-slate-600" data-testid="price-currency-unit">
              <Badge>{currency.trim().toUpperCase() || "—"}</Badge>
              <span>{t("price.currencyFromSettings")}</span>
            </div>
            <Field label={t("price.effectiveAt")}>
              <Input
                type="datetime-local"
                value={effectiveAt}
                onChange={(e) => setEffectiveAt(e.target.value)}
                data-testid="price-effective-at"
              />
            </Field>

            {peakEnabled ? (
              <PriceGroup
                title={t("price.peak")}
                fields={PEAK_FIELDS}
                draft={draft}
                currency={currency}
                onChange={(field, value) => setDraft((prev) => ({ ...prev, [field]: value }))}
              />
            ) : (
              <p className="text-xs text-slate-500">{t("price.peakDisabledHint")}</p>
            )}
            <PriceGroup
              title={t("price.offpeak")}
              fields={OFFPEAK_FIELDS}
              draft={draft}
              currency={currency}
              onChange={(field, value) => setDraft((prev) => ({ ...prev, [field]: value }))}
            />

            {peakEnabled ? (
              <div>
                <div className="mb-1 text-sm font-medium text-slate-700">{t("price.peakHours")}</div>
                <PeakHoursEditor value={peakHours} onChange={setPeakHours} />
              </div>
            ) : null}

            {error ? <ErrorText>{error}</ErrorText> : null}
            <Button onClick={() => void submit()} disabled={busy}>
              {t("price.add")}
            </Button>
          </section>
        </div>
      )}

      <ConfirmDialog
        open={deleting !== null}
        title={t("common.delete")}
        description={deleting ? formatDateTime(deleting.effective_at) : undefined}
        danger
        busy={busy}
        onCancel={() => setDeleting(null)}
        onConfirm={() => deleting && void remove(deleting)}
      />
    </Drawer>
  );
}

const FIELD_LABELS: Record<PriceField, MessageKey> = {
  peak_input_hit_micros: "price.inputHit",
  peak_input_miss_micros: "price.inputMiss",
  peak_output_micros: "price.output",
  offpeak_input_hit_micros: "price.inputHit",
  offpeak_input_miss_micros: "price.inputMiss",
  offpeak_output_micros: "price.output",
};

// TIER_LABELS 是三个计价档位的展示名，顺序与 *_FIELDS 一致。
const TIER_LABELS: MessageKey[] = ["price.inputHit", "price.inputMiss", "price.output"];

function PriceLine({ prefix, price }: { prefix: "peak" | "offpeak"; price: AIModelPrice }) {
  const { t } = useI18n();
  const values =
    prefix === "peak"
      ? [price.peak_input_hit_micros, price.peak_input_miss_micros, price.peak_output_micros]
      : [price.offpeak_input_hit_micros, price.offpeak_input_miss_micros, price.offpeak_output_micros];
  return (
    <div className="col-span-2 flex flex-wrap gap-x-3 gap-y-1">
      <span className="font-medium text-slate-700">{t(prefix === "peak" ? "price.peak" : "price.offpeak")}</span>
      {values.map((micros, index) => (
        <span key={index}>
          {t(TIER_LABELS[index])} {microsToInput(micros)}
        </span>
      ))}
    </div>
  );
}

function PriceGroup({
  title,
  fields,
  draft,
  currency,
  onChange,
}: {
  title: string;
  fields: readonly PriceField[];
  draft: PriceDraft;
  currency: string;
  onChange: (field: PriceField, value: string) => void;
}) {
  const { t } = useI18n();
  return (
    <div className="rounded-lg border border-slate-200 p-3">
      <div className="mb-2 text-xs font-medium text-slate-500">{title}</div>
      <div className="grid grid-cols-3 gap-2">
        {fields.map((field) => (
          <Field key={field} label={t(FIELD_LABELS[field])}>
            <Input
              type="text"
              inputMode="decimal"
              value={draft[field]}
              onChange={(e) => onChange(field, e.target.value)}
              data-testid={"price-" + field}
            />
          </Field>
        ))}
      </div>
      <p className="mt-1 text-xs text-slate-500">{t("price.amountHint", { currency: currency.trim().toUpperCase() || "—" })}</p>
    </div>
  );
}