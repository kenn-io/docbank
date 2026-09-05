import { defineConfig, devices } from "@playwright/test";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const repositoryRoot = path.resolve(here, "..", "..");
const port = 41738;

export default defineConfig({
  testDir: ".",
  testMatch: /site\.spec\.ts/,
  fullyParallel: false,
  workers: 1,
  timeout: 60_000,
  expect: {
    timeout: 10_000,
  },
  outputDir: path.join(repositoryRoot, ".superpowers", "docs-site-playwright"),
  reporter: "line",
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    trace: "retain-on-failure",
  },
  webServer: {
    command: "node scripts/docs/serve.mjs",
    cwd: repositoryRoot,
    env: {
      DOCBANK_DOCS_PORT: String(port),
    },
    url: `http://127.0.0.1:${port}`,
    reuseExistingServer: false,
    timeout: 120_000,
  },
  projects: [
    {
      name: "chromium",
      use: {
        ...devices["Desktop Chrome"],
        colorScheme: "dark",
      },
    },
    {
      name: "webkit",
      use: {
        ...devices["Desktop Safari"],
        colorScheme: "dark",
      },
    },
  ],
});
