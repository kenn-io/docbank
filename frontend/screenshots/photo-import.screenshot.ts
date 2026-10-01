import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const binary = process.env.DOCBANK_SCREENSHOT_BINARY ??
  path.join(repository, process.platform === "win32" ? "docbank.exe" : "docbank");
const output = process.env.DOCBANK_SCREENSHOT_DIR;
if (!output) throw new Error("DOCBANK_SCREENSHOT_DIR is required");

test("photo import progress and cancellation are visible", async ({ page }) => {
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-photo-import-screenshot-"));
  const vault = path.join(workspace, "vault");
  const lock = path.join(workspace, "locks");
  const source = path.join(workspace, "camera");
  const run = async (...args: string[]) =>
    (await exec(binary, args, { cwd: repository, env: { ...process.env, DOCBANK_HOME: vault, DOCBANK_LOCK_DIR: lock }, timeout: 60_000 })).stdout.trim();
  try {
    await mkdir(path.join(source, "trip"), { recursive: true, mode: 0o700 });
    await writeFile(path.join(source, "trip", "IMG_0001.JPG"), Buffer.from("synthetic-jpeg"), { mode: 0o600 });
    await writeFile(path.join(source, "trip", "IMG_0001.ARW"), Buffer.from("synthetic-raw"), { mode: 0o600 });
    for (let index = 2; index <= 2000; index += 1) {
      await writeFile(path.join(source, "trip", `IMG_${String(index).padStart(4, "0")}.JPG`), Buffer.from(`synthetic-jpeg-${index}`), { mode: 0o600 });
    }
    const webURL = await run("web", "--no-browser");
    await page.goto(webURL, { waitUntil: "domcontentloaded" });
    await page.getByRole("button", { name: "Background jobs", exact: true }).click();
    await run("photos", "import", source, "/Photos/Trip");
    await page.getByRole("button", { name: "Refresh background jobs" }).click();
    const progress = page.getByRole("progressbar", { name: "Photo import progress" });
    await expect(progress).toBeVisible();
    const cancel = page.getByRole("button", { name: "Cancel photo import" });
    await expect(cancel).toBeVisible();
    await expect.poll(async () => {
      await page.getByRole("button", { name: "Refresh background jobs" }).click();
      return Number(await progress.getAttribute("aria-valuenow"));
    }).toBeGreaterThan(0);
    const drawer = page.getByLabel("Daemon background jobs");
    await cancel.scrollIntoViewIfNeeded();
    await drawer.screenshot({ path: path.join(output, "web-photo-import-dark.png") });
    await page.screenshot({ path: path.join(output, "web-photo-import-body.png") });
    for (const width of [1280, 768, 400]) {
      await page.setViewportSize({ width, height: 960 });
      await cancel.scrollIntoViewIfNeeded();
      await drawer.screenshot({ path: path.join(output, `web-photo-import-${width}-dark.png`) });
      await page.emulateMedia({ colorScheme: "light" });
      await drawer.screenshot({ path: path.join(output, `web-photo-import-${width}-light.png`) });
      await page.emulateMedia({ colorScheme: "dark" });
    }
    await cancel.click();
    await expect(page.getByText(/^cancelled$/)).toBeVisible();
    await page.emulateMedia({ colorScheme: "light" });
    await drawer.screenshot({ path: path.join(output, "web-photo-import-light.png") });
    await page.setViewportSize({ width: 400, height: 960 });
    await page.emulateMedia({ colorScheme: "dark" });
    await drawer.screenshot({ path: path.join(output, "web-photo-import-mobile-dark.png") });
  } finally {
    await run("daemon", "stop");
    await rm(workspace, { recursive: true, force: true });
  }
});
