import { expect, test } from "@playwright/test";
import { mkdir, mkdtemp, writeFile } from "node:fs/promises";
import path from "node:path";
import { photoFixture, repository } from "./photo-fixture.js";
import { expectReadableTone } from "./tone-contrast.js";

test("keeps tag errors readable in both themes", async ({ page }) => {
  await mkdir(path.join(repository, ".superpowers"), { recursive: true });
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "tag-error-"));
  const fixture = photoFixture(workspace);
  try {
    const source = path.join(workspace, "synthetic.txt");
    await writeFile(source, "Synthetic document\n");
    await fixture.run("add", source, "--dest", "/");
    await page.goto(await fixture.run("web", "--no-browser"));
    await page.getByRole("combobox", { name: /^Browse or filter by tag:/ }).click();
    await page.getByRole("button", { name: "Manage tags", exact: true }).click();
    const catalog = page.getByRole("dialog", { name: "Manage tag definitions" });
    const name = catalog.getByRole("textbox", { name: "New tag name" });
    await name.fill("reviewed");
    await catalog.getByRole("button", { name: "Create" }).click();
    await expect(catalog).toContainText("Created reviewed.");
    await name.fill("reviewed");
    await catalog.getByRole("button", { name: "Create" }).click();
    const error = catalog.getByRole("alert");
    await expect(error).toBeVisible();
    for (const theme of ["light", "dark"] as const) {
      await page.evaluate((value) => {
        localStorage.setItem("docbank-theme", value);
        document.documentElement.classList.toggle("dark", value === "dark");
      }, theme);
      await expectReadableTone(error, theme, "danger");
    }
    await catalog.getByRole("button", { name: "Done" }).click();
    await page.getByRole("cell", { name: "synthetic.txt", exact: true }).click();
    await page.getByRole("button", { name: "Add tag", exact: true }).click();
    const manage = page.getByRole("dialog", { name: "Manage tags for synthetic.txt" });
    await page.route("**/api/v1/nodes/*/tags/*", (route) => route.fulfill({ status: 500, body: "Unavailable" }));
    await manage.getByRole("combobox", { name: "Tag to assign" }).fill("reviewed");
    await manage.getByRole("option", { name: "reviewed (0)" }).click();
    const assignError = manage.getByRole("alert");
    await expect(assignError).toBeVisible();
    for (const theme of ["light", "dark"] as const) {
      await page.evaluate((value) => {
        localStorage.setItem("docbank-theme", value);
        document.documentElement.classList.toggle("dark", value === "dark");
      }, theme);
      await expectReadableTone(assignError, theme, "danger");
    }
  } finally {
    await fixture.dispose();
  }
});

test("keeps the error banner readable in both themes", async ({ page }) => {
  await mkdir(path.join(repository, ".superpowers"), { recursive: true });
  const fixture = photoFixture(await mkdtemp(path.join(repository, ".superpowers", "banner-error-")));
  try {
    await page.route("**/api/v1/path?*", (route) => route.fulfill({ status: 500, body: "Unavailable" }));
    await page.goto(await fixture.run("web", "--no-browser"));
    const error = page.locator(".banner.error[role=alert]");
    await expect(error).toBeVisible();
    for (const theme of ["light", "dark"] as const) {
      await page.evaluate((value) => {
        localStorage.setItem("docbank-theme", value);
        document.documentElement.classList.toggle("dark", value === "dark");
      }, theme);
      await expectReadableTone(error, theme, "danger");
    }
  } finally {
    await fixture.dispose();
  }
});
