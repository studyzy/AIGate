import { useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";

import type { Settings } from "@/api/types";
import { QuotaRuleEditor } from "@/components/QuotaRuleEditor";
import { QueryError } from "@/components/ui/QueryError";
import { useToast } from "@/components/ui/Toast";
import { Button, Field, Input, PageHeader, Select, Spinner } from "@/components/ui/primitives";
import { useApiMutation } from "@/hooks/useApiMutation";
import { useI18n } from "@/i18n";
import { api } from "@/lib/api";

// CURRENCY_OPTIONS 是常见 ISO 4217 结算币种选项。
const CURRENCY_OPTIONS = ["CNY", "USD", "HKD", "EUR", "GBP", "JPY", "SGD", "AUD", "CAD", "KRW"];

export function SettingsPage() {
  const { t } = useI18n();
  const toast = useToast();
  const query = useQuery({ queryKey: ["settings"], queryFn: () => api<Settings>("/admin/settings") });
  const [form, setForm] = useState<Settings | null>(null);

  useEffect(() => {
    if (query.data) setForm(query.data);
  }, [query.data]);

  const save = useApiMutation([["settings"]], (next: Settings) =>
    api<Settings>("/admin/settings", { method: "PUT", body: JSON.stringify(next) }), {
    onSuccess: (data) => {
      setForm(data);
      toast(t("common.saved"), "success");
    },
  });

  // 失败必须先于 loading 判断，否则 form 永远为 null，页面会一直转圈。
  if (query.isError) return <QueryError error={query.error} onRetry={() => void query.refetch()} />;
  if (!form) return <Spinner label={t("common.loading")} />;

  const set = <K extends keyof Settings>(key: K, value: Settings[K]) => setForm((f) => (f ? { ...f, [key]: value } : f));

  return (
    <section className="max-w-3xl">
      <PageHeader
        title={t("settings.title")}
        actions={
          <Button onClick={() => save.mutate(form)} disabled={save.isPending}>
            {t("common.save")}
          </Button>
        }
      />
      <div className="rounded-xl border border-slate-200 bg-white p-4">
        <div className="mb-2 text-sm font-medium text-slate-700">{t("settings.scope")}</div>
        <Select value={form.global_quota_scope} onChange={(e) => set("global_quota_scope", e.target.value)}>
          <option value="site">{t("settings.scope.site")}</option>
          <option value="group">{t("settings.scope.group")}</option>
        </Select>
      </div>
      <div className="mt-4 space-y-4 rounded-xl border border-slate-200 bg-white p-4">
        <div>
          <div className="mb-1 text-sm font-medium text-slate-700">{t("settings.defaultQuota")}</div>
          <QuotaRuleEditor value={form.default_token_quota_rules} onChange={(next) => set("default_token_quota_rules", next)} />
        </div>
        <div>
          <div className="mb-1 text-sm font-medium text-slate-700">{t("settings.globalQuota")}</div>
          <QuotaRuleEditor value={form.global_token_quota_rules} onChange={(next) => set("global_token_quota_rules", next)} />
        </div>
        <Field label={t("settings.platformCurrency")} hint={t("settings.platformCurrencyHint")}>
          <Select
            value={form.platform_currency}
            onChange={(e) => set("platform_currency", e.target.value)}
            data-testid="settings-platform-currency"
          >
            {CURRENCY_OPTIONS.map((code) => (
              <option key={code} value={code}>
                {code}
              </option>
            ))}
            {/* 已配置但不常见的币种也要能显示并保存回去，否则一改别处就被压成默认值。 */}
            {CURRENCY_OPTIONS.includes(form.platform_currency) ? null : (
              <option value={form.platform_currency}>{form.platform_currency}</option>
            )}
          </Select>
        </Field>
        <Field label={t("settings.userKeyLimit")}>
          <Input
            type="number"
            min={0}
            value={form.user_key_limit}
            onChange={(e) => set("user_key_limit", Number(e.target.value))}
          />
        </Field>
      </div>
    </section>
  );
}
