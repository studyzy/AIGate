import { beforeEach, describe, expect, it, vi } from "vitest";

import { api } from "./api";
import { ApiError, UnauthorizedError } from "./errors";
import { getToken, setToken } from "./session";

function mockFetch(res: Partial<Response>) {
  const fn = vi.fn().mockResolvedValue({
    ok: res.status ? res.status < 400 : true,
    status: 200,
    json: async () => ({}),
    ...res,
  });
  vi.stubGlobal("fetch", fn);
  return fn;
}

describe("api", () => {
  beforeEach(() => {
    sessionStorage.clear();
    vi.unstubAllGlobals();
  });

  it("returns parsed json on success", async () => {
    mockFetch({ status: 200, json: async () => ({ version: "1" }) });
    await expect(api<{ version: string }>("/admin/settings")).resolves.toEqual({ version: "1" });
  });

  it("injects bearer token", async () => {
    setToken("sk-test");
    const fn = mockFetch({ status: 200 });
    await api("/admin/settings");
    expect((fn.mock.calls[0][1] as RequestInit).headers).toMatchObject({
      Authorization: "Bearer sk-test",
    });
  });

  it("clears token and throws UnauthorizedError on 401", async () => {
    setToken("stale");
    mockFetch({ status: 401, ok: false });
    await expect(api("/admin/settings")).rejects.toBeInstanceOf(UnauthorizedError);
    expect(getToken()).toBeNull();
  });

  it("uses the backend error message", async () => {
    mockFetch({ status: 400, ok: false, json: async () => ({ error: { message: "bad model" } }) });
    await expect(api("/admin/models")).rejects.toMatchObject({ message: "bad model", status: 400 });
    await expect(api("/admin/models")).rejects.toBeInstanceOf(ApiError);
  });
});
