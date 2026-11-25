import { expect, test } from "@playwright/test";

import { ADMIN_TOKEN, login, seed } from "./helpers";

const auth = { Authorization: "Bearer " + ADMIN_TOKEN };

test("filters request logs by api key and time, then shows request body", async ({ page, request }) => {
  const stamp = String(Date.now());
  const suffixA = "rl-a-" + stamp;
  const suffixB = "rl-b-" + stamp;
  // 两条请求正文共用一个标记：落库后可只用一个 q 查询同时等到两条日志。
  const shared = "rl-marker-" + stamp;
  const markerA = shared + "-a";
  const markerB = shared + "-b";
  const startedAt = Date.now();

  const a = await seed(request, suffixA);
  const b = await seed(request, suffixB);
  const chats: [string, string, string][] = [
    [a.token, a.modelKey, markerA],
    [b.token, b.modelKey, markerB],
  ];
  for (const [token, modelKey, marker] of chats) {
    const chat = await request.post("/v1/chat/completions", {
      headers: { Authorization: "Bearer " + token },
      data: { model: modelKey, messages: [{ role: "user", content: marker }] },
    });
    expect(chat.ok()).toBeTruthy();
  }
  const endedAt = Date.now();

  // R35：读接口只在 -request-log=sqlite 时注册；异步写入器最长 2s 刷写一次，
  // 先轮询到两条日志落库再进页面，避免首屏查询撞上空库。插件为 none 时此处 404 → RED。
  await expect
    .poll(
      async () => {
        const res = await request.get("/admin/request-logs?q=" + encodeURIComponent(shared), { headers: auth });
        if (!res.ok()) return 0;
        return ((await res.json()) as { total: number }).total;
      },
      { message: "request logs were not persisted (is the request-log plugin enabled?)", timeout: 15_000 },
    )
    .toBeGreaterThanOrEqual(2);

  // R37：seed 不返回数字 ID，从 /admin/keys 解析目标 key 的 ID。
  const keys = await request.get("/admin/keys", { headers: auth });
  expect(keys.ok()).toBeTruthy();
  const keysBody = (await keys.json()) as { keys: { id: number; name: string }[] };
  const targetID = keysBody.keys.find((k) => k.name === "key-" + suffixA)?.id;
  expect(targetID, "target key must exist").toBeTruthy();

  await login(page);
  await page.getByRole("link", { name: "LLM request logs" }).click();
  await expect(page.getByRole("heading", { name: "LLM request logs" })).toBeVisible();

  // 时间筛选：绝对时间戳在浏览器本地换算成 datetime-local，前后各留 5 分钟吸收分钟截断。
  const window = await page.evaluate(
    ({ from, to }) => {
      const fmt = (ms: number) => {
        const d = new Date(ms);
        const p = (n: number) => String(n).padStart(2, "0");
        return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
      };
      return { start: fmt(from - 5 * 60_000), end: fmt(to + 5 * 60_000) };
    },
    { from: startedAt, to: endedAt },
  );
  await page.getByLabel("Start").fill(window.start);
  await expect(page).toHaveURL(/[?&]start=/);
  await page.getByLabel("End").fill(window.end);
  await expect(page).toHaveURL(/[?&]end=/);

  await page.getByLabel("API key").selectOption(String(targetID));
  await expect(page).toHaveURL(/[?&]api_key_id=/);
  await expect(page.getByRole("table").locator("tbody tr")).toHaveCount(1);

  await page.getByRole("table").locator("tbody tr").click();
  await expect(page.getByText("Request body", { exact: true })).toBeVisible();
  const requestBody = page.locator("pre").filter({ hasText: markerA });
  await expect(requestBody).toBeVisible();
  await expect(requestBody).not.toContainText(markerB);
});
