import { expect, test } from "@playwright/test";
import { mkdir, mkdtemp } from "node:fs/promises";
import path from "node:path";
import * as api from "../src/generated/docbank.js";
import { captureTheme } from "./capture-themes.js";
import { photoFixture, repository, withPhotoAPI } from "./photo-fixture.js";

const output = process.env.DOCBANK_PEOPLE_SCREENSHOT_DIR;
test.skip(!output, "DOCBANK_PEOPLE_SCREENSHOT_DIR enables synthetic People proof");

test("People cards, rename, merge, and Library navigation", async ({ page }) => {
  test.setTimeout(300_000);
  await mkdir(path.join(repository, ".superpowers"), { recursive: true });
  const fixture = photoFixture(
    await mkdtemp(path.join(repository, ".superpowers", "people-proof-")),
  );
  try {
    await fixture.seed(3);
    const ada = JSON.parse(await fixture.run("people", "create", "Ada"));
    const grace = JSON.parse(await fixture.run("people", "create", "Grace Hopper"));
    await fixture.run("people", "create", "Morgan Lee");
    const url = new URL(await fixture.run("web", "--no-browser"));
    const session = new URLSearchParams(url.hash.slice(1)).get("web_session")!;
    const photos = await withPhotoAPI(url, () =>
      api.listPhotoAssets({ query: { text: "", syntax: "simple" }, page_size: 10 }, { session }),
    );
    const synthetic = photos.items.filter((photo) => photo.name.startsWith("Synthetic-photo-"));
    await fixture.run(
      "photos",
      "faces",
      "create",
      synthetic[0]!.content_version_id,
      "--box",
      "0.15,0.3,0.2,0.2",
      "--person",
      ada.person_id,
    );
    await fixture.run(
      "photos",
      "faces",
      "create",
      synthetic[1]!.content_version_id,
      "--box",
      "0.65,0.3,0.2,0.2",
      "--person",
      grace.person_id,
    );
    url.pathname = "/photos/people";
    await page.goto(url.href);
    await page.setViewportSize({ width: 1440, height: 800 });
    const people = page.getByRole("main", { name: "People", exact: true });
    await expect(people.getByRole("img")).toHaveCount(2);
    await expect(people.getByRole("button", { name: "Morgan Lee 0 Library photos" })).toBeVisible();
    await mkdir(output!, { recursive: true });
    await captureTheme(page, output!, "after-people-cards", "dark");
    await people.getByRole("button", { name: "Rename", exact: true }).first().click();
    const rename = page.getByRole("dialog", { name: "Rename Ada" });
    await rename.getByRole("textbox", { name: "Person name" }).fill("Ada Lovelace");
    await rename.getByRole("button", { name: "Rename", exact: true }).click();
    await expect(rename).toBeHidden();
    await people.getByRole("checkbox", { name: "Select Ada Lovelace" }).check();
    await people.getByRole("checkbox", { name: "Select Grace Hopper" }).check();
    await people.getByRole("button", { name: "Merge", exact: true }).click();
    const merge = page.getByRole("dialog", { name: "Merge people" });
    await merge.getByRole("radio", { name: /Keep Grace Hopper/ }).check();
    await merge.getByRole("checkbox", { name: "Confirm merge" }).check();
    await captureTheme(page, output!, "after-people-merge", "dark");
    await page.setViewportSize({ width: 390, height: 844 });
    await expect(merge).toBeVisible();
    expect(await merge.evaluate((dialog) => dialog.scrollWidth <= dialog.clientWidth)).toBe(true);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(
      true,
    );
    await captureTheme(page, output!, "after-people-narrow", "dark");
    await merge.getByRole("button", { name: "Merge", exact: true }).click();
    await expect(merge).toBeHidden();
    await expect(
      people.getByRole("button", { name: /Grace Hopper 2 Library photos/ }),
    ).toBeVisible();
    await people.getByRole("button", { name: /Grace Hopper 2 Library photos/ }).click();
    await expect(page).toHaveURL(/\/photos(?:[?#]|$)/);
    await expect(page.getByText("Person: Grace Hopper", { exact: true })).toBeVisible();
    await page.getByRole("button", { name: "People", exact: true }).click();
    await expect(
      people.getByRole("button", { name: /Grace Hopper 2 Library photos/ }),
    ).toBeVisible();
  } finally {
    await fixture.dispose();
  }
});
