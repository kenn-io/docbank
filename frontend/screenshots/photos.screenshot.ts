import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const binary = process.env.DOCBANK_SCREENSHOT_BINARY ?? path.join(repository, "bin", process.platform === "win32" ? "docbank.exe" : "docbank");
const output = process.env.DOCBANK_SCREENSHOT_DIR;
test.skip(!output, "DOCBANK_SCREENSHOT_DIR enables synthetic photo proof");

test("10,000 photos stay windowed, retain previews and selection, and remember density", async ({ page }) => {
  test.setTimeout(900_000);
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "photos-proof-"));
  const vault = path.join(workspace, "vault");
  const env = { ...process.env, DOCBANK_HOME: vault, DOCBANK_LOCK_DIR: path.join(workspace, "locks"), DOCBANK_TELEMETRY_ENABLED: "0" };
  const run = async (...args: string[]) => (await exec(binary, args, { cwd: repository, env, timeout: 60_000 })).stdout.trim();
  const requests = new Map<string, number>();
  page.on("request", request => {
    if (request.url().includes("/previews/")) requests.set(request.url(), (requests.get(request.url()) ?? 0) + 1);
  });
  try {
    await mkdir(output!, { recursive: true });
    await exec("go", ["run", "-tags", "fts5", "./frontend/screenshots/photos-fixture.go", vault], { cwd: repository, env, timeout: 480_000 });
    const webURL = new URL(await run("web", "--no-browser"));
    webURL.pathname = "/photos";
    await page.goto(webURL.href);
    await expect(page.getByRole("main", { name: "Photo library" })).toBeVisible();
    await expect(page.getByText(/10,000 photos/)).toBeVisible();
    const scroll = page.getByTestId("photo-scroll");
    await expect(scroll.locator("img").first()).toBeVisible();
    const firstAsset = await page.locator("[data-asset]").first().getAttribute("data-asset");
    await page.locator("[data-asset]").first().getByRole("button").first().click();
    for (let count = 0; count < 50; count++) {
      if (await page.getByText("10,000 photos · 10,000 loaded").isVisible()) break;
      const loaded = await page.locator(".library-title span").innerText();
      await scroll.evaluate(element => element.scrollTop = element.scrollHeight);
      await expect.poll(() => page.locator(".library-title span").innerText()).not.toBe(loaded);
      expect(await page.locator("[data-asset]").count()).toBeLessThan(200);
    }
    await expect(page.getByText("10,000 photos · 10,000 loaded")).toBeVisible();
    await expect(page.getByRole("navigation", { name: "Photo years" }).getByRole("button", { name: "2022", exact: true })).toBeVisible();
    const last = page.locator("[data-asset]").last().getByRole("button").first();
    await last.click({ modifiers: ["Shift"] });
    await expect.poll(async () => Number((await page.locator(".selection-summary strong").innerText()).split(" ")[0])).toBeGreaterThan(9000);
    await expect(page.getByRole("button", { name: "Clear selection", exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Clear selection", exact: true }).click();
    await page.getByRole("navigation", { name: "Photo years" }).getByRole("button", { name: "2024", exact: true }).click();
    await expect(page.locator(".sticky-month")).toContainText("2024");
    await scroll.evaluate(element => element.scrollTop = 0);
    await expect(page.locator(`[data-asset="${firstAsset}"] img`)).toBeVisible();
    await page.waitForLoadState("networkidle");
    const seen = new Map(requests);
    await scroll.evaluate(element => element.scrollTop = 15_000);
    await page.waitForLoadState("networkidle");
    await scroll.evaluate(element => element.scrollTop = 0);
    await expect(page.locator(`[data-asset="${firstAsset}"] img`)).toBeVisible();
    await page.waitForLoadState("networkidle");
    for (const [url, count] of seen) expect(requests.get(url)).toBe(count);
    await page.getByRole("combobox", { name: /^Grid density/ }).click();
    await page.getByRole("option", { name: "Compact", exact: true }).click();
    const fresh = new URL(await run("web", "--no-browser"));
    fresh.pathname = "/photos";
    await page.goto("about:blank");
    await page.goto(fresh.href);
    await expect(page.getByRole("combobox", { name: /^Grid density/ })).toContainText("Compact");
    await expect(scroll.locator("img").first()).toBeVisible();
    for (const width of [1440, 400]) {
      await page.setViewportSize({ width, height: 960 });
      for (const theme of ["light", "dark"]) {
        await page.evaluate(value => { localStorage.setItem("docbank-theme", value); document.documentElement.classList.toggle("dark", value === "dark"); }, theme);
        await page.screenshot({ path: path.join(output!, `web-photos-${width}-${theme}.png`), animations: "disabled", timeout: 15_000 });
      }
    }
  } finally {
    if (process.env.DOCBANK_KEEP_PHOTO_PREVIEW) {
      const previewURL = new URL(await run("web", "--no-browser"));
      previewURL.pathname = "/photos";
      await writeFile(path.join(output!, "photos-preview.json"), JSON.stringify({ url: previewURL.href, workspace }, null, 2));
    } else {
      await run("daemon", "stop");
      await rm(workspace, { recursive: true, force: true });
    }
  }
});
