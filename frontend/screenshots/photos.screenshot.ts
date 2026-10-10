import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";
import { listPhotoAssets } from "../src/generated/docbank.js";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const binary = process.env.DOCBANK_SCREENSHOT_BINARY ?? path.join(repository, "bin", process.platform === "win32" ? "docbank.exe" : "docbank");
const output = process.env.DOCBANK_PHOTOS_SCREENSHOT_DIR;
test.skip(!output, "DOCBANK_PHOTOS_SCREENSHOT_DIR enables synthetic photo proof");

test("10,000 photos stay windowed, retain previews and selection, and remember density", async ({ page }) => {
  test.setTimeout(900_000);
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "photos-proof-"));
  const vault = path.join(workspace, "vault");
  const env = { ...process.env, DOCBANK_HOME: vault, DOCBANK_LOCK_DIR: path.join(workspace, "locks"), DOCBANK_TELEMETRY_ENABLED: "0" };
  const run = async (...args: string[]) => (await exec(binary, args, { cwd: repository, env, timeout: 60_000 })).stdout.trim();
  const requests = new Map<string, number>();
  let listings = 0;
  page.on("request", request => {
    if (request.url().includes("/photos/assets/query")) listings++;
    if (request.url().includes("/previews/")) requests.set(request.url(), (requests.get(request.url()) ?? 0) + 1);
  });
  try {
    await mkdir(output!, { recursive: true });
    await page.addInitScript(() => {
      const active = new Set<string>();
      Object.assign(window, { photoActiveURLs: active });
      const create = URL.createObjectURL.bind(URL);
      const revoke = URL.revokeObjectURL.bind(URL);
      URL.createObjectURL = blob => { const url = create(blob); active.add(url); return url; };
      URL.revokeObjectURL = url => { active.delete(url); revoke(url); };
    });
    await exec("go", ["run", "-tags", "fts5", "./frontend/screenshots/photos-fixture.go", vault], { cwd: repository, env, timeout: 480_000 });
    const webURL = new URL(await run("web", "--no-browser"));
    webURL.pathname = "/photos";
    let releaseTransition!: () => void;
    let enteredTransition!: () => void;
    const transitionPending = new Promise<void>(resolve => enteredTransition = resolve);
    const transitionHold = new Promise<void>(resolve => releaseTransition = resolve);
    await page.route("**/api/v1/photos/assets/query", async route => {
      const body = route.request().postDataJSON();
      body.page_size = 100;
      const url = new URL(route.request().url());
      const host = url.host;
      url.hostname = "127.0.0.1";
      const response = await route.fetch({ url: url.href, headers: { ...await route.request().allHeaders(), host }, postData: JSON.stringify(body) });
      if (body.cursor) { enteredTransition(); await transitionHold; }
      await route.fulfill({ response });
    });
    await page.goto(webURL.href);
    await expect(page.getByText("10,000 photos · 100 loaded", { exact: true })).toBeVisible();
    await expect(page.getByRole("navigation", { name: "Photo years" })).toHaveCount(0);
    await page.getByRole("button", { name: "Load more", exact: true }).click();
    await transitionPending;
    const transitionScroll = page.getByTestId("photo-scroll");
    await transitionScroll.evaluate(element => element.scrollTop = 500);
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    const transitionAnchor = await transitionScroll.evaluate(element => {
      const top = element.getBoundingClientRect().top;
      const cell = [...element.querySelectorAll<HTMLElement>("[data-asset]")].find(item => item.getBoundingClientRect().bottom > top + 56)!;
      return { id: cell.dataset.asset!, offset: cell.getBoundingClientRect().top - top };
    });
    releaseTransition();
    await expect(page.getByText("10,000 photos · 200 loaded", { exact: true })).toBeVisible();
    await expect(page.getByRole("navigation", { name: "Photo years" }).getByRole("button")).toHaveCount(2);
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    expect(await page.locator(`[data-asset="${transitionAnchor.id}"]`).evaluate(element => element.getBoundingClientRect().top - element.closest(".photo-scroll")!.getBoundingClientRect().top)).toBeCloseTo(transitionAnchor.offset, 0);
    await page.unroute("**/api/v1/photos/assets/query");
    await page.goto("about:blank");
    await page.goto(webURL.href);
    await expect(page.getByRole("main", { name: "Photo library" })).toBeVisible();
    await expect(page.getByText(/10,000 photos/)).toBeVisible();
    const scroll = page.getByTestId("photo-scroll");
    await expect(scroll.locator("img").first()).toBeVisible();
    await expect(page.getByRole("navigation", { name: "Photo years" }).getByRole("button")).toHaveCount(3);
    const firstAsset = await page.locator("[data-asset]").first().getAttribute("data-asset");
    await page.locator("[data-asset]").first().getByRole("button").first().click();
    await page.getByRole("combobox", { name: /^Group photos/ }).click();
    await page.getByRole("option", { name: "Capture sessions", exact: true }).click();
    const requestNext = async (anchor = false) => {
      const button = page.getByRole("button", { name: "Load more", exact: true });
      await button.focus();
      await page.keyboard.press("Enter");
      await scroll.evaluate((element, gap) => element.scrollTop = element.scrollHeight - element.clientHeight - gap, anchor ? 400 : 0);
    };
    for (let count = 0; count < 50; count++) {
      if (await page.getByText("10,000 photos · 10,000 loaded").isVisible()) break;
      const loaded = await page.locator(".library-title span").innerText();
      if (loaded.endsWith("· 1,750 loaded") || loaded.endsWith("· 2,750 loaded")) {
        let release!: () => void;
        let entered!: () => void;
        const held = new Promise<void>(resolve => entered = resolve);
        const resumed = new Promise<void>(resolve => release = resolve);
        await page.route("**/api/v1/photos/assets/query", async route => {
          const url = new URL(route.request().url());
          const host = url.host;
          url.hostname = "127.0.0.1";
          const response = await route.fetch({ url: url.href, headers: { ...await route.request().allHeaders(), host } });
          entered(); await resumed; await route.fulfill({ response });
        }, { times: 1 });
        if (loaded.includes("2,750 loaded")) {
          await page.route("**/api/v1/photos/assets/query", route => route.abort("failed"), { times: 1 });
          await requestNext(true);
          await expect(page.getByRole("button", { name: "Retry", exact: true })).toBeVisible();
          await page.getByRole("button", { name: "Retry", exact: true }).click();
        } else await requestNext(true);
        await held;
        await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
        const anchor = await scroll.evaluate(element => {
          const top = element.getBoundingClientRect().top;
          const cell = [...element.querySelectorAll<HTMLElement>("[data-asset]")].find(item => item.getBoundingClientRect().bottom > top + 56)!;
          return { id: cell.dataset.asset!, offset: cell.getBoundingClientRect().top - top };
        });
        if (loaded.endsWith("· 1,750 loaded")) {
          let resumePage!: () => void;
          let requested!: () => void;
          const continued = new Promise<void>(resolve => resumePage = resolve);
          const pending = new Promise<void>(resolve => requested = resolve);
          await page.route("**/api/v1/photos/assets/query", async route => { requested(); await continued; await route.continue(); }, { times: 1 });
          await page.getByRole("button", { name: "Documents", exact: true }).click();
          release();
          await page.getByRole("button", { name: "Photos", exact: true }).click();
          await pending;
          await expect(page.getByText(loaded, { exact: true })).toBeVisible();
          await expect.poll(() => page.locator(`[data-asset="${anchor.id}"]`).evaluate(element => element.getBoundingClientRect().top - element.closest(".photo-scroll")!.getBoundingClientRect().top)).toBeCloseTo(anchor.offset, 0);
          await expect.poll(() => scroll.locator("img").count()).toBe(await page.locator("[data-asset]").count());
          resumePage();
        }
        release();
        await expect(page.getByRole("button", { name: "Refresh previews" })).toBeEnabled();
        const offset = await page.locator(`[data-asset="${anchor.id}"]`).evaluate(element => element.getBoundingClientRect().top - element.closest(".photo-scroll")!.getBoundingClientRect().top);
        expect(offset).toBeCloseTo(anchor.offset, 0);
        const settled = listings;
        await page.waitForTimeout(300);
        expect(listings).toBe(settled);
      } else await requestNext();
      await expect.poll(() => page.locator(".library-title span").innerText()).not.toBe(loaded);
      await expect(page.getByRole("button", { name: "Refresh previews" })).toBeEnabled();
      expect(await page.locator("[data-asset]").count()).toBeLessThan(200);
      expect(await page.evaluate(() => (window as unknown as { photoActiveURLs: Set<string> }).photoActiveURLs.size)).toBeLessThan(200);
    }
    await expect(page.getByText("10,000 photos · 10,000 loaded")).toBeVisible();
    await page.waitForLoadState("networkidle");
    await expect(page.getByRole("button", { name: "Load more", exact: true })).toHaveCount(0);
    await expect(page.getByRole("navigation", { name: "Photo years" }).getByRole("button", { name: "2022", exact: true })).toBeVisible();
    await page.getByRole("combobox", { name: /^Group photos/ }).click();
    await page.getByRole("option", { name: "Months", exact: true }).click();
    await scroll.evaluate(element => element.scrollTop = element.scrollHeight);
    const last = page.locator("[data-asset]").last().getByRole("button").first();
    await last.click({ modifiers: ["Shift"] });
    await expect.poll(async () => Number((await page.locator(".selection-summary strong").innerText()).split(" ")[0])).toBeGreaterThan(9000);
    await expect(page.getByRole("button", { name: "Clear selection", exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Clear selection", exact: true }).click();
    await page.getByRole("navigation", { name: "Photo years" }).getByRole("button", { name: "2024", exact: true }).click();
    await expect.poll(() => scroll.evaluate(element => [...element.querySelectorAll("h2")].find(header => header.getBoundingClientRect().top <= element.getBoundingClientRect().top + 1 && header.getBoundingClientRect().bottom > element.getBoundingClientRect().top)?.textContent)).toContain("2024");
    await scroll.evaluate(element => element.scrollTop += 100);
    await expect.poll(() => scroll.evaluate(element => [...element.querySelectorAll("h2")].find(header => header.getBoundingClientRect().top <= element.getBoundingClientRect().top + 1 && header.getBoundingClientRect().bottom > element.getBoundingClientRect().top)?.textContent)).toContain("2024");
    const visibleID = await scroll.evaluate(element => {
      const top = element.getBoundingClientRect().top + 56;
      return [...element.querySelectorAll<HTMLElement>("[data-asset]")].find(cell => cell.getBoundingClientRect().top >= top)?.dataset.asset;
    });
    await page.locator(`[data-asset="${visibleID}"]`).getByRole("checkbox").check();
    const position = await scroll.evaluate(element => element.scrollTop);
    let release!: () => void;
    let entered!: () => void;
    const delayed = new Promise<void>(resolve => entered = resolve);
    const hold = new Promise<void>(resolve => release = resolve);
    await page.route("**/api/v1/photos/assets/query", async route => { entered(); await hold; await route.continue(); }, { times: 1 });
    await page.getByRole("button", { name: "Refresh previews" }).click();
    await delayed;
    await page.getByRole("navigation", { name: "Photo years" }).getByRole("button", { name: "2022", exact: true }).click();
    await expect.poll(() => scroll.evaluate(element => [...element.querySelectorAll("h2")].find(header => header.getBoundingClientRect().top <= element.getBoundingClientRect().top + 1 && header.getBoundingClientRect().bottom > element.getBoundingClientRect().top)?.textContent)).toContain("2022");
    const latestPosition = await scroll.evaluate(element => element.scrollTop);
    expect(latestPosition).not.toBe(position);
    release();
    await expect(page.getByRole("button", { name: "Refresh previews" })).toBeEnabled({ timeout: 60_000 });
    expect(await scroll.evaluate(element => element.scrollTop)).toBeCloseTo(latestPosition, 0);
    await page.getByRole("button", { name: "Clear selection", exact: true }).click();
    await scroll.evaluate(element => element.scrollTop = 0);
    await expect(page.locator(`[data-asset="${firstAsset}"] img`)).toBeVisible();
    await page.waitForLoadState("networkidle");
    const retained = await page.evaluate(async () => {
      const names = (await caches.keys()).filter(name => name.startsWith("docbank-photo-previews-"));
      return (await Promise.all(names.map(async name => (await (await caches.open(name)).keys()).map(key => key.url)))).flat();
    });
    const seen = new Map([...requests].filter(([url]) => retained.includes(url)));
    await scroll.evaluate(element => element.scrollTop = 15_000);
    await page.waitForLoadState("networkidle");
    await scroll.evaluate(element => element.scrollTop = 0);
    await expect(page.locator(`[data-asset="${firstAsset}"] img`)).toBeVisible();
    await page.waitForLoadState("networkidle");
    for (const [url, count] of seen) expect(requests.get(url)).toBe(count);
    expect(await page.evaluate(() => (window as unknown as { photoActiveURLs: Set<string> }).photoActiveURLs.size)).toBeLessThan(200);
    await page.getByRole("combobox", { name: /^Grid density/ }).click();
    await page.getByRole("option", { name: "Compact", exact: true }).click();
    await page.getByRole("navigation", { name: "Photo years" }).getByRole("button", { name: "2024", exact: true }).click();
    for (const width of [1440, 400]) {
      await page.setViewportSize({ width, height: 960 });
      await page.getByRole("navigation", { name: "Photo years" }).getByRole("button", { name: "2024", exact: true }).click();
      await page.waitForLoadState("networkidle");
      for (const theme of ["light", "dark"]) {
        await page.evaluate(value => { localStorage.setItem("docbank-theme", value); document.documentElement.classList.toggle("dark", value === "dark"); }, theme);
        await page.screenshot({ path: path.join(output!, `web-photos-${width}-${theme}-loaded.png`), animations: "disabled", timeout: 15_000 });
      }
    }
    const fresh = new URL(await run("web", "--no-browser"));
    fresh.pathname = "/photos";
    await page.goto("about:blank");
    await page.goto(fresh.href);
    await expect(page.getByRole("combobox", { name: /^Grid density/ })).toContainText("Compact");
    await expect(scroll.locator("img").first()).toBeVisible();
    await expect(page.getByRole("navigation", { name: "Photo years" }).getByRole("button")).toHaveCount(3);
    for (const width of [1440, 400]) {
      await page.setViewportSize({ width, height: 960 });
      await page.waitForLoadState("networkidle");
      for (const theme of ["light", "dark"]) {
        await page.evaluate(value => { localStorage.setItem("docbank-theme", value); document.documentElement.classList.toggle("dark", value === "dark"); }, theme);
        await page.screenshot({ path: path.join(output!, `web-photos-${width}-${theme}.png`), animations: "disabled" });
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

test("photo loupe keeps painted geometry and crop through delayed resolution upgrades", async ({ page }) => {
  test.setTimeout(300_000);
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "loupe-geometry-"));
  const env = { ...process.env, DOCBANK_HOME: path.join(workspace, "vault"), DOCBANK_LOCK_DIR: path.join(workspace, "locks"), DOCBANK_TELEMETRY_ENABLED: "0" };
  const run = async (...args: string[]) => (await exec(binary, args, { cwd: repository, env, timeout: 60_000 })).stdout.trim();
  const evidence = [];
  let previewURL = "";
  try {
    await mkdir(output!, { recursive: true });
    await exec("go", ["run", "-tags", "fts5", "./frontend/screenshots/photos-fixture.go", env.DOCBANK_HOME, "12"], { cwd: repository, env, timeout: 240_000 });
    await page.addInitScript(() => Object.defineProperty(window, "caches", { value: undefined }));
    for (const width of [1440, 400]) {
      await page.setViewportSize({ width, height: 960 });
      const url = new URL(await run("web", "--no-browser")); url.pathname = "/photos";
      await page.goto("about:blank");
      const listing = page.waitForResponse(response => response.url().endsWith("/photos/assets/query") && response.ok());
      await page.goto(url.href);
      const rows = (await (await listing).json()).items;
      const row = rows.find((item: { name: string }) => item.name === `Synthetic-photo-${width === 1440 ? "00002" : "00008"}.jpg`);
      let releaseFit!: () => void, releaseLarge!: () => void, releaseOriginal!: () => void;
      const fitHold = new Promise<void>(resolve => releaseFit = resolve), largeHold = new Promise<void>(resolve => releaseLarge = resolve), originalHold = new Promise<void>(resolve => releaseOriginal = resolve);
      const preparePattern = `**/api/v1/photos/assets/${row.asset_id}/preview`;
      await page.route(preparePattern, async route => { await (route.request().postDataJSON().size === "fit" ? fitHold : largeHold); await route.continue(); });
      await page.route("**/api/daemon/web-download**", async route => { await originalHold; await route.continue(); });
      await page.locator(`[data-asset="${row.asset_id}"] .photo-image`).press("Enter");
      const viewport = page.locator(".photo-viewport"), image = viewport.locator("img");
      await expect(page.getByText("Grid preview", { exact: true })).toBeVisible();
      const measure = async (tier: string) => {
        await image.evaluate(async element => { await (element as HTMLImageElement).decode(); });
        const geometry = await image.evaluate(element => {
          const image = element as HTMLImageElement, box = image.getBoundingClientRect(), viewport = image.closest(".photo-viewport")!.getBoundingClientRect();
          const scale = Math.min(box.width / image.naturalWidth, box.height / image.naturalHeight);
          const paint = { x: box.x + (box.width - image.naturalWidth * scale) / 2, y: box.y + (box.height - image.naturalHeight * scale) / 2, width: image.naturalWidth * scale, height: image.naturalHeight * scale };
          return { natural: { width: image.naturalWidth, height: image.naturalHeight }, element: { x: box.x, y: box.y, width: box.width, height: box.height }, paint, aspect: paint.width / paint.height, crop: [Math.max(0, (viewport.left - paint.x) / paint.width), Math.max(0, (viewport.top - paint.y) / paint.height), Math.min(1, (viewport.right - paint.x) / paint.width), Math.min(1, (viewport.bottom - paint.y) / paint.height)], transform: (image.parentElement as HTMLElement).style.transform };
        });
        const screenshot = await viewport.screenshot({ path: path.join(output!, `web-photo-geometry-${width}-${tier}.png`), animations: "disabled" });
        const pixels = await page.evaluate(async base64 => {
          const bitmap = await createImageBitmap(new Blob([Uint8Array.from(atob(base64), character => character.charCodeAt(0))], { type: "image/png" }));
          const canvas = document.createElement("canvas"); canvas.width = bitmap.width; canvas.height = bitmap.height;
          const context = canvas.getContext("2d")!; context.drawImage(bitmap, 0, 0); bitmap.close();
          const { data } = context.getImageData(0, 0, canvas.width, canvas.height);
          let left = canvas.width, top = canvas.height, right = -1, bottom = -1;
          const columns = new Array<number>(canvas.width).fill(0), rows = new Array<number>(canvas.height).fill(0);
          const samples = [];
          for (let y = 0; y < canvas.height; y++) for (let x = 0; x < canvas.width; x++) {
            const i = (y * canvas.width + x) * 4, r = data[i], g = data[i + 1], b = data[i + 2];
            if (b > r + 45 && g > r + 35) { columns[x]++; rows[y]++; }
            if (x % 8 === 4 && y % 8 === 4) samples.push(r, g, b);
          }
          for (let x = 0; x < columns.length; x++) if (columns[x] > 100) { left = Math.min(left, x); right = Math.max(right, x); }
          for (let y = 0; y < rows.length; y++) if (rows[y] > 100) { top = Math.min(top, y); bottom = Math.max(bottom, y); }
          return { bounds: { x: left, y: top, width: right - left + 1, height: bottom - top + 1 }, samples };
        }, screenshot.toString("base64"));
        return { tier, ...geometry, ...pixels };
      };
      const resting = await measure("grid-rest");
      expect(resting.bounds.width / resting.bounds.height).toBeCloseTo(resting.aspect, 2);
      await viewport.dblclick();
      const box = (await viewport.boundingBox())!;
      await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2); await page.mouse.down();
      await page.mouse.move(box.x + box.width / 2 + 32, box.y + box.height / 2 + 19); await page.mouse.up();
      const grid = await measure("grid");
      const stages = [resting, grid];
      for (const tier of ["fit", "large", "original-pending", "original"]) {
        if (tier === "fit") releaseFit();
        else if (tier === "large") releaseLarge();
        else if (tier === "original-pending") {
          const requested = page.waitForRequest(request => request.url().includes("/api/daemon/web-download"));
          await page.getByRole("button", { name: "View original", exact: true }).click(); await requested;
        } else releaseOriginal();
        await expect(page.getByText(tier === "original" ? "Original" : `${tier === "fit" ? "Fit" : "Large"} preview`, { exact: true })).toBeVisible({ timeout: 60_000 });
        const current = await measure(tier); stages.push(current);
        await writeFile(path.join(output!, "geometryproof.json"), JSON.stringify([...evidence, { width, stages: stages.map(({ samples, ...stage }) => stage) }], null, 2));
        expect(current.transform).toBe(grid.transform);
        expect(current.aspect).toBeCloseTo(grid.aspect, 2);
        for (let i = 0; i < grid.crop.length; i++) expect(Math.abs(current.crop[i] - grid.crop[i])).toBeLessThan(0.002);
        // The original-loading spinner can wrap the existing mobile toolbar.
        if (tier === "original-pending") continue;
        for (const key of ["x", "y", "width", "height"] as const) expect(Math.abs(current.element[key] - grid.element[key])).toBeLessThan(1);
        for (const key of ["x", "y", "width", "height"] as const) expect(Math.abs(current.bounds[key] - grid.bounds[key])).toBeLessThanOrEqual(2);
        const changed = current.samples.filter((value, i) => Math.abs(value - grid.samples[i]) > 20).length / grid.samples.length;
        expect(changed).toBeLessThan(0.04);
        Object.assign(current, { changedPixelFraction: changed });
      }
      evidence.push({ width, stages: stages.map(({ samples, ...stage }) => stage) });
      previewURL = page.url();
      await page.screenshot({ path: path.join(output!, `web-photo-geometry-${width}-viewer.png`), animations: "disabled" });
      await page.keyboard.press("Escape");
      await page.unroute(preparePattern); await page.unroute("**/api/daemon/web-download**");
    }
    await writeFile(path.join(output!, "geometryproof.json"), JSON.stringify(evidence, null, 2));
  } finally {
    if (process.env.DOCBANK_KEEP_PHOTO_PREVIEW) {
      const url = new URL(await run("web", "--no-browser")); url.pathname = "/photos";
      if (previewURL) url.hash += `&photo=${new URLSearchParams(new URL(previewURL).hash.slice(1)).get("photo")}`;
      await writeFile(path.join(output!, "geometry-preview.json"), JSON.stringify({ url: url.href, workspace, binary }, null, 2));
    } else { await run("daemon", "stop"); await rm(workspace, { recursive: true, force: true }); }
  }
});


test("photo loupe opens, upgrades, navigates, restores focus and reloads a direct URL", async ({ page, browser }) => {
  test.setTimeout(480_000);
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "loupe-proof-"));
  const vault = path.join(workspace, "vault");
  const env = { ...process.env, DOCBANK_HOME: vault, DOCBANK_LOCK_DIR: path.join(workspace, "locks"), DOCBANK_TELEMETRY_ENABLED: "0" };
  const run = async (...args: string[]) => (await exec(binary, args, { cwd: repository, env, timeout: 60_000 })).stdout.trim();
  let assetID = "";
  const errors: string[] = [];
  page.on("pageerror", error => errors.push(error.message));
  try {
    await mkdir(output!, { recursive: true });
    await exec("go", ["run", "-tags", "fts5", "./frontend/screenshots/photos-fixture.go", "-unavailable", vault, "300"], { cwd: repository, env, timeout: 240_000 });
    const webURL = new URL(await run("web", "--no-browser")); webURL.pathname = "/photos";
    await page.goto(webURL.href);
    const scroll = page.getByTestId("photo-scroll");
    await expect(scroll.locator("img").first()).toBeVisible();
    await expect(page.getByText("300 photos · 250 loaded", { exact: true })).toBeVisible();
    await scroll.evaluate(element => element.scrollTop = 400);
    const opening = scroll.locator("[data-asset]").filter({ has: page.locator("img") }).first();
    assetID = (await opening.getAttribute("data-asset"))!;
    const firstLoadedID = assetID;
    const button = scroll.locator(`[data-asset="${assetID}"] .photo-image`);
    const separate = scroll.locator("[data-asset]").nth(1);
    const separateID = (await separate.getAttribute("data-asset"))!;
    await separate.getByRole("checkbox").click();
    await button.dblclick();
    const position = await scroll.evaluate(element => element.scrollTop);
    await expect(scroll.locator(`[data-asset="${assetID}"]`).getByRole("checkbox")).toBeChecked();
    await expect(scroll.locator(`[data-asset="${separateID}"]`).getByRole("checkbox")).not.toBeChecked();
    await expect(page.getByRole("dialog", { name: /^Photo viewer/ })).toBeVisible();
    await expect(page.getByText("Fit preview", { exact: true })).toBeVisible({ timeout: 60_000 });
    const viewport = page.locator(".photo-viewport");
    await viewport.dblclick();
    const transform = await page.locator(".pan").evaluate(element => (element as HTMLElement).style.transform);
    await expect(page.getByText("Large preview", { exact: true })).toBeVisible({ timeout: 60_000 });
    expect(await page.locator(".pan").evaluate(element => (element as HTMLElement).style.transform)).toBe(transform);
    await page.keyboard.press("0");
    await expect(viewport).not.toHaveAttribute("data-zoomed");
    await page.keyboard.press("ArrowRight");
    await expect(page).not.toHaveURL(new RegExp(`photo=${assetID}`));
    await page.getByRole("navigation", { name: "Photos in current result" }).getByRole("button").first().click();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog", { name: /^Photo viewer/ })).toHaveCount(0);
    await expect.poll(() => scroll.evaluate(element => element.scrollTop)).toBeCloseTo(position, 0);
    await expect(button).toBeFocused();
    await expect(page.getByText("1 selected photo", { exact: true })).toBeVisible();
    await scroll.evaluate(element => element.scrollTop = 1200);
    const forwardID = await scroll.evaluate(element => [...element.querySelectorAll<HTMLElement>("[data-asset]")].find(cell => cell.getBoundingClientRect().top >= element.getBoundingClientRect().top)!.dataset.asset!);
    const forwardFocus = scroll.locator(`[data-asset="${forwardID}"] .photo-image`);
    await forwardFocus.evaluate(element => (element as HTMLElement).focus({preventScroll:true}));
    const forwardPosition = await scroll.evaluate(element => element.scrollTop);
    await page.goForward();
    await expect(page.getByRole("dialog", { name: /^Photo viewer/ })).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog", { name: /^Photo viewer/ })).toHaveCount(0);
    await expect.poll(() => scroll.evaluate(element => element.scrollTop)).toBeCloseTo(forwardPosition, 0);
    await expect(forwardFocus).toBeFocused();
    await scroll.locator(`[data-asset="${forwardID}"] .open-photo`).click();
    await expect(page.getByRole("dialog", {name:/^Photo viewer/})).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page.getByText("1 selected photo", {exact:true})).toBeVisible();
    await forwardFocus.press("Enter");
    await expect(page.getByRole("dialog", {name:/^Photo viewer/})).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page.getByText("1 selected photo", {exact:true})).toBeVisible();
    await scroll.evaluate(element => element.scrollTop = 400);
    const aID = await scroll.evaluate(element => [...element.querySelectorAll<HTMLElement>("[data-asset]")].find(cell => cell.getBoundingClientRect().top >= element.getBoundingClientRect().top)!.dataset.asset!);
    const aOpen = scroll.locator(`[data-asset="${aID}"] .open-photo`);
    await aOpen.click(); const aPosition = await scroll.evaluate(element => element.scrollTop);
    await page.getByRole("button", {name:"Background jobs", exact:true}).click();
    await expect(page.getByRole("dialog", {name:"Daemon background jobs"})).toBeVisible();
    const jobsURL = page.url(); await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog", {name:"Daemon background jobs"})).toHaveCount(0);
    await expect(page.getByRole("dialog", {name:/^Photo viewer/})).toBeFocused(); await expect(page).toHaveURL(jobsURL);
    await page.getByRole("button", {name:"Documents", exact:true}).click(); await page.goBack();
    await expect(page.getByRole("navigation", {name:"Photos in current result"})).toBeVisible();
    const returnedA = page.url(); await page.keyboard.press("ArrowRight"); await expect(page).not.toHaveURL(returnedA);
    await page.keyboard.press("ArrowLeft"); await expect(page).toHaveURL(returnedA);
    await page.getByRole("button", {name:"Documents", exact:true}).click();
    await page.getByRole("button", {name:"Photos", exact:true}).click();
    await expect(page.getByRole("dialog", {name:/^Photo viewer/})).toHaveCount(0);
    await expect(page.getByRole("button", {name:"Photos", exact:true})).toBeFocused();
    await expect.poll(() => scroll.evaluate(element => element.scrollTop)).toBeCloseTo(aPosition, 0);
    await scroll.evaluate(element => element.scrollTop = 1200);
    const cID = await scroll.evaluate(element => [...element.querySelectorAll<HTMLElement>("[data-asset]")].find(cell => cell.getBoundingClientRect().top >= element.getBoundingClientRect().top)!.dataset.asset!);
    const cOpen = scroll.locator(`[data-asset="${cID}"] .open-photo`);
    await cOpen.click(); const cPosition = await scroll.evaluate(element => element.scrollTop);
    await page.keyboard.press("Escape");
    await expect.poll(() => scroll.evaluate(element => element.scrollTop)).toBeCloseTo(cPosition, 0); await expect(cOpen).toBeFocused();
    await page.goBack(); await expect(page).not.toHaveURL(/\/photos/);
    await page.goBack(); await expect(page).toHaveURL(returnedA);
    await expect(page.getByRole("navigation", {name:"Photos in current result"})).toBeVisible();
    await page.keyboard.press("Escape");
    await expect.poll(() => scroll.evaluate(element => element.scrollTop)).toBeCloseTo(aPosition, 0); await expect(aOpen).toBeFocused();
    let releaseContinuation!: () => void;
    let continuationRequested!: () => void;
    const continuationHold = new Promise<void>(resolve => releaseContinuation = resolve);
    const continuationPending = new Promise<void>(resolve => continuationRequested = resolve);
    await page.route("**/api/v1/photos/assets/query", async route => { continuationRequested(); await continuationHold; await route.abort("failed"); }, { times: 1 });
    await scroll.evaluate(element => element.scrollTop = element.scrollHeight - element.clientHeight - 400);
    await continuationPending;
    await expect(page.getByText("300 photos · 250 loaded", { exact: true })).toBeVisible();
    const last = scroll.locator("[data-asset]").last();
    await last.locator(".photo-image").dblclick();
    const before = await page.locator(".caption strong").innerText();
    await page.keyboard.press("ArrowRight");
    releaseContinuation();
    const viewer = page.getByRole("dialog", { name: /^Photo viewer/ });
    await expect(viewer.getByRole("button", { name: "Retry navigation" })).toBeVisible();
    await expect(page.locator(".caption strong")).toHaveText(before);
    const failedURL = page.url(), gesture = await page.context().newCDPSession(page);
    const bounds = (await page.locator(".photo-viewport").boundingBox())!, x = bounds.x + bounds.width / 2, y = bounds.y + bounds.height / 2;
    await gesture.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x, y }] });
    await gesture.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [{ x: x - 150, y }] });
    await gesture.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
    await gesture.detach();
    await expect(page).toHaveURL(failedURL);
    await expect(page.locator(".pan")).toHaveCSS("transform", "matrix(1, 0, 0, 1, 0, 0)");
    await expect(page.getByText("Fit preview", { exact: true })).toBeVisible({ timeout: 60_000 });
    await expect.poll(() => viewer.locator(".filmstrip img").evaluateAll(images => images.every(image => (image as HTMLImageElement).naturalWidth > 0))).toBe(true);
    for (const theme of ["light", "dark"]) {
      await page.evaluate(value => { localStorage.setItem("docbank-theme", value); document.documentElement.classList.toggle("dark", value === "dark"); }, theme);
      await page.screenshot({ path: path.join(output!, `web-photo-loupe-retry-1440-${theme}.png`), animations: "disabled" });
    }
    const continued = page.waitForResponse(response => response.url().endsWith("/photos/assets/query") && response.ok());
    await viewer.getByRole("button", { name: "Retry navigation" }).click();
    const continuation = await (await continued).json();
    const terminalID = continuation.items.find((item: {name:string}) => item.name === "Synthetic-unavailable.jpg").asset_id;
    await expect(page.locator(".caption strong")).toHaveText(before); await page.keyboard.press("ArrowRight");
    await expect(page.locator(".caption strong")).not.toHaveText(before);
    await expect(page.locator(".library-title span")).toHaveText("300 photos · 300 loaded");
    await page.getByRole("button", { name: "View original", exact: true }).click();
    await expect(page.getByText("Original", { exact: true })).toBeVisible({ timeout: 60_000 });
    await page.keyboard.press("i");
    await expect(page.getByRole("complementary", { name: "Photo details" }).getByText("Synthetic Camera Model 1")).toBeVisible();
    for (const width of [1440, 400]) {
      await page.setViewportSize({ width, height: 960 });
      for (const theme of ["light", "dark"]) {
        await page.evaluate(value => { localStorage.setItem("docbank-theme", value); document.documentElement.classList.toggle("dark", value === "dark"); }, theme);
        await page.screenshot({ path: path.join(output!, `web-photo-loupe-${width}-${theme}.png`), animations: "disabled" });
      }
    }
    assetID = new URL(page.url()).hash.slice("#photo=".length);
    await page.reload();
    await expect(page.getByText("Open your Docbank", { exact: true })).toBeVisible();
    const directURL = new URL(await run("web", "--no-browser")); directURL.pathname = "/photos"; directURL.hash += `&photo=${assetID}`;
    await page.goto("about:blank");
    await page.goto(directURL.href);
    await expect(page.getByText("Fit preview", { exact: true })).toBeVisible({ timeout: 60_000 });
    await expect(page).toHaveURL(new RegExp(`#photo=${assetID}$`));
    await expect(page.getByRole("navigation", { name: "Photos in current result" })).toHaveCount(0);
    await page.keyboard.press("Escape");
    await expect(page).not.toHaveURL(/#photo=/);
    let releaseListing!: () => void;
    const listingHold = new Promise<void>(resolve => releaseListing = resolve);
    const listingRoute = async (route: import("@playwright/test").Route) => {
      if (route.request().postDataJSON().query.filters?.asset_ids) await route.abort("failed");
      else { await listingHold; await route.continue(); }
    };
    await page.route("**/api/v1/photos/assets/query", listingRoute);
    const lookupURL = new URL(await run("web", "--no-browser")); lookupURL.pathname = "/photos"; lookupURL.hash += `&photo=${firstLoadedID}`;
    await page.goto("about:blank"); await page.goto(lookupURL.href);
    await expect(page.locator(".photo-loupe [role=alert]")).toContainText("Failed to fetch");
    releaseListing();
    await expect(page.getByText("300 photos · 250 loaded", {exact:true})).toBeVisible();
    await expect(page.locator(".photo-loupe [role=alert]")).toContainText("Failed to fetch");
    await expect(page.getByText("Fit preview", {exact:true})).toHaveCount(0);
    await page.unroute("**/api/v1/photos/assets/query", listingRoute);
    await page.getByRole("button", {name:"Reload photo"}).click();
    await expect(page.getByText("Fit preview", {exact:true})).toBeVisible({timeout:60_000});
    await expect(page.locator(".photo-loupe [role=alert]")).toHaveCount(0);
    const reloadURL = new URL(await run("web", "--no-browser")); reloadURL.pathname = "/photos";
    await page.goto("about:blank"); await page.goto(reloadURL.href);
    await expect(page.getByText("300 photos · 250 loaded", {exact:true})).toBeVisible();
    await scroll.locator("[data-asset]").first().getByRole("checkbox").click();
    await page.route("**/api/v1/photos/assets/query", async route => route.abort("failed"), {times:1});
    await page.evaluate(id => { history.pushState(null, "", `/photos#photo=${id}`); window.dispatchEvent(new PopStateEvent("popstate")); }, assetID);
    await expect(page.locator(".photo-loupe [role=alert]")).toContainText("Failed to fetch");
    const failedDirectURL = page.url();
    await page.screenshot({path:path.join(output!, "web-photo-loupe-reload-400-dark.png"), animations:"disabled"});
    await page.getByRole("button", {name:"Reload photo"}).click();
    await expect(page.getByText("Fit preview", {exact:true})).toBeVisible({timeout:60_000}); await expect(page).toHaveURL(failedDirectURL);
    await page.keyboard.press("Escape"); await expect(page.getByText("1 selected photo", {exact:true})).toBeVisible();
    for (const operation of ["preview", "original"]) {
      const freshURL = new URL(await run("web", "--no-browser")); freshURL.pathname = "/photos";
      let staleID = "";
      await page.route("**/api/v1/photos/assets/query", async route => {
        const url = new URL(route.request().url()), host = url.host; url.hostname = "127.0.0.1";
        const response = await route.fetch({url:url.href, headers:{...await route.request().allHeaders(), host}}); const body = await response.json(); staleID = body.items[0].asset_id;
        body.items[0].content_version_id = "00000000-0000-4000-8000-000000000001";
        if (operation === "preview") body.items[0].previews.fit = {state:"missing"};
        await route.fulfill({response, json:body});
      }, {times:1});
      await page.goto("about:blank"); await page.goto(freshURL.href);
      await expect(scroll.locator("img").first()).toBeVisible();
      await scroll.locator("[data-asset]").nth(1).getByRole("checkbox").click();
      await scroll.locator(`[data-asset="${staleID}"] .photo-image`).press("Enter");
      if (operation === "original") await page.getByRole("button", {name:"View original", exact:true}).click();
      await expect(page.getByRole("button", {name:"Reload photo"})).toBeVisible();
      if (operation === "original") await expect(page.locator(".photo-loupe [role=alert]")).toContainText("Reload this photo");
      const staleURL = page.url(); await page.getByRole("button", {name:"Reload photo"}).click();
      await expect(page.getByText("Fit preview", {exact:true})).toBeVisible({timeout:60_000}); await expect(page).toHaveURL(staleURL);
      await page.keyboard.press("Escape"); await expect(page.getByText("1 selected photo", {exact:true})).toBeVisible();
      await expect(scroll.locator(`[data-asset="${staleID}"] .photo-image`)).toBeFocused();
    }
    const replacementURL = new URL(await run("web", "--no-browser")); replacementURL.pathname = "/photos";
    const replacementListing = page.waitForResponse(response => response.url().endsWith("/photos/assets/query") && response.ok());
    await page.goto("about:blank"); await page.goto(replacementURL.href);
    const replacementRow = (await (await replacementListing).json()).items[0];
    const replacementCell = scroll.locator(`[data-asset="${replacementRow.asset_id}"] .photo-image`);
    await expect(scroll.locator("img").first()).toBeVisible(); await scroll.locator("[data-asset]").nth(1).getByRole("checkbox").click();
    await replacementCell.press("Enter"); await expect(page.getByText("Fit preview", {exact:true})).toBeVisible({timeout:60_000});
    const oldThumbnail = page.locator(`.filmstrip [data-asset="${replacementRow.asset_id}"] img`);
    await expect(oldThumbnail).toHaveAttribute("src", /^blob:/); const oldThumbnailURL = await oldThumbnail.getAttribute("src");
    const replacementFile = path.join(workspace, "replacement.jpg");
    await run("get", "/Synthetic-photo-00001.jpg", replacementFile);
    await page.keyboard.press("Escape"); await expect(replacementCell).toBeFocused();
    await run("put", replacementFile, `/${replacementRow.name}`, "--mime-type", "image/jpeg");
    const originalFetch = globalThis.fetch, sourceURL = new URL(replacementURL); const sourceHost = sourceURL.host; sourceURL.hostname = "127.0.0.1";
    globalThis.fetch = (input, init) => { const headers = new Headers(init?.headers); headers.set("Host", sourceHost); return originalFetch(typeof input === "string" && input.startsWith("/") ? sourceURL.origin + input : input, {...init, headers}); };
    try {
      await expect.poll(async () => (await listPhotoAssets({query:{v:1, syntax:"advanced", mode:"lexical", text:"", filters:{asset_ids:[replacementRow.asset_id]}, sort:{field:"capture_time", direction:"desc"}}, page_size:1}, {session:new URLSearchParams(replacementURL.hash.slice(1)).get("web_session")!})).items[0]?.previews.grid.state, {timeout:60_000}).toBe("ready");
    } finally { globalThis.fetch = originalFetch; }
    await page.evaluate(async () => { for (const name of await caches.keys()) { const cache = await caches.open(name); for (const key of await cache.keys()) await cache.delete(key); } });
    const obsoleteBytes = page.waitForResponse(response => response.url().includes("/previews/") && response.status() === 404);
    await replacementCell.press("Enter"); await obsoleteBytes; await expect(page.getByRole("button", {name:"Reload photo"})).toBeVisible();
    await page.screenshot({path:path.join(output!, "web-photo-loupe-replaced-400-dark.png"), animations:"disabled"});
    const replacedURL = page.url(); await page.getByRole("button", {name:"Reload photo"}).click();
    await expect(page.getByText("Fit preview", {exact:true})).toBeVisible({timeout:60_000}); await expect(page).toHaveURL(replacedURL);
    await expect(oldThumbnail).toHaveAttribute("src", /^blob:/); await expect(oldThumbnail).not.toHaveAttribute("src", oldThumbnailURL!);
    await expect(page.locator(".photo-loupe [role=alert]")).toHaveCount(0); await page.keyboard.press("Escape");
    await expect(replacementCell).toBeFocused(); await expect(page.getByText("1 selected photo", {exact:true})).toBeVisible();
    for (const delayed of [false, true]) {
      const progressive = await page.context().newPage(); await progressive.addInitScript(() => Object.defineProperty(window, "caches", {value:undefined}));
      let selectedID = "", gridGeneration = "", releaseGrid!: () => void, releaseFit!: () => void;
      const gridHold = new Promise<void>(resolve => releaseGrid = resolve), fitHold = new Promise<void>(resolve => releaseFit = resolve);
      await progressive.route("**/api/v1/photos/assets/query", async route => {
        const url = new URL(route.request().url()), host = url.host; url.hostname = "127.0.0.1";
        const response = await route.fetch({url:url.href, headers:{...await route.request().allHeaders(), host}}); const body = await response.json();
        selectedID = body.items[0].asset_id; gridGeneration = body.items[0].previews.grid.generation_id; await route.fulfill({response});
      }, {times:1});
      await progressive.route("**/api/v1/photos/assets/*/previews/*", async route => {
        if (!route.request().url().includes(`/assets/${selectedID}/`)) { await route.continue(); return; }
        if (route.request().url().endsWith(`/${gridGeneration}`)) { await gridHold; await route.abort("failed"); }
        else { await fitHold; await route.continue(); }
      });
      const progressiveURL = new URL(await run("web", "--no-browser")); progressiveURL.pathname = "/photos";
      await progressive.goto(progressiveURL.href, {waitUntil:"domcontentloaded"}); await expect(progressive.getByText("300 photos · 250 loaded", {exact:true})).toBeVisible();
      await progressive.locator(`[data-asset="${selectedID}"] .photo-image`).press("Enter");
      if (!delayed) { releaseGrid(); await expect(progressive.locator(".photo-loupe [role=alert]")).toContainText("Failed to fetch"); }
      releaseFit(); await expect(progressive.getByText("Fit preview", {exact:true})).toBeVisible({timeout:60_000});
      if (delayed) { const failed = progressive.waitForEvent("requestfailed", {predicate:request => request.url().includes(`/assets/${selectedID}/previews/${gridGeneration}`)}); releaseGrid(); await failed; }
      await expect(progressive.locator(".photo-loupe [role=alert]")).toHaveCount(0); await progressive.close();
    }
    const recovery = await page.context().newPage(); await recovery.addInitScript(() => Object.defineProperty(window, "caches", {value:undefined}));
    const recoveryURL = new URL(await run("web", "--no-browser")); recoveryURL.pathname = "/photos";
    const recoveryListing = recovery.waitForResponse(response => response.url().endsWith("/photos/assets/query") && response.ok());
    await recovery.goto(recoveryURL.href); const recoveryRows = (await (await recoveryListing).json()).items;
    const retained = recoveryRows[0], neighbor = recoveryRows[1];
    await expect(recovery.locator(`[data-asset="${retained.asset_id}"] img`)).toBeVisible();
    await recovery.route(`**/api/v1/photos/assets/${neighbor.asset_id}/previews/${neighbor.previews.grid.generation_id}`, route => route.abort("failed"), {times:1});
    await recovery.locator(`[data-asset="${retained.asset_id}"] .photo-image`).press("Enter");
    const railRetry = recovery.getByRole("button", {name:`Retry preview for ${neighbor.name}`}); await expect(railRetry).toBeVisible();
    await expect(recovery.locator(`.filmstrip [data-asset="${neighbor.asset_id}"] .placeholder`)).toContainText("Preview failed");
    for (const width of [1440, 400]) {
      await recovery.setViewportSize({width, height:960});
      if (width === 400) await expect(recovery.locator(".filmstrip")).toBeHidden();
      await recovery.screenshot({path:path.join(output!, `web-photo-loupe-rail-retry-${width}-dark.png`), animations:"disabled"});
    }
    await recovery.setViewportSize({width:1440, height:960});
    const railLocation = recovery.url(); await railRetry.click();
    await expect.poll(() => recovery.locator(`.filmstrip [data-asset="${neighbor.asset_id}"] img`).evaluate(image => (image as HTMLImageElement).naturalWidth)).toBeGreaterThan(0);
    await expect(recovery).toHaveURL(railLocation); await recovery.keyboard.press("Escape");
    await expect(recovery.getByRole("dialog", {name:/^Photo viewer/})).toHaveCount(0);
    let releaseRefresh!: () => void, enteredRefresh!: () => void;
    const refreshHold = new Promise<void>(resolve => releaseRefresh = resolve), refreshPending = new Promise<void>(resolve => enteredRefresh = resolve);
    await recovery.route("**/api/v1/photos/assets/query", async route => {
      const body = route.request().postDataJSON(); body.query.filters = {asset_ids:[neighbor.asset_id]}; delete body.cursor;
      const url = new URL(route.request().url()), host = url.host; url.hostname = "127.0.0.1";
      const response = await route.fetch({url:url.href, headers:{...await route.request().allHeaders(), host}, postData:JSON.stringify(body)});
      enteredRefresh(); await refreshHold; await route.fulfill({response});
    }, {times:1});
    await recovery.getByRole("button", {name:"Refresh previews"}).click(); await refreshPending;
    let failRetainedFit = true;
    await recovery.route(`**/api/v1/photos/assets/${retained.asset_id}/previews/*`, route => failRetainedFit && !route.request().url().endsWith(`/${retained.previews.grid.generation_id}`) ? route.abort("failed") : route.continue());
    await recovery.locator(`[data-asset="${retained.asset_id}"] .photo-image`).press("Enter"); releaseRefresh();
    const recoveryAlert = recovery.locator(".photo-loupe [role=alert]");
    await expect(recoveryAlert).toContainText("This photo is no longer in the refreshed result."); await expect(recoveryAlert).toContainText("Failed to fetch");
    await expect(recovery.getByRole("button", {name:"Reload photo"})).toBeVisible(); await expect(recovery.getByRole("button", {name:"Retry preview", exact:true})).toBeVisible();
    for (const width of [1440, 400]) {
      await recovery.setViewportSize({width, height:960});
      await recovery.screenshot({path:path.join(output!, `web-photo-loupe-membership-failure-${width}-dark.png`), animations:"disabled"});
    }
    await expect(recovery.getByRole("button", {name:"Reload photo"})).toBeEnabled();
    await recovery.route("**/api/v1/photos/assets/query", route => route.abort("failed"), {times:1});
    await recovery.getByRole("button", {name:"Reload photo"}).click();
    await expect(recoveryAlert.locator("span")).toHaveCount(3);
    failRetainedFit = false; await recovery.getByRole("button", {name:"Retry preview", exact:true}).click(); await expect(recovery.getByText("Fit preview", {exact:true})).toBeVisible({timeout:60_000});
    await expect(recoveryAlert).toContainText("This photo is no longer in the refreshed result."); await expect(recoveryAlert).toContainText("Failed to fetch");
    await recovery.getByRole("button", {name:"Reload photo"}).click(); await expect(recoveryAlert.locator("span")).toHaveCount(1);
    await writeFile(path.join(output!, "browserproof.json"), JSON.stringify({railRetry:true, cacheDisabled:true, retainedMembershipWithPreviewFailure:true, retainedMembershipWithLookupFailure:true, previewRetry:true, reloadRecovery:true, widths:[1440,400], binary}, null, 2));
    await recovery.close();
    const stalePaging = await page.context().newPage(), staleScroll = stalePaging.getByTestId("photo-scroll");
    const stalePagingURL = new URL(await run("web", "--no-browser")); stalePagingURL.pathname = "/photos";
    await stalePaging.goto(stalePagingURL.href); await expect(stalePaging.getByText("300 photos · 250 loaded", {exact:true})).toBeVisible();
    await stalePaging.route("**/api/v1/photos/assets/query", route => route.abort("failed"), {times:1});
    await staleScroll.evaluate(element => element.scrollTop = element.scrollHeight - element.clientHeight - 400);
    await expect(stalePaging.locator(".photo-error")).toContainText("Failed to fetch"); await staleScroll.locator("[data-asset]").last().locator(".photo-image").press("Enter");
    const stalePageURL = stalePaging.url(), staleName = await stalePaging.locator(".caption strong").innerText();
    await stalePaging.getByRole("button", {name:"Retry navigation"}).click(); await expect(stalePaging.locator(".library-title span")).toHaveText("300 photos · 300 loaded");
    await expect(stalePaging).toHaveURL(stalePageURL); await expect(stalePaging.locator(".caption strong")).toHaveText(staleName);
    await stalePaging.keyboard.press("ArrowRight"); await expect(stalePaging).not.toHaveURL(stalePageURL); await stalePaging.close();
    const touchContext = await browser.newContext({ hasTouch: true, viewport: { width: 400, height: 960 }, deviceScaleFactor: Number(process.env.DOCBANK_SCREENSHOT_SCALE ?? 1), colorScheme: "dark" });
    const touch = await touchContext.newPage(); touch.setDefaultTimeout(15_000);
    const touchURL = new URL(await run("web", "--no-browser")); touchURL.pathname = "/photos";
    await touch.goto(touchURL.href);
    const cells = touch.locator("[data-asset]");
    await expect(cells.first().locator("img")).toBeVisible();
    const firstID = (await cells.first().getAttribute("data-asset"))!;
    const secondID = (await cells.nth(1).getAttribute("data-asset"))!;
    const a = touch.locator(`[data-asset="${firstID}"]`), b = touch.locator(`[data-asset="${secondID}"]`);
    await a.locator(".photo-image").click();
    await expect(touch.getByRole("dialog", {name:/^Photo viewer/})).toHaveCount(0);
    await expect(touch.getByText("1 selected photo", {exact:true})).toBeVisible();
    await touch.getByRole("button", {name:"Clear selection"}).tap();
    await a.getByRole("checkbox").tap();
    await b.locator(".photo-image").tap();
    await expect(a.getByRole("checkbox")).toBeChecked(); await expect(b.getByRole("checkbox")).toBeChecked();
    await b.locator(".photo-image").tap();
    await expect(a.getByRole("checkbox")).toBeChecked(); await expect(b.getByRole("checkbox")).not.toBeChecked();
    await expect(touch.getByText("1 selected photo", { exact: true })).toBeVisible();
    await touch.getByRole("button", { name: "Clear selection" }).tap();
    await a.locator(".photo-image").tap();
    await expect(touch.getByText("Fit preview", { exact: true })).toBeVisible({ timeout: 60_000 });
    const swipe = async (direction: -1 | 1) => {
      const box = (await touch.locator(".photo-viewport").boundingBox())!;
      const session = await touchContext.newCDPSession(touch);
      const x = box.x + box.width / 2, y = box.y + box.height / 2;
      await session.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x, y }] });
      await session.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [{ x: x - direction * 150, y }] });
      await session.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
      await session.detach();
      await expect(touch.locator(".pan")).toHaveCSS("transform", "matrix(1, 0, 0, 1, 0, 0)");
    };
    const boundaryURL = touch.url(); await swipe(-1); await expect(touch).toHaveURL(boundaryURL);
    await touch.getByRole("button", { name: "Close photo viewer" }).tap();
    const retryID = (await cells.last().getAttribute("data-asset"))!;
    const standalone = new URL(await run("web", "--no-browser")); standalone.pathname = "/photos"; standalone.hash += `&photo=${retryID}`;
    await touch.goto("about:blank");
    await touch.route("**/api/v1/photos/assets/*/preview", async route => route.request().postDataJSON().size === "fit" ? route.abort("failed") : route.continue(), { times: 1 });
    await touch.goto(standalone.href);
    await expect(touch.getByRole("button", { name: "Retry preview" })).toBeVisible();
    await expect.poll(() => touch.locator(".photo-viewport img").evaluate(image => (image as HTMLImageElement).naturalWidth)).toBeGreaterThan(0);
    await touch.screenshot({ path: path.join(output!, "web-photo-loupe-fit-retry-400-dark.png"), animations: "disabled" });
    await touch.getByRole("button", { name: "Retry preview" }).tap();
    await expect(touch.getByText("Fit preview", { exact: true })).toBeVisible({ timeout: 60_000 });
    const standaloneURL = touch.url(); await swipe(1); await expect(touch).toHaveURL(standaloneURL);
    await touch.screenshot({ path: path.join(output!, "web-photo-loupe-touch-400-dark.png"), animations: "disabled" });
    const terminal = new URL(await run("web", "--no-browser")); terminal.pathname = "/photos"; terminal.hash += `&photo=${terminalID}`;
    await touch.goto("about:blank"); await touch.goto(terminal.href);
    await expect(touch.getByText("No preview", {exact:true})).toBeVisible({timeout:60_000});
    await expect(touch.getByRole("button", {name:"Retry preview"})).toHaveCount(0);
    await touch.screenshot({path:path.join(output!, "web-photo-loupe-unavailable-400-dark.png"), animations:"disabled"});
    await touchContext.close();
    const displayURL = new URL(await run("web", "--no-browser")); displayURL.pathname = "/photos";
    const displayListing = page.waitForResponse(response => response.url().endsWith("/photos/assets/query") && response.ok());
    await page.goto("about:blank"); await page.goto(displayURL.href);
    const displayRows = (await (await displayListing).json()).items, oldDisplay = displayRows[0], newDisplay = displayRows[2];
    await run("photos", "assets", "detach", newDisplay.asset_id, newDisplay.display_file_id);
    const attached = JSON.parse(await run("photos", "assets", "attach", oldDisplay.asset_id, `id:${newDisplay.node_id}`, "--role", "image"));
    const newFile = attached.files.find((file: {node_id:number}) => file.node_id === newDisplay.node_id).id;
    await run("photos", "assets", "display", oldDisplay.asset_id, oldDisplay.display_file_id);
    const displayCell = scroll.locator(`[data-asset="${oldDisplay.asset_id}"] .photo-image`);
    await expect(scroll.locator("img").first()).toBeVisible(); await scroll.locator("[data-asset]").nth(1).getByRole("checkbox").click();
    await displayCell.press("Enter"); await expect(page.getByText("Fit preview", {exact:true})).toBeVisible({timeout:60_000});
    const shownURL = await page.locator(".photo-viewport img").getAttribute("src"), shownLocation = page.url();
    const originalRequests: import("@playwright/test").Request[] = []; page.on("request", request => { if (request.url().includes("web-download")) originalRequests.push(request); });
    await run("photos", "assets", "display", oldDisplay.asset_id, newFile);
    await page.getByRole("button", {name:"View original", exact:true}).click(); await expect(page.getByRole("button", {name:"Reload photo"})).toBeVisible();
    await expect(page.locator(".caption strong")).toHaveText(oldDisplay.name); await expect(page.locator(".photo-viewport img")).toHaveAttribute("src", shownURL!);
    expect(originalRequests).toHaveLength(0); await expect(page).toHaveURL(shownLocation);
    await page.screenshot({path:path.join(output!, "web-photo-loupe-display-change-400-dark.png"), animations:"disabled"});
    await page.getByRole("button", {name:"Reload photo"}).click(); await expect(page.locator(".caption strong")).toHaveText(newDisplay.name);
    await expect(page.getByText("Fit preview", {exact:true})).toBeVisible({timeout:60_000}); await page.getByRole("button", {name:"View original", exact:true}).click();
    await expect(page.getByText("Original", {exact:true})).toBeVisible({timeout:60_000});
    expect(originalRequests.find(request => request.method() === "POST")!.postDataJSON().node_id).toBe(newDisplay.node_id);
    await page.keyboard.press("Escape"); await expect(displayCell).toBeFocused(); await expect(page.getByText("1 selected photo", {exact:true})).toBeVisible();
    expect(errors).toEqual([]);
  } finally {
    if (process.env.DOCBANK_KEEP_PHOTO_PREVIEW) {
      const previewURL = new URL(await run("web", "--no-browser")); previewURL.pathname = "/photos"; previewURL.hash += `&photo=${assetID}`;
      await writeFile(path.join(output!, "loupe-preview.json"), JSON.stringify({ url: previewURL.href, workspace, binary }, null, 2));
    } else { await run("daemon", "stop"); await rm(workspace, { recursive: true, force: true }); }
  }
});
