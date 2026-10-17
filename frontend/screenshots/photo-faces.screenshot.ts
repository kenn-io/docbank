import { expect, test, type Page } from "@playwright/test";
import { mkdir, mkdtemp } from "node:fs/promises";
import path from "node:path";
import * as api from "../src/generated/docbank.js";
import { photoFixture, repository } from "./photo-fixture.js";

const output = process.env.DOCBANK_FACES_SCREENSHOT_DIR;
test.use({ actionTimeout: 15_000 });
test.skip(!output, "DOCBANK_FACES_SCREENSHOT_DIR enables synthetic face review proof");

async function withPhotoAPI<T>(url: URL, run: () => Promise<T>): Promise<T> {
  const fetcher = globalThis.fetch;
  try {
    globalThis.fetch = (input, init) => {
      const address = new URL(String(input), url.origin);
      const headers = new Headers(init?.headers);
      headers.set("Host", address.host);
      address.hostname = "127.0.0.1";
      return fetcher(address, { ...init, headers });
    };
    return await run();
  } finally {
    globalThis.fetch = fetcher;
  }
}

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
    await page
      .locator(".photo-cell")
      .nth(0)
      .getByRole("button", { name: /^Select / })
      .click();
    await page
      .locator(".photo-cell")
      .nth(1)
      .getByRole("button", { name: /^Select / })
      .click({ modifiers: ["Control"] });
    await page.getByRole("button", { name: "Compare", exact: true }).click();
    const compare = page.getByRole("dialog", { name: "Compare photos" });
    await expect(compare.locator(".photo-viewport img").first()).toBeVisible();
    await compare.getByRole("button", { name: "Manual closeup", exact: true }).click();
    await compare.locator(".photo-viewport").first().click();
    await expect(compare.getByLabel("Matching closeups")).toBeVisible();
    await compare.getByRole("button", { name: "Exit compare", exact: true }).click();
    await page.locator(".photo-image").first().dblclick();
    const loupe = page.getByRole("dialog", { name: /^Photo viewer,/ });
    await expect(loupe.locator(".photo-viewport img")).toBeVisible();
    await expect
      .poll(() =>
        loupe
          .locator(".photo-viewport img")
          .evaluate((image) => (image as HTMLImageElement).naturalWidth > 512),
      )
      .toBe(true);
    const viewport = loupe.locator(".photo-viewport");
    await viewport.dblclick();
    const bounds = (await viewport.boundingBox())!;
    await page.mouse.move(bounds.x + bounds.width / 2, bounds.y + bounds.height / 2);
    await page.mouse.down();
    await page.mouse.move(bounds.x + bounds.width / 2 + 80, bounds.y + bounds.height / 2 + 30, {
      steps: 5,
    });
    await page.mouse.up();
    let facePosts = 0;
    page.on("request", (request) => {
      if (request.method() === "POST" && request.url().endsWith("/faces")) facePosts++;
    });
    await loupe.getByRole("button", { name: "Add face", exact: true }).click();
    await page.mouse.click(bounds.x + bounds.width / 2, bounds.y + bounds.height / 2);
    await page.mouse.move(bounds.x + bounds.width / 2, bounds.y + bounds.height / 2);
    await page.mouse.down();
    await page.mouse.move(bounds.x + bounds.width / 2 + 1, bounds.y + bounds.height / 2 + 1);
    await page.mouse.up();
    await expect(loupe.locator(".face-box")).toHaveCount(0);
    expect(facePosts).toBe(0);
    const faceCreated = page.waitForResponse(
      (response) => response.request().method() === "POST" && response.url().endsWith("/faces"),
    );
    await loupe.getByRole("button", { name: /^Position face on/ }).focus();
    await page.keyboard.press("ArrowLeft");
    const marker = loupe.locator(".keyboard-region");
    await expect(marker).toBeVisible();
    const target = (await marker.boundingBox())!;
    await page.screenshot({
      path: path.join(output!, "web-photo-faces-keyboard.png"),
      animations: "disabled",
    });
    await page.keyboard.press("Enter");
    const face = (await (await faceCreated).json()).face;
    await expect(loupe.locator(".face-box")).toHaveCount(1);
    const placed = (await loupe.locator(".face-box").boundingBox())!;
    expect(placed.x).toBeCloseTo(target.x, 0);
    expect(placed.y).toBeCloseTo(target.y, 0);
    expect(placed.width).toBeCloseTo(target.width, 0);
    expect(placed.height).toBeCloseTo(target.height, 0);
    await loupe.getByRole("button", { name: "Reset zoom", exact: true }).click();
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
    const renamedPersonID = await withPhotoAPI(
      url,
      async () => (await api.listPeople({ query: "Ada" }, { session })).items[0]!.person_id,
    );
    await fixture.run("people", "rename", renamedPersonID, "Ada Updated", "--revision", "1");
    await page.locator(".photo-image").first().dblclick();
    await expect(loupe.locator(".face-box")).toHaveText("Ada Updated");
    await loupe.getByRole("button", { name: "Close photo viewer", exact: true }).click();
    await expect(page.getByText("Person: Ada Updated", { exact: true })).toBeVisible();
    await fixture.run("people", "rename", renamedPersonID, "Ada", "--revision", "2");
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
    const personID = await withPhotoAPI(
      url,
      async () =>
        (await api.listPeople({ query: "Ada", limit: 100 }, { session })).items[0]!.person_id,
    );
    await fixture.run("photos", "faces", "create", face.content_version_id, "--person", personID);
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
    await cull.getByRole("button", { name: "Exit cull", exact: true }).click();
    await page.getByRole("button", { name: "Hidden", exact: true }).click();
    await page.getByLabel("Passcode", { exact: true }).fill("synthetic-passcode");
    await page.getByRole("button", { name: "Set passcode", exact: true }).click();
    await page.getByRole("button", { name: "Library", exact: true }).click();
    await page
      .locator(".photo-cell")
      .first()
      .getByRole("button", { name: /^Select / })
      .click({ button: "right" });
    await page.getByRole("menuitem", { name: "Hide", exact: true }).click();
    await expect(page.locator(".photo-cell")).toHaveCount(0);
    url.pathname = "/photos/hidden";
    await page.goto("about:blank");
    await page.goto(url.href);
    await page.getByLabel("Passcode", { exact: true }).fill("synthetic-passcode");
    await page.getByRole("button", { name: "Unlock", exact: true }).click();
    await page.locator(".photo-image").first().dblclick();
    await loupe.getByRole("button", { name: "Metadata", exact: true }).click();
    const library = page.waitForRequest(
      (request) =>
        request.url().endsWith("/photos/assets/query") &&
        request.postDataJSON()?.query?.filters?.person_ids?.includes(personID),
    );
    await loupe.getByRole("button", { name: "Ada", exact: true }).first().click();
    expect((await library).postDataJSON()).toMatchObject({
      hidden: false,
      query: { text: "", sort: { field: "capture_time", direction: "desc" } },
    });
    await expect(page).toHaveURL(/\/photos$/);
    await expect(page.getByText("Person: Ada", { exact: true })).toBeVisible();
    await expect(page.locator(".photo-cell")).toHaveCount(0);
    await page.screenshot({
      path: path.join(output!, "web-photo-faces-hidden-person.png"),
      animations: "disabled",
    });
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

