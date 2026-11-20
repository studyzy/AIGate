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
  },
});
