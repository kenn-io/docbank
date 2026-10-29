import { expect, test } from "@playwright/test";
import path from "node:path";
import { output, withPhotoFixture } from "./photo-workspace-fixture.js";

test.skip(!output, "DOCBANK_PHOTOS_SCREENSHOT_DIR enables synthetic photo proof");

test("centers Photos access loading and replaces it with loaded photos", async ({ page }) => {
  await withPhotoFixture(3, "loading", async (run) => {
    await page.goto(await run("web", "--no-browser"));
    await expect(page.getByRole("button", { name: "Photos", exact: true })).toBeVisible();
    let release!: () => void;
    const held = new Promise<void>((resolve) => (release = resolve));
    await page.route("**/api/v1/photos/hidden", async (route) => {
      const response = await route.fetch();
      await held;
      await route.fulfill({ response });
    });
    try {
      await page.getByRole("button", { name: "Photos", exact: true }).click();
      const spinner = page.getByRole("status", { name: "Loading", exact: true });
      await expect(spinner).toBeVisible();
      await page.screenshot({
        animations: "disabled",
        path: path.join(output!, "photos-loading.png"),
      });
      const content = await page.locator(".app-main").boundingBox();
      const toolbar = await page.locator(".app-main > header").boundingBox();
      const box = await spinner.boundingBox();
      expect(content).toBeTruthy();
      expect(box).toBeTruthy();
      const top = toolbar ? toolbar.y + toolbar.height : content!.y;
      expect(Math.abs(box!.x + box!.width / 2 - (content!.x + content!.width / 2))).toBeLessThan(2);
      expect(
        Math.abs(box!.y + box!.height / 2 - (top + (content!.y + content!.height - top) / 2)),
      ).toBeLessThan(2);
    } finally {
      release();
      await page.unrouteAll({ behavior: "wait" });
    }
    await expect(page.locator("[data-asset]")).toHaveCount(3);
    await expect(page.getByRole("status", { name: "Loading", exact: true })).toHaveCount(0);
    await page.screenshot({
      animations: "disabled",
      path: path.join(output!, "photos-loaded.png"),
    });
    await page.getByRole("button", { name: "Documents", exact: true }).click();
    await expect(page.getByRole("button", { name: "Searches", exact: true })).toBeVisible();
    const imports = page.getByRole("button", { name: "Imports", exact: true });
    await expect(imports.locator("svg.lucide-history")).toBeVisible();
    await imports.click();
    await expect(page.getByRole("main", { name: "Imports", exact: true })).toBeVisible();
    await page.screenshot({ animations: "disabled", path: path.join(output!, "imports.png") });
    await page.getByRole("button", { name: "Searches", exact: true }).click();
    await expect(page.getByRole("main", { name: "Searches", exact: true })).toBeVisible();
    await page.screenshot({ animations: "disabled", path: path.join(output!, "searches.png") });
  });
});
