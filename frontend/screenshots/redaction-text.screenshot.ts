import { test, expect, type Locator } from "@playwright/test";
import type { RedactionMask, RedactionPreparation } from "../src/generated/docbank.js";
import type {
  PluginRegistry,
  DocumentManagerPlugin,
  SelectionPlugin,
  ViewportPlugin,
} from "@embedpdf/svelte-pdf-viewer";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { mkdir, readFile, writeFile, rm, mkdtemp } from "node:fs/promises";
import path from "node:path";
import { tmpdir } from "node:os";
import {
  textSelectionPDF,
  textCoveragePDF,
  standardTextFonts,
  variedTextCases,
} from "./redaction-text-fixture.js";
const exec = promisify(execFile),
  repository = process.cwd();
const captures = process.env.DOCBANK_REDACTION_REVIEW_SCREENSHOT_DIR;
declare global {
  interface Window {
    textSelectionDecode: {
      pause: boolean;
      failPage: number;
      failed: number;
      pending: (() => void)[];
    };
  }
}

test.skip(!captures, "The real-runtime PDF review runner enables this qualification");
test.use({ reducedMotion: "reduce", deviceScaleFactor: 2 });

async function pauseTextRead(target: Locator) {
  await target.evaluate(async (el) => {
    const host = el as HTMLElement & {
      registry: Promise<PluginRegistry>;
      textProof?: { pause: boolean; entered: boolean; release?: () => void };
    };
    const engine = (await host.registry).getEngine(),
      original = engine.getTextSlices.bind(engine);
    const proof = { pause: true, entered: false, release: undefined as (() => void) | undefined };
    host.textProof = proof;
    engine.getTextSlices = (...args) => {
      const task = original(...args),
        promise = task.toPromise.bind(task);
      task.toPromise = () => {
        const result = promise();
        if (!proof.pause) return result;
        proof.pause = false;
        return result.then(
          (value) =>
            new Promise((resolve) => {
              proof.entered = true;
              proof.release = () => {
                engine.getTextSlices = original;
                resolve(value);
              };
            }),
        );
      };
      return task;
    };
  });
}
test("masks selected PDF text using native spans and verified delivery", async ({ page }) => {
  test.setTimeout(360000);
  const binary = process.env.DOCBANK_SCREENSHOT_BINARY!,
    inspector = process.env.DOCBANK_REDACTION_REVIEW_INSPECTOR!;
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-text-selection-"));
  const env = {
    ...process.env,
    DOCBANK_HOME: path.join(workspace, "vault"),
    DOCBANK_TELEMETRY_ENABLED: "0",
  };
  const run = async (...args: string[]) =>
    (
      await exec(binary, args, { cwd: repository, env, timeout: 180000, maxBuffer: 20 << 20 })
    ).stdout.trim();
  await page.addInitScript(() => {
    const state = { pause: false, failPage: 0, failed: 0, pending: [] as (() => void)[] };
    (window as Window & { textSelectionDecode: typeof state }).textSelectionDecode = state;
    const decode = HTMLImageElement.prototype.decode;
    HTMLImageElement.prototype.decode = function () {
      return decode.call(this).then(() => {
        if (!(this.getRootNode() instanceof ShadowRoot)) return;
        let parent = this.parentElement;
        while (parent && !parent.querySelector("[data-review-page]")) parent = parent.parentElement;
        const number = Number(
          parent?.querySelector<HTMLElement>("[data-review-page]")?.dataset.reviewPage,
        );
        if (number === state.failPage) {
          state.failed++;
          throw Error("Synthetic neighbor decode failure");
        }
        if (state.pause) return new Promise<void>((resolve) => state.pending.push(resolve));
      });
    };
  });
  let started = false;
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  try {
    const renderer =
      process.env.DOCBANK_TEST_PDFTOPPM ?? (await exec("which", ["pdftoppm"])).stdout.trim();
    await exec(
      "go",
      [
        "run",
        "-tags",
        "fts5",
        "./frontend/screenshots/bates-recovery-fixture.go",
        workspace,
        inspector,
        renderer,
        "/usr/bin/prlimit",
      ],
      { cwd: repository, env, timeout: 120000 },
    );
    await mkdir(captures!, { recursive: true });
    for (const kind of ["small", "fractional", "duplicate", "edge", "tight", "large"] as const) {
      const name = `${kind}-selection.pdf`;
      await writeFile(path.join(workspace, name), textSelectionPDF(kind));
      started = true;
      await run("add", path.join(workspace, name), "--dest", "/", "--json");
      await page.goto("about:blank");
      await page.goto(await run("web", "--no-browser"));
      await page.getByRole("cell", { name, exact: true }).click();
      await page.getByRole("button", { name: "Redact PDF", exact: true }).click();
      const drawer = page.getByRole("dialog", { name: `Redact ${name}`, exact: true });
      const draw = drawer.getByRole("switch", { name: "Draw rectangle", exact: true });
      await expect(draw).toBeEnabled({ timeout: 60000 });
      const host = drawer.locator("embedpdf-container"),
        add = drawer.getByRole("button", { name: "Mask selected text", exact: true });
      const select = async (n: number, from: number, to: number) =>
        host.evaluate(
          async (el, { n, from, to }) => {
            const r = await (el as HTMLElement & { registry: Promise<PluginRegistry> }).registry;
            const d = r
              .getPlugin<DocumentManagerPlugin>("document-manager")!
              .provides()
              .getActiveDocument()!;
            await r
              .getPlugin<SelectionPlugin>("selection")!
              .provides()
              .forDocument(d.id)
              .setSelection({
                start: { page: n - 1, index: from },
                end: { page: n - 1, index: to },
              })
              .toPromise();
          },
          { n, from, to },
        );
      if (kind === "small" || kind === "fractional") {
        const points = await host.evaluate(async (el) => {
          const r = await (el as HTMLElement & { registry: Promise<PluginRegistry> }).registry;
          const d = r
            .getPlugin<DocumentManagerPlugin>("document-manager")!
            .provides()
            .getActiveDocument()!;
          const scope = r.getPlugin<SelectionPlugin>("selection")!.provides().forDocument(d.id);
          await scope
            .setSelection({ start: { page: 0, index: 0 }, end: { page: 0, index: 1000 } })
            .toPromise();
          const runs = scope.getState().geometry[0]!.runs;
          const first = runs[0]!.glyphs[11]!,
            last = runs[2]!.glyphs[5]!;
          scope.clear();
          return {
            width: d.pages[0]!.size.width,
            height: d.pages[0]!.size.height,
            a: {
              x: first.tightX! + first.tightWidth! / 2,
              y: first.tightY! + first.tightHeight! / 2,
            },
            b: { x: last.tightX! + last.tightWidth! / 2, y: last.tightY! + last.tightHeight! / 2 },
          };
        });
        const zoom = drawer.getByRole("textbox", { name: "Set zoom" });
        await zoom.fill("200");
        await zoom.press("Enter");
        await expect(draw).toBeEnabled();
        const layer = drawer.locator('[data-review-page="1"]');
        const initial = (await layer.boundingBox())!;
        await page.mouse.move(initial.x + initial.width / 2, Math.max(initial.y + 50, 200));
        await page.mouse.wheel(0, 20);
        await expect
          .poll(() =>
            host.evaluate(async (el) =>
              (await (el as HTMLElement & { registry: Promise<PluginRegistry> }).registry)
                .getPlugin<ViewportPlugin>("viewport")!
                .provides()
                .isScrolling(),
            ),
          )
          .toBe(false);
        const drag = async (reverse = false, rotated = false) => {
          const viewport = (await host.boundingBox())!;
          await page.mouse.move(viewport.x + viewport.width / 2, viewport.y + 100);
          await page.mouse.wheel(-3000, -3000);
          const metrics = () =>
            host.evaluate(async (el) => {
              const r = await (el as HTMLElement & { registry: Promise<PluginRegistry> }).registry,
                d = r
                  .getPlugin<DocumentManagerPlugin>("document-manager")!
                  .provides()
                  .getActiveDocument()!;
              const m = r
                .getPlugin<ViewportPlugin>("viewport")!
                .provides()
                .forDocument(d.id)
                .getMetrics();
              return { x: m.scrollLeft, y: m.scrollTop };
            });
          await expect.poll(metrics).toEqual({ x: 0, y: 0 });
          await page.mouse.wheel(0, 20);
          if (kind === "fractional") await expect.poll(async () => (await metrics()).y).toBe(20);
          await expect
            .poll(() =>
              host.evaluate(async (el) =>
                (await (el as HTMLElement & { registry: Promise<PluginRegistry> }).registry)
                  .getPlugin<ViewportPlugin>("viewport")!
                  .provides()
                  .isScrolling(),
              ),
            )
            .toBe(false);
          await expect(draw).toBeEnabled();
          const bounds = (await layer.boundingBox())!;
          const screen = (p: { x: number; y: number }) =>
            rotated
              ? {
                  x: bounds.x + ((points.height - p.y) / points.height) * bounds.width,
                  y: bounds.y + (p.x / points.width) * bounds.height,
                }
              : {
                  x: bounds.x + (p.x / points.width) * bounds.width,
                  y: bounds.y + (p.y / points.height) * bounds.height,
                };
          const start = screen(reverse ? points.b : points.a),
            end = screen(reverse ? points.a : points.b);
          await page.mouse.move(start.x, start.y);
          await page.mouse.down();
          await page.mouse.move(end.x, end.y, { steps: 8 });
          await page.mouse.up();
          await expect(add).toBeEnabled();
        };
        await drag();
        await drag(true);
        if (kind === "small") {
          await drawer.getByRole("button", { name: "Page Settings", exact: true }).click();
          await drawer.getByRole("menuitem", { name: "Rotate Clockwise", exact: true }).click();
          await expect(draw).toBeEnabled();
          await drag(false, true);
          await drawer.getByRole("button", { name: "Page Settings", exact: true }).click();
          await drawer
            .getByRole("menuitem", { name: "Rotate Counter-Clockwise", exact: true })
            .click();
          await expect(draw).toBeEnabled();
          await drag();
        }
        await drawer.screenshot({
          path: path.join(captures!, `text-selection-action-${kind}.png`),
          animations: "disabled",
        });
      } else
        await select(
          1,
          kind === "edge" ? 22 : kind === "tight" || kind === "large" ? 0 : 11,
          kind === "edge" ? 23 : kind === "tight" ? 5 : kind === "large" ? 1024 : 24,
        );
      if (kind === "duplicate" || kind === "edge") {
        await expect(
          drawer.getByText("This text could not be matched uniquely. Draw a rectangle instead.", {
            exact: true,
          }),
        ).toBeVisible();
        await expect(add).toBeDisabled();
        await drawer.getByRole("button", { name: "Back to reading", exact: true }).click();
        continue;
      }
      if (kind === "small") {
        await page.setViewportSize({ width: 640, height: 400 });
        await expect(draw).toBeEnabled();
        await select(1, 11, 24);
        await expect(add).toBeEnabled();
        await drawer.locator(".review-footer").hover();
        await page.mouse.wheel(0, 600);
        await expect(add).toBeInViewport({ ratio: 1 });
        await drawer.screenshot({
          path: path.join(captures!, "text-selection-short.png"),
          animations: "disabled",
        });
      }
      await expect(add).toBeEnabled();
      const addBounds = (await add.boundingBox())!;
      await page.mouse.click(addBounds.x + addBounds.width / 2, addBounds.y + addBounds.height / 2);
      if (kind === "small") {
        await page.setViewportSize({ width: 1440, height: 960 });
        await expect(draw).toBeEnabled();
      }
      await expect(drawer.getByText("Selected text · Page 1", { exact: true })).toBeVisible();
      const selectionStatus = drawer.locator(
        '.redaction-editor > [role="status"][aria-live="polite"]',
      );
      await expect(selectionStatus).not.toBeEmpty();
      const firstAnnouncement = await selectionStatus.textContent();
      if (kind === "small")
        await drawer.screenshot({
          path: path.join(captures!, "text-selection-masks.png"),
          animations: "disabled",
        });
      if (kind === "large") {
        await expect(drawer.getByRole("button", { name: "Remove", exact: true })).toHaveCount(1);
        await drawer.getByRole("button", { name: "Back to reading", exact: true }).click();
        await page
          .getByRole("dialog", { name: "Discard redaction?", exact: true })
          .getByRole("button", { name: "Discard and close", exact: true })
          .click();
        continue;
      }
      if (kind !== "tight") {
        await drawer.getByRole("button", { name: "Remove", exact: true }).click();
        await expect(drawer.getByText("No masks", { exact: true })).toBeVisible();
        await select(1, 11, 24);
        await expect(add).toBeEnabled();
        await add.click();
        if (kind === "small") {
          const hostState = async (
            command: "neighbor-fail" | "neighbor-ready" | "pause" | "resume",
          ) =>
            host.evaluate(async (el, command) => {
              const r = await (el as HTMLElement & { registry: Promise<PluginRegistry> }).registry;
              const d = r
                .getPlugin<DocumentManagerPlugin>("document-manager")!
                .provides()
                .getActiveDocument()!;
              const state = (
                window as Window & {
                  textSelectionDecode: {
                    pause: boolean;
                    failPage: number;
                    pending: (() => void)[];
                  };
                }
              ).textSelectionDecode;
              if (command === "neighbor-fail" || command === "neighbor-ready") {
                state.failPage = command === "neighbor-fail" ? 2 : 0;
                r.getStore().dispatch({
                  type: "REFRESH_PAGES",
                  payload: { documentId: d.id, pageIndexes: [1] },
                });
              } else if (command === "pause") {
                state.pause = true;
                r.getStore().dispatch({
                  type: "REFRESH_PAGES",
                  payload: { documentId: d.id, pageIndexes: [0] },
                });
              } else {
                state.pause = false;
                state.pending.splice(0).forEach((f) => f());
              }
            }, command);
          const zoom = drawer.getByRole("textbox", { name: "Set zoom" });
          await zoom.fill("100");
          await zoom.press("Enter");
          await expect(draw).toBeEnabled();
          await select(1, 11, 24);
          await expect(add).toBeEnabled();
          await hostState("neighbor-fail");
          await expect
            .poll(() =>
              page.evaluate(
                () =>
                  (window as Window & { textSelectionDecode: { failed: number } })
                    .textSelectionDecode.failed,
              ),
            )
            .toBeGreaterThan(0);
          await expect(add).toBeEnabled();
          await expect(drawer.getByRole("button", { name: "Remove", exact: true })).toHaveCount(1);
          await hostState("neighbor-ready");
          // Redraw the page under a live pointer draft, without a pointer-up.
          await select(1, 0, 1000);
          const point = await host.evaluate(async (el) => {
            const r = await (el as HTMLElement & { registry: Promise<PluginRegistry> }).registry;
            const d = r
              .getPlugin<DocumentManagerPlugin>("document-manager")!
              .provides()
              .getActiveDocument()!;
            const scope = r.getPlugin<SelectionPlugin>("selection")!.provides().forDocument(d.id);
            const g = scope.getState().geometry[0]!.runs[0]!.glyphs[11]!;
            scope.clear();
            return {
              x: g.tightX! + g.tightWidth! / 2,
              y: g.tightY! + g.tightHeight! / 2,
              width: d.pages[0]!.size.width,
              height: d.pages[0]!.size.height,
            };
          });
          const layer = drawer.locator('[data-review-page="1"]'),
            bounds = (await layer.boundingBox())!;
          await page.mouse.move(
            bounds.x + (point.x / point.width) * bounds.width,
            bounds.y + (point.y / point.height) * bounds.height,
          );
          await page.mouse.down();
          await page.mouse.move(
            bounds.x + (point.x / point.width) * bounds.width + 12,
            bounds.y + (point.y / point.height) * bounds.height + 3,
          );
          await hostState("pause");
          await expect
            .poll(() =>
              page.evaluate(
                () =>
                  (window as Window & { textSelectionDecode: { pending: (() => void)[] } })
                    .textSelectionDecode.pending.length,
              ),
            )
            .toBeGreaterThan(0);
          await expect(draw).toBeDisabled();
          await expect(add).toBeDisabled();
          expect(
            await host.evaluate(async (el) => {
              const r = await (el as HTMLElement & { registry: Promise<PluginRegistry> }).registry;
              const d = r
                .getPlugin<DocumentManagerPlugin>("document-manager")!
                .provides()
                .getActiveDocument()!;
              const state = r
                .getPlugin<SelectionPlugin>("selection")!
                .provides()
                .forDocument(d.id)
                .getState();
              return { selection: state.selection, selecting: state.selecting };
            }),
          ).toEqual({ selection: null, selecting: false });
          await page.mouse.up();
          await hostState("resume");
          await expect(draw).toBeEnabled();
          await expect(add).toBeDisabled();
          await expect(drawer.getByRole("button", { name: "Remove", exact: true })).toHaveCount(1);
          // Cancellation alone must retire the SDK anchor before any pointer-up.
          const cancelBounds = (await layer.boundingBox())!;
          await page.mouse.move(
            cancelBounds.x + (point.x / point.width) * cancelBounds.width,
            cancelBounds.y + (point.y / point.height) * cancelBounds.height,
          );
          await page.mouse.down();
          await page.mouse.move(
            cancelBounds.x + (point.x / point.width) * cancelBounds.width + 12,
            cancelBounds.y + (point.y / point.height) * cancelBounds.height + 3,
          );
          await layer.dispatchEvent("pointercancel", {
            pointerId: 1,
            pointerType: "mouse",
            bubbles: true,
            composed: true,
          });
          expect(
            await host.evaluate(async (el) => {
              const r = await (el as HTMLElement & { registry: Promise<PluginRegistry> }).registry;
              const d = r
                .getPlugin<DocumentManagerPlugin>("document-manager")!
                .provides()
                .getActiveDocument()!;
              const s = r
                .getPlugin<SelectionPlugin>("selection")!
                .provides()
                .forDocument(d.id)
                .getState();
              return { selection: s.selection, selecting: s.selecting };
            }),
          ).toEqual({ selection: null, selecting: false });
          await expect(add).toBeDisabled();
          await page.mouse.up();
          // Delay a real worker response, then replace the selection before it arrives.
          await pauseTextRead(host);
          await select(1, 11, 16);
          await expect
            .poll(() =>
              host.evaluate(
                (el) => (el as HTMLElement & { textProof: { entered: boolean } }).textProof.entered,
              ),
            )
            .toBe(true);
          await select(1, 19, 24);
          await expect(add).toBeDisabled();
          await host.evaluate((el) =>
            (el as HTMLElement & { textProof: { release: () => void } }).textProof.release(),
          );
          await expect(add).toBeEnabled();
          await add.click();
          await expect(layer.locator("svg rect")).toHaveCount(18);
          expect(
            await layer
              .locator("svg rect")
              .evaluateAll((nodes) =>
                nodes.slice(-6).every((n) => Number(n.getAttribute("y")) > 30),
              ),
          ).toBe(true);
          await drawer.getByRole("button", { name: "Remove", exact: true }).last().click();
          await expect(drawer.getByRole("button", { name: "Remove", exact: true })).toHaveCount(1);
        }
        for (const n of [2, 3, 4]) {
          const field = drawer.getByRole("textbox", { name: "", exact: true });
          await field.fill(String(n));
          await field.press("Enter");
          await expect(drawer.locator(`[data-review-page="${n}"]`)).toBeInViewport();
          await page.evaluate(
            () =>
              new Promise<void>((resolve) =>
                requestAnimationFrame(() => requestAnimationFrame(() => resolve())),
              ),
          );
          await expect
            .poll(() =>
              host.evaluate(async (el) =>
                (await (el as HTMLElement & { registry: Promise<PluginRegistry> }).registry)
                  .getPlugin<ViewportPlugin>("viewport")!
                  .provides()
                  .isScrolling(),
              ),
            )
            .toBe(false);
          await expect(draw).toBeEnabled();
          await select(n, 11, 24);
          await expect(add).toBeEnabled();
          await add.click();
          if (n === 2) {
            await expect(selectionStatus).not.toBeEmpty();
            expect(await selectionStatus.textContent()).not.toBe(firstAnnouncement);
            await drawer.getByRole("button", { name: "Remove", exact: true }).last().click();
            await expect(drawer.getByRole("button", { name: "Remove", exact: true })).toHaveCount(
              1,
            );
            await select(n, 11, 24);
            await expect(add).toBeEnabled();
            await add.click();
          }
        }
      }
      await drawer.screenshot({
        path: path.join(captures!, `text-selection-${kind}.png`),
        animations: "disabled",
      });
      await drawer.getByRole("button", { name: "Preview redaction", exact: true }).click();
      if (kind === "tight") {
        await expect(
          drawer.getByText(
            "The safety margin reaches nearby text. Review the added area before continuing.",
            { exact: true },
          ),
        ).toBeVisible();
        await drawer.getByRole("button", { name: "Use expanded mask", exact: true }).click();
        await drawer.getByRole("button", { name: "Preview redaction", exact: true }).click();
      }
      await expect(
        drawer.getByRole("button", { name: "Confirm and download ZIP", exact: true }),
      ).toBeVisible({ timeout: 120000 });
      await drawer.getByRole("button", { name: "Text", exact: true }).click();
      await expect(drawer.locator("pre")).toHaveText(
        kind === "tight" ? "[REDACTED] [REDACTED]EEP" : "KEEP FIRST [REDACTED] KEEP SECOND",
      );
      await drawer.getByRole("button", { name: "Confirm and download ZIP", exact: true }).click();
      const download = page.waitForEvent("download");
      await page
        .getByRole("dialog", { name: "Download reviewed PDF?", exact: true })
        .getByRole("button", { name: "Download ZIP", exact: true })
        .click();
      const zip = path.join(workspace, `${kind}.zip`);
      await (await download).saveAs(zip);
      for (const privateKind of ["preparation", "masks", "review"] as const) {
        const button = drawer.getByRole("button", { name: `Save ${privateKind}`, exact: true });
        if (!(await button.isVisible()))
          await drawer.getByText("Private review files", { exact: true }).click();
        const pending = page.waitForEvent("download");
        await button.click();
        await (await pending).saveAs(path.join(workspace, `${privateKind}.json`));
      }
      await run(
        "redaction",
        "verify",
        path.join(workspace, "preparation.json"),
        path.join(workspace, "masks.json"),
        path.join(workspace, "review.json"),
        zip,
        "--json",
      );
      const masks = JSON.parse(
        await readFile(path.join(workspace, "masks.json"), "utf8"),
      ) as RedactionMask[];
      if (kind !== "tight") {
        expect(masks).toHaveLength(5);
        expect(masks.map((m) => m.selector.span)).toEqual([
          { start: 11, end: 25 },
          { start: 37, end: 43 },
          { start: 68, end: 74 },
          { start: 85, end: 99 },
          { start: 122, end: 136 },
        ]);
        expect(masks.every((m) => m.selector.kind === "text" && !m.selector.boxes)).toBe(true);
      }
      await drawer.getByRole("button", { name: "Back to reading", exact: true }).click();
      const discard = page.getByRole("dialog", { name: "Discard redaction?", exact: true });
      if (await discard.isVisible())
        await discard.getByRole("button", { name: "Discard and close", exact: true }).click();
    }
    await exec(
      "go",
      ["run", "-tags", "fts5", "./frontend/screenshots/redaction-text-fixture.go", workspace],
      { cwd: repository, env, timeout: 120000 },
    );
    for (const [name, cases] of [
      [
        "standard-selection.pdf",
        standardTextFonts.map((name) => ({ name, target: "staff", match: true })),
      ],
      ["varied-selection.pdf", variedTextCases],
      [
        "subsets-selection.pdf",
        [
          { name: "Greek", target: "Ελληνικά", match: false },
          { name: "two subsets", target: "résumé", match: true },
        ],
      ],
    ] as const) {
      if (name !== "subsets-selection.pdf")
        await writeFile(
          path.join(workspace, name),
          textCoveragePDF(name === "standard-selection.pdf"),
        );
      await run("add", path.join(workspace, name), "--dest", "/", "--json");
      const prep = JSON.parse(
        await run("redaction", "prepare", `/${name}`, "--json"),
      ) as RedactionPreparation;
      await page.goto("about:blank");
      await page.goto(await run("web", "--no-browser"));
      await page.getByRole("cell", { name, exact: true }).click();
      await page.getByRole("button", { name: "Redact PDF", exact: true }).click();
      const drawer = page.getByRole("dialog", { name: `Redact ${name}`, exact: true }),
        host = drawer.locator("embedpdf-container");
      const draw = drawer.getByRole("switch", { name: "Draw rectangle", exact: true }),
        add = drawer.getByRole("button", { name: "Mask selected text", exact: true });
      for (const [index, entry] of cases.entries()) {
        if (index) {
          const field = drawer.getByRole("textbox", { name: "", exact: true });
          await field.fill(String(index + 1));
          await field.press("Enter");
        }
        // Page readiness alone does not mean navigation and tiling have settled.
        await expect(drawer.locator(`[data-review-page="${index + 1}"]`)).toBeInViewport();
        await page.evaluate(
          () =>
            new Promise<void>((resolve) =>
              requestAnimationFrame(() => requestAnimationFrame(() => resolve())),
            ),
        );
        await expect
          .poll(() =>
            host.evaluate(async (el) =>
              (await (el as HTMLElement & { registry: Promise<PluginRegistry> }).registry)
                .getPlugin<ViewportPlugin>("viewport")!
                .provides()
                .isScrolling(),
            ),
          )
          .toBe(false);
        await expect(draw).toBeEnabled({ timeout: 60000 });
        await host.evaluate(
          async (el, { index, target }) => {
            const r = await (el as HTMLElement & { registry: Promise<PluginRegistry> }).registry,
              d = r
                .getPlugin<DocumentManagerPlugin>("document-manager")!
                .provides()
                .getActiveDocument()!;
            const scope = r.getPlugin<SelectionPlugin>("selection")!.provides().forDocument(d.id);
            await scope
              .setSelection({
                start: { page: index, index: 0 },
                end: { page: index, index: 100000 },
              })
              .toPromise();
            const text = (await scope.getSelectedText().toPromise())[0]!,
              at = text.indexOf(target);
            if (at < 0) throw Error("Synthetic selection target absent");
            await scope
              .setSelection({
                start: { page: index, index: at },
                end: { page: index, index: at + target.length - 1 },
              })
              .toPromise();
          },
          { index, target: entry.target },
        );
        if (entry.match) await expect(add, entry.name).toBeEnabled();
        else {
          await expect(
            drawer.getByText("This text could not be matched uniquely. Draw a rectangle instead.", {
              exact: true,
            }),
            entry.name,
          ).toBeVisible();
          await expect(add).toBeDisabled();
        }
        expect(
          prep.map.gaps.some((g) => g.box.page === index + 1),
          entry.name,
        ).toBe(!entry.match);
      }
      if (name === "subsets-selection.pdf") {
        await pauseTextRead(host);
        await host.evaluate(async (el) => {
          const r = await (el as HTMLElement & { registry: Promise<PluginRegistry> }).registry,
            d = r
              .getPlugin<DocumentManagerPlugin>("document-manager")!
              .provides()
              .getActiveDocument()!;
          await r
            .getPlugin<SelectionPlugin>("selection")!
            .provides()
            .forDocument(d.id)
            .setSelection({ start: { page: 1, index: 0 }, end: { page: 1, index: 4 } })
            .toPromise();
        });
        await expect
          .poll(() =>
            host.evaluate(
              (el) => (el as HTMLElement & { textProof: { entered: boolean } }).textProof.entered,
            ),
          )
          .toBe(true);
        await drawer.getByRole("button", { name: "Back to reading", exact: true }).click();
        await expect(drawer).toBeHidden();
        await page
          .getByRole("dialog", { name: "Read subsets-selection.pdf", exact: true })
          .getByRole("button", { name: "Redact PDF", exact: true })
          .click();
        await expect(draw).toBeEnabled({ timeout: 60000 });
        await host.evaluate((el) =>
          (el as HTMLElement & { textProof: { release: () => void } }).textProof.release(),
        );
        await page.evaluate(
          () =>
            new Promise<void>((resolve) =>
              requestAnimationFrame(() => requestAnimationFrame(() => resolve())),
            ),
        );
        await expect(add).toBeDisabled();
        await expect(drawer.getByText("No masks", { exact: true })).toBeVisible();
      }
      await drawer.getByRole("button", { name: "Back to reading", exact: true }).click();
    }
    expect(errors).toEqual([]);
  } finally {
    if (started && JSON.parse(await run("daemon", "status", "--json")).running)
      await run("daemon", "stop");
    if (started) expect(JSON.parse(await run("daemon", "status", "--json")).running).toBe(false);
    await rm(workspace, { recursive: true, force: true });
  }
});
