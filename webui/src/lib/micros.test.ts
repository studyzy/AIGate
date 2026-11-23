import { describe, expect, it } from "vitest";

import { inputToMicros, microsToInput } from "./micros";

describe("micros", () => {
  it("renders integer micros as currency units", () => {
    expect(microsToInput(0)).toBe("0");
    expect(microsToInput(1_000_000)).toBe("1");
    expect(microsToInput(20_000)).toBe("0.02");
    expect(microsToInput(1_500_000)).toBe("1.5");
    expect(microsToInput(7_100_000)).toBe("7.1");
  });

  it("parses currency units into exact integer micros", () => {
    expect(inputToMicros("1")).toBe(1_000_000);
    expect(inputToMicros("0.02")).toBe(20_000);
    expect(inputToMicros("1.5")).toBe(1_500_000);
    // 0.1 × 1e6 在浮点里是 100000.00000000001；整数路径必须给出精确值。
    expect(inputToMicros("0.1")).toBe(100_000);
    expect(inputToMicros(" 12.3456 ")).toBe(12_345_600);
  });

  it("rejects malformed and over-precise input", () => {
    expect(inputToMicros("")).toBeNull();
    expect(inputToMicros("abc")).toBeNull();
    expect(inputToMicros("-1")).toBeNull();
    expect(inputToMicros("1.2345678")).toBeNull();
    expect(inputToMicros("1e6")).toBeNull();
  });

  it("round-trips through the editor", () => {
    for (const micros of [0, 20_000, 1_000_000, 8_000_000, 123_456]) {
      expect(inputToMicros(microsToInput(micros))).toBe(micros);
    }
  });
});