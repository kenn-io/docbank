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
const output = process.env.DOCBANK_PRODUCTION_SCREENSHOT_DIR;
test.skip(!output, "DOCBANK_PRODUCTION_SCREENSHOT_DIR enables the synthetic production screenshot");

test("creates and inspects a production draft through the real daemon", async ({ page }) => {
  test.setTimeout(180_000);
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-production-screenshot-"));
  const vault = path.join(workspace, "vault");
  const docbank = async (...args: string[]): Promise<string> => (await run(binary, args, {
    cwd: repository, env: { ...process.env, DOCBANK_HOME: vault }, timeout: 60_000, maxBuffer: 4 * 1024 * 1024,
  })).stdout.trim();
  try {
    await mkdir(output!, { recursive: true, mode: 0o700 });
    await docbank("production", "sets", "create", "--name", "Synthetic review", "--json");
    const webURL = await docbank("web", "--no-browser");
    await page.setViewportSize({ width: 1440, height: 1100 });
    await page.addInitScript(() => localStorage.setItem("docbank-theme", "dark"));
    await page.goto(webURL, { waitUntil: "domcontentloaded" });
    await page.getByRole("button", { name: "Production sets" }).click();
    const drawer = page.getByRole("dialog", { name: "Production sets" });
    await expect(drawer.getByRole("button", { name: "Synthetic review" })).toBeVisible();
    await drawer.getByRole("textbox", { name: "Set name" }).fill("Synthetic follow-up");
    await drawer.getByRole("textbox", { name: "Instructions" }).fill("Review selected synthetic pages.");
    await drawer.getByRole("button", { name: "Create draft" }).click();
    await expect(drawer.getByText("Draft revision 1")).toBeVisible();
    await expect(drawer.getByText("Membership open")).toBeVisible();
    await page.screenshot({ path: path.join(output!, "web-production-sets.png"), fullPage: true, animations: "disabled" });
  } finally {
    try { await docbank("daemon", "stop"); } catch { /* daemon may not have started */ }
    await rm(workspace, { recursive: true, force: true });
    await expect(stat(workspace)).rejects.toMatchObject({ code: "ENOENT" });
  }
});
