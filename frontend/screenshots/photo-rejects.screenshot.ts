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
    for (const fixture of ["photos-fixture.go", "photo-rejects-fixture.go"]) {
      await exec("go", ["run", "-tags", "fts5", `./frontend/screenshots/${fixture}`, vault, ...(fixture === "photos-fixture.go" ? ["12"] : [])], { cwd: repository, env, timeout: 240_000 });
    }
    const webURL = new URL(await run("web", "--no-browser"));
    webURL.pathname = "/photos";
    await page.goto(webURL.href);
    await expect(page.locator("[data-asset]")).toHaveCount(11);
    await page.getByRole("button", { name: "Move rejects", exact: true }).click();
    const modal = page.getByRole("dialog", { name: "Move rejects to trash" });
    await expect(modal.getByText("2 photos · 2 files including sidecars")).toBeVisible();
    await expect(modal.getByText("Mixed flags", { exact: true })).toBeVisible();
    await expect(modal.getByText(/Synthetic-photo-00003.jpg: reject/)).toBeVisible();
    await expect(modal.getByText(/Synthetic-photo-00004.jpg: undecided/)).toBeVisible();
    for (const theme of ["light", "dark"]) {
      await page.evaluate(value => { localStorage.setItem("docbank-theme", value); document.documentElement.classList.toggle("dark", value === "dark"); }, theme);
      await page.screenshot({ path: path.join(output!, `web-photo-rejects-${theme}.png`), animations: "disabled" });
    }
    await modal.getByRole("button", { name: "Move 2 to trash", exact: true }).click();
    await expect(modal).toHaveCount(0);
    await expect(page.locator("[data-asset]")).toHaveCount(9);
    await page.getByRole("button", { name: "Recoverable trash", exact: true }).click();
    await expect(page.getByText("Synthetic-photo-00001.jpg", { exact: true })).toBeVisible();
    await expect(page.getByText("Synthetic-photo-00002.jpg", { exact: true })).toBeVisible();
    const trash = JSON.parse(await run("trash", "list", "--json")) as { items: { id: number }[] };
    for (const node of trash.items) await run("restore", String(node.id));
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
