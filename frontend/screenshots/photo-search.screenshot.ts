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
test.skip(!output, "DOCBANK_PHOTOS_SCREENSHOT_DIR enables synthetic photo proof");

test("photo search, whole-library facets and ordered continuation use the real vault", async ({ browser }) => {
  test.setTimeout(900_000);
  const workspace = process.env.DOCBANK_PHOTO_PROOF_WORKSPACE ?? await mkdtemp(path.join(repository, ".superpowers", "photo-search-proof-"));
  const vault = path.join(workspace, "vault");
  const env = { ...process.env, DOCBANK_HOME: vault, DOCBANK_LOCK_DIR: path.join(workspace, "locks"), DOCBANK_TELEMETRY_ENABLED: "0" };
  const run = async (...args: string[]) => (await exec(binary, args, { cwd: repository, env, timeout: 60_000 })).stdout.trim();
  const context = await browser.newContext({ viewport: { width: 1440, height: 960 }, deviceScaleFactor: Number(process.env.DOCBANK_SCREENSHOT_SCALE ?? 1) });
  const page = await context.newPage();
  page.setDefaultTimeout(15_000);
  try {
    await mkdir(output!, { recursive: true });
    if (!process.env.DOCBANK_PHOTO_PROOF_WORKSPACE) await exec("go", ["run", "-tags", "fts5", "./frontend/screenshots/photos-fixture.go", vault], { cwd: repository, env, timeout: 480_000 });
    const webURL = new URL(await run("web", "--no-browser"));
    webURL.pathname = "/photos";
    let releaseCounts!: () => void;
    const countsGate = new Promise<void>(resolve => releaseCounts = resolve);
    let deferCounts = true;
    await page.route("**/photos/assets/query", async route => {
      if (route.request().postDataJSON().facets.length && deferCounts) await countsGate;
      await route.continue();
    });
    await page.goto(webURL.href);
    await expect(page.getByText(/10,000 photos/)).toBeVisible();
    await expect(page.locator("[data-asset] img").first()).toBeVisible();
    await expect(page.getByText("Loading counts…")).toBeVisible();
    for (const theme of ["light", "dark"]) {
      await page.evaluate(value => { localStorage.setItem("docbank-theme", value); document.documentElement.classList.toggle("dark", value === "dark"); }, theme);
      await page.screenshot({ path: path.join(output!, `photo-rows-before-counts-${theme}.png`), clip: { x: 0, y: 0, width: 1440, height: 720 }, animations: "disabled" });
    }
    deferCounts = false; releaseCounts();
    const camera = page.getByRole("group", { name: "Camera facet" });
    await expect(camera.getByRole("button", { name: "Canon EOS R6, 5000 photos" })).toBeVisible();
    await camera.getByRole("button", { name: "Canon EOS R6, 5000 photos" }).click();
    await expect(page.getByText(/5,000 photos/)).toBeVisible();
    await expect(camera.getByRole("button", { name: "Nikon Z6, 5000 photos" })).toBeVisible();
    await page.getByRole("button", { name: "Clear filters", exact: true }).click();
    await expect(page.getByText(/10,000 photos/)).toBeVisible();
    await page.getByRole("searchbox", { name: "Search photos" }).fill("Canon");
    const searchPage = page.waitForResponse(response => response.url().includes("/photos/assets/query") && response.request().postDataJSON().query.text === "Canon" && response.request().postDataJSON().facets.length === 0);
    await page.getByRole("button", { name: "Search", exact: true }).click();
    const first = await (await searchPage).json();
    await expect(page.getByText(/5,000 photos/)).toBeVisible();
    await expect(page.getByRole("heading", { name: "Search results" })).toBeVisible();
    await expect(page.getByRole("navigation", { name: "Photo years" })).toHaveCount(0);
    await expect(page.getByRole("group", { name: "Albums facet" }).getByRole("button", { name: "Paris walks, 5000 photos" })).toBeVisible();
    const continuation = page.waitForResponse(response => response.url().includes("/photos/assets/query") && Boolean(response.request().postDataJSON().cursor));
    await page.getByRole("button", { name: "Load more", exact: true }).click();
    const next = await (await continuation).json();
    const ids = [...first.items, ...next.items].map(item => item.asset_id);
    expect(new Set(ids).size).toBe(ids.length);
    const order = [...first.items, ...next.items].map(item => item.capture_time ?? "");
    expect(order).toEqual([...order].sort().reverse());
    await expect(page.getByRole("group", { name: "Lens facet" }).getByRole("button", { name: "RF 24-70mm F2.8, 5000 photos" })).toBeVisible();
    await page.waitForLoadState("networkidle");
    for (const theme of ["light", "dark"]) {
      await page.evaluate(value => { localStorage.setItem("docbank-theme", value); document.documentElement.classList.toggle("dark", value === "dark"); }, theme);
      await page.screenshot({ path: path.join(output!, `photo-search-after-${theme}.png`), clip: { x: 0, y: 0, width: 1440, height: 720 }, animations: "disabled" });
      await page.getByRole("group", { name: "Albums facet" }).scrollIntoViewIfNeeded();
      await expect(page.getByRole("group", { name: "Location facet" })).toBeInViewport();
      await expect(page.getByRole("group", { name: "Albums facet" })).toBeInViewport();
      await page.screenshot({ path: path.join(output!, `photo-facets-after-${theme}.png`), clip: { x: 0, y: 0, width: 1440, height: 960 }, animations: "disabled" });
      await page.locator(".photo-facets").evaluate(element => element.scrollTop = 0);
    }
    await page.getByRole("button", { name: "Clear filters", exact: true }).click();
    await page.getByRole("searchbox", { name: "Search photos" }).fill("Paris");
    await page.getByRole("button", { name: "Search", exact: true }).click();
    await expect(page.getByText(/10,000 photos/)).toBeVisible();
    await expect(page.getByRole("group", { name: "Location facet" }).getByRole("button").first()).toContainText("Paris");
  } finally {
    await context.close();
    if (process.env.DOCBANK_KEEP_PHOTO_PREVIEW) {
      const url = new URL(await run("web", "--no-browser")); url.pathname = "/photos";
      await writeFile(path.join(output!, "photo-search-preview.json"), JSON.stringify({ url: url.href, workspace }, null, 2));
    } else if (!process.env.DOCBANK_PHOTO_PROOF_WORKSPACE) { await run("daemon", "stop"); await rm(workspace, { recursive: true, force: true }); }
  }
});
