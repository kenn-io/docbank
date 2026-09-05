import { spawnSync } from "node:child_process";
import { mkdir, readFile, rm } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { publishScreenshots } from "./publish.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
const frontendRoot = path.resolve(here, "..");
const repositoryRoot = path.resolve(frontendRoot, "..");
const workspaceRoot = path.join(repositoryRoot, ".superpowers");
const output = path.join(workspaceRoot, "screenshots");
const staging = path.join(workspaceRoot, ".screenshots.next");
const manifestPath = path.join(repositoryRoot, "scripts", "docs-assets.txt");

const build = spawnSync("make", ["build"], {
  cwd: repositoryRoot,
  stdio: "inherit",
});
if (build.error) throw build.error;
if (build.status !== 0) process.exit(build.status ?? 1);

await rm(staging, { recursive: true, force: true });
await mkdir(staging, { recursive: true, mode: 0o700 });

const playwrightCLI = path.join(
  frontendRoot,
  "node_modules",
  "@playwright",
  "test",
  "cli.js",
);
const result = spawnSync(
  process.execPath,
  [
    playwrightCLI,
    "test",
    "--config",
    path.join(here, "playwright.config.ts"),
    "--project",
    "chromium",
    ...process.argv.slice(2),
  ],
  {
    cwd: frontendRoot,
    stdio: "inherit",
    env: {
      ...process.env,
      DOCBANK_SCREENSHOT_DIR: staging,
    },
  },
);
if (result.error) throw result.error;
if (result.status !== 0) {
  await rm(staging, { recursive: true, force: true });
  process.exit(result.status ?? 1);
}

const names = (await readFile(manifestPath, "utf8"))
  .split("\n")
  .filter((line) => line !== "");
await publishScreenshots({ output, staging, names });
