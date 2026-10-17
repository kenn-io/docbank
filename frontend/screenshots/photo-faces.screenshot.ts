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
    path: path.join(directory, name),
    clip: {
      x: bounds.x,
      y: bounds.y,
      width: bounds.width,
      height: Math.min(cropHeight, viewport.height - bounds.y),
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
    await fixture.seed(3);
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
    await page.keyboard.press("Escape");
    await expect(loupe).toBeVisible();
    await expect(loupe.getByRole("button", { name: /^Position face on/ })).toHaveCount(0);
    await add.click();
    const bounds = (await viewport.boundingBox())!;
    await page.mouse.move(bounds.x + bounds.width / 2, bounds.y + bounds.height / 2);
    await page.mouse.down();
    await page.mouse.move(bounds.x + bounds.width / 2 + 1, bounds.y + bounds.height / 2 + 1);
    await page.mouse.up();
    await expect(loupe.locator(".face-box")).toHaveCount(0);
    const created = page.waitForResponse(
      (response) => response.request().method() === "POST" && response.url().endsWith("/faces"),
    );
    await loupe.getByRole("button", { name: /^Position face on/ }).focus();
    const keyboardRegion = loupe.locator(".keyboard-region");
    const initialMarker = (await keyboardRegion.boundingBox())!;
    await page.keyboard.press("ArrowLeft");
    await expect
      .poll(async () => (await keyboardRegion.boundingBox())?.x)
      .toBeLessThan(initialMarker.x);
    const marker = (await keyboardRegion.boundingBox())!;
    await page.keyboard.press("Enter");
    const face = (await (await created).json()).face;
    await expect(loupe.locator(".face-box")).toHaveCount(1);
    const box = (await loupe.locator(".face-box").boundingBox())!;
    expect(box.x).toBeCloseTo(marker.x, 0);
    expect(box.width).toBeCloseTo(marker.width, 0);
    await loupe.getByRole("button", { name: "Metadata", exact: true }).click();
    const people = loupe.getByRole("region", { name: "People", exact: true });
    await people.getByRole("button", { name: "Find or create a person: Unnamed face" }).click();
    await people.getByRole("combobox").fill("Ada");
    await people.getByRole("option", { name: 'Create person "Ada"' }).click();
    await expect(loupe.locator(".face-box")).toHaveText("Ada");
    const personID = await withPhotoAPI(
      url,
      async () => (await api.listPeople({ query: "Ada" }, { session })).items[0]!.person_id,
    );
    await fixture.run("photos", "faces", "create", face.content_version_id, "--person", personID);
    await people.getByRole("button", { name: "Find or create a person: Ada" }).click();
    await people.getByRole("option", { name: "Clear name", exact: true }).click();
    await expect(loupe.locator(".face-box")).toHaveText("Unnamed face");
    await people.getByRole("button", { name: "Find or create a person: Unnamed face" }).click();
    await people.getByRole("combobox").fill("Ada");
    await people.getByRole("option", { name: "Ada", exact: true }).click();
    await people.getByRole("button", { name: "Clear whole-photo mark" }).click();
    await expect(people.getByRole("button", { name: "Clear whole-photo mark" })).toHaveCount(0);
    await expect(loupe.locator(".face-box")).toHaveText("Ada");
    // Zoomed drags retain their literal screen dimensions, even for narrow boxes.
    await viewport.evaluate((element) =>
      element.dispatchEvent(
        Object.assign(new Event("wheel", { bubbles: true, cancelable: true }), {
          deltaY: -5000,
          clientX: element.getBoundingClientRect().x + element.clientWidth / 2,
          clientY: element.getBoundingClientRect().y + element.clientHeight / 2,
        }),
      ),
    );
    await expect(loupe.locator(".face-box span")).toHaveCSS(
      "transform",
      "matrix(0.125, 0, 0, 0.125, 0, 0)",
    );
    await add.click();
    const drawn = page.waitForResponse(
      (response) => response.request().method() === "POST" && response.url().endsWith("/faces"),
    );
    const zoomBounds = (await viewport.boundingBox())!;
    const x = zoomBounds.x + zoomBounds.width / 2,
      y = zoomBounds.y + zoomBounds.height / 2;
    await page.mouse.move(x, y);
    await page.mouse.down();
    await page.mouse.move(x + 20, y + 12);
    await page.mouse.up();
    const drawnFace = (await (await drawn).json()).face;
    await expect(loupe.locator(".face-box")).toHaveCount(2);
    const layer = (await loupe.locator(".face-layer").boundingBox())!;
    expect(drawnFace.box.width * layer.width).toBeCloseTo(20, 0);
    expect(drawnFace.box.height * layer.height).toBeCloseTo(12, 0);
    await people
      .locator(".person-row")
      .filter({ hasText: "Unnamed face" })
      .getByRole("button", { name: "Remove face" })
      .click();
    await expect(loupe.locator(".face-box")).toHaveCount(1);
    await withPhotoAPI(url, async () => {
      const rows = await api.listPhotoFaces(face.content_version_id, { session });
      expect(rows.faces.find((row) => row.face_id === drawnFace.face_id)?.dismissed).toBe(true);
    });
    await loupe.getByRole("button", { name: "Reset zoom", exact: true }).click();
    await captureDarkDialog(
      page,
      loupe,
      output!,
      "after-photo-faces-loupe-dark.png",
      760,
      async () =>
        (await viewport
          .locator("img")
          .evaluate((image) => (image as HTMLImageElement).naturalWidth)) > 0,
    );
    const filtered = page.waitForRequest(
      (request) =>
        request.url().endsWith("/photos/assets/query") &&
        request.postDataJSON()?.query?.filters?.person_ids?.includes(personID),
    );
    await people.getByRole("button", { name: "Ada", exact: true }).click();
    await filtered;
    await expect(loupe).toHaveCount(0);
    await expect(page.locator(".photo-cell")).toHaveCount(1);
    await page.setViewportSize({ width: 1440, height: 720 });
    await page.getByRole("button", { name: "Cull", exact: true }).click();
    const cull = page.getByRole("dialog", { name: "Cull photos" });
    const closeups = cull.getByRole("region", { name: "Face closeups" });
    await expect(closeups.getByRole("img")).toHaveCount(1);
    await expect(closeups.locator("figcaption")).toHaveText(["Ada"]);
    await captureDarkDialog(page, cull, output!, "after-photo-faces-cull-dark.png", 620, async () =>
      closeups.getByRole("img").evaluate((image) => (image as HTMLImageElement).naturalWidth > 0),
    );
    await cull.getByRole("button", { name: "Exit cull", exact: true }).click();
    await fixture.run("people", "retire", personID, "--revision", "1");
    await page.locator(".photo-image").first().dblclick();
    if (!(await people.isVisible()))
      await loupe.getByRole("button", { name: "Metadata", exact: true }).click();
    await expect(loupe.locator(".face-box")).toHaveText("Ada");
    await expect(people.getByRole("button", { name: "Ada", exact: true })).toHaveCount(0);
    await people.getByRole("button", { name: "Remove face" }).click();
    await expect(loupe.locator(".face-box")).toHaveCount(0);
  } finally {
    await fixture.dispose();
  }
});
