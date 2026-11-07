import { test, expect } from "@playwright/test";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { createHash } from "node:crypto";
import { mkdir, mkdtemp, readFile, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import type {
  RedactionPreparation,
  NumberedReservation,
  BatesExport,
} from "../src/generated/docbank.js";
const exec = promisify(execFile),
  repository = process.cwd(),
  captures = process.env.DOCBANK_REDACTION_REVIEW_SCREENSHOT_DIR;
test.skip(!captures, "The real-runtime review runner enables numbering qualification");
test.use({ reducedMotion: "reduce", deviceScaleFactor: 2 });
test("keeps a numbered reviewed PDF and recovers it without a renderer", async ({ page }) => {
  test.setTimeout(240000);
  page.setDefaultTimeout(20000);
  const binary = process.env.DOCBANK_SCREENSHOT_BINARY!,
    inspector = process.env.DOCBANK_REDACTION_REVIEW_INSPECTOR!;
  const scratch = await mkdtemp(path.join(tmpdir(), "docbank-numbered-browser-"));
  const env = {
    ...process.env,
    DOCBANK_HOME: path.join(scratch, "vault"),
    DOCBANK_TELEMETRY_ENABLED: "0",
  };
  const run = async (...args: string[]) =>
    (
      await exec(binary, args, { cwd: repository, env, timeout: 180000, maxBuffer: 20 << 20 })
    ).stdout.trim();
  let started = false;
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  const stop = async () => {
    if (started) {
      if (JSON.parse(await run("daemon", "status", "--json")).running) await run("daemon", "stop");
      started = false;
    }
  };
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
        scratch,
        inspector,
        renderer,
        "/usr/bin/prlimit",
      ],
      { cwd: repository, env, timeout: 120000 },
    );
    started = true;
    await run("add", path.join(scratch, "two-pages.pdf"), "--dest", "/", "--json");
    const prepPath = path.join(scratch, "earlier-preparation.json"),
      masksPath = path.join(scratch, "earlier-masks.json"),
      reviewPath = path.join(scratch, "earlier-review.json");
    const prep: RedactionPreparation = JSON.parse(
      await run("redaction", "prepare", "/two-pages.pdf", "--json"),
    );
    await writeFile(prepPath, JSON.stringify(prep));
    await writeFile(
      masksPath,
      JSON.stringify([
        {
          id: "11111111-1111-4111-8111-111111111111",
          selector: { kind: "page", map_sha256: prep.map.sha256, pages: [1, 2] },
        },
      ]),
    );
    await writeFile(reviewPath, await run("redaction", "resolve", prepPath, masksPath, "--json"));
    const ns = JSON.parse(
      await run("bates", "namespaces", "--create", "--prefix", "QUAL", "--padding", "6", "--json"),
    );
    const reserveAt = async (start: number) =>
      JSON.parse(
        await run(
          "redaction",
          "reserve",
          prepPath,
          masksPath,
          reviewPath,
          "--namespace",
          ns.namespace_id,
          "--start",
          String(start),
          "--retain-original",
          "--json",
        ),
      ) as NumberedReservation;
    await reserveAt(1);
    const exhausted = JSON.parse(
      await run("bates", "namespaces", "--create", "--prefix", "FULL", "--padding", "1", "--json"),
    );
    await run(
      "redaction",
      "reserve",
      prepPath,
      masksPath,
      reviewPath,
      "--namespace",
      exhausted.namespace_id,
      "--start",
      "8",
      "--retain-original",
      "--json",
    );
    await page.goto(await run("web", "--no-browser"));
    await page.getByRole("cell", { name: "two-pages.pdf", exact: true }).click();
    await page.getByRole("button", { name: "Redact PDF", exact: true }).click();
    const drawer = page.getByRole("dialog", { name: "Redact two-pages.pdf", exact: true });
    await expect(drawer.getByRole("switch", { name: "Draw rectangle" })).toBeEnabled({
      timeout: 60000,
    });
    await drawer.getByRole("button", { name: "Mask whole page 1", exact: true }).click();
    // Original's shared reader owns page navigation.
    const pageNumber = drawer.getByRole("textbox", { name: "", exact: true });
    await pageNumber.fill("2");
    await pageNumber.press("Enter");
    await expect(pageNumber).toHaveValue("2");
    await expect(
      drawer.getByRole("button", { name: "Mask whole page 2", exact: true }),
    ).toBeEnabled();
    await drawer.getByRole("button", { name: "Mask whole page 2", exact: true }).click();
    await drawer.getByRole("button", { name: "Preview redaction", exact: true }).click();
    await expect(
      drawer.getByRole("button", { name: "Confirm and download ZIP", exact: true }),
    ).toBeEnabled({ timeout: 60000 });
    await drawer.getByText("Private review files", { exact: true }).click();
    const privateFiles: string[] = [];
    for (const kind of ["preparation", "masks", "review"]) {
      const event = page.waitForEvent("download");
      await drawer.getByRole("button", { name: `Save ${kind}`, exact: true }).click();
      const download = await event,
        file = path.join(scratch, download.suggestedFilename());
      await download.saveAs(file);
      privateFiles.push(file);
    }
    await drawer.getByRole("button", { name: "Number and keep PDF", exact: true }).click();
    const first = drawer.getByRole("textbox", { name: "First number", exact: true });
    const picker = drawer.getByRole("combobox", { name: /^Numbering namespace/ });
    await expect(picker).toBeEnabled();
    await picker.click();
    await page.getByRole("option", { name: "FULL0", exact: true }).click();
    await expect(first).toHaveValue("10");
    await drawer
      .getByRole("checkbox", { name: "Keep the unredacted original indefinitely" })
      .check();
    await first.fill("1");
    await expect(
      drawer.getByRole("button", { name: "Review numbering", exact: true }),
    ).toBeDisabled();
    await expect(
      drawer.getByText("This namespace cannot fit 2 pages. Choose another namespace."),
    ).toBeVisible();
    await drawer
      .getByRole("checkbox", { name: "Keep the unredacted original indefinitely" })
      .uncheck();
    await picker.click();
    await page.getByRole("option", { name: "QUAL000000", exact: true }).click();
    await expect(first).toHaveValue("3");
    await expect(drawer.getByText("QUAL000003–QUAL000004", { exact: true })).toBeVisible();
    const reviewNumbering = drawer.getByRole("button", { name: "Review numbering", exact: true });
    await expect(reviewNumbering).toBeDisabled();
    await drawer
      .getByRole("checkbox", { name: "Keep the unredacted original indefinitely" })
      .check();
    await reviewNumbering.click();
    const confirmation = page.getByRole("dialog", { name: "Keep this numbered PDF?", exact: true });
    await expect(confirmation).toContainText("half-inch footer");
    await page.keyboard.press("Escape");
    await expect(confirmation).toBeHidden();
    await expect(drawer).toBeVisible();
    await mkdir(captures!, { recursive: true });
    await page.setViewportSize({ width: 640, height: 400 });
    await drawer.locator(".review-footer").hover();
    await page.mouse.wheel(0, 500);
    const editor = drawer.locator(".redaction-editor");
    await expect(editor).toBeInViewport({ ratio: 1 });
    await expect(async () => {
      const bounds = await editor.boundingBox();
      if (!bounds) throw Error("Editor missing");
      await page.mouse.move(bounds.x + bounds.width / 2, bounds.y + 20);
      await page.mouse.wheel(0, 350);
      await expect(reviewNumbering).toBeInViewport({ timeout: 500 });
    }).toPass({ timeout: 10000 });
    await page.screenshot({ path: path.join(captures!, "numbered-short.png") });
    await page.setViewportSize({ width: 1440, height: 960 });
    await reviewNumbering.click();
    await page.screenshot({ path: path.join(captures!, "numbered-confirmation.png") });
    let reserved: NumberedReservation | undefined,
      reserveCalls = 0;
    const starts: number[] = [];
    await page.route("**/api/v1/redaction/reservations", async (route) => {
      reserveCalls++;
      starts.push(route.request().postDataJSON().start_at);
      const response = await route.fetch();
      expect(response.status()).toBe(200);
      if (reserveCalls === 1) {
        reserved = await response.json();
        await reserveAt(5);
        await route.abort("failed");
      } else await route.fulfill({ response });
    });
    let publications = 0,
      committed: BatesExport | undefined;
    await page.route("**/api/v1/redaction/publications", async (route) => {
      publications++;
      if (publications === 1) {
        await route.fulfill({
          status: 503,
          contentType: "application/problem+json",
          body: JSON.stringify({
            detail: "Synthetic publication interruption",
            code: "redaction_busy",
          }),
        });
        return;
      }
      const response = await route.fetch();
      expect(response.status()).toBe(200);
      committed = await response.json();
      if (publications === 2) await route.abort("failed");
      else await route.fulfill({ response });
    });
    await page.getByRole("button", { name: "Reserve and keep PDF", exact: true }).click();
    await drawer.getByRole("button", { name: "Retry reservation", exact: true }).click();
    await expect(
      drawer.getByRole("button", { name: "Retry publication", exact: true }),
    ).toBeEnabled();
    expect(starts).toEqual([3, 3]);
    expect(reserved?.pages.map((p) => p.label)).toEqual(["QUAL000003", "QUAL000004"]);
    await drawer.getByRole("button", { name: "Text", exact: true }).click();
    await expect(drawer.getByLabel("Sanitized text for page 2")).toContainText("[REDACTED]");
    await drawer.getByRole("button", { name: "Confirm and download ZIP", exact: true }).click();
    const zipEvent = page.waitForEvent("download");
    await page.getByRole("button", { name: "Download ZIP", exact: true }).click();
    await (await zipEvent).saveAs(path.join(scratch, "plain.zip"));
    await run("redaction", "verify", ...privateFiles, path.join(scratch, "plain.zip"), "--json");
    await run("rm", "/two-pages.pdf");
    await run("trash", "empty", "--run");
    await drawer.getByRole("button", { name: "Retry publication", exact: true }).click();
    await expect(
      drawer.getByRole("button", { name: "Retry publication", exact: true }),
    ).toBeEnabled({ timeout: 60000 });
    expect(committed?.pages.map((p) => p.label)).toEqual(["QUAL000003", "QUAL000004"]);
    // A server-expiry response must not discard the independent reservation.
    await page.route("**/api/v1/redaction/previews/*/text", (route) =>
      route.fulfill({
        status: 410,
        contentType: "application/problem+json",
        body: JSON.stringify({ code: "redaction_preview_expired", detail: "Preview expired" }),
      }),
    );
    await drawer.getByRole("button", { name: "Redacted", exact: true }).click();
    await drawer.getByRole("button", { name: "Text", exact: true }).click();
    await expect(
      drawer.getByRole("button", { name: "Retry publication", exact: true }),
    ).toBeEnabled();
    await expect(drawer.getByRole("button", { name: "Save review", exact: true })).toBeVisible();
    await drawer.getByRole("button", { name: "Retry publication", exact: true }).click();
    const downloadNumbered = drawer.getByRole("button", {
      name: "Download numbered PDF",
      exact: true,
    });
    await expect(downloadNumbered).toBeEnabled({ timeout: 60000 });
    const downloadEvent = page.waitForEvent("download");
    await downloadNumbered.click();
    const pdf = path.join(scratch, "numbered.pdf");
    await (await downloadEvent).saveAs(pdf);
    const bytes = await readFile(pdf),
      digest = createHash("sha256").update(bytes).digest("hex");
    expect(digest).toBe(committed!.blob_sha256);
    expect(bytes.toString("latin1").match(/\/MediaBox \[0 0 612 828\]/g)).toHaveLength(2);
    const checked = JSON.parse(
      await run(
        "bates",
        "export",
        "download",
        reserved!.allocation_id,
        path.join(scratch, "checked.pdf"),
        "--preparation",
        privateFiles[0]!,
        "--masks",
        privateFiles[1]!,
        "--review",
        privateFiles[2]!,
        "--json",
      ),
    );
    expect(JSON.stringify(checked)).toContain("masks_text_and_numbers");
    expect(await readFile(path.join(scratch, "checked.pdf"))).toEqual(bytes);
    await page.screenshot({ path: path.join(captures!, "numbered-kept.png") });
    await drawer.getByRole("button", { name: "Open Productions", exact: true }).click();
    await expect(page.getByRole("main", { name: "Productions", exact: true })).toBeVisible();
    await stop();
    await writeFile(path.join(scratch, "vault", "config.toml"), "");
    started = true;
    await page.goto(await run("web", "--no-browser"));
    await page.getByRole("button", { name: "Productions", exact: true }).click();
    const productions = page.getByRole("main", { name: "Productions", exact: true });
    await productions
      .getByRole("textbox", { name: "Bates number", exact: true })
      .fill("QUAL000004");
    await productions.getByRole("button", { name: "Find", exact: true }).click();
    const results = productions.getByRole("list", { name: "Matching productions" });
    await expect(results.getByText("Numbered redaction", { exact: true })).toBeVisible();
    const recovered = page.waitForEvent("download");
    await results.getByRole("button", { name: "Download PDF", exact: true }).click();
    await (await recovered).saveAs(path.join(scratch, "recovered.pdf"));
    expect(await readFile(path.join(scratch, "recovered.pdf"))).toEqual(bytes);
    expect(errors).toEqual([]);
  } finally {
    await stop();
    await rm(scratch, { recursive: true, force: true });
  }
});
