import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";

import type { BillingCalendarDay, BillingCalendarInput } from "@/api/types";
import { ConfirmDialog, Drawer } from "@/components/ui/Overlay";
import { QueryError } from "@/components/ui/QueryError";
import { useToast } from "@/components/ui/Toast";
import { Button, EmptyState, ErrorText, Field, PageHeader, Spinner } from "@/components/ui/primitives";
import { useI18n } from "@/i18n";
import { api } from "@/lib/api";

const DATE_PATTERN = /^\d{4}-\d{2}-\d{2}$/;

function pad(n: number): string {
  return String(n).padStart(2, "0");
}

// dateLabel 拼出 YYYY-MM-DD；用本地日期分量而不是 toISOString，避免时区把日期推过午夜。
function dateLabel(year: number, month: number, day: number): string {
  return year + "-" + pad(month) + "-" + pad(day);
}

/** isWeekend 按星期推断默认口径：未登记日期里周一至周五视为工作日，与后端一致。 */
function isWeekend(year: number, month: number, day: number): boolean {
  const weekday = new Date(year, month - 1, day).getDay();
  return weekday === 0 || weekday === 6;
}

// WORKDAY_FLAGS 是 is_workday 列的可接受写法；未列出的值一律视为该行非法。
const WORKDAY_FLAGS: Record<string, boolean | undefined> = {
  "1": true, true: true, yes: true, y: true,
  "0": false, false: false, no: false, n: false,
};

type ParsedLine = { input: BillingCalendarInput; line: number };

/**
 * parseImport 解析「YYYY-MM-DD,is_workday,note」逐行文本。
 * is_workday 接受 1/0、true/false、y/n；note 可省略（其后逗号都算备注内容）。
 */
export function parseImport(text: string): { days: ParsedLine[]; errors: number[] } {
  const days: ParsedLine[] = [];
  const errors: number[] = [];
  const seen = new Set<string>();
  text.split(/\r?\n/).forEach((raw, index) => {
    const line = raw.trim();
    if (line === "") return;
    const parts = line.split(",").map((p) => p.trim());
    const [date, flag, ...rest] = parts;
    const isWorkday = WORKDAY_FLAGS[flag];
    if (!DATE_PATTERN.test(date) || isWorkday === undefined || seen.has(date)) {
      errors.push(index + 1);
      return;
    }
    seen.add(date);
    days.push({ input: { date, is_workday: isWorkday, note: rest.join(",") }, line: index + 1 });
  });
  return { days, errors };
}

