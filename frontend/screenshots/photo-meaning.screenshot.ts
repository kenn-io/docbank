import { expect, test } from "@playwright/test";
import { mkdir, mkdtemp } from "node:fs/promises";
import path from "node:path";
import { photoFixture, repository } from "./photo-fixture.js";

const output = process.env.DOCBANK_PHOTOS_SCREENSHOT_DIR;
test.skip(!output, "DOCBANK_PHOTOS_SCREENSHOT_DIR enables synthetic photo proof");

test("Meaning search explains metadata fallback and captures it for Cull", async ({ page }) => {
  test.setTimeout(300_000);
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "photo-meaning-"));
  const fixture = photoFixture(workspace);
  try {
    await fixture.seed(12);
    const url = new URL(await fixture.run("web", "--no-browser"));
    url.pathname = "/photos";
    await page.goto(url.href);
    await expect(page.getByRole("searchbox", { name: "Search photos" })).toBeVisible();
    await page.getByRole("combobox", { name: "Search mode: Metadata" }).click();
    await page.getByRole("option", { name: "Meaning", exact: true }).click();
    await page.getByRole("searchbox", { name: "Search photos" }).fill("Synthetic");
    const response = page.waitForResponse(
      (response) =>
        response.url().includes("/photos/assets/query") &&
        response.request().postDataJSON().query.mode === "semantic",
    );
    await page.getByRole("button", { name: "Search", exact: true }).click();
    const body = await (await response).json();
    expect(body.snapshot_id).toBeUndefined();
    expect(body.actual_mode).toBe("lexical");
    await expect(
      page.getByText("Image search is unavailable. Showing metadata matches."),
    ).toBeVisible();
    await expect(page.locator("[data-asset] img").first()).toBeVisible();
    await page.evaluate(() => document.fonts.ready);
    await mkdir(output!, { recursive: true });
    await page.screenshot({
      path: path.join(output!, "photo-meaning-fallback.png"),
      fullPage: true,
      animations: "disabled",
    });
    const cull = page.waitForRequest(
      (request) => request.url().includes("/workspace/queries") && request.method() === "POST",
    );
    await page.getByRole("button", { name: "Cull", exact: true }).click();
    const capture = (await cull).postDataJSON();
    expect(capture.snapshot_id).toBeUndefined();
    expect(capture.query).toMatchObject({ mode: "lexical", text: "Synthetic" });
    await expect(page.getByRole("dialog", { name: "Cull photos", exact: true })).toBeVisible();
    await page.screenshot({
      path: path.join(output!, "photo-meaning-cull.png"),
      fullPage: true,
      animations: "disabled",
    });
  } finally {
    await fixture.dispose();
  }
});
