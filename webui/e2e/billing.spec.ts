import { expect, test, type APIRequestContext } from "@playwright/test";

import { login, seed } from "./helpers";

// 计费配置的用户路径：模型币种/峰谷开关 → 价目抽屉（六档单价 + 峰谷时段）→ 日历 → 汇率。

test("configures model currency, peak pricing and price rows", async ({ page, request }) => {
  const suffix = String(Date.now());
  const { modelName, modelKey } = await seed(request, suffix);
  await login(page);

  await page.goto("/ui/providers");
  // 模型行在供应商分组里，分组默认折叠，先展开。
  const group = page.getByRole("button", { name: /^OA openai/ });
  await expect(group).toBeVisible();
  if ((await group.getAttribute("aria-expanded")) === "false") await group.click();

  // modelRow 定位到本用例创建的那一行（其他用例的模型可能也在同一分组里）。
  const modelRow = page
    .locator("div.rounded-lg", { has: page.getByText(modelName, { exact: true }) })
    .last();
  await expect(modelRow).toBeVisible();
  await modelRow.getByRole("button", { name: "Edit" }).click();

  // 布局回归：grid 的列轨道数必须等于子元素数，且相邻单元格不重叠。
  // 上一版声明了 7 个轨道却只填 6 个单元格，按钮被挤到价格列、文字互相压盖。
  const layout = await modelRow.evaluate((row) => {
    const cells = [...row.children] as HTMLElement[];
    const tracks = getComputedStyle(row).gridTemplateColumns.split(" ").filter(Boolean).length;
    // 检测可见重叠：任一单元格的左边不得越过前一单元格内容的右边界。
    let overlap = false;
    for (let i = 1; i < cells.length; i += 1) {
      const prev = cells[i - 1].getBoundingClientRect();
      const cur = cells[i].getBoundingClientRect();
      if (cur.left < prev.left) overlap = true;
      if (i <= 4 && cur.left < cells[i - 1].getBoundingClientRect().right - 1) overlap = true;
    }
    const actions = cells[cells.length - 1].getBoundingClientRect();
    const rowBox = row.getBoundingClientRect();
    return {
      tracks,
      cells: cells.length,
      overlap,
      actionsRightGap: rowBox.right - actions.right,
    };
  });
  expect(layout.tracks).toBe(layout.cells);
  expect(layout.overlap).toBe(false);
  // 按钮组贴住行的右边缘（允许 padding 余量）。
  expect(layout.actionsRightGap).toBeLessThan(40);

  // 模型编辑抽屉只编辑模型能力：上下文窗口、输出上限、图片、effort 等级。
  const drawer = page.getByRole("dialog", { name: "Edit model" });
  await expect(drawer).toBeVisible();
  // 计价状态只在列表里露出，模型抽屉里不再重复（避免同一信息出现在两处）。
  await expect(drawer.getByTestId("model-current-price")).toHaveCount(0);
  await expect(modelRow.getByTestId("model-price-status")).toContainText("No currency");
  await drawer.getByTestId("model-context-window").fill("128000");
  await drawer.getByTestId("model-max-output-tokens").fill("8192");
  await drawer.getByTestId("model-supports-image").check();
  await drawer.getByTestId("model-effort-levels").getByRole("checkbox", { name: "high", exact: true }).check();
  await drawer.getByTestId("model-effort-levels").getByRole("checkbox", { name: "low", exact: true }).check();
  await drawer.getByRole("button", { name: "Save" }).click();
  await expect(drawer).toHaveCount(0);

  // 保存后回显：能力字段持久化，且 effort 由服务端按阶梯升序归一化。
  await modelRow.getByRole("button", { name: "Edit" }).click();
  await expect(drawer.getByTestId("model-context-window")).toHaveValue("128000");
  await expect(drawer.getByTestId("model-max-output-tokens")).toHaveValue("8192");
  await expect(drawer.getByTestId("model-supports-image")).toBeChecked();
  await expect(drawer.getByTestId("model-effort-levels").getByRole("checkbox", { name: "high", exact: true })).toBeChecked();
  await expect(drawer.getByTestId("model-effort-levels").getByRole("checkbox", { name: "low", exact: true })).toBeChecked();
  await expect(drawer.getByTestId("model-effort-levels").getByRole("checkbox", { name: "medium", exact: true })).not.toBeChecked();
  await page.keyboard.press("Escape");
  await expect(drawer).toHaveCount(0);

  // 价目抽屉承担计价配置：币种 + 两个开关在这里落地，再追加价目。
  await modelRow.getByRole("button", { name: "Prices" }).click();
  const priceDrawer = page.getByRole("dialog", { name: "Prices" });
  await expect(priceDrawer).toBeVisible();
  await expect(priceDrawer.getByText("No prices yet")).toBeVisible();

  await priceDrawer.getByTestId("price-currency-select").selectOption("CNY");
  await priceDrawer.getByTestId("price-peak-pricing").check();
  await priceDrawer.getByTestId("price-upstream-cost").uncheck();
  await priceDrawer.getByRole("button", { name: "Save settings" }).click();
  // 峰谷开关打开后，六个单价框才出现（峰三列 + 谷三列）。
  await expect(priceDrawer.getByTestId("price-offpeak_output_micros")).toBeVisible();
  await expect(priceDrawer.getByTestId("price-peak_output_micros")).toBeVisible();

  // 开关是模型级配置，重开抽屉后必须保持。
  await page.keyboard.press("Escape");
  await expect(priceDrawer).toHaveCount(0);
  await modelRow.getByRole("button", { name: "Prices" }).click();
  await expect(priceDrawer.getByTestId("price-currency-select")).toHaveValue("CNY");
  await expect(priceDrawer.getByTestId("price-peak-pricing")).toBeChecked();
  await expect(priceDrawer.getByTestId("price-upstream-cost")).not.toBeChecked();

  // 单价按币种单位录入，提交时换算为整数微元。
  await priceDrawer.getByTestId("price-peak_input_hit_micros").fill("1");
  await priceDrawer.getByTestId("price-peak_input_miss_micros").fill("2");
  await priceDrawer.getByTestId("price-peak_output_micros").fill("8");
  await priceDrawer.getByTestId("price-offpeak_input_hit_micros").fill("0.5");
  await priceDrawer.getByTestId("price-offpeak_input_miss_micros").fill("1");
  await priceDrawer.getByTestId("price-offpeak_output_micros").fill("4");

  // 两个高峰窗口：工作日 09:00–12:00 与 14:00–18:00。
  await priceDrawer.getByTestId("peak-timezone").selectOption("Asia/Shanghai");
  for (const index of [0, 1]) {
    await priceDrawer.getByRole("button", { name: "Add window" }).click();
    const win = priceDrawer.getByTestId("peak-window-" + index);
    await win.getByRole("checkbox", { name: "Mon" }).check();
    await win.getByRole("checkbox", { name: "Fri" }).check();
    await win.getByLabel("Start").fill(index === 0 ? "09:00" : "14:00");
    await win.getByLabel("End").fill(index === 0 ? "12:00" : "18:00");
  }

  await priceDrawer.getByRole("button", { name: "Add price" }).click();
  await expect(priceDrawer.getByTestId("price-version")).toHaveCount(1);
  await expect(priceDrawer.getByText("In effect")).toBeVisible();
  // 微元回显：8 元/百万 token → 8（不是 8000000）。
  await expect(priceDrawer.getByTestId("price-version").first()).toContainText("Output 8");

  // 服务端落库的形状：六列微元 + peak_hours JSON。
  const prices = await request.get("/admin/models/" + (await modelID(request, modelKey)) + "/prices", {
    headers: { Authorization: "Bearer e2e-admin" },
  });
  expect(prices.ok()).toBeTruthy();
  const body = await prices.json();
  expect(body.prices).toHaveLength(1);
  const row = body.prices[0];
  expect(row.currency).toBe("CNY");
  expect(row.peak_output_micros).toBe(8_000_000);
  expect(row.peak_input_hit_micros).toBe(1_000_000);
  expect(row.offpeak_input_hit_micros).toBe(500_000);
  expect(row.offpeak_output_micros).toBe(4_000_000);
  // 模型级能力字段与开关走 /admin/models，不从价目行读。
  const modelResp = await request.get("/admin/models/" + (await modelID(request, modelKey)), {
    headers: { Authorization: "Bearer e2e-admin" },
  });
  expect(modelResp.ok()).toBeTruthy();
  const model = await modelResp.json();
  expect(model.context_window).toBe(128000);
  expect(model.max_output_tokens).toBe(8192);
  expect(model.supports_image).toBe(true);
  expect(model.effort_levels).toEqual(["low", "high"]);
  expect(model.currency).toBe("CNY");
  expect(model.enable_peak_pricing).toBe(true);
  expect(model.use_upstream_cost).toBe(false);

  const schedule = JSON.parse(row.peak_hours);
  expect(schedule.timezone).toBe("Asia/Shanghai");
  expect(schedule.windows).toHaveLength(2);
  // 新增窗口默认勾周一至周五（即 Go 的 1..5），改时刻即可复用工作日口径。
  expect(schedule.windows[1]).toEqual({ days: [1, 2, 3, 4, 5], start: "14:00", end: "18:00" });
});

