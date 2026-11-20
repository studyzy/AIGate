import { expect, type APIRequestContext, type Page } from "@playwright/test";

export const ADMIN_TOKEN = "e2e-admin";

export async function login(page: Page) {
  await page.goto("/ui/login");
  await page.getByLabel("Admin token").fill(ADMIN_TOKEN);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
}

/** seed 用 API 准备基础资源，避免每个用例都走一遍 UI。 */
export async function seed(request: APIRequestContext, suffix: string) {
  const headers = { Authorization: "Bearer " + ADMIN_TOKEN };
  const credential = await request.post("/admin/credentials", {
    headers,
    data: { name: "cred-" + suffix, description: "e2e", secrets: [{ key: "api_key", value: "sk-mock", location: "header" }] },
  });
  expect(credential.ok()).toBeTruthy();
  const credentialID = (await credential.json()).id as number;

  const model = await request.post("/admin/models", {
    headers,
    data: {
      provider: "openai",
      model_id: "mock-model-" + suffix,
      model_name: "Mock " + suffix,
      url: "http://127.0.0.1:19090/v1",
      model_type: "openai-completions",
      credential_id: credentialID,
      enabled: true,
    },
  });
  expect(model.ok()).toBeTruthy();
  const modelID = (await model.json()).id as number;

  const user = await request.post("/admin/users", { headers, data: { username: "user-" + suffix } });
  expect(user.ok()).toBeTruthy();
  const userID = (await user.json()).id as number;

  const key = await request.post("/admin/keys", {
    headers,
    data: {
      name: "key-" + suffix,
      user_id: userID,
      max_tokens: 0,
      bindings: [{ ai_model_id: modelID, role: "primary", sort_order: 0 }],
    },
  });
  expect(key.ok()).toBeTruthy();
  const keyBody = await key.json();
  return { modelID, modelName: "Mock " + suffix, modelKey: "mock-model-" + suffix, token: keyBody.token as string };
}
