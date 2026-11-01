import { expect, test as base } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const run = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const binary = path.join(
  repository,
  "bin",
  process.platform === "win32" ? "docbank.exe" : "docbank",
);
const output = process.env.DOCBANK_DOCUMENT_READER_PROOF_DIR;
base.skip(!output, "DOCBANK_DOCUMENT_READER_PROOF_DIR enables the document reader proof");

function syntheticPDF(): string {
  const streams = ["Synthetic PDF preview", "Second page evidence"].map(
    (text) => `BT /F1 24 Tf 72 720 Td (${text}) Tj ET\n`,
  );
  const objects = [
    "<< /Type /Catalog /Pages 2 0 R >>",
    "<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 >>",
    ...[6, 7].map(
      (content) =>
        `<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents ${content} 0 R >>`,
    ),
    "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
    ...streams.map(
      (stream) => `<< /Length ${Buffer.byteLength(stream)} >>\nstream\n${stream}endstream`,
    ),
  ];
  let pdf = "%PDF-1.7\n";
  const offsets = objects.map((object, index) => {
    const offset = Buffer.byteLength(pdf);
    pdf += `${index + 1} 0 obj\n${object}\nendobj\n`;
    return offset;
  });
  const xref = Buffer.byteLength(pdf);
  return (
    pdf +
    `xref\n0 8\n0000000000 65535 f \n${offsets.map((offset) => `${String(offset).padStart(10, "0")} 00000 n \n`).join("")}trailer\n<< /Size 8 /Root 1 0 R >>\nstartxref\n${xref}\n%%EOF\n`
  );
}

const test = base.extend<{ readerURL: string }>({
  readerURL: async ({ page }, use) => {
    const workspace = await mkdtemp(path.join(tmpdir(), "docbank-reader-"));
    const docbank = async (...args: string[]) =>
      (
        await run(binary, args, {
          cwd: repository,
          env: { ...process.env, DOCBANK_HOME: path.join(workspace, "vault") },
          timeout: 60_000,
        })
      ).stdout.trim();
    try {
      await mkdir(output!, { recursive: true });
      for (const [name, content] of [
        ["Sample.pdf", syntheticPDF()],
        [
          "Notes.md",
          "# Review notes\n\n- First item\n- Second item\n\n| File | Status |\n| --- | --- |\n| Sample | Reviewed |\n\n<script>window.markdownExecuted = true</script>\n",
        ],
      ]) {
        const file = path.join(workspace, name!);
        await writeFile(file, content!);
        await docbank("add", file, "--dest", "/");
      }
      const webURL = await docbank("web", "--no-browser");
      await use(webURL);
    } finally {
      await page.goto("about:blank");
      await docbank("daemon", "stop");
      await rm(workspace, { recursive: true, force: true });
    }
  },
});