test("pages to every person match after leaving a relevance search", async ({ page }) => {
  test.setTimeout(300_000);
  await mkdir(path.join(repository, ".superpowers"), { recursive: true });
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "faces-pagination-proof-"));
  const fixture = photoFixture(workspace);
  try {
    await fixture.seed(251);
    const url = new URL(await fixture.run("web", "--no-browser"));
    const session = new URLSearchParams(url.hash.slice(1)).get("web_session")!;
    url.pathname = "/photos";
    await page.goto(url.href);
    const { personID, lastName } = await withPhotoAPI(url, async () => {
      const person = await api.createPerson({ display_name: "Ada" }, { session });
      let cursor: string | undefined;
      const rows: api.PhotoBrowseRow[] = [];
      do {
        const result = await api.listPhotoAssets(
          {
            query: {
              v: 1,
              text: "",
              syntax: "simple",
              mode: "lexical",
              filters: {},
              sort: { field: "capture_time", direction: "desc" },
            },
            page_size: 250,
            cursor,
          },
          { session },
        );
        rows.push(...result.items);
        cursor = result.next_cursor;
      } while (cursor);
      if (rows.length !== 251) throw new Error(`Expected 251 rows, got ${rows.length}`);
      for (const row of rows) {
        await api.createPhotoFace(
          row.content_version_id,
          {
            person_id: person.person_id,
            face_id: crypto.randomUUID(),
            box: { x: 0.1, y: 0.2, width: 0.3, height: 0.4 },
          },
          { session },
        );
      }
      return { personID: person.person_id, lastName: rows.at(-1)!.name };
    });
    await page.getByRole("searchbox", { name: "Search photos" }).fill("Synthetic");
    await page.getByRole("button", { name: "Search", exact: true }).click();
    await expect(page.locator(".photo-results-summary")).toContainText("251 photos");
    await page.locator(".photo-image").first().dblclick();
    const loupe = page.getByRole("dialog", { name: /^Photo viewer,/ });
    await loupe.getByRole("button", { name: "Metadata", exact: true }).click();
    const query = page.waitForRequest(
      (request) =>
        request.url().endsWith("/photos/assets/query") &&
        request.postDataJSON()?.query?.filters?.person_ids?.includes(personID),
    );
    const personPage = page.waitForResponse(
      (response) =>
        response.url().endsWith("/photos/assets/query") &&
        response.request().postDataJSON()?.query?.filters?.person_ids?.includes(personID) &&
        response.request().postDataJSON()?.page_size === 250,
    );
    await loupe.getByRole("button", { name: "Ada", exact: true }).click();
    const firstPage = await (await personPage).json();
    expect(firstPage.total).toBe(251);
    expect(firstPage.items).toHaveLength(250);
    expect(firstPage.next_cursor).toBeTruthy();
    expect((await query).postDataJSON().query.sort.field).toBe("capture_time");
    await expect(page.locator(".photo-results-summary")).toContainText("251 photos");
    await expect(page.getByRole("searchbox", { name: "Search photos" })).toHaveValue("");
    await expect
      .poll(async () => {
        await page.getByTestId("photo-scroll").evaluate((element) => {
          element.scrollTop = element.scrollHeight;
        });
        return page.getByRole("button", { name: `Select ${lastName}`, exact: true }).count();
      })
      .toBe(1);
  } finally {
    await fixture.dispose();
  }
});

