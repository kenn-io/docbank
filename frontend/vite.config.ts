import { defineConfig } from "vite-plus";
import { svelte } from "@sveltejs/vite-plugin-svelte";

const apiTarget = process.env.VITE_API_TARGET ?? "http://127.0.0.1:8080";

export default defineConfig({
  fmt: {
    svelte: true,
    ignorePatterns: ["src/generated/docbank.ts"],
  },
  lint: {
    jsPlugins: [{ name: "vite-plus", specifier: "vite-plus/oxlint-plugin" }],
    rules: {
      "vite-plus/prefer-vite-plus-imports": "error",
    },
  },
  plugins: [svelte()],
  resolve: {
    conditions: ["browser"],
  },
  server: {
    proxy: {
      "/api": {
        target: apiTarget,
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test-setup.ts"],
    // Node 25+ defines its own localStorage, which hides jsdom's unless disabled.
    execArgv: ["--no-experimental-webstorage"],
    exclude: ["docs-site/**", "node_modules/**"],
    maxWorkers: 2,
    // GitHub Actions lists every passing test by name so reruns can confirm
    // flaky tests, and keeps Vitest's default failure annotations.
    ...(process.env.GITHUB_ACTIONS === "true" && { reporters: ["verbose", "github-actions"] }),
    testTimeout: 30_000,
    server: {
      deps: {
        inline: ["svelte"],
      },
    },
  },
});
