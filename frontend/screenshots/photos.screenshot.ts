import { revokeWebSession } from "../src/generated/docbank.js";
import { expect, test, type Page } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";
import { listPhotoAssets } from "../src/generated/docbank.js";
import { output, withPhotoFixture } from "./photo-workspace-fixture.js";
import { photoFixture, repository } from "./photo-fixture.js";

const exec = promisify(execFile);
const binary =
  process.env.DOCBANK_SCREENSHOT_BINARY ??
  path.join(repository, "bin", process.platform === "win32" ? "docbank.exe" : "docbank");

test.skip(!output, "DOCBANK_PHOTOS_SCREENSHOT_DIR enables synthetic photo proof");

test("paging keeps the bottom of the photo grid stable while loading", async ({ page }) => {
  await withPhotoFixture(300, "paging", async (run) => {
    let release!: () => void;
    let entered!: () => void;
    const pending = new Promise<void>((resolve) => (entered = resolve));
    const hold = new Promise<void>((resolve) => (release = resolve));
    await page.route("**/api/v1/photos/assets/query", async (route) => {
      if (route.request().postDataJSON().cursor) {
        entered();
        await hold;
      }
      await route.continue();
    });
    try {
      const url = new URL(await run("web", "--no-browser"));
      url.pathname = "/photos";
      await page.goto(url.href);
      await expect(page.getByRole("button", { name: "Load more", exact: true })).toBeVisible();
      const scroll = page.getByTestId("photo-scroll");
      const before = await scroll.evaluate((element) => {
        element.scrollTop = element.scrollHeight;
        return { top: element.scrollTop, height: element.clientHeight };
      });
      expect(before.top).toBeGreaterThan(0);
      await pending;
      await expect(page.getByText("Loading photos…", { exact: true })).toBeVisible();
      expect(
        await scroll.evaluate((element) => ({
          top: element.scrollTop,
          height: element.clientHeight,
        })),
      ).toEqual(before);
    } finally {
      release();
      await page.unrouteAll({ behavior: "wait" });
    }
    await expect(page.getByText("Loading photos…", { exact: true })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Load more", exact: true })).toHaveCount(0);
  });
});

test("photo inspector keeps Apply reachable at 400×480", async ({ page }) => {
  test.setTimeout(300_000);
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "metadata-search-proof-"));
  const fixture = photoFixture(workspace);
  try {
    await fixture.seed(12, "-pair");
    const url = new URL(await fixture.run("web", "--no-browser"));
    url.pathname = "/photos";
    await page.goto(url.href);
    await page.getByRole("searchbox", { name: "Search photos", exact: true }).fill("Synthetic");
    await page.getByRole("searchbox", { name: "Search photos", exact: true }).press("Enter");
    await page
      .getByRole("button", { name: "Select Synthetic-photo-00001.jpg", exact: true })
      .click();
    await page.getByRole("button", { name: "Select loaded photos", exact: true }).click();
    await page.getByRole("button", { name: "Select all 12 photos", exact: true }).click();
    await page.getByRole("button", { name: "Metadata", exact: true }).click();
    const inspector = page.getByRole("region", { name: "Photo metadata" });
    await mkdir(output!, { recursive: true });
    await page.setViewportSize({ width: 400, height: 480 });
    const panel = page.locator(".grid-inspector");
    await expect
      .poll(() =>
        panel.evaluate(
          (element) => element.scrollHeight > element.clientHeight && element.clientHeight > 0,
        ),
      )
      .toBe(true);
    const apply = inspector.getByRole("button", { name: "Apply", exact: true });
    await apply.scrollIntoViewIfNeeded();
    expect(
      await apply.evaluate((element) => {
        const button = element.getBoundingClientRect(),
          panel = element.closest(".grid-inspector")!.getBoundingClientRect();
        return (
          button.top >= panel.top && button.bottom <= panel.bottom && button.bottom <= innerHeight
        );
      }),
    ).toBe(true);
    await page.screenshot({
      path: path.join(output!, "web-photo-inspector-400x480-apply.png"),
      animations: "disabled",
    });
    await panel.evaluate((element) => {
      element.scrollTop = 0;
      element.scrollIntoView({ block: "start" });
    });
    await expect(inspector.getByRole("combobox", { name: /^Rating:/ })).toBeInViewport();
    await expect(inspector.getByRole("combobox", { name: /^Flag:/ })).toBeInViewport();
    for (const theme of ["light", "dark"]) {
      await page.evaluate((value) => {
        localStorage.setItem("docbank-theme", value);
        document.documentElement.classList.toggle("dark", value === "dark");
      }, theme);
      await page.screenshot({
        path: path.join(output!, `web-photo-inspector-400x480-${theme}.png`),
        animations: "disabled",
      });
    }
  } finally {
    await fixture.dispose();
  }
});

test("Hidden photos lock, unlock, unhide, expire, and discard previews", async ({ page }) => {
  test.setTimeout(480_000);
  page.setDefaultTimeout(15_000);
  await page.setViewportSize({ width: 1440, height: 720 });
  await withPhotoFixture(12, "hidden", async (run) => {
    const webURL = new URL(await run("web", "--no-browser"));
    webURL.pathname = "/photos/hidden";
    await page.goto(webURL.href);
    await page.getByLabel("Passcode", { exact: true }).fill("synthetic-passcode");
    await page.getByRole("button", { name: "Set passcode", exact: true }).click();
    await expect(page.getByText(/Locks in/)).toBeVisible();
    await page.getByRole("button", { name: "Library", exact: true }).click();
    await expect(page.locator("[data-asset]")).toHaveCount(12);
    await expect(page.locator("[data-asset] img").first()).toBeVisible();
    const id = (await page.locator("[data-asset]").first().getAttribute("data-asset"))!;
    let releaseHide!: () => void;
    let enteredHide!: () => void;
    const hidePending = new Promise<void>((resolve) => (enteredHide = resolve));
    const hideHold = new Promise<void>((resolve) => (releaseHide = resolve));
    await page.route(
      `**/api/v1/photos/assets/${id}/hide`,
      async (route) => {
        enteredHide();
        await hideHold;
        await route.continue();
      },
      { times: 1 },
    );
    await page
      .locator(`[data-asset="${id}"]`)
      .getByRole("button", { name: /^Select / })
      .click({ button: "right" });
    await page.getByRole("menuitem", { name: "Hide", exact: true }).click();
    await hidePending;
    await page.getByRole("button", { name: "Hidden", exact: true }).click();
    await expect(page.getByText(/Locks in/)).toHaveCount(0);
    await expect(page.locator("[data-asset]")).toHaveCount(0);
    releaseHide();
    await expect(page.getByText(/Locks in/)).toBeVisible();
    await expect(page.locator(`[data-asset="${id}"] img`)).toBeVisible();
    await page.getByRole("button", { name: "Library", exact: true }).click();
    await expect(page.locator(`[data-asset="${id}"]`)).toHaveCount(0);
    await expect(page.locator("[data-asset]")).toHaveCount(11);
    await expect(page.locator("[data-asset] img").first()).toBeVisible();
    await page.getByRole("button", { name: "Hidden", exact: true }).click();
    await expect(page.locator(`[data-asset="${id}"] img`)).toBeVisible();
    await page.route("**/api/v1/photos/hidden", (route) => route.abort("failed"), { times: 1 });
    await expect(page.getByRole("button", { name: "Retry", exact: true })).toBeVisible({
      timeout: 6000,
    });
    await expect(page.locator("[data-asset]")).toHaveCount(0);
    await expect(page.locator(`[data-asset="${id}"] img`)).toBeVisible({ timeout: 6000 });
    await expect(page.getByRole("button", { name: "Retry", exact: true })).toHaveCount(0);
    const leave = async () =>
      page.evaluate(() => {
        Object.defineProperty(document, "visibilityState", { configurable: true, value: "hidden" });
        document.dispatchEvent(new Event("visibilitychange"));
      });
    const returnToPage = async () =>
      page.evaluate(() => {
        Object.defineProperty(document, "visibilityState", {
          configurable: true,
          value: "visible",
        });
        document.dispatchEvent(new Event("visibilitychange"));
      });
    const locked = page.waitForResponse(
      (response) => response.url().endsWith("/photos/hidden/lock") && response.ok(),
    );
    await leave();
    await expect(page.locator("[data-asset]")).toHaveCount(0);
    await locked;
    await returnToPage();
    await expect(page.getByRole("button", { name: "Unlock", exact: true })).toBeVisible();
    await expect(page.locator("[data-asset]")).toHaveCount(0);
    await expect(page.getByLabel("Passcode", { exact: true })).toBeEnabled();
    for (const theme of ["light", "dark"]) {
      await page.evaluate((value) => {
        localStorage.setItem("docbank-theme", value);
        document.documentElement.classList.toggle("dark", value === "dark");
      }, theme);
      await page.screenshot({
        path: path.join(output!, `web-hidden-locked-${theme}.png`),
        animations: "disabled",
      });
    }
    await page.getByLabel("Passcode", { exact: true }).fill("synthetic-passcode");
    await page.getByRole("button", { name: "Unlock", exact: true }).click();
    await expect(page.locator(`[data-asset="${id}"] img`)).toBeVisible();
    for (const theme of ["light", "dark"]) {
      await page.evaluate((value) => {
        localStorage.setItem("docbank-theme", value);
        document.documentElement.classList.toggle("dark", value === "dark");
      }, theme);
      await page.screenshot({
        path: path.join(output!, `web-hidden-unlocked-${theme}.png`),
        animations: "disabled",
      });
    }
    await page.route("**/photos/hidden/lock", (route) => route.abort("failed"), { times: 1 });
    await leave();
    await expect(page.getByRole("button", { name: "Retry lock", exact: true })).toBeVisible();
    await returnToPage();
    await expect(page.locator("[data-asset]")).toHaveCount(0);
    await expect(page.getByLabel("Passcode", { exact: true })).toHaveCount(0);
    await page.screenshot({
      path: path.join(output!, "web-hidden-lock-failure-dark.png"),
      animations: "disabled",
    });
    await page.getByRole("button", { name: "Retry lock", exact: true }).click();
    await expect(page.getByRole("button", { name: "Retry lock", exact: true })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Unlock", exact: true })).toBeVisible({
      timeout: 5000,
    });
    await page.getByLabel("Passcode", { exact: true }).fill("synthetic-passcode");
    await page.getByRole("button", { name: "Unlock", exact: true }).click();
    await expect(page.locator(`[data-asset="${id}"] img`)).toBeVisible();
    await page.route(
      `**/api/v1/photos/assets/${id}/unhide`,
      (route) =>
        route.fulfill({
          status: 412,
          contentType: "application/problem+json",
          body: JSON.stringify({
            detail: "Synthetic stale photo revision",
            code: "stale_revision",
          }),
        }),
      { times: 1 },
    );
    await page
      .locator(`[data-asset="${id}"]`)
      .getByRole("button", { name: /^Actions for / })
      .click();
    await page.getByRole("menuitem", { name: "Unhide", exact: true }).click();
    await expect(page.getByRole("alert")).toHaveText(
      "1 photo failed: Synthetic stale photo revision",
    );
    await expect(page.locator(`[data-asset="${id}"] img`)).toBeVisible();
    await page.waitForResponse(
      (response) =>
        response.url().endsWith("/api/v1/photos/hidden") && response.request().method() === "GET",
    );
    await expect(page.getByRole("alert")).toHaveText(
      "1 photo failed: Synthetic stale photo revision",
    );
    await expect(page.locator(`[data-asset="${id}"] img`)).toBeVisible();
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    await page.screenshot({
      path: path.join(output!, "web-hidden-unhide-failure-dark.png"),
      animations: "disabled",
    });
    await page
      .locator(`[data-asset="${id}"]`)
      .getByRole("button", { name: /^Actions for / })
      .click();
    await page.getByRole("menuitem", { name: "Unhide", exact: true }).click();
    await expect(page.getByRole("alert")).toHaveCount(0);
    await expect(page.locator(`[data-asset="${id}"]`)).toHaveCount(0);
    await page.getByRole("button", { name: "Library", exact: true }).click();
    await expect(page.locator(`[data-asset="${id}"] img`)).toBeVisible();
    await page
      .locator(`[data-asset="${id}"]`)
      .getByRole("button", { name: /^Actions for / })
      .click();
    await page.getByRole("menuitem", { name: "Hide", exact: true }).click();
    await page.getByRole("button", { name: "Hidden", exact: true }).click();
    await expect(page.locator(`[data-asset="${id}"] img`)).toBeVisible();
    await run("photos", "hidden", "lock");
    await expect(page.getByRole("button", { name: "Unlock", exact: true })).toBeVisible({
      timeout: 5000,
    });
    await expect(page.locator("[data-asset]")).toHaveCount(0);
    await page.getByLabel("Passcode", { exact: true }).fill("synthetic-passcode");
    await page.getByRole("button", { name: "Unlock", exact: true }).click();
    await expect(page.locator(`[data-asset="${id}"] img`)).toBeVisible();
    await page.clock.install();
    await page.clock.fastForward(301_000);
    await expect(page.getByRole("button", { name: "Unlock", exact: true })).toBeVisible();
    await expect(page.locator("[data-asset]")).toHaveCount(0);
    await page.clock.setSystemTime(Date.now());
    await page.getByLabel("Passcode", { exact: true }).fill("synthetic-passcode");
    await page.getByRole("button", { name: "Unlock", exact: true }).click();
    await expect(page.locator(`[data-asset="${id}"] img`)).toBeVisible();
    const tab = await page.context().newPage();
    await tab.goto(webURL.href);
    await expect(tab.locator(`[data-asset="${id}"] img`)).toBeVisible();
    await tab.getByRole("button", { name: "Lock", exact: true }).click();
    await expect(page.getByRole("button", { name: "Unlock", exact: true })).toBeVisible();
    await expect(page.locator("[data-asset]")).toHaveCount(0);
    await tab.close();
    const token = new URLSearchParams(webURL.hash.slice(1)).get("web_session")!;
    const fetcher = globalThis.fetch;
    try {
      globalThis.fetch = async (input, init) => {
        const address = new URL(String(input), webURL.origin);
        const host = address.host;
        address.hostname = "127.0.0.1";
        const headers = new Headers(init?.headers);
        headers.set("Host", host);
        return fetcher(address, { ...init, headers });
      };
      await revokeWebSession(undefined, { session: token });
    } finally {
      globalThis.fetch = fetcher;
    }
    await expect(
      page.getByText("The browser session expired or was rejected. Run `docbank web` again."),
    ).toBeVisible({ timeout: 6000 });
    await expect(page.locator("[data-asset]")).toHaveCount(0);
  });
});