test("reads PDFs and Markdown from file rows without a page runtime", async ({
  page,
  context,
  readerURL: webURL,
}) => {
  page.setDefaultTimeout(15_000);
  const errors: string[] = [];
  const externalRequests: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("console", (message) => {
    if (message.type() === "error") errors.push(message.text());
  });
  const origin = new URL(webURL).origin;
  page.on("request", (request) => {
    if (/^https?:/.test(request.url()) && new URL(request.url()).origin !== origin)
      externalRequests.push(request.url());
  });
  await context.grantPermissions(["clipboard-read", "clipboard-write"], { origin });
  await page.goto(webURL);
  const pdfRow = page
    .getByRole("row")
    .filter({ has: page.getByRole("cell", { name: "Sample.pdf", exact: true }) });
  await pdfRow.click();
  const inspector = page.getByRole("complementary", { name: "Document inspector" });
  await expect(inspector.locator("embedpdf-container img").first()).toBeVisible();
  await inspector.getByRole("tab", { name: "Text", exact: true }).click();
  const visibleText = inspector.getByText("Text extraction isn't set up.", { exact: true });
  await expect(visibleText).toBeVisible();
  await visibleText.evaluate((element) => {
    const selection = window.getSelection()!;
    const range = document.createRange();
    range.selectNodeContents(element);
    selection.removeAllRanges();
    selection.addRange(range);
  });
  await page.keyboard.press("ControlOrMeta+c");
  await expect
    .poll(() => page.evaluate(() => navigator.clipboard.readText()))
    .toBe("Text extraction isn't set up.");
  await inspector.getByRole("tab", { name: "Preview", exact: true }).click();
  const previewWorkers = page.workers().length;
  await pdfRow.dblclick();
  const reader = page.getByRole("dialog", { name: "Read Sample.pdf" });
  await expect(reader.locator("embedpdf-container img").first()).toBeVisible();
  await expect(reader.getByRole("button", { name: "Redact PDF", exact: true })).toBeEnabled();
  await reader.getByRole("button", { name: "Close", exact: true }).focus();
  await page.keyboard.press("Tab");
  await expect(reader.locator("embedpdf-container")).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(reader.getByRole("button", { name: "Document Menu" })).toBeFocused();
  const zoom = reader.getByRole("textbox", { name: "Set zoom" });
  await zoom.fill("100");
  await zoom.press("Enter");
  await expect(zoom).toHaveValue("100");
  const pageImage = await reader.locator("embedpdf-container img").first().boundingBox();
  expect(pageImage).not.toBeNull();
  await page.mouse.click(pageImage!.x + pageImage!.width / 2, pageImage!.y + 100);
  await page.mouse.move(
    pageImage!.x + (70 / 612) * pageImage!.width,
    pageImage!.y + (64 / 792) * pageImage!.height,
  );
  await page.mouse.down();
  await page.mouse.move(
    pageImage!.x + (360 / 612) * pageImage!.width,
    pageImage!.y + (64 / 792) * pageImage!.height,
    { steps: 8 },
  );
  await page.mouse.up();
  await page.keyboard.press("ControlOrMeta+c");
  await expect
    .poll(() => page.evaluate(() => navigator.clipboard.readText()))
    .toContain("Synthetic PDF preview");
  await page.keyboard.press("ControlOrMeta+f");
  await expect(inspector.getByRole("textbox", { name: "Search", exact: true })).toBeHidden();
  await reader.getByRole("textbox", { name: "Search", exact: true }).fill("Second page evidence");
  await reader.getByText("Second page evidence", { exact: true }).click();
  await expect
    .poll(() =>
      reader
        .locator("embedpdf-container img")
        .evaluateAll(
          (images) =>
            images.filter(
              (image) =>
                (image as HTMLImageElement).complete &&
                (image as HTMLImageElement).naturalWidth > 0,
            ).length,
        ),
    )
    .toBeGreaterThan(0);
  await page.screenshot({ path: path.join(output!, "pdf-reader.png") });
  await page.setViewportSize({ width: 640, height: 900 });
  await expect(reader.getByRole("button", { name: "Close", exact: true })).toBeInViewport();
  await expect(reader.getByRole("button", { name: "Redact PDF", exact: true })).toBeInViewport();
  await page.screenshot({ path: path.join(output!, "pdf-reader-narrow.png") });
  await page.setViewportSize({ width: 1440, height: 960 });
  await reader.getByRole("button", { name: "Document Menu" }).click();
  await page.getByRole("menuitem", { name: "Print", exact: true }).click();
  await expect(reader.getByRole("heading", { name: "Print Settings" })).toBeVisible();
  await reader.getByRole("button", { name: "Cancel", exact: true }).click();
  const downloaded = page.waitForEvent("download");
  await reader.getByRole("button", { name: "Download", exact: true }).click();
  const download = await downloaded;
  expect(download.suggestedFilename()).toBe("Sample.pdf");
  expect(await readFile((await download.path())!, "utf8")).toBe(syntheticPDF());
  await reader.getByRole("button", { name: "Close", exact: true }).click();
  await expect(reader).toBeHidden();
  await expect(pdfRow).toBeFocused();
  await expect.poll(() => page.workers().length).toBe(previewWorkers);
  await pdfRow.press("Enter");
  await expect(reader.locator("embedpdf-container img").first()).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(reader).toBeHidden();
  await expect(pdfRow).toBeFocused();

  const markdownRow = page
    .getByRole("row")
    .filter({ has: page.getByRole("cell", { name: "Notes.md", exact: true }) });
  await markdownRow.dblclick();
  const markdown = page.getByRole("dialog", { name: "Read Notes.md" });
  await expect(markdown.getByRole("heading", { name: "Review notes" })).toBeVisible();
  await expect(markdown.getByRole("listitem")).toHaveCount(2);
  await expect(markdown.getByRole("cell", { name: "Reviewed" })).toBeVisible();
  await expect(markdown.getByRole("article").locator("script")).toHaveCount(0);
  await page.screenshot({ path: path.join(output!, "markdown-reader.png") });
  await page.keyboard.press("Escape");
  await expect(markdownRow).toBeFocused();
  await expect.poll(() => page.workers().length).toBe(0);
  await inspector.getByRole("button", { name: "Open", exact: true }).click();
  await expect(markdown.getByRole("heading", { name: "Review notes" })).toBeVisible();
  await page.keyboard.press("Escape");
  await page.getByRole("combobox", { name: "Search syntax: Plain words" }).click();
  await page.getByRole("option", { name: "Query syntax" }).click();
  const search = page.getByRole("searchbox", { name: "Search documents" });
  await search.fill("Notes");
  await search.press("Enter");
  const result = page.locator("tr[data-snapshot-node]").filter({ hasText: "Notes.md" });
  await expect(result).toBeVisible();
  await result.dblclick();
  await expect(markdown.getByRole("heading", { name: "Review notes" })).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(result).toBeFocused();
  await search.fill("Sample");
  await search.press("Enter");
  const frozenPDF = page.locator("tr[data-snapshot-node]").filter({ hasText: "Sample.pdf" });
  await frozenPDF.dblclick();
  await expect(reader.locator("embedpdf-container img").first()).toBeVisible();
  await expect(reader.getByRole("button", { name: "Redact PDF", exact: true })).toBeHidden();
  await page.keyboard.press("Escape");
  await expect(frozenPDF).toBeFocused();
  expect(errors).toEqual([]);
  expect(externalRequests).toEqual([]);
});

