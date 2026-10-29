import { expect, test } from "@playwright/test";
import path from "node:path";
import { withPhotoFixture, output } from "./photo-workspace-fixture.js";

test("rotation suggestion in dark and light themes", async ({ page }) => {
  test.setTimeout(120_000);
  page.setDefaultTimeout(15000);
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
    },
    false,
    ["-suggestions"],
  );
});
