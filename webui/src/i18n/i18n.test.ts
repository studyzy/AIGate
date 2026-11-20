import { describe, expect, it } from "vitest";

import { en } from "./en";
import { translate } from "./index";
import { zhCN } from "./zh-CN";

describe("i18n", () => {
  it("keeps english and chinese key sets identical", () => {
    expect(Object.keys(zhCN).sort()).toEqual(Object.keys(en).sort());
  });

  it("translates by language", () => {
    expect(translate("en", "nav.dashboard")).toBe("Overview");
    expect(translate("zh-CN", "nav.dashboard")).toBe("总览");
  });

  it("interpolates named placeholders", () => {
    expect(translate("en", "app.name", { name: "x" })).toBe("AIGate Console");
  });
});
