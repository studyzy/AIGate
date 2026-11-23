import { expect, test } from "@playwright/test";

import { login, seed } from "./helpers";

test("records a proxied request in the usage page", async ({ page, request }) => {
  const suffix = String(Date.now());
  const { token, modelKey, modelName } = await seed(request, suffix);

  const chat = await request.post("/v1/chat/completions", {
    headers: { Authorization: "Bearer " + token },
    data: { model: modelKey, messages: [{ role: "user", content: "ping" }] },
  });
  expect(chat.ok()).toBeTruthy();

  await login(page);
  await page.getByRole("link", { name: "Usage" }).click();
  await page.getByLabel("Group by").selectOption("model");
  await expect(page.getByRole("table")).toBeVisible();
  await expect(page.getByText(modelName).first()).toBeVisible();

  // 明细表的两列金额：模型币种金额与结算币种金额。
  await page.getByRole("button", { name: "Request logs" }).click();
  await expect(page.getByRole("columnheader", { name: "Cost (model currency)" })).toBeVisible();
  await expect(page.getByRole("columnheader", { name: "Cost (settlement)" })).toBeVisible();
});

test("shows cost columns and total in the usage report", async ({ page }) => {
  await login(page);
  await page.goto("/ui/usage");
  await expect(page.getByRole("columnheader", { name: "Cost (settlement)" })).toBeVisible();
  await expect(page.getByTestId("usage-total-cost")).toContainText("¥");
});
