import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const git = (...args) => execFileSync("git", args, { cwd: repository, encoding: "utf8" }).trim();
const commit = git("rev-parse", "HEAD");
const dirty = git("status", "--porcelain") !== "";
console.log(`Browser qualification: ${commit} (${dirty ? "dirty" : "clean"} tree)`);
const scratch = await mkdtemp(path.join(tmpdir(), "docbank-browser-build-"));
const failures = [];
// Playwright stops its own run, retries included, before the watchdog kills it,
// so its JSON reporter always writes the result.
const browserWatchdog = 360_000;
const browserGlobalTimeout = browserWatchdog - 30_000;
// Screenshot proofs a branch adds to this lane. Each entry is { spec, env }:
// the branch run sets env=branch. Each spec holds exactly one test, so the
// stats check counts specs.
const proofs = [
  // P07 role search: matched-role chips and the People facet (#1453).
  { spec: "role-search.screenshot.ts", env: "DOCBANK_ROLE_SEARCH_PROOF" },
];
const proofEnv = (side) => Object.fromEntries(proofs.map((proof) => [proof.env, side]));
// Runs the named specs against one binary and requires every case to pass.
const qualify = async (mode, binary, specs, extraEnv) => {
  // CI keeps each mode's JSON report for upload; proof screenshots ride in it
  // as base64 attachments.
  const report = process.env.CI
    ? path.join(repository, "frontend/test-results/playwright", `report-export-${mode}.json`)
    : path.join(scratch, `${mode}.json`);
  const env = {
    ...process.env,
    DOCBANK_SCREENSHOT_BINARY: binary,
    PLAYWRIGHT_JSON_OUTPUT_FILE: report,
    ...extraEnv,
  };
  const run = spawnSync(
    process.execPath,
    [
      "frontend/node_modules/@playwright/test/cli.js",
      "test",
      ...specs,
      "--config",
      "frontend/screenshots/playwright.config.ts",
      "--project",
      "chromium",
      "--reporter",
      "line,json",
      "--global-timeout",
      String(browserGlobalTimeout),
    ],
    { cwd: repository, env, stdio: "inherit", timeout: browserWatchdog },
  );
  if (run.error) throw run.error;
  assert.equal(run.status, 0, `${mode} browser cases failed`);
  const result = JSON.parse(await readFile(report, "utf8"));
  assert.equal(result.errors.length, 0, `${mode} runner errors`);
  const { expected, unexpected, flaky, skipped } = result.stats;
  assert.deepEqual(
    { expected, unexpected, flaky, skipped },
    { expected: specs.length, unexpected: 0, flaky: 0, skipped: 0 },
    `${mode} must execute ${specs.length} passing cases`,
  );
  console.log(`${mode}: executed=${specs.length} passed=${specs.length} skipped=0 failed=0`);
};
try {
  for (const [mode, cgo] of [
    ["cgo", "1"],
    ["purego", "0"],
  ]) {
    console.log(`Building CGO_ENABLED=${cgo} (${mode})`);
    const binary = path.join(
      scratch,
      `docbank-${mode}${process.platform === "win32" ? ".exe" : ""}`,
    );
    const env = {
      ...process.env,
      CGO_ENABLED: cgo,
      DOCBANK_REPORT_EXPORT_SCREENSHOT_DIR: path.join(
        repository,
        ".superpowers/report-export-browser",
        mode,
      ),
      ...proofEnv("branch"),
    };
    try {
      await rm(env.DOCBANK_REPORT_EXPORT_SCREENSHOT_DIR, { recursive: true, force: true });
      const build = spawnSync("go", ["build", "-tags", "fts5", "-o", binary, "./cmd/docbank"], {
        cwd: repository,
        env,
        stdio: "inherit",
        timeout: 600_000,
      });
      if (build.error) throw build.error;
      assert.equal(build.status, 0, `${mode} build failed`);
      await qualify(
        mode,
        binary,
        ["report-export.screenshot.ts", ...proofs.map((proof) => proof.spec)],
        env,
      );
    } catch (error) {
      failures.push(mode);
      console.error(`${mode}: ${error.message}`);
    }
  }
} finally {
  await rm(scratch, { recursive: true, force: true });
}
if (failures.length) {
  throw new Error(`Browser qualification failed: ${failures.join(", ")}`);
}
