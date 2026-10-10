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
    await exec("go", ["run", "-tags", "fts5", "./frontend/screenshots/photos-fixture.go", vault, "750", "--rejects"], { cwd: repository, env, timeout: 240_000 });
    const webURL = new URL(await run("web", "--no-browser"));
    webURL.pathname = "/photos";
    await page.goto(webURL.href);
    const scroll = page.getByTestId("photo-scroll");
    for (const loaded of [250, 500, 749]) {
      await expect(page.getByText(`749 photos · ${loaded} loaded`, { exact: true })).toBeVisible();
      if (loaded < 749) await scroll.evaluate(element => element.scrollTop = element.scrollHeight);
    }
    const select = async (name: string) => {
      const checkbox = page.getByRole("checkbox", { name: `Select photo ${name}`, exact: true });
      const height = await scroll.evaluate(element => element.scrollHeight);
      const step = await scroll.evaluate(element => element.clientHeight);
      for (let top = 0; top <= height; top += step) {
        await scroll.evaluate((element, value) => element.scrollTop = value, top);
        await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
        if (await checkbox.count()) { await checkbox.check(); return; }
      }
      throw new Error(`Photo was not mounted: ${name}`);
    };
    await scroll.evaluate(element => element.scrollTop = 0);
    await page.getByRole("button", { name: "Move rejects", exact: true }).click();
    const modal = page.getByRole("dialog", { name: "Move rejects to trash" });
    await expect(modal.getByText("2 photos · 2 files including sidecars")).toBeVisible();
    await expect(modal.getByText("Mixed flags (1)", { exact: true })).toBeVisible();
    await expect(modal.getByText(/Synthetic-photo-00003.jpg: reject/)).toBeVisible();
    await expect(modal.getByText(/Synthetic-photo-00004.jpg: undecided \(in Trash\)/)).toBeVisible();
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
    await expect(page.getByRole("option", { name: "Selected photos (749)", exact: true })).toBeDisabled();
    await modal.getByRole("combobox", { name: "Rejects scope: Library", exact: true }).click();
    for (const theme of ["light", "dark"]) {
      await page.evaluate(value => { localStorage.setItem("docbank-theme", value); document.documentElement.classList.toggle("dark", value === "dark"); }, theme);
      await page.screenshot({ path: path.join(output!, `web-photo-rejects-selection-limit-${theme}.png`), animations: "disabled" });
    }
    await modal.getByRole("button", { name: "Keep in Docbank", exact: true }).click();
    await page.getByRole("button", { name: "Clear selection", exact: true }).click();
    await select("Synthetic-photo-00001.jpg");
    await page.getByRole("button", { name: "Move rejects", exact: true }).click();
    await expect(modal.getByText("2 photos · 2 files including sidecars")).toBeVisible();
    await modal.getByRole("combobox", { name: "Rejects scope: Library", exact: true }).click();
    await page.getByRole("option", { name: "Selected photos (1)", exact: true }).click();
    await expect(modal.getByText("1 photo · 1 file including sidecars")).toBeVisible();
    for (const theme of ["light", "dark"]) {
      await page.evaluate(value => { localStorage.setItem("docbank-theme", value); document.documentElement.classList.toggle("dark", value === "dark"); }, theme);
      await page.screenshot({ path: path.join(output!, `web-photo-rejects-selected-${theme}.png`), animations: "disabled" });
    }
    await modal.getByRole("button", { name: "Keep in Docbank", exact: true }).click();
    await page.route("**/api/v1/photos/assets/*/trash", route => route.fulfill({ status: 412, contentType: "application/problem+json", body: JSON.stringify({ detail: "Synthetic trash refusal", code: "stale_revision" }) }), { times: 1 });
    await page.getByRole("button", { name: "Move to trash", exact: true }).click();
    const ordinaryTrash = page.getByRole("dialog", { name: "Move selected photos to trash", exact: true });
    await ordinaryTrash.getByRole("button", { name: "Move to trash", exact: true }).click();
    await expect(ordinaryTrash.getByRole("alert")).toContainText("Synthetic trash refusal");
    await ordinaryTrash.getByRole("button", { name: "Keep in Docbank", exact: true }).click();
    await select("Synthetic-photo-00005.jpg");
    await select("Synthetic-photo-00003.jpg");
    const mounted = await scroll.elementHandle();
    const anchor = await scroll.evaluate(element => {
      const top = element.getBoundingClientRect().top;
      const cell = [...element.querySelectorAll<HTMLElement>("[data-asset]")].find(item => item.getBoundingClientRect().bottom > top + 56)!;
      return { id: cell.dataset.asset!, offset: cell.getBoundingClientRect().top - top, scroll: element.scrollTop };
    });
    expect(anchor.scroll).toBeGreaterThan(10_000);
    await page.getByRole("button", { name: "Move rejects", exact: true }).click();
    await modal.getByRole("combobox", { name: "Rejects scope: Library", exact: true }).click();
    await page.getByRole("option", { name: "Selected photos (3)", exact: true }).click();
    await expect(modal.getByText("1 photo · 1 file including sidecars")).toBeVisible();
    await expect(modal.getByText("Mixed flags (1)", { exact: true })).toBeVisible();
    await page.route("**/api/v1/photos/assets/query", route => route.fulfill({ status: 503, contentType: "application/problem+json", body: JSON.stringify({ detail: "Photo reload unavailable" }) }), { times: 1 });
    await modal.getByRole("button", { name: "Move 1 to trash", exact: true }).click();
    await expect(modal).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Retry", exact: true })).toBeVisible();
    await expect(scroll).toBeVisible();
    expect(await mounted!.evaluate(element => element.isConnected)).toBe(true);
    await expect(page.locator(".library-title span")).toBeVisible();
    await expect(page.getByRole("button", { name: "Move to trash", exact: true })).toBeEnabled();
    await expect(page.getByRole("button", { name: "Move rejects", exact: true })).toBeEnabled();
    for (const theme of ["light", "dark"]) {
      await page.evaluate(value => { localStorage.setItem("docbank-theme", value); document.documentElement.classList.toggle("dark", value === "dark"); }, theme);
      await page.screenshot({ path: path.join(output!, `web-photo-rejects-retry-${theme}.png`), animations: "disabled" });
    }
    await page.getByRole("button", { name: "Retry", exact: true }).click();
    await expect(page.getByText("748 photos · 748 loaded", { exact: true })).toBeVisible();
    await expect(page.getByText("2 selected photos", { exact: true })).toBeVisible();
    expect(await mounted!.evaluate(element => element.isConnected)).toBe(true);
    await expect.poll(() => scroll.locator(`[data-asset="${anchor.id}"]`).evaluate(element => element.getBoundingClientRect().top - element.closest(".photo-scroll")!.getBoundingClientRect().top)).toBeCloseTo(anchor.offset, 0);
    await expect(page.getByRole("button", { name: "Move rejects", exact: true })).toBeEnabled();
    for (const theme of ["light", "dark"]) {
      await page.evaluate(value => { localStorage.setItem("docbank-theme", value); document.documentElement.classList.toggle("dark", value === "dark"); }, theme);
      await page.screenshot({ path: path.join(output!, `web-photo-rejects-recovered-${theme}.png`), animations: "disabled" });
    }
    await page.getByRole("button", { name: "Recoverable trash", exact: true }).click();
    await expect(page.getByText("Synthetic-photo-00001.jpg", { exact: true })).toBeVisible();
    const trash = JSON.parse(await run("trash", "list", "--json")) as { items: { id: number; name: string }[] };
    for (const node of trash.items.filter(node => /^Synthetic-photo-0000[12]\.jpg$/.test(node.name))) await run("restore", String(node.id));
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
