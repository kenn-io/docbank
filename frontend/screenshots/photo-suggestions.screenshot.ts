import { expect, test } from "@playwright/test";
import { mkdir, mkdtemp } from "node:fs/promises";
import path from "node:path";
import { photoFixture, repository } from "./photo-fixture.js";

const output = process.env.DOCBANK_PHOTOS_SCREENSHOT_DIR;
test.skip(!output, "DOCBANK_PHOTOS_SCREENSHOT_DIR enables synthetic photo proof");

test("captures suggested photo metadata and its search badge", async ({ page }) => {
  test.setTimeout(300_000);
  await page.setViewportSize({ width: 2000, height: 1400 });
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "suggestions-proof-"));
  const fixture = photoFixture(workspace);
  try {
    await mkdir(output!, { recursive: true });
    const url = new URL(await fixture.run("web", "--no-browser"));
    await fixture.seed(12, "--suggestions");
    url.pathname = "/photos";
    await page.goto(url.href);
    await page.getByRole("searchbox", { name: "Search photos", exact: true }).fill("QUAYSIDE");
    await page.getByRole("button", { name: "Search", exact: true }).click();
    await expect(page.getByText("Matched AI suggestion")).toBeVisible();
    await page
      .getByRole("button", { name: "Select Synthetic-photo-00001.jpg", exact: true })
      .click();
    await page.getByRole("button", { name: "Metadata", exact: true }).click();
    await expect(
      page.getByRole("checkbox", { name: "A calm harbor under a blue sky." }),
    ).toBeVisible();
    const dismiss = page.getByRole("button", { name: "Dismiss", exact: true });
    await expect(dismiss).toBeInViewport({ ratio: 1 });
    const metadata = page.getByRole("region", { name: "Photo metadata", exact: true });
    await expect(metadata).toBeInViewport({ ratio: 1 });
    const metadataBounds = await metadata.boundingBox();
    expect(metadataBounds).not.toBeNull();
    expect(metadataBounds!.y).toBeGreaterThanOrEqual(70);
    expect(metadataBounds!.y + metadataBounds!.height).toBeLessThanOrEqual(1120);
    for (const state of ["pending", "partial", "accepted"]) {
      if (state === "partial") {
        await page.getByRole("checkbox", { name: "A calm harbor under a blue sky." }).check();
        await page.getByRole("button", { name: "Accept selected", exact: true }).click();
        await expect(
          page.getByRole("checkbox", { name: "A calm harbor under a blue sky." }),
        ).toHaveCount(0);
        await expect(page.getByRole("checkbox", { name: "coast", exact: true })).toBeVisible();
        await expect(dismiss).toBeVisible();
      }
      if (state === "accepted") {
        await page.getByRole("checkbox", { name: "coast", exact: true }).check();
        await page.getByRole("checkbox", { name: "harbor", exact: true }).check();
        await page.getByRole("button", { name: "Accept selected", exact: true }).click();
        await expect(
          page.getByRole("button", { name: "Accept selected", exact: true }),
        ).toHaveCount(0);
        await expect(dismiss).toBeVisible();
      }
      for (const theme of ["light", "dark"]) {
        await page.evaluate((theme) => {
          localStorage.setItem("docbank-theme", theme);
          document.documentElement.classList.toggle("dark", theme === "dark");
        }, theme);
        await page.screenshot({
          path: path.join(
            output!,
            `photo-suggestions-${state === "pending" ? "" : `${state}-`}${theme}.png`,
          ),
          animations: "disabled",
          clip: { x: 0, y: 70, width: 2000, height: 1050 },
        });
      }
    }
    await page.getByRole("searchbox", { name: "Search photos", exact: true }).fill("coast");
    await page.getByRole("button", { name: "Search", exact: true }).click();
    await expect(
      page.getByRole("button", { name: "Select Synthetic-photo-00001.jpg", exact: true }),
    ).toBeVisible();
    await expect(page.getByText("Matched AI suggestion")).toHaveCount(0);
    await page.screenshot({
      path: path.join(output!, "photo-suggestions-accepted-keyword.png"),
      animations: "disabled",
      clip: { x: 0, y: 70, width: 2000, height: 1050 },
    });
    await page.getByRole("searchbox", { name: "Search photos", exact: true }).fill("forest");
    await page.getByRole("button", { name: "Search", exact: true }).click();
    await page
      .getByRole("button", { name: "Select Synthetic-photo-00002.jpg", exact: true })
      .click();
    await page.getByRole("checkbox", { name: "A forest beside a lake." }).check();
    await page.getByRole("checkbox", { name: "forest", exact: true }).check();
    await page.getByRole("button", { name: "Accept selected", exact: true }).click();
    await expect(page.getByRole("region", { name: "Suggested photo metadata" })).toHaveCount(0);
    for (const theme of ["light", "dark"]) {
      await page.evaluate((theme) => {
        localStorage.setItem("docbank-theme", theme);
        document.documentElement.classList.toggle("dark", theme === "dark");
      }, theme);
      await page.screenshot({
        path: path.join(output!, `photo-suggestions-empty-${theme}.png`),
        animations: "disabled",
        clip: { x: 0, y: 70, width: 2000, height: 1050 },
      });
    }
  } finally {
    await fixture.dispose();
  }
});
