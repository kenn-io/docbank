import { expect, test, type Locator, type Page } from "@playwright/test";
import { mkdir, mkdtemp } from "node:fs/promises";
import path from "node:path";
import * as api from "../src/generated/docbank.js";
import { photoFixture, repository, withPhotoAPI } from "./photo-fixture.js";

const output = process.env.DOCBANK_FACES_SCREENSHOT_DIR;
test.use({ actionTimeout: 15_000 });
test.skip(!output, "DOCBANK_FACES_SCREENSHOT_DIR enables synthetic face review proof");

async function captureDarkDialog(
  page: Page,
  dialog: Locator,
  directory: string,
  name: string,
  cropHeight: number,
  ready: () => Promise<unknown>,
) {
  await expect.poll(ready).toBe(true);
  await page.evaluate(async () => {
    localStorage.setItem("docbank-theme", "dark");
    document.documentElement.classList.add("dark");
    await document.fonts.ready;
  });
  const bounds = await dialog.boundingBox();
  const viewport = page.viewportSize();
  if (!bounds || !viewport) throw new Error("Photo dialog is not measurable");
  await page.screenshot({
    path: path.join(directory, `${name}-dark.png`),
    clip: {
      x: bounds.x,
      y: bounds.y,
      width: bounds.width,
      height: Math.min(cropHeight, bounds.height, viewport.height - bounds.y),
    },
    animations: "disabled",
  });
}

test("names and corrects faces in Loupe, inspector, and Cull", async ({ page }) => {
  test.setTimeout(300_000);
  await mkdir(path.join(repository, ".superpowers"), { recursive: true });
  const fixture = photoFixture(
    await mkdtemp(path.join(repository, ".superpowers", "faces-proof-")),
  );
  try {
    await fixture.seed(3, "--namesakes");
    await mkdir(output!, { recursive: true });
    const url = new URL(await fixture.run("web", "--no-browser"));
    const session = new URLSearchParams(url.hash.slice(1)).get("web_session")!;
    url.pathname = "/photos";
    await page.goto(url.href);
    await page.locator(".photo-image").first().dblclick();
    const loupe = page.getByRole("dialog", { name: /^Photo viewer,/ });
    const viewport = loupe.locator(".photo-viewport");
    const add = loupe.getByRole("button", { name: "Add face", exact: true });
    await expect(add).toBeEnabled();
    await add.click();
    const created = page.waitForResponse(
      (response) => response.request().method() === "POST" && response.url().endsWith("/faces"),
    );
    await loupe.getByRole("button", { name: /^Position face on/ }).focus();
    await page.keyboard.press("Enter");
    const face = (await (await created).json()).face;
    await loupe.getByRole("button", { name: "Metadata", exact: true }).click();
    const people = loupe.getByRole("region", { name: "People", exact: true });
    await people.getByRole("button", { name: "Find or create a person: Face 1" }).click();
    await people.getByRole("combobox").fill("Ada");
    await people.getByRole("option", { name: 'Create person "Ada"' }).click();
    await expect(loupe.locator(".face-box")).toHaveText("Face 1 · Ada");
    const personID = await withPhotoAPI(
      url,
      async () => (await api.listPeople({ query: "Ada" }, { session })).items[0]!.person_id,
    );
    const duplicate = JSON.parse(await fixture.run("people", "create", "Ada"));
    const otherPhotos = await withPhotoAPI(url, async () =>
      (
        await api.listPhotoAssets(
          {
            query: {
              text: "",
              syntax: "simple",
              sort: { field: "capture_time", direction: "desc" },
            },
            page_size: 10,
          },
          { session },
        )
      ).items.filter((row) => row.content_version_id !== face.content_version_id),
    );
    await fixture.run(
      "photos",
      "faces",
      "create",
      otherPhotos[0]!.content_version_id,
      "--person",
      personID,
    );
    await fixture.run(
      "photos",
      "faces",
      "create",
      otherPhotos[1]!.content_version_id,
      "--person",
      duplicate.person_id,
    );
    await people.getByRole("button", { name: "Find or create a person: Ada" }).click();
    await people.getByRole("option", { name: "Clear name", exact: true }).click();
    await expect(loupe.locator(".face-box")).toHaveText("Face 1");
    await people.getByRole("button", { name: "Find or create a person: Face 1" }).click();
    await people.getByRole("combobox").fill("Ada");
    await people.getByRole("option", { name: "Ada · 2 people", exact: true }).click();
    const chooser = page.getByRole("dialog", { name: "Choose Ada" });
    await expect(chooser.getByRole("img")).toHaveCount(2);
    await captureDarkDialog(page, chooser, output!, "after-photo-people-chooser", 760, async () =>
      chooser
        .getByRole("img")
        .evaluateAll((images) =>
          images.every((image) => (image as HTMLImageElement).naturalWidth > 0),
        ),
    );
    await chooser.locator(`input[value="${personID}"]`).check();
    await chooser.getByRole("button", { name: "Name face", exact: true }).click();
    await expect(loupe.locator(".face-box")).toHaveText("Face 1 · Ada");
    await captureDarkDialog(
      page,
      loupe,
      output!,
      "after-photo-faces-loupe",
      760,
      async () =>
        (await viewport
          .locator("img")
          .evaluate((image) => (image as HTMLImageElement).naturalWidth)) > 0,
    );
    await people.getByRole("button", { name: "Find or create a person: Ada" }).click();
    await people.getByRole("combobox").fill("Alex Smith");
    await people.getByRole("option", { name: "Alex Smith · 2 people", exact: true }).click();
    const identityChooser = page.getByRole("dialog", { name: "Choose Alex Smith" });
    await expect(identityChooser.getByText("alex-a@example.test")).toBeVisible();
    await expect(identityChooser.getByText("alex-b@example.test")).toBeVisible();
    await expect(identityChooser.getByText("No photos you can see.")).toHaveCount(2);
    await captureDarkDialog(
      page,
      identityChooser,
      output!,
      "after-photo-people-identities",
      760,
      async () => (await identityChooser.getByRole("img").count()) === 0,
    );
    await identityChooser.getByRole("button", { name: "Cancel", exact: true }).click();
    await loupe.getByRole("button", { name: "Close photo viewer" }).click();
    await page.setViewportSize({ width: 1440, height: 720 });
    await page.getByRole("button", { name: "Cull", exact: true }).click();
    const cull = page.getByRole("dialog", { name: "Cull photos" });
    const closeups = cull.getByRole("region", { name: "Face closeups" });
    await captureDarkDialog(page, cull, output!, "after-photo-faces-cull", 620, async () =>
      closeups.locator("img").evaluate((image) => (image as HTMLImageElement).naturalWidth > 0),
    );
  } finally {
    await fixture.dispose();
  }
});