test("timeline seeks an unloaded leap day and preserves full-scope counts", async ({ page }) => {
  test.setTimeout(600_000);
  await withPhotoFixture(10000, "timeline", async (run) => {
    const webURL = new URL(await run("web", "--no-browser"));
    webURL.pathname = "/photos";
    await page.goto(webURL.href);
    await expect(page.getByText(/10,000 photos/)).toBeVisible();
    await page.getByRole("button", { name: "Timeline", exact: true }).click();
    await expect(page.getByText(/10,000\s+photos in scope/)).toBeVisible({ timeout: 35_000 });
    await expect(
      page
        .getByRole("navigation", { name: "Timeline years" })
        .getByRole("button", { name: /^2018/ }),
    ).toBeVisible();
    const views = page.getByRole("navigation", { name: "Photo views" });
    const gridBounds = await views.getByRole("button", { name: "Grid", exact: true }).boundingBox();
    const timelineBounds = await views
      .getByRole("button", { name: "Timeline", exact: true })
      .boundingBox();
    expect(timelineBounds!.x - gridBounds!.x - gridBounds!.width).toBeGreaterThan(0);
    const heading = page.locator(".photo-bar .toolbar-actions > span");
    const years = page.getByRole("navigation", { name: "Timeline years" });
    await years.getByRole("button", { name: /^2026/ }).click();
    await expect(heading).toHaveText("108 photos in 2026");
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    const scroll = page.getByTestId("photo-scroll");
    await scroll.getByRole("checkbox").first().check();
    await scroll.evaluate((element) => (element.scrollTop = 400));
    await expect.poll(() => scroll.evaluate((element) => element.scrollTop)).toBe(400);
    await page.route("**/api/v1/photos/assets/query", (route) => route.abort(), { times: 1 });
    await years.getByRole("button", { name: /^2025/ }).click();
    await expect(page.locator(".photo-error")).toBeVisible();
    await expect(heading).toHaveText("108 photos in 2026");
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    await expect(page.getByText("1 selected photo", { exact: true })).toBeVisible();
    await expect.poll(() => scroll.evaluate((element) => element.scrollTop)).toBe(400);
    await page.getByRole("button", { name: "Retry", exact: true }).click();
    await expect(heading).toHaveText("106 photos in 2025");
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    await expect(page.getByText("1 selected photo", { exact: true })).toHaveCount(0);
    await expect.poll(() => scroll.evaluate((element) => element.scrollTop)).toBe(0);
    await page.getByRole("button", { name: "December 2025", exact: false }).click();
    await expect(heading).toHaveText("27 photos in December 2025");
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Load more", exact: true })).toHaveCount(0);
    await expect(page.getByTestId("photo-scroll").locator("img").first()).toBeVisible();
    for (const theme of ["light", "dark"]) {
      await page.evaluate((value) => {
        localStorage.setItem("docbank-theme", value);
        document.documentElement.classList.toggle("dark", value === "dark");
      }, theme);
      await page.screenshot({
        path: path.join(output!, `web-timeline-${theme}.png`),
        clip: { x: 0, y: 0, width: 1440, height: 640 },
        animations: "disabled",
      });
    }
    await page.getByRole("button", { name: /^2025-12-15 ·/ }).click();
    await expect(heading).toHaveText("27 photos on 2025-12-15");
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    await expect(page.getByText(/10,000\s+photos in scope/)).toBeVisible();
    await expect(page.getByTestId("photo-scroll").locator("img").first()).toBeVisible();
    await expect
      .poll(() => page.getByTestId("photo-scroll").locator("img").count())
      .toBe(await page.locator("[data-asset]").count());
    for (const theme of ["light", "dark"]) {
      await page.evaluate((value) => {
        localStorage.setItem("docbank-theme", value);
        document.documentElement.classList.toggle("dark", value === "dark");
      }, theme);
      await page.screenshot({
        path: path.join(output!, `web-timeline-day-${theme}.png`),
        clip: { x: 0, y: 0, width: 1440, height: 640 },
        animations: "disabled",
      });
    }
    await years.getByRole("button", { name: /^2024/ }).click();
    await page.getByRole("button", { name: "February 2024", exact: false }).click();
    await expect(heading).toHaveText("27 photos in February 2024");
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    await page.getByRole("button", { name: /^2024-02-29 ·/ }).click();
    await expect(heading).toHaveText("27 photos on 2024-02-29");
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    await expect(page.getByText(/10,000\s+photos in scope/)).toBeVisible();
    await page.getByRole("button", { name: "Clear date", exact: true }).click();
    await expect(heading).toHaveText("10,000 photos");
    await page.getByRole("button", { name: "Grid", exact: true }).click();
    const excludedID = await page.locator("[data-asset]").first().getAttribute("data-asset");
    await run("photos", "assets", "exclude", excludedID!, "--revision", "1");
    await page.getByRole("button", { name: "More photo actions" }).click();
    await page.getByRole("menuitem", { name: "Refresh previews" }).click();
    await expect(heading).toHaveText("9,999 photos");
    await page.getByRole("button", { name: "Timeline", exact: true }).click();
    await expect(page.getByText(/9,999\s+photos in scope/)).toBeVisible({ timeout: 35_000 });
    await page.getByRole("button", { name: "Grid", exact: true }).click();
    await run("photos", "assets", "exclude", excludedID!, "--revision", "2", "--excluded=false");
    await page.getByRole("button", { name: "More photo actions" }).click();
    await page.getByRole("menuitem", { name: "Refresh previews" }).click();
    await expect(heading).toHaveText("10,000 photos");
    await page.getByRole("button", { name: "Timeline", exact: true }).click();
    await expect(page.getByText(/10,000\s+photos in scope/)).toBeVisible({ timeout: 35_000 });
    await page
      .getByRole("navigation", { name: "Timeline years" })
      .getByRole("button", { name: /^2022/ })
      .click();
    await expect(heading).toHaveText("9,104 photos in 2022");
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    await page.getByRole("button", { name: "Load more", exact: true }).click();
    await expect(heading).toHaveText("9,104 photos in 2022");
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    await page.getByRole("button", { name: "June 2022", exact: false }).click();
    await expect(heading).toHaveText("9,000 photos in June 2022");
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    await page.getByRole("button", { name: "Load more", exact: true }).click();
    await expect(heading).toHaveText("9,000 photos in June 2022");
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    await page.getByRole("button", { name: /^2022-06-15 ·/ }).click();
    await expect(page.getByText(/9,0\d\d photos on 2022-06-15/)).toBeVisible();
    await page.getByRole("button", { name: "Load more", exact: true }).click();
    await expect(page.getByText("500 loaded", { exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Clear date", exact: true }).click();
    await expect(heading).toHaveText("10,000 photos");
  });
});

test("10,000 photos stay windowed, retain previews and selection, and remember density", async ({
  page,
}) => {
  test.setTimeout(900_000);
  const workspace =
    process.env.DOCBANK_PHOTO_PROOF_WORKSPACE ??
    (await mkdtemp(path.join(repository, ".superpowers", "photos-proof-")));
  const { run, seed, dispose } = photoFixture(workspace);
  const requests = new Map<string, number>();
  let listings = 0;
  let loadedRows = new Set<string>();
  page.on("response", async (response) => {
    if (!response.url().endsWith("/photos/assets/query") || !response.ok()) return;
    const request = response.request().postDataJSON();
    if (request.facets?.length) return;
    const body = await response.json().catch(() => undefined);
    if (!body?.items) return;
    if (!request.cursor) loadedRows = new Set();
    for (const row of body.items) loadedRows.add(row.asset_id);
  });
  page.on("request", (request) => {
    if (request.url().includes("/photos/assets/query")) listings++;
    if (request.url().includes("/previews/"))
      requests.set(request.url(), (requests.get(request.url()) ?? 0) + 1);
  });
  try {
    await page.addInitScript(() => {
      const active = new Set<string>();
      Object.assign(window, { photoActiveURLs: active });
      const create = URL.createObjectURL.bind(URL);
      const revoke = URL.revokeObjectURL.bind(URL);
      URL.createObjectURL = (blob) => {
        const url = create(blob);
        active.add(url);
        return url;
      };
      URL.revokeObjectURL = (url) => {
        active.delete(url);
        revoke(url);
      };
    });
    if (!process.env.DOCBANK_PHOTO_PROOF_WORKSPACE) await seed();
    const webURL = new URL(await run("web", "--no-browser"));
    webURL.pathname = "/photos";
    let releaseTransition!: () => void;
    let enteredTransition!: () => void;
    const transitionPending = new Promise<void>((resolve) => (enteredTransition = resolve));
    const transitionHold = new Promise<void>((resolve) => (releaseTransition = resolve));
    await page.route("**/api/v1/photos/assets/query", async (route) => {
      const body = route.request().postDataJSON();
      if (body.facets?.length) {
        await route.continue();
        return;
      }
      body.page_size = 100;
      const url = new URL(route.request().url());
      const host = url.host;
      url.hostname = "127.0.0.1";
      const response = await route.fetch({
        url: url.href,
        headers: { ...(await route.request().allHeaders()), host },
        postData: JSON.stringify(body),
      });
      if (body.cursor) {
        enteredTransition();
        await transitionHold;
      }
      await route.fulfill({ response });
    });
    await page.goto(webURL.href);
    await expect(page.getByText("10,000 photos", { exact: true })).toBeVisible();
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    await expect.poll(() => loadedRows.size).toBe(100);
    await expect(page.getByRole("navigation", { name: "Photo years" })).toHaveCount(0);
    await page.getByRole("button", { name: "Load more", exact: true }).click();
    await transitionPending;
    const transitionScroll = page.getByTestId("photo-scroll");
    await transitionScroll.evaluate((element) => (element.scrollTop = 500));
    await page.evaluate(
      () => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))),
    );
    const transitionAnchor = await transitionScroll.evaluate((element) => {
      const top = element.getBoundingClientRect().top;
      const cell = [...element.querySelectorAll<HTMLElement>("[data-asset]")].find(
        (item) => item.getBoundingClientRect().bottom > top + 56,
      )!;
      return { id: cell.dataset.asset!, offset: cell.getBoundingClientRect().top - top };
    });
    releaseTransition();
    await expect(page.getByText("10,000 photos", { exact: true })).toBeVisible();
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    await expect.poll(() => loadedRows.size).toBe(200);
    await expect(
      page.getByRole("navigation", { name: "Photo years" }).getByRole("button"),
    ).toHaveCount(2);
    await page.evaluate(
      () => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))),
    );
    expect(
      await page
        .locator(`[data-asset="${transitionAnchor.id}"]`)
        .evaluate(
          (element) =>
            element.getBoundingClientRect().top -
            element.closest(".photo-scroll")!.getBoundingClientRect().top,
        ),
    ).toBeCloseTo(transitionAnchor.offset, 0);
    await page.unroute("**/api/v1/photos/assets/query");
    await page.goto("about:blank");
    await page.goto(webURL.href);
    await expect(page.getByRole("main", { name: "Photo library" })).toBeVisible();
    await expect(page.getByText(/10,000 photos/)).toBeVisible();
    const scroll = page.getByTestId("photo-scroll");
    await expect(scroll.locator("img").first()).toBeVisible();
    await expect(
      page.getByRole("navigation", { name: "Photo years" }).getByRole("button"),
    ).toHaveCount(3);
    const firstAsset = await page.locator("[data-asset]").first().getAttribute("data-asset");
    await page.locator("[data-asset]").first().getByRole("button").first().click();
    await page.getByRole("combobox", { name: /^Group photos/ }).click();
    await page.getByRole("option", { name: "Capture sessions", exact: true }).click();
    const requestNext = async (anchor = false) => {
      const button = page.getByRole("button", { name: "Load more", exact: true });
      await button.focus();
      await page.keyboard.press("Enter");
      await scroll.evaluate(
        (element, gap) => (element.scrollTop = element.scrollHeight - element.clientHeight - gap),
        anchor ? 400 : 0,
      );
    };
    await page.getByRole("button", { name: "Filters", exact: true }).click();
    await expect(
      page
        .getByRole("group", { name: "Camera facet" })
        .getByRole("button", { name: "Canon EOS R6, 5000 photos" }),
    ).toBeVisible();
    for (let count = 0; count < 50; count++) {
      if (loadedRows.size === 10000) break;
      const loaded = loadedRows.size;
      if (loaded === 1750 || loaded === 2750) {
        let release!: () => void;
        let entered!: () => void;
        const held = new Promise<void>((resolve) => (entered = resolve));
        const resumed = new Promise<void>((resolve) => (release = resolve));
        await page.route(
          "**/api/v1/photos/assets/query",
          async (route) => {
            const url = new URL(route.request().url());
            const host = url.host;
            url.hostname = "127.0.0.1";
            const response = await route.fetch({
              url: url.href,
              headers: { ...(await route.request().allHeaders()), host },
            });
            entered();
            await resumed;
            await route.fulfill({ response });
          },
          { times: 1 },
        );
        if (loaded === 2750) {
          await page.route("**/api/v1/photos/assets/query", (route) => route.abort("failed"), {
            times: 1,
          });
          await requestNext(true);
          await expect(page.getByRole("button", { name: "Retry", exact: true })).toBeVisible();
          await page.getByRole("button", { name: "Retry", exact: true }).click();
        } else await requestNext(true);
        await held;
        await page.evaluate(
          () =>
            new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))),
        );
        const anchor = await scroll.evaluate((element) => {
          const top = element.getBoundingClientRect().top;
          const cell = [...element.querySelectorAll<HTMLElement>("[data-asset]")].find(
            (item) => item.getBoundingClientRect().bottom > top + 56,
          )!;
          return { id: cell.dataset.asset!, offset: cell.getBoundingClientRect().top - top };
        });
        if (loaded === 1750) {
          let resumePage!: () => void;
          let requested!: () => void;
          const continued = new Promise<void>((resolve) => (resumePage = resolve));
          const pending = new Promise<void>((resolve) => (requested = resolve));
          await page.route("**/api/v1/photos/assets/query", async (route) => {
            if (!route.request().postDataJSON().facets?.length) {
              await route.continue();
              return;
            }
            requested();
            await continued;
            await route.continue();
          });
          await page.getByRole("button", { name: "Documents", exact: true }).click();
          release();
          await page.getByRole("button", { name: "Photos", exact: true }).click();
          await pending;
          await expect.poll(() => loadedRows.size).toBe(loaded);
          await expect
            .poll(() =>
              page
                .locator(`[data-asset="${anchor.id}"]`)
                .evaluate(
                  (element) =>
                    element.getBoundingClientRect().top -
                    element.closest(".photo-scroll")!.getBoundingClientRect().top,
                ),
            )
            .toBeCloseTo(anchor.offset, 0);
          await expect
            .poll(() => scroll.locator("img").count())
            .toBe(await page.locator("[data-asset]").count());
          resumePage();
          await page.unroute("**/api/v1/photos/assets/query");
        }
        release();
        await expect(page.locator(".photo-pill")).toHaveCount(0);
        const offset = await page
          .locator(`[data-asset="${anchor.id}"]`)
          .evaluate(
            (element) =>
              element.getBoundingClientRect().top -
              element.closest(".photo-scroll")!.getBoundingClientRect().top,
          );
        expect(offset).toBeCloseTo(anchor.offset, 0);
        const settled = listings;
        await page.waitForTimeout(300);
        expect(listings).toBe(settled);
      } else await requestNext();
      await expect.poll(() => loadedRows.size).not.toBe(loaded);
      await expect(page.locator(".photo-pill")).toHaveCount(0);
      expect(await page.locator("[data-asset]").count()).toBeLessThan(200);
      expect(
        await page.evaluate(
          () => (window as unknown as { photoActiveURLs: Set<string> }).photoActiveURLs.size,
        ),
      ).toBeLessThan(200);
    }
    await expect(page.getByText("10,000 photos", { exact: true })).toBeVisible();
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    await expect.poll(() => loadedRows.size).toBe(10000);
    await page.waitForLoadState("networkidle");
    await expect(page.getByRole("button", { name: "Load more", exact: true })).toHaveCount(0);
    await expect(
      page
        .getByRole("navigation", { name: "Photo years" })
        .getByRole("button", { name: "2022", exact: true }),
    ).toBeVisible();
    await page.getByRole("combobox", { name: /^Group photos/ }).click();
    await page.getByRole("option", { name: "Months", exact: true }).click();
    await scroll.evaluate((element) => (element.scrollTop = element.scrollHeight));
    const last = page.locator("[data-asset]").last().getByRole("button").first();
    await last.click({ modifiers: ["Shift"] });
    await expect
      .poll(async () =>
        Number((await page.locator(".selection-summary strong").innerText()).split(" ")[0]),
      )
      .toBeGreaterThan(9000);
    await expect(page.getByRole("button", { name: "Clear selection", exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Clear selection", exact: true }).click();
    await page
      .getByRole("navigation", { name: "Photo years" })
      .getByRole("button", { name: "2024", exact: true })
      .click();
    await expect
      .poll(() =>
        scroll.evaluate(
          (element) =>
            [...element.querySelectorAll("h2")].find(
              (header) =>
                header.getBoundingClientRect().top <= element.getBoundingClientRect().top + 1 &&
                header.getBoundingClientRect().bottom > element.getBoundingClientRect().top,
            )?.textContent,
        ),
      )
      .toContain("2024");
    await scroll.evaluate((element) => (element.scrollTop += 100));
    await expect
      .poll(() =>
        scroll.evaluate(
          (element) =>
            [...element.querySelectorAll("h2")].find(
              (header) =>
                header.getBoundingClientRect().top <= element.getBoundingClientRect().top + 1 &&
                header.getBoundingClientRect().bottom > element.getBoundingClientRect().top,
            )?.textContent,
        ),
      )
      .toContain("2024");
    const visibleID = await scroll.evaluate((element) => {
      const top = element.getBoundingClientRect().top + 56;
      return [...element.querySelectorAll<HTMLElement>("[data-asset]")].find(
        (cell) => cell.getBoundingClientRect().top >= top,
      )?.dataset.asset;
    });
    await page.locator(`[data-asset="${visibleID}"]`).getByRole("checkbox").check();
    const position = await scroll.evaluate((element) => element.scrollTop);
    let release!: () => void;
    let entered!: () => void;
    const delayed = new Promise<void>((resolve) => (entered = resolve));
    const hold = new Promise<void>((resolve) => (release = resolve));
    await page.route(
      "**/api/v1/photos/assets/query",
      async (route) => {
        entered();
        await hold;
        await route.continue();
      },
      { times: 1 },
    );
    await page.getByRole("button", { name: "More photo actions" }).click();
    await page.getByRole("menuitem", { name: "Refresh previews" }).click();
    await delayed;
    await page
      .getByRole("navigation", { name: "Photo years" })
      .getByRole("button", { name: "2022", exact: true })
      .click();
    await expect
      .poll(() =>
        scroll.evaluate(
          (element) =>
            [...element.querySelectorAll("h2")].find(
              (header) =>
                header.getBoundingClientRect().top <= element.getBoundingClientRect().top + 1 &&
                header.getBoundingClientRect().bottom > element.getBoundingClientRect().top,
            )?.textContent,
        ),
      )
      .toContain("2022");
    const latestPosition = await scroll.evaluate((element) => element.scrollTop);
    expect(latestPosition).not.toBe(position);
    release();
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    expect(await scroll.evaluate((element) => element.scrollTop)).toBeCloseTo(latestPosition, 0);
    await page.getByRole("button", { name: "Clear selection", exact: true }).click();
    await scroll.evaluate((element) => (element.scrollTop = 0));
    await expect(page.locator(`[data-asset="${firstAsset}"] img`)).toBeVisible();
    await page.waitForLoadState("networkidle");
    const retained = await page.evaluate(async () => {
      const names = (await caches.keys()).filter((name) =>
        name.startsWith("docbank-photo-previews-"),
      );
      return (
        await Promise.all(
          names.map(async (name) => (await (await caches.open(name)).keys()).map((key) => key.url)),
        )
      ).flat();
    });
    const seen = new Map([...requests].filter(([url]) => retained.includes(url)));
    await scroll.evaluate((element) => (element.scrollTop = 15_000));
    await page.waitForLoadState("networkidle");
    await scroll.evaluate((element) => (element.scrollTop = 0));
    await expect(page.locator(`[data-asset="${firstAsset}"] img`)).toBeVisible();
    await page.waitForLoadState("networkidle");
    for (const [url, count] of seen) expect(requests.get(url)).toBe(count);
    expect(
      await page.evaluate(
        () => (window as unknown as { photoActiveURLs: Set<string> }).photoActiveURLs.size,
      ),
    ).toBeLessThan(200);
    await page.getByRole("combobox", { name: /^Grid density/ }).click();
    await page.getByRole("option", { name: "Compact", exact: true }).click();
    await page
      .getByRole("navigation", { name: "Photo years" })
      .getByRole("button", { name: "2024", exact: true })
      .click();
    for (const width of [1440, 400]) {
      await page.setViewportSize({ width, height: 960 });
      await page
        .getByRole("navigation", { name: "Photo years" })
        .getByRole("button", { name: "2024", exact: true })
        .click();
      await page.waitForLoadState("networkidle");
      for (const theme of ["light", "dark"]) {
        await page.evaluate((value) => {
          localStorage.setItem("docbank-theme", value);
          document.documentElement.classList.toggle("dark", value === "dark");
        }, theme);
        await page.screenshot({
          path: path.join(output!, `web-photos-${width}-${theme}-loaded.png`),
          animations: "disabled",
          timeout: 15_000,
        });
      }
    }
    const fresh = new URL(await run("web", "--no-browser"));
    fresh.pathname = "/photos";
    await page.setViewportSize({ width: 1440, height: 960 });
    await page.goto("about:blank");
    await page.goto(fresh.href);
    await expect(page.getByRole("combobox", { name: /^Grid density/ })).toContainText("Compact");
    await expect(scroll.locator("img").first()).toBeVisible();
    await expect(
      page.getByRole("navigation", { name: "Photo years" }).getByRole("button"),
    ).toHaveCount(3);
    for (const width of [1440, 400]) {
      await page.setViewportSize({ width, height: 960 });
      await page.waitForLoadState("networkidle");
      for (const theme of ["light", "dark"]) {
        await page.evaluate((value) => {
          localStorage.setItem("docbank-theme", value);
          document.documentElement.classList.toggle("dark", value === "dark");
        }, theme);
        await page.screenshot({
          path: path.join(output!, `web-photos-${width}-${theme}.png`),
          animations: "disabled",
        });
      }
    }
  } finally {
    if (process.env.DOCBANK_KEEP_PHOTO_PREVIEW) {
      const previewURL = new URL(await run("web", "--no-browser"));
      previewURL.pathname = "/photos";
      await writeFile(
        path.join(output!, "photos-preview.json"),
        JSON.stringify({ url: previewURL.href, workspace }, null, 2),
      );
    } else if (!process.env.DOCBANK_PHOTO_PROOF_WORKSPACE) {
      await dispose();
    }
  }
});

