import { expect, test } from "@playwright/test";

import { login } from "./helpers";

test("switches language and remembers it", async ({ page }) => {
  await login(page);
  await expect(page.getByRole("link", { name: "Overview" })).toBeVisible();

  await page.getByRole("combobox").first().selectOption("zh-CN");
  await expect(page.getByRole("link", { name: "总览" })).toBeVisible();

  await page.reload();
  await expect(page.getByRole("link", { name: "总览" })).toBeVisible();

  await page.getByRole("combobox").first().selectOption("en");
  await expect(page.getByRole("link", { name: "Overview" })).toBeVisible();
});
