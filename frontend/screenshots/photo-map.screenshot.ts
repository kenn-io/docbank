import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, readFile, writeFile, rm } from "node:fs/promises";
import { createServer, type Server } from "node:https";
import type { AddressInfo } from "node:net";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";
const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const binary =
  process.env.DOCBANK_SCREENSHOT_BINARY ??
  path.join(repository, "bin", process.platform === "win32" ? "docbank.exe" : "docbank");
const output = process.env.DOCBANK_MAP_PROOF_DIR;
const tilePNG =
  "iVBORw0KGgoAAAANSUhEUgAAAQAAAAEACAIAAADTED8xAAAEEElEQVR42u3YwQmFMBBF0fTfkS2Ie5eCNQRxqSBIOggzc2Aa8F0O/Py2n0fQu+7+X9BPWLb1PftPvAYAAAAIAAAAAgAAgAAAACAAAAAIAAAAAgAAgAAAACAAAAAIAAAAAgAAgAAAACAAAAAIAAAAAgAAgAAAACAAAAAIAAAAAgAAgAAAACAAAAAIAAAAAgAAgAAAACAAAAAAAAAAANgfAADsDwAA9gcAAPsDAID9AQDA/gAAYH8AALA/AADYHwAA7A8AAPYHAAD7AwCA/QEAwP4AAGB/AACwPwAA2B8AAOwPAAD2BwAA+wMAgP0BAMD+AABgfwAAsD8AAAAAAAAAAAAAAAIAAIAAAAAgAAAACAAAAAIAAIAAAAAgAAAACABAQgDjZzhX7QBwADgHgHMVAXgEewT7F0gAAAAQAAAABAAAAAEAAEAAAAAQAAAABAAAAAEAAEAAAAAQAAAABAAAAAEAAEAAAAAQAAAABAAAAAEAAEAAAAAQAAAABAAAAAEAAEAAAAAQAAAABAAAAAEAAAAAAAAAwP4AAGB/AACwPwAA2B8AAOwPAAD2BwAA+wMAgP0BAMD+AABgfwAAsD8AANgfAADsDwAA9gcAAPsDAID9AQDA/gAAYH8AALA/AADYHwAA7A8AAPYHAAD7AwCA/QEAAAAAAAAAAAAAEAAAAAQAAAABAABAAAAAEAAAAAQAAAABAABAAAASAvgaRLwxQNyvsP/cA8DVBuAnkJ9A3gACAACAAAAAIAAAAAgAAAACAACAAAAAIAAAAAgAAAACAACAAAAAIAAAAAgAAAACAACAAAAAIAAAAAgAAAACAACAAAAAIAAAAAgAAAACAACAAAAAIAAAAAgAAAAAAAAAAPYHAAD7AwCA/QEAwP4AAGB/AACwPwAA2B8AAOwPAAD2BwAA+wMAgP0BAMD+AABgfwAAsD8AANgfAADsDwAA9gcAAPsDAID9AQDA/gAAYH8AALA/AADYHwAA7A8AAAAAAAAAAAAAgAAAACAAAAAIAAAAAgAAgAAAACAAAAAIAAAAAgCQEMD4Gc5VOwAcAM4B4FxFAB7BHsH+BRIAAAAEAAAAAQAAQAAAABAAAAAEAAAAAQAAQAAAABAAAAAEAAAAAQAAQAAAABAAAAAEAAAAAQAAQAAAABAAAAAEAAAAAQAAQAAAABAAAAAEAAAAAQAAQAAAAAAAAAAAsD8AANgfAADsDwAA9gcAAPsDAID9AQDA/gAAYH8AALA/AADYHwAA7A8AAPYHAAD7AwCA/QEAwP4AAGB/AACwPwAA2B8AAOwPAAD2BwAA+wMAgP0BAMD+AABgfwAAAAAAAAAAAAAABAAAAAEAAEAAAAAQAAAABAAAAAEAAEAAAAAQAAAABAAgFYAHCWia2leX7zIAAAAASUVORK5CYII=";
