import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { access, mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const git = (...args) => execFileSync("git", args, { cwd: repository, encoding: "utf8" }).trim();
console.log(
  `Numbered PDF browser recovery: ${git("rev-parse", "HEAD")} (${git("status", "--porcelain") ? "dirty" : "clean"} tree)`,
);
const scratch = await mkdtemp(path.join(tmpdir(), "docbank-bates-browser-build-"));
let keepScratch = false;
const failures = [];

function run(command, args, env, timeout) {
  const result = spawnSync(command, args, { cwd: repository, env, stdio: "inherit", timeout });
  if (result.error) throw result.error;
  assert.equal(result.status, 0, `${path.basename(command)} ${args[0]} failed`);
}

async function stopAbandonedDaemon(binary, workspace, env) {
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
  for (const [mode, cgo] of [
    ["cgo", "1"],
    ["purego", "0"],
  ]) {
    const binary = path.join(scratch, `docbank-${mode}`);
    const inspector = path.join(scratch, `docbank-page-inspect-${mode}`);
    const workspace = path.join(scratch, mode);
    const report = path.join(scratch, `${mode}.json`);
    const env = {
      ...process.env,
      CGO_ENABLED: cgo,
      DOCBANK_TELEMETRY_ENABLED: "0",
      DOCBANK_SCREENSHOT_BINARY: binary,
      DOCBANK_BATES_RECOVERY_INSPECTOR: inspector,
      DOCBANK_BATES_RECOVERY_WORKSPACE: workspace,
      DOCBANK_BATES_RECOVERY_SCREENSHOT_DIR: path.join(
        repository,
        ".superpowers/bates-recovery-browser",
        mode,
      ),
      PLAYWRIGHT_JSON_OUTPUT_FILE: report,
    };
    try {
      await rm(env.DOCBANK_BATES_RECOVERY_SCREENSHOT_DIR, { recursive: true, force: true });
      console.log(`Building CGO_ENABLED=${cgo} (${mode})`);
      run("go", ["build", "-tags", "fts5", "-o", binary, "./cmd/docbank"], env, 600_000);
      run(
        "go",
        [
          "build",
          "-tags",
          "fts5",
          "-o",
          inspector,
          "./document/pagerender/cmd/docbank-page-inspect",
        ],
        env,
        600_000,
      );
      run(
        process.execPath,
        [
          "frontend/node_modules/@playwright/test/cli.js",
          "test",
          "bates-recovery.screenshot.ts",
          "--config",
          "frontend/screenshots/playwright.config.ts",
          "--project",
          "chromium",
          "--reporter",
          "line,json",
        ],
        env,
        480_000,
      );
      const result = JSON.parse(await readFile(report, "utf8"));
      assert.equal(result.errors.length, 0, `${mode} runner errors`);
      const { expected, unexpected, flaky, skipped } = result.stats;
      assert.deepEqual(
        { expected, unexpected, flaky, skipped },
        { expected: 1, unexpected: 0, flaky: 0, skipped: 0 },
        `${mode} must execute one passing case`,
      );
      console.log(`${mode}: executed=1 passed=1 skipped=0 failed=0`);
    } catch (error) {
      failures.push(mode);
      keepScratch = true;
      console.error(`${mode}: ${error.message}`);
    } finally {
      try {
        await stopAbandonedDaemon(binary, workspace, env);
      } catch (error) {
        keepScratch = true;
        failures.push(`${mode} shutdown`);
        console.error(error.message);
      }
    }
  }
} finally {
  if (keepScratch) console.error(`Qualification scratch retained at ${scratch}`);
  else await rm(scratch, { recursive: true, force: true });
}
if (failures.length)
  throw new Error(`Numbered PDF browser recovery failed: ${failures.join(", ")}`);
