import geometry from "../../internal/redactiontest/testdata/page-geometry.json" with { type: "json" };
import { croppedReviewPDF } from "./redaction-review-fixture.js";
import { expect, test, type Route, type Locator } from "@playwright/test";
import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";
import type { RedactionPreview } from "../src/generated/docbank.js";
import type {
  PluginRegistry,
  ViewportPlugin,
  ScrollPlugin,
  DocumentManagerPlugin,
} from "@embedpdf/svelte-pdf-viewer";
const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const captures = process.env.DOCBANK_REDACTION_REVIEW_SCREENSHOT_DIR;
// The browser decoder is the integration boundary: engine tasks still succeed.
declare global {
  interface Window {
    reviewDecode: { mode: "pass" | "delay" | "fail"; pending: (() => void)[] };
  }
}

test.skip(!captures, "DOCBANK_REDACTION_REVIEW_SCREENSHOT_DIR enables real PDF review");
test.use({ reducedMotion: "reduce", deviceScaleFactor: 2 });

test("reviews masks and downloads the exact verified PDF in the browser", async ({ page }) => {
  test.setTimeout(360000);
  page.setDefaultTimeout(20000);
  const binary = process.env.DOCBANK_SCREENSHOT_BINARY,
    inspector = process.env.DOCBANK_REDACTION_REVIEW_INSPECTOR;
  if (!binary || !inspector || !path.isAbsolute(binary) || !path.isAbsolute(inspector))
    throw new Error("Explicit branch binaries required");
  const scratch =
    process.env.DOCBANK_REDACTION_REVIEW_WORKSPACE ??
    (await mkdtemp(path.join(tmpdir(), "docbank-redaction-browser-")));
  const env = {
    ...process.env,
    DOCBANK_HOME: path.join(scratch, "vault"),
    DOCBANK_TELEMETRY_ENABLED: "0",
  };
  const run = async (...args: string[]) =>
    (
      await exec(binary, args, { cwd: repository, env, timeout: 180000, maxBuffer: 20 << 20 })
    ).stdout.trim();
  let failure: unknown,
    started = false;
  const errors: string[] = [];
  let originalLoads = 0;
  page.on("request", (request) => {
    if (new URL(request.url()).pathname === "/api/daemon/web-download/file") originalLoads++;
  });
  page.on("pageerror", (e) => errors.push(e.message));
  const stop = async () => {
    if (started) {
      if (JSON.parse(await run("daemon", "status", "--json")).running) await run("daemon", "stop");
      expect(JSON.parse(await run("daemon", "status", "--json")).running).toBe(false);
      started = false;
    }
  };
  try {
    await mkdir(captures!, { recursive: true, mode: 0o700 });
    const renderer =
      process.env.DOCBANK_TEST_PDFTOPPM ?? (await exec("which", ["pdftoppm"])).stdout.trim();
    const limiter = (await exec("which", ["prlimit"])).stdout.trim();
    await exec(
      "go",
      [
        "run",
        "-tags",
        "fts5",
        "./frontend/screenshots/bates-recovery-fixture.go",
        scratch,
        inspector,
        renderer,
        limiter,
      ],
      { cwd: repository, env, timeout: 120000 },
    );
    started = true;
    await run("add", path.join(scratch, "two-pages.pdf"), "--dest", "/", "--json");
    let sessionURL = "";
    const open = async (
      name = "two-pages.pdf",
      reuseSession = false,
      fromReader = false,
      beforeReview?: () => Promise<void>,
    ) => {
      await page.goto("about:blank");
      const url = reuseSession ? sessionURL : await run("web", "--no-browser");
      sessionURL = url;
      const session = new URLSearchParams(new URL(url).hash.slice(1)).get("web_session")!;
      try {
        await page.goto(url, { waitUntil: "domcontentloaded" });
      } catch (cause) {
        throw new Error("Could not open synthetic browser session", {
          cause: new Error(String(cause).replaceAll(session, "[redacted]")),
        });
      }
      const file = page.getByRole("cell", { name, exact: true });
      if (fromReader) {
        await file.dblclick();
        const reader = page.getByRole("dialog", { name: `Read ${name}`, exact: true });
        await expect(reader.locator("embedpdf-container img").first()).toBeVisible();
        const zoom = reader.getByRole("textbox", { name: "Set zoom" });
        await zoom.fill("125");
        await zoom.press("Enter");
        const redact = await reader
          .getByRole("button", { name: "Redact PDF", exact: true })
          .elementHandle();
        // Enter review before the smooth page turn can move the viewport.
        await reader
          .getByRole("button", { name: "Next Page", exact: true })
          .evaluate((button, redact) => {
            (button as HTMLButtonElement).click();
            (redact as HTMLButtonElement).click();
          }, redact);
      } else {
        await file.click();
        await expect(page.getByRole("button", { name: "Redact PDF", exact: true })).toBeEnabled();
        await beforeReview?.();
        await page.getByRole("button", { name: "Redact PDF", exact: true }).click();
      }
      await expect(
        page.getByRole("button", { name: "Preview redaction", exact: true }),
      ).toBeVisible();
      return session;
    };
    await page.addInitScript(() => {
      if (location.protocol === "http:") localStorage.setItem("docbank-theme", "dark");
      window.reviewDecode = { mode: "pass", pending: [] };
      const decode = HTMLImageElement.prototype.decode;
      HTMLImageElement.prototype.decode = function () {
        const result = decode.call(this);
        if (!(this.getRootNode() instanceof ShadowRoot)) return result;
        const control = window.reviewDecode;
        if (control.mode === "fail")
          return result.then(() => {
            // A decode started in fail mode may finish after recovery begins.
            if (control.mode === "fail") throw new Error("Injected displayed-image decode failure");
          });
        if (control.mode === "delay")
          return result.then(() => {
            // A decode started during the pause may finish after it is released.
            if (control.mode === "delay")
              return new Promise<void>((resolve) => control.pending.push(resolve));
          });
        return result;
      };
    });
    const session = await open("two-pages.pdf", false, true);
    const drawer = page.getByRole("dialog", { name: "Redact two-pages.pdf", exact: true });
    const scrollIdle = async (target: Locator = drawer, targetPage?: number) => {
      // Page readiness can precede navigation and replacement tiles.
      await target.evaluate(
        () =>
          new Promise<void>((resolve) =>
            requestAnimationFrame(() => requestAnimationFrame(() => resolve())),
          ),
      );
      // A clamped page turn can leave the reader's navigation flag set.
      // Wait for actual viewport activity to finish.
      await expect
        .poll(() =>
          target.locator("embedpdf-container").evaluate(async (host, requestedPage) => {
            const registry = await (host as HTMLElement & { registry: Promise<PluginRegistry> })
              .registry;
            const scroll = registry.getPlugin<ScrollPlugin>("scroll")!.provides();
            return (
              (requestedPage !== undefined && scroll.getCurrentPage() !== requestedPage) ||
              registry.getPlugin<ViewportPlugin>("viewport")!.provides().isScrolling()
            );
          }, targetPage),
        )
        .toBe(false);
    };
    const awaitPage = async (n: number, target: Locator = drawer) => {
      await scrollIdle(target, n);
      await expect(target.getByRole("textbox", { name: "", exact: true })).toHaveValue(String(n));
      await expect(target.getByRole("switch", { name: "Draw rectangle", exact: true })).toBeEnabled(
        { timeout: 60000 },
      );
    };
    const originalPage = async (n: number, target: Locator = drawer) => {
      const field = target.getByRole("textbox", { name: "", exact: true });
      // The reader rewrites this box whenever its current page changes, even
      // between the fill and Enter. Retype until the page sticks.
      await expect(async () => {
        const currentPage = await target.locator("embedpdf-container").evaluate(async (host) => {
          const registry = await (host as HTMLElement & { registry: Promise<PluginRegistry> })
            .registry;
          return registry.getPlugin<ScrollPlugin>("scroll")!.provides().getCurrentPage();
        });
        if ((await field.inputValue()) !== String(n) || currentPage !== n) {
          await field.fill(String(n));
          await field.press("Enter");
        }
        await scrollIdle(target, n);
        await expect(field).toHaveValue(String(n), { timeout: 1000 });
        await expect(
          target.getByRole("switch", { name: "Draw rectangle", exact: true }),
        ).toBeEnabled({ timeout: 5000 });
      }).toPass({ timeout: 60000 });
    };
    await expect(drawer.getByRole("switch", { name: "Draw rectangle", exact: true })).toBeEnabled({
      timeout: 60000,
    });
    await awaitPage(2);
    expect(originalLoads).toBe(1);
    const readerWorkers = page.workers();
    // Returning from review keeps the live reader, including its page and zoom.
    await drawer.getByRole("button", { name: "Back to reading", exact: true }).click();
    await expect(drawer).toBeHidden();
    const reader = page.getByRole("dialog", { name: "Read two-pages.pdf", exact: true });
    await expect(reader.getByRole("textbox", { name: "Set zoom" })).toHaveValue("125");
    await expect(
      reader.locator('[data-review-page="2"]').locator("..").locator("img").first(),
    ).toBeInViewport({ ratio: 0.5 });
    await expect(reader.getByRole("textbox", { name: "", exact: true })).toHaveValue("2");
    await expect(reader.getByRole("button", { name: "Redact PDF", exact: true })).toBeFocused();
    await page.screenshot({
      path: path.join(captures!, "reader-return-page-two.png"),
      animations: "disabled",
    });
    const expectFitPage = async () => {
      await expect(async () => {
        const output = drawer.getByRole("region", { name: "Scrollable page image" });
        if (await output.isVisible()) {
          const r = await output.evaluate((e) => ({
            height: e.clientHeight,
            scroll: e.scrollHeight,
          }));
          expect(r.scroll).toBeLessThanOrEqual(r.height + 1);
        } else {
          const bounds = await drawer.locator('[data-review-page="1"]').boundingBox();
          const viewer = await drawer.locator("embedpdf-container").boundingBox();
          expect(bounds!.height).toBeGreaterThan(100);
          expect(bounds!.y).toBeGreaterThanOrEqual(viewer!.y);
          expect(bounds!.y + bounds!.height).toBeLessThanOrEqual(viewer!.y + viewer!.height);
        }
      }).toPass({ timeout: 10000 });
    };
    await reader.getByRole("button", { name: "Redact PDF", exact: true }).click();
    await originalPage(1);
    // Page navigation reports its target before the viewport finishes moving.
    await expectFitPage();
    await page.setViewportSize({ width: 640, height: 400 });
    await expect(
      drawer.getByRole("button", { name: "Preview redaction", exact: true }),
    ).toBeInViewport();
    const setReviewDrawing = async (toggle: Locator, checked: boolean) => {
      await expect(async () => {
        await toggle.setChecked(checked);
      }).toPass({ timeout: 15000 });
    };
    const shortDraw = drawer.getByRole("switch", { name: "Draw rectangle", exact: true });
    await expect(shortDraw).toBeEnabled();
    await setReviewDrawing(shortDraw, true);
    await setReviewDrawing(shortDraw, false);
    await page.screenshot({
      path: path.join(captures!, "redaction-short-editing.png"),
      animations: "disabled",
    });
    await page.setViewportSize({ width: 1440, height: 960 });
    expect(originalLoads).toBe(1);
    expect(page.workers()).toEqual(readerWorkers);
    await drawer.getByRole("button", { name: "Document Menu", exact: true }).click();
    for (const name of ["Print", "Download", "Export"])
      await expect(drawer.getByRole("menuitem", { name, exact: true })).toBeHidden();
    await drawer.getByRole("button", { name: "Document Menu", exact: true }).click();
    await drawer.locator("embedpdf-container").focus();
    await page.keyboard.press("ControlOrMeta+p");
    await expect(drawer.getByRole("heading", { name: "Print Settings" })).toBeHidden();
    await expectFitPage();
    const fitZoom = await drawer.getByRole("textbox", { name: "Set zoom" }).inputValue();
    // A new zoom position can appear before replacement images start
    // decoding. Observe that redraw before using the ready marking surface.
    const redraw = async (action: () => Promise<unknown>, target: Locator = drawer) => {
      await page.evaluate(() => (window.reviewDecode.mode = "delay"));
      await action();
      await expect
        .poll(() => page.evaluate(() => window.reviewDecode.pending.length))
        .toBeGreaterThan(0);
      await expect(
        target.getByRole("switch", { name: "Draw rectangle", exact: true }),
      ).toBeDisabled();
      await page.evaluate(() => {
        window.reviewDecode.mode = "pass";
        window.reviewDecode.pending.splice(0).forEach((resolve) => resolve());
      });
      await expect(
        target.getByRole("switch", { name: "Draw rectangle", exact: true }),
      ).toBeEnabled();
    };
    const setZoom = async (value: string, target: Locator = drawer) => {
      const zoom = target.getByRole("textbox", { name: "Set zoom" });
      await redraw(async () => {
        await zoom.fill(value);
        await zoom.press("Enter");
      }, target);
    };
    const scrollPage = async (layer: Locator, x = 0, y = 0, target: Locator = drawer) => {
      const before = await layer.evaluate(
        async (element, { x, y }) => {
          let viewport = element.parentElement;
          while (viewport && getComputedStyle(viewport).overflowY !== "auto")
            viewport = viewport.parentElement;
          if (!viewport) throw new Error("Reader scroll container absent");
          let top = element.getBoundingClientRect().y;
          const previous = { x: viewport.scrollLeft, y: viewport.scrollTop };
          await new Promise<void>((resolve) => {
            const done = () => {
              viewport.removeEventListener("scrollend", done);
              resolve();
            };
            viewport.addEventListener("scrollend", done);
            element.scrollIntoView({ block: "start", inline: "start", behavior: "instant" });
            top = element.getBoundingClientRect().y;
            viewport.scrollBy({ left: x, top: y, behavior: "instant" });
            if (viewport.scrollLeft === previous.x && viewport.scrollTop === previous.y) done();
          });
          return top;
        },
        { x, y },
      );
      // Scrolling may reuse existing tiles. Wait for the viewer to settle,
      // rather than requiring an image decode that might never be needed.
      await scrollIdle(target);
      await expect(
        target.getByRole("switch", { name: "Draw rectangle", exact: true }),
      ).toBeEnabled();
      return before;
    };
    const drawToggle = drawer.getByRole("switch", { name: "Draw rectangle", exact: true });
    await page.evaluate(() => (window.reviewDecode.mode = "delay"));
    const readerZoom = drawer.getByRole("textbox", { name: "Set zoom" });
    await readerZoom.fill("100");
    await readerZoom.press("Enter");
    await expect
      .poll(() => page.evaluate(() => window.reviewDecode.pending.length))
      .toBeGreaterThan(0);
    await expect(drawToggle).toBeDisabled();
    await page.evaluate(() => {
      window.reviewDecode.mode = "pass";
      window.reviewDecode.pending.splice(0).forEach((f) => f());
    });
    await expect(drawToggle).toBeEnabled();
    await page.evaluate(() => (window.reviewDecode.mode = "fail"));
    await readerZoom.fill("125");
    await readerZoom.press("Enter");
    await expect(drawer.getByRole("button", { name: "Retry page", exact: true })).toBeVisible();
    await expect(drawToggle).toBeDisabled();
    await page.evaluate(() => (window.reviewDecode.mode = "pass"));
    await drawer.getByRole("button", { name: "Retry page", exact: true }).click();
    await expect(drawToggle).toBeEnabled();
    await setZoom(fitZoom);
    const canvas = drawer.locator('[data-review-page="1"]');
    const drag = async (x0: number, y0: number, x1: number, y1: number) => {
      const toggle = drawer.getByRole("switch", { name: "Draw rectangle", exact: true });
      await setReviewDrawing(toggle, true);
      const rect = await canvas.boundingBox();
      if (!rect) throw new Error("Canvas absent");
      await page.mouse.move(
        rect.x + (x0 / 85000) * rect.width,
        rect.y + (y0 / 110000) * rect.height,
      );
      await page.mouse.down();
      await page.mouse.move(
        rect.x + (x1 / 85000) * rect.width,
        rect.y + (y1 / 110000) * rect.height,
        { steps: 5 },
      );
      await page.mouse.up();
      await expect(toggle).toBeChecked();
    };
    // Keyboard marks use the same authoritative boxes, and Escape cancels only a draft.
    await setReviewDrawing(drawToggle, true);
    const cursorStatus = drawer.getByRole("status", { name: "Mask cursor", exact: true });
    await drawer.locator("embedpdf-container").focus();
    for (const [index, key] of [
      "Shift+ArrowRight",
      "Shift+ArrowRight",
      "Shift+ArrowDown",
      "Shift+ArrowDown",
      "Enter",
      "Shift+ArrowRight",
      "Shift+ArrowRight",
      "Shift+ArrowDown",
      "Shift+ArrowDown",
      "Enter",
    ].entries()) {
      await page.keyboard.press(key);
      // Finishing a prior scroll cannot cancel a draft on a stationary page.
      if (index === 4) await scrollIdle();
    }
    await expect(drawer.getByRole("button", { name: "Remove", exact: true })).toHaveCount(1);
    await expect(cursorStatus).toHaveText("Page 1, cursor 0.40, 0.40 inches.");
    const cursor = canvas.locator('svg [stroke="#0076d6"]').last();
    await expect
      .poll(async () => {
        const bounds = await cursor.boundingBox();
        return bounds ? Math.min(bounds.width, bounds.height) : 0;
      })
      .toBeGreaterThanOrEqual(12);
    await page.screenshot({
      path: path.join(captures!, "redaction-keyboard.png"),
      animations: "disabled",
    });
    const keyboardBox = canvas.locator('svg rect[fill="#00000040"]');
    await expect(keyboardBox).toHaveCount(1);
    for (const [attribute, value] of Object.entries({
      x: "14.4",
      y: "14.4",
      width: "14.4",
      height: "14.4",
    }))
      await expect
        .poll(async () => Number(await keyboardBox.getAttribute(attribute)))
        .toBeCloseTo(Number(value), 8);
    await drawer.getByRole("button", { name: "Page Settings", exact: true }).click();
    await drawer.getByRole("menuitem", { name: "Rotate Clockwise", exact: true }).click();
    await expect(drawToggle).toBeEnabled();
    await drawer.locator("embedpdf-container").focus();
    await page.keyboard.press("Shift+ArrowRight");
    await expect(cursorStatus).toHaveText("Page 1, cursor 0.40, 0.30 inches.");
    await page.keyboard.press("Shift+ArrowDown");
    await expect(cursorStatus).toHaveText("Page 1, cursor 0.50, 0.30 inches.");
    await page.keyboard.press("Enter");
    await page.keyboard.press("Shift+ArrowRight");
    await page.keyboard.press("Shift+ArrowDown");
    await page.keyboard.press("Enter");
    await expect(keyboardBox).toHaveCount(2);
    for (const [attribute, value] of Object.entries({ x: 36, y: 14.4, width: 7.2, height: 7.2 }))
      await expect
        .poll(async () => Number(await keyboardBox.last().getAttribute(attribute)))
        .toBeCloseTo(value, 8);
    await drawer.getByRole("button", { name: "Remove", exact: true }).last().click();
    await drawer.getByRole("button", { name: "Page Settings", exact: true }).click();
    await drawer.getByRole("menuitem", { name: "Rotate Counter-Clockwise", exact: true }).click();
    await expect(drawToggle).toBeEnabled();
    const previewAction = drawer.getByRole("button", { name: "Preview redaction", exact: true });
    // Touch cancellation has no pointerup. It must abandon only the unfinished
    // stroke so the already committed mask can still be previewed.
    const touch = await page.context().newCDPSession(page);
    const touchBounds = (await canvas.boundingBox())!;
    const touchPoint = {
      x: touchBounds.x + touchBounds.width / 3,
      y: touchBounds.y + touchBounds.height / 3,
    };
    await touch.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [touchPoint] });
    await touch.send("Input.dispatchTouchEvent", {
      type: "touchMove",
      touchPoints: [{ x: touchPoint.x + 20, y: touchPoint.y + 20 }],
    });
    await expect(previewAction).toBeDisabled();
    await touch.send("Input.dispatchTouchEvent", { type: "touchCancel", touchPoints: [] });
    await expect(previewAction).toBeEnabled();
    await expect(drawToggle).toBeChecked();
    await expect(drawer.getByRole("button", { name: "Remove", exact: true })).toHaveCount(1);
    await touch.detach();
    for (const { gesture, failedPage } of [
      { gesture: "keyboard", failedPage: 2 },
      { gesture: "pointer", failedPage: 2 },
      { gesture: "keyboard", failedPage: 1 },
    ]) {
      await drawer.locator("embedpdf-container").focus();
      const bounds = (await canvas.boundingBox())!;
      if (gesture === "keyboard") {
        await page.keyboard.press("Enter");
        await page.keyboard.press("Shift+ArrowRight");
        await page.keyboard.press("Shift+ArrowDown");
      } else {
        await page.mouse.move(bounds.x + bounds.width / 3, bounds.y + bounds.height / 3);
        await page.mouse.down();
        await page.mouse.move(bounds.x + bounds.width / 2, bounds.y + bounds.height / 2);
      }
      await expect(
        previewAction,
        `${gesture} draft before page ${failedPage} fails`,
      ).toBeDisabled();
      // Fail an actual displayed image: only its own page's draft may be canceled.
      const failedLayer = drawer.locator(`[data-review-page="${failedPage}"]`);
      const imageURL = await failedLayer.evaluate(
        (layer, failedPage) =>
          new Promise<string>((resolve) => {
            const host = (layer.getRootNode() as ShadowRoot).host;
            const image = layer.parentElement!.querySelector("img")!;
            const source = image.src;
            const failed = (raw: Event) => {
              const { pageIndex, status } = (raw as CustomEvent).detail;
              if (pageIndex !== failedPage - 1 || status !== "error") return;
              host.removeEventListener("embedpdf:page-render", failed);
              resolve(source);
            };
            host.addEventListener("embedpdf:page-render", failed);
            image.src = "data:image/png;base64,AAAA";
          }),
        failedPage,
      );
      if (failedPage === 1) {
        await expect(drawToggle).toBeDisabled();
        await expect(previewAction).toBeEnabled();
        await expect(drawer.getByRole("button", { name: "Remove", exact: true })).toHaveCount(1);
      } else {
        await expect(drawToggle).toBeEnabled();
        await expect(previewAction).toBeDisabled();
        if (gesture === "keyboard") await page.keyboard.press("Enter");
        else await page.mouse.up();
        await expect(drawer.getByRole("button", { name: "Remove", exact: true })).toHaveCount(2);
        await drawer.getByRole("button", { name: "Remove", exact: true }).last().click();
      }
      await failedLayer.evaluate((layer, source) => {
        layer.parentElement!.querySelector("img")!.src = source;
      }, imageURL);
      await expect
        .poll(() =>
          failedLayer.evaluate((layer) => layer.parentElement!.querySelector("img")!.naturalWidth),
        )
        .toBeGreaterThan(0);
      await expect(drawToggle).toBeEnabled();
    }
    await drawer.locator("embedpdf-container").focus();
    await page.keyboard.press("Enter");
    await page.keyboard.press("Shift+ArrowRight");
    await page.keyboard.press("Shift+ArrowDown");
    await expect(previewAction).toBeDisabled();
    await drawer
      .getByRole("complementary", { name: "Masks and coverage" })
      .getByRole("heading")
      .first()
      .click();
    await expect(previewAction).toBeDisabled();
    await drawer.locator("embedpdf-container").focus();
    await page.keyboard.press("Escape");
    await expect(drawToggle).toBeChecked();
    await expect(previewAction).toBeEnabled();
    await expect(page.getByRole("dialog", { name: "Discard redaction?" })).toBeHidden();
    await page.keyboard.press("Enter");
    await page.keyboard.press("Shift+ArrowRight");
    await page.keyboard.press("Shift+ArrowDown");
    await expect(previewAction).toBeDisabled();
    await drawer.getByRole("button", { name: "Back to reading", exact: true }).click();
    await page
      .getByRole("dialog", { name: "Discard redaction?", exact: true })
      .getByRole("button", { name: "Keep reviewing", exact: true })
      .click();
    await expect(previewAction).toBeEnabled();
    await drawer.getByRole("button", { name: "Remove", exact: true }).click();
    await drawer.getByRole("button", { name: "Toggle Pan Mode", exact: true }).click();
    await expect(drawToggle).not.toBeChecked();
    // Cut a real glyph: the server must propose an expansion, never adopt it silently.
    await drag(10300, 9000, 10700, 9700);
    await expect(drawer.getByRole("button", { name: "Remove", exact: true })).toHaveCount(1);
    await page.keyboard.press("Escape");
    const discard = page.getByRole("dialog", { name: "Discard redaction?", exact: true });
    await expect(discard).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(discard).toBeHidden();
    await expect(drawer.getByRole("button", { name: "Remove", exact: true })).toHaveCount(1);
    await drawer.getByRole("button", { name: "Preview redaction", exact: true }).click();
    await expect(drawer.getByRole("region", { name: "Proposed mask expansion" })).toBeVisible();
    await page.screenshot({
      path: path.join(captures!, "redaction-expansion.png"),
      animations: "disabled",
    });
    await drawer.getByRole("button", { name: "Keep editing", exact: true }).click();
    await drawer.getByRole("button", { name: "Remove", exact: true }).click();
    await setZoom("200");
    await scrollPage(canvas);
    await drag(9000, 7800, 17000, 11000);
    await drag(24000, 12000, 28000, 15000);
    await expect(drawer.getByRole("button", { name: "Remove", exact: true })).toHaveCount(2);
    await drawer.getByRole("button", { name: "Remove", exact: true }).last().click();
    await page.evaluate(() => (window.reviewDecode.mode = "fail"));
    await readerZoom.fill("125");
    await readerZoom.press("Enter");
    await expect(drawer.getByRole("button", { name: "Retry page", exact: true })).toBeVisible();
    await expect(drawToggle).toBeDisabled();
    await page.evaluate(() => (window.reviewDecode.mode = "pass"));
    await drawer.getByRole("button", { name: "Retry page", exact: true }).click();
    await expect(drawToggle).toBeEnabled();
    await expect(drawer.getByRole("button", { name: "Remove", exact: true })).toHaveCount(1);
    await setZoom(fitZoom);
    // Metadata edits keep the immutable original and masks in the same reader.
    await run("tag", "create", "review-pending");
    await run("tag", "assign", "review-pending", "/two-pages.pdf");
    await expect(drawToggle).toBeChecked();
    await drawer.getByRole("button", { name: "Next Page", exact: true }).focus();
    await page.keyboard.press("Enter");
    await awaitPage(2);
    await drawer.locator("embedpdf-container").focus();
    await page.keyboard.press("Shift+ArrowRight");
    await page.keyboard.press("ArrowDown");
    await expect(cursorStatus).toHaveText("Page 2, cursor 0.10, 0.01 inches.");
    await drawer.getByRole("button", { name: "Mask whole page 2", exact: true }).click();
    await expect(drawer.getByText("Whole page 2", { exact: true })).toBeVisible();
    await originalPage(1);
    expect(originalLoads).toBe(1);
    expect(page.workers()).toEqual(readerWorkers);
    await expect(async () => {
      const sheet = await canvas.boundingBox();
      const reader = await drawer.locator("embedpdf-container").boundingBox();
      expect(sheet!.y).toBeGreaterThanOrEqual(reader!.y);
      expect(sheet!.y + sheet!.height).toBeLessThanOrEqual(reader!.y + reader!.height);
    }).toPass({ timeout: 10000 });
    await page.screenshot({
      path: path.join(captures!, "redaction-masks.png"),
      animations: "disabled",
    });
    // Publish on the real daemon, then lose or cancel the reply before the
    // browser learns its handle. Retry must wait for owner-scoped cleanup.
    for (const outcome of ["lost", "canceled"]) {
      let releaseResponse!: () => void, releaseCleanup!: () => void;
      const responseGate = new Promise<void>((resolve) => (releaseResponse = resolve));
      const cleanupGate = new Promise<void>((resolve) => (releaseCleanup = resolve));
      let createdStatus = 0,
        cleanupRequests = 0;
      const intercept = async (route: Route) => {
        if (route.request().method() === "POST") {
          const response = await route.fetch();
          createdStatus = response.status();
          await responseGate;
          await route.abort("failed");
        } else if (route.request().method() === "DELETE") {
          await cleanupGate;
          if (outcome === "lost" && cleanupRequests++ === 0)
            await route.fulfill({
              status: 503,
              contentType: "application/problem+json",
              body: JSON.stringify({ detail: "Preview cleanup interrupted." }),
            });
          else await route.continue();
        } else await route.continue();
      };
      await page.route("**/api/v1/redaction/previews", intercept);
      try {
        await drawer.getByRole("button", { name: "Preview redaction", exact: true }).click();
        await expect.poll(() => createdStatus, { timeout: 60000 }).toBe(201);
        if (outcome === "canceled")
          await drawer.getByRole("button", { name: "Cancel preview", exact: true }).click();
        releaseResponse();
        await expect(drawer.getByText("Releasing preview…", { exact: true })).toBeVisible();
        await expect(
          drawer.getByRole("button", { name: "Release preview", exact: true }),
        ).toBeDisabled();
        await expect(
          drawer.getByRole("button", { name: "Preview redaction", exact: true }),
        ).toHaveCount(0);
        await expect(drawer.getByRole("button", { name: "Remove", exact: true })).toHaveCount(2);
        releaseCleanup();
        if (outcome === "lost") {
          await expect(
            drawer.getByRole("button", { name: "Release preview", exact: true }),
          ).toBeEnabled();
          await expect(
            drawer.getByRole("button", { name: "Preview redaction", exact: true }),
          ).toHaveCount(0);
          await drawer.getByRole("button", { name: "Release preview", exact: true }).click();
        }
        await expect(
          drawer.getByRole("button", { name: "Preview redaction", exact: true }),
        ).toBeEnabled();
      } finally {
        releaseResponse();
        releaseCleanup();
        await page.unroute("**/api/v1/redaction/previews", intercept);
      }
    }
    const createPreview = async (): Promise<RedactionPreview> => {
      // Capture the real reply before forwarding it; Chromium can discard its response body.
      const url = "**/api/v1/redaction/previews";
      let body: Buffer | undefined;
      const capture = async (route: Route) => {
        if (route.request().method() !== "POST") return route.continue();
        const response = await route.fetch();
        try {
          body = await response.body();
          await route.fulfill({ response, body });
        } finally {
          await response.dispose();
        }
      };
      await page.route(url, capture);
      try {
        const created = page.waitForResponse(
          (r) =>
            new URL(r.url()).pathname === "/api/v1/redaction/previews" &&
            r.request().method() === "POST",
        );
        await drawer.getByRole("button", { name: "Preview redaction", exact: true }).click();
        expect((await created).status()).toBe(201);
        expect(body).toBeDefined();
        return JSON.parse(body!.toString());
      } finally {
        await page.unroute(url, capture);
      }
    };
    // The daemon owns expiry; a fast browser clock must not forget a live preview.
    await page.clock.setFixedTime(new Date(Date.now() + 60 * 60 * 1000));
    let preview = await createPreview();
    await drawer.getByRole("combobox", { name: /^Page:/ }).click();
    await drawer.getByRole("option", { name: "1 of 2", exact: true }).click();
    await expect(
      drawer.getByRole("img", { name: "Verified redacted page 1 of two-pages.pdf" }),
    ).toBeVisible({ timeout: 60000 });
    await expectFitPage();
    await page.screenshot({
      path: path.join(captures!, "redaction-ready.png"),
      animations: "disabled",
    });
    await drawer.getByRole("button", { name: "Text", exact: true }).click();
    await expect(drawer.getByLabel("Sanitized text for page 1")).toHaveText(
      "[REDACTED] KEEP SECRET",
    );
    await drawer.getByRole("button", { name: "Next page", exact: true }).click();
    await expect(drawer.getByLabel("Sanitized text for page 2")).toHaveText("[REDACTED]");
    // Retire the real server handle while the editor is showing its output.
    expect(
      await page.evaluate(
        async ({ session, id }) =>
          (
            await fetch(`/api/v1/redaction/previews/${id}`, {
              method: "DELETE",
              headers: { "X-Docbank-Web-Session": session },
            })
          ).status,
        { session, id: preview.id },
      ),
    ).toBe(204);
    await drawer.getByRole("button", { name: "Redacted", exact: true }).click();
    await awaitPage(2);
    await expect(drawer.locator('[data-review-page="2"]')).toBeVisible();
    await expect(drawer.getByRole("button", { name: "Remove", exact: true })).toHaveCount(2);
    preview = await createPreview();
    await drawer.getByRole("combobox", { name: /^Page:/ }).click();
    await drawer.getByRole("option", { name: "2 of 2", exact: true }).click();
    await expect(
      drawer.getByRole("img", { name: "Verified redacted page 2 of two-pages.pdf" }),
    ).toBeVisible();
    await drawer.getByRole("button", { name: "Text", exact: true }).click();
    await expect(drawer.getByLabel("Sanitized text for page 2")).toHaveText("[REDACTED]");
    // The ready result remains bound to its captured source after replacement.
    const replacement = path.join(scratch, "replacement.pdf");
    await writeFile(replacement, croppedReviewPDF());
    await run("put", replacement, "/two-pages.pdf");
    await drawer.getByText("Private review files", { exact: true }).click();
    const privateFiles: string[] = [];
    for (const kind of ["preparation", "masks", "review"]) {
      const event = page.waitForEvent("download");
      await drawer.getByRole("button", { name: `Save ${kind}`, exact: true }).click();
      const download = await event;
      expect(download.suggestedFilename()).toBe(`two-pages-PRIVATE-${kind}.json`);
      const file = path.join(scratch, download.suggestedFilename());
      await download.saveAs(file);
      privateFiles.push(file);
    }
    await drawer.getByRole("button", { name: "Download unnumbered ZIP", exact: true }).click();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog", { name: "Download reviewed PDF?" })).toBeHidden();
    await expect(drawer).toBeVisible();
    const bundles: Buffer[] = [];
    for (let i = 0; i < 2; i++) {
      await drawer.getByRole("button", { name: "Download unnumbered ZIP", exact: true }).click();
      const event = page.waitForEvent("download");
      await page.getByRole("button", { name: "Download ZIP", exact: true }).click();
      const download = await event,
        file = path.join(scratch, `delivery-${i}.zip`);
      await download.saveAs(file);
      const data = await readFile(file);
      expect(data.length).toBe(preview.archive.size);
      expect(createHash("sha256").update(data).digest("hex")).toBe(preview.archive.sha256);
      bundles.push(data);
      await run("redaction", "verify", ...privateFiles, file, "--json");
    }
    expect(bundles[0]!.equals(bundles[1]!)).toBe(true);
    await run("put", path.join(scratch, "two-pages.pdf"), "/two-pages.pdf");
    await page.setViewportSize({ width: 640, height: 400 });
    const saveReview = drawer.getByRole("button", { name: "Save review", exact: true });
    const footer = drawer.locator(".review-footer");
    await expect(
      drawer.getByRole("button", { name: "Download unnumbered ZIP", exact: true }),
    ).toBeInViewport();
    await drawer.getByRole("button", { name: "Download unnumbered ZIP", exact: true }).click();
    await page.keyboard.press("Escape");
    await footer.hover();
    await page.mouse.wheel(0, 500);
    const editor = drawer.locator(".redaction-editor");
    await expect(editor).toBeInViewport({ ratio: 1 });
    const editorBounds = (await editor.boundingBox())!;
    await page.mouse.move(editorBounds.x + editorBounds.width / 2, editorBounds.y + 20);
    await page.mouse.wheel(0, 500);
    await expect(saveReview).toBeInViewport({ ratio: 1 });
    const saveBounds = (await saveReview.boundingBox())!;
    const narrowReview = page.waitForEvent("download");
    await page.mouse.click(
      saveBounds.x + saveBounds.width / 2,
      saveBounds.y + saveBounds.height / 2,
    );
    await narrowReview;
    await expect(
      drawer.getByRole("button", { name: "Download unnumbered ZIP", exact: true }),
    ).toBeInViewport();
    await page.screenshot({
      path: path.join(captures!, "redaction-narrow.png"),
      animations: "disabled",
    });
    await page.setViewportSize({ width: 1440, height: 960 });
    // Reload retires only the preview. The same session can keep working.
    await page.reload({ waitUntil: "domcontentloaded" });
    await open("two-pages.pdf", true);
    await originalPage(1);
    await drawer.getByRole("button", { name: "Mask whole page 1", exact: true }).click();
    const startedRender = page.waitForRequest(
      (r) => new URL(r.url()).pathname === "/api/v1/redaction/previews" && r.method() === "POST",
    );
    await drawer.getByRole("button", { name: "Preview redaction", exact: true }).click();
    await startedRender;
    await page.reload({ waitUntil: "domcontentloaded" });
    await open("two-pages.pdf", true);
    await originalPage(1);
    await drawer.getByRole("button", { name: "Mask whole page 1", exact: true }).click();
    const last = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === "/api/v1/redaction/previews" &&
        r.request().method() === "POST",
    );
    await drawer.getByRole("button", { name: "Preview redaction", exact: true }).click();
    expect((await last).status()).toBe(201);
    await expect(drawer.getByRole("button", { name: "Back to masks", exact: true })).toBeVisible();
    await drawer.getByRole("button", { name: "Back to masks", exact: true }).click();
    await drawer.getByRole("button", { name: "Close", exact: true }).click();
    await page.getByRole("button", { name: "Discard and close" }).click();
    await writeFile(path.join(scratch, "cropped.pdf"), croppedReviewPDF());
    await run("add", path.join(scratch, "cropped.pdf"), "--dest", "/", "--json");
    await open("cropped.pdf", false, false, async () => {
      // The inspector holds the earlier revision; preparation revalidates it.
      await run("tag", "assign", "review-pending", "/cropped.pdf");
    });
    const cropped = page.getByRole("dialog", { name: "Redact cropped.pdf", exact: true });
    await originalPage(1, cropped);
    await cropped.getByRole("button", { name: "Back to reading", exact: true }).click();
    const croppedReader = page.getByRole("dialog", { name: "Read cropped.pdf", exact: true });
    const originalDownload = page.waitForEvent("download");
    await croppedReader.getByRole("button", { name: "Download", exact: true }).click();
    const originalFile = path.join(scratch, "downloaded-cropped.pdf");
    await (await originalDownload).saveAs(originalFile);
    expect(await readFile(originalFile)).toEqual(croppedReviewPDF());
    await croppedReader.getByRole("button", { name: "Redact PDF", exact: true }).click();
    const expectedBoxes = [
      [3000, 3500, 10500, 6500],
      [3500, 3000, 6500, 10500],
      [9500, 3500, 17000, 6500],
      [3500, 9500, 6500, 17000],
    ];
    for (let i = 0; i < 4; i++) {
      await originalPage(i + 1, cropped);
      await setZoom(i % 2 ? "200" : "100", cropped);
      if (i === 3) {
        await cropped.getByRole("button", { name: "Page Settings", exact: true }).click();
        await cropped.getByRole("menuitem", { name: "Rotate Clockwise", exact: true }).click();
      }
      if (i === 2) await page.setViewportSize({ width: 1280, height: 960 });
      const toggle = cropped.getByRole("switch", { name: "Draw rectangle", exact: true });
      await setReviewDrawing(toggle, true);
      const c = cropped.locator(`[data-review-page="${i + 1}"]`);
      await c.scrollIntoViewIfNeeded();
      await expect(async () => {
        const bounds = await c.boundingBox();
        expect(bounds!.width / bounds!.height).toBeCloseTo(i % 2 && i !== 3 ? 0.5 : 2, 5);
      }).toPass({ timeout: 10000 });
      const rect = await c.boundingBox();
      if (!rect) throw new Error("Crop canvas absent");
      const [w, h] = i % 2 ? [10000, 20000] : [20000, 10000];
      const b = expectedBoxes[i]!;
      let [x0, y0, x1, y1] = i % 2 ? [b[2]!, b[3]!, b[0]!, b[1]!] : b;
      let screenW = w!,
        screenH = h!;
      if (i === 3) {
        [x0, y0, x1, y1] = [h! - y0!, x0!, h! - y1!, x1!];
        [screenW, screenH] = [h!, w!];
      }
      await page.mouse.move(
        rect.x + (x0! / screenW) * rect.width,
        rect.y + (y0! / screenH) * rect.height,
      );
      await page.mouse.down();
      await page.mouse.move(
        rect.x + (x1! / screenW) * rect.width,
        rect.y + (y1! / screenH) * rect.height,
        {
          steps: 5,
        },
      );
      await page.mouse.up();
      await expect(cropped.getByRole("button", { name: "Remove", exact: true })).toHaveCount(i + 1);
    }
    await originalPage(4, cropped);
    const cropResolve = page.waitForRequest(
      (r) => new URL(r.url()).pathname === "/api/v1/redaction/resolve" && r.method() === "POST",
    );
    const cropCreate = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === "/api/v1/redaction/previews" &&
        r.request().method() === "POST",
    );
    await cropped.getByRole("button", { name: "Preview redaction", exact: true }).click();
    const cropRequest = (await cropResolve).postDataJSON();
    cropRequest.masks.forEach(
      (
        mask: { selector: { boxes: Array<{ x0: number; y0: number; x1: number; y1: number }> } },
        i: number,
      ) => {
        const b = mask.selector.boxes[0]!;
        // Mouse positions are quantized to CSS pixels; a 0.01-inch tolerance is below 3 output pixels.
        [b.x0, b.y0, b.x1, b.y1].forEach((v, j) =>
          expect(Math.abs(v - expectedBoxes[i]![j]!)).toBeLessThan(100),
        );
      },
    );
    expect((await cropCreate).status()).toBe(201);
    await cropped.getByRole("combobox", { name: /^Page:/ }).click();
    await cropped.getByRole("option", { name: "4 of 4", exact: true }).click();
    await expect(
      cropped.getByRole("img", { name: "Verified redacted page 4 of cropped.pdf" }),
    ).toBeVisible();
    await page.screenshot({
      path: path.join(captures!, "redaction-cropped.png"),
      animations: "disabled",
    });
    await cropped.getByText("Private review files", { exact: true }).click();
    const cropInputs: string[] = [];
    for (const kind of ["preparation", "masks", "review"]) {
      const event = page.waitForEvent("download");
      await cropped.getByRole("button", { name: `Save ${kind}`, exact: true }).click();
      const d = await event,
        file = path.join(scratch, d.suggestedFilename());
      await d.saveAs(file);
      cropInputs.push(file);
    }
    await cropped.getByRole("button", { name: "Download unnumbered ZIP", exact: true }).click();
    const cropDownload = page.waitForEvent("download");
    await page.getByRole("button", { name: "Download ZIP", exact: true }).click();
    const cropFile = path.join(scratch, "cropped.zip");
    await (await cropDownload).saveAs(cropFile);
    await run("redaction", "verify", ...cropInputs, cropFile, "--json");
    await exec("python3", [
      "-I",
      "-c",
      "import sys,zipfile; z=zipfile.ZipFile(sys.argv[1]); t=z.read('redacted.txt'); assert b'SECRET' not in t; assert t.count(b'KEEP') == 4; assert t.count(b'[REDACTED]') == 4",
      cropFile,
    ]);
    const checkReaderBoxes = async (target: Locator, fixture: (typeof geometry)[number]) => {
      const observed = await target.locator("embedpdf-container").evaluate(async (host) => {
        const registry = await (host as HTMLElement & { registry: Promise<PluginRegistry> })
          .registry;
        return registry
          .getPlugin<DocumentManagerPlugin>("document-manager")!
          .provides()
          .getActiveDocument()!
          .pages.map((p) => {
            const { media, crop } = p.boxes!;
            return {
              media: [media.left, media.bottom, media.right, media.top],
              crop: [crop.left, crop.bottom, crop.right, crop.top],
            };
          });
      });
      // Same literals are observed through the server PDFium APIs in TestNativeSourceGeometry.
      expect(observed).toEqual(
        Array(4).fill({ media: fixture.observedMedia, crop: fixture.observedCrop }),
      );
      await writeFile(
        path.join(captures!, `${fixture.name}-browser-boxes.json`),
        JSON.stringify(observed),
      );
    };
    await writeFile(path.join(scratch, "quantized.pdf"), croppedReviewPDF("quantized"));
    await run("add", path.join(scratch, "quantized.pdf"), "--dest", "/", "--json");
    await open("quantized.pdf");
    const quantized = page.getByRole("dialog", { name: "Redact quantized.pdf", exact: true });
    await originalPage(1, quantized);
    await checkReaderBoxes(quantized, geometry[0]!);
    await quantized.getByRole("button", { name: "Close", exact: true }).click();
    await expect(quantized).toBeHidden();
    // A four-decimal crop over 1,024 points exercises PDFium's float32 boxes
    // against the server's retained integer geometry, on every intrinsic rotation.
    await writeFile(path.join(scratch, "fractional.pdf"), croppedReviewPDF("fractional"));
    await run("add", path.join(scratch, "fractional.pdf"), "--dest", "/", "--json");
    await open("fractional.pdf");
    const fractional = page.getByRole("dialog", { name: "Redact fractional.pdf", exact: true });
    await originalPage(1, fractional);
    await checkReaderBoxes(fractional, geometry[1]!);
    await fractional.screenshot({
      path: path.join(captures!, "redaction-fractional-crop.png"),
      animations: "disabled",
    });
    await setZoom("200", fractional);
    const fractionalLayer = fractional.locator('[data-review-page="1"]');
    const beforeScroll = await scrollPage(fractionalLayer, 200, 200, fractional);
    await expect
      .poll(async () => (await fractionalLayer.boundingBox())!.y)
      .toBeLessThan(beforeScroll - 100);
    await setReviewDrawing(
      fractional.getByRole("switch", { name: "Draw rectangle", exact: true }),
      true,
    );
    await expect
      .poll(() =>
        fractionalLayer.evaluate((layer) => getComputedStyle(layer.parentElement!).cursor),
      )
      .toBe("crosshair");
    const fractionalBounds = (await fractionalLayer.boundingBox())!;
    // Two to three inches from the crop's top-left, after zooming and scrolling.
    await page.mouse.move(
      fractionalBounds.x + fractionalBounds.width / 8,
      fractionalBounds.y + (fractionalBounds.height * 2) / 11,
    );
    await page.mouse.down();
    await page.mouse.move(
      fractionalBounds.x + (fractionalBounds.width * 3) / 16,
      fractionalBounds.y + (fractionalBounds.height * 3) / 11,
      { steps: 5 },
    );
    await page.mouse.up();
    await expect(fractional.getByRole("button", { name: "Remove", exact: true })).toHaveCount(1);
    for (let n = 1; n <= 4; n++) {
      await originalPage(n, fractional);
      if (n === 4) {
        await expect(
          fractional.getByRole("button", { name: "Page 4 · whole page", exact: true }),
        ).toBeVisible();
        const gap = fractional
          .locator('[data-review-page="4"]')
          .locator("svg rect[stroke-dasharray]");
        await expect(gap).toHaveAttribute("width", "1152");
        await expect(gap).toHaveAttribute("height", "792");
      }
      await fractional.getByRole("button", { name: `Mask whole page ${n}`, exact: true }).click();
      await expect(fractional.getByRole("button", { name: "Remove", exact: true })).toHaveCount(
        n + 1,
      );
    }
    // Check the submitted physical box without rendering the large four-page fixture.
    await page.route("**/api/v1/redaction/resolve", (route) => route.abort());
    const fractionalResolve = page.waitForRequest(
      (r) => new URL(r.url()).pathname === "/api/v1/redaction/resolve" && r.method() === "POST",
    );
    await fractional.getByRole("button", { name: "Preview redaction", exact: true }).click();
    const submittedBox = (await fractionalResolve).postDataJSON().masks[0].selector.boxes[0];
    for (const [key, value] of Object.entries({ x0: 20000, y0: 20000, x1: 30000, y1: 30000 }))
      expect(Math.abs(submittedBox[key] - value)).toBeLessThan(100);
    await expect(
      fractional.getByRole("button", { name: "Preview redaction", exact: true }),
    ).toBeEnabled();
    await page.unroute("**/api/v1/redaction/resolve");
    // Closing while a decoded image is delayed must not revive the old review.
    await page.evaluate(() => (window.reviewDecode.mode = "delay"));
    const fractionalZoom = fractional.getByRole("textbox", { name: "Set zoom" });
    await fractionalZoom.fill("100");
    await fractionalZoom.press("Enter");
    await expect
      .poll(() => page.evaluate(() => window.reviewDecode.pending.length))
      .toBeGreaterThan(0);
    await fractional.getByRole("button", { name: "Close", exact: true }).click();
    await page.getByRole("button", { name: "Discard and close", exact: true }).click();
    await expect(fractional).toBeHidden();
    await page.evaluate(() => {
      window.reviewDecode.mode = "pass";
      window.reviewDecode.pending.splice(0).forEach((f) => f());
    });
    await expect.poll(() => page.workers().length).toBe(0);
    await page.setViewportSize({ width: 1440, height: 960 });
    await open("two-pages.pdf", false, true);
    await drawer.getByRole("button", { name: "Back to reading", exact: true }).click();
    const readingZoom = reader.getByRole("textbox", { name: "Set zoom" });
    await expect(readingZoom).toHaveValue("125");
    await reader.getByRole("button", { name: "Zoom Menu", exact: true }).click();
    await reader.getByRole("menuitem", { name: "Fit to Width", exact: true }).click();
    await expect(readingZoom).not.toHaveValue("125");
    const widthZoom = Number(await readingZoom.inputValue());
    await reader.getByRole("button", { name: "Redact PDF", exact: true }).click();
    await expect(drawer.getByRole("switch", { name: "Draw rectangle", exact: true })).toBeEnabled();
    await drawer.getByRole("button", { name: "Back to reading", exact: true }).click();
    await page.setViewportSize({ width: 1280, height: 960 });
    await expect
      .poll(async () =>
        Number(await reader.getByRole("textbox", { name: "Set zoom" }).inputValue()),
      )
      .toBeLessThan(widthZoom);
    await page.setViewportSize({ width: 1440, height: 960 });
    await expect(reader.getByRole("textbox", { name: "Set zoom" })).toHaveValue(String(widthZoom));
    await reader.getByRole("button", { name: "Close", exact: true }).click();
    await expect.poll(() => page.workers().length).toBe(0);
    expect(errors).toEqual([]);
  } catch (cause) {
    failure = cause;
    await page
      .screenshot({ path: path.join(captures!, "failure.png"), animations: "disabled" })
      .catch(() => undefined);
  }
  try {
    await stop();
  } catch (cause) {
    failure = failure ? new AggregateError([failure, cause], "Review and shutdown failed") : cause;
  }
  if (failure) throw failure;
  await rm(scratch, { recursive: true, force: true });
});
