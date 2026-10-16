import { expect, test, type Page } from "@playwright/test";
import { mkdir, mkdtemp } from "node:fs/promises";
import path from "node:path";
import { photoFixture, repository } from "./photo-fixture.js";

const output = process.env.DOCBANK_FACES_SCREENSHOT_DIR;
test.use({ actionTimeout: 15_000 });
test.skip(!output, "DOCBANK_FACES_SCREENSHOT_DIR enables synthetic face review proof");

async function captureThemes(page: Page, name: string) {
  await expect
    .poll(() =>
      page
        .locator(".photo-viewport img")
        .evaluateAll(
          (images) =>
            images.length > 0 &&
            images.every((image) => (image as HTMLImageElement).naturalWidth > 512),
        ),
    )
    .toBe(true);
  for (const theme of ["light", "dark"]) {
    await page.evaluate((value) => {
      localStorage.setItem("docbank-theme", value);
      document.documentElement.classList.toggle("dark", value === "dark");
      return document.fonts.ready.then(() => {});
    }, theme);
    await page.screenshot({
      path: path.join(output!, `${name}${theme === "light" ? "-light" : ""}.png`),
      animations: "disabled",
    });
  }
}

test("draws and names faces, opens person-filtered Library, and reads cull closeups", async ({
  page,
}) => {
  test.setTimeout(300_000);
  await mkdir(path.join(repository, ".superpowers"), { recursive: true });
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "faces-proof-"));
  const fixture = photoFixture(workspace);
  try {
    await fixture.seed(3);
    await mkdir(output!, { recursive: true });
    const url = new URL(await fixture.run("web", "--no-browser"));
    const session = new URLSearchParams(url.hash.slice(1)).get("web_session")!;
    url.pathname = "/photos";
    await page.goto(url.href);
    await page.locator(".photo-image").first().dblclick();
    const loupe = page.getByRole("dialog", { name: /^Photo viewer,/ });
    await expect(loupe.locator(".photo-viewport img")).toBeVisible();
    await loupe.getByRole("button", { name: "Add face", exact: true }).click();
    const faceCreated = page.waitForResponse(
      (response) => response.request().method() === "POST" && response.url().endsWith("/faces"),
    );
    await loupe.getByRole("button", { name: /^Position face on/ }).focus();
    await page.keyboard.press("ArrowLeft");
    await page.keyboard.press("Enter");
    const face = (await (await faceCreated).json()).face;
    await expect(loupe.locator(".face-box")).toHaveCount(1);
    await loupe.getByRole("button", { name: "Metadata", exact: true }).click();
    const people = loupe.getByRole("region", { name: "People", exact: true });
    await people.getByRole("button", { name: "Find or create a person: Unnamed face" }).click();
    await people.getByRole("combobox").fill("Ada");
    await people.getByRole("option", { name: 'Create person "Ada"' }).click();
    await expect(loupe.locator(".face-box")).toHaveText("Ada");
    await people.getByRole("button", { name: "Ada", exact: true }).click();
    await expect(loupe).toHaveCount(0);
    await expect(page.locator(".photo-cell")).toHaveCount(1);
    await expect(page.locator(".photo-results-summary")).toContainText("1 photos");
    await expect(page.getByText("Person: Ada", { exact: true })).toBeVisible();
    await expect
      .poll(() =>
        page
          .locator(".photo-cell img")
          .first()
          .evaluate((image) => (image as HTMLImageElement).naturalWidth > 0),
      )
      .toBe(true);
    await page.evaluate(() => document.fonts.ready.then(() => {}));
    await page.screenshot({
      path: path.join(output!, "web-photo-faces-library.png"),
      animations: "disabled",
    });
    await page.locator(".photo-image").first().dblclick();
    await expect(loupe.locator(".face-box")).toHaveText("Ada");
    if (!(await people.isVisible()))
      await loupe.getByRole("button", { name: "Metadata", exact: true }).click();
    await people.locator(".kit-typeahead__trigger").click();
    await people.getByRole("combobox").fill("Grace");
    await people.getByRole("option", { name: 'Create person "Grace"' }).click();
    await expect(loupe.locator(".face-box")).toHaveText("Grace");
    await loupe.getByRole("button", { name: "Close photo viewer", exact: true }).click();
    await expect(page.locator(".photo-cell")).toHaveCount(0);
    await expect(page.locator(".photo-results-summary")).toContainText("0 photos");
    await page.getByRole("button", { name: "Clear filters", exact: true }).click();
    await expect(page.locator(".photo-cell")).toHaveCount(3);
    await page.locator(".photo-image").first().dblclick();
    await expect(loupe.locator(".face-box")).toHaveText("Grace");
    if (!(await people.isVisible()))
      await loupe.getByRole("button", { name: "Metadata", exact: true }).click();
    await people.locator(".kit-typeahead__trigger").click();
    await people.getByRole("option", { name: "Clear name", exact: true }).click();
    await expect(loupe.locator(".face-box")).toHaveText("Unnamed face");
    await people.locator(".kit-typeahead__trigger").click();
    await people.getByRole("combobox").fill("Ada");
    await people.getByRole("option", { name: "Ada", exact: true }).click();
    await expect(loupe.locator(".face-box")).toHaveText("Ada");
    await loupe.getByRole("combobox", { name: /^Rotation:/ }).click();
    await page.getByRole("option", { name: "90°", exact: true }).click();
    await loupe.getByRole("button", { name: "Apply", exact: true }).click();
    await expect(loupe.locator(".photo-viewport img")).toHaveAttribute("style", /rotate\(90deg\)/);
    await loupe.getByRole("button", { name: "Add face", exact: true }).click();
    const layer = await loupe.locator(".face-layer").boundingBox();
    expect(layer).not.toBeNull();
    const drawn = page.waitForResponse(
      (response) => response.request().method() === "POST" && response.url().endsWith("/faces"),
    );
    await page.mouse.move(layer!.x + layer!.width * 0.1, layer!.y + layer!.height * 0.2);
    await page.mouse.down();
    await page.mouse.move(layer!.x + layer!.width * 0.2, layer!.y + layer!.height * 0.3);
    await page.mouse.up();
    const drawnFace = (await (await drawn).json()).face;
    expect(drawnFace.box.x).toBeCloseTo(0.2, 2);
    expect(drawnFace.box.y).toBeCloseTo(0.8, 2);
    expect(drawnFace.box.width).toBeCloseTo(0.1, 2);
    expect(drawnFace.box.height).toBeCloseTo(0.1, 2);
    await expect(loupe.locator(".face-box")).toHaveCount(2);
    await page.goto("about:blank");
    await page.goto(url.href);
    await expect(page.locator(".photo-cell")).toHaveCount(3);
    await page.locator(".photo-image").first().dblclick();
    await expect(loupe.locator(".face-box")).toHaveCount(2);
    if (!(await people.isVisible()))
      await loupe.getByRole("button", { name: "Metadata", exact: true }).click();
    const personID = await page.evaluate(
      async ({ session }) => {
        const response = await fetch("/api/v1/people?query=Ada&limit=100", {
          headers: { "X-Docbank-Web-Session": session },
        });
        if (!response.ok) throw new Error(await response.text());
        return (await response.json()).items[0].person_id as string;
      },
      { session },
    );
    const marked = await page.evaluate(
      async ({ version, personID, session }) => {
        const response = await fetch(`/api/v1/photos/versions/${version}/faces`, {
          method: "POST",
          headers: { "Content-Type": "application/json", "X-Docbank-Web-Session": session },
          body: JSON.stringify({ person_id: personID }),
        });
        return response.status;
      },
      { version: face.content_version_id, personID, session },
    );
    expect(marked).toBe(201);
    await captureThemes(page, "web-photo-faces-loupe");
    const filtered = page.waitForRequest(
      (request) =>
        request.url().endsWith("/photos/assets/query") &&
        request.postDataJSON()?.query?.filters?.person_ids?.includes(personID),
    );
    await people.getByRole("button", { name: "Ada", exact: true }).click();
    await filtered;
    await expect(loupe).toHaveCount(0);
    await expect(page.locator(".photo-cell")).toHaveCount(1);
    await page.getByRole("button", { name: "Cull", exact: true }).click();
    const cull = page.getByRole("dialog", { name: "Cull photos" });
    const closeups = cull.getByRole("region", { name: "Face closeups" });
    await expect(closeups.getByRole("img")).toHaveCount(3);
    await expect
      .poll(() =>
        closeups
          .getByRole("img")
          .evaluateAll((images) =>
            images.every((image) => (image as HTMLImageElement).naturalWidth > 0),
          ),
      )
      .toBe(true);
    await expect(closeups.locator("figcaption")).toHaveText(["Ada", "Ada", "Unnamed face"]);
    await captureThemes(page, "web-photo-faces-cull");
  } finally {
    await fixture.dispose();
  }
});

