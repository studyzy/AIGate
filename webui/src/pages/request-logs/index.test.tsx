// 本环境 NODE_ENV=production 会加载 React 生产构建，@testing-library/react 的 act 不可用。
// vi.hoisted 先于所有 import 执行，把 React 切到 development 构建；用例结束后还原。
const ORIGINAL_NODE_ENV = vi.hoisted(() => {
  const original = process.env.NODE_ENV;
  process.env.NODE_ENV = "development";
  return original;
});

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterAll, beforeEach, describe, expect, it, vi } from "vitest";

import { setLang } from "@/i18n";
import { api } from "@/lib/api";

import { RequestLogsPage } from "./index";

// 只替身网络边界：页面仍走真实的 useQuery / useSearchParams / i18n。
vi.mock("@/lib/api", () => ({
  api: vi.fn((path: string) =>
    path.startsWith("/admin/keys")
      ? Promise.resolve({ keys: [] })
      : Promise.resolve({ logs: [], total: 0, page: 1, page_size: 20 }),
  ),
}));

function renderPage(initialEntries: string[] = ["/request-logs"]) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter
        initialEntries={initialEntries}
        future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
      >
        <RequestLogsPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("RequestLogsPage", () => {
  afterAll(() => {
    process.env.NODE_ENV = ORIGINAL_NODE_ENV;
  });

  beforeEach(() => {
    setLang("en");
    vi.mocked(api).mockClear();
  });

  it("renders and requests the first page of logs", async () => {
    renderPage();

    expect(screen.getByRole("heading", { name: "LLM request logs" })).toBeTruthy();
    await waitFor(() => {
      expect(api).toHaveBeenCalledWith(expect.stringContaining("page=1&page_size=20"));
    });
  });

  it("writes a changed filter into the URL and resets page to 1", async () => {
    renderPage(["/request-logs?page=3"]);

    fireEvent.change(screen.getByLabelText("Model"), { target: { value: "gpt-4" } });

    await waitFor(() => {
      expect(api).toHaveBeenCalledWith(expect.stringContaining("model=gpt-4&page=1"));
    });
  });

  it("converts a datetime-local bound to RFC3339 for the list API", async () => {
    renderPage();

    fireEvent.change(screen.getByLabelText("Start"), { target: { value: "2026-11-25T10:30" } });

    await waitFor(() => {
      const listUrls = vi
        .mocked(api)
        .mock.calls.map(([path]) => String(path))
        .filter((path) => path.startsWith("/admin/request-logs?"));
      expect(
        listUrls.some((path) =>
          /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}/.test(new URLSearchParams(path.split("?")[1]).get("start") ?? ""),
        ),
      ).toBe(true);
    });
  });

  it("defaults the initial range to the last 7 days when the URL has no start/end", async () => {
    renderPage();

    await waitFor(() => {
      expect(api).toHaveBeenCalledWith(expect.stringContaining("page=1&page_size=20"));
    });

    const listUrl = vi
      .mocked(api)
      .mock.calls.map(([path]) => String(path))
      .filter((path) => path.startsWith("/admin/request-logs?"))
      .at(-1);
    const params = new URLSearchParams((listUrl ?? "").split("?")[1] ?? "");
    const start = params.get("start") ?? "";
    const end = params.get("end") ?? "";
    // 默认值必须经 timeBound 变成 RFC3339（带秒），不是裸 datetime-local。
    expect(start).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}/);
    expect(end).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}/);
    const delta = Date.parse(end) - Date.parse(start);
    expect(Math.abs(delta - 7 * 86400000)).toBeLessThan(2 * 3600000);
  });
});
