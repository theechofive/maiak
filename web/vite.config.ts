/// <reference types="vitest/config" />
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

const apiTarget = process.env.MAIAK_API_TARGET ?? "http://localhost:8080";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api": { target: apiTarget, changeOrigin: true },
    },
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test-setup.ts"],
  },
});
