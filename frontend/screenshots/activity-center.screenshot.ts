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
const binary =
  process.env.DOCBANK_SCREENSHOT_BINARY ??
  path.join(repository, "bin", process.platform === "win32" ? "docbank.exe" : "docbank");
const output = process.env.DOCBANK_SCREENSHOT_DIR!;

test("activity lanes and controls use the real daemon", async ({ browser }) => {
  test.setTimeout(240_000);
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-activity-"));
  const env = {
    ...process.env,
    DOCBANK_HOME: path.join(workspace, "vault"),
    DOCBANK_LOCK_DIR: path.join(workspace, "locks"),
  };
  const run = async (...args: string[]) =>
    (await exec(binary, args, { cwd: repository, env, timeout: 60_000 })).stdout.trim();
  const context = await browser.newContext({
    viewport: { width: 1440, height: 960 },
    deviceScaleFactor: Number(process.env.DOCBANK_SCREENSHOT_SCALE ?? 1),
    colorScheme: "dark",
  });
  const page = await context.newPage();
  try {
    await mkdir(output, { recursive: true });
    const source = path.join(workspace, "camera");
    await createSyntheticCameraFiles(source, 1);
    // A larger second import keeps running long enough to report a known total.
    const bulk = path.join(workspace, "camera-bulk");
    await createSyntheticCameraFiles(bulk, 1500, 2);
    const url = await run("web", "--no-browser");
    await run("jobs", "pause", "photo_import");
    await run("jobs", "pause", "place");
    const { id: completedID } = JSON.parse(
      await run("photos", "import", source, "/Photos/Trip", "--json"),
    ) as { id: string };
    const { id: bulkID } = JSON.parse(
      await run("photos", "import", bulk, "/Photos/Second trip", "--json"),
    ) as { id: string };
    type Listed = {
      items: {
        operation_id?: string;
        status: string;
        completed_objects?: number;
        total_objects?: number;
        error?: string;
      }[];
    };
    const bulkImport = async () =>
      (JSON.parse(await run("jobs", "--json")) as Listed).items.find(
        (job) => job.operation_id === bulkID,
      );
    const bulkOperation = page
      .locator(".operation")
      .filter({ has: page.getByText(bulkID, { exact: true }) });
    const completedImport = async () =>
      (JSON.parse(await run("jobs", "--json")) as Listed).items.find(
        (job) => job.operation_id === completedID,
      );
    await page.goto(url);
    await page.getByRole("button", { name: "System", exact: true }).click();
    await page.getByRole("tab", { name: "Running now", exact: true }).click();
    const drawer = page.getByRole("dialog", { name: "System" });
    const importing = page.getByRole("region", { name: "Importing photos" });
    const pausedImport = page.getByRole("table", { name: "All workers" }).getByRole("row", {
      name: /^Importing photos Paused/,
    });
    await expect(pausedImport).toBeVisible();
    await expect(page.getByRole("button", { name: "Resume Importing photos" })).toBeVisible();
    await expect(page.getByRole("button", { name: /^Cancel Importing photos/ })).toHaveCount(2);
    await expect(page.getByRole("button", { name: "Resume Moving stored files" })).toBeVisible();
    await page.getByRole("button", { name: "Resume Importing photos" }).click();
    await expect
      .poll(async () => (await bulkImport())?.completed_objects ?? 0, { timeout: 60_000 })
      .toBeGreaterThan(0);
    await expect(importing).toBeVisible();
    await expect(importing.getByText(/^\d+ of \d+ groups$/)).toBeVisible();
    await page.getByRole("button", { name: "Pause Importing photos" }).click();
    await expect(page.getByRole("button", { name: "Resume Importing photos" })).toBeVisible();
    await expect.poll(async () => (await bulkImport())?.status, { timeout: 30_000 }).toBe("queued");
    await expect(pausedImport).toBeVisible();
    await pausedImport.evaluate((element) =>
      element.scrollIntoView({ block: "start", behavior: "instant" }),
    );
    for (const theme of ["dark", "light"] as const) {
      await page.emulateMedia({ colorScheme: theme });
      await drawer.screenshot({
        path: path.join(output, `activity-after-${theme}.png`),
        animations: "disabled",
      });
    }
    await page.setViewportSize({ width: 390, height: 844 });
    await pausedImport.evaluate((element) =>
      element.scrollIntoView({ block: "start", behavior: "instant" }),
    );
    for (const theme of ["dark", "light"] as const) {
      await page.emulateMedia({ colorScheme: theme });
      await page.screenshot({
        path: path.join(output, `activity-after-narrow-${theme}.png`),
        animations: "disabled",
      });
    }
    // Previews carry their worker count beside the pause control.
    const previewWorkers = page.getByRole("combobox", { name: /^Workers for Making previews/ });
    await previewWorkers.first().scrollIntoViewIfNeeded();
    await expect(previewWorkers.first()).toBeInViewport();
    for (const theme of ["dark", "light"] as const) {
      await page.emulateMedia({ colorScheme: theme });
      await page.screenshot({
        path: path.join(output, `activity-after-narrow-previews-${theme}.png`),
        animations: "disabled",
      });
    }
    await page.setViewportSize({ width: 1440, height: 960 });
    await page.getByRole("button", { name: "Resume Moving stored files" }).click();
    await expect(page.getByRole("button", { name: "Pause Moving stored files" })).toBeVisible();
    await page.getByRole("button", { name: "Resume Importing photos" }).click();
    await expect(page.getByRole("button", { name: "Pause Importing photos" })).toBeVisible();
    await page.getByRole("button", { name: `Cancel Importing photos ${bulkID}` }).click();
    await expect
      .poll(async () => (await bulkImport())?.status, { timeout: 30_000 })
      .toBe("cancelled");
    await expect
      .poll(async () => (await completedImport())?.status, { timeout: 60_000 })
      .toBe("completed");
    // Finished work leaves the list, so a stopped import no longer shows up as running.
    await expect(bulkOperation).toHaveCount(0);
    const controlsPath = path.join(env.DOCBANK_HOME, "lane-controls.json");
    const settings = await readFile(controlsPath);
    try {
      await writeFile(controlsPath, "{");
      await page.getByRole("button", { name: "Refresh background jobs" }).click();
      await expect(page.getByRole("alert")).toBeVisible();
      await page.screenshot({
        path: path.join(output, "activity-controls-unavailable.png"),
        animations: "disabled",
      });
    } finally {
      await writeFile(controlsPath, settings);
    }
  } finally {
    await context.close();
    try {
      await run("daemon", "stop");
    } finally {
      await rm(workspace, { recursive: true, force: true });
    }
  }
});
