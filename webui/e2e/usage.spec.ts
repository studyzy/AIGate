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
});