test("photo loupe keeps painted geometry and crop through delayed resolution upgrades", async ({
  page,
}) => {
  test.setTimeout(300_000);
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "loupe-geometry-"));
  const env = {
    ...process.env,
    DOCBANK_HOME: path.join(workspace, "vault"),
    DOCBANK_LOCK_DIR: path.join(workspace, "locks"),
  };
  const run = async (...args: string[]) =>
    (await exec(binary, args, { cwd: repository, env, timeout: 60_000 })).stdout.trim();
  const evidence = [];
  let previewURL = "";
  try {
    await mkdir(output!, { recursive: true });
    await exec(
      "go",
      ["run", "-tags", "fts5", "./frontend/screenshots/photos-fixture.go", env.DOCBANK_HOME, "12"],
      { cwd: repository, env, timeout: 240_000 },
    );
    await page.addInitScript(() => Object.defineProperty(window, "caches", { value: undefined }));
    for (const width of [1440, 400]) {
      await page.setViewportSize({ width, height: 960 });
      const url = new URL(await run("web", "--no-browser"));
      url.pathname = "/photos";
      await page.goto("about:blank");
      const listing = page.waitForResponse(
        (response) => response.url().endsWith("/photos/assets/query") && response.ok(),
      );
      await page.goto(url.href);
      const rows = (await (await listing).json()).items;
      const row = rows.find(
        (item: { name: string }) =>
          item.name === `Synthetic-photo-${width === 1440 ? "00002" : "00008"}.jpg`,
      );
      let releaseFit!: () => void, releaseLarge!: () => void, releaseOriginal!: () => void;
      const fitHold = new Promise<void>((resolve) => (releaseFit = resolve)),
        largeHold = new Promise<void>((resolve) => (releaseLarge = resolve)),
        originalHold = new Promise<void>((resolve) => (releaseOriginal = resolve));
      const preparePattern = `**/api/v1/photos/assets/${row.asset_id}/preview`;
      await page.route(preparePattern, async (route) => {
        await (route.request().postDataJSON().size === "fit" ? fitHold : largeHold);
        await route.continue();
      });
      await page.route("**/api/daemon/web-download**", async (route) => {
        await originalHold;
        await route.continue();
      });
      await page.locator(`[data-asset="${row.asset_id}"] .photo-image`).press("Enter");
      const viewport = page.locator(".photo-viewport"),
        image = viewport.locator("img");
      await expect(page.getByText("Grid preview", { exact: true })).toBeVisible();
      const measure = async (tier: string) => {
        await image.evaluate(async (element) => {
          await (element as HTMLImageElement).decode();
        });
        const geometry = await image.evaluate((element) => {
          const image = element as HTMLImageElement,
            box = image.getBoundingClientRect(),
            viewport = image.closest(".photo-viewport")!.getBoundingClientRect();
          const scale = Math.min(box.width / image.naturalWidth, box.height / image.naturalHeight);
          const paint = {
            x: box.x + (box.width - image.naturalWidth * scale) / 2,
            y: box.y + (box.height - image.naturalHeight * scale) / 2,
            width: image.naturalWidth * scale,
            height: image.naturalHeight * scale,
          };
          return {
            natural: { width: image.naturalWidth, height: image.naturalHeight },
            element: { x: box.x, y: box.y, width: box.width, height: box.height },
            paint,
            aspect: paint.width / paint.height,
            crop: [
              Math.max(0, (viewport.left - paint.x) / paint.width),
              Math.max(0, (viewport.top - paint.y) / paint.height),
              Math.min(1, (viewport.right - paint.x) / paint.width),
              Math.min(1, (viewport.bottom - paint.y) / paint.height),
            ],
            transform: (image.parentElement as HTMLElement).style.transform,
          };
        });
        const screenshot = await viewport.screenshot({
          path: path.join(output!, `web-photo-geometry-${width}-${tier}.png`),
          animations: "disabled",
        });
        const pixels = await page.evaluate(async (base64) => {
          const bitmap = await createImageBitmap(
            new Blob([Uint8Array.from(atob(base64), (character) => character.charCodeAt(0))], {
              type: "image/png",
            }),
          );
          const canvas = document.createElement("canvas");
          canvas.width = bitmap.width;
          canvas.height = bitmap.height;
          const context = canvas.getContext("2d")!;
          context.drawImage(bitmap, 0, 0);
          bitmap.close();
          const { data } = context.getImageData(0, 0, canvas.width, canvas.height);
          let left = canvas.width,
            top = canvas.height,
            right = -1,
            bottom = -1;
          const columns = new Array<number>(canvas.width).fill(0),
            rows = new Array<number>(canvas.height).fill(0);
          const samples = [];
          for (let y = 0; y < canvas.height; y++)
            for (let x = 0; x < canvas.width; x++) {
              const i = (y * canvas.width + x) * 4,
                r = data[i],
                g = data[i + 1],
                b = data[i + 2];
              if (b > r + 45 && g > r + 35) {
                columns[x]++;
                rows[y]++;
              }
              if (x % 8 === 4 && y % 8 === 4) samples.push(r, g, b);
            }
          for (let x = 0; x < columns.length; x++)
            if (columns[x] > 100) {
              left = Math.min(left, x);
              right = Math.max(right, x);
            }
          for (let y = 0; y < rows.length; y++)
            if (rows[y] > 100) {
              top = Math.min(top, y);
              bottom = Math.max(bottom, y);
            }
          return {
            bounds: { x: left, y: top, width: right - left + 1, height: bottom - top + 1 },
            samples,
          };
        }, screenshot.toString("base64"));
        return { tier, ...geometry, ...pixels };
      };
      const resting = await measure("grid-rest");
      expect(resting.bounds.width / resting.bounds.height).toBeCloseTo(resting.aspect, 2);
      await viewport.dblclick();
      const box = (await viewport.boundingBox())!;
      await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
      await page.mouse.down();
      await page.mouse.move(box.x + box.width / 2 + 32, box.y + box.height / 2 + 19);
      await page.mouse.up();
      const grid = await measure("grid");
      const stages = [resting, grid];
      for (const tier of ["fit", "large", "original-pending", "original"]) {
        if (tier === "fit") releaseFit();
        else if (tier === "large") releaseLarge();
        else if (tier === "original-pending") {
          const requested = page.waitForRequest((request) =>
            request.url().includes("/api/daemon/web-download"),
          );
          await page.getByRole("button", { name: "View original", exact: true }).click();
          await requested;
        } else releaseOriginal();
        await expect(
          page.getByText(
            tier === "original" ? "Original" : `${tier === "fit" ? "Fit" : "Large"} preview`,
            { exact: true },
          ),
        ).toBeVisible({ timeout: 60_000 });
        const current = await measure(tier);
        stages.push(current);
        await writeFile(
          path.join(output!, "geometryproof.json"),
          JSON.stringify(
            [...evidence, { width, stages: stages.map(({ samples, ...stage }) => stage) }],
            null,
            2,
          ),
        );
        expect(current.transform).toBe(grid.transform);
        expect(current.aspect).toBeCloseTo(grid.aspect, 2);
        for (let i = 0; i < grid.crop.length; i++)
          expect(Math.abs(current.crop[i] - grid.crop[i])).toBeLessThan(0.002);
        // The original-loading spinner can wrap the existing mobile toolbar.
        if (tier === "original-pending") continue;
        for (const key of ["x", "y", "width", "height"] as const)
          expect(Math.abs(current.element[key] - grid.element[key])).toBeLessThan(1);
        for (const key of ["x", "y", "width", "height"] as const)
          expect(Math.abs(current.bounds[key] - grid.bounds[key])).toBeLessThanOrEqual(2);
        const changed =
          current.samples.filter((value, i) => Math.abs(value - grid.samples[i]) > 20).length /
          grid.samples.length;
        expect(changed).toBeLessThan(0.04);
        Object.assign(current, { changedPixelFraction: changed });
      }
      evidence.push({ width, stages: stages.map(({ samples, ...stage }) => stage) });
      previewURL = page.url();
      await page.screenshot({
        path: path.join(output!, `web-photo-geometry-${width}-viewer.png`),
        animations: "disabled",
      });
      await page.keyboard.press("Escape");
      await page.unroute(preparePattern);
      await page.unroute("**/api/daemon/web-download**");
    }
    await writeFile(path.join(output!, "geometryproof.json"), JSON.stringify(evidence, null, 2));
  } finally {
    if (process.env.DOCBANK_KEEP_PHOTO_PREVIEW) {
      const url = new URL(await run("web", "--no-browser"));
      url.pathname = "/photos";
      if (previewURL)
        url.hash += `&photo=${new URLSearchParams(new URL(previewURL).hash.slice(1)).get("photo")}`;
      await writeFile(
        path.join(output!, "geometry-preview.json"),
        JSON.stringify({ url: url.href, workspace, binary }, null, 2),
      );
    } else {
      await run("daemon", "stop");
      await rm(workspace, { recursive: true, force: true, maxRetries: 10, retryDelay: 500 });
    }
  }
});

