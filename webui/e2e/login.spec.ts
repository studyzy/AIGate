import { expect, test } from "@playwright/test";

import { login } from "./helpers";

test("rejects a bad token", async ({ page }) => {
  await page.goto("/ui/login");
  await page.getByLabel("Admin token").fill("wrong-token");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByText(/token/i).first()).toBeVisible();
  await expect(page).toHaveURL(/\/ui\/login/);
});

test("signs in and lands on the dashboard", async ({ page }) => {
  await login(page);
  await expect(page).toHaveURL(/\/ui\/?$/);
});

test("redirects to login when the token is gone", async ({ page }) => {
  await login(page);
  await page.evaluate(() => sessionStorage.clear());
  await page.goto("/ui/keys");
  await expect(page.getByRole("button", { name: "Sign in" })).toBeVisible();
});
