// 金额一律以整数微元跨 API 传输（1 微元 = 1e-6 个币种单位），只有录入与展示在这里换算。

export const MICROS_PER_UNIT = 1_000_000;

const MAX_INPUT_MICROS = 10_000_000_000;
const MICROS_PATTERN = /^\d+(?:\.\d{1,6})?$/;

/** microsToInput 把整数微元换成可编辑的币种单位文本（0.02 → "0.02"，1 → "1"）。 */
export function microsToInput(micros: number): string {
  const negative = micros < 0;
  const digits = String(Math.abs(Math.trunc(micros))).padStart(MICROS_PER_UNIT.toString().length, "0");
  const whole = digits.slice(0, -6);
  const fraction = digits.slice(-6).replace(/0+$/, "");
  return (negative ? "-" : "") + whole + (fraction ? "." + fraction : "");
}

/**
 * inputToMicros 把币种单位文本换成整数微元；非法或超过 6 位小数时返回 null。
 * 用字符串拆分而不是 parseFloat：浮点乘法会把 1.000001 之类的输入舍成邻值。
 */
export function inputToMicros(raw: string): number | null {
  const text = raw.trim();
  if (text === "" || !MICROS_PATTERN.test(text)) return null;
  const [whole, fraction = ""] = text.split(".");
  const micros = Number(whole) * MICROS_PER_UNIT + Number(fraction.padEnd(6, "0"));
  return Number.isSafeInteger(micros) && micros <= MAX_INPUT_MICROS ? micros : null;
}