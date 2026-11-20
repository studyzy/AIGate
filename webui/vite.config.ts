import { fileURLToPath, URL } from "node:url";

import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// 产物直接写进 Go 侧 go:embed 的目录，避免构建后再拷贝一步。
export default defineConfig({
  plugins: [react()],
  base: "/ui/",
  resolve: {
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  build: {
    outDir: "../internal/webui/dist",
    // 保留 .gitkeep：它让没有 node 的机器上 go:embed 仍能编译。
    emptyOutDir: false,
    rollupOptions: {
      output: {
        // 第三方库按用途拆成独立 chunk：页面已在 router 里 lazy 加载，
        // 供应商 chunk 不随业务代码变动，浏览器可长期缓存。
        manualChunks(id) {
          if (!id.includes("node_modules")) return;
          if (id.includes("@tanstack/react-query")) return "vendor-query";
          if (
            id.includes("/recharts") ||
            id.includes("recharts-scale") ||
            id.includes("react-smooth") ||
            id.includes("victory-vendor") ||
            id.includes("/d3-")
          ) {
            return "vendor-charts";
          }
          if (
            id.includes("/react/") ||
            id.includes("/react-dom/") ||
            id.includes("/react-router") ||
            id.includes("/scheduler/")
          ) {
            return "vendor-react";
          }
        },
      },
    },
  },
  server: {
    port: 5173,
    proxy: {
      "/admin": "http://localhost:8080",
      "/api": "http://localhost:8080",
      "/v1": "http://localhost:8080",
    },
  },
  test: {
    environment: "jsdom",
    globals: true,
    css: false,
    // e2e 目录是 Playwright 用例，不归 Vitest 收集。
    exclude: ["**/node_modules/**", "**/dist/**", "e2e/**"],
  },
});
