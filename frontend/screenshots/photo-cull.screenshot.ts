import { expect, test } from "@playwright/test";
import { mkdir, mkdtemp } from "node:fs/promises";
import path from "node:path";
import { photoFixture, repository } from "./photo-fixture.js";

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
    let acceptDialogs = true;
    page.on("dialog", (dialog) => (acceptDialogs ? dialog.accept() : dialog.dismiss()));
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
    acceptDialogs = false;
    const canceledScope = page.waitForEvent("dialog");
    await page.keyboard.press("s");
    await canceledScope;
    await expect(cull.getByRole("button", { name: "Scope: unflagged", exact: true })).toBeVisible();
    await expect(cull.locator("header strong")).toHaveText(firstName);
    acceptDialogs = true;
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
    await page.screenshot({
      path: path.join(output!, "web-photo-cull.png"),
      animations: "disabled",
    });
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
    expect(Object.keys(originalCheckpoint).sort()).toEqual([
      "autoAdvance",
      "passID",
      "position",
      "revision",
      "runID",
      "scope",
    ]);
    acceptDialogs = false;
    const canceledReplacement = page.waitForEvent("dialog");
    await page.getByRole("button", { name: "Cull", exact: true }).click();
    await canceledReplacement;
    await expect(cull).toHaveCount(0);
    expect(await readCheckpoint()).toEqual(originalCheckpoint);
    acceptDialogs = true;
    await page.getByRole("button", { name: "Resume cull", exact: true }).click();
    await expect(cull.locator("header strong")).toHaveText(resumedName);
    const other = await page.context().newPage();
    try {
      const otherURL = new URL(await fixture.run("web", "--no-browser"));
      otherURL.pathname = "/photos";
      await other.goto(otherURL.href);
      other.on("dialog", (dialog) => dialog.accept());
      await other.getByRole("button", { name: "Resume cull", exact: true }).click();
      const otherCull = other.getByRole("dialog", { name: "Cull photos" });
      await expect(otherCull.locator(".photo-stage img")).toBeVisible();
      await other.keyboard.press("s");
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
    await page.screenshot({
      path: path.join(output!, "web-photo-compare-closeup.png"),
      animations: "disabled",
    });
    await compare.getByRole("button", { name: "Exit compare", exact: true }).click();
    await page
      .getByRole("searchbox", { name: "Search photos", exact: true })
      .fill("Synthetic-photo-00001");
    await page.getByRole("button", { name: "Search", exact: true }).click();
    await expect(page.locator(".photo-cell")).toHaveCount(1);
    await page.getByRole("button", { name: "Cull", exact: true }).click();
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
