import { expect, test } from "@playwright/test";

import { login } from "./helpers";

test("guides an empty install to a working key", async ({ page }) => {
  await login(page);

  // 供应商 = 凭据 + 上游地址 + 模型列表；凭据在同一个表单里内联创建。
  await page.getByRole("link", { name: "Providers" }).click();
  await expect(page.getByText("No providers yet")).toBeVisible();
  await page.getByRole("button", { name: "New provider" }).click();
  await page.getByLabel("Provider", { exact: true }).fill("openai");
  await page.getByLabel("Endpoint").fill("http://127.0.0.1:19090/v1");
  await page.getByLabel("Credential name").fill("e2e-cred");
  await page.getByPlaceholder("sk-...").fill("sk-mock");
  await page.getByRole("button", { name: "Fetch models" }).click();
  await expect(page.getByRole("checkbox", { name: "mock-model-e2e" })).toBeChecked();
  await expect(page.getByRole("checkbox", { name: "mock-model-fallback" })).toBeChecked();
  await page.getByRole("button", { name: "Save" }).click();

  const providerRow = page.getByText("openai", { exact: true }).first();
  await expect(providerRow).toBeVisible();
  await providerRow.click();
  await expect(page.getByText("mock-model-e2e").first()).toBeVisible();
  await page.getByRole("button", { name: "Test connectivity" }).first().click();
  // 探测是真实对话请求：toast 里必须出现上游回复内容，而不是只报「连通」。
  await expect(page.getByText(/upstream replied: hello/i).first()).toBeVisible();

  await page.getByRole("link", { name: "Users" }).click();
  await page.getByRole("button", { name: "New user" }).click();
  await page.getByLabel("Username").fill("e2e-user");
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page.getByTestId("token-value")).toBeVisible();
  await page.getByRole("button", { name: "Close" }).click();

  // 主模型 + 备用模型：后端只认 primary / fallback，且必须恰好一个 primary。
  await page.getByRole("link", { name: "Keys" }).click();
  await page.getByRole("button", { name: "New key" }).click();
  await page.getByLabel("Name").fill("e2e-key");
  await page.getByLabel("User").selectOption({ label: "e2e-user" });
  await page.getByRole("button", { name: "Add binding" }).click();
  await page.getByTestId("binding-model-0").selectOption({ label: "mock-model-e2e" });
  await page.getByTestId("binding-role-0").selectOption("primary");
  await page.getByRole("button", { name: "Add binding" }).click();
  await page.getByTestId("binding-model-1").selectOption({ label: "mock-model-fallback" });
  await page.getByTestId("binding-role-1").selectOption("fallback");
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page.getByTestId("token-value")).toBeVisible();
  await expect(page.getByTestId("token-value")).toHaveText(/^sk-/);
});
