import { defineConfig, devices } from "@playwright/test";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const repositoryRoot = path.resolve(here, "..", "..");

export default defineConfig({
  testDir: ".",
  testMatch: /.*\.screenshot\.ts/,
  fullyParallel: false,
  workers: 1,
  timeout: 120_000,
  expect: {
    timeout: 15_000,
  },
  outputDir: path.join(repositoryRoot, ".superpowers", "playwright"),
  reporter: "line",
  use: {
    ...devices["Desktop Chrome"],
    viewport: { width: 1440, height: 960 },
    deviceScaleFactor: 1,
    colorScheme: "dark",
    trace: "off",
  },
  projects: [
    {
      name: "chromium",
      use: {
        ...devices["Desktop Chrome"],
        viewport: { width: 1440, height: 960 },
        deviceScaleFactor: 1,
        colorScheme: "dark",
      },
    },
  ],
});