test("photo loupe opens, upgrades, navigates, restores focus and reloads a direct URL", async ({
  page,
  browser,
}) => {
  test.setTimeout(480_000);
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "loupe-proof-"));
  const vault = path.join(workspace, "vault");
  const env = {
    ...process.env,
    DOCBANK_HOME: vault,
    DOCBANK_LOCK_DIR: path.join(workspace, "locks"),
  };
  const run = async (...args: string[]) =>
    (await exec(binary, args, { cwd: repository, env, timeout: 60_000 })).stdout.trim();
  let assetID = "";
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  try {
    await mkdir(output!, { recursive: true });
    await exec(
      "go",
      [
        "run",
        "-tags",
        "fts5",
        "./frontend/screenshots/photos-fixture.go",
        "-unavailable",
        vault,
        "300",
      ],
      { cwd: repository, env, timeout: 240_000 },
    );
    const webURL = new URL(await run("web", "--no-browser"));
    webURL.pathname = "/photos";
    await page.goto(webURL.href);
    const scroll = page.getByTestId("photo-scroll");
    await expect(scroll.locator("img").first()).toBeVisible();
    await expect(page.getByText("300 photos", { exact: true })).toBeVisible();
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    await scroll.evaluate((element) => (element.scrollTop = 400));
    const opening = scroll
      .locator("[data-asset]")
      .filter({ has: page.locator("img") })
      .first();
    assetID = (await opening.getAttribute("data-asset"))!;
    const firstLoadedID = assetID;
    const button = scroll.locator(`[data-asset="${assetID}"] .photo-image`);
    const separate = scroll.locator("[data-asset]").nth(1);
    const separateID = (await separate.getAttribute("data-asset"))!;
    await separate.getByRole("checkbox").click();
    await button.dblclick();
    const position = await scroll.evaluate((element) => element.scrollTop);
    await expect(scroll.locator(`[data-asset="${assetID}"]`).getByRole("checkbox")).toBeChecked();
    await expect(
      scroll.locator(`[data-asset="${separateID}"]`).getByRole("checkbox"),
    ).not.toBeChecked();
    await expect(page.getByRole("dialog", { name: /^Photo viewer/ })).toBeVisible();
    await expect(page.getByText("Fit preview", { exact: true })).toBeVisible({ timeout: 60_000 });
    const viewport = page.locator(".photo-viewport");
    await viewport.dblclick();
    const transform = await page
      .locator(".pan")
      .evaluate((element) => (element as HTMLElement).style.transform);
    await expect(page.getByText("Large preview", { exact: true })).toBeVisible({ timeout: 60_000 });
    expect(
      await page.locator(".pan").evaluate((element) => (element as HTMLElement).style.transform),
    ).toBe(transform);
    await page.getByRole("button", { name: "Reset zoom", exact: true }).click();
    await expect(viewport).not.toHaveAttribute("data-zoomed");
    await page.keyboard.press("ArrowRight");
    await expect(page).not.toHaveURL(new RegExp(`photo=${assetID}`));
    await page
      .getByRole("navigation", { name: "Photos in current result" })
      .getByRole("button")
      .first()
      .click();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog", { name: /^Photo viewer/ })).toHaveCount(0);
    await expect
      .poll(() => scroll.evaluate((element) => element.scrollTop))
      .toBeCloseTo(position, 0);
    await expect(button).toBeFocused();
    await expect(page.getByText("1 selected photo", { exact: true })).toBeVisible();
    await scroll.evaluate((element) => (element.scrollTop = 1200));
    const forwardID = await scroll.evaluate(
      (element) =>
        [...element.querySelectorAll<HTMLElement>("[data-asset]")].find(
          (cell) => cell.getBoundingClientRect().top >= element.getBoundingClientRect().top,
        )!.dataset.asset!,
    );
    const forwardFocus = scroll.locator(`[data-asset="${forwardID}"] .photo-image`);
    await forwardFocus.evaluate((element) =>
      (element as HTMLElement).focus({ preventScroll: true }),
    );
    const forwardPosition = await scroll.evaluate((element) => element.scrollTop);
    await page.goForward();
    await expect(page.getByRole("dialog", { name: /^Photo viewer/ })).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog", { name: /^Photo viewer/ })).toHaveCount(0);
    await expect
      .poll(() => scroll.evaluate((element) => element.scrollTop))
      .toBeCloseTo(forwardPosition, 0);
    await expect(forwardFocus).toBeFocused();
    await scroll.locator(`[data-asset="${forwardID}"] .open-photo`).focus();
    await scroll.locator(`[data-asset="${forwardID}"] .open-photo`).press("Enter");
    await expect(page.getByRole("dialog", { name: /^Photo viewer/ })).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page.getByText("1 selected photo", { exact: true })).toBeVisible();
    await forwardFocus.press("Enter");
    await expect(page.getByRole("dialog", { name: /^Photo viewer/ })).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page.getByText("1 selected photo", { exact: true })).toBeVisible();
    await scroll.evaluate((element) => (element.scrollTop = 400));
    const aID = await scroll.evaluate(
      (element) =>
        [...element.querySelectorAll<HTMLElement>("[data-asset]")].find(
          (cell) => cell.getBoundingClientRect().top >= element.getBoundingClientRect().top,
        )!.dataset.asset!,
    );
    const aOpen = scroll.locator(`[data-asset="${aID}"] .open-photo`);
    await aOpen.focus();
    await aOpen.press("Enter");
    const aPosition = await scroll.evaluate((element) => element.scrollTop);
    await page.getByRole("button", { name: "Background jobs", exact: true }).click();
    await expect(page.getByRole("dialog", { name: "Daemon background jobs" })).toBeVisible();
    const jobsURL = page.url();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog", { name: "Daemon background jobs" })).toHaveCount(0);
    await expect(page.getByRole("dialog", { name: /^Photo viewer/ })).toBeFocused();
    await expect(page).toHaveURL(jobsURL);
    await page.getByRole("button", { name: "Documents", exact: true }).click();
    await page.goBack();
    await expect(page.getByRole("navigation", { name: "Photos in current result" })).toBeVisible();
    const returnedA = page.url();
    await page.keyboard.press("ArrowRight");
    await expect(page).not.toHaveURL(returnedA);
    await page.keyboard.press("ArrowLeft");
    await expect(page).toHaveURL(returnedA);
    await page.getByRole("button", { name: "Documents", exact: true }).click();
    await page.getByRole("button", { name: "Photos", exact: true }).click();
    await expect(page.getByRole("dialog", { name: /^Photo viewer/ })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Photos", exact: true })).toBeFocused();
    await expect
      .poll(() => scroll.evaluate((element) => element.scrollTop))
      .toBeCloseTo(aPosition, 0);
    await scroll.evaluate((element) => (element.scrollTop = 1200));
    const cID = await scroll.evaluate(
      (element) =>
        [...element.querySelectorAll<HTMLElement>("[data-asset]")].find(
          (cell) => cell.getBoundingClientRect().top >= element.getBoundingClientRect().top,
        )!.dataset.asset!,
    );
    const cOpen = scroll.locator(`[data-asset="${cID}"] .open-photo`);
    await cOpen.focus();
    await cOpen.press("Enter");
    const cPosition = await scroll.evaluate((element) => element.scrollTop);
    await page.keyboard.press("Escape");
    await expect
      .poll(() => scroll.evaluate((element) => element.scrollTop))
      .toBeCloseTo(cPosition, 0);
    await expect(cOpen).toBeFocused();
    await page.goBack();
    await expect(page).not.toHaveURL(/\/photos/);
    await page.goBack();
    await expect(page).toHaveURL(returnedA);
    await expect(page.getByRole("navigation", { name: "Photos in current result" })).toBeVisible();
    await page.keyboard.press("Escape");
    await expect
      .poll(() => scroll.evaluate((element) => element.scrollTop))
      .toBeCloseTo(aPosition, 0);
    await expect(aOpen).toBeFocused();
    let releaseContinuation!: () => void;
    let continuationRequested!: () => void;
    const continuationHold = new Promise<void>((resolve) => (releaseContinuation = resolve));
    const continuationPending = new Promise<void>((resolve) => (continuationRequested = resolve));
    await page.route(
      "**/api/v1/photos/assets/query",
      async (route) => {
        continuationRequested();
        await continuationHold;
        await route.abort("failed");
      },
      { times: 1 },
    );
    await scroll.evaluate(
      (element) => (element.scrollTop = element.scrollHeight - element.clientHeight - 400),
    );
    await continuationPending;
    await expect(page.getByText("300 photos", { exact: true })).toBeVisible();
    await expect(page.locator(".photo-pill")).toBeVisible();
    const last = scroll.locator("[data-asset]").last();
    await last.locator(".photo-image").dblclick();
    const before = await page.locator(".caption strong").innerText();
    await page.keyboard.press("ArrowRight");
    releaseContinuation();
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    const viewer = page.getByRole("dialog", { name: /^Photo viewer/ });
    await expect(viewer.getByRole("button", { name: "Retry navigation" })).toBeVisible();
    await expect(page.locator(".caption strong")).toHaveText(before);
    const failedURL = page.url(),
      gesture = await page.context().newCDPSession(page);
    const bounds = (await page.locator(".photo-viewport").boundingBox())!,
      x = bounds.x + bounds.width / 2,
      y = bounds.y + bounds.height / 2;
    await gesture.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x, y }] });
    await gesture.send("Input.dispatchTouchEvent", {
      type: "touchMove",
      touchPoints: [{ x: x - 150, y }],
    });
    await gesture.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
    await gesture.detach();
    await expect(page).toHaveURL(failedURL);
    await expect(page.locator(".pan")).toHaveCSS("transform", "matrix(1, 0, 0, 1, 0, 0)");
    await expect(page.getByText("Fit preview", { exact: true })).toBeVisible({ timeout: 60_000 });
    await expect
      .poll(() =>
        viewer
          .locator(".filmstrip img")
          .evaluateAll((images) =>
            images.every((image) => (image as HTMLImageElement).naturalWidth > 0),
          ),
      )
      .toBe(true);
    for (const theme of ["light", "dark"]) {
      await page.evaluate((value) => {
        localStorage.setItem("docbank-theme", value);
        document.documentElement.classList.toggle("dark", value === "dark");
      }, theme);
      await page.screenshot({
        path: path.join(output!, `web-photo-loupe-retry-1440-${theme}.png`),
        animations: "disabled",
      });
    }
    const continued = page.waitForResponse(
      (response) => response.url().endsWith("/photos/assets/query") && response.ok(),
    );
    await viewer.getByRole("button", { name: "Retry navigation" }).click();
    const continuation = await (await continued).json();
    const terminalID = continuation.items.find(
      (item: { name: string }) => item.name === "Synthetic-unavailable.jpg",
    ).asset_id;
    await expect(page.locator(".caption strong")).toHaveText(before);
    await page.keyboard.press("ArrowRight");
    await expect(page.locator(".caption strong")).not.toHaveText(before);
    await expect(page.locator(".photo-bar .toolbar-actions > span")).toHaveText("300 photos");
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    await page.getByRole("button", { name: "View original", exact: true }).click();
    await expect(page.getByText("Original", { exact: true })).toBeVisible({ timeout: 60_000 });
    await page.keyboard.press("i");
    await expect(
      page
        .getByRole("complementary", { name: "Photo details" })
        .getByText(/^(Canon Canon EOS R6|Nikon Nikon Z6)$/),
    ).toBeVisible();
    for (const width of [1440, 400]) {
      await page.setViewportSize({ width, height: 960 });
      for (const theme of ["light", "dark"]) {
        await page.evaluate((value) => {
          localStorage.setItem("docbank-theme", value);
          document.documentElement.classList.toggle("dark", value === "dark");
        }, theme);
        await page.screenshot({
          path: path.join(output!, `web-photo-loupe-${width}-${theme}.png`),
          animations: "disabled",
        });
      }
    }
    assetID = new URL(page.url()).hash.slice("#photo=".length);
    await page.reload();
    await expect(page.getByText("Open your Docbank", { exact: true })).toBeVisible();
    const directURL = new URL(await run("web", "--no-browser"));
    directURL.pathname = "/photos";
    directURL.hash += `&photo=${assetID}`;
    await page.goto("about:blank");
    await page.goto(directURL.href);
    await expect(page.getByText("Fit preview", { exact: true })).toBeVisible({ timeout: 60_000 });
    await expect(page).toHaveURL(new RegExp(`#photo=${assetID}$`));
    await expect(page.getByRole("navigation", { name: "Photos in current result" })).toHaveCount(0);
    await page.keyboard.press("Escape");
    await expect(page).not.toHaveURL(/#photo=/);
    let releaseListing!: () => void;
    const listingHold = new Promise<void>((resolve) => (releaseListing = resolve));
    const listingRoute = async (route: import("@playwright/test").Route) => {
      if (route.request().postDataJSON().query.filters?.asset_ids) await route.abort("failed");
      else {
        await listingHold;
        await route.continue();
      }
    };
    await page.route("**/api/v1/photos/assets/query", listingRoute);
    const lookupURL = new URL(await run("web", "--no-browser"));
    lookupURL.pathname = "/photos";
    lookupURL.hash += `&photo=${firstLoadedID}`;
    await page.goto("about:blank");
    await page.goto(lookupURL.href);
    await expect(page.locator(".photo-loupe [role=alert]")).toContainText("Failed to fetch");
    releaseListing();
    await expect(page.getByText("300 photos", { exact: true })).toBeVisible();
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    await expect(page.locator(".photo-loupe [role=alert]")).toContainText("Failed to fetch");
    await expect(page.getByText("Fit preview", { exact: true })).toHaveCount(0);
    await page.unroute("**/api/v1/photos/assets/query", listingRoute);
    await page.getByRole("button", { name: "Reload photo" }).click();
    await expect(page.getByText("Fit preview", { exact: true })).toBeVisible({ timeout: 60_000 });
    await expect(page.locator(".photo-loupe [role=alert]")).toHaveCount(0);
    const reloadURL = new URL(await run("web", "--no-browser"));
    reloadURL.pathname = "/photos";
    await page.goto("about:blank");
    await page.goto(reloadURL.href);
    await expect(page.getByText("300 photos", { exact: true })).toBeVisible();
    await expect(page.locator(".photo-pill")).toHaveCount(0);
    await scroll.locator("[data-asset]").first().getByRole("checkbox").click();
    await page.route("**/api/v1/photos/assets/query", async (route) => route.abort("failed"), {
      times: 1,
    });
    await page.evaluate((id) => {
      history.pushState(null, "", `/photos#photo=${id}`);
      window.dispatchEvent(new PopStateEvent("popstate"));
    }, assetID);
    await expect(page.locator(".photo-loupe [role=alert]")).toContainText("Failed to fetch");
    const failedDirectURL = page.url();
    await page.screenshot({
      path: path.join(output!, "web-photo-loupe-reload-400-dark.png"),
      animations: "disabled",
    });
    await page.getByRole("button", { name: "Reload photo" }).click();
    await expect(page.getByText("Fit preview", { exact: true })).toBeVisible({ timeout: 60_000 });
    await expect(page).toHaveURL(failedDirectURL);
    await page.keyboard.press("Escape");
    await expect(page.getByText("1 selected photo", { exact: true })).toBeVisible();
    for (const operation of ["preview", "original"]) {
      const freshURL = new URL(await run("web", "--no-browser"));
      freshURL.pathname = "/photos";
      let staleID = "";
      await page.route(
        "**/api/v1/photos/assets/query",
        async (route) => {
          const url = new URL(route.request().url()),
            host = url.host;
          url.hostname = "127.0.0.1";
          const response = await route.fetch({
            url: url.href,
            headers: { ...(await route.request().allHeaders()), host },
          });
          const body = await response.json();
          staleID = body.items[0].asset_id;
          body.items[0].content_version_id = "00000000-0000-4000-8000-000000000001";
          if (operation === "preview") body.items[0].previews.fit = { state: "missing" };
          await route.fulfill({ response, json: body });
        },
        { times: 1 },
      );
      await page.goto("about:blank");
      await page.goto(freshURL.href);
      await expect(scroll.locator("img").first()).toBeVisible();
      await scroll.locator("[data-asset]").nth(1).getByRole("checkbox").click();
      await scroll.locator(`[data-asset="${staleID}"] .photo-image`).press("Enter");
      if (operation === "original")
        await page.getByRole("button", { name: "View original", exact: true }).click();
      await expect(page.getByRole("button", { name: "Reload photo" })).toBeVisible();
      if (operation === "original")
        await expect(page.locator(".photo-loupe [role=alert]")).toContainText("Reload this photo");
      const staleURL = page.url();
      await page.getByRole("button", { name: "Reload photo" }).click();
      await expect(page.getByText("Fit preview", { exact: true })).toBeVisible({ timeout: 60_000 });
      await expect(page).toHaveURL(staleURL);
      await page.keyboard.press("Escape");
      await expect(page.getByText("1 selected photo", { exact: true })).toBeVisible();
      await expect(scroll.locator(`[data-asset="${staleID}"] .photo-image`)).toBeFocused();
    }
    const replacementURL = new URL(await run("web", "--no-browser"));
    replacementURL.pathname = "/photos";
    const replacementListing = page.waitForResponse(
      (response) => response.url().endsWith("/photos/assets/query") && response.ok(),
    );
    await page.goto("about:blank");
    await page.goto(replacementURL.href);
    const replacementRow = (await (await replacementListing).json()).items[0];
    const replacementCell = scroll.locator(
      `[data-asset="${replacementRow.asset_id}"] .photo-image`,
    );
    await expect(scroll.locator("img").first()).toBeVisible();
    await scroll.locator("[data-asset]").nth(1).getByRole("checkbox").click();
    await replacementCell.press("Enter");
    await expect(page.getByText("Fit preview", { exact: true })).toBeVisible({ timeout: 60_000 });
    const oldThumbnail = page.locator(`.filmstrip [data-asset="${replacementRow.asset_id}"] img`);
    await expect(oldThumbnail).toHaveAttribute("src", /^blob:/);
    const oldThumbnailURL = await oldThumbnail.getAttribute("src");
    const replacementFile = path.join(workspace, "replacement.jpg");
    await run("get", "/Synthetic-photo-00001.jpg", replacementFile);
    await page.keyboard.press("Escape");
    await expect(replacementCell).toBeFocused();
    await run("put", replacementFile, `/${replacementRow.name}`, "--mime-type", "image/jpeg");
    const originalFetch = globalThis.fetch,
      sourceURL = new URL(replacementURL);
    const sourceHost = sourceURL.host;
    sourceURL.hostname = "127.0.0.1";
    globalThis.fetch = (input, init) => {
      const headers = new Headers(init?.headers);
      headers.set("Host", sourceHost);
      return originalFetch(
        typeof input === "string" && input.startsWith("/") ? sourceURL.origin + input : input,
        { ...init, headers },
      );
    };
    try {
      await expect
        .poll(
          async () =>
            (
              await listPhotoAssets(
                {
                  query: {
                    v: 1,
                    syntax: "advanced",
                    mode: "lexical",
                    text: "",
                    filters: { asset_ids: [replacementRow.asset_id] },
                    sort: { field: "capture_time", direction: "desc" },
                  },
                  page_size: 1,
                },
                { session: new URLSearchParams(replacementURL.hash.slice(1)).get("web_session")! },
              )
            ).items[0]?.previews.grid.state,
          { timeout: 60_000 },
        )
        .toBe("ready");
    } finally {
      globalThis.fetch = originalFetch;
    }
    await page.evaluate(async () => {
      for (const name of await caches.keys()) {
        const cache = await caches.open(name);
        for (const key of await cache.keys()) await cache.delete(key);
      }
    });
    const obsoleteBytes = page.waitForResponse(
      (response) => response.url().includes("/previews/") && response.status() === 404,
    );
    await replacementCell.press("Enter");
    await obsoleteBytes;
    await expect(page.getByRole("button", { name: "Reload photo" })).toBeVisible();
    await page.screenshot({
      path: path.join(output!, "web-photo-loupe-replaced-400-dark.png"),
      animations: "disabled",
    });
    const replacedURL = page.url();
    await page.getByRole("button", { name: "Reload photo" }).click();
    await expect(page.getByText("Fit preview", { exact: true })).toBeVisible({ timeout: 60_000 });
    await expect(page).toHaveURL(replacedURL);
    await expect(oldThumbnail).toHaveAttribute("src", /^blob:/);
    await expect(oldThumbnail).not.toHaveAttribute("src", oldThumbnailURL!);
    await expect(page.locator(".photo-loupe [role=alert]")).toHaveCount(0);
    await page.keyboard.press("Escape");
    await expect(replacementCell).toBeFocused();
    await expect(page.getByText("1 selected photo", { exact: true })).toBeVisible();
    for (const delayed of [false, true]) {
      const progressive = await page.context().newPage();
      await progressive.addInitScript(() =>
        Object.defineProperty(window, "caches", { value: undefined }),
      );
      let selectedID = "",
        gridGeneration = "",
        releaseGrid!: () => void,
        releaseFit!: () => void;
      const gridHold = new Promise<void>((resolve) => (releaseGrid = resolve)),
        fitHold = new Promise<void>((resolve) => (releaseFit = resolve));
      await progressive.route(
        "**/api/v1/photos/assets/query",
        async (route) => {
          const url = new URL(route.request().url()),
            host = url.host;
          url.hostname = "127.0.0.1";
          const response = await route.fetch({
            url: url.href,
            headers: { ...(await route.request().allHeaders()), host },
          });
          const body = await response.json();
          selectedID = body.items[0].asset_id;
          gridGeneration = body.items[0].previews.grid.generation_id;
          await route.fulfill({ response });
        },
        { times: 1 },
      );
      await progressive.route("**/api/v1/photos/assets/*/previews/*", async (route) => {
        if (!route.request().url().includes(`/assets/${selectedID}/`)) {
          await route.continue();
          return;
        }
        if (route.request().url().endsWith(`/${gridGeneration}`)) {
          await gridHold;
          await route.abort("failed");
        } else {
          await fitHold;
          await route.continue();
        }
      });
      const progressiveURL = new URL(await run("web", "--no-browser"));
      progressiveURL.pathname = "/photos";
      await progressive.goto(progressiveURL.href, { waitUntil: "domcontentloaded" });
      await expect(progressive.getByText("300 photos", { exact: true })).toBeVisible();
      await expect(progressive.locator(".photo-pill")).toHaveCount(0);
      await progressive.locator(`[data-asset="${selectedID}"] .photo-image`).press("Enter");
      if (!delayed) {
        releaseGrid();
        await expect(progressive.locator(".photo-loupe [role=alert]")).toContainText(
          "Failed to fetch",
        );
      }
      releaseFit();
      await expect(progressive.getByText("Fit preview", { exact: true })).toBeVisible({
        timeout: 60_000,
      });
      if (delayed) {
        const failed = progressive.waitForEvent("requestfailed", {
          predicate: (request) =>
            request.url().includes(`/assets/${selectedID}/previews/${gridGeneration}`),
        });
        releaseGrid();
        await failed;
      }
      await expect(progressive.locator(".photo-loupe [role=alert]")).toHaveCount(0);
      await progressive.close();
    }
    const recovery = await page.context().newPage();
    await recovery.addInitScript(() =>
      Object.defineProperty(window, "caches", { value: undefined }),
    );
    const recoveryURL = new URL(await run("web", "--no-browser"));
    recoveryURL.pathname = "/photos";
    const recoveryListing = recovery.waitForResponse(
      (response) => response.url().endsWith("/photos/assets/query") && response.ok(),
    );
    await recovery.goto(recoveryURL.href);
    const recoveryRows = (await (await recoveryListing).json()).items;
    const retained = recoveryRows[0],
      neighbor = recoveryRows[1];
    await expect(recovery.locator(`[data-asset="${retained.asset_id}"] img`)).toBeVisible();
    await recovery.route(
      `**/api/v1/photos/assets/${neighbor.asset_id}/previews/${neighbor.previews.grid.generation_id}`,
      (route) => route.abort("failed"),
      { times: 1 },
    );
    await recovery.locator(`[data-asset="${retained.asset_id}"] .photo-image`).press("Enter");
    const railRetry = recovery.getByRole("button", { name: `Retry preview for ${neighbor.name}` });
    await expect(railRetry).toBeVisible();
    await expect(
      recovery.locator(`.filmstrip [data-asset="${neighbor.asset_id}"] .placeholder`),
    ).toContainText("Preview failed");
    for (const width of [1440, 400]) {
      await recovery.setViewportSize({ width, height: 960 });
      if (width === 400) await expect(recovery.locator(".filmstrip")).toBeHidden();
      await recovery.screenshot({
        path: path.join(output!, `web-photo-loupe-rail-retry-${width}-dark.png`),
        animations: "disabled",
      });
    }
    await recovery.setViewportSize({ width: 1440, height: 960 });
    const railLocation = recovery.url();
    await railRetry.click();
    await expect
      .poll(() =>
        recovery
          .locator(`.filmstrip [data-asset="${neighbor.asset_id}"] img`)
          .evaluate((image) => (image as HTMLImageElement).naturalWidth),
      )
      .toBeGreaterThan(0);
    await expect(recovery).toHaveURL(railLocation);
    await recovery.keyboard.press("Escape");
    await expect(recovery.getByRole("dialog", { name: /^Photo viewer/ })).toHaveCount(0);
    let releaseRefresh!: () => void, enteredRefresh!: () => void;
    const refreshHold = new Promise<void>((resolve) => (releaseRefresh = resolve)),
      refreshPending = new Promise<void>((resolve) => (enteredRefresh = resolve));
    await recovery.route(
      "**/api/v1/photos/assets/query",
      async (route) => {
        const body = route.request().postDataJSON();
        body.query.filters = { asset_ids: [neighbor.asset_id] };
        delete body.cursor;
        const url = new URL(route.request().url()),
          host = url.host;
        url.hostname = "127.0.0.1";
        const response = await route.fetch({
          url: url.href,
          headers: { ...(await route.request().allHeaders()), host },
          postData: JSON.stringify(body),
        });
        enteredRefresh();
        await refreshHold;
        await route.fulfill({ response });
      },
      { times: 1 },
    );
    await recovery.getByRole("button", { name: "More photo actions" }).click();
    await recovery.getByRole("menuitem", { name: "Refresh previews" }).click();
    await refreshPending;
    let failRetainedFit = true;
    await recovery.route(`**/api/v1/photos/assets/${retained.asset_id}/previews/*`, (route) =>
      failRetainedFit && !route.request().url().endsWith(`/${retained.previews.grid.generation_id}`)
        ? route.abort("failed")
        : route.continue(),
    );
    await recovery.locator(`[data-asset="${retained.asset_id}"] .photo-image`).press("Enter");
    releaseRefresh();
    const recoveryAlert = recovery.locator(".photo-loupe [role=alert]");
    await expect(recoveryAlert).toContainText("This photo is no longer in the refreshed result.");
    await expect(recoveryAlert).toContainText("Failed to fetch");
    await expect(recovery.getByRole("button", { name: "Reload photo" })).toBeVisible();
    await expect(
      recovery.getByRole("button", { name: "Retry preview", exact: true }),
    ).toBeVisible();
    for (const width of [1440, 400]) {
      await recovery.setViewportSize({ width, height: 960 });
      await recovery.screenshot({
        path: path.join(output!, `web-photo-loupe-membership-failure-${width}-dark.png`),
        animations: "disabled",
      });
    }
    await expect(recovery.getByRole("button", { name: "Reload photo" })).toBeEnabled();
    await recovery.route("**/api/v1/photos/assets/query", (route) => route.abort("failed"), {
      times: 1,
    });
    await recovery.getByRole("button", { name: "Reload photo" }).click();
    await expect(recoveryAlert.locator("span")).toHaveCount(3);
    failRetainedFit = false;
    await recovery.getByRole("button", { name: "Retry preview", exact: true }).click();
    await expect(recovery.getByText("Fit preview", { exact: true })).toBeVisible({
      timeout: 60_000,
    });
    await expect(recoveryAlert).toContainText("This photo is no longer in the refreshed result.");
    await expect(recoveryAlert).toContainText("Failed to fetch");
    await recovery.getByRole("button", { name: "Reload photo" }).click();
    await expect(recoveryAlert.locator("span")).toHaveCount(1);
    await writeFile(
      path.join(output!, "browserproof.json"),
      JSON.stringify(
        {
          railRetry: true,
          cacheDisabled: true,
          retainedMembershipWithPreviewFailure: true,
          retainedMembershipWithLookupFailure: true,
          previewRetry: true,
          reloadRecovery: true,
          widths: [1440, 400],
          binary,
        },
        null,
        2,
      ),
    );
    await recovery.close();
    const stalePaging = await page.context().newPage(),
      staleScroll = stalePaging.getByTestId("photo-scroll");
    const stalePagingURL = new URL(await run("web", "--no-browser"));
    stalePagingURL.pathname = "/photos";
    await stalePaging.goto(stalePagingURL.href);
    await expect(stalePaging.getByText("300 photos", { exact: true })).toBeVisible();
    await expect(stalePaging.locator(".photo-pill")).toHaveCount(0);
    await stalePaging.route("**/api/v1/photos/assets/query", (route) => route.abort("failed"), {
      times: 1,
    });
    await staleScroll.evaluate(
      (element) => (element.scrollTop = element.scrollHeight - element.clientHeight - 400),
    );
    await expect(stalePaging.locator(".photo-error")).toContainText("Failed to fetch");
    await staleScroll.locator("[data-asset]").last().locator(".photo-image").press("Enter");
    const stalePageURL = stalePaging.url(),
      staleName = await stalePaging.locator(".caption strong").innerText();
    await stalePaging.getByRole("button", { name: "Retry navigation" }).click();
    await expect(stalePaging.locator(".photo-bar .toolbar-actions > span")).toHaveText(
      "300 photos",
    );
    await expect(stalePaging.locator(".photo-pill")).toHaveCount(0);
    await expect(stalePaging).toHaveURL(stalePageURL);
    await expect(stalePaging.locator(".caption strong")).toHaveText(staleName);
    await stalePaging.keyboard.press("ArrowRight");
    await expect(stalePaging).not.toHaveURL(stalePageURL);
    await stalePaging.close();
    const touchContext = await browser.newContext({
      hasTouch: true,
      viewport: { width: 400, height: 960 },
      deviceScaleFactor: Number(process.env.DOCBANK_SCREENSHOT_SCALE ?? 1),
      colorScheme: "dark",
    });
    const touch = await touchContext.newPage();
    touch.setDefaultTimeout(15_000);
    const touchURL = new URL(await run("web", "--no-browser"));
    touchURL.pathname = "/photos";
    await touch.goto(touchURL.href);
    const cells = touch.locator("[data-asset]");
    await expect(cells.first().locator("img")).toBeVisible();
    const firstID = (await cells.first().getAttribute("data-asset"))!;
    const secondID = (await cells.nth(1).getAttribute("data-asset"))!;
    const a = touch.locator(`[data-asset="${firstID}"]`),
      b = touch.locator(`[data-asset="${secondID}"]`);
    await a.locator(".photo-image").click();
    await expect(touch.getByRole("dialog", { name: /^Photo viewer/ })).toHaveCount(0);
    await expect(touch.getByText("1 selected photo", { exact: true })).toBeVisible();
    await touch.getByRole("button", { name: "Clear selection" }).tap();
    await a.getByRole("checkbox").tap();
    const selectedTapBox = (await b.locator(".photo-image").boundingBox())!;
    const selectedTapX = selectedTapBox.x + selectedTapBox.width / 2,
      selectedTapY = selectedTapBox.y + selectedTapBox.height / 2;
    await touch.touchscreen.tap(selectedTapX, selectedTapY);
    await touch.touchscreen.tap(selectedTapX, selectedTapY);
    await expect(touch.getByRole("dialog", { name: /^Photo viewer/ })).toHaveCount(1);
    await touch.keyboard.press("Escape");
    await expect(b.locator(".photo-image")).toBeFocused();
    await expect(a.getByRole("checkbox")).toBeChecked();
    await expect(b.getByRole("checkbox")).toBeChecked();
    await a.locator(".photo-image").tap();
    await a.getByRole("checkbox").tap();
    await a.locator(".photo-image").tap();
    await expect(touch.getByRole("dialog", { name: /^Photo viewer/ })).toHaveCount(0);
    await expect(b.getByRole("checkbox")).toBeChecked();
    await expect(a.getByRole("checkbox")).not.toBeChecked();
    await expect(touch.getByText("1 selected photo", { exact: true })).toBeVisible();
    await touch.getByRole("button", { name: "Clear selection" }).tap();
    const emptyTapBox = (await a.locator(".photo-image").boundingBox())!;
    const emptyTapX = emptyTapBox.x + emptyTapBox.width / 2,
      emptyTapY = emptyTapBox.y + emptyTapBox.height / 2;
    await touch.touchscreen.tap(emptyTapX, emptyTapY);
    await touch.touchscreen.tap(emptyTapX, emptyTapY);
    await expect(touch.getByRole("dialog", { name: /^Photo viewer/ })).toHaveCount(1);
    await expect(touch.getByText("Fit preview", { exact: true })).toBeVisible({ timeout: 60_000 });
    const swipe = async (direction: -1 | 1) => {
      const box = (await touch.locator(".photo-viewport").boundingBox())!;
      const session = await touchContext.newCDPSession(touch);
      const x = box.x + box.width / 2,
        y = box.y + box.height / 2;
      await session.send("Input.dispatchTouchEvent", {
        type: "touchStart",
        touchPoints: [{ x, y }],
      });
      await session.send("Input.dispatchTouchEvent", {
        type: "touchMove",
        touchPoints: [{ x: x - direction * 150, y }],
      });
      await session.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
      await session.detach();
      await expect(touch.locator(".pan")).toHaveCSS("transform", "matrix(1, 0, 0, 1, 0, 0)");
    };
    const boundaryURL = touch.url();
    await swipe(-1);
    await expect(touch).toHaveURL(boundaryURL);
    await touch.getByRole("button", { name: "Close photo viewer" }).tap();
    const retryID = (await cells.last().getAttribute("data-asset"))!;
    const standalone = new URL(await run("web", "--no-browser"));
    standalone.pathname = "/photos";
    standalone.hash += `&photo=${retryID}`;
    await touch.goto("about:blank");
    await touch.route(
      "**/api/v1/photos/assets/*/preview",
      async (route) =>
        route.request().postDataJSON().size === "fit" ? route.abort("failed") : route.continue(),
      { times: 1 },
    );
    await touch.goto(standalone.href);
    await expect(touch.getByRole("button", { name: "Retry preview" })).toBeVisible();
    await expect
      .poll(() =>
        touch
          .locator(".photo-viewport img")
          .evaluate((image) => (image as HTMLImageElement).naturalWidth),
      )
      .toBeGreaterThan(0);
    await touch.screenshot({
      path: path.join(output!, "web-photo-loupe-fit-retry-400-dark.png"),
      animations: "disabled",
    });
    await touch.getByRole("button", { name: "Retry preview" }).tap();
    await expect(touch.getByText("Fit preview", { exact: true })).toBeVisible({ timeout: 60_000 });
    const standaloneURL = touch.url();
    await swipe(1);
    await expect(touch).toHaveURL(standaloneURL);
    await touch.screenshot({
      path: path.join(output!, "web-photo-loupe-touch-400-dark.png"),
      animations: "disabled",
    });
    const terminal = new URL(await run("web", "--no-browser"));
    terminal.pathname = "/photos";
    terminal.hash += `&photo=${terminalID}`;
    await touch.goto("about:blank");
    await touch.goto(terminal.href);
    await expect(touch.getByText("No preview", { exact: true })).toBeVisible({ timeout: 60_000 });
    await expect(touch.getByRole("button", { name: "Retry preview" })).toHaveCount(0);
    await touch.screenshot({
      path: path.join(output!, "web-photo-loupe-unavailable-400-dark.png"),
      animations: "disabled",
    });
    await touchContext.close();
    const displayURL = new URL(await run("web", "--no-browser"));
    displayURL.pathname = "/photos";
    const displayListing = page.waitForResponse(
      (response) => response.url().endsWith("/photos/assets/query") && response.ok(),
    );
    await page.goto("about:blank");
    await page.goto(displayURL.href);
    const displayRows = (await (await displayListing).json()).items,
      oldDisplay = displayRows[0],
      newDisplay = displayRows[2];
    await run("photos", "assets", "detach", newDisplay.asset_id, newDisplay.display_file_id);
    const attached = JSON.parse(
      await run(
        "photos",
        "assets",
        "attach",
        oldDisplay.asset_id,
        `id:${newDisplay.node_id}`,
        "--role",
        "image",
      ),
    );
    const newFile = attached.files.find(
      (file: { node_id: number }) => file.node_id === newDisplay.node_id,
    ).id;
    await run("photos", "assets", "display", oldDisplay.asset_id, oldDisplay.display_file_id);
    const displayCell = scroll.locator(`[data-asset="${oldDisplay.asset_id}"] .photo-image`);
    await expect(scroll.locator("img").first()).toBeVisible();
    await scroll.locator("[data-asset]").nth(1).getByRole("checkbox").click();
    await displayCell.press("Enter");
    await expect(page.getByText("Fit preview", { exact: true })).toBeVisible({ timeout: 60_000 });
    const shownURL = await page.locator(".photo-viewport img").getAttribute("src"),
      shownLocation = page.url();
    const originalRequests: import("@playwright/test").Request[] = [];
    page.on("request", (request) => {
      if (request.url().includes("web-download")) originalRequests.push(request);
    });
    await run("photos", "assets", "display", oldDisplay.asset_id, newFile);
    await page.getByRole("button", { name: "View original", exact: true }).click();
    await expect(page.getByRole("button", { name: "Reload photo" })).toBeVisible();
    await expect(page.locator(".caption strong")).toHaveText(oldDisplay.name);
    await expect(page.locator(".photo-viewport img")).toHaveAttribute("src", shownURL!);
    expect(originalRequests).toHaveLength(0);
    await expect(page).toHaveURL(shownLocation);
    await page.screenshot({
      path: path.join(output!, "web-photo-loupe-display-change-400-dark.png"),
      animations: "disabled",
    });
    await page.getByRole("button", { name: "Reload photo" }).click();
    await expect(page.locator(".caption strong")).toHaveText(newDisplay.name);
    await expect(page.getByText("Fit preview", { exact: true })).toBeVisible({ timeout: 60_000 });
    await page.getByRole("button", { name: "View original", exact: true }).click();
    await expect(page.getByText("Original", { exact: true })).toBeVisible({ timeout: 60_000 });
    expect(
      originalRequests.find((request) => request.method() === "POST")!.postDataJSON().node_id,
    ).toBe(newDisplay.node_id);
    await page.keyboard.press("Escape");
    await expect(displayCell).toBeFocused();
    await expect(page.getByText("1 selected photo", { exact: true })).toBeVisible();
    expect(errors).toEqual([]);
  } finally {
    if (process.env.DOCBANK_KEEP_PHOTO_PREVIEW) {
      const previewURL = new URL(await run("web", "--no-browser"));
      previewURL.pathname = "/photos";
      previewURL.hash += `&photo=${assetID}`;
      await writeFile(
        path.join(output!, "loupe-preview.json"),
        JSON.stringify({ url: previewURL.href, workspace, binary }, null, 2),
      );
    } else {
      await run("daemon", "stop");
      await rm(workspace, { recursive: true, force: true, maxRetries: 10, retryDelay: 500 });
    }
  }
});

