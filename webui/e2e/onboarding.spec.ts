import { expect, test } from "@playwright/test";

import { login } from "./helpers";

test("guides an empty install to a working key", async ({ page }) => {
  await login(page);

  await page.getByRole("link", { name: "Credentials" }).click();
  await expect(page.getByText("No credentials yet")).toBeVisible();
  await page.getByRole("button", { name: "New credential" }).click();
  await page.getByLabel("Name").fill("e2e-cred");
  await page.getByPlaceholder("Key").fill("api_key");
  await page.getByPlaceholder("Value").fill("sk-mock");
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page.getByText("e2e-cred")).toBeVisible();

  await page.getByRole("link", { name: "Models" }).click();
  await page.getByRole("button", { name: "New model" }).click();
  await page.getByLabel("Display name").fill("E2E Mock");
  await page.getByLabel("Provider").fill("openai");
  await page.getByLabel("Model ID").fill("mock-model-e2e");
  await page.getByLabel("Upstream URL").fill("http://127.0.0.1:19090/v1");
  await page.getByLabel("Credential").selectOption({ label: "e2e-cred" });
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page.getByText("E2E Mock")).toBeVisible();

  await page.getByRole("button", { name: "Test connectivity" }).first().click();
  await expect(page.getByText(/ms|error/i).first()).toBeVisible();

  await page.getByRole("link", { name: "Users" }).click();
  await page.getByRole("button", { name: "New user" }).click();
  await page.getByLabel("Username").fill("e2e-user");
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page.getByTestId("token-value")).toBeVisible();
  await page.getByRole("button", { name: "Close" }).click();

  await page.getByRole("link", { name: "Keys" }).click();
  await page.getByRole("button", { name: "New key" }).click();
  await page.getByLabel("Name").fill("e2e-key");
  await page.getByLabel("User").selectOption({ label: "e2e-user" });
  await page.getByRole("button", { name: "Add binding" }).click();
  await page.getByRole("combobox").nth(3).selectOption({ label: "E2E Mock" });
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page.getByTestId("token-value")).toBeVisible();
  await expect(page.getByTestId("token-value")).toHaveText(/^sk-/);
});
