import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { access, mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

// Hosted Taxes page capture: one synthetic vault, the real daemon, PNGs in
// .superpowers/taxes-shots for the workflow artifact.
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const git = (...args) => execFileSync("git", args, { cwd: repository, encoding: "utf8" }).trim();
console.log(
  `Taxes page screenshots: ${git("rev-parse", "HEAD")} (${git("status", "--porcelain") ? "dirty" : "clean"} tree)`,
);
const scratch = await mkdtemp(path.join(tmpdir(), "docbank-taxes-screenshots-"));
const binary = path.join(scratch, "docbank");
const workspace = path.join(scratch, "workspace");
const report = path.join(scratch, "report.json");
const shots = path.join(repository, ".superpowers/taxes-shots");
const env = {
  ...process.env,
  DOCBANK_TELEMETRY_ENABLED: "0",
  DOCBANK_SCREENSHOT_BINARY: binary,
  DOCBANK_TAXES_WORKSPACE: workspace,
  DOCBANK_TAXES_SCREENSHOT_DIR: shots,
  PLAYWRIGHT_JSON_OUTPUT_FILE: report,
};
let keepScratch = false;

function run(command, args, timeout) {
  const result = spawnSync(command, args, { cwd: repository, env, stdio: "inherit", timeout });
  if (result.error) throw result.error;
  assert.equal(result.status, 0, `${path.basename(command)} ${args[0]} failed`);
}

async function stopAbandonedDaemon() {
  try {
    await access(path.join(workspace, "vault"));
  } catch {
    return;
  }
  const options = {
    cwd: repository,
    env: { ...env, DOCBANK_HOME: path.join(workspace, "vault") },
    encoding: "utf8",
    timeout: 30_000,
  };
  const status = () => JSON.parse(execFileSync(binary, ["daemon", "status", "--json"], options));
  if (status().running) execFileSync(binary, ["daemon", "stop"], options);
  assert.equal(status().running, false, "Synthetic daemon must stop before removing its binary");
}

try {
  await rm(shots, { recursive: true, force: true });
  run("go", ["build", "-tags", "fts5", "-o", binary, "./cmd/docbank"], 600_000);
  run(
    process.execPath,
    [
      "frontend/node_modules/@playwright/test/cli.js",
      "test",
      "taxes.screenshot.ts",
      "--config",
      "frontend/screenshots/playwright.config.ts",
      "--project",
      "chromium",
      // The run requires flaky: 0, so a retry could only hide the first failure.
      "--retries",
      "0",
      "--reporter",
      "line,json",
    ],
    480_000,
  );
  const result = JSON.parse(await readFile(report, "utf8"));
  assert.equal(result.errors.length, 0, "runner errors");
  const { expected, unexpected, flaky, skipped } = result.stats;
  assert.deepEqual(
    { expected, unexpected, flaky, skipped },
    { expected: 1, unexpected: 0, flaky: 0, skipped: 0 },
    "the Taxes capture must execute one passing case",
  );
  console.log("taxes: executed=1 passed=1 skipped=0 failed=0");
} catch (error) {
  keepScratch = true;
  throw error;
} finally {
  try {
    await stopAbandonedDaemon();
  } catch (error) {
    keepScratch = true;
    process.exitCode = 1;
    console.error(error.message);
  }
  if (keepScratch) console.error(`Screenshot scratch retained at ${scratch}`);
  else await rm(scratch, { recursive: true, force: true });
}
