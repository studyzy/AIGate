import { expect, test } from "@playwright/test";

import { login } from "./helpers";

test("collapses the sidebar and remembers it", async ({ page }) => {
  await login(page);
  await expect(page.locator("aside")).toBeVisible();

  await page.getByRole("button", { name: "Collapse sidebar" }).click();
  await expect(page.locator("aside")).toHaveCount(0);

  await page.reload();
  await expect(page.getByRole("button", { name: "Expand sidebar" })).toBeVisible();
  await expect(page.locator("aside")).toHaveCount(0);

  await page.getByRole("button", { name: "Expand sidebar" }).click();
  await expect(page.locator("aside")).toBeVisible();
});

test("opens the navigation drawer on a narrow viewport", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 780 });
  await login(page);
  await expect(page.locator("aside")).toBeHidden();

  await page.getByRole("button", { name: "Menu" }).click();
  await expect(page.getByRole("link", { name: "Providers" })).toBeVisible();

  await page.getByRole("link", { name: "Providers" }).click();
  await expect(page.getByRole("heading", { name: "Providers" })).toBeVisible();
  await expect(page.locator("aside")).toBeHidden();
});
