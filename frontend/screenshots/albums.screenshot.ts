import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const binary = process.env.DOCBANK_SCREENSHOT_BINARY ?? path.join(repository, "bin", process.platform === "win32" ? "docbank.exe" : "docbank");
const output = process.env.DOCBANK_ALBUMS_SCREENSHOT_DIR;
test.skip(!output, "DOCBANK_ALBUMS_SCREENSHOT_DIR enables synthetic album proof");

test("organizes a 10,000-photo query through the dock, B and sidebar drag", async ({ page }) => {
  test.setTimeout(900_000);
  await mkdir(path.join(repository, ".superpowers"), { recursive: true });
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "albums-proof-"));
  const vault = path.join(workspace, "vault");
  const env = { ...process.env, DOCBANK_HOME: vault, DOCBANK_LOCK_DIR: path.join(workspace, "locks"), DOCBANK_TELEMETRY_ENABLED: "0" };
  const run = async (...args: string[]) => (await exec(binary, args, { cwd: repository, env, timeout: 60_000 })).stdout.trim();
  const adds: unknown[] = [];
  let albumID = "";
  page.on("request", request => { if (request.url().endsWith("/members/add")) adds.push(request.postDataJSON()); });
  try {
    await mkdir(output!, { recursive: true });
    await exec("go", ["run", "-tags", "fts5", "./frontend/screenshots/photos-fixture.go", vault], { cwd: repository, env, timeout: 480_000 });
    const webURL = new URL(await run("web", "--no-browser")); webURL.pathname = "/photos";
    await page.goto(webURL.href);
    await expect(page.getByText("10,000 photos · 250 loaded", { exact: true })).toBeVisible();
    await expect(page.locator(".photo-cell img").first()).toBeVisible();
    await page.locator(".photo-image").nth(0).click();
    await page.locator(".photo-image").nth(1).click({ modifiers: ["Control"] });
    await page.locator(".photo-image").nth(1).click({ modifiers: ["Control"] });
    await page.getByRole("button", { name: "Refresh previews", exact: true }).click();
    await expect(page.locator(".photo-loading")).not.toContainText("Loading");
    await page.locator(".photo-image").nth(2).click({ modifiers: ["Shift"] });
    await expect(page.getByText("3 selected photos", { exact: true })).toBeVisible();
    for (const theme of ["dark", "light"]) {
      await page.evaluate(theme => document.documentElement.classList.toggle("dark", theme === "dark"), theme);
      await page.screenshot({ path: path.join(output!, `web-albums-refresh-range-${theme}.png`), animations: "disabled" });
    }
    await page.getByRole("button", { name: "Clear selection", exact: true }).click();
    await page.locator(".photo-image").first().click();
    await page.getByRole("button", { name: "Select loaded photos" }).click();
    await page.getByRole("button", { name: "Select all 10,000 photos" }).click();
    await page.getByRole("button", { name: /Add to album/ }).click();
    await page.getByRole("combobox", { name: "Find or create an album" }).fill("Trip");
    await page.getByRole("option", { name: 'Create album "Trip"' }).click();
    await expect(page.getByText("Added to Trip · now 10,000 photos")).toBeVisible();
    expect(adds).toHaveLength(1);
    expect(adds[0]).toHaveProperty("query"); expect(adds[0]).not.toHaveProperty("asset_ids");
    await expect(page.getByText("10,000 photos · 250 loaded", { exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Clear selection", exact: true }).click();
    await page.locator(".photo-image").first().click();
    await page.locator(".library-title h1").click();
    await page.keyboard.press("b");
    await expect.poll(() => adds.length).toBe(2);
    expect(adds[1]).toHaveProperty("asset_ids");
    await expect(page.getByText("Added to Trip · now 10,000 photos")).toBeVisible();
    const nav = page.getByRole("navigation", { name: "Docbank navigation" });
    await page.locator(".photo-image").first().dragTo(nav.getByRole("button", { name: /Trip/ }));
    await expect.poll(() => adds.length).toBe(3);
    await nav.getByRole("button", { name: "Albums", exact: true }).click();
    await expect(page.getByRole("main", { name: "Albums" }).getByText("10,000 photos")).toBeVisible();
    const card = page.locator(".album-card a").first();
    albumID = (await card.getAttribute("href"))!.split("/").at(-1)!;
    await card.click();
    await expect(page.getByRole("main", { name: "Photo album" })).toBeVisible();
    await expect(page.getByText("10,000 photos · 250 loaded", { exact: true })).toBeVisible();
    await page.getByRole("combobox", { name: /Sort photos/ }).click();
    await page.getByRole("option", { name: "Imported", exact: true }).click();
    await expect(page.locator(".photo-cell img").first()).toBeVisible();
    await page.locator(".photo-image").first().click();
    await page.getByTestId("photo-scroll").evaluate(element => element.scrollTop = 500);
    await nav.getByRole("button", { name: "Documents", exact: true }).click();
    await nav.getByRole("button", { name: "Photos", exact: true }).click();
    expect(new URL(page.url()).pathname).toBe(`/photos/albums/${albumID}`);
    await expect(page.getByRole("combobox", { name: "Sort photos: Imported" })).toBeVisible();
    await expect(page.getByText("1 selected photo", { exact: true })).toBeVisible();
    await expect.poll(() => page.getByTestId("photo-scroll").evaluate(element => element.scrollTop)).toBe(500);
    await page.getByRole("button", { name: "Refresh previews", exact: true }).click();
    await expect(page.locator(".photo-loading")).not.toContainText("Loading");
    await page.getByRole("button", { name: "Clear selection", exact: true }).click();
    await page.reload();
    await expect(page.getByText("Open your Docbank", { exact: true })).toBeVisible();
    const reloaded = new URL(await run("web", "--no-browser")); reloaded.pathname = `/photos/albums/${albumID}`;
    await page.goto("about:blank");
    await page.goto(reloaded.href);
    await expect(page.getByRole("heading", { name: "Trip", exact: true })).toBeVisible();
    await expect(page.locator(".photo-cell img").first()).toBeVisible();
    for (const width of [1440, 400]) {
      await page.setViewportSize({ width, height: 960 });
      await page.evaluate(() => { localStorage.setItem("docbank-theme", "dark"); document.documentElement.classList.add("dark"); });
      await page.waitForLoadState("networkidle");
      await page.screenshot({ path: path.join(output!, `web-albums-detail-${width}-dark.png`), animations: "disabled" });
      await page.locator(".photo-image").first().click();
      await page.getByRole("button", { name: /Add to album/ }).click();
      await expect(page.getByRole("listbox")).toBeVisible();
      await page.screenshot({ path: path.join(output!, `web-albums-picker-${width}-dark.png`), animations: "disabled" });
      await page.keyboard.press("Escape");
      await page.getByRole("button", { name: "Clear selection", exact: true }).click();
      await page.locator(".album-back").click();
      await expect(page.locator(".album-cover img").first()).toBeVisible();
      await page.waitForLoadState("networkidle");
      await page.screenshot({ path: path.join(output!, `web-albums-index-${width}-dark.png`), animations: "disabled" });
      await page.locator(".album-card a").first().click();
      await expect(page.locator(".photo-cell img").first()).toBeVisible();
    }
    await page.goBack();
    await expect(page.getByRole("main", { name: "Albums" })).toBeVisible();
    const url = new URL(await run("web", "--no-browser")); url.pathname = `/photos/albums/${albumID}`;
    await page.goto("about:blank");
    await page.goto(url.href);
    await expect(page.getByRole("heading", { name: "Trip", exact: true })).toBeVisible();
    await page.setViewportSize({ width: 1440, height: 960 });
    await expect(page.locator(".photo-cell img").first()).toBeVisible();
    await page.locator(".photo-image").first().click();
    await page.getByRole("button", { name: /Add to album/ }).click();
    await page.getByRole("combobox", { name: "Find or create an album" }).fill(albumID);
    const createUUID = page.getByRole("option", { name: `Create album "${albumID}"` });
    await expect(createUUID).toBeVisible();
    for (const theme of ["dark", "light"]) {
      await page.evaluate(theme => document.documentElement.classList.toggle("dark", theme === "dark"), theme);
      await page.screenshot({ path: path.join(output!, `web-albums-uuid-name-${theme}.png`), animations: "disabled" });
    }
    await createUUID.click();
    await expect(page.getByText(`Added to ${albumID} · now 1 photo`, { exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Clear selection", exact: true }).click();
    await writeFile(path.join(output!, "albums-preview.json"), JSON.stringify({ url: url.href, workspace, adds, albumID }, null, 2));
  } finally {
    if (!process.env.DOCBANK_KEEP_ALBUM_PREVIEW) { await run("daemon", "stop"); await rm(workspace, { recursive: true, force: true }); }
  }
});
