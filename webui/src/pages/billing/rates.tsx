import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import type { CurrencyRate, CurrencyRateInput } from "@/api/types";
import { ConfirmDialog, Drawer } from "@/components/ui/Overlay";
import { QueryError } from "@/components/ui/QueryError";
import { useToast } from "@/components/ui/Toast";
import { Button, EmptyState, ErrorText, Field, Input, PageHeader, Spinner } from "@/components/ui/primitives";
import { useI18n } from "@/i18n";
import { api } from "@/lib/api";
import { formatDateTime, toLocalInput } from "@/lib/format";
import { inputToMicros, microsToInput } from "@/lib/micros";

// CURRENCIES 是常见 ISO 4217 币种，下拉只是便利；两边都允许手输（服务端只校验三位大写）。
const CURRENCIES = ["CNY", "USD", "HKD", "EUR", "GBP", "JPY", "SGD", "AUD", "CAD", "KRW"];

/** CurrencyRatePage 维护「1 单位 from = rate 单位 to」的汇率快照，按币种对成对追加。 */
export function CurrencyRatePage() {
  const { t } = useI18n();
  const toast = useToast();
  const client = useQueryClient();
  const [from, setFrom] = useState("USD");
  const [to, setTo] = useState("CNY");
  const [adding, setAdding] = useState(false);
  const [deleting, setDeleting] = useState<CurrencyRate | null>(null);
  const [busy, setBusy] = useState(false);

  const query = useQuery({
    queryKey: ["currency-rates", from, to],
    queryFn: () =>
      api<{ from_currency: string; to_currency: string; rates: CurrencyRate[] }>(
        "/admin/currency-rates?from=" + encodeURIComponent(from) + "&to=" + encodeURIComponent(to),
      ),
    // 币种必须是三位大写才查得动；输入过程中不发请求。
    enabled: /^[A-Z]{3}$/.test(from) && /^[A-Z]{3}$/.test(to),
  });

  const rates = query.data?.rates ?? [];
  const sameCurrency = from === to;

  async function remove(rate: CurrencyRate) {
    setBusy(true);
    try {
      await api("/admin/currency-rates/" + rate.id, { method: "DELETE" });
      await client.invalidateQueries({ queryKey: ["currency-rates", from, to] });
      toast(t("common.saved"), "success");
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
      setDeleting(null);
    }
  }

  return (
    <section>
      <PageHeader
        title={t("rates.title")}
        actions={
          <Button onClick={() => setAdding(true)} disabled={sameCurrency}>
            {t("rates.new")}
          </Button>
        }
      />

      <p className="mb-3 text-xs text-slate-500">{t("rates.hint")}</p>

      <div className="mb-4 flex flex-wrap items-end gap-2">
        <div className="w-32">
          <Field label={t("rates.from")}>
            <Input
              value={from}
              onChange={(e) => setFrom(e.target.value.trim().toUpperCase())}
              list="rate-currencies"
              data-testid="rate-from"
            />
          </Field>
        </div>
        <span className="pb-2 text-slate-400">→</span>
        <div className="w-32">
          <Field label={t("rates.to")}>
            <Input
              value={to}
              onChange={(e) => setTo(e.target.value.trim().toUpperCase())}
              list="rate-currencies"
              data-testid="rate-to"
            />
          </Field>
        </div>
        <datalist id="rate-currencies">
          {CURRENCIES.map((code) => (
            <option key={code} value={code} />
          ))}
        </datalist>
      </div>

      {sameCurrency ? (
        <p className="text-sm text-amber-600">{t("rates.sameCurrency")}</p>
      ) : query.isError ? (
        <QueryError error={query.error} onRetry={() => void query.refetch()} />
      ) : query.isLoading ? (
        <Spinner label={t("common.loading")} />
      ) : rates.length === 0 ? (
        <EmptyState title={t("rates.empty")} description={t("rates.emptyHint")} />
      ) : (
        <div className="overflow-x-auto rounded-xl border border-slate-200 bg-white">
          <table className="w-full text-sm">
            <thead className="bg-slate-50 text-xs uppercase tracking-wide text-slate-500">
              <tr>
                <th className="px-4 py-2 text-left">{t("rates.pair")}</th>
                <th className="px-4 py-2 text-left">{t("rates.rate")}</th>
                <th className="px-4 py-2 text-left">{t("rates.effectiveAt")}</th>
                <th className="px-4 py-2 text-right">{t("common.actions")}</th>
              </tr>
            </thead>
            <tbody data-testid="rate-list">
              {rates.map((rate) => (
                <tr key={rate.id} className="border-t border-slate-100">
                  <td className="px-4 py-2 font-mono text-xs">
                    {rate.from_currency} → {rate.to_currency}
                  </td>
                  <td className="px-4 py-2 font-medium" data-testid="rate-value">
                    {microsToInput(rate.rate_micros)}
                  </td>
                  <td className="px-4 py-2 text-xs text-slate-600">{formatDateTime(rate.effective_at)}</td>
                  <td className="px-4 py-2 text-right">
                    <Button variant="ghost" className="text-red-600" onClick={() => setDeleting(rate)} disabled={busy}>
                      {t("common.delete")}
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {adding ? (
        <RateDrawer
          from={from}
          to={to}
          onClose={() => setAdding(false)}
          onCreated={() => {
            setAdding(false);
            void client.invalidateQueries({ queryKey: ["currency-rates", from, to] });
          }}
        />
      ) : null}

      <ConfirmDialog
        open={deleting !== null}
        title={t("common.delete")}
        description={deleting ? deleting.from_currency + " → " + deleting.to_currency : undefined}
        danger
        busy={busy}
        onCancel={() => setDeleting(null)}
        onConfirm={() => deleting && void remove(deleting)}
      />
    </section>
  );
}

function RateDrawer({
  from,
  to,
  onClose,
  onCreated,
}: {
  from: string;
  to: string;
  onClose: () => void;
  onCreated: () => void;
}) {
  const { t } = useI18n();
  const toast = useToast();
  const [rate, setRate] = useState("");
  const [effectiveAt, setEffectiveAt] = useState(() => toLocalInput(new Date()));
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit() {
    setError("");
    const micros = inputToMicros(rate);
    // 服务端只接受 1e-6 … 1000（rate_micros 上限 1e9），这里提前给出同样的约束。
    if (micros === null || micros <= 0 || micros > 1_000_000_000) {
      setError(t("rates.invalidRate"));
      return;
    }
    const when = effectiveAt ? new Date(effectiveAt) : null;
    if (!when || Number.isNaN(when.getTime())) {
      setError(t("rates.effectiveRequired"));
      return;
    }
    setBusy(true);
    try {
      const body: CurrencyRateInput = {
        from_currency: from,
        to_currency: to,
        effective_at: when.toISOString(),
        rate_micros: micros,
      };
      await api("/admin/currency-rates", { method: "POST", body: JSON.stringify(body) });
      toast(t("common.saved"), "success");
      onCreated();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Drawer
      open
      title={t("rates.new")}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>{t("common.cancel")}</Button>
          <Button onClick={() => void submit()} disabled={busy}>{t("common.save")}</Button>
        </>
      }
    >
      <div className="space-y-3">
        <p className="text-xs text-slate-500">{t("rates.formHint", { from, to })}</p>
        <Field label={t("rates.rate")} hint={t("rates.rateHint", { from, to })}>
          <Input value={rate} onChange={(e) => setRate(e.target.value)} inputMode="decimal" placeholder="7.1" data-testid="rate-input" />
        </Field>
        <Field label={t("rates.effectiveAt")}>
          <Input type="datetime-local" value={effectiveAt} onChange={(e) => setEffectiveAt(e.target.value)} />
        </Field>
        {error ? <ErrorText>{error}</ErrorText> : null}
      </div>
    </Drawer>
  );
}