import { expect, test } from "@playwright/test";
import path from "node:path";
import { output, withPhotoFixture } from "./photo-workspace-fixture.js";

test.use({
  viewport: { width: 1440, height: 720 },
  deviceScaleFactor: Number(process.env.DOCBANK_SCREENSHOT_SCALE ?? 1),
});
test.skip(!output, "DOCBANK_PHOTOS_SCREENSHOT_DIR enables synthetic photo proof");

test("Move rejects recovers position and selection after a failed refresh", async ({ page }) => {
  test.setTimeout(480_000);
  page.setDefaultTimeout(15_000);
  await withPhotoFixture(
    750,
    "rejects",
    async (run) => {
      const webURL = new URL(await run("web", "--no-browser"));
      webURL.pathname = "/photos";
      await page.goto(webURL.href);
      const scroll = page.getByTestId("photo-scroll");
      for (const loaded of [250, 500, 749]) {
        await expect(
          page.getByText(`749 photos · ${loaded} loaded`, { exact: true }),
        ).toBeVisible();
        if (loaded < 749)
          await scroll.evaluate((element) => (element.scrollTop = element.scrollHeight));
      }
      const select = async (name: string) => {
        const checkbox = page.getByRole("checkbox", { name: `Select photo ${name}`, exact: true });
        const height = await scroll.evaluate((element) => element.scrollHeight);
        const step = await scroll.evaluate((element) => element.clientHeight);
        for (let top = 0; top <= height; top += step) {
          await scroll.evaluate((element, value) => (element.scrollTop = value), top);
          await page.evaluate(
            () =>
              new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))),
          );
          if (await checkbox.count()) {
            await checkbox.check();
            return;
          }
        }
        throw new Error(`Photo was not mounted: ${name}`);
      };
      await scroll.evaluate((element) => (element.scrollTop = 0));
      await page.getByRole("button", { name: "Move rejects", exact: true }).click();
      const modal = page.getByRole("dialog", { name: "Move rejects to trash" });
      await modal.getByText("You can restore these photos from Trash.", { exact: true }).waitFor();
      for (const theme of ["light", "dark"]) {
        await page.evaluate((value) => {
          localStorage.setItem("docbank-theme", value);
          document.documentElement.classList.toggle("dark", value === "dark");
        }, theme);
        await page.screenshot({
          path: path.join(output!, `web-photo-rejects-${theme}.png`),
          animations: "disabled",
        });
      }
      await modal.getByRole("button", { name: "Keep in Docbank", exact: true }).click();
      await page
        .getByRole("checkbox", { name: /^Select photo / })
        .first()
        .check();
      await page.getByRole("button", { name: "Select loaded photos", exact: true }).click();
      await page.getByRole("button", { name: "Move rejects", exact: true }).click();
      await modal.getByText("You can restore these photos from Trash.", { exact: true }).waitFor();
      for (const theme of ["light", "dark"]) {
        await page.evaluate((value) => {
          localStorage.setItem("docbank-theme", value);
          document.documentElement.classList.toggle("dark", value === "dark");
        }, theme);
        await page.screenshot({
          path: path.join(output!, `web-photo-rejects-selection-limit-${theme}.png`),
          animations: "disabled",
        });
      }
      await modal.getByRole("button", { name: "Keep in Docbank", exact: true }).click();
      await page.getByRole("button", { name: "Clear selection", exact: true }).click();
      await select("Synthetic-photo-00001.jpg");
      await page.getByRole("button", { name: "Move rejects", exact: true }).click();
      await modal
        .getByRole("combobox", { name: "Rejects scope: Current view", exact: true })
        .click();
      await page.getByRole("option", { name: "Selected photos (1)", exact: true }).click();
      await modal.getByText("You can restore these photos from Trash.", { exact: true }).waitFor();
      for (const theme of ["light", "dark"]) {
        await page.evaluate((value) => {
          localStorage.setItem("docbank-theme", value);
          document.documentElement.classList.toggle("dark", value === "dark");
        }, theme);
        await page.screenshot({
          path: path.join(output!, `web-photo-rejects-selected-${theme}.png`),
          animations: "disabled",
        });
      }
      await modal.getByRole("button", { name: "Keep in Docbank", exact: true }).click();
      await select("Synthetic-photo-00005.jpg");
      await select("Synthetic-photo-00003.jpg");
      const beforeMoveTop = await scroll.evaluate((element) => element.scrollTop);
      expect(beforeMoveTop).toBeGreaterThan(0);
      await page.getByRole("button", { name: "Move rejects", exact: true }).click();
      await modal
        .getByRole("combobox", { name: "Rejects scope: Current view", exact: true })
        .click();
      await page.getByRole("option", { name: "Selected photos (3)", exact: true }).click();
      await page.route(
        "**/api/v1/photos/assets/query",
        (route) =>
          route.fulfill({
            status: 503,
            contentType: "application/problem+json",
            body: JSON.stringify({ detail: "Photo reload unavailable" }),
          }),
        { times: 1 },
      );
      await modal.getByRole("button", { name: "Move 1 to trash", exact: true }).click();
      await expect(modal).toHaveCount(0);
      await expect(page.getByRole("button", { name: "Retry", exact: true })).toBeVisible();
      await expect(scroll).toBeVisible();
      const retainedTop = await scroll.evaluate((element) => element.scrollTop);
      expect(retainedTop).toBeGreaterThan(0);
      await expect(page.locator(".library-title span")).toBeVisible();
      for (const theme of ["light", "dark"]) {
        await page.evaluate((value) => {
          localStorage.setItem("docbank-theme", value);
          document.documentElement.classList.toggle("dark", value === "dark");
        }, theme);
        await page.screenshot({
          path: path.join(output!, `web-photo-rejects-retry-${theme}.png`),
          animations: "disabled",
        });
      }
      await page.getByRole("button", { name: "Retry", exact: true }).click();
      await expect(page.getByText("748 photos · 748 loaded", { exact: true })).toBeVisible();
      await expect(page.getByText("2 selected photos", { exact: true })).toBeVisible();
      await expect
        .poll(() => scroll.evaluate((element) => element.scrollTop))
        .toBeCloseTo(retainedTop, 0);
      for (const theme of ["light", "dark"]) {
        await page.evaluate((value) => {
          localStorage.setItem("docbank-theme", value);
          document.documentElement.classList.toggle("dark", value === "dark");
        }, theme);
        await page.screenshot({
          path: path.join(output!, `web-photo-rejects-recovered-${theme}.png`),
          animations: "disabled",
        });
      }
      await page.getByRole("button", { name: "Recoverable trash", exact: true }).click();
      await expect(page.getByText("Synthetic-photo-00001.jpg", { exact: true })).toBeVisible();
    },
    true,
  );
});
