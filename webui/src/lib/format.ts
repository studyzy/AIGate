// 展示层格式化统一走 Intl，避免手写数字/日期拼接。
import { microsToInput } from "./micros";

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

const CURRENCY_SYMBOLS: Record<string, string> = {
  CNY: "¥",
  USD: "$",
  HKD: "HK$",
  EUR: "€",
  GBP: "£",
  JPY: "¥",
  SGD: "S$",
  AUD: "A$",
  CAD: "C$",
  KRW: "₩",
};

/**
 * formatMicros 把整数微元渲染成金额文本：最多 6 位小数、去尾零、最少 2 位（"1.5" → "1.50"）。
 * 换算复用 microsToInput（字符串拆分，不经浮点除法）；符号取自币种，未知币种追加 ISO 代码。
 */
export function formatMicros(micros: number, currency: string): string {
  const code = currency.trim().toUpperCase();
  const negative = micros < 0;
  const text = microsToInput(Math.abs(Math.trunc(micros)));
  const [whole, fraction = ""] = text.split(".");
  const amount = whole + "." + fraction.padEnd(2, "0");
  const sign = negative ? "-" : "";
  const symbol = CURRENCY_SYMBOLS[code];
  if (symbol) return sign + symbol + amount;
  return sign + amount + (code ? " " + code : "");
}

export function todayISO(offsetDays = 0): string {
  const d = new Date();
  d.setDate(d.getDate() + offsetDays);
  const pad = (n: number) => String(n).padStart(2, "0");
  return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate());
}

/** toLocalInput 把 Date 换成 <input type="datetime-local"> 认的本地时刻文本（不带秒与时区）。 */
export function toLocalInput(date: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return (
    date.getFullYear() +
    "-" +
    pad(date.getMonth() + 1) +
    "-" +
    pad(date.getDate()) +
    "T" +
    pad(date.getHours()) +
    ":" +
    pad(date.getMinutes())
  );
}
