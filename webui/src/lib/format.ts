// 展示层格式化统一走 Intl，避免手写数字/日期拼接。
let locale = "en";
let timeZone: string | undefined;

export function setLocale(next: string) {
  locale = next;
}

export function setTimeZone(next: string | undefined) {
  timeZone = next;
}

export function formatNumber(value: number): string {
  return new Intl.NumberFormat(locale).format(value);
}

export function formatDateTime(value: string | null | undefined): string {
  if (!value) return "—";
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return "—";
  return new Intl.DateTimeFormat(locale, { dateStyle: "short", timeStyle: "short", timeZone }).format(d);
}

export function formatDate(value: string | null | undefined): string {
  if (!value) return "—";
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return "—";
  return new Intl.DateTimeFormat(locale, { dateStyle: "medium", timeZone }).format(d);
}

export function formatLatency(ms: number): string {
  return ms >= 1000 ? (ms / 1000).toFixed(2) + " s" : ms + " ms";
}

export function todayISO(offsetDays = 0): string {
  const d = new Date();
  d.setDate(d.getDate() + offsetDays);
  const pad = (n: number) => String(n).padStart(2, "0");
  return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate());
}
