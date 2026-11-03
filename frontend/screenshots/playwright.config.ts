import { defineConfig, devices } from "@playwright/test";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const repositoryRoot = path.resolve(here, "..", "..");

// Every spec's daemon inherits this, so harness runs never count as installs.
process.env.DOCBANK_TELEMETRY_ENABLED = "0";

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
  // CI retries a failed test and records every result as JSON, which CI
  // uploads so a test that passed only on a retry stays visible. Each CI run
  // names its suite so its report has its own file.
  retries: process.env.CI ? 2 : 0,
  reporter: process.env.CI
    ? [
        ["line"],
        [
          "json",
          {
            outputFile: path.join(
              here,
              "..",
              "test-results",
              "playwright",
              `${process.env.DOCBANK_PLAYWRIGHT_SUITE ?? "screenshots"}.json`,
            ),
          },
        ],
      ]
    : "line",
  use: {
    ...devices["Desktop Chrome"],
    viewport: { width: 1440, height: 960 },
    deviceScaleFactor: Number(process.env.DOCBANK_SCREENSHOT_SCALE ?? 1),
    colorScheme: "dark",
    trace: "off",
  },
  projects: [
    {
      name: "chromium",
      use: {
        ...devices["Desktop Chrome"],
        viewport: { width: 1440, height: 960 },
        deviceScaleFactor: Number(process.env.DOCBANK_SCREENSHOT_SCALE ?? 1),
        colorScheme: "dark",
      },
    },
  ],
});
