import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const binary = process.env.DOCBANK_SCREENSHOT_BINARY ?? path.join(repository, "bin", process.platform === "win32" ? "docbank.exe" : "docbank");
const output = process.env.DOCBANK_PHOTOS_SCREENSHOT_DIR;
test.use({ viewport: { width: 1440, height: 720 }, deviceScaleFactor: Number(process.env.DOCBANK_SCREENSHOT_SCALE ?? 1) });
test.skip(!output, "DOCBANK_PHOTOS_SCREENSHOT_DIR enables synthetic photo proof");

test("Move rejects previews mixed flags and refreshes Photos and Trash", async ({ page }) => {
  test.setTimeout(480_000);
  page.setDefaultTimeout(15_000);
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "rejects-proof-"));
  const vault = path.join(workspace, "vault");
  const env = { ...process.env, DOCBANK_HOME: vault, DOCBANK_LOCK_DIR: path.join(workspace, "locks"), DOCBANK_TELEMETRY_ENABLED: "0" };
  const run = async (...args: string[]) => (await exec(binary, args, { cwd: repository, env, timeout: 60_000 })).stdout.trim();
  try {
    await mkdir(output!, { recursive: true });
    await exec("go", ["run", "-tags", "fts5", "./frontend/screenshots/photos-fixture.go", vault, "70", "--rejects"], { cwd: repository, env, timeout: 240_000 });
    const webURL = new URL(await run("web", "--no-browser"));
    webURL.pathname = "/photos";
    await page.goto(webURL.href);
    await expect(page.getByText("69 photos · 69 loaded", { exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Move rejects", exact: true }).click();
    const modal = page.getByRole("dialog", { name: "Move rejects to trash" });
    await expect(modal.getByText("2 photos · 2 files including sidecars")).toBeVisible();
    await expect(modal.getByText("Mixed flags (1)", { exact: true })).toBeVisible();
    await expect(modal.getByText(/Synthetic-photo-00003.jpg: reject/)).toBeVisible();
    await expect(modal.getByText(/Synthetic-photo-00004.jpg: undecided/)).toBeVisible();
    for (const theme of ["light", "dark"]) {
      await page.evaluate(value => { localStorage.setItem("docbank-theme", value); document.documentElement.classList.toggle("dark", value === "dark"); }, theme);
      await page.screenshot({ path: path.join(output!, `web-photo-rejects-${theme}.png`), animations: "disabled" });
    }
    await modal.getByRole("button", { name: "Keep in Docbank", exact: true }).click();
    await page.getByRole("checkbox", { name: /^Select photo / }).first().check();
    await page.getByRole("button", { name: "Select loaded photos", exact: true }).click();
    await page.getByRole("button", { name: "Move rejects", exact: true }).click();
    await expect(modal.getByText("2 photos · 2 files including sidecars")).toBeVisible();
    await expect(modal.getByText("Close this dialog and select up to 64 photos.", { exact: true })).toBeVisible();
    await modal.getByRole("combobox", { name: "Rejects scope: Library", exact: true }).click();
    await expect(page.getByRole("option", { name: "Selected photos (69)", exact: true })).toBeDisabled();
    await modal.getByRole("combobox", { name: "Rejects scope: Library", exact: true }).click();
    for (const theme of ["light", "dark"]) {
      await page.evaluate(value => { localStorage.setItem("docbank-theme", value); document.documentElement.classList.toggle("dark", value === "dark"); }, theme);
      await page.screenshot({ path: path.join(output!, `web-photo-rejects-selection-limit-${theme}.png`), animations: "disabled" });
    }
    await modal.getByRole("button", { name: "Keep in Docbank", exact: true }).click();
    await page.getByRole("button", { name: "Clear selection", exact: true }).click();
    await page.getByTestId("photo-scroll").evaluate(element => { element.scrollTop = element.scrollHeight; });
    await page.getByRole("checkbox", { name: "Select photo Synthetic-photo-00001.jpg", exact: true }).check();
    await page.getByRole("button", { name: "Move rejects", exact: true }).click();
    await expect(modal.getByText("2 photos · 2 files including sidecars")).toBeVisible();
    await modal.getByRole("combobox", { name: "Rejects scope: Library", exact: true }).click();
    await page.getByRole("option", { name: "Selected photos (1)", exact: true }).click();
    await expect(modal.getByText("1 photo · 1 file including sidecars")).toBeVisible();
    for (const theme of ["light", "dark"]) {
      await page.evaluate(value => { localStorage.setItem("docbank-theme", value); document.documentElement.classList.toggle("dark", value === "dark"); }, theme);
      await page.screenshot({ path: path.join(output!, `web-photo-rejects-selected-${theme}.png`), animations: "disabled" });
    }
    await modal.getByRole("combobox", { name: "Rejects scope: Selected photos (1)", exact: true }).click();
    await page.getByRole("option", { name: "Library", exact: true }).click();
    await expect(modal.getByText("2 photos · 2 files including sidecars")).toBeVisible();
    await modal.getByRole("button", { name: "Move 2 to trash", exact: true }).click();
    await expect(modal).toHaveCount(0);
    await expect(page.getByText("67 photos · 67 loaded", { exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Recoverable trash", exact: true }).click();
    await expect(page.getByText("Synthetic-photo-00001.jpg", { exact: true })).toBeVisible();
    await expect(page.getByText("Synthetic-photo-00002.jpg", { exact: true })).toBeVisible();
    const trash = JSON.parse(await run("trash", "list", "--json")) as { items: { id: number }[] };
    for (const node of trash.items) await run("restore", String(node.id));
    webURL.pathname = "/photos/hidden";
    await page.goto(webURL.href);
    await page.getByLabel("Passcode", { exact: true }).fill("synthetic-passcode");
    await page.getByRole("button", { name: "Set passcode", exact: true }).click();
    await expect(page.getByText(/Locks in/)).toBeVisible();
    await page.getByRole("button", { name: "Move rejects", exact: true }).click();
    await expect(modal).toBeVisible();
    await page.clock.install();
    await page.clock.fastForward(301_000);
    await expect(page.getByRole("button", { name: "Unlock", exact: true })).toBeVisible();
    await expect(modal).toHaveCount(0);
  } finally {
    if (process.env.DOCBANK_KEEP_PHOTO_PREVIEW) {
      const url = new URL(await run("web", "--no-browser"));
      url.pathname = "/photos";
      await writeFile(path.join(output!, "rejects-preview.json"), JSON.stringify({ url: url.href, workspace }, null, 2));
    } else {
      await run("daemon", "stop");
      await rm(workspace, { recursive: true, force: true });
    }
  }
});
