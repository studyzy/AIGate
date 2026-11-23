import { describe, expect, it } from "vitest";

import { formatMicros } from "./format";

describe("formatMicros", () => {
  it("renders integer micros with at most six decimals and at least two", () => {
    expect(formatMicros(0, "USD")).toBe("$0.00");
    expect(formatMicros(20_000, "USD")).toBe("$0.02");
    expect(formatMicros(1_500_000, "USD")).toBe("$1.50");
    expect(formatMicros(123_456, "USD")).toBe("$0.123456");
    expect(formatMicros(12_345_600, "USD")).toBe("$12.3456");
  });

  it("prefixes known currencies with their symbol", () => {
    expect(formatMicros(7_100_000, "CNY")).toBe("¥7.10");
    expect(formatMicros(1_000_000, "cad")).toBe("C$1.00");
  });

  it("falls back to the ISO code for unknown currencies", () => {
    expect(formatMicros(1_000_000, "XYZ")).toBe("1.00 XYZ");
    expect(formatMicros(1_000_000, "")).toBe("1.00");
  });

  it("keeps the sign", () => {
    expect(formatMicros(-20_000, "USD")).toBe("-$0.02");
  });
});