test("updates a mounted face picker's cached name after a CLI rename and another face decision", async ({
  page,
}) => {
  test.setTimeout(300_000);
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "faces-rename-proof-"));
  const fixture = photoFixture(workspace);
  try {
    await fixture.seed(1);
    const url = new URL(await fixture.run("web", "--no-browser"));
    const session = new URLSearchParams(url.hash.slice(1)).get("web_session")!;
    url.pathname = "/photos";
    await page.goto(url.href);
    const ada = await withPhotoAPI(url, async () => {
      const rows = await api.listPhotoAssets(
        {
          query: {
            v: 1,
            syntax: "simple",
            text: "",
            mode: "lexical",
            filters: {},
            sort: { field: "capture_time", direction: "desc" },
          },
          page_size: 250,
        },
        { session },
      );
      const ada = await api.createPerson({ display_name: "Ada" }, { session });
      await api.createPerson({ display_name: "Grace" }, { session });
      for (const x of [0.1, 0.6]) {
        await api.createPhotoFace(
          rows.items[0]!.content_version_id,
          {
            face_id: crypto.randomUUID(),
            box: { x, y: 0.2, width: 0.2, height: 0.3 },
          },
          { session },
        );
      }
      return ada;
    });
    await page.locator(".photo-image").first().dblclick();
    const loupe = page.getByRole("dialog", { name: /^Photo viewer,/ });
    await loupe.getByRole("button", { name: "Metadata", exact: true }).click();
    const people = loupe.getByRole("region", { name: "People", exact: true });
    await people
      .getByRole("button", { name: "Find or create a person: Unnamed face" })
      .first()
      .click();
    await people.getByRole("combobox").fill("Ada");
    await people.getByRole("option", { name: "Ada", exact: true }).click();
    await expect(
      people.getByRole("button", { name: "Find or create a person: Ada" }),
    ).toBeVisible();
    await fixture.run(
      "people",
      "rename",
      ada.person_id,
      "Ada Updated",
      "--revision",
      String(ada.revision),
    );
    await people.getByRole("button", { name: "Find or create a person: Unnamed face" }).click();
    await people.getByRole("combobox").fill("Grace");
    await people.getByRole("option", { name: "Grace", exact: true }).click();
    await expect(
      people.getByRole("button", { name: "Find or create a person: Ada Updated" }),
    ).toBeVisible();
    await expect(people.getByRole("button", { name: "Ada Updated", exact: true })).toBeVisible();
    await expect(
      people.getByRole("button", { name: "Find or create a person: Grace" }),
    ).toBeVisible();
    await expect(loupe.locator(".face-box")).toHaveText(["Ada Updated", "Grace"]);
    await expect
      .poll(() =>
        loupe
          .locator(".photo-viewport img")
          .evaluate((image) => (image as HTMLImageElement).naturalWidth > 512),
      )
      .toBe(true);
    await page.evaluate(() => document.fonts.ready.then(() => {}));
    await mkdir(output!, { recursive: true });
    await page.screenshot({
      path: path.join(output!, "web-photo-faces-canonical-rename.png"),
      animations: "disabled",
    });
  } finally {
    await fixture.dispose();
  }
});
