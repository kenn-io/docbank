import { croppedReviewPDF } from "./redaction-review-fixture.js";
import { expect, test, type Route } from "@playwright/test";
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
    });
    const session = await open("two-pages.pdf", false, true);
    const drawer = page.getByRole("dialog", { name: "Redact two-pages.pdf", exact: true });
    const originalPage = async (n: number) => {
      const render = drawer.getByRole("button", { name: `Render page ${n}`, exact: true });
      await expect(async () => {
        if (await render.isVisible()) await render.click();
        await expect(
          drawer.getByRole("switch", { name: "Draw rectangle", exact: true }),
        ).toBeEnabled();
      }).toPass({ timeout: 60000 });
    };
    await originalPage(1);
    // Returning from review keeps the live reader, including its page and zoom.
    await page.keyboard.press("Escape");
    await expect(drawer).toBeHidden();
    const reader = page.getByRole("dialog", { name: "Read two-pages.pdf", exact: true });
    await expect(reader.getByRole("textbox", { name: "Set zoom" })).toHaveValue("125");
    await expect(reader.getByRole("textbox", { name: "", exact: true })).toHaveValue("2");
    await expect(reader.getByRole("button", { name: "Redact PDF", exact: true })).toBeFocused();
    await reader.getByRole("button", { name: "Redact PDF", exact: true }).click();
    await originalPage(1);
    const expectFitPage = async () => {
      await expect(
        drawer.getByRole("combobox", { name: "Zoom: Fit page", exact: true }),
      ).toBeVisible();
      await expect(async () => {
        const geometry = await drawer
          .getByRole("region", { name: "Scrollable page image" })
          .evaluate((element) => ({
            width: element.clientWidth,
            height: element.clientHeight,
            scrollWidth: element.scrollWidth,
            scrollHeight: element.scrollHeight,
            imageHeight: element.querySelector("img")!.getBoundingClientRect().height,
          }));
        expect(geometry.imageHeight).toBeGreaterThan(100);
        expect(geometry.scrollHeight).toBeLessThanOrEqual(geometry.height + 1);
        expect(geometry.scrollWidth).toBeLessThanOrEqual(geometry.width + 1);
      }).toPass();
    };
    await expectFitPage();
    const canvas = drawer.getByRole("application", { name: "Mask canvas" });
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
    // Cut a real glyph: the server must propose an expansion, never adopt it silently.
    await drag(10300, 9000, 10700, 9700);
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
    await drawer.getByRole("combobox", { name: /^Zoom:/ }).click();
    await drawer.getByRole("option", { name: "200%", exact: true }).click();
    await drawer.getByRole("region", { name: "Scrollable page image" }).evaluate((element) => {
      element.scrollTop = 70;
      element.scrollLeft = 70;
    });
    await drag(9000, 7800, 17000, 11000);
    await drag(24000, 12000, 28000, 15000);
    await expect(drawer.getByRole("button", { name: "Remove", exact: true })).toHaveCount(2);
    await drawer.getByRole("button", { name: "Remove", exact: true }).last().click();
    await drawer.getByRole("combobox", { name: /^Zoom:/ }).click();
    await drawer.getByRole("option", { name: "Fit page", exact: true }).click();
    // Metadata edits keep the source bytes and masks, but advance page admission's revision.
    await run("tag", "create", "review-pending");
    await run("tag", "assign", "review-pending", "/two-pages.pdf");
    const inventory = page.waitForResponse(
      (r) => new URL(r.url()).pathname === "/api/v1/pages/inventory",
    );
    await drawer.getByRole("button", { name: "Next page", exact: true }).click();
    expect((await inventory).status()).toBe(200);
    await originalPage(2);
    await drawer.getByRole("button", { name: "Mask whole page", exact: true }).click();
    await drawer.getByRole("button", { name: "Previous page", exact: true }).click();
    await originalPage(1);
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
    await expect(
      drawer.getByRole("img", { name: "Original page 2 of two-pages.pdf" }),
    ).toBeVisible();
    await expect(drawer.getByRole("switch", { name: "Draw rectangle", exact: true })).toBeEnabled();
    expect(
      await drawer
        .getByRole("img", { name: "Original page 2 of two-pages.pdf" })
        .evaluate((img: HTMLImageElement) => img.complete && img.naturalWidth > 0),
    ).toBe(true);
    await expect(drawer.getByRole("button", { name: "Remove", exact: true })).toHaveCount(2);
    preview = await createPreview();
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
    await drawer.getByRole("button", { name: "Mask whole page", exact: true }).click();
    const startedRender = page.waitForRequest(
      (r) => new URL(r.url()).pathname === "/api/v1/redaction/previews" && r.method() === "POST",
    );
    await drawer.getByRole("button", { name: "Preview redaction", exact: true }).click();
    await startedRender;
    await page.reload({ waitUntil: "domcontentloaded" });
    await open("two-pages.pdf", true);
    await originalPage(1);
    await drawer.getByRole("button", { name: "Mask whole page", exact: true }).click();
    const last = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === "/api/v1/redaction/previews" &&
        r.request().method() === "POST",
    );
    await drawer.getByRole("button", { name: "Preview redaction", exact: true }).click();
    expect((await last).status()).toBe(201);
    await expect(drawer.getByRole("button", { name: "Back to masks", exact: true })).toBeVisible();
    await drawer.getByRole("button", { name: "Back to masks", exact: true }).click();
    await drawer.getByRole("button", { name: "Close redaction" }).click();
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
      if (i) await cropped.getByRole("button", { name: "Next page", exact: true }).click();
      const render = cropped.getByRole("button", { name: `Render page ${i + 1}`, exact: true });
      await expect(render).toBeVisible();
      await render.click();
      await expect(
        cropped.getByRole("switch", { name: "Draw rectangle", exact: true }),
      ).toBeEnabled({ timeout: 60000 });
      await cropped.getByRole("combobox", { name: /^Zoom:/ }).click();
      await cropped.getByRole("option", { name: i % 2 ? "200%" : "100%", exact: true }).click();
      await cropped.getByRole("switch", { name: "Draw rectangle", exact: true }).click();
      const c = cropped.getByRole("application", { name: "Mask canvas" });
      const rect = await c.boundingBox();
      if (!rect) throw new Error("Crop canvas absent");
      const [w, h] = i % 2 ? [10000, 20000] : [20000, 10000];
      const b = expectedBoxes[i]!;
      const [x0, y0, x1, y1] = i % 2 ? [b[2]!, b[3]!, b[0]!, b[1]!] : b;
      await page.mouse.move(rect.x + (x0! / w!) * rect.width, rect.y + (y0! / h!) * rect.height);
      await page.mouse.down();
      await page.mouse.move(rect.x + (x1! / w!) * rect.width, rect.y + (y1! / h!) * rect.height, {
        steps: 5,
      });
      await page.mouse.up();
    }
    const cropCreate = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === "/api/v1/redaction/previews" &&
        r.request().method() === "POST",
    );
    await cropped.getByRole("button", { name: "Preview redaction", exact: true }).click();
    const cropResponse = await cropCreate;
    expect(cropResponse.status()).toBe(201);
    const cropRequest = cropResponse.request().postDataJSON();
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
