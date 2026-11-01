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
test.use({ reducedMotion: "reduce" });

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
    const open = async (name = "two-pages.pdf", reuseSession = false, fromReader = false) => {
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
        await reader.getByRole("button", { name: "Next Page", exact: true }).click();
        await expect(reader.getByRole("textbox", { name: "", exact: true })).toHaveValue("2");
        await reader.getByRole("button", { name: "Redact PDF", exact: true }).click();
      } else {
        await file.click();
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
            throw new Error("Injected displayed-image decode failure");
          });
        if (control.mode === "delay")
          return result.then(() => new Promise<void>((resolve) => control.pending.push(resolve)));
        return result;
      };
    });
    const session = await open("two-pages.pdf", false, true);
    const drawer = page.getByRole("dialog", { name: "Redact two-pages.pdf", exact: true });
    const originalPage = async (n: number, target: Locator = drawer) => {
      const field = target.getByRole("textbox", { name: "", exact: true });
      if ((await field.inputValue()) !== String(n)) {
        await field.fill(String(n));
        await field.press("Enter");
      }
      await expect(target.getByRole("switch", { name: "Draw rectangle", exact: true })).toBeEnabled(
        { timeout: 60000 },
      );
      await expect(field).toHaveValue(String(n));
    };
    await originalPage(2);
    expect(originalLoads).toBe(1);
    const readerWorkers = page.workers();
    // Returning from review keeps the live reader, including its page and zoom.
    await drawer.getByRole("button", { name: "Back to reading", exact: true }).click();
    await expect(drawer).toBeHidden();
    const reader = page.getByRole("dialog", { name: "Read two-pages.pdf", exact: true });
    await expect(reader.getByRole("textbox", { name: "Set zoom" })).toHaveValue("125");
    await expect(reader.getByRole("textbox", { name: "", exact: true })).toHaveValue("2");
    await expect(reader.getByRole("button", { name: "Redact PDF", exact: true })).toBeFocused();
    await reader.getByRole("button", { name: "Redact PDF", exact: true }).click();
    await originalPage(1);
    expect(originalLoads).toBe(1);
    expect(page.workers()).toEqual(readerWorkers);
    await drawer.getByRole("button", { name: "Document Menu", exact: true }).click();
    for (const name of ["Print", "Download", "Export"])
      await expect(drawer.getByRole("menuitem", { name, exact: true })).toBeHidden();
    await drawer.getByRole("button", { name: "Document Menu", exact: true }).click();
    await drawer.locator("embedpdf-container").focus();
    await page.keyboard.press("ControlOrMeta+p");
    await expect(drawer.getByRole("heading", { name: "Print Settings" })).toBeHidden();
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
          const bounds = await drawer
            .getByRole("group", { name: "Page 1 review layer", exact: true })
            .boundingBox();
          const viewer = await drawer.locator("embedpdf-container").boundingBox();
          expect(bounds!.height).toBeGreaterThan(100);
          expect(bounds!.y).toBeGreaterThanOrEqual(viewer!.y);
          expect(bounds!.y + bounds!.height).toBeLessThanOrEqual(viewer!.y + viewer!.height);
        }
      }).toPass({ timeout: 10000 });
    };
    await expectFitPage();
    const fitZoom = await drawer.getByRole("textbox", { name: "Set zoom" }).inputValue();
    const setZoom = async (value: string, target: Locator = drawer) => {
      const zoom = target.getByRole("textbox", { name: "Set zoom" });
      await zoom.fill(value);
      await zoom.press("Enter");
      await expect(
        target.getByRole("switch", { name: "Draw rectangle", exact: true }),
      ).toBeEnabled();
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
    const canvas = drawer.getByRole("group", { name: "Page 1 review layer", exact: true });
    const drag = async (x0: number, y0: number, x1: number, y1: number) => {
      const toggle = drawer.getByRole("switch", { name: "Draw rectangle", exact: true });
      if (!(await toggle.isChecked())) await toggle.click();
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
    await drawToggle.click();
    await drawer.locator("embedpdf-container").focus();
    for (const key of [
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
    ])
      await page.keyboard.press(key);
    await expect(drawer.getByRole("button", { name: "Remove", exact: true })).toHaveCount(1);
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
    await page.keyboard.press("Enter");
    await page.keyboard.press("Shift+ArrowRight");
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog", { name: "Discard redaction?" })).toBeHidden();
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
    await canvas.evaluate((element) => element.scrollIntoView({ block: "start", inline: "start" }));
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
    await originalPage(2);
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
    await originalPage(2);
    await expect(
      drawer.getByRole("group", { name: "Page 2 review layer", exact: true }),
    ).toBeVisible();
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
    await drawer.getByRole("button", { name: "Confirm and download ZIP", exact: true }).click();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog", { name: "Download reviewed PDF?" })).toBeHidden();
    await expect(drawer).toBeVisible();
    const bundles: Buffer[] = [];
    for (let i = 0; i < 2; i++) {
      await drawer.getByRole("button", { name: "Confirm and download ZIP", exact: true }).click();
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
    await page.setViewportSize({ width: 640, height: 900 });
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
    await open("cropped.pdf");
    const cropped = page.getByRole("dialog", { name: "Redact cropped.pdf", exact: true });
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
      if (!(await toggle.isChecked())) await toggle.click();
      const c = cropped.getByRole("group", { name: `Page ${i + 1} review layer`, exact: true });
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
    await cropped.getByRole("button", { name: "Confirm and download ZIP", exact: true }).click();
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
    // A four-decimal crop over 1,024 points exercises PDFium's float32 boxes
    // against the server's retained integer geometry, on every intrinsic rotation.
    await writeFile(path.join(scratch, "fractional.pdf"), croppedReviewPDF(true));
    await run("add", path.join(scratch, "fractional.pdf"), "--dest", "/", "--json");
    await open("fractional.pdf");
    const fractional = page.getByRole("dialog", { name: "Redact fractional.pdf", exact: true });
    for (let n = 1; n <= 4; n++) {
      await originalPage(n, fractional);
      if (n === 4) {
        await expect(
          fractional.getByRole("button", { name: "Page 4 · whole page", exact: true }),
        ).toBeVisible();
        const gap = fractional
          .getByRole("group", { name: "Page 4 review layer", exact: true })
          .locator("svg rect[stroke-dasharray]");
        await expect(gap).toHaveAttribute("width", "1152");
        await expect(gap).toHaveAttribute("height", "792");
      }
      await fractional.getByRole("button", { name: `Mask whole page ${n}`, exact: true }).click();
      await expect(fractional.getByRole("button", { name: "Remove", exact: true })).toHaveCount(n);
    }
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