async function expectToolbarContained(page: Page) {
  await expect
    .poll(() =>
      page.locator(".photo-bar").evaluate((element) => {
        const crumbs = element.querySelector(".photo-crumbs")!.getBoundingClientRect();
        const actions = element.querySelector(".toolbar-actions")!;
        const row = actions.getBoundingClientRect();
        const controls = [...actions.children].map((control) => control.getBoundingClientRect());
        const gap = Number.parseFloat(getComputedStyle(actions).gap);
        return (
          row.top >= crumbs.bottom &&
          row.right <= innerWidth &&
          document.documentElement.scrollWidth === innerWidth &&
          controls.every((control) => control.top >= row.top && control.bottom <= row.bottom) &&
          actions.scrollWidth === actions.clientWidth &&
          controls.every(
            (control, index) =>
              control.left >= row.left &&
              control.right <= row.right &&
              (index === 0 ||
                (control.top >= controls[index - 1].bottom
                  ? control.top - controls[index - 1].bottom >= gap
                  : control.left - controls[index - 1].right >= gap)),
          )
        );
      }),
    )
    .toBe(true);
}

test("compact photo actions wrap with full labels and normal gaps", async ({ page }) => {
  test.setTimeout(300_000);
  await withPhotoFixture(400, "compact-layout", async (run) => {
    const url = new URL(await run("web", "--no-browser"));
    url.pathname = "/photos";
    await page.goto(url.href);
    await expect(page.locator(".photo-cell img").first()).toBeVisible();
    const actions = page.locator(".photo-bar .toolbar-actions");
    await expect(actions.locator(":scope > span")).toHaveText("400 photos");
    for (const [scope, width] of [
      ["library", 400],
      ["library", 320],
      ["album", 400],
    ] as const) {
      if (scope === "album") {
        await page.setViewportSize({ width: 1440, height: 960 });
        await page
          .getByRole("navigation", { name: "Docbank navigation" })
          .getByRole("button", { name: /^Paris walks/ })
          .click();
        await expect(page.getByRole("heading", { name: "Paris walks", exact: true })).toBeVisible();
        await expect(
          actions.getByRole("button", { name: "Star album", exact: true }),
        ).toBeVisible();
      }
      await page.setViewportSize({ width, height: 800 });
      await page.evaluate(() => document.fonts.ready);
      await expectToolbarContained(page);
      await page.screenshot({
        path: path.join(output!, `wrap-${scope}-${width}.png`),
        animations: "disabled",
      });
      for (const control of await actions.locator("button").all()) {
        await expect(control).toBeInViewport();
        expect(
          await control.evaluate((element) => {
            const bounds = element.getBoundingClientRect();
            const label = element.querySelector(".kit-select-dropdown__value") ?? element;
            const range = document.createRange();
            range.selectNodeContents(label);
            const text = range.getBoundingClientRect();
            return (
              bounds.left >= 0 &&
              bounds.right <= innerWidth &&
              text.left > bounds.left &&
              text.right < bounds.right
            );
          }),
        ).toBe(true);
      }
      await page.getByRole("button", { name: "More photo actions" }).click();
      await expect(page.getByRole("menuitem", { name: "Cull", exact: true })).toBeVisible();
      await page.keyboard.press("Escape");
    }
  });
});

