import { expect, test } from "@playwright/test";
import { mkdir, mkdtemp, readdir, utimes, writeFile } from "node:fs/promises";
import path from "node:path";
import { photoFixture, repository } from "./photo-fixture.js";
import { createSyntheticCameraFiles } from "./camera-fixture.js";

const output = process.env.DOCBANK_PHOTOS_SCREENSHOT_DIR;
test.skip(!output, "DOCBANK_PHOTOS_SCREENSHOT_DIR enables synthetic photo proof");

test("a change in one photo tab shows up in the other through one shared stream", async ({
  context,
}) => {
  test.setTimeout(180_000);
  await mkdir(path.join(repository, ".superpowers"), { recursive: true });
  const fixture = photoFixture(
    await mkdtemp(path.join(repository, ".superpowers", "live-tabs-proof-")),
  );
  let streams = 0;
  context.on("request", (request) => {
    if (new URL(request.url()).pathname === "/api/v1/photos/events") streams++;
  });
  try {
    await mkdir(output!, { recursive: true });
    await fixture.seed(3);
    const tabs = [];
    for (let i = 0; i < 2; i++) {
      // Each tab signs in with its own web session.
      const url = new URL(await fixture.run("web", "--no-browser"));
      url.pathname = "/photos";
      const page = await context.newPage();
      await page.goto(url.href);
      await expect(page.locator("[data-asset]")).toHaveCount(3);
      await page
        .locator("[data-asset]")
        .first()
        .getByRole("button", { name: /^Select / })
        .click();
      await expect(page.getByRole("region", { name: "Photo metadata" })).toBeVisible();
      tabs.push(page);
    }
    const [editing, watching] = tabs as [(typeof tabs)[number], (typeof tabs)[number]];
    const caption = (page: typeof editing) =>
      page.getByRole("region", { name: "Photo metadata" }).getByLabel("Caption", { exact: true });
    await expect(caption(watching)).toHaveValue("");
    // Web Locks give the origin one stream; the other tab follows its wake-ups.
    expect(streams).toBe(1);
    await caption(editing).fill("Captioned in the other tab");
    await editing.getByRole("button", { name: "Apply", exact: true }).click();
    await expect(caption(watching)).toHaveValue("Captioned in the other tab");
    for (const theme of ["dark", "light"]) {
      await watching.evaluate(
        (theme) => document.documentElement.classList.toggle("dark", theme === "dark"),
        theme,
      );
      await watching.screenshot({
        path: path.join(output!, `web-photo-tabs-${theme}.png`),
        animations: "disabled",
      });
    }

    await watching.getByRole("button", { name: "Documents", exact: true }).click();
    let release!: () => void;
    const held = new Promise<void>((resolve) => (release = resolve));
    let snapshotReady!: () => void;
    const snapshot = new Promise<void>((resolve) => (snapshotReady = resolve));
    await watching.route(
      "**/api/v1/photos/assets/query",
      async (route) => {
        const response = await route.fetch();
        snapshotReady();
        await held;
        await route.fulfill({ response });
      },
      { times: 1 },
    );
    await watching.getByRole("button", { name: "Photos", exact: true }).click();
    await snapshot;
    await fixture.run("mv", "/Synthetic-photo-00001.jpg", "/Renamed-in-CLI.jpg");
    await expect(
      editing.getByRole("button", { name: "Select Renamed-in-CLI.jpg", exact: true }),
    ).toBeVisible();
    release();
    await expect(
      watching.getByRole("button", { name: "Select Renamed-in-CLI.jpg", exact: true }),
    ).toBeVisible();
    expect(streams).toBe(1);
  } finally {
    await fixture.dispose();
  }
});

test("measure loaded-prefix refresh during a 10,000-photo CLI import", async ({ page }) => {
  test.skip(
    process.env.DOCBANK_PHOTO_IMPORT_MEASURE !== "1",
    "Enable the full import measurement explicitly",
  );
  test.setTimeout(1_200_000);
  await mkdir(path.join(repository, ".superpowers"), { recursive: true });
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "live-import-proof-"));
  const fixture = photoFixture(workspace);
  try {
    await mkdir(output!, { recursive: true });
    await fixture.seed(1000);
    const source = path.join(workspace, "camera");
    await createSyntheticCameraFiles(source, 10_000);
    // Incoming photos sort after the seeded 2022 photos, keeping the measured prefix fixed.
    const modified = new Date("2020-01-01T00:00:00Z");
    for (const file of await readdir(source))
      await utimes(path.join(source, file), modified, modified);
    const url = new URL(await fixture.run("web", "--no-browser"));
    url.pathname = "/photos";
    await page.goto(url.href);
    const listResponse = (response: import("@playwright/test").Response) =>
      new URL(response.url()).pathname === "/api/v1/photos/assets/query" &&
      response.request().postDataJSON()?.page_size === 250;
    await expect(page.locator("[data-asset]").first()).toBeVisible();
    for (let i = 0; i < 3; i++) {
      const height = await page
        .getByTestId("photo-scroll")
        .evaluate((element) => element.scrollHeight);
      const next = page.waitForResponse(
        (response) => listResponse(response) && !!response.request().postDataJSON()?.cursor,
      );
      await page.getByTestId("photo-scroll").evaluate((element) => {
        element.scrollTop = element.scrollHeight;
      });
      await (await next).finished();
      await expect
        .poll(() => page.getByTestId("photo-scroll").evaluate((element) => element.scrollHeight))
        .toBeGreaterThan(height);
    }
    // Measure from the start of the loaded prefix.
    await page.getByTestId("photo-scroll").evaluate((element) => {
      element.scrollTop = 0;
    });
    // Measure complete replacement passes; cursor requests belong to the latest first page.
    const passes: { milliseconds: number; requests: number; rows: number; bytes: number }[] = [];
    let began = 0,
      total = 0;
    page.on("request", (request) => {
      if (
        new URL(request.url()).pathname !== "/api/v1/photos/assets/query" ||
        request.postDataJSON()?.page_size !== 250
      )
        return;
      if (!request.postDataJSON()?.cursor) {
        began = performance.now();
        passes.push({ milliseconds: 0, requests: 0, rows: 0, bytes: 0 });
      }
    });
    page.on("response", async (response) => {
      if (!listResponse(response) || !passes.length) return;
      const body = await response.body();
      const result = JSON.parse(body.toString()) as { total: number; items: unknown[] };
      const pass = passes.at(-1)!;
      pass.requests++;
      pass.rows += result.items.length;
      pass.bytes += body.byteLength;
      pass.milliseconds = performance.now() - began;
      total = result.total;
    });
    const started = performance.now();
    await fixture.run("photos", "import", source, "/Imported");
    await expect.poll(() => total, { timeout: 1_080_000, intervals: [500, 1000] }).toBe(11_000);
    await expect.poll(() => passes.at(-1)?.rows).toBe(1000);
    expect(passes.length).toBeGreaterThan(1);
    for (const pass of passes) {
      expect(pass.requests).toBe(4);
      expect(pass.rows).toBe(1000);
    }
    page.removeAllListeners("response");
    page.removeAllListeners("request");
    await writeFile(
      path.join(output!, "photo-import-refresh.json"),
      JSON.stringify(
        {
          imported: 10_000,
          loadedPrefix: 1000,
          total,
          elapsedMilliseconds: performance.now() - started,
          passes,
        },
        null,
        2,
      ),
    );
  } finally {
    await fixture.dispose();
  }
});