test("releases an engine that finishes initializing after changing documents", async ({
  page,
  readerURL,
}) => {
  await page.addInitScript(() => {
    const NativeWorker = window.Worker;
    const state = Object.assign(window, { readerWorkersCreated: 0 });
    window.Worker = class extends NativeWorker {
      constructor(...args: ConstructorParameters<typeof Worker>) {
        super(...args);
        state.readerWorkersCreated++;
      }
    };
  });
  let release!: () => void;
  const heldImport = new Promise<void>((resolve) => {
    release = resolve;
  });
  let requested!: () => void;
  const importRequested = new Promise<void>((resolve) => {
    requested = resolve;
  });
  await page.route("**/worker-engine-*.js", async (route) => {
    requested();
    await heldImport;
    await route.continue();
  });
  try {
    await page.goto(readerURL);
    await page
      .getByRole("row")
      .filter({ has: page.getByRole("cell", { name: "Sample.pdf", exact: true }) })
      .click();
    await importRequested;
    await page
      .getByRole("row")
      .filter({ has: page.getByRole("cell", { name: "Notes.md", exact: true }) })
      .click();
    await expect(page.getByRole("heading", { name: "Review notes" })).toBeVisible();
    release();
    await expect
      .poll(() => page.evaluate(() => Reflect.get(window, "readerWorkersCreated")))
      .toBeGreaterThan(0);
    await expect.poll(() => page.workers().length).toBe(0);
  } finally {
    release();
    await page.goto("about:blank");
  }
});
