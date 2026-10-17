import { expect, test } from "@playwright/test";
import { mkdir, mkdtemp } from "node:fs/promises";
import path from "node:path";
import { photoFixture, repository, capturePhotoThemes } from "./photo-fixture.js";

const output = process.env.DOCBANK_CULL_SCREENSHOT_DIR;
test.skip(!output, "DOCBANK_CULL_SCREENSHOT_DIR enables synthetic cull and compare proof");

test("culls, resumes saved membership and compares manual closeups", async ({ page }) => {
  test.setTimeout(300_000);
  await mkdir(path.join(repository, ".superpowers"), { recursive: true });
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "cull-proof-"));
  const fixture = photoFixture(workspace);
  try {
    await fixture.seed(12, "-pair");
    await mkdir(output!, { recursive: true });
    const url = new URL(await fixture.run("web", "--no-browser"));
    url.pathname = "/photos";
    await page.goto(url.href);
    await expect(page.locator(".photo-cell img").first()).toBeVisible();
    await page.locator(".photo-image").first().click();
    await page.getByRole("button", { name: "Metadata", exact: true }).click();
    const inspector = page.getByRole("region", { name: "Photo metadata" });
    await expect(inspector.getByRole("combobox", { name: /^Rating:/ })).toBeVisible();
    await page.getByRole("button", { name: "Cull", exact: true }).click();
    const cull = page.getByRole("dialog", { name: "Cull photos" });
    await expect(cull.locator(".photo-stage img")).toBeVisible();
    const firstName = await cull.locator("header strong").innerText();
    await expect(cull.getByRole("button", { name: "Scope: unflagged", exact: true })).toBeEnabled();
    await page.keyboard.press("s");
    const scopeConfirmation = page.getByRole("dialog", {
      name: "Change scope and reset this pass's progress?",
    });
    await expect(
      scopeConfirmation.getByRole("button", { name: "Reset pass", exact: true }),
    ).toBeVisible();
    await capturePhotoThemes(page, output!, "web-photo-cull-confirmation", () =>
      page
        .locator(".photo-stage img")
        .evaluateAll((images) =>
          images.every((image) => (image as HTMLImageElement).naturalWidth > 512),
        )
    );
    await scopeConfirmation.getByRole("button", { name: "Cancel", exact: true }).click();
    await expect(scopeConfirmation).toHaveCount(0);
    await expect(cull.getByRole("button", { name: "Scope: unflagged", exact: true })).toBeVisible();
    await expect(cull.locator("header strong")).toHaveText(firstName);
    await page.evaluate(() => {
      const writes: { store: string; bytes: number }[] = [];
      (window as unknown as Window & { cullWrites: typeof writes }).cullWrites = writes;
      const put = IDBObjectStore.prototype.put;
      IDBObjectStore.prototype.put = function (value: unknown, key?: IDBValidKey) {
        if (["members", "checkpoints", "markers"].includes(this.name))
          writes.push({ store: this.name, bytes: JSON.stringify(value).length });
        return put.call(this, value, key);
      };
    });
    await page.keyboard.press("Control+p");
    await expect(cull.locator("header strong")).toHaveText(firstName);
    await page.keyboard.press("p");
    await expect(cull.locator("header strong")).not.toHaveText(firstName);
    await expect(cull.getByRole("button", { name: "Undo", exact: true })).toBeVisible();
    let resumedName = await cull.locator("header strong").innerText();
    await expect(cull.locator(".filmstrip img").first()).toBeVisible();
    await capturePhotoThemes(page, output!, "web-photo-cull", () =>
      page
        .locator(".photo-stage img")
        .evaluateAll((images) =>
          images.every((image) => (image as HTMLImageElement).naturalWidth > 512),
        ),
    );
    // Commit a competing edit after preparation so the original write uses stale revisions.
    await page.route(
      "**/api/v1/photos/authored/edit",
      async (route) => {
        const competing = await route.fetch({
          postData: JSON.stringify({ ...route.request().postDataJSON(), patch: { rating: 3 } }),
        });
        expect(competing.ok()).toBe(true);
        await route.continue();
      },
      { times: 1 },
    );
    await page.keyboard.press("p");
    const reload = cull.getByRole("button", { name: "Reload photo", exact: true });
    await expect(reload).toBeVisible();
    await capturePhotoThemes(page, output!, "web-photo-cull-conflict", () =>
      page
        .locator(".photo-stage img")
        .evaluateAll((images) =>
          images.every((image) => (image as HTMLImageElement).naturalWidth > 512),
        )
    );
    await reload.click();
    await expect(reload).toHaveCount(0);
    let failLarge = true;
    await page.route("**/api/v1/photos/assets/*/preview", async (route) => {
      if (!failLarge || !route.request().postData()?.includes('"large"')) return route.fallback();
      failLarge = false;
      await route.fulfill({
        status: 503,
        contentType: "application/json",
        body: JSON.stringify({
          title: "Service Unavailable",
          status: 503,
          code: "photo_preview_unavailable",
          detail: "Preview preparation timed out. Retry.",
        }),
      });
    });
    await page.keyboard.press("z");
    const retry = cull.getByRole("button", { name: "Retry preview", exact: true });
    await expect(retry).toBeVisible();
    await expect(cull.locator(".photo-stage img")).toBeVisible();
    await capturePhotoThemes(page, output!, "web-photo-cull-retry", () =>
      page
        .locator(".photo-stage img")
        .evaluateAll((images) =>
          images.every((image) => (image as HTMLImageElement).naturalWidth > 512),
        ),
    );
    await retry.click();
    await expect(retry).toHaveCount(0);
    await expect(cull.locator(".photo-stage").getByRole("status")).toHaveCount(0);
    await page.unroute("**/api/v1/photos/assets/*/preview");
    await page.keyboard.press("z");
    await cull.getByRole("button", { name: "Undo", exact: true }).click();
    await expect(cull.getByRole("button", { name: "Undo", exact: true })).toHaveCount(0);
    await cull.getByRole("button", { name: "Exit cull", exact: true }).click();
    await expect(page.getByRole("button", { name: "Cull", exact: true })).toBeFocused();
    await expect(inspector.getByRole("combobox", { name: /^Flag:/ })).toContainText("Unflagged");
    await inspector.getByRole("combobox", { name: /^Rating:/ }).click();
    await page.getByRole("option", { name: "4 stars", exact: true }).click();
    await inspector.getByRole("button", { name: "Apply", exact: true }).click();
    await expect(inspector.getByRole("button", { name: "Apply", exact: true })).toBeDisabled();
    await expect(page.getByRole("alert")).toHaveCount(0);
    const readCheckpoint = () =>
      page.evaluate(async () => {
        const db = await new Promise<IDBDatabase>((resolve, reject) => {
          const request = indexedDB.open("docbank-photo-cull-v1", 1);
          request.onsuccess = () => resolve(request.result);
          request.onerror = () => reject(request.error);
        });
        try {
          const tx = db.transaction(["checkpoints", "markers"], "readonly");
          const markers = tx.objectStore("markers").getAllKeys();
          const request = tx.objectStore("checkpoints").getAll();
          const [keys, checkpoints] = await Promise.all([
            new Promise<IDBValidKey[]>((resolve, reject) => {
              markers.onsuccess = () => resolve(markers.result);
              markers.onerror = () => reject(markers.error);
            }),
            new Promise<{ runID: string; position: number }[]>((resolve, reject) => {
              request.onsuccess = () => resolve(request.result);
              request.onerror = () => reject(request.error);
            }),
          ]);
          const indices = keys
            .filter((key) => (key as IDBValidKey[])[1] === "visited")
            .map((key) => Number((key as IDBValidKey[])[2]));
          if (indices.some((value, index) => value !== index))
            throw new Error("Visited markers changed order");
          return checkpoints[0]!;
        } finally {
          db.close();
        }
      });
    const originalCheckpoint = await readCheckpoint();
    const writes = await page.evaluate(
      () =>
        (window as unknown as Window & { cullWrites: { store: string; bytes: number }[] })
          .cullWrites,
    );
    expect(writes.length).toBeGreaterThan(0);
    expect(writes.every((write) => write.store !== "members" && write.bytes < 256)).toBe(true);
    await page.getByRole("button", { name: "Cull", exact: true }).click();
    const replacementConfirmation = page.getByRole("dialog", {
      name: "Replace saved cull progress?",
    });
    await replacementConfirmation.getByRole("button", { name: "Cancel", exact: true }).click();
    await expect(replacementConfirmation).toHaveCount(0);
    await expect(cull).toHaveCount(0);
    expect(await readCheckpoint()).toEqual(originalCheckpoint);
    await page.getByRole("button", { name: "Resume cull", exact: true }).click();
    await expect(cull.locator("header strong")).toHaveText(resumedName);
    const other = await page.context().newPage();
    try {
      const otherURL = new URL(await fixture.run("web", "--no-browser"));
      otherURL.pathname = "/photos";
      await other.goto(otherURL.href);
      await other.getByRole("button", { name: "Resume cull", exact: true }).click();
      const otherCull = other.getByRole("dialog", { name: "Cull photos" });
      await expect(otherCull.locator(".photo-stage img")).toBeVisible();
      await expect(
        otherCull.getByRole("button", { name: "Scope: unflagged", exact: true }),
      ).toBeEnabled();
      await other.keyboard.press("s");
      await other
        .getByRole("dialog", { name: "Change scope and reset this pass's progress?" })
        .getByRole("button", { name: "Reset pass", exact: true })
        .click();
      await expect(
        otherCull.getByRole("button", { name: "Scope: picks", exact: true }),
      ).toBeVisible();
      await expect(
        otherCull.getByRole("button", { name: "Review unflagged remainder", exact: true }),
      ).toBeVisible();
      const changedPass = await readCheckpoint();
      await page.keyboard.press("ArrowRight");
      await expect(cull.getByRole("alert")).toContainText("Cull progress changed in another tab");
      expect(await readCheckpoint()).toEqual(changedPass);
      await cull.getByRole("button", { name: "Exit cull", exact: true }).click();
      await otherCull.getByRole("button", { name: "Exit cull", exact: true }).click();
      await other.getByRole("button", { name: "Cull", exact: true }).click();
      await other
        .getByRole("dialog", { name: "Replace saved cull progress?" })
        .getByRole("button", { name: "Replace", exact: true })
        .click();
      await expect(otherCull.locator(".photo-stage img")).toBeVisible();
      await page.getByRole("button", { name: "Resume cull", exact: true }).click();
      await expect(cull.locator(".photo-stage img")).toBeVisible();
      for (let i = 0; i < 10; i++) await other.keyboard.press("ArrowRight");
      for (let i = 0; i < 9; i++) await other.keyboard.press("ArrowLeft");
      await expect(otherCull.locator(".photo-stage img")).toBeVisible();
      const replacementName = await otherCull.locator("header strong").innerText();
      await otherCull.getByRole("button", { name: "Exit cull", exact: true }).click();
      await expect(other.getByRole("button", { name: "Cull", exact: true })).toBeFocused();
      const replacement = await readCheckpoint();
      expect(replacement.runID).not.toBe(originalCheckpoint.runID);
      await page.keyboard.press("ArrowRight");
      await expect(cull.getByRole("alert")).toContainText("Cull progress changed in another tab");
      expect(await readCheckpoint()).toEqual(replacement);
      await cull.getByRole("button", { name: "Exit cull", exact: true }).click();
      await page.getByRole("button", { name: "Resume cull", exact: true }).click();
      await expect(cull.locator("header strong")).toHaveText(replacementName);
      await page.keyboard.press("ArrowRight");
      await cull.getByRole("button", { name: "Exit cull", exact: true }).click();
      expect((await readCheckpoint()).position).toBe(replacement.position + 1);
      await page.getByRole("button", { name: "Resume cull", exact: true }).click();
      await expect(cull.locator(".photo-stage img")).toBeVisible();
      resumedName = await cull.locator("header strong").innerText();
      await cull.getByRole("button", { name: "Exit cull", exact: true }).click();
      await other.getByRole("button", { name: "Resume cull", exact: true }).click();
      await expect(otherCull.locator(".photo-stage img")).toBeVisible();
      let release!: () => void;
      const held = new Promise<void>((resolve) => {
        release = resolve;
      });
      let captured!: () => void;
      const started = new Promise<void>((resolve) => {
        captured = resolve;
      });
      await page.route("**/api/v1/workspace/queries", async (route) => {
        const response = await route.fetch();
        captured();
        await held;
        await route.fulfill({ response });
      });
      await page.getByRole("button", { name: "Cull", exact: true }).click();
      await replacementConfirmation.getByRole("button", { name: "Replace", exact: true }).click();
      await started;
      await other.keyboard.press("ArrowRight");
      await expect(otherCull.locator(".photo-stage img")).toBeVisible();
      await otherCull.getByRole("button", { name: "Exit cull", exact: true }).click();
      await expect(other.getByRole("button", { name: "Resume cull", exact: true })).toBeFocused();
      const newer = await readCheckpoint();
      release();
      await expect(page.getByRole("alert")).toContainText("Resume before replacing it");
      expect(await readCheckpoint()).toEqual(newer);
      await page.unroute("**/api/v1/workspace/queries");
      await page.getByRole("button", { name: "Resume cull", exact: true }).click();
      await expect(cull.locator(".photo-stage img")).toBeVisible();
      resumedName = await cull.locator("header strong").innerText();
      await cull.getByRole("button", { name: "Exit cull", exact: true }).click();
    } finally {
      await other.close();
    }
    await page.goto("about:blank");
    const resumedURL = new URL(await fixture.run("web", "--no-browser"));
    resumedURL.pathname = "/photos";
    await page.goto(resumedURL.href);
    await page.getByRole("button", { name: "Resume cull", exact: true }).click();
    await expect(cull.locator("header strong")).toHaveText(resumedName);
    await cull.getByRole("button", { name: "Exit cull", exact: true }).click();
    await page.locator(".photo-image").nth(0).click();
    await page
      .locator(".photo-image")
      .nth(1)
      .click({ modifiers: ["Control"] });
    await page.getByRole("button", { name: "Compare", exact: true }).click();
    const compare = page.getByRole("dialog", { name: "Compare photos" });
    await expect(compare.locator(".frames img")).toHaveCount(2);
    await expect(compare.locator(".frames img").first()).toBeVisible();
    await compare.locator(".frames .photo-viewport").first().hover();
    await page.mouse.wheel(0, -250);
    await expect
      .poll(async () =>
        compare.locator(".frames .pan").evaluateAll((elements) => {
          const scales = elements.map((element) =>
            Number((element as HTMLElement).style.transform.match(/scale\(([^)]+)\)/)?.[1]),
          );
          return scales.length === 2 && scales[0]! > 1 && scales[0] === scales[1];
        }),
      )
      .toBe(true);
    await compare.getByRole("button", { name: "Manual closeup", exact: true }).click();
    const comparedName = await compare.locator(".frames h2").first().innerText();
    const keyboardTarget = compare.getByRole("button", {
      name: `Position closeup on ${comparedName}`,
    });
    await page.keyboard.press("Tab");
    await expect(keyboardTarget).toBeFocused();
    await page.keyboard.press("ArrowRight");
    await page.keyboard.press("Enter");
    await expect(compare.locator(".closeups section")).toHaveCount(2);
    await compare.getByRole("button", { name: "Clear closeup", exact: true }).click();
    await compare.getByRole("button", { name: "Manual closeup", exact: true }).click();
    await compare
      .locator(".frames .photo-viewport")
      .first()
      .click({ position: { x: 350, y: 360 } });
    await expect(compare.locator(".closeups img")).toHaveCount(2);
    await expect(compare.locator(".closeups img").first()).toBeVisible();
    await compare.getByRole("button", { name: "Manual closeup", exact: true }).click();
    const frame = await compare.locator(".frames .photo-viewport").nth(1).boundingBox();
    await page.mouse.move(frame!.x + frame!.width * 0.4, frame!.y + frame!.height * 0.45);
    await page.mouse.down();
    await page.mouse.move(frame!.x + frame!.width * 0.6, frame!.y + frame!.height * 0.65);
    await page.mouse.up();
    await expect(compare.getByRole("status")).toHaveCount(0);
    await capturePhotoThemes(page, output!, "web-photo-compare-closeup", () =>
      page
        .locator(".frames img, .closeups img")
        .evaluateAll((images) =>
          images.every((image) => (image as HTMLImageElement).naturalWidth > 512),
        ),
    );
    await compare.getByRole("button", { name: "Exit compare", exact: true }).click();
    for (let i = 2; i < 6; i++)
      await page
        .locator(".photo-image")
        .nth(i)
        .click({ modifiers: ["Control"] });
    await page.getByRole("button", { name: "Compare", exact: true }).click();
    await expect(compare.locator(".frames img")).toHaveCount(6);
    await capturePhotoThemes(page, output!, "web-photo-compare-six", () =>
      compare
        .locator(".frames img")
        .evaluateAll((images) =>
          images.every((image) => (image as HTMLImageElement).naturalWidth > 512),
        ),
    );
    await compare.getByRole("button", { name: "Exit compare", exact: true }).click();
    await page
      .getByRole("searchbox", { name: "Search photos", exact: true })
      .fill("Synthetic-photo-00001");
    await page.getByRole("button", { name: "Search", exact: true }).click();
    await expect(page.locator(".photo-cell")).toHaveCount(1);
    await page.getByRole("button", { name: "Cull", exact: true }).click();
    await replacementConfirmation.getByRole("button", { name: "Replace", exact: true }).click();
    await expect(cull.locator(".photo-stage img")).toBeVisible();
    const finalPhoto = await cull.locator("header strong").innerText();
    await page.keyboard.press("p");
    await expect(cull.getByRole("heading", { name: "Pass complete", exact: true })).toBeVisible();
    await expect(cull.locator("p[role=status]")).toHaveCount(0);
    await expect(
      cull.getByRole("button", { name: "Review unflagged remainder", exact: true }),
    ).toHaveCount(0);
    await cull.getByRole("button", { name: "Undo", exact: true }).click();
    await cull.getByRole("button", { name: "Review unflagged remainder", exact: true }).click();
    await expect(cull.locator("header strong")).toHaveText(finalPhoto);
  } finally {
    await fixture.dispose();
  }
});
