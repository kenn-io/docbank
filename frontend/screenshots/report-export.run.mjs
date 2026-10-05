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
try {
  for (const [mode, cgo] of [["cgo", "1"], ["purego", "0"]]) {
    console.log(`Building CGO_ENABLED=${cgo} (${mode})`);
    const binary = path.join(scratch, `docbank-${mode}${process.platform === "win32" ? ".exe" : ""}`);
    const report = path.join(scratch, `${mode}.json`);
    const env = {
      ...process.env, CGO_ENABLED: cgo, DOCBANK_SCREENSHOT_BINARY: binary,
      DOCBANK_REPORT_EXPORT_SCREENSHOT_DIR:
        path.join(repository, ".superpowers/report-export-browser", mode),
      PLAYWRIGHT_JSON_OUTPUT_FILE: report,
    };
    try {
      const build = spawnSync("go", ["build", "-tags", "fts5", "-o", binary, "./cmd/docbank"], {
        cwd: repository, env, stdio: "inherit", timeout: 600_000,
      });
      if (build.error) throw build.error;
      assert.equal(build.status, 0, `${mode} build failed`);
      const run = spawnSync(process.execPath, [
        "frontend/node_modules/@playwright/test/cli.js", "test", "report-export.screenshot.ts",
        "--config", "frontend/screenshots/playwright.config.ts", "--project", "chromium",
        "--reporter", "line,json",
      ], { cwd: repository, env, stdio: "inherit", timeout: 360_000 });
      if (run.error) throw run.error;
      assert.equal(run.status, 0, `${mode} browser case failed`);
      const result = JSON.parse(await readFile(report, "utf8"));
      assert.equal(result.errors.length, 0, `${mode} runner errors`);
      const { expected, unexpected, flaky, skipped } = result.stats;
      assert.deepEqual({ expected, unexpected, flaky, skipped },
        { expected: 1, unexpected: 0, flaky: 0, skipped: 0 }, `${mode} must execute one passing case`);
      console.log(`${mode}: executed=1 passed=1 skipped=0 failed=0`);
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
