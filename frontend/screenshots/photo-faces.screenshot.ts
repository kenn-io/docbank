import { expect, test, type Locator, type Page } from "@playwright/test";
import { mkdir, mkdtemp } from "node:fs/promises";
import path from "node:path";
import * as api from "../src/generated/docbank.js";
import { captureThemes } from "./capture-themes.js";
import { photoFixture, repository, withPhotoAPI } from "./photo-fixture.js";

const output = process.env.DOCBANK_FACES_SCREENSHOT_DIR;
test.use({ actionTimeout: 15_000 });
test.skip(!output, "DOCBANK_FACES_SCREENSHOT_DIR enables synthetic face review proof");

async function captureDialogThemes(
  page: Page,
  dialog: Locator,
  directory: string,
  name: string,
  cropHeight: number,
  ready: () => Promise<unknown>,
) {
  await expect.poll(ready).toBe(true);
  const bounds = await dialog.boundingBox();
  const viewport = page.viewportSize();
  if (!bounds || !viewport) throw new Error("Photo dialog is not measurable");
  const height = Math.min(cropHeight, bounds.height, viewport.height - bounds.y);
  const width = Math.min(viewport.width, Math.max(bounds.width, height * 2));
  await captureThemes(page, directory, name, {
    x: Math.max(0, Math.min(bounds.x + (bounds.width - width) / 2, viewport.width - width)),
    y: bounds.y,
    width,
    height,
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
    await page
      .getByRole("button", { name: "Select Synthetic-photo-00003.jpg", exact: true })
      .dblclick();
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
    let failed = false;
    await page.route("**/api/v1/people?*", async (route) => {
      if (!failed && new URL(route.request().url()).searchParams.get("query") === "Ada") {
        failed = true;
        await route.abort("failed");
      } else await route.continue();
    });
    await people.getByRole("combobox").fill("Ada");
    await captureDialogThemes(page, loupe, output!, "after-photo-people-retry", 760, async () =>
      people.getByRole("option", { name: "Retry", exact: true }).isVisible(),
    );
    await people.getByRole("option", { name: "Retry", exact: true }).click();
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
      ).items.filter(
        (row) =>
          row.name.startsWith("Synthetic-photo-") &&
          row.content_version_id !== face.content_version_id,
      ),
    );
    await fixture.run(
      "photos",
      "faces",
      "create",
      otherPhotos[0]!.content_version_id,
      "--box",
      "0.15,0.3,0.2,0.2",
      "--person",
      personID,
    );
    await fixture.run(
      "photos",
      "faces",
      "create",
      otherPhotos[1]!.content_version_id,
      "--box",
      "0.65,0.3,0.2,0.2",
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
    await expect(chooser.getByRole("img")).toHaveCount(0);
    await chooser.getByRole("button", { name: "Photos", exact: true }).first().click();
    await expect(chooser.getByRole("img")).toHaveCount(1);
    await expect(chooser.getByRole("radio", { name: personID })).toBeVisible();
    await captureDialogThemes(page, chooser, output!, "after-photo-people-chooser", 760, async () =>
      chooser
        .getByRole("img")
        .evaluateAll((images) =>
          images.every((image) => (image as HTMLImageElement).naturalWidth > 0),
        ),
    );
    await chooser.locator(`input[value="${personID}"]`).check();
    await chooser.getByRole("button", { name: "Name face", exact: true }).click();
    await expect(loupe.locator(".face-box")).toHaveText("Face 1 · Ada");
    await captureDialogThemes(
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
    await expect(identityChooser.getByRole("radio", { name: "alex-a@example.test" })).toBeVisible();
    await expect(identityChooser.getByRole("radio", { name: "alex-b@example.test" })).toBeVisible();
    const alexA = await identityChooser
      .getByRole("radio", { name: "alex-a@example.test" })
      .getAttribute("value");
    await identityChooser
      .locator(`[data-person="${alexA}"]`)
      .getByRole("button", { name: "Photos", exact: true })
      .click();
    await expect(identityChooser.getByRole("img")).toHaveCount(1);
    await expect
      .poll(() =>
        identityChooser.getByRole("img").evaluate((image) => {
          const img = image as HTMLImageElement;
          return img.naturalHeight > img.naturalWidth && img.naturalHeight === 512;
        }),
      )
      .toBe(true);
    await captureDialogThemes(
      page,
      identityChooser,
      output!,
      "after-photo-people-identities",
      760,
      async () =>
        identityChooser
          .getByRole("img")
          .evaluate((image) => (image as HTMLImageElement).naturalWidth > 0),
    );
    await identityChooser.getByRole("button", { name: "Cancel", exact: true }).click();
    await loupe.getByRole("button", { name: "Close photo viewer" }).click();
    await page.setViewportSize({ width: 1440, height: 720 });
    await page.getByRole("searchbox", { name: "Search photos" }).fill("photo");
    await page.getByRole("searchbox", { name: "Search photos" }).press("Enter");
    await page.getByRole("button", { name: "Cull", exact: true }).click();
    const cull = page.getByRole("dialog", { name: "Cull photos" });
    const closeups = cull.getByRole("region", { name: "Face closeups" });
    await expect(closeups).toContainText("Ada");
    const refreshed = page.waitForResponse(
      (response) =>
        response.url().endsWith("/photos/assets/query") &&
        !response.request().postDataJSON().query.filters?.asset_ids?.length,
    );
    const reloadedFaces = page.waitForResponse(async (response) => {
      if (!response.url().endsWith("/faces")) return false;
      const refreshedPage: api.PhotoBrowsePage = await (await refreshed).json();
      const selectedName = await cull.locator(".photo-viewport img").getAttribute("alt");
      const selected = refreshedPage.items.find((photo) => photo.name === selectedName);
      return response.url().endsWith(`/versions/${selected?.content_version_id}/faces`);
    });
    await cull.locator(".photo-cull").press("5");
    await refreshed;
    await reloadedFaces;
    await expect(closeups).toContainText("Ada");
    await captureDialogThemes(page, cull, output!, "after-photo-faces-cull", 620, async () =>
      closeups.locator("img").evaluate((image) => (image as HTMLImageElement).naturalWidth > 0),
    );
    await cull.getByRole("button", { name: "Exit cull", exact: true }).click();
    await page.getByRole("searchbox", { name: "Search photos" }).fill("");
    await page.getByRole("searchbox", { name: "Search photos" }).press("Enter");
    const portrait = page.getByRole("button", {
      name: "Select Synthetic-photo-00003.jpg",
      exact: true,
    });
    await portrait.dblclick();
    await loupe.getByRole("button", { name: "Metadata", exact: true }).click();
    await people.getByRole("button", { name: "Find or create a person: Ada" }).click();
    await people.getByRole("combobox").fill("Grace");
    await people.getByRole("option", { name: 'Create person "Grace"', exact: true }).click();
    await expect(loupe.locator(".face-box")).toHaveText("Face 1 · Grace");
    await loupe.getByRole("button", { name: "Close photo viewer", exact: true }).click();
    await page.getByRole("button", { name: "Hidden", exact: true }).click();
    await page.getByLabel("Passcode", { exact: true }).fill("synthetic-passcode");
    await page.getByRole("button", { name: "Set passcode", exact: true }).click();
    await expect(page.getByText(/Locks in/)).toBeVisible();
    await page.getByRole("button", { name: "Library", exact: true }).click();
    await portrait.click({ button: "right" });
    await page.getByRole("menuitem", { name: "Hide", exact: true }).click();
    await expect(portrait).toHaveCount(0);
    await page.getByRole("button", { name: "Hidden", exact: true }).click();
    await portrait.dblclick();
    await loupe.getByRole("button", { name: "Metadata", exact: true }).click();
    await people.getByRole("button", { name: "Grace", exact: true }).click();
    await expect(page).toHaveURL(/\/photos\/hidden(?:[?#]|$)/);
    await expect(page.getByText("Person: Grace", { exact: true })).toBeVisible();
    await expect(portrait).toBeVisible();
    await captureThemes(page, output!, "after-photo-people-hidden-filter");
    if (await loupe.isVisible()) {
      await loupe.getByRole("button", { name: "Close photo viewer", exact: true }).click();
    }
    await page.getByRole("button", { name: "Library", exact: true }).click();
    await expect(portrait).toHaveCount(0);
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
    const more = people.getByRole("option", { name: "Load more", exact: true });
    await more.scrollIntoViewIfNeeded();
    await expect(more).toBeVisible();
    await expect(
      people.getByRole("option", { name: "Aaron 099 Smith", exact: true }),
    ).toBeVisible();
    await expect(
      people.getByRole("option", { name: 'Create person "Smith"', exact: true }),
    ).toHaveCount(0);
    const bounds = await loupe.boundingBox();
    if (!bounds) throw new Error("Photo viewer is not measurable");
    await captureThemes(page, output!, "after-photo-people-before-load-more", {
      x: bounds.x,
      y: bounds.y + (bounds.height - bounds.width / 2) / 2,
      width: bounds.width,
      height: bounds.width / 2,
    });
    await more.click();
    const existing = people.getByRole("option", { name: "Smith", exact: true });
    await existing.scrollIntoViewIfNeeded();
    await expect(existing).toBeVisible();
    await captureThemes(page, output!, "after-photo-people-later-match");
    await existing.click();
    await expect(loupe.locator(".face-box")).toHaveText("Face 1 · Smith");
    await people.getByRole("button", { name: "Find or create a person: Smith" }).click();
    await people.getByRole("combobox").fill("Robin");
    await people.getByRole("option", { name: "Robin · 2 people", exact: true }).click();
    const chooser = page.getByRole("dialog", { name: "Choose Robin" });
    await expect(chooser.getByRole("img")).toHaveCount(0);
    await chooser.getByRole("button", { name: "Photos", exact: true }).first().click();
    await expect(chooser.getByRole("img")).toHaveCount(1);
    await expect(chooser.getByRole("img").first()).toHaveAttribute("src", /^blob:/);
    await captureDialogThemes(
      page,
      chooser,
      output!,
      "after-photo-people-rotation",
      760,
      async () =>
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

test("continues an incomplete group of 300 namesakes", async ({ page }) => {
  test.setTimeout(300_000);
  await mkdir(path.join(repository, ".superpowers"), { recursive: true });
  const fixture = photoFixture(
    await mkdtemp(path.join(repository, ".superpowers", "faces-namesakes-proof-")),
  );
  try {
    await fixture.seed(1, "--many-namesakes", "--face-search");
    await mkdir(output!, { recursive: true });
    const url = new URL(await fixture.run("web", "--no-browser"));
    url.pathname = "/photos";
    await page.goto(url.href);
    await page.locator(".photo-image").first().dblclick();
    const loupe = page.getByRole("dialog", { name: /^Photo viewer,/ });
    await loupe.getByRole("button", { name: "Add face", exact: true }).click();
    await loupe.getByRole("button", { name: /^Position face on/ }).focus();
    const created = page.waitForResponse(
      (response) => response.request().method() === "POST" && response.url().endsWith("/faces"),
    );
    await page.keyboard.press("Enter");
    const face = (await (await created).json()).face;
    await loupe.getByRole("button", { name: "Metadata", exact: true }).click();
    const people = loupe.getByRole("region", { name: "People", exact: true });
    await people.getByRole("button", { name: "Find or create a person: Face 1" }).click();
    await people.getByRole("combobox").fill("Alex");
    await captureDialogThemes(
      page,
      loupe,
      output!,
      "after-photo-people-incomplete-group",
      960,
      async () => people.getByRole("option", { name: "Load more", exact: true }).isVisible(),
    );
    let laterPerson = "";
    for (let continuation = 0; continuation < 2; continuation++) {
      const response = page.waitForResponse((row) => {
        const request = new URL(row.url());
        return (
          request.pathname === "/api/v1/people" &&
          request.searchParams.get("query") === "Alex" &&
          request.searchParams.has("cursor")
        );
      });
      await people.getByRole("option", { name: "Load more", exact: true }).click();
      const result = (await (await response).json()) as { items: { person_id: string }[] };
      laterPerson = result.items.at(-1)!.person_id;
    }
    await fixture.run(
      "photos",
      "faces",
      "create",
      face.content_version_id,
      "--box",
      "0.15,0.3,0.2,0.2",
      "--person",
      laterPerson,
    );
    await people.getByRole("option", { name: "Alex · 300 people", exact: true }).click();
    const chooser = page.getByRole("dialog", { name: "Choose Alex", exact: true });
    const later = chooser.locator(`[data-person="${laterPerson}"]`);
    await later.getByRole("button", { name: "Photos", exact: true }).click();
    await chooser.locator(`input[value="${laterPerson}"]`).check();
    await later.locator(".person-preview").scrollIntoViewIfNeeded();
    await captureDialogThemes(
      page,
      chooser,
      output!,
      "after-photo-people-expanded-candidate",
      760,
      async () =>
        later.getByRole("img").evaluate((image) => (image as HTMLImageElement).naturalWidth > 0),
    );
    const assigned = page.waitForResponse(
      (row) => row.request().method() === "POST" && row.url().endsWith("/face-decisions"),
    );
    await chooser.getByRole("button", { name: "Name face", exact: true }).click();
    await assigned;
  } finally {
    await fixture.dispose();
  }
});
