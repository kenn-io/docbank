import { expect, test } from "@playwright/test";
import { execFile, spawn } from "node:child_process";
import { cp, mkdir, mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const run = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const binary = path.join(repository, "docbank");
const output = process.env.DOCBANK_PRODUCTION_SCREENSHOT_DIR;
test.skip(!output, "DOCBANK_PRODUCTION_SCREENSHOT_DIR enables the synthetic production screenshot");

test("looks up a retained synthetic production job through the real daemon", async ({ page }) => {
  test.setTimeout(240_000);
  page.setDefaultTimeout(15_000);
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-job-screenshot-"));
  const ready = path.join(workspace, "fixture-ready.json");
  const done = path.join(workspace, "fixture-done");
  const vault = path.join(workspace, "vault");
  const fixture = spawn("go", ["test", "-tags", "fts5", "./internal/store", "-run", "^TestProductionJobScreenshotFixture$", "-count=1"], {
    cwd: repository, env: { ...process.env, GOTOOLCHAIN: "go1.27.0",
      DOCBANK_PRODUCTION_JOB_FIXTURE_READY: ready, DOCBANK_PRODUCTION_JOB_FIXTURE_DONE: done },
  });
  let fixtureOutput = "";
  fixture.stdout.on("data", chunk => { fixtureOutput += String(chunk); });
  fixture.stderr.on("data", chunk => { fixtureOutput += String(chunk); });
  const fixtureExit = new Promise<number | null>(resolve => fixture.on("exit", resolve));
  let daemonStarted = false;
  const docbank = async (...args: string[]): Promise<string> => (await run(binary, args, {
    cwd: repository, env: { ...process.env, DOCBANK_HOME: vault }, timeout: 60_000, maxBuffer: 4 * 1024 * 1024,
  })).stdout.trim();
  try {
    await expect.poll(async () => {
      try { return JSON.parse(await readFile(ready, "utf8")) as { root: string; set_id: string; job_id: string }; }
      catch { return null; }
    }, { timeout: 180_000, message: `synthetic job fixture did not appear: ${fixtureOutput}` }).not.toBeNull();
    const source = JSON.parse(await readFile(ready, "utf8")) as { root: string; set_id: string; job_id: string };
    await cp(source.root, vault, { recursive: true });
    await writeFile(done, "copied", { mode: 0o600 });
    expect(await fixtureExit, fixtureOutput).toBe(0);

    await mkdir(output!, { recursive: true, mode: 0o700 });
    const webURL = await docbank("web", "--no-browser");
    daemonStarted = true;
    await page.setViewportSize({ width: 1440, height: 1100 });
    await page.addInitScript(() => localStorage.setItem("docbank-theme", "dark"));
    const outside: string[] = [];
    const browserErrors: string[] = [];
    page.on("request", request => {
      if (/^https?:/.test(request.url()) && new URL(request.url()).origin !== new URL(webURL).origin) outside.push(request.url());
    });
    page.on("pageerror", error => browserErrors.push(error.message));
    page.on("console", message => { if (message.type() === "error") browserErrors.push(message.text()); });
    page.on("response", response => { if (response.status() >= 400) browserErrors.push(`${response.status()} ${response.url()}`); });
    await page.goto(webURL, { waitUntil: "domcontentloaded" });
    await page.getByRole("button", { name: "Production sets" }).click();
    const drawer = page.getByRole("dialog", { name: "Production sets" });
    await drawer.getByRole("button", { name: "Synthetic job fixture" }).click();
    await drawer.getByRole("textbox", { name: "Job ID" }).fill(source.job_id);
    await drawer.getByRole("button", { name: "Look up job" }).click();
    const status = drawer.getByRole("region", { name: `Status for production job ${source.job_id}` });
    await expect(status.getByText("canceled", { exact: true })).toBeVisible();
    await expect(status.getByText(source.job_id)).toBeVisible();
    await status.getByRole("button", { name: "Refresh status" }).click();
    await expect(status.getByText("canceled", { exact: true })).toBeVisible();
    expect(outside).toEqual([]);
    expect(browserErrors).toEqual([]);
    await drawer.getByRole("region", { name: "Production job status" }).screenshot({
      path: path.join(output!, "web-production-job-status.png"), animations: "disabled",
    });
  } finally {
    await writeFile(done, "stopped", { mode: 0o600 });
    await fixtureExit;
    if (daemonStarted) { try { await docbank("daemon", "stop"); } catch { /* daemon may already be stopped */ } }
    await rm(workspace, { recursive: true, force: true });
    await expect(stat(workspace)).rejects.toMatchObject({ code: "ENOENT" });
  }
});
