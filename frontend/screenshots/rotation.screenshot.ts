import { expect, test } from "@playwright/test";
import path from "node:path";
import { withPhotoFixture, output } from "./photo-workspace-fixture.js";

test("rotation suggestion saves only after the owner responds", async ({ page }) => {
  test.setTimeout(120_000);
  page.setDefaultTimeout(15000);
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await withPhotoFixture(
    12,
    "rotation",
    async (run) => {
      const url = new URL(await run("web", "--no-browser"));
      url.pathname = "/photos";
      await page.goto(url.href);
      await page
        .getByRole("button", { name: "Select Synthetic-photo-00001.jpg", exact: true })
        .click();
      const inspector = page.getByRole("region", { name: "Photo metadata" });
      await expect(inspector.getByText(/^Suggested 270°/)).toBeVisible();
      await expect(inspector.getByText("from photo description", { exact: true })).toBeVisible();
      const chip = inspector.locator(".rotation-suggestion");
      await chip.scrollIntoViewIfNeeded();
      for (const theme of ["dark", "light"]) {
        await page.evaluate((value) => {
          localStorage.setItem("docbank-theme", value);
          document.documentElement.classList.toggle("dark", value === "dark");
        }, theme);
        await page.screenshot({
          path: path.join(output!, `web-photo-rotation-${theme}.png`),
          animations: "disabled",
        });
      }
      await inspector.getByRole("button", { name: "Rotate 270°", exact: true }).click();
      await expect(inspector.getByText(/^Suggested 270°/)).toHaveCount(0);
      await expect(
        inspector.getByRole("combobox", { name: "Rotation: 270°", exact: true }),
      ).toBeVisible();
      const reopened = new URL(await run("web", "--no-browser"));
      reopened.pathname = "/photos";
      await page.goto(reopened.href);
      await page
        .getByRole("button", { name: "Select Synthetic-photo-00001.jpg", exact: true })
        .click();
      await expect(
        inspector.getByRole("combobox", { name: "Rotation: 270°", exact: true }),
      ).toBeVisible();
      expect(errors).toEqual([]);
    },
    false,
    ["-suggestions"],
  );
});