test.skip(!output, "DOCBANK_MAP_PROOF_DIR enables synthetic map proof");
test("photo map privacy, clusters, marker return and mobile grid", async ({ page }) => {
  test.setTimeout(300_000);
  await mkdir(output!, { recursive: true });
  const workspace = await mkdtemp(path.join(output!, "map-proof-"));
  const vault = path.join(workspace, "vault");
  const env = {
    ...process.env,
    DOCBANK_HOME: vault,
    DOCBANK_LOCK_DIR: path.join(workspace, "locks"),
    DOCBANK_MAP_TILE_TEST_CA: path.join(workspace, "tls", "ca.pem"),
    GOCACHE: process.env.GOCACHE,
  };
  const run = async (...args: string[]) =>
    (await exec(binary, args, { cwd: repository, env, timeout: 60_000 })).stdout.trim();
  const errors: string[] = [];
  page.on("pageerror", (error) => {
    errors.push(error.message);
    console.error(error.stack);
  });
  const browserRequests: string[] = [];
  let tileRequests = 0,
    listings = 0,
    mapChunks = 0,
    libraryReads = 0;
  page.on("request", (request) => {
    browserRequests.push(request.url());
    if (/\/assets\/PhotoMap-[^/]+\.js$/.test(new URL(request.url()).pathname)) mapChunks++;
    if (request.url().endsWith("/photos/assets/query")) {
      listings++;
      const filters = request.postDataJSON().query.filters;
      if (!filters.gps_bounds && !filters.asset_ids) libraryReads++;
    }
    if (new URL(request.url()).pathname.startsWith("/api/daemon/web-map-tiles/")) tileRequests++;
  });
  await page.addInitScript(() => {
    Object.defineProperty(window, "L", {
      configurable: true,
      set(leaflet: typeof import("leaflet")) {
        Object.defineProperty(window, "L", { value: leaflet, writable: true, configurable: true });
        leaflet.Map.addInitHook(function (this: import("leaflet").Map) {
          (window as typeof window & { photoMap: import("leaflet").Map }).photoMap = this;
        });
      },
    });
  });
  // A synthetic HTTPS provider that the daemon reaches through the real tile route.
  let provider: Server | undefined,
    providerRequests = 0,
    providerFails = false;
  try {
    await mkdir(path.join(workspace, "tls"));
    await exec("go", ["run", "./frontend/screenshots/tile-tls.go", path.join(workspace, "tls")], {
      cwd: repository,
      env,
      timeout: 180_000,
    });
    provider = createServer(
      {
        key: await readFile(path.join(workspace, "tls", "leaf.key")),
        cert: await readFile(path.join(workspace, "tls", "leaf.pem")),
      },
      (_request, response) => {
        providerRequests++;
        if (providerFails) {
          response.writeHead(404).end();
          return;
        }
        response
          .writeHead(200, { "Content-Type": "image/png" })
          .end(Buffer.from(tilePNG, "base64"));
      },
    );
    await new Promise<void>((resolve) => provider!.listen(0, "127.0.0.1", resolve));
    const providerHost = `127.0.0.1:${(provider.address() as AddressInfo).port}`;
    await exec(
      "go",
      [
        "run",
        "-tags",
        "fts5",
        "./frontend/screenshots/photos-fixture.go",
        "-map",
        "-current-metadata",
        vault,
        "15",
      ],
      { cwd: repository, env, timeout: 180_000 },
    );
    const disabledURL = new URL(await run("web", "--no-browser"));
    disabledURL.pathname = "/photos/map";
    await page.goto(disabledURL.href);
    await page.getByText("Maps are disabled", { exact: true }).waitFor();
    expect(tileRequests).toBe(0);
    expect(mapChunks).toBe(0);
    await page.screenshot({
      path: path.join(output!, "photo-map-disabled-1440-dark.png"),
      animations: "disabled",
    });
    await run("daemon", "stop");
    await writeFile(
      path.join(vault, "config.toml"),
      '[web.map_tiles]\ntile_url = "https://' +
        providerHost +
        '/{z}/{x}/{y}.png?key=synthetic-provider-key"\nattribution = "Example map provider"\nmax_zoom = 19\n',
    );
    const enabledURL = new URL(await run("web", "--no-browser"));
    enabledURL.pathname = "/photos/map";
    const beforeMap = libraryReads;
    await page.goto(enabledURL.href);
    await expect(page.getByText("14 located photos", { exact: true })).toBeVisible();
    expect(libraryReads).toBe(beforeMap);
    await expect(page.locator(".photo-map-cluster").first()).toBeVisible();
    expect(mapChunks).toBe(1);
    await expect(
      page.getByText(`${providerHost} receives Docbank's IP address and viewed map areas.`, {
        exact: true,
      }),
    ).toBeVisible();
    await expect.poll(() => tileRequests).toBeGreaterThan(0);
    await expect.poll(() => providerRequests).toBeGreaterThan(0);
    expect(await page.content()).not.toContain("synthetic-provider-key");
    expect(
      browserRequests.some(
        (url) => url.includes("synthetic-provider-key") || new URL(url).host === providerHost,
      ),
    ).toBe(false);
    await expect(page.getByTestId("photo-scroll").locator("img")).toHaveCount(9);
    await page.screenshot({
      path: path.join(output!, "photo-map-enabled-1440-dark.png"),
      animations: "disabled",
    });
    await page.evaluate(() =>
      (window as typeof window & { photoMap: import("leaflet").Map }).photoMap.setView(
        [0, 372],
        2,
        { animate: false },
      ),
    );
    await expect
      .poll(() =>
        page.evaluate(
          () =>
            (window as typeof window & { photoMap: import("leaflet").Map }).photoMap.getCenter()
              .lng,
        ),
      )
      .toBeLessThanOrEqual(180);
    await expect(
      page.getByRole("button", { name: "Open Synthetic-photo-00007.jpg" }),
    ).toBeAttached();
    await page.evaluate(() =>
      (window as typeof window & { photoMap: import("leaflet").Map }).photoMap.setView([0, 0], 0, {
        animate: false,
      }),
    );
    await expect(page.getByText("14 photos", { exact: true })).toBeVisible();
    await expect(
      page.getByRole("button", { name: "Open Synthetic-photo-00007.jpg" }),
    ).toBeAttached();
    await expect(page.locator(".leaflet-tile-loaded")).toHaveCount(1);
    await expect(page.locator(".leaflet-tile-loaded")).toHaveCSS("opacity", "1");
    await page.screenshot({
      path: path.join(output!, "photo-map-world-1440-dark.png"),
      animations: "disabled",
    });
    const open14 = page.getByRole("button", { name: "Open Synthetic-photo-00014.jpg" });
    await open14.focus();
    await open14.press("Enter");
    await expect(
      page.getByRole("dialog", { name: "Photo viewer, Synthetic-photo-00014.jpg" }),
    ).toBeVisible();
    await page.getByRole("button", { name: "Next photo", exact: true }).first().click();
    await expect(
      page.getByRole("dialog", { name: "Photo viewer, Synthetic-photo-00010.jpg" }),
    ).toBeVisible();
    await page.keyboard.press("Escape");
    await page.evaluate(() =>
      (window as typeof window & { photoMap: import("leaflet").Map }).photoMap.setView([20, 0], 2, {
        animate: false,
      }),
    );
    for (const key of ["Enter", "Space"]) {
      const ordinary = page.locator(".photo-map-cluster").filter({ hasText: /^6$/ });
      await expect(ordinary).toHaveCount(1);
      await ordinary.press(key);
      await expect
        .poll(() =>
          page.evaluate(() =>
            (window as typeof window & { photoMap: import("leaflet").Map }).photoMap.getZoom(),
          ),
        )
        .toBeGreaterThan(2);
      await expect(page.locator(".leaflet-zoom-anim, .leaflet-cluster-anim")).toHaveCount(0);
      await page.evaluate(() =>
        (window as typeof window & { photoMap: import("leaflet").Map }).photoMap.setView(
          [20, 0],
          2,
          { animate: false },
        ),
      );
    }
    const mapBox = await page.locator(".photo-map").boundingBox();
    const queries = listings;
    await page.getByRole("checkbox", { name: "Select photo Synthetic-photo-00007.jpg" }).check();
    await page.mouse.move(mapBox!.x + mapBox!.width / 2, mapBox!.y + mapBox!.height / 2);
    await page.mouse.down();
    await page.mouse.move(mapBox!.x + mapBox!.width / 2 + 30, mapBox!.y + mapBox!.height / 2, {
      steps: 5,
    });
    await page.mouse.up();
    await page.locator(".leaflet-control-zoom-in").click();
    await expect(
      page.getByRole("checkbox", { name: "Select photo Synthetic-photo-00007.jpg" }),
    ).toBeChecked();
    expect(listings).toBe(queries);
    await page.locator(".leaflet-control-zoom-out").click();
    await expect(page.locator(".leaflet-zoom-anim, .leaflet-cluster-anim")).toHaveCount(0);
    const expandedOverlap = async () => {
      const first = page.locator('.photo-map-dot[title="Synthetic-photo-00009.jpg"]'),
        second = page.locator('.photo-map-dot[title="Synthetic-photo-00010.jpg"]');
      await expect(page.locator(".leaflet-cluster-anim")).toHaveCount(0);
      await expect(first).toHaveCSS("opacity", "1");
      await expect(second).toHaveCSS("opacity", "1");
      await expect
        .poll(async () => {
          const a = await first.boundingBox(),
            b = await second.boundingBox();
          return a && b ? Math.hypot(a.x - b.x, a.y - b.y) : 0;
        })
        .toBeGreaterThan(20);
    };
    await page.evaluate(() =>
      (window as typeof window & { photoMap: import("leaflet").Map }).photoMap.setView(
        [40.748816666666667, -73.98565555555555],
        19,
        { animate: false },
      ),
    );
    await expect(page.locator(".leaflet-zoom-anim, .leaflet-cluster-anim")).toHaveCount(0);
    for (const key of ["Enter", "Space"]) {
      await page.evaluate(
        () =>
          new Promise<void>((resolve) =>
            requestAnimationFrame(() => requestAnimationFrame(() => resolve())),
          ),
      );
      await expect(page.locator(".leaflet-zoom-anim, .leaflet-cluster-anim")).toHaveCount(0);
      await expect(page.locator(".photo-map-cluster").filter({ hasText: /^2$/ })).toHaveCount(1);
      await page.locator(".photo-map-cluster").filter({ hasText: /^2$/ }).press(key);
      await expandedOverlap();
      await expect(page.getByText("2 photos", { exact: true })).toBeVisible();
      if (key === "Enter") {
        await page.locator(".leaflet-control-zoom-out").click();
        await expect(page.locator(".leaflet-zoom-anim, .leaflet-cluster-anim")).toHaveCount(0);
        await page.locator(".leaflet-control-zoom-in").click();
        await expect(page.locator(".leaflet-zoom-anim, .leaflet-cluster-anim")).toHaveCount(0);
      }
    }
    await page.evaluate(() => {
      const map = (window as typeof window & { photoMap: import("leaflet").Map }).photoMap;
      map.panBy([map.getSize().x / 2 - 8, 0], { animate: false });
    });
    await expect(page.locator(".leaflet-cluster-anim")).toHaveCount(0);
    await page.locator(".photo-map-cluster").filter({ hasText: /^2$/ }).press("Space");
    await expandedOverlap();
    await expect(page.getByText("2 photos", { exact: true })).toBeVisible();
    await page.evaluate(() =>
      (window as typeof window & { photoMap: import("leaflet").Map }).photoMap.setView(
        [40.748816666666667, -73.98565555555555],
        19,
        { animate: false },
      ),
    );
    await page.locator(".photo-map-cluster").filter({ hasText: /^2$/ }).press("Space");
    await expandedOverlap();
    await page.screenshot({
      path: path.join(output!, "photo-map-overlap-1440-dark.png"),
      animations: "disabled",
    });

    await page.getByRole("button", { name: "Move to trash", exact: true }).click();
    await page.getByRole("dialog", { name: "Move selected photos to trash" }).waitFor();
    await page.screenshot({
      path: path.join(output!, "photo-map-selection-1440-dark.png"),
      animations: "disabled",
    });
    await page.getByRole("button", { name: "Keep in Docbank" }).click();
    await page.evaluate(() =>
      (window as typeof window & { photoMap: import("leaflet").Map }).photoMap.setView([20, 0], 2, {
        animate: false,
      }),
    );
    await expect(page.locator(".leaflet-zoom-anim, .leaflet-cluster-anim")).toHaveCount(0);
    const marker = page.locator('.photo-map-dot[title="Synthetic-photo-00007.jpg"]');
    await expect(marker).toBeVisible();
    await marker.press("Enter");
    await expect(page.locator(".photo-loupe")).toBeVisible();
    await expect(page.locator(".photo-viewport img")).toBeVisible({ timeout: 60_000 });
    await page.screenshot({
      path: path.join(output!, "photo-map-viewer-1440-dark.png"),
      animations: "disabled",
    });
    await page.keyboard.press("Escape");
    await expect(marker).toBeFocused();
    await marker.press("Enter");
    await expect(
      page.getByRole("dialog", { name: "Photo viewer, Synthetic-photo-00007.jpg" }),
    ).toBeVisible();
    const beforeClose = listings;
    await page
      .getByRole("navigation", { name: "Docbank navigation" })
      .getByRole("button", { name: "Map", exact: true })
      .click();
    await expect(page.getByRole("dialog", { name: /Photo viewer/ })).toHaveCount(0);
    expect(listings).toBe(beforeClose);
    const open9 = page.getByRole("button", { name: "Open Synthetic-photo-00009.jpg" });
    await open9.focus();
    await open9.press("Enter");
    await expect(
      page.getByRole("dialog", { name: "Photo viewer, Synthetic-photo-00009.jpg" }),
    ).toBeVisible();
    await page.setViewportSize({ width: 400, height: 860 });
    await expect(
      page.getByRole("button", { name: "Next photo", exact: true }).first(),
    ).toBeEnabled();
    await expect(
      page.getByRole("dialog", { name: "Photo viewer, Synthetic-photo-00009.jpg" }),
    ).toBeVisible();
    await expect(page.locator(".photo-viewport img")).toBeVisible({ timeout: 60_000 });
    await page.screenshot({
      path: path.join(output!, "photo-map-viewer-400-dark.png"),
      animations: "disabled",
    });
    await page.keyboard.press("Escape");
    await page.getByRole("button", { name: "Map", exact: true }).last().click();
    await expect(page.locator(".photo-map")).toBeVisible();
    await page.screenshot({
      path: path.join(output!, "photo-map-enabled-400-dark.png"),
      animations: "disabled",
    });
    await page.locator(".map-tabs").getByRole("button", { name: "Photos", exact: true }).click();
    await expect(page.getByTestId("photo-scroll").locator("img").first()).toBeVisible();
    await page.screenshot({
      path: path.join(output!, "photo-map-grid-400-dark.png"),
      animations: "disabled",
    });
    await page.setViewportSize({ width: 1440, height: 960 });
    await page.getByRole("button", { name: "Library", exact: true }).click();
    await expect(page.getByText("15 photos", { exact: true })).toBeVisible();
    await page
      .getByRole("navigation", { name: "Docbank navigation" })
      .getByRole("button", { name: /Paris walks/ })
      .click();
    await expect(page.getByRole("heading", { name: "Paris walks", exact: true })).toBeVisible();
    await expect(page.getByText("8 photos", { exact: true })).toBeVisible();
    await page
      .getByRole("navigation", { name: "Docbank navigation" })
      .getByRole("button", { name: "Map", exact: true })
      .click();
    await expect(page.getByText("7 located photos", { exact: true })).toBeVisible();
    await page.evaluate(() =>
      (window as typeof window & { photoMap: import("leaflet").Map }).photoMap.setView([0, 0], 0, {
        animate: false,
      }),
    );
    await expect(page.getByText("7 photos", { exact: true })).toBeVisible();
    await expect(page.getByTestId("photo-scroll").locator("img")).toHaveCount(7);
    for (const index of [1, 3, 5, 7, 9, 11, 13])
      await expect(
        page.getByRole("button", {
          name: `Open Synthetic-photo-${String(index).padStart(5, "0")}.jpg`,
          exact: true,
        }),
      ).toBeAttached();
    await expect(page.locator(".leaflet-tile-loaded")).toHaveCount(1);
    await expect(page.locator(".leaflet-tile-loaded")).toHaveCSS("opacity", "1");
    await page.screenshot({
      path: path.join(output!, "photo-map-album-1440-dark.png"),
      animations: "disabled",
    });
    await page.getByRole("button", { name: "Library", exact: true }).click();
    let releasePage!: () => void;
    const pendingPage = new Promise<void>((resolve) => (releasePage = resolve));
    let failed = false;
    await page.route("**/photos/assets/query", async (route) => {
      const request = route.request().postDataJSON();
      if (request.query.filters.asset_ids || !request.projection) {
        await route.fallback();
        return;
      }
      const url = new URL(route.request().url());
      const host = url.host;
      url.hostname = "127.0.0.1";
      const response = await route.fetch({
        url: url.href,
        headers: { ...(await route.request().allHeaders()), host },
      });
      await pendingPage;
      if (!failed) {
        failed = true;
        await route.fulfill({ status: 503, json: { detail: "Page unavailable" } });
      } else await route.fulfill({ response });
    });
    await page.setViewportSize({ width: 1440, height: 960 });
    await page
      .getByRole("navigation", { name: "Docbank navigation" })
      .getByRole("button", { name: "Map", exact: true })
      .click();
    await expect(page.getByText("Loading located photos: 0 of 0", { exact: true })).toBeVisible();
    await page.screenshot({
      path: path.join(output!, "photo-map-loading-1440-dark.png"),
      animations: "disabled",
    });
    await page.setViewportSize({ width: 400, height: 860 });
    await page.locator(".map-tabs").getByRole("button", { name: "Map", exact: true }).click();
    releasePage();
    await page.getByText("Page unavailable", { exact: true }).waitFor();
    await page.screenshot({
      path: path.join(output!, "photo-map-retry-400-dark.png"),
      animations: "disabled",
    });
    await page.locator(".map-tabs").getByRole("button", { name: "Photos", exact: true }).click();
    await expect(page.getByRole("button", { name: "Retry", exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Retry", exact: true }).click();
    await expect(page.getByText("14 located photos", { exact: true })).toBeVisible();
    await page.locator(".map-tabs").getByRole("button", { name: "Map", exact: true }).click();
    await expect(page.locator(".photo-map")).toBeVisible();
    await page.unroute("**/photos/assets/query");
    await page.goto("about:blank");
    await page.goto(enabledURL.href);
    await expect(page.getByText("14 located photos", { exact: true })).toBeVisible();
    await page.setViewportSize({ width: 400, height: 860 });
    await page.locator(".map-tabs").getByRole("button", { name: "Map", exact: true }).click();
    await page.evaluate(() =>
      (window as typeof window & { photoMap: import("leaflet").Map }).photoMap.setView(
        [40.748816666666667, -73.98565555555555],
        19,
        { animate: false },
      ),
    );
    await expect(page.locator(".leaflet-zoom-anim, .leaflet-cluster-anim")).toHaveCount(0);
    await expect(page.locator(".photo-map-cluster").filter({ hasText: /^2$/ })).toHaveCount(1);
    await page.locator(".photo-map-cluster").filter({ hasText: /^2$/ }).press("Enter");
    await expandedOverlap();
    await page.locator(".map-tabs").getByRole("button", { name: "Photos", exact: true }).click();
    await page.getByRole("checkbox", { name: "Select photo Synthetic-photo-00009.jpg" }).check();
    await page.getByRole("button", { name: "Move to trash", exact: true }).click();
    await page
      .getByRole("dialog", { name: "Move selected photos to trash" })
      .getByRole("button", { name: "Move to trash", exact: true })
      .click();
    await expect(page.getByRole("dialog", { name: "Move selected photos to trash" })).toHaveCount(
      0,
    );
    await expect(page.getByText("13 located photos", { exact: true })).toBeVisible();
    await expect(
      page.getByRole("button", { name: "Open Synthetic-photo-00010.jpg" }),
    ).toBeAttached();
    await expect(page.getByRole("button", { name: "Open Synthetic-photo-00009.jpg" })).toHaveCount(
      0,
    );
    await page.locator(".map-tabs").getByRole("button", { name: "Map", exact: true }).click();
    await expect(page.locator(".leaflet-cluster-anim")).toHaveCount(0);
    await page.locator('.photo-map-dot[title="Synthetic-photo-00010.jpg"]').press("Enter");
    await expect(
      page.getByRole("dialog", { name: "Photo viewer, Synthetic-photo-00010.jpg" }),
    ).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page.locator(".photo-map")).toBeVisible();
    await expect(page.locator('.photo-map-dot[title="Synthetic-photo-00010.jpg"]')).toBeFocused();
    await page.evaluate(() =>
      (window as typeof window & { photoMap: import("leaflet").Map }).photoMap.panBy([5, 0], {
        animate: false,
      }),
    );
    await expect(page.locator(".photo-map")).toBeVisible();
    await expect(page.locator(".leaflet-cluster-anim")).toHaveCount(0);
    await expect(page.locator('.photo-map-dot[title="Synthetic-photo-00010.jpg"]')).toHaveCSS(
      "opacity",
      "1",
    );
    await page.screenshot({
      path: path.join(output!, "photo-map-overlap-trash-400-dark.png"),
      animations: "disabled",
    });
    await page.route("**/photos/assets/query", (route) =>
      route.request().postDataJSON().query.filters.gps_bounds
        ? route.fulfill({ status: 503, json: { detail: "Map unavailable" } })
        : route.fallback(),
    );
    await page.evaluate(() => {
      Object.defineProperty(document, "visibilityState", { configurable: true, value: "hidden" });
      document.dispatchEvent(new Event("visibilitychange"));
    });
    const setup = exec(binary, ["photos", "hidden", "setup"], {
      cwd: repository,
      env,
      timeout: 60_000,
    });
    setup.child.stdin!.end("synthetic-passcode\n");
    await setup;
    const asset = JSON.parse(
      await run("photos", "assets", "inspect", "/Synthetic-photo-00015.jpg"),
    );
    // A changed Hidden state forces revalidation; unlocated photo 15 keeps map counts unchanged.
    await run("photos", "assets", "hide", asset.id, "--revision", String(asset.revision));
    await page.evaluate(() => {
      Object.defineProperty(document, "visibilityState", { configurable: true, value: "visible" });
      document.dispatchEvent(new Event("visibilitychange"));
    });
    const accessError = page.locator(".photo-access-error");
    await expect(accessError.getByRole("alert")).toHaveText("Map unavailable");
    await page.screenshot({
      path: path.join(output!, "photo-map-access-retry-400-dark.png"),
      animations: "disabled",
    });
    await page.evaluate(() => {
      Object.defineProperty(document, "visibilityState", { configurable: true, value: "hidden" });
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await page.unroute("**/photos/assets/query");
    await accessError.getByRole("button", { name: "Retry", exact: true }).evaluate((button) =>
      button.addEventListener(
        "click",
        () => {
          Object.defineProperty(document, "visibilityState", {
            configurable: true,
            value: "visible",
          });
        },
        { capture: true, once: true },
      ),
    );
    await accessError
      .getByRole("button", { name: "Retry", exact: true })
      .click({ timeout: 15_000 });
    await expect(page.getByText("13 located photos", { exact: true })).toBeVisible();
    providerFails = true;
    // Routing disables the browser cache so requests reach the failing provider.
    await page.route("**/api/daemon/web-map-tiles/**", (route) => route.continue());
    const beforeFailure = providerRequests;
    await page.setViewportSize({ width: 1440, height: 960 });
    await page.goto("about:blank");
    await page.goto(enabledURL.href);
    await expect(page.getByText("13 located photos", { exact: true })).toBeVisible();
    await expect(page.locator(".photo-map-cluster").first()).toBeVisible();
    await expect.poll(() => providerRequests).toBeGreaterThan(beforeFailure);
    await expect(page.getByRole("alert")).toHaveText(
      `Map tiles from ${providerHost} couldn't load. Check [web.map_tiles], then restart Docbank.`,
    );
    await page.screenshot({
      path: path.join(output!, "photo-map-tile-error-1440-dark.png"),
      animations: "disabled",
    });
    await page.setViewportSize({ width: 400, height: 860 });
    await page.locator(".map-tabs").getByRole("button", { name: "Map", exact: true }).click();
    await page.screenshot({
      path: path.join(output!, "photo-map-tile-error-400-dark.png"),
      animations: "disabled",
    });
    await page.route("**/assets/PhotoMap-*.js", (route) => route.abort());
    await page.setViewportSize({ width: 1440, height: 960 });
    await page.goto("about:blank");
    await page.goto(enabledURL.href);
    await expect(page.getByText("Map couldn't load", { exact: true })).toBeVisible();
    await expect(page.getByText("No photos in this area", { exact: true })).toHaveCount(0);
    await expect(page.getByRole("heading", { name: "Photos in view", exact: true })).toHaveCount(0);
    await expect(
      page.getByRole("button", { name: "Select visible photos", exact: true }),
    ).toHaveCount(0);
    await page.screenshot({
      path: path.join(output!, "photo-map-chunk-error-1440-dark.png"),
      animations: "disabled",
    });
    await page.unroute("**/assets/PhotoMap-*.js");
    await page.goto("about:blank");
    await page.goto(enabledURL.href);
    await expect(page.getByText("13 located photos", { exact: true })).toBeVisible();
    await expect(page.locator(".photo-map-cluster").first()).toBeVisible();
    expect(errors).toEqual([]);
    await writeFile(
      path.join(output!, "preview.json"),
      JSON.stringify({ url: enabledURL.href, workspace, binary }, null, 2),
    );
  } finally {
    provider?.close();
    if (!process.env.DOCBANK_KEEP_MAP_PREVIEW) {
      await run("daemon", "stop");
      await rm(workspace, { recursive: true, force: true });
    }
  }
});
