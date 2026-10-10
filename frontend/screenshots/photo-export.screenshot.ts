import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const binary = process.env.DOCBANK_SCREENSHOT_BINARY ?? path.join(repository, "bin", process.platform === "win32" ? "docbank.exe" : "docbank");
const output = process.env.DOCBANK_PHOTO_EXPORT_SCREENSHOT_DIR;
test.skip(!output, "DOCBANK_PHOTO_EXPORT_SCREENSHOT_DIR enables synthetic photo export proof");
test.use({ deviceScaleFactor: Number(process.env.DOCBANK_SCREENSHOT_SCALE ?? "1") });

test("selected JPEG and complete-scope PNG export through verified downloads", async ({ page }) => {
  test.setTimeout(480_000);
  page.setDefaultTimeout(15_000);
  await page.setViewportSize({ width: 1440, height: 1000 });
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "photo-export-proof-"));
  const vault = path.join(workspace, "vault");
  const env = { ...process.env, DOCBANK_HOME: vault, DOCBANK_LOCK_DIR: path.join(workspace, "locks"), DOCBANK_TELEMETRY_ENABLED: "0" };
  const run = async (...args: string[]) => (await exec(binary, args, { cwd: repository, env, timeout: 60_000 })).stdout.trim();
  try {
    await mkdir(output!, { recursive: true });
    const count = Number(process.env.DOCBANK_PHOTO_EXPORT_COUNT ?? "12");
    await exec("go", ["run", "-tags", "fts5", "./frontend/screenshots/photos-fixture.go", vault, String(count)], { cwd: repository, env, timeout: 240_000 });
    const webURL = new URL(await run("web", "--no-browser"));
    webURL.pathname = "/photos";
    await page.goto(webURL.href);
    await page.getByRole("button", { name: "Documents", exact: true }).click();
    await page.getByRole("button", { name: "Export", exact: true }).click();
    await page.getByRole("combobox", { name: /^ZIP packaging/ }).click();
    await page.getByRole("option", { name: "One output / 512 MiB per volume", exact: true }).click();
    await page.getByRole("combobox", { name: /^Duplicate outputs/ }).click();
    await page.getByRole("option", { name: "Share exact duplicate outputs", exact: true }).click();
    await page.getByRole("button", { name: "Close export", exact: true }).click();
    await page.getByRole("button", { name: "Photos", exact: true }).click();
    await expect(page.locator("[data-asset]")).toHaveCount(count);
    const capture = async (state: string) => {
      for (const theme of ["light", "dark"]) {
        await page.evaluate(value => { localStorage.setItem("docbank-theme", value); document.documentElement.classList.toggle("dark", value === "dark"); }, theme);
        await page.screenshot({ path: path.join(output!, `photo-export-${state}-${theme}.png`), animations: "disabled" });
      }
    };
    if (count > 16) {
      await expect(page.getByRole("button", { name: "Export photos", exact: true })).toBeDisabled();
      await expect(page.getByText("Select up to 16 photos to export.", { exact: true })).toBeVisible();
      await capture("scope-limit");
      await page.locator("[data-asset]").first().getByRole("button", { name: /^Select / }).click();
      await page.getByRole("button", { name: "Select loaded photos", exact: true }).click();
      await expect(page.getByRole("button", { name: "Export selection", exact: true })).toBeDisabled();
      await capture("selection-limit");
      await page.getByRole("button", { name: "Clear selection", exact: true }).click();
    }
    await page.locator("[data-asset]").first().getByRole("button", { name: /^Select / }).click();
    await page.locator("[data-asset]").nth(1).getByRole("button", { name: /^Select / }).click({ modifiers: ["ControlOrMeta"] });
    await page.getByRole("button", { name: "Export selection", exact: true }).click();
    await expect(page.getByLabel("JPEG quality", { exact: true })).toHaveValue("90");
    await expect(page.getByRole("checkbox", { name: "Include metadata", exact: true })).toBeChecked();
    await expect(page.getByRole("checkbox", { name: "Remove GPS", exact: true })).toBeChecked();
    await page.getByLabel("Long edge, pixels", { exact: true }).fill("256");
    await page.getByLabel("Downloaded ZIP filename", { exact: true }).fill("synthetic-selected.zip");
    await capture("settings");
    const preparation = page.waitForRequest(request => request.url().endsWith("/exports/plans") && request.method() === "POST");
    await page.getByRole("button", { name: "Prepare", exact: true }).click();
    const submitted = (await preparation).postDataJSON();
    expect(submitted.volume_limits).toBeUndefined();
    expect(submitted.duplicate_policy).toBeUndefined();
    await expect(page.getByTestId("export-total")).toHaveText("2");
    await expect(page.getByRole("button", { name: "Start reviewed export", exact: true })).toBeEnabled();
    await capture("review");
    const download = async (filename: string) => {
      await page.getByRole("button", { name: "Start reviewed export", exact: true }).click();
      await expect(page.getByText("Archive verified and ready", { exact: true })).toBeVisible();
      const pending = page.waitForEvent("download");
      await page.getByRole("button", { name: "Download verified ZIP", exact: true }).click();
      const archive = await pending;
      expect(archive.suggestedFilename()).toBe(filename);
      const saved = path.join(workspace, filename);
      await archive.saveAs(saved);
      expect((await readFile(saved)).subarray(0, 2).toString()).toBe("PK");
    };
    await download("synthetic-selected.zip");
    await page.getByRole("button", { name: "Prepare another export", exact: true }).click();
    await page.getByRole("button", { name: "Close export", exact: true }).click();
    await page.getByRole("button", { name: "Clear selection", exact: true }).click();
    if (count > 16) return;
    await page.getByRole("button", { name: "Export photos", exact: true }).click();
    await page.getByRole("combobox", { name: /^Photo format/ }).click();
    await page.getByRole("option", { name: "PNG", exact: true }).click();
    await page.getByRole("checkbox", { name: "Include metadata", exact: true }).uncheck();
    await page.getByLabel("Downloaded ZIP filename", { exact: true }).fill("synthetic-scope.zip");
    await page.getByRole("button", { name: "Prepare", exact: true }).click();
    await expect(page.getByTestId("export-total")).toHaveText(String(count));
    await capture("scope-review");
    await download("synthetic-scope.zip");
  } finally {
    if (process.env.DOCBANK_KEEP_PHOTO_EXPORT_PREVIEW) {
      const url = new URL(await run("web", "--no-browser"));
      url.pathname = "/photos";
      await writeFile(path.join(output!, "photo-export-preview.json"), JSON.stringify({ url: url.href, workspace }, null, 2));
    } else {
      await run("daemon", "stop");
      await rm(workspace, { recursive: true, force: true });
    }
  }
});
