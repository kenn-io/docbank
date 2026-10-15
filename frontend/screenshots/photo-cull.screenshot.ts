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
    await page.goto(url.href);
    await expect(page.locator(".photo-cell img").first()).toBeVisible();
    await page.getByRole("button", { name: "Cull", exact: true }).click();
    const cull = page.getByRole("dialog", { name: "Cull photos" });
    await expect(cull.locator(".photo-stage img")).toBeVisible();
    const firstName = await cull.locator("header strong").innerText();
    await page.keyboard.press("Control+p");
    await expect(cull.locator("header strong")).toHaveText(firstName);
    await page.keyboard.press("p");
    await expect(cull.locator("header strong")).not.toHaveText(firstName);
    await expect(cull.getByRole("button", { name: "Undo", exact: true })).toBeVisible();
    const resumedName = await cull.locator("header strong").innerText();
    await expect(cull.locator(".filmstrip img").first()).toBeVisible();
    await page.screenshot({
      path: path.join(output!, "web-photo-cull.png"),
      animations: "disabled",
    });
    await cull.getByRole("button", { name: "Exit cull", exact: true }).click();
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
  } finally {
    await fixture.dispose();
  }
});
