import { spawnSync } from "node:child_process";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const frontendRoot = path.resolve(here, "..");
const repositoryRoot = path.resolve(frontendRoot, "..");

const build = spawnSync("make", ["build"], {
  cwd: repositoryRoot,
  stdio: "inherit",
});
if (build.error) throw build.error;
if (build.status !== 0) process.exit(build.status ?? 1);

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
  },
);
if (result.error) throw result.error;
process.exit(result.status ?? 1);
