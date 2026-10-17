import { expect, test } from "@playwright/test";
import { mkdtemp } from "node:fs/promises";
import path from "node:path";
import { photoFixture, repository } from "./photo-fixture.js";

test("dismissal refreshes a Library suggestion search after browser back from an album", async ({
  page,
}) => {
  test.setTimeout(300_000);
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "dismiss-cache-"));
  const fixture = photoFixture(workspace);
  try {
    const url = new URL(await fixture.run("web", "--no-browser"));
    await fixture.seed(12, "--suggestions");
    url.pathname = "/photos";
    await page.goto(url.href);
    const search = page.getByRole("searchbox", { name: "Search photos", exact: true });
    await search.fill("QUAYSIDE");
    await page.getByRole("button", { name: "Search", exact: true }).click();
    await expect(
      page.getByRole("button", { name: "Select Synthetic-photo-00001.jpg", exact: true }),
    ).toBeVisible();
    await expect(page.getByText("Matched AI suggestion")).toBeVisible();
    const album = page.getByRole("button", { name: /^Paris walks \d+$/ });
    await expect(album).toBeVisible();
    await album.click();
    await expect(page.getByRole("heading", { name: "Paris walks", exact: true })).toBeVisible();
    await page
      .getByRole("button", { name: "Select Synthetic-photo-00001.jpg", exact: true })
      .click();
    await page.getByRole("button", { name: "Metadata", exact: true }).click();
    await page.getByRole("button", { name: "Dismiss", exact: true }).click();
    await expect(page.getByRole("region", { name: "Suggested photo metadata" })).toHaveCount(0);
    await page.goBack();
    await expect(search).toHaveValue("QUAYSIDE");
    await expect(
      page.getByRole("button", { name: "Select Synthetic-photo-00001.jpg", exact: true }),
    ).toHaveCount(0);
    await expect(page.getByText("Matched AI suggestion")).toHaveCount(0);
    await expect(page.getByText("Search results 0 photos", { exact: true })).toBeVisible();
  } finally {
    await fixture.dispose();
  }
});
