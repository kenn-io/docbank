import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { expect, test } from "@playwright/test";
import { mkdir, mkdtemp, writeFile } from "node:fs/promises";
import path from "node:path";
import { photoFixture, repository } from "./photo-fixture.js";
const output =
  process.env.DOCBANK_BEST_SHOTS_PROOF_DIR ??
  path.join(repository, ".superpowers/artifacts/best-shots");
test.skip(
  !process.env.DOCBANK_BEST_SHOTS_PROOF_DIR,
  "DOCBANK_BEST_SHOTS_PROOF_DIR enables the synthetic best-shot proof",
);
const before = process.env.DOCBANK_BEST_SHOTS_BEFORE === "1";
test("best shots stay separate from authored flags and settle across refresh", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await mkdir(output, { recursive: true });
  const workspace = await mkdtemp(path.join(repository, ".superpowers/best-shots-vault-"));
  const fixture = photoFixture(workspace);
  const context = await browser.newContext({
    viewport: { width: 1440, height: 960 },
    deviceScaleFactor: Number(process.env.DOCBANK_SCREENSHOT_SCALE ?? 1),
    colorScheme: "dark",
    ...(before
      ? {}
      : { recordVideo: { dir: path.join(output, "video"), size: { width: 1440, height: 960 } } }),
  });
  const page = await context.newPage();
  context.setDefaultTimeout(15_000);
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  const capture = async (name: string) => {
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({
      path: path.join(output, `${before ? "before" : "after"}-${name}-dark.png`),
      animations: "disabled",
    });
    if (!before) await page.waitForTimeout(600);
  };
  try {
    if (before)
      await promisify(execFile)(
        "go",
        [
          "run",
          "-tags",
          "fts5",
          "./frontend/screenshots/photos-fixture.go",
          "-bursts",
          "-current-metadata",
          "-detected-faces",
          path.join(workspace, "vault"),
          "6",
        ],
        {
          cwd: path.join(repository, ".superpowers/base"),
          env: { ...process.env, DOCBANK_TELEMETRY_ENABLED: "0" },
          timeout: 480_000,
        },
      );
    else await fixture.seed(6, "-bursts", "-current-metadata", "-detected-faces", "-best-shots");
    const url = new URL(await fixture.run("web", "--no-browser"));
    url.pathname = "/photos";
    await page.goto(url.href);
    await expect(page.getByRole("button", { name: "Expand stack of 3 photos" })).toHaveCount(2);
    await expect(page.locator(".photo-cell img").first()).toBeVisible();
    if (!before) await expect(page.getByLabel(/Suggested (keep|cut)/).first()).toBeVisible();
    await capture("library");
    await page.getByRole("button", { name: "Expand stack of 3 photos" }).last().click();
    const cells = page.locator(".photo-cell");
    await cells
      .last()
      .getByRole("button", { name: /^Select / })
      .click();
    if (!before)
      await expect(page.getByRole("button", { name: "Accept all", exact: true })).toBeEnabled();
    await capture("selected-stack");
    await page.getByRole("button", { name: "Cull", exact: true }).click();
    const cull = page.getByRole("dialog", { name: "Cull photos" });
    await expect(cull.locator(".photo-stage img")).toBeVisible();
    // Review the third frame, which carries the synthetic detected face.
    const third = cull.getByRole("button", { name: "Show Synthetic-photo-00003.jpg", exact: true });
    if (await third.count()) await third.click();
    await expect(cull.getByRole("region", { name: "Face closeups" })).toBeVisible();
    if (!before) {
      await expect(cull.getByText("Sharpness", { exact: true }).first()).toBeVisible();
      await expect(cull.getByText("Smile", { exact: true }).first()).toBeVisible();
      await expect(cull.locator(".filmstrip .proposal")).toHaveCount(3);
    }
    await expect(cull.locator(".closeups img").first()).toBeVisible();
    await expect(cull.getByText("No preview", { exact: true })).toHaveCount(0);
    await page.waitForTimeout(500);
    await expect(cull.getByText("No preview", { exact: true })).toHaveCount(0);
    await capture("face-reports");
    if (!before) {
      const auto = cull.getByRole("switch", { name: "Auto-advance" });
      if ((await auto.getAttribute("aria-checked")) === "true") await auto.click();
      const badge = cull.locator(".photo-stage .proposal");
      await expect(badge).toHaveAttribute("aria-label", "Suggested keep");
      const key = "x";
      const accepted = page.waitForResponse(
        (r) => r.url().endsWith("/photos/authored/edit") && r.request().method() === "POST",
      );
      await page.keyboard.press(key);
      const acceptedBody = await (await accepted).json();
      expect(acceptedBody.after).toHaveLength(1);
      expect(acceptedBody.after[0].values.flag).toBe("reject");
      await expect(cull.getByLabel("Suggested keep").first()).toBeVisible();
      await page.waitForTimeout(600);
      await cull.getByRole("button", { name: "Undo", exact: true }).click();
      await expect(badge).toHaveAttribute("aria-label", "Suggested keep");
      await page.waitForTimeout(600);
      const dismissed = page.waitForResponse(
        (r) => r.url().endsWith("/photos/authored/stack") && r.request().method() === "POST",
      );
      await cull.getByRole("button", { name: "Dismiss all", exact: true }).click();
      const dismissalResponse = await dismissed;
      expect(dismissalResponse.status(), await dismissalResponse.text()).toBe(200);
      const dismissal = await dismissalResponse.json();
      expect(dismissal.receipt_id).toBeTruthy();
      expect(dismissal.after).toHaveLength(3);
      expect(dismissal.after.every((p: { values: { flag: string } }) => p.values.flag === "")).toBe(
        true,
      );
      await expect(cull.getByRole("button", { name: "Dismiss all", exact: true })).toHaveCount(0);
      await cull.getByRole("button", { name: "Undo", exact: true }).click();
      await expect(cull.locator(".filmstrip .proposal")).toHaveCount(3);
      await page.waitForTimeout(600);
      await cull.getByRole("button", { name: "Dismiss all", exact: true }).click();
      await expect(cull.getByRole("button", { name: "Dismiss all", exact: true })).toHaveCount(0);
      await page.waitForTimeout(600);
      const kept = page.waitForResponse(
        (r) => r.url().endsWith("/photos/authored/stack") && r.request().method() === "POST",
      );
      await page.keyboard.press("Enter");
      const keptBody = await (await kept).json();
      expect(keptBody.after).toHaveLength(1);
      expect(keptBody.after[0].values.flag).toBe("pick");
      await page.waitForTimeout(600);
      await cull.getByRole("button", { name: "Undo", exact: true }).click();
      await expect(cull.locator(".filmstrip .proposal")).toHaveCount(0);
      await page.waitForTimeout(600);
      await cull.getByRole("button", { name: "Exit cull", exact: true }).click();
      await expect(cull).toHaveCount(0);
      const reopened = new URL(await fixture.run("web", "--no-browser"));
      reopened.pathname = "/photos";
      await page.goto(reopened.href);
      await expect(page.locator(".photo-cell").first()).toBeVisible();
      // The other stack remains pending and can be accepted in one response.
      await page.locator(".photo-image").first().click();
      await expect(page.getByRole("button", { name: "Accept all", exact: true })).toBeEnabled();
      await page.getByRole("button", { name: "Accept all", exact: true }).click();
      await expect(
        page.getByRole("button", { name: "Undo last photo edit", exact: true }),
      ).toBeVisible();
      await page.waitForTimeout(600);
      await page.getByRole("button", { name: "Undo last photo edit", exact: true }).click();
      const finalURL = new URL(await fixture.run("web", "--no-browser"));
      finalURL.pathname = "/photos";
      await page.goto(finalURL.href);
      await expect(page.getByLabel(/Suggested (keep|cut)/).first()).toBeVisible();
      await page.locator(".photo-image").first().click();
      await expect(page.getByRole("button", { name: "Dismiss all", exact: true })).toBeEnabled();
      await page.getByRole("button", { name: "Dismiss all", exact: true }).click();
      await expect(page.getByRole("button", { name: "Dismiss all", exact: true })).toHaveCount(0);
      await expect(page.locator(".photo-cell img").first()).toBeVisible();
      await page.waitForTimeout(600);
    }
    expect(errors).toEqual([]);
    await writeFile(
      path.join(output, `${before ? "before" : "after"}-browser.json`),
      JSON.stringify(
        {
          errors,
          video: await page.video()?.path(),
          viewport: { width: 1440, height: 960 },
          scale: Number(process.env.DOCBANK_SCREENSHOT_SCALE ?? 1),
        },
        null,
        2,
      ),
    );
  } finally {
    await context.close();
    await fixture.dispose();
  }
});
