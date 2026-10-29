import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { expect, test, type Locator } from "@playwright/test";
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
  test.setTimeout(300_000);
  await mkdir(output, { recursive: true });
  const workspace = await mkdtemp(path.join(repository, ".superpowers/best-shots-vault-"));
  const fixture = photoFixture(workspace);
  const context = await browser.newContext({
    viewport: { width: 1280, height: 800 },
    deviceScaleFactor: Number(process.env.DOCBANK_SCREENSHOT_SCALE ?? 1),
    colorScheme: "dark",
    ...(before
      ? {}
      : { recordVideo: { dir: path.join(output, "video"), size: { width: 1280, height: 800 } } }),
  });
  await context.addInitScript(() => {
    document.addEventListener("DOMContentLoaded", () => {
      const pointer = document.createElement("div");
      pointer.id = "capture-pointer";
      pointer.style.cssText =
        "position:fixed;width:14px;height:14px;border:2px solid #202124;border-radius:50%;background:white;pointer-events:none;z-index:2147483647;transform:translate(-50%,-50%);left:30px;top:100px";
      const caption = document.createElement("div");
      caption.id = "capture-step";
      caption.style.cssText =
        "position:fixed;top:52px;left:50%;transform:translateX(-50%);padding:8px 16px;border-radius:6px;background:#111e;color:white;font:16px system-ui;pointer-events:none;z-index:2147483646;white-space:nowrap";
      document.body.append(pointer, caption);
      document.addEventListener("mousemove", (event) => {
        pointer.style.left = `${event.clientX}px`;
        pointer.style.top = `${event.clientY}px`;
      });
    });
  });
  const page = await context.newPage();
  const resultPause = 2500;
  let number = 0;
  let position = { x: 30, y: 100 };
  const step = async (text: string) => {
    if (before) return;
    number++;
    await page.locator("#capture-step").evaluate((el, text) => {
      el.textContent = text;
    }, `Step ${number} · ${text}`);
    await page.waitForTimeout(1200);
  };
  const point = async (locator: Locator, text: string, click = true) => {
    await step(text);
    const box = await locator.boundingBox();
    if (!box) throw new Error(`Missing recording target: ${text}`);
    const target = { x: box.x + box.width / 2, y: box.y + box.height / 2 };
    for (let i = 1; i <= 24; i++) {
      await page.mouse.move(
        position.x + ((target.x - position.x) * i) / 24,
        position.y + ((target.y - position.y) * i) / 24,
      );
      await page.waitForTimeout(30);
    }
    position = target;
    await page.waitForTimeout(500);
    if (click) await locator.click();
  };
  context.setDefaultTimeout(15_000);
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  const capture = async (name: string) => {
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({
      path: path.join(output, `${before ? "before" : "after"}-${name}-dark.png`),
      animations: "disabled",
      style: "#capture-pointer, #capture-step { visibility: hidden }",
    });
    if (!before) await page.waitForTimeout(resultPause);
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
    await step("Review suggested keep and cut badges");
    await capture("library");
    await point(
      page.getByRole("button", { name: "Expand stack of 3 photos" }).last(),
      "Expand the burst",
    );
    const cells = page.locator(".photo-cell");
    await point(cells.last().getByRole("button", { name: /^Select / }), "Select a frame");
    if (!before)
      await expect(page.getByRole("button", { name: "Accept all", exact: true })).toBeEnabled();
    await capture("selected-stack");
    await point(page.getByRole("button", { name: "Cull", exact: true }), "Open Cull");
    const cull = page.getByRole("dialog", { name: "Cull photos" });
    await expect(cull.locator(".photo-stage img")).toBeVisible();
    // Review the third frame, which carries the synthetic detected face.
    const third = cull.getByRole("button", { name: "Show Synthetic-photo-00003.jpg", exact: true });
    if (await third.count()) await point(third, "Review the frame with a detected face");
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
      await point(cull.locator(".closeups figure").first(), "Read the face measurements", false);
      await page.waitForTimeout(10_000);
      const auto = cull.getByRole("switch", { name: "Auto-advance" });
      if (await auto.isChecked()) {
        await point(auto, "Turn off Auto-advance");
        await page.waitForTimeout(resultPause);
      }
      await cull.locator(".photo-cull").focus();
      const badge = cull.locator(".photo-stage .proposal");
      await expect(badge).toHaveAttribute("aria-label", "Suggested keep");
      const key = "x";
      const accepted = page.waitForResponse(
        (r) => r.url().endsWith("/photos/authored/edit") && r.request().method() === "POST",
      );
      await step("Press X to cut this frame");
      await page.keyboard.press(key);
      const acceptedBody = await (await accepted).json();
      expect(acceptedBody.after).toHaveLength(1);
      expect(acceptedBody.after[0].values.flag).toBe("reject");
      await expect(cull.getByLabel("Suggested keep").first()).toBeVisible();
      await page.waitForTimeout(resultPause);
      await point(cull.getByRole("button", { name: "Undo", exact: true }), "Undo");
      await expect(badge).toHaveAttribute("aria-label", "Suggested keep");
      await page.waitForTimeout(resultPause);
      const dismissed = page.waitForResponse(
        (r) => r.url().endsWith("/photos/authored/stack") && r.request().method() === "POST",
      );
      await point(cull.getByRole("button", { name: "Dismiss all", exact: true }), "Dismiss all");
      const dismissalResponse = await dismissed;
      expect(dismissalResponse.status(), await dismissalResponse.text()).toBe(200);
      const dismissal = await dismissalResponse.json();
      expect(dismissal.receipt_id).toBeTruthy();
      expect(dismissal.after).toHaveLength(3);
      expect(dismissal.after.every((p: { values: { flag: string } }) => p.values.flag === "")).toBe(
        true,
      );
      await expect(cull.getByRole("button", { name: "Dismiss all", exact: true })).toHaveCount(0);
      await point(cull.getByRole("button", { name: "Undo", exact: true }), "Undo");
      await expect(cull.locator(".filmstrip .proposal")).toHaveCount(3);
      await page.waitForTimeout(resultPause);
      await point(cull.getByRole("button", { name: "Dismiss all", exact: true }), "Dismiss all");
      await expect(cull.getByRole("button", { name: "Dismiss all", exact: true })).toHaveCount(0);
      await page.waitForTimeout(resultPause);
      const kept = page.waitForResponse(
        (r) => r.url().endsWith("/photos/authored/stack") && r.request().method() === "POST",
      );
      await step("Press Enter to keep the current frame");
      await page.keyboard.press("Enter");
      const keptBody = await (await kept).json();
      expect(keptBody.after).toHaveLength(1);
      expect(keptBody.after[0].values.flag).toBe("pick");
      await page.waitForTimeout(resultPause);
      await point(cull.getByRole("button", { name: "Undo", exact: true }), "Undo");
      await expect(cull.locator(".filmstrip .proposal")).toHaveCount(0);
      await page.waitForTimeout(resultPause);
      await point(cull.getByRole("button", { name: "Exit cull", exact: true }), "Exit cull");
      await expect(cull).toHaveCount(0);
      const reopened = new URL(await fixture.run("web", "--no-browser"));
      reopened.pathname = "/photos";
      await page.goto(reopened.href);
      await step("Reload to confirm the saved choices");
      await page.waitForTimeout(resultPause);
      await expect(page.locator(".photo-cell").first()).toBeVisible();
      // The other stack remains pending and can be accepted in one response.
      await point(page.locator(".photo-image").first(), "Select the remaining burst");
      await expect(page.getByRole("button", { name: "Accept all", exact: true })).toBeEnabled();
      await point(page.getByRole("button", { name: "Accept all", exact: true }), "Accept all");
      await expect(
        page.getByRole("button", { name: "Undo last photo edit", exact: true }),
      ).toBeVisible();
      await page.waitForTimeout(resultPause);
      await point(
        page.getByRole("button", { name: "Undo last photo edit", exact: true }),
        "Undo last photo edit",
      );
      const finalURL = new URL(await fixture.run("web", "--no-browser"));
      finalURL.pathname = "/photos";
      await page.goto(finalURL.href);
      await step("Reload to review restored suggestions");
      await page.waitForTimeout(resultPause);
      await expect(page.getByLabel(/Suggested (keep|cut)/).first()).toBeVisible();
      await point(page.locator(".photo-image").first(), "Select the remaining burst");
      await expect(page.getByRole("button", { name: "Dismiss all", exact: true })).toBeEnabled();
      await point(page.getByRole("button", { name: "Dismiss all", exact: true }), "Dismiss all");
      await expect(page.getByRole("button", { name: "Dismiss all", exact: true })).toHaveCount(0);
      await expect(page.locator(".photo-cell .proposal")).toHaveCount(0);
      await point(
        page.getByRole("button", { name: "Undo last photo edit", exact: true }),
        "Undo last photo edit",
      );
      await expect(page.locator(".photo-cell .proposal").first()).toBeVisible();
      await page.waitForTimeout(resultPause);
      await point(page.getByRole("button", { name: "Dismiss all", exact: true }), "Dismiss all");
      await expect(page.locator(".photo-cell .proposal")).toHaveCount(0);
      await expect(page.locator(".photo-cell img").first()).toBeVisible();
      await page.waitForTimeout(resultPause);
    }
    expect(errors).toEqual([]);
    await writeFile(
      path.join(output, `${before ? "before" : "after"}-browser.json`),
      JSON.stringify(
        {
          errors,
          video: await page.video()?.path(),
          viewport: { width: 1280, height: 800 },
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
