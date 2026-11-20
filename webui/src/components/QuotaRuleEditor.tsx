import { Button, Field, Input, Select } from "@/components/ui/primitives";
import { useI18n } from "@/i18n";
import type { QuotaRule } from "@/api/types";

function parse(raw: string): QuotaRule[] {
  if (!raw.trim()) return [];
  try {
    const parsed = JSON.parse(raw);
    return Array.isArray(parsed) ? (parsed as QuotaRule[]) : [];
  } catch {
    return [];
  }
}

function toISO(seconds?: number): string {
  if (!seconds) return "";
  return new Date(seconds * 1000).toISOString().slice(0, 10);
}

function fromISO(value: string): number | undefined {
  if (!value) return undefined;
  return Math.floor(new Date(value + "T00:00:00Z").getTime() / 1000);
}

/** QuotaRuleEditor 用「窗口 + 阈值」行式编辑配额规则，产出 API 需要的 JSON 文本。 */
export function QuotaRuleEditor({ value, onChange }: { value: string; onChange: (next: string) => void }) {
  const { t } = useI18n();
  const rules = parse(value);

  function commit(next: QuotaRule[]) {
    onChange(next.length === 0 ? "" : JSON.stringify(next));
  }

  function update(index: number, patch: Partial<QuotaRule>) {
    commit(rules.map((rule, i) => (i === index ? { ...rule, ...patch } : rule)));
  }

  return (
    <div className="space-y-2">
      {rules.length === 0 ? <p className="text-xs text-slate-500">{t("quota.empty")}</p> : null}
      {rules.map((rule, index) => (
        <div key={index} className="space-y-2 rounded-lg border border-slate-200 p-3">
          <div className="flex gap-2">
            <Field label={t("quota.mode")}>
              <Select
                value={rule.mode}
                onChange={(e) => update(index, { mode: e.target.value as QuotaRule["mode"] })}
              >
                <option value="day">{t("quota.mode.day")}</option>
                <option value="month">{t("quota.mode.month")}</option>
                <option value="year">{t("quota.mode.year")}</option>
                <option value="custom">{t("quota.mode.custom")}</option>
              </Select>
            </Field>
            <Field label={t("quota.limit")}>
              <Input
                type="number"
                min={0}
                value={rule.limit}
                onChange={(e) => update(index, { limit: Number(e.target.value) })}
              />
            </Field>
            <div className="flex items-end">
              <Button variant="ghost" className="text-red-600" onClick={() => commit(rules.filter((_, i) => i !== index))}>
                {t("common.delete")}
              </Button>
            </div>
          </div>
          {rule.mode === "custom" ? (
            <div className="flex gap-2">
              <Field label={t("quota.start")}>
                <Input
                  type="date"
                  value={toISO(rule.start)}
                  onChange={(e) => update(index, { start: fromISO(e.target.value) })}
                />
              </Field>
              <Field label={t("quota.end")}>
                <Input
                  type="date"
                  value={toISO(rule.end)}
                  onChange={(e) => update(index, { end: fromISO(e.target.value) })}
                />
              </Field>
              <Field label={t("quota.refresh")}>
                <Select
                  value={rule.refresh ?? "none"}
                  onChange={(e) => update(index, { refresh: e.target.value as QuotaRule["refresh"] })}
                >
                  <option value="daily">{t("quota.refresh.daily")}</option>
                  <option value="monthly">{t("quota.refresh.monthly")}</option>
                  <option value="yearly">{t("quota.refresh.yearly")}</option>
                  <option value="none">{t("quota.refresh.none")}</option>
                </Select>
              </Field>
            </div>
          ) : null}
        </div>
      ))}
      <Button variant="secondary" onClick={() => commit([...rules, { mode: "day", limit: 0 }])}>
        {t("quota.add")}
      </Button>
    </div>
  );
}