/** BillingCalendarPage 是计费日历：按年取已登记日期，月历里点一天即切换工作日/节假日。 */
export function BillingCalendarPage() {
  const { t } = useI18n();
  const toast = useToast();
  const client = useQueryClient();
  const today = useMemo(() => new Date(), []);
  const [year, setYear] = useState(today.getFullYear());
  const [busy, setBusy] = useState(false);
  const [importing, setImporting] = useState(false);

  const query = useQuery({
    queryKey: ["billing-calendar", year],
    queryFn: () => api<{ year: number; days: BillingCalendarDay[] }>("/admin/billing-calendar?year=" + year),
  });

  const overrides = useMemo(() => {
    const map = new Map<string, BillingCalendarDay>();
    (query.data?.days ?? []).forEach((day) => map.set(day.date, day));
    return map;
  }, [query.data]);

  // effectiveWorkday 给出某天最终的「是否工作日」：日历登记优先，未登记按星期推断。
  function effectiveWorkday(month: number, day: number): boolean {
    const registered = overrides.get(dateLabel(year, month, day));
    if (registered) return registered.is_workday;
    return !isWeekend(year, month, day);
  }

  async function save(days: BillingCalendarInput[]) {
    setBusy(true);
    try {
      await api("/admin/billing-calendar", { method: "PUT", body: JSON.stringify({ days }) });
      await client.invalidateQueries({ queryKey: ["billing-calendar", year] });
      toast(t("common.saved"), "success");
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function remove(date: string) {
    setBusy(true);
    try {
      await api("/admin/billing-calendar/" + date, { method: "DELETE" });
      await client.invalidateQueries({ queryKey: ["billing-calendar", year] });
      toast(t("common.saved"), "success");
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section>
      <PageHeader
        title={t("calendar.title")}
        actions={
          <>
            <Button variant="secondary" onClick={() => setYear((y) => y - 1)} aria-label={t("calendar.prevYear")}>
              ‹
            </Button>
            <span className="min-w-[5rem] text-center text-sm font-medium" data-testid="calendar-year">
              {year}
            </span>
            <Button variant="secondary" onClick={() => setYear((y) => y + 1)} aria-label={t("calendar.nextYear")}>
              ›
            </Button>
            <Button onClick={() => setImporting(true)}>{t("calendar.bulkImport")}</Button>
          </>
        }
      />

      <p className="mb-3 text-xs text-slate-500">{t("calendar.hint")}</p>

      {query.isError ? (
        <QueryError error={query.error} onRetry={() => void query.refetch()} />
      ) : query.isLoading ? (
        <Spinner label={t("common.loading")} />
      ) : (
        <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
          {Array.from({ length: 12 }, (_, index) => index + 1).map((month) => (
            <MonthGrid
              key={month}
              year={year}
              month={month}
              busy={busy}
              overrides={overrides}
              effectiveWorkday={effectiveWorkday}
              onToggle={(date, workday) => void save([{ date, is_workday: workday }])}
              onReset={(date) => void remove(date)}
            />
          ))}
        </div>
      )}

      {importing ? (
        <ImportDrawer
          onClose={() => setImporting(false)}
          onSubmit={async (days) => {
            await save(days);
            setImporting(false);
          }}
        />
      ) : null}
    </section>
  );
}

function MonthGrid({
  year,
  month,
  busy,
  overrides,
  effectiveWorkday,
  onToggle,
  onReset,
}: {
  year: number;
  month: number;
  busy: boolean;
  overrides: Map<string, BillingCalendarDay>;
  effectiveWorkday: (month: number, day: number) => boolean;
  onToggle: (date: string, workday: boolean) => void;
  onReset: (date: string) => void;
}) {
  const { t } = useI18n();
  const firstWeekday = new Date(year, month - 1, 1).getDay();
  const total = new Date(year, month, 0).getDate();
  // 网格从周日开始：前面补 firstWeekday 个占位格。
  const cells: (number | null)[] = [
    ...Array.from({ length: firstWeekday }, () => null),
    ...Array.from({ length: total }, (_, i) => i + 1),
  ];
  const [resetting, setResetting] = useState<string | null>(null);

  return (
    <div className="rounded-xl border border-slate-200 bg-white p-3">
      <div className="mb-2 text-sm font-medium text-slate-700">
        {year}-{pad(month)}
      </div>
      <div className="grid grid-cols-7 gap-1 text-center text-[10px] uppercase text-slate-400">
        {[t("weekday.sun"), t("weekday.mon"), t("weekday.tue"), t("weekday.wed"), t("weekday.thu"), t("weekday.fri"), t("weekday.sat")].map(
          (label, index) => (
            <div key={index}>{label.slice(0, 2)}</div>
          ),
        )}
      </div>
      <div className="mt-1 grid grid-cols-7 gap-1">
        {cells.map((day, index) => {
          if (day === null) return <div key={"pad-" + index} />;
          const date = dateLabel(year, month, day);
          const workday = effectiveWorkday(month, day);
          const registered = overrides.has(date);
          return (
            <button
              key={date}
              type="button"
              disabled={busy}
              data-testid={"calendar-day-" + date}
              data-workday={workday ? "1" : "0"}
              data-registered={registered ? "1" : "0"}
              title={overrides.get(date)?.note || undefined}
              onClick={() => onToggle(date, !workday)}
              onContextMenu={(e) => {
                // 右键清除登记，回到按星期推断的默认口径。
                e.preventDefault();
                if (registered) setResetting(date);
              }}
              className={
                "rounded-md py-1 text-xs " +
                (workday ? "bg-emerald-50 text-emerald-800 hover:bg-emerald-100 " : "bg-rose-50 text-rose-800 hover:bg-rose-100 ") +
                (registered ? "font-semibold ring-1 ring-slate-400 " : "")
              }
            >
              {day}
            </button>
          );
        })}
      </div>
      <ConfirmDialog
        open={resetting !== null}
        title={t("calendar.resetConfirm")}
        description={resetting ?? undefined}
        onCancel={() => setResetting(null)}
        onConfirm={() => {
          if (resetting) onReset(resetting);
          setResetting(null);
        }}
      />
    </div>
  );
}

function ImportDrawer({
  onClose,
  onSubmit,
}: {
  onClose: () => void;
  onSubmit: (days: BillingCalendarInput[]) => Promise<void>;
}) {
  const { t } = useI18n();
  const [text, setText] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const parsed = parseImport(text);

  async function submit() {
    setError("");
    if (parsed.days.length === 0) {
      setError(t("calendar.importEmpty"));
      return;
    }
    if (parsed.errors.length > 0) {
      setError(t("calendar.importInvalid", { lines: parsed.errors.join(", ") }));
      return;
    }
    setBusy(true);
    try {
      await onSubmit(parsed.days.map((d) => d.input));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Drawer
      open
      title={t("calendar.bulkImport")}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>{t("common.cancel")}</Button>
          <Button onClick={() => void submit()} disabled={busy}>{t("calendar.import")}</Button>
        </>
      }
    >
      <div className="space-y-3">
        <Field label={t("calendar.importLabel")} hint={t("calendar.importHint")}>
          <textarea
            className="h-40 w-full rounded-md border border-slate-300 px-3 py-2 font-mono text-xs outline-none focus:border-blue-500"
            value={text}
            onChange={(e) => setText(e.target.value)}
            placeholder={"2026-10-01,0,国庆\n2026-10-10,1,调休上班"}
            aria-label={t("calendar.importLabel")}
            data-testid="calendar-import-text"
          />
        </Field>
        {text.trim() !== "" ? (
          <p className="text-xs text-slate-500">
            {parsed.errors.length === 0
              ? t("calendar.importCount", { count: parsed.days.length })
              : t("calendar.importInvalid", { lines: parsed.errors.join(", ") })}
          </p>
        ) : null}
        {parsed.days.length > 0 && parsed.errors.length === 0 ? (
          <div className="max-h-40 space-y-1 overflow-y-auto rounded-md border border-slate-200 p-2 text-xs">
            {parsed.days.map((d) => (
              <div key={d.input.date} className="flex justify-between gap-2">
                <span className="font-mono">{d.input.date}</span>
                <span className={d.input.is_workday ? "text-emerald-700" : "text-rose-700"}>
                  {d.input.is_workday ? t("calendar.workday") : t("calendar.holiday")}
                </span>
                <span className="truncate text-slate-500">{d.input.note || "—"}</span>
              </div>
            ))}
          </div>
        ) : null}
        {error ? <ErrorText>{error}</ErrorText> : null}
        {parsed.days.length === 0 && text.trim() !== "" ? (
          <EmptyState title={t("calendar.importEmpty")} />
        ) : null}
      </div>
    </Drawer>
  );
}