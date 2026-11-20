import { defineConfig, devices } from "@playwright/test";

// run.sh 负责起真实二进制与 mock 上游，这里只管浏览器与超时。
export default defineConfig({
  testDir: "./e2e",
  timeout: 30_000,
  expect: { timeout: 10_000 },
  workers: 1,
  retries: 0,
  reporter: [["list"]],
  use: {
    baseURL: process.env.WEBUI_BASE_URL ?? "http://127.0.0.1:18080",
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