test("photo layout keeps search and actions above Library and preserves Timeline", async ({
  page,
}) => {
  test.setTimeout(300_000);
  page.setDefaultTimeout(15_000);
  await withPhotoFixture(10000, "layout", async (run) => {
    const url = new URL(await run("web", "--no-browser"));
    url.pathname = "/photos";
    await page.goto(url.href);
    const capture = async (name: string) => {
      await expect(page.locator(".photo-facets .photo-loading")).toHaveCount(0);
      await page.evaluate(() => document.fonts.ready);
      await page.screenshot({
        path: path.join(output!, `layout-${name}.png`),
        animations: "disabled",
      });
    };
    const expectStackAlignment = async () => {
      const bar = await page.locator(".photo-bar").boundingBox();
      const gap = page.getByRole("spinbutton", { name: "Burst gap (seconds)" });
      await expect(gap).toBeInViewport();
      const label = await gap.locator("..").boundingBox();
      const padding = await page
        .locator(".photo-bar")
        .evaluate((element) => Number.parseFloat(getComputedStyle(element).paddingLeft));
      expect(label!.x).toBeCloseTo(bar!.x + padding, 0);
    };
    const expectCompactActions = async () => {
      const actions = page.locator(".photo-bar .toolbar-actions");
      await expectToolbarContained(page);
      await expect
        .poll(() =>
          page.locator(".photo-crumbs").evaluate((element) => {
            const crumbs = element.getBoundingClientRect();
            const titles = [...element.children].map((item) => item.getBoundingClientRect());
            return crumbs.height <= Math.max(...titles.map((item) => item.height)) + 1;
          }),
        )
        .toBe(true);
      const count = actions.locator(":scope > span");
      await expect(count).toHaveAttribute("title", (await count.textContent())!);
      expect(
        await count.evaluate((element) => {
          const value = element.querySelector(".photo-count-value")!;
          const range = document.createRange();
          range.selectNodeContents(value);
          const text = range.getBoundingClientRect();
          const bounds = value.getBoundingClientRect();
          return bounds.width >= text.width && bounds.right <= innerWidth;
        }),
      ).toBe(true);
      await expect(actions.getByRole("combobox", { name: /^Grid density/ })).toHaveCount(0);
      for (const control of await actions.locator("button").all()) {
        await expect(control).toBeInViewport();
        expect(
          await control.evaluate((element) => {
            const bounds = element.getBoundingClientRect();
            return bounds.left >= 0 && bounds.right <= innerWidth;
          }),
        ).toBe(true);
      }
      const sort = actions.getByRole("combobox", { name: /^Sort photos/ });
      await expect(sort).toBeEnabled();
      expect(
        await sort.locator(".kit-select-dropdown__value").evaluate((element) => {
          const label = document.createRange();
          label.selectNodeContents(element);
          const bounds = label.getBoundingClientRect();
          const trigger = element.closest("button")!.getBoundingClientRect();
          return bounds.left >= trigger.left && bounds.right <= trigger.right;
        }),
      ).toBe(true);
      await sort.click();
      await expect(page.getByRole("option", { selected: true })).toBeVisible();
      await page.keyboard.press("Escape");
    };
    await expect(page.locator(".photo-cell img").first()).toBeVisible();
    await expect(page.getByRole("searchbox", { name: "Search photos" })).toBeVisible();
    await expect(page.locator(".photo-facets")).toHaveCount(0);
    await expectStackAlignment();
    await capture("library-1440");
    await page.setViewportSize({ width: 320, height: 800 });
    await capture("library-320");
    const count = page.locator(".photo-bar .toolbar-actions > span");
    expect(
      await count.evaluate((element) => {
        const value = element.firstElementChild ?? element;
        const range = document.createRange();
        range.selectNodeContents(value);
        const text = range.getBoundingClientRect();
        const bounds = value.getBoundingClientRect();
        return text.width > 0 && bounds.width >= text.width && bounds.right <= innerWidth;
      }),
    ).toBe(true);
    await page.getByRole("button", { name: "Filters", exact: true }).click();
    await capture("filters-320");
    await page.getByRole("button", { name: "Filters", exact: true }).click();
    await page.setViewportSize({ width: 400, height: 800 });
    await expectStackAlignment();
    await capture("library-400");
    const bar = page.locator(".photo-bar");
    await expect
      .poll(() =>
        bar.evaluate((element) => {
          const crumbs = element.querySelector(".photo-crumbs")!.getBoundingClientRect();
          const actions = element.querySelector(".toolbar-actions")!.getBoundingClientRect();
          const controls = [...element.querySelectorAll(".toolbar-actions > *")].map((item) =>
            item.getBoundingClientRect(),
          );
          return (
            element.getBoundingClientRect().right <= innerWidth &&
            actions.top >= crumbs.bottom &&
            actions.right <= innerWidth &&
            controls.every((item) => item.left >= actions.left && item.right <= actions.right)
          );
        }),
      )
      .toBe(true);
    await expectCompactActions();
    await expect(page.getByRole("button", { name: "Timeline", exact: true })).toBeVisible();
    await expect(page.getByRole("combobox", { name: /^Group photos/ })).toHaveCount(0);
    await capture("library-400");
    await page.getByRole("button", { name: "More photo actions" }).click();
    await expect(page.getByRole("menuitem", { name: "Cull", exact: true })).toBeVisible();
    await expect(page.getByRole("menuitemradio", { name: "Months", exact: true })).toBeVisible();
    await expect(
      page.getByRole("menuitemradio", { name: "Comfortable", exact: true }),
    ).toBeVisible();
    await capture("menu-400");
    await page.keyboard.press("Escape");
    await page.setViewportSize({ width: 1440, height: 960 });
    await page.getByRole("button", { name: "Filters", exact: true }).click();
    await expect(page.locator(".photo-facets")).toHaveCSS("width", "230px");
    await capture("filters-1440");
    await page.setViewportSize({ width: 400, height: 800 });
    await expectCompactActions();
    await expect(page.locator(".photo-facets")).toHaveCSS("max-height", "180px");
    await capture("filters-400");
    await page.setViewportSize({ width: 1440, height: 960 });
    await page.getByRole("button", { name: "Filters", exact: true }).click();
    await page.locator(".photo-cell input[type=checkbox]").nth(0).check();
    await page.locator(".photo-cell input[type=checkbox]").nth(1).check();
    await expect(page.getByRole("button", { name: "Compare", exact: true })).toBeEnabled();
    await expect(bar.getByRole("button", { name: "Compare", exact: true })).toHaveCount(0);
    await capture("selection-1440");
    await page
      .getByRole("navigation", { name: "Docbank navigation" })
      .getByRole("button", { name: /^Paris walks/ })
      .click();
    await expect(page.getByRole("heading", { name: "Paris walks", exact: true })).toBeVisible();
    await expect(page.getByRole("searchbox", { name: "Search photos" })).toHaveCount(0);
    await expect(page.locator(".photo-cell img").first()).toBeVisible();
    await capture("album-1440");
    await page.setViewportSize({ width: 400, height: 800 });
    await expectCompactActions();
    await capture("album-400");
    await page.setViewportSize({ width: 1440, height: 960 });
    await page.getByRole("button", { name: "Library", exact: true }).click();
    await page.getByRole("button", { name: "Clear selection", exact: true }).click();
    await page.getByRole("button", { name: "Timeline", exact: true }).click();
    await expect(page.getByRole("navigation", { name: "Timeline years" })).toBeVisible();
    await expect(page.getByRole("region", { name: "Photo timeline" })).toBeVisible();
    await capture("timeline-1440");
    await page.setViewportSize({ width: 400, height: 800 });
    await page
      .getByRole("navigation", { name: "Timeline years" })
      .getByRole("button", { name: /^2025/ })
      .click();
    await page.getByRole("button", { name: /^December 2025/ }).click();
    await expect(bar.locator(".toolbar-actions > span")).toHaveText("27 photos in December 2025");
    await expectCompactActions();
    await capture("library-month-400");
    await page.getByRole("button", { name: /^2025-12-15 ·/ }).click();
    await expect(bar.locator(".toolbar-actions > span")).toHaveText("27 photos on 2025-12-15");
    await expectCompactActions();
    await capture("library-day-400");
    await page.setViewportSize({ width: 320, height: 800 });
    expect(
      await count.evaluate((element) => {
        const value = element.querySelector(".photo-count-value")!;
        const range = document.createRange();
        range.selectNodeContents(value);
        const text = range.getBoundingClientRect();
        const bounds = value.getBoundingClientRect();
        const date = element.querySelector(".photo-count-date")!;
        return (
          bounds.width >= text.width &&
          bounds.right <= innerWidth &&
          date.getBoundingClientRect().right <= innerWidth
        );
      }),
    ).toBe(true);
    await capture("library-day-320");
    await page.setViewportSize({ width: 400, height: 800 });
    await page.getByRole("button", { name: "Filters", exact: true }).click();
    await expectCompactActions();
    await capture("library-day-filters-400");
    await page.setViewportSize({ width: 1440, height: 960 });
    expect(
      await count.evaluate((element) => {
        const value = element.querySelector(".photo-count-value")!.getBoundingClientRect();
        const date = element.querySelector(".photo-count-date")!.firstChild!;
        const word = document.createRange();
        word.setStart(date, 1);
        word.setEnd(date, 3);
        return word.getBoundingClientRect().left > value.right + 2;
      }),
    ).toBe(true);
    await capture("library-day-filters-1440");
  });
});
