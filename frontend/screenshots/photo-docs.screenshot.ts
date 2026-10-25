import { expect, test, type Page } from "@playwright/test";
import { mkdir, mkdtemp } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";

import { photoFixture } from "./photo-fixture.js";

const output = process.env.DOCBANK_SCREENSHOT_DIR;
if (!output) throw new Error("DOCBANK_SCREENSHOT_DIR is required");

async function capture(page: Page, name: string) {
  await expect
    .poll(async () => page.locator(".photo-cell img").count())
    .toBe(await page.locator(".photo-cell").count());
  await page.screenshot({ path: path.join(output!, name), animations: "disabled" });
}

// Each step starts from a freshly loaded Library. Returning to the same URL
// changes only its fragment, which keeps selections and the active smart set.
async function openLibrary(page: Page, run: (...args: string[]) => Promise<string>) {
  const url = new URL(await run("web", "--no-browser"));
  url.pathname = "/photos";
  await page.goto("about:blank");
  await page.goto(url.href);
  await expect(page.locator(".photo-cell img").first()).toBeVisible();
}

// Library opens with its filter panel collapsed; the docs show the facets.
async function openFilters(page: Page) {
  const filters = page.getByRole("button", { name: /^Filters/ });
  if ((await filters.getAttribute("aria-expanded")) !== "true") await filters.click();
}

async function selectFirst(page: Page, count: number) {
  for (let index = 0; index < count; index++) {
    await page.locator(".photo-cell").nth(index).getByRole("checkbox").click();
  }
}

test("photo library, viewer, timeline, search, albums, cull and trash", async ({ page }) => {
  test.setTimeout(600_000);
  page.setDefaultTimeout(30_000);
  await mkdir(output, { recursive: true, mode: 0o700 });
  const fixture = photoFixture(await mkdtemp(path.join(tmpdir(), "docbank-photo-docs-")));
  try {
    await fixture.seed(150, "-map", "-rejects", "-current-metadata");
    await openLibrary(page, fixture.run);
    await openFilters(page);
    await expect(page.getByRole("group", { name: "Camera facet" })).toBeVisible();
    await capture(page, "web-photo-library.png");

    await page.locator(".photo-cell .photo-image").first().dblclick();
    const viewer = page.getByRole("dialog", { name: /^Photo viewer/ });
    await expect(page.getByText("Fit preview", { exact: true })).toBeVisible({ timeout: 60_000 });
    await viewer.getByRole("button", { name: "Metadata", exact: true }).click();
    await expect(viewer.getByRole("complementary", { name: "Photo details" })).toBeVisible();
    await expect(viewer.getByText(/^Loading metadata/)).toHaveCount(0);
    await page.screenshot({ path: path.join(output, "web-photo-viewer.png") });
    await page.keyboard.press("Escape");
    await expect(viewer).toHaveCount(0);

    await openLibrary(page, fixture.run);
    await page.getByRole("button", { name: "Timeline", exact: true }).click();
    await expect(page.getByRole("navigation", { name: "Timeline years" })).toBeVisible();
    await capture(page, "web-photo-timeline.png");

    await openLibrary(page, fixture.run);
    await page.getByRole("searchbox", { name: "Search photos" }).fill("Nikon");
    await page.getByRole("searchbox", { name: "Search photos" }).press("Enter");
    await page.getByRole("button", { name: "Save search" }).click();
    await page.getByRole("textbox", { name: "Search name" }).fill("Nikon walks");
    await page.getByRole("button", { name: "Save", exact: true }).click();
    await expect(page.getByText("Smart set", { exact: true })).toBeVisible();
    await capture(page, "web-photo-smart-set.png");

    await page.locator("[data-albums-nav]").click();
    await expect(page.getByRole("region", { name: "Smart sets" })).toBeVisible();
    await page.screenshot({ path: path.join(output, "web-photo-albums.png") });

    await openLibrary(page, fixture.run);
    await page.getByRole("button", { name: "Cull", exact: true }).click();
    const cull = page.getByRole("dialog", { name: "Cull photos" });
    await expect(cull.getByRole("button", { name: "Pick", exact: true })).toBeEnabled();
    await expect(cull.locator(".photo-stage img")).toBeVisible();
    await page.screenshot({ path: path.join(output, "web-photo-cull.png") });
    await cull.getByRole("button", { name: "Exit cull", exact: true }).click();

    await openLibrary(page, fixture.run);
    await selectFirst(page, 3);
    await page.getByRole("button", { name: "Compare", exact: true }).click();
    await expect(page.locator(".frames img").first()).toBeVisible();
    await page.screenshot({ path: path.join(output, "web-photo-compare.png") });
    await page.getByRole("button", { name: "Exit compare", exact: true }).click();

    await openLibrary(page, fixture.run);
    await page.getByRole("button", { name: "More photo actions" }).click();
    await page.getByRole("menuitem", { name: "Trash rejected photos", exact: true }).click();
    const rejects = page.getByRole("dialog", { name: "Trash rejected photos" });
    await expect(rejects.getByRole("button", { name: /^Move [1-9]\d* to trash$/ })).toBeVisible();
    await page.screenshot({ path: path.join(output, "web-photo-rejects.png") });
    await rejects.getByRole("button", { name: "Keep in Docbank", exact: true }).click();

    await openLibrary(page, fixture.run);
    await selectFirst(page, 3);
    await page.getByRole("button", { name: "Export selection", exact: true }).click();
    await expect(page.getByRole("button", { name: "Prepare", exact: true })).toBeInViewport();
    await page.screenshot({
      path: path.join(output, "web-photo-export.png"),
      animations: "disabled",
    });

    await openLibrary(page, fixture.run);
    await page.getByRole("button", { name: "Hidden", exact: true }).click();
    await page.getByLabel("Passcode", { exact: true }).fill("synthetic-passcode");
    await page.getByRole("button", { name: "Set passcode", exact: true }).click();
    await page.getByRole("button", { name: "Lock", exact: true }).click();
    await expect(page.getByRole("button", { name: "Unlock", exact: true })).toBeEnabled();
    await page.screenshot({ path: path.join(output, "web-photo-hidden.png") });
  } finally {
    await fixture.dispose();
  }
});

test("burst stacks in the library and in cull", async ({ page }) => {
  test.setTimeout(300_000);
  page.setDefaultTimeout(30_000);
  await mkdir(output, { recursive: true, mode: 0o700 });
  const fixture = photoFixture(await mkdtemp(path.join(tmpdir(), "docbank-photo-docs-")));
  try {
    await fixture.seed(24, "-bursts", "-current-metadata");
    await openLibrary(page, fixture.run);
    await expect(
      page.getByRole("button", { name: "Expand stack of 3 photos" }).first(),
    ).toBeVisible();
    await capture(page, "web-photo-stacks.png");
    await page.getByRole("button", { name: "Cull", exact: true }).click();
    const cull = page.getByRole("dialog", { name: "Cull photos" });
    await expect(cull.getByRole("button", { name: "Keep current", exact: true })).toBeEnabled();
    await expect(cull.locator(".photo-stage img")).toBeVisible();
    await page.screenshot({ path: path.join(output, "web-photo-cull-stack.png") });
  } finally {
    await fixture.dispose();
  }
});