test("edits billing calendar and currency rates", async ({ page, request }) => {
  await login(page);

  // 日历：把某天标记为节假日，刷新后保持。
  await page.getByRole("link", { name: "Billing calendar" }).click();
  await expect(page.getByRole("heading", { name: "Billing calendar" })).toBeVisible();

  // 固定用 2026 的日期：切换年份后按年取数据，刷新后仍停在该年。
  while ((await page.getByTestId("calendar-year").innerText()) !== "2026") {
    await page.getByRole("button", { name: "Next year" }).click();
  }

  const holiday = page.getByTestId("calendar-day-2026-01-01");
  await expect(holiday).toHaveAttribute("data-workday", "1");
  await expect(holiday).toHaveAttribute("data-registered", "0");
  await holiday.click();
  await expect(holiday).toHaveAttribute("data-workday", "0");
  await expect(holiday).toHaveAttribute("data-registered", "1");

  await page.reload();
  await expect(page.getByTestId("calendar-year")).toHaveText("2026");
  await expect(page.getByTestId("calendar-day-2026-01-01")).toHaveAttribute("data-workday", "0");
  await expect(page.getByTestId("calendar-day-2026-01-01")).toHaveAttribute("data-registered", "1");

  // 批量导入：YYYY-MM-DD,is_workday,note 每行一天。
  await page.getByRole("button", { name: "Bulk import" }).click();
  const importDrawer = page.getByRole("dialog", { name: "Bulk import" });
  await importDrawer.getByTestId("calendar-import-text").fill("2026-02-17,0,Spring Festival\n2026-02-22,1,Make-up workday");
  await expect(importDrawer.getByText("2 day(s) ready to import")).toBeVisible();
  await importDrawer.getByRole("button", { name: "Import" }).click();
  await expect(importDrawer).toHaveCount(0);
  await expect(page.getByTestId("calendar-day-2026-02-17")).toHaveAttribute("data-workday", "0");
  await expect(page.getByTestId("calendar-day-2026-02-22")).toHaveAttribute("data-workday", "1");

  // 服务端口径确认（2026-02-17 是周二，被登记为节假日）。
  const calendar = await request.get("/admin/billing-calendar?year=2026", { headers: { Authorization: "Bearer e2e-admin" } });
  expect(calendar.ok()).toBeTruthy();
  const days = (await calendar.json()).days as { date: string; is_workday: boolean; note: string }[];
  expect(days.find((d) => d.date === "2026-01-01")).toMatchObject({ is_workday: false });
  expect(days.find((d) => d.date === "2026-02-22")).toMatchObject({ is_workday: true, note: "Make-up workday" });

  // 汇率：新增 USD→CNY 7.1，列表出现，删除后消失。
  await page.getByRole("link", { name: "Currency rates" }).click();
  await expect(page.getByRole("heading", { name: "Currency rates" })).toBeVisible();
  await page.getByRole("button", { name: "New rate" }).click();
  const rateDrawer = page.getByRole("dialog", { name: "New rate" });
  await rateDrawer.getByTestId("rate-input").fill("7.1");
  await rateDrawer.getByRole("button", { name: "Save" }).click();
  await expect(rateDrawer).toHaveCount(0);
  await expect(page.getByTestId("rate-value").first()).toHaveText("7.1");

  const rates = await request.get("/admin/currency-rates?from=USD&to=CNY", { headers: { Authorization: "Bearer e2e-admin" } });
  expect(rates.ok()).toBeTruthy();
  const rateRows = (await rates.json()).rates as { id: number; rate_micros: number }[];
  expect(rateRows.some((r) => r.rate_micros === 7_100_000)).toBeTruthy();

  await page.getByTestId("rate-list").getByRole("button", { name: "Delete" }).first().click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Confirm" }).click();
  await expect(page.getByText("No rates for this pair")).toBeVisible();
});

/** modelID 按 model_id 反查模型主键，供直接断言服务端形状使用。 */
async function modelID(request: APIRequestContext, modelKey: string): Promise<number> {
  const res = await request.get("/admin/models", { headers: { Authorization: "Bearer e2e-admin" } });
  expect(res.ok()).toBeTruthy();
  const models = (await res.json()).models as { id: number; model_id: string }[];
  const found = models.find((m) => m.model_id === modelKey);
  expect(found).toBeTruthy();
  return found!.id;
}