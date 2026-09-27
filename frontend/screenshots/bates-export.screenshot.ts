import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const run = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const binary = path.join(repository, "docbank");
const output = process.env.DOCBANK_BATES_SCREENSHOT_DIR;
test.skip(!output, "DOCBANK_BATES_SCREENSHOT_DIR enables the synthetic Bates proof");

test("publishes selected pages from a sealed synthetic package through the real daemon", async ({ page }) => {
  test.setTimeout(180_000);
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-bates-screenshot-"));
  const vault = path.join(workspace, "vault");
  const docbank = async (...args: string[]): Promise<string> => (await run(binary, args, {
    cwd: repository, env: { ...process.env, DOCBANK_HOME: vault }, timeout: 60_000, maxBuffer: 4 * 1024 * 1024,
  })).stdout.trim();
  try {
    await mkdir(output!, { recursive: true, mode: 0o700 });
    await run("go", ["run", "-tags", "fts5", "./frontend/screenshots/bates-fixture.go", vault], {
      cwd: repository, env: { ...process.env, GOTOOLCHAIN: "go1.27.0" }, timeout: 90_000, maxBuffer: 4 * 1024 * 1024,
    });

    const webURL = await docbank("web", "--no-browser");
    await page.setViewportSize({ width: 1440, height: 1400 });
    await page.addInitScript(() => localStorage.setItem("docbank-theme", "dark"));
    await page.goto(webURL, { waitUntil: "domcontentloaded" });
    await page.getByRole("button", { name: "Bates export", exact: true }).click();
    const drawer = page.getByRole("dialog", { name: "Bates export" });
    await expect(drawer.getByText("Synthetic-Bates-production", { exact: false })).toBeVisible();
    await drawer.getByRole("textbox", { name: "Bates prefix" }).fill("CASE");
    await drawer.getByRole("button", { name: "Create namespace", exact: true }).click();
    await drawer.getByRole("button", { name: "Preview Bates labels", exact: true }).click();
    await expect(drawer.getByText("CASE000001", { exact: true })).toBeVisible();
    await expect(drawer.getByText("CASE000002", { exact: true })).toBeVisible();
    await drawer.getByRole("button", { name: "Reserve CASE000001–CASE000002", exact: true }).click();
    await expect(drawer.getByText("Range reserved", { exact: true })).toBeVisible();
    await drawer.getByRole("button", { name: "Start Bates export", exact: true }).click();
    await expect(drawer.getByText("Stamped PDF ready", { exact: true })).toBeVisible({ timeout: 60_000 });
    await expect(drawer.getByRole("button", { name: "Download verified PDF", exact: true })).toBeVisible();
    await expect(drawer.getByRole("button", { name: /Open Bates export/ })).toBeVisible();
    await page.screenshot({ path: path.join(output!, "web-bates-export.png"), fullPage: true, animations: "disabled" });
  } finally {
    try { await docbank("daemon", "stop"); } catch { /* daemon may not have started */ }
    await rm(workspace, { recursive: true, force: true });
    await expect(stat(workspace)).rejects.toMatchObject({ code: "ENOENT" });
  }
});