test("keeps face tools and requests out of video loupe, inspector, and cull", async ({ page }) => {
  test.setTimeout(300_000);
  await mkdir(path.join(repository, ".superpowers"), { recursive: true });
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "faces-video-proof-"));
  const fixture = photoFixture(workspace);
  const faceRequests: string[] = [];
  page.on("request", (request) => {
    if (/\/versions\/[^/]+\/faces/.test(request.url())) faceRequests.push(request.url());
  });
  try {
    await fixture.seed(1, "--video");
    const url = new URL(await fixture.run("web", "--no-browser"));
    url.pathname = "/photos";
    await page.goto(url.href);
    const cell = page.getByRole("button", { name: "Select Synthetic-video.mp4" });
    await cell.click();
    await page.getByRole("button", { name: "Metadata", exact: true }).click();
    const metadata = page.getByRole("region", { name: "Photo metadata", exact: true });
    await expect(metadata).toBeVisible();
    await expect(metadata.getByRole("region", { name: "People", exact: true })).toHaveCount(0);
    await expect(metadata.getByRole("combobox", { name: /^Rotation:/ })).toBeEnabled();
    await page.getByRole("button", { name: "Open Synthetic-video.mp4", exact: true }).focus();
    await page.keyboard.press("Enter");
    const loupe = page.getByRole("dialog", { name: /^Photo viewer,/ });
    await expect(loupe).toBeVisible();
    await expect(loupe.getByRole("button", { name: "Add face", exact: true })).toHaveCount(0);
    await loupe.getByRole("button", { name: "Metadata", exact: true }).click();
    await expect(loupe.getByRole("region", { name: "Photo metadata", exact: true })).toBeVisible();
    await expect(loupe.getByRole("region", { name: "People", exact: true })).toHaveCount(0);
    await loupe.getByRole("button", { name: "Close photo viewer", exact: true }).click();
    await page.getByRole("button", { name: "Cull", exact: true }).click();
    const cull = page.getByRole("dialog", { name: "Cull photos", exact: true });
    await expect(cull.getByText("Synthetic-video.mp4", { exact: true })).toBeVisible();
    await expect(cull.getByRole("region", { name: "Face closeups", exact: true })).toHaveCount(0);
    expect(faceRequests).toEqual([]);
  } finally {
    await fixture.dispose();
  }
});
