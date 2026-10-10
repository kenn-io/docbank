import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";
import { createSyntheticCameraFiles } from "./camera-fixture";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const binary = process.env.DOCBANK_SCREENSHOT_BINARY ?? path.join(repository, "bin", process.platform === "win32" ? "docbank.exe" : "docbank");
const output = process.env.DOCBANK_SCREENSHOT_DIR!;
const before = process.env.DOCBANK_ACTIVITY_BEFORE === "1";

test("activity lanes and controls use the real daemon", async ({ browser }) => {
  test.setTimeout(240_000);
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-activity-"));
  const env = { ...process.env, DOCBANK_HOME: path.join(workspace, "vault"), DOCBANK_LOCK_DIR: path.join(workspace, "locks") };
  const run = async (...args: string[]) => (await exec(binary, args, { cwd: repository, env, timeout: 60_000 })).stdout.trim();
  const context = await browser.newContext({ viewport: { width: 1440, height: 960 }, deviceScaleFactor: Number(process.env.DOCBANK_SCREENSHOT_SCALE ?? 1), colorScheme: "dark" });
  const page = await context.newPage();
  try {
    await mkdir(output, { recursive: true });
    const source = path.join(workspace, "camera");
    await createSyntheticCameraFiles(source, 1);
    // A larger second import keeps running long enough to report a known total.
    const bulk = path.join(workspace, "camera-bulk");
    await createSyntheticCameraFiles(bulk, 100, 2);
    const url = await run("web", "--no-browser");
    await run("jobs", "pause", "photo_import");
    await run("jobs", "pause", "place");
    const { id: completedID } = JSON.parse(await run("photos", "import", source, "/Photos/Trip", "--json")) as { id: string };
    const { id: bulkID } = JSON.parse(await run("photos", "import", bulk, "/Photos/Second trip", "--json")) as { id: string };
    type Listed = { items: { operation_id?: string; status: string; completed_objects?: number; total_objects?: number; error?: string }[] };
    const bulkImport = async () => (JSON.parse(await run("jobs", "--json")) as Listed).items.find((job) => job.operation_id === bulkID);
    const bulkOperation = page.locator(".operation").filter({ has: page.getByText(bulkID, { exact: true }) });
    const completedImport = async () => (JSON.parse(await run("jobs", "--json")) as Listed).items.find((job) => job.operation_id === completedID);
    const completedOperation = page.locator(".operation").filter({ has: page.getByText(completedID, { exact: true }) });
    if (before) {
      // The base drawer has no lane controls, so the CLI reaches the same paused, known-progress state.
      await run("jobs", "resume", "photo_import");
      await expect.poll(async () => (await bulkImport())?.completed_objects ?? 0, { timeout: 60_000 }).toBeGreaterThan(0);
      await run("jobs", "pause", "photo_import");
      await expect.poll(async () => (await bulkImport())?.status, { timeout: 30_000 }).toBe("queued");
    }
    await page.goto(url);
    await page.getByRole("button", { name: "Background jobs", exact: true }).click();
    await expect(page.getByText("Photo import", { exact: true }).first()).toBeVisible();
    if (!before) {
      await expect(page.getByText("Photo import", { exact: true })).toHaveCount(1);
      await expect(page.getByRole("button", { name: "Resume Photo import" })).toBeVisible();
      await expect(page.getByRole("button", { name: /^Cancel Photo import/ })).toHaveCount(2);
      await expect(page.getByRole("button", { name: "Resume Storage placement" })).toBeVisible();
      await expect(bulkOperation.getByText("Queued", { exact: true })).toBeVisible();
      await page.getByRole("button", { name: "Resume Photo import" }).click();
      await expect.poll(async () => (await bulkImport())?.completed_objects ?? 0, { timeout: 60_000 }).toBeGreaterThan(0);
      await expect(bulkOperation.getByText(/^\d+ of 100 groups$/)).toBeVisible();
      await page.getByRole("button", { name: "Pause Photo import" }).click();
      await expect(page.getByRole("button", { name: "Resume Photo import" })).toBeVisible();
      await expect.poll(async () => (await bulkImport())?.status, { timeout: 30_000 }).toBe("queued");
      await expect(bulkOperation.getByText("Queued", { exact: true })).toBeVisible();
    }
    const drawer = page.getByRole("dialog", { name: "Daemon background jobs" });
    await expect.poll(async () => Math.round((await drawer.boundingBox())!.x)).toBe(820);
    await page.getByText("Photo import", { exact: true }).first().evaluate((element) => element.scrollIntoView({ block: "start", behavior: "instant" }));
    const card = page.locator(".kit-card").filter({ has: page.getByText("Photo import", { exact: true }) }).first();
    const bounds = (await card.boundingBox())!;
    for (const theme of ["dark", "light"] as const) {
      await page.emulateMedia({ colorScheme: theme });
      await page.screenshot({ path: path.join(output, `activity-${before ? "before" : "after"}-${theme}.png`), animations: "disabled", clip: { x: 680, y: Math.min(340, Math.max(0, bounds.y - 40)), width: 760, height: 620 } });
    }
    await page.setViewportSize({ width: 390, height: 844 });
    await page.getByText("Photo import", { exact: true }).first().evaluate((element) => element.scrollIntoView({ block: "start", behavior: "instant" }));
    for (const theme of ["dark", "light"] as const) {
      await page.emulateMedia({ colorScheme: theme });
      await page.screenshot({ path: path.join(output, `activity-${before ? "before" : "after"}-narrow-${theme}.png`), animations: "disabled" });
    }
    if (!before) {
      // Visual previews carries the concurrency control in its header actions.
      await page.getByText("Visual previews", { exact: true }).evaluate((element) => element.scrollIntoView({ block: "center", behavior: "instant" }));
      await expect(page.getByRole("combobox", { name: /^Concurrency Visual previews/ })).toBeInViewport();
      for (const theme of ["dark", "light"] as const) {
        await page.emulateMedia({ colorScheme: theme });
        await page.screenshot({ path: path.join(output, `activity-after-narrow-previews-${theme}.png`), animations: "disabled" });
      }
    }
    await page.setViewportSize({ width: 1440, height: 960 });
    if (!before) {
      await page.getByRole("button", { name: "Resume Storage placement" }).click();
      await expect(page.getByRole("button", { name: "Pause Storage placement" })).toBeVisible();
      await page.getByRole("button", { name: "Resume Photo import" }).click();
      await expect(page.getByRole("button", { name: "Pause Photo import" })).toBeVisible();
      await bulkOperation.getByRole("button", { name: /^Cancel Photo import/ }).click();
      await expect.poll(async () => (await bulkImport())?.status, { timeout: 30_000 }).toBe("cancelled");
      await expect(bulkOperation.getByText("Cancelled", { exact: true })).toBeVisible();
      await expect.poll(async () => (await completedImport())?.status, { timeout: 60_000 }).toBe("completed");
      await expect(completedOperation.getByText("Completed", { exact: true })).toBeVisible();
      await expect(completedOperation.getByText("1 of 1 groups", { exact: true })).toBeVisible();
      const controlsPath = path.join(env.DOCBANK_HOME, "lane-controls.json");
      const settings = await readFile(controlsPath);
      try {
        await writeFile(controlsPath, "{");
        await page.getByRole("button", { name: "Refresh background jobs" }).click();
        await expect(page.getByRole("alert")).toBeVisible();
        await expect(completedOperation.getByText("1 of 1 groups", { exact: true })).toBeVisible();
        await page.screenshot({ path: path.join(output, "activity-controls-unavailable.png"), animations: "disabled" });
      } finally {
        await writeFile(controlsPath, settings);
      }
    }
  } finally {
    await context.close();
    try { await run("daemon", "stop"); } finally { await rm(workspace, { recursive: true, force: true }); }
  }
});