test("pages to an existing person and shows authored namesake rotation", async ({ page }) => {
  test.setTimeout(300_000);
  await mkdir(path.join(repository, ".superpowers"), { recursive: true });
  const fixture = photoFixture(
    await mkdtemp(path.join(repository, ".superpowers", "faces-search-proof-")),
  );
  try {
    await fixture.seed(3, "--face-search");
    await mkdir(output!, { recursive: true });
    const url = new URL(await fixture.run("web", "--no-browser"));
    const session = new URLSearchParams(url.hash.slice(1)).get("web_session")!;
    url.pathname = "/photos";
    const photos = await withPhotoAPI(
      url,
      async () =>
        (
          await api.listPhotoAssets(
            {
              query: {
                text: "",
                syntax: "simple",
                sort: { field: "capture_time", direction: "desc" },
              },
              page_size: 10,
            },
            { session },
          )
        ).items,
    );
    expect(photos).toHaveLength(3);
    const [current, ...previews] = photos;
    expect(previews.some((photo) => photo.rotation === 90)).toBe(true);
    const namesakes = await withPhotoAPI(
      url,
      async () => (await api.listPeople({ query: "Robin" }, { session })).items,
    );
    expect(namesakes).toHaveLength(2);
    await fixture.run(
      "photos",
      "faces",
      "create",
      current!.content_version_id,
      "--box",
      "0.3,0.3,0.2,0.2",
    );
    for (const [index, person] of namesakes.entries())
      await fixture.run(
        "photos",
        "faces",
        "create",
        previews[1]!.content_version_id,
        "--person",
        person.person_id,
        "--box",
        `${index === 0 ? 0.15 : 0.65},0.3,0.2,0.2`,
      );
    await page.goto(url.href);
    await page.locator(".photo-image").first().dblclick();
    const loupe = page.getByRole("dialog", { name: /^Photo viewer,/ });
    await loupe.getByRole("button", { name: "Metadata", exact: true }).click();
    const people = loupe.getByRole("region", { name: "People", exact: true });
    await people.getByRole("button", { name: "Find or create a person: Face 1" }).click();
    await people.getByRole("combobox").fill("Smith");
    await expect(people.getByRole("option", { name: "Load more", exact: true })).toBeVisible();
    await expect(people.getByRole("option", { name: "Smith", exact: true })).toHaveCount(0);
    await expect(people.getByRole("option", { name: 'Create person "Smith"' })).toBeVisible();
    await people.getByRole("option", { name: "Load more", exact: true }).click();
    const existing = people.getByRole("option", { name: "Smith", exact: true });
    await existing.scrollIntoViewIfNeeded();
    await expect(existing).toBeVisible();
    await page.evaluate(async () => {
      localStorage.setItem("docbank-theme", "dark");
      document.documentElement.classList.add("dark");
      await document.fonts.ready;
    });
    await page.screenshot({
      path: path.join(output!, "after-photo-people-later-match-dark.png"),
      animations: "disabled",
    });
    await existing.click();
    await expect(loupe.locator(".face-box")).toHaveText("Face 1 · Smith");
    await people.getByRole("button", { name: "Find or create a person: Smith" }).click();
    await people.getByRole("combobox").fill("Robin");
    await people.getByRole("option", { name: "Robin · 2 people", exact: true }).click();
    const chooser = page.getByRole("dialog", { name: "Choose Robin" });
    await expect(chooser.getByRole("img")).toHaveCount(2);
    await expect(chooser.locator('img[style*="rotate(90deg)"]')).toHaveCount(2);
    await expect(chooser.getByRole("img").first()).toHaveAttribute("src", /^blob:/);
    await captureDarkDialog(page, chooser, output!, "after-photo-people-rotation", 760, async () =>
      chooser
        .getByRole("img")
        .evaluateAll((images) =>
          images.every((image) => (image as HTMLImageElement).naturalWidth > 0),
        ),
    );
  } finally {
    await fixture.dispose();
  }
});
