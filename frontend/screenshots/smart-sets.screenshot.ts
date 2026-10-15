import { expect, test } from "@playwright/test";
import { mkdir, mkdtemp } from "node:fs/promises";
import path from "node:path";
import { photoFixture, repository } from "./photo-fixture.js";

const output = process.env.DOCBANK_SMART_SETS_SCREENSHOT_DIR;
test.skip(!output, "DOCBANK_SMART_SETS_SCREENSHOT_DIR enables synthetic smart set proof");

test("save, reopen, freeze and preserve a photo smart set as an album", async ({ page }) => {
  test.setTimeout(900_000);
  await page.setViewportSize({ width: 1440, height: 720 });
  await mkdir(path.join(repository, ".superpowers"), { recursive: true });
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "smart-set-proof-"));
  const { run, seed, dispose } = photoFixture(workspace);
  try {
    await mkdir(output!, { recursive: true });
    await seed(60);
    const url = new URL(await run("web", "--no-browser"));
    url.pathname = "/photos";
    await page.goto(url.href);
    await expect(page.locator(".photo-cell img").first()).toBeVisible();
    await page.getByRole("searchbox", { name: "Search photos" }).fill("Canon");
    await page.getByRole("button", { name: "Search", exact: true }).click();
    await expect(page.getByText("30 photos", { exact: true })).toBeVisible();
    await expect(
      page.locator(".library-title").getByText("30 loaded", { exact: true }),
    ).toBeVisible();
    const capture = async (name: string) => {
      await expect
        .poll(async () => page.locator(".photo-cell img").count())
        .toBe(await page.locator(".photo-cell").count());
      await page.evaluate(() => document.documentElement.classList.add("dark"));
      if (await page.locator(".photo-facets").count()) {
        await page.locator(".photo-facets").evaluate((element) => {
          element.scrollTop = element.scrollHeight;
        });
        for (const name of ["Rating facet", "Flag facet", "Color label facet"])
          await expect(page.getByRole("group", { name })).toBeInViewport({ ratio: 0.99 });
      }
      await page.screenshot({
        path: path.join(output!, `${name}-dark.png`),
        animations: "disabled",
        clip: { x: 0, y: 0, width: 1440, height: 720 },
      });
    };
    await capture("photo-search-after");
    await page.getByRole("button", { name: "Save search" }).click();
    await page.getByRole("textbox", { name: "Search name" }).fill("Canon photos");
    await page.getByRole("button", { name: "Save", exact: true }).click();
    await expect(page.getByText("Canon photos", { exact: true })).toBeVisible();
    await expect(page.getByText("Smart set", { exact: true })).toBeVisible();
    await capture("smart-set-saved-after");
    await page.locator("[data-albums-nav]").click();
    await page
      .getByRole("region", { name: "Smart sets" })
      .getByRole("button", { name: "Canon photos", exact: true })
      .click();
    await page.getByRole("button", { name: "Freeze results" }).click();
    await expect(page.getByText(/30 photos · Expires/)).toBeVisible();
    await page.locator("[data-albums-nav]").click();
    await page
      .getByRole("region", { name: "Smart sets" })
      .getByRole("button", { name: "Canon photos", exact: true })
      .click();
    await expect(page.getByText(/30 photos · Expires/)).toBeVisible();
    await expect
      .poll(async () => page.locator(".photo-cell img").count())
      .toBe(await page.locator(".photo-cell").count());
    await capture("smart-set-frozen-after");
    await page.getByRole("button", { name: "Save as album" }).click();
    await page.getByRole("textbox", { name: "Album name" }).fill("Contact sheet");
    await page.getByRole("button", { name: "Save", exact: true }).click();
    await expect(page.getByRole("heading", { name: "Contact sheet", exact: true })).toBeVisible();
    await page.locator("[data-albums-nav]").click();
    await expect(
      page
        .getByRole("region", { name: "Smart sets" })
        .getByRole("button", { name: "Canon photos", exact: true }),
    ).toBeVisible();
    await expect(page.getByRole("link", { name: /Contact sheet/ })).toBeVisible();
    await capture("smart-set-albums-after");
  } finally {
    await dispose();
  }
});
