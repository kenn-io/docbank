import { expect, test } from "@playwright/test";
import { mkdir, mkdtemp } from "node:fs/promises";
import path from "node:path";
import { photoFixture, repository } from "./photo-fixture.js";
import { expectReadableTone } from "./tone-contrast.js";

test("keeps face grades readable in both themes", async ({ page }) => {
  await mkdir(path.join(repository, ".superpowers"), { recursive: true });
  const fixture = photoFixture(
    await mkdtemp(path.join(repository, ".superpowers", "face-grades-proof-")),
  );
  try {
    await fixture.seed(3, "--face-grades");
    const url = new URL(await fixture.run("web", "--no-browser"));
    url.pathname = "/photos";
    await page.goto(url.href);
    await page.getByRole("button", { name: "Cull", exact: true }).click();
    const cull = page.getByRole("dialog", { name: "Cull photos" });
    await cull.getByRole("button", { name: "Show Synthetic-photo-00003.jpg", exact: true }).click();
    const grades = cull.getByRole("region", { name: "Face closeups" }).locator(".grade");
    await expect(grades).toHaveText(["green", "yellow", "red"]);
    for (const theme of ["light", "dark"] as const) {
      await page.evaluate((value) => {
        localStorage.setItem("docbank-theme", value);
        document.documentElement.classList.toggle("dark", value === "dark");
      }, theme);
      await expectReadableTone(grades.nth(0), theme, "success");
      await expectReadableTone(grades.nth(1), theme, "warning");
      await expectReadableTone(grades.nth(2), theme, "danger");
    }
  } finally {
    await fixture.dispose();
  }
});
