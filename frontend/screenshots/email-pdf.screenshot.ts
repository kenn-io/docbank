import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdir, mkdtemp, readFile, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const run = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const binary = path.join(repository, "docbank");
const backup = process.env.DOCBANK_EMAILPDF_SCREENSHOT_BACKUP;
const output = process.env.DOCBANK_SCREENSHOT_DIR;

test.describe("retained email PDF screenshot", () => {
  test.skip(!backup, "requires a real Chromium-produced synthetic backup fixture");
  let workspace = "";
  let control = "";
  let vault = "";
  let webURL = "";
  async function docbank(home: string, args: string[]): Promise<string> {
    return (await run(binary, args, { cwd: repository, env: { ...process.env, DOCBANK_HOME: home }, timeout: 90_000, maxBuffer: 4 * 1024 * 1024 })).stdout.trim();
  }
  test.beforeAll(async () => {
    if (!backup || !output) throw new Error("synthetic backup and screenshot output are required");
    if ((await readFile(path.join(backup, "DB29-SYNTHETIC"), "utf8")).trim() !== "docbank-email-pdf-synthetic/v1") throw new Error("fixture lacks explicit synthetic-only witness");
    workspace = await mkdtemp(path.join(tmpdir(), "docbank-email-pdf-screenshot-"));
    control = path.join(workspace, "control");
    vault = path.join(workspace, "restored");
    await mkdir(output, { recursive: true, mode: 0o700 });
    await docbank(control, ["backup", "restore", "--repo", backup, "--target", vault]);
    webURL = await docbank(vault, ["web", "--no-browser"]);
    const url = new URL(webURL);
    if (!/^docbank-[0-9a-f]{32}\.localhost$/.test(url.hostname) || url.protocol !== "http:" || !url.hash) throw new Error("unexpected daemon browser origin");
  });
  test.afterAll(async () => {
    for (const home of [vault, control]) {
      if (!home) continue;
      await docbank(home, ["daemon", "stop"]);
      const status = JSON.parse(await docbank(home, ["daemon", "status", "--json"])) as { running: boolean };
      if (status.running) throw new Error(`synthetic daemon still live; retained ${workspace}`);
    }
    if (workspace) { await rm(workspace, { recursive: true, force: true }); await expect(stat(workspace)).rejects.toMatchObject({ code: "ENOENT" }); }
  });
  test("restored exact version downloads verified Chromium PDF without renderer", async ({ page }) => {
    await page.goto(webURL, { waitUntil: "domcontentloaded" });
    await page.getByRole("cell", { name: "synthetic.eml", exact: true }).click();
    await page.getByRole("button", { name: "Version history", exact: true }).click();
    const region = page.getByRole("region", { name: "Exact email PDF" });
    const retained = region.getByRole("button", { name: /^Download retained PDF/ }).first();
    await expect(retained).toBeVisible();
    const pending = page.waitForEvent("download");
    await retained.click();
    const download = await pending;
    const downloaded = await download.path();
    if (!downloaded) throw new Error("missing verified browser download");
    const bytes = await readFile(downloaded);
    expect(bytes.subarray(0, 5).toString()).toBe("%PDF-");
    await expect(region).toContainText("browser save started");
    await page.screenshot({ path: path.join(output!, "web-email-pdf-retained.png"), fullPage: true, animations: "disabled" });
    test.info().annotations.push({ type: "verified-pdf-sha256", description: createHash("sha256").update(bytes).digest("hex") });
    await download.delete();
  });
});
