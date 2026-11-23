import type { PeakSchedule, PeakWindow } from "@/api/types";
import { Button, Field, Input } from "@/components/ui/primitives";
import { useI18n } from "@/i18n";
import type { MessageKey } from "@/i18n/en";

// DAYS 按「周一 … 周日」排序展示（中国习惯），但值仍是 Go time.Weekday 编号（0=周日）。
const DAYS: { value: number; key: MessageKey }[] = [
  { value: 1, key: "weekday.mon" },
  { value: 2, key: "weekday.tue" },
  { value: 3, key: "weekday.wed" },
  { value: 4, key: "weekday.thu" },
  { value: 5, key: "weekday.fri" },
  { value: 6, key: "weekday.sat" },
  { value: 0, key: "weekday.sun" },
];

// TIMEZONES 是常见 IANA 时区，值必须是服务端 time.LoadLocation 能解析的名字。
const TIMEZONES = [
  "Asia/Shanghai",
  "Asia/Hong_Kong",
  "Asia/Tokyo",
  "Asia/Singapore",
  "Asia/Kolkata",
  "Europe/London",
  "Europe/Berlin",
  "America/New_York",
  "America/Los_Angeles",
  "UTC",
];

const EMPTY: PeakSchedule = { timezone: "", windows: [] };

// wrapsMidnight 判断窗口是否跨午夜（end <= start）：按分钟比较，字符串比较会把 "9:00" 排在 "10:00" 之后。
function wrapsMidnight(window: PeakWindow): boolean {
  const toMinutes = (clock: string) => {
    const [h, m] = clock.split(":");
    return Number(h) * 60 + Number(m);
  };
  return toMinutes(window.end) <= toMinutes(window.start);
}

/**
 * parsePeakHours 解出价目里的 peak_hours 文本。
 * "{}" 或空串表示无时段；JSON 损坏也按无时段处理（服务端写入前已校验，正常不会出现）。
 */
export function parsePeakHours(raw: string): PeakSchedule {
  if (!raw.trim()) return EMPTY;
  try {
    const parsed = JSON.parse(raw) as Partial<PeakSchedule> | null;
    if (!parsed || typeof parsed !== "object") return EMPTY;
    const windows = Array.isArray(parsed.windows) ? parsed.windows : [];
    return { timezone: parsed.timezone ?? "", windows };
  } catch {
    return EMPTY;
  }
}

/** serializePeakHours 产出入库文本：无时段落 "{}"，有时段时按服务端结构序列化。 */
export function serializePeakHours(schedule: PeakSchedule): string {
  if (!schedule.timezone && schedule.windows.length === 0) return "{}";
  return JSON.stringify({ timezone: schedule.timezone, windows: schedule.windows });
}

/** PeakHoursEditor 编辑模型的峰谷时段：时区下拉 + 窗口列表（星期多选、起止时刻）。 */
export function PeakHoursEditor({ value, onChange }: { value: string; onChange: (next: string) => void }) {
  const { t } = useI18n();
  const schedule = parsePeakHours(value);

  const commit = (next: PeakSchedule) => onChange(serializePeakHours(next));

  function update(index: number, patch: Partial<PeakWindow>) {
    commit({ ...schedule, windows: schedule.windows.map((w, i) => (i === index ? { ...w, ...patch } : w)) });
  }

  function addWindow() {
    // 默认填 DeepSeek 式的工作日 09:00–12:00，管理员改时刻即可。
    commit({ ...schedule, windows: [...schedule.windows, { days: [1, 2, 3, 4, 5], start: "09:00", end: "12:00" }] });
  }

  function toggleDay(index: number, day: number) {
    const window = schedule.windows[index];
    const days = window.days.includes(day) ? window.days.filter((d) => d !== day) : [...window.days, day].sort();
    update(index, { days });
  }

  // 未选时区却已有窗口时，服务端会拒绝写入：这里先给出同样的提示。
  const needsTimezone = schedule.windows.length > 0 && schedule.timezone === "";
  // 一个窗口都没勾星期时，该窗口在运行期不可评估，服务端同样会拒。
  const emptyWindow = schedule.windows.some((w) => w.days.length === 0);

  return (
    <div className="space-y-2">
      <Field label={t("peak.timezone")}>
        <select
          className="w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm outline-none focus:border-blue-500"
          value={schedule.timezone}
          onChange={(e) => commit({ ...schedule, timezone: e.target.value })}
          data-testid="peak-timezone"
        >
          <option value="">{t("peak.timezone.none")}</option>
          {TIMEZONES.map((zone) => (
            <option key={zone} value={zone}>
              {zone}
            </option>
          ))}
        </select>
      </Field>

      {schedule.windows.length === 0 ? <p className="text-xs text-slate-500">{t("peak.empty")}</p> : null}

      {schedule.windows.map((window, index) => (
        <div key={index} className="space-y-2 rounded-lg border border-slate-200 p-3" data-testid={"peak-window-" + index}>
          <div className="flex flex-wrap gap-1">
            {DAYS.map((day) => (
              <label
                key={day.value}
                className="flex items-center gap-1 rounded-md border border-slate-200 px-2 py-1 text-xs"
              >
                <input
                  type="checkbox"
                  checked={window.days.includes(day.value)}
                  onChange={() => toggleDay(index, day.value)}
                />
                {t(day.key)}
              </label>
            ))}
          </div>
          <div className="flex gap-2">
            <Field label={t("peak.start")}>
              <Input type="time" value={window.start} onChange={(e) => update(index, { start: e.target.value })} />
            </Field>
            <Field label={t("peak.end")}>
              <Input type="time" value={window.end} onChange={(e) => update(index, { end: e.target.value })} />
            </Field>
            <div className="flex items-end">
              <Button
                variant="ghost"
                className="text-red-600"
                onClick={() => commit({ ...schedule, windows: schedule.windows.filter((_, i) => i !== index) })}
              >
                {t("common.delete")}
              </Button>
            </div>
          </div>
          {wrapsMidnight(window) ? <p className="text-xs text-amber-600">{t("peak.crossMidnight")}</p> : null}
        </div>
      ))}

      {needsTimezone ? <p className="text-xs text-red-600">{t("peak.timezoneRequired")}</p> : null}
      {emptyWindow ? <p className="text-xs text-red-600">{t("peak.daysRequired")}</p> : null}

      <Button variant="secondary" onClick={addWindow}>
        {t("peak.addWindow")}
      </Button>
    </div>
  );
}