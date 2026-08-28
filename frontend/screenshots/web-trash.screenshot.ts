import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const execFileAsync = promisify(execFile);
const here = path.dirname(fileURLToPath(import.meta.url));
const repositoryRoot = path.resolve(here, "..", "..");
const binary = path.join(repositoryRoot, "docbank");
const screenshotPath = path.join(
  repositoryRoot,
  ".superpowers",
  "screenshots",
  "web-trash-confirmation.png",
);
const restoreScreenshotPath = path.join(
  repositoryRoot,
  ".superpowers",
  "screenshots",
  "web-trash-restore-confirmation.png",
);
const tagAssignmentScreenshotPath = path.join(
  repositoryRoot,
  ".superpowers",
  "screenshots",
  "web-tag-assignment.png",
);
const tagCatalogScreenshotPath = path.join(
  repositoryRoot,
  ".superpowers",
  "screenshots",
  "web-tag-catalog.png",
);
const auditEvidenceScreenshotPath = path.join(
  repositoryRoot,
  ".superpowers",
  "screenshots",
  "web-audit-evidence.png",
);
const storageScreenshotPath = path.join(
  repositoryRoot,
  ".superpowers",
  "screenshots",
  "web-multi-store-storage.png",
);
const tuiStorageScreenshotPath = path.join(
  repositoryRoot,
  ".superpowers",
  "screenshots",
  "tui-multi-store-storage.png",
);
const vaultBrowserScreenshotPath = path.join(
  repositoryRoot,
  ".superpowers",
  "screenshots",
  "web-vault-browser.png",
);
const searchResultsScreenshotPath = path.join(
  repositoryRoot,
  ".superpowers",
  "screenshots",
  "web-search-results.png",
);
const retainedVersionScreenshotPath = path.join(
  repositoryRoot,
  ".superpowers",
  "screenshots",
  "web-retained-version-download.png",
);
const packedStorageScreenshotPath = path.join(
  repositoryRoot,
  ".superpowers",
  "screenshots",
  "web-storage-status.png",
);
const processingPlanScreenshotPath = path.join(
  repositoryRoot,
  ".superpowers",
  "screenshots",
  "web-document-processing-plan.png",
);
const processingPartialScreenshotPath = path.join(
  repositoryRoot,
  ".superpowers",
  "screenshots",
  "web-document-processing-partial.png",
);
const renditionScreenshotPath = path.join(
  repositoryRoot,
  ".superpowers",
  "screenshots",
  "web-document-rendition.png",
);

test.describe("Docbank web screenshots", () => {
  let workspace = "";
  let vault = "";
  let webURL = "";
  let embeddingServer: Server | undefined;

  async function runDocbank(args: string[]): Promise<string> {
    const result = await execFileAsync(binary, args, {
      cwd: repositoryRoot,
      env: {
        ...process.env,
        DOCBANK_HOME: vault,
        DOCBANK_SCREENSHOT_EMBEDDING_KEY: "synthetic-secret",
      },
      maxBuffer: 1024 * 1024,
      timeout: 60_000,
    });
    return result.stdout.trim();
  }

  async function placeForScreenshot(selector: string, move = false): Promise<void> {
    const previewArgs = [
      "storage",
      "place",
      selector,
      "--to",
      "archive",
      "--json",
    ];
    if (move) previewArgs.push("--move");
    const preview = JSON.parse(await runDocbank(previewArgs)) as {
      preview_token?: unknown;
    };
    if (typeof preview.preview_token !== "string" || preview.preview_token === "") {
      throw new Error("storage placement preview omitted its token");
    }
    const operation = JSON.parse(
      await runDocbank([
        "storage",
        "place",
        "--run",
        "--token",
        preview.preview_token,
        "--json",
      ]),
    ) as { id?: unknown };
    if (typeof operation.id !== "string" || operation.id === "") {
      throw new Error("storage placement start omitted its operation ID");
    }
    for (let attempt = 0; attempt < 100; attempt += 1) {
      const current = JSON.parse(
        await runDocbank(["jobs", "show", operation.id, "--json"]),
      ) as { state?: unknown; error?: unknown };
      if (current.state === "completed") return;
      if (current.state === "failed" || current.state === "cancelled") {
        throw new Error(
          `storage placement ${current.state}: ${String(current.error ?? "")}`,
        );
      }
      await new Promise((resolve) => setTimeout(resolve, 50));
    }
    throw new Error("storage placement did not complete");
  }

  test.beforeAll(async () => {
    workspace = await mkdtemp(path.join(tmpdir(), "docbank-screenshot-"));
    vault = path.join(workspace, "vault");
    await mkdir(path.dirname(screenshotPath), { recursive: true, mode: 0o700 });
    await rm(screenshotPath, { force: true });
    await rm(restoreScreenshotPath, { force: true });
    await rm(tagAssignmentScreenshotPath, { force: true });
    await rm(tagCatalogScreenshotPath, { force: true });
    await rm(auditEvidenceScreenshotPath, { force: true });
    await rm(storageScreenshotPath, { force: true });
    await rm(tuiStorageScreenshotPath, { force: true });
    await rm(vaultBrowserScreenshotPath, { force: true });
    await rm(searchResultsScreenshotPath, { force: true });
    await rm(retainedVersionScreenshotPath, { force: true });
    await rm(packedStorageScreenshotPath, { force: true });
    await rm(processingPlanScreenshotPath, { force: true });
    await rm(processingPartialScreenshotPath, { force: true });
    await rm(renditionScreenshotPath, { force: true });
    const archive = path.join(workspace, "archive-store");
    await mkdir(vault, { recursive: true, mode: 0o700 });
    await mkdir(archive, { recursive: true, mode: 0o700 });

    embeddingServer = createServer((_request, response) => {
      response.writeHead(503, { "Content-Type": "application/json" });
      response.end(JSON.stringify({ error: { message: "synthetic provider unavailable" } }));
    });
    await new Promise<void>((resolve, reject) => {
      embeddingServer!.once("error", reject);
      embeddingServer!.listen(0, "127.0.0.1", resolve);
    });
    const embeddingAddress = embeddingServer.address() as AddressInfo;
    const embeddingOrigin = `http://127.0.0.1:${embeddingAddress.port}`;
    const identityResult = await execFileAsync(
      "go",
      ["run", "-tags", "fts5", path.join(here, "processing-profile.go"), embeddingOrigin],
      { cwd: repositoryRoot, maxBuffer: 1024 * 1024, timeout: 60_000 },
    );
    const identities = JSON.parse(identityResult.stdout) as {
      rendition_id: string;
      rendition_fingerprint: string;
      embedding_id: string;
      embedding_fingerprint: string;
      compatibility_id: string;
    };
    await writeFile(
      path.join(vault, "config.toml"),
      `[store_bindings.archive]
kind = "filesystem"
path = ${JSON.stringify(archive)}
priority = 20

[credential_bindings.semantic]
environment_variable = "DOCBANK_SCREENSHOT_EMBEDDING_KEY"

[rendition_profiles.plaintext]
adapter_contract = "docbank-plaintext-rendition/v1"
authorization_fingerprint = "1111111111111111111111111111111111111111111111111111111111111111"
credential_binding = "credential:none"
deployment_fingerprint = "2222222222222222222222222222222222222222222222222222222222222222"
descriptor_id = ${JSON.stringify(identities.rendition_id)}
descriptor_fingerprint = ${JSON.stringify(identities.rendition_fingerprint)}
disclose_filename = false
disclosure_fingerprint = "3333333333333333333333333333333333333333333333333333333333333333"
max_document_bytes = 16777216
max_response_bytes = 16777216
max_units = 1
requested_artifacts = ["structured_evidence"]
trust_boundary = "local_process"
upload_options_fingerprint = "4444444444444444444444444444444444444444444444444444444444444444"

[embedding_profiles.semantic]
activation = "optional"
authorization_fingerprint = "5555555555555555555555555555555555555555555555555555555555555555"
compatibility_id = ${JSON.stringify(identities.compatibility_id)}
credential_binding = "credential:semantic"
descriptor_id = ${JSON.stringify(identities.embedding_id)}
descriptor_fingerprint = ${JSON.stringify(identities.embedding_fingerprint)}
dimensions = 2
disclosure_fingerprint = "6666666666666666666666666666666666666666666666666666666666666666"
document_formatter = "openai-compatible/document/v1"
input_kind = "rendition_chunk"
max_batch_items = 8
max_input_bytes = 1048576
max_response_bytes = 1048576
metric = "cosine"
model = "synthetic-model"
normalization = "none"
query_formatter = "openai-compatible/query/v1"
scalar_encoding = "float32"
trust_boundary = "operator_network"

[embedding_profiles.semantic.chunk]
context_fingerprint = "7777777777777777777777777777777777777777777777777777777777777777"
formatter = "rendition-chunk/v1"
max_tokens = 128
overlap_tokens = 8
tokenizer = "unicode-runes@v1"
truncation_policy = "reject_indivisible"

[embedding_profiles.semantic.model_input]
profile = "nomic/v1"

[embedding_profiles.semantic.runtime]
adapter_contract = "docbank-openai-compatible-embeddings/v1"
endpoint = ${JSON.stringify(embeddingOrigin)}
model_revision = "deployment-v1"
deployment_epoch = "deployment-v1"
request_timeout = "1s"
max_request_bytes = 1048576
max_retries = 1
allowed_cidrs = ["127.0.0.0/8"]
proxy_mode = "disabled"
connect_timeout = "1s"
keep_alive = "1s"
tls_handshake_timeout = "1s"

[retrieval_profiles.hybrid]
lexical_limit = 20
vector_limit = 20

[processing_profiles.private_text]
rendition = "plaintext"
embeddings = ["semantic"]
retrieval = "hybrid"
attachment_policy_fingerprint = "8888888888888888888888888888888888888888888888888888888888888888"
completeness_fingerprint = "9999999999999999999999999999999999999999999999999999999999999999"
consent_fingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
lexical_segmenter_fingerprint = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
max_segment_runes = 2000
max_unit_runes = 100000
normalizer_fingerprint = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
sanitizer_fingerprint = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
retain_sanitized_markdown = true
retain_typed_artifacts = true
trust_boundary = "local_process"
`,
      { mode: 0o600 },
    );
    const reports = path.join(workspace, "synthetic", "Reports");
    await mkdir(reports, { recursive: true, mode: 0o700 });
    await writeFile(
      path.join(reports, "quarterly-tax-report.txt"),
      "Synthetic quarterly tax report for screenshot validation.\n",
      { mode: 0o600 },
    );
    await writeFile(
      path.join(reports, "filing-checklist.md"),
      "# Filing checklist\n\n- Review totals\n- Confirm signatures\n",
      { mode: 0o600 },
    );
    await writeFile(
      path.join(reports, "supporting-schedule.csv"),
      "category,amount\nSynthetic revenue,125000\nSynthetic expense,42000\n",
      { mode: 0o600 },
    );
    const archiveReference = path.join(
      workspace,
      "synthetic",
      "archive-reference.txt",
    );
    await writeFile(
      archiveReference,
      "Synthetic remote-only reference for storage status.\n",
      { mode: 0o600 },
    );

    await runDocbank(["add", reports, "--dest", "/", "--progress", "plain"]);
    await runDocbank([
      "add",
      archiveReference,
      "--dest",
      "/",
      "--progress",
      "plain",
    ]);
    await runDocbank(["tag", "create", "tax"]);
    await runDocbank(["tag", "create", "reviewed"]);
    await runDocbank([
      "tag",
      "assign",
      "tax",
      "/Reports/quarterly-tax-report.txt",
    ]);
    const storePreview = JSON.parse(
      await runDocbank([
        "storage",
        "add",
        "archive",
        "--binding",
        "archive",
        "--json",
      ]),
    ) as { preview_token?: unknown };
    if (
      typeof storePreview.preview_token !== "string" ||
      storePreview.preview_token === ""
    ) {
      throw new Error("storage registration preview omitted its token");
    }
    await runDocbank([
      "storage",
      "add",
      "--run",
      "--token",
      storePreview.preview_token,
      "--json",
    ]);
    await placeForScreenshot("/Reports");
    await placeForScreenshot("/archive-reference.txt", true);
    const preview = JSON.parse(
      await runDocbank(["audit", "enable", "/Reports", "--json"]),
    ) as { preview_token?: unknown };
    if (
      typeof preview.preview_token !== "string" ||
      preview.preview_token === ""
    ) {
      throw new Error("audit enrollment preview omitted its token");
    }
    await runDocbank([
      "audit",
      "enable",
      "--run",
      "--token",
      preview.preview_token,
      "--acknowledge-permanent-retention",
      "--json",
    ]);
    let extractionReady = false;
    for (let attempt = 0; attempt < 100; attempt += 1) {
      const report = JSON.parse(
        await runDocbank(["search", "Synthetic", "--json"]),
      ) as { hits?: unknown[] };
      if ((report.hits?.length ?? 0) > 0) {
        extractionReady = true;
        break;
      }
      await new Promise((resolve) => setTimeout(resolve, 50));
    }
    if (!extractionReady) {
      throw new Error("synthetic text extraction did not complete");
    }
    const revisedReport = path.join(
      workspace,
      "synthetic",
      "revised-quarterly-tax-report.txt",
    );
    await writeFile(
      revisedReport,
      "Synthetic quarterly tax report with reviewed totals.\n",
      { mode: 0o600 },
    );
    await runDocbank([
      "put",
      revisedReport,
      "/Reports/quarterly-tax-report.txt",
    ]);
    webURL = await runDocbank(["web", "--no-browser"]);
    const browserURL = new URL(webURL);
    const port = Number(browserURL.port);
    if (
      browserURL.protocol !== "http:" ||
      !/^docbank-[0-9a-f]{32}\.localhost$/.test(browserURL.hostname) ||
      !Number.isInteger(port) ||
      port < 1 ||
      port > 65_535 ||
      browserURL.username !== "" ||
      browserURL.password !== "" ||
      browserURL.pathname !== "/" ||
      browserURL.search !== "" ||
      browserURL.hash === ""
    ) {
      throw new Error("docbank web returned an unexpected browser URL");
    }
  });

  test.afterAll(async () => {
    if (vault) {
      let stopped = false;
      let stopError: unknown;
      for (let attempt = 0; attempt < 2; attempt += 1) {
        try {
          await runDocbank(["daemon", "stop"]);
          const status = JSON.parse(
            await runDocbank(["daemon", "status", "--json"]),
          ) as { running?: unknown };
          if (status.running !== false) {
            throw new Error("synthetic Docbank daemon is still running");
          }
          stopped = true;
          break;
        } catch (cause) {
          stopError = cause;
        }
      }
      if (!stopped) {
        throw new Error(
          `could not stop the synthetic Docbank daemon; workspace retained at ${workspace}`,
          { cause: stopError },
        );
      }
    }
    if (workspace) await rm(workspace, { recursive: true, force: true });
    if (embeddingServer) {
      await new Promise<void>((resolve, reject) => {
        embeddingServer!.close((error) => (error ? reject(error) : resolve()));
      });
    }
  });

  test("trash confirmation", async ({ page }) => {
    await page.addInitScript(() => {
      localStorage.setItem("docbank-theme", "dark");
    });
    await page.goto(webURL, { waitUntil: "domcontentloaded" });
    await page.addStyleTag({
      content: `
        *, *::before, *::after {
          animation-duration: 0.001s !important;
          animation-delay: 0s !important;
          transition-duration: 0s !important;
          caret-color: transparent !important;
        }
      `,
    });

    await page.getByRole("cell", { name: "Reports", exact: true }).dblclick();
    const report = page.getByRole("cell", {
      name: "quarterly-tax-report.txt",
      exact: true,
    });
    await expect(report).toBeVisible();
    await report.click();
    await expect(page.getByText("tax", { exact: true })).toBeVisible();
    await expect(page.getByText("Protected", { exact: true })).toBeVisible();
    await page.screenshot({
      path: vaultBrowserScreenshotPath,
      fullPage: true,
      animations: "disabled",
    });

    await page.getByRole("button", { name: "Process and retrieve" }).click();
    const processing = page.getByRole("dialog", {
      name: "Document processing and coverage",
    });
    await expect(processing).toContainText("Private network");
    await expect(processing).toContainText("sanitized_markdown");
    await page.screenshot({
      path: processingPlanScreenshotPath,
      fullPage: true,
      animations: "disabled",
    });
    await processing.getByRole("button", { name: "Consent and run" }).click();
    await expect(processing.getByRole("button", { name: "Read sanitized Markdown" })).toBeVisible({ timeout: 30_000 });
    await expect(processing).toContainText(/semantic.*unavailable/i);
    await page.screenshot({
      path: processingPartialScreenshotPath,
      fullPage: true,
      animations: "disabled",
    });
    await processing.getByRole("button", { name: "Read sanitized Markdown" }).click();
    const rendition = page.getByRole("dialog", { name: "Sanitized Markdown rendition" });
    await expect(rendition).toContainText("Synthetic quarterly tax report with reviewed totals");
    await expect(rendition).toContainText("degraded_provenance");
    await page.screenshot({
      path: renditionScreenshotPath,
      fullPage: true,
      animations: "disabled",
    });
    await rendition.getByRole("button", { name: "Close sanitized Markdown" }).click();

    await page.getByRole("button", { name: "Version history" }).click();
    const versions = page.getByRole("dialog", {
      name: "Immutable version history for /Reports/quarterly-tax-report.txt",
    });
    await expect(versions).toContainText("2 retained versions");
    await versions.getByRole("button", { name: /Created at/ }).click();
    await expect(
      versions
        .getByLabel("Complete version authority")
        .getByText("Revision 1", { exact: true }),
    ).toBeVisible();
    await page.screenshot({
      path: retainedVersionScreenshotPath,
      fullPage: true,
      animations: "disabled",
    });
    await versions
      .getByRole("button", { name: "Close version history" })
      .click();

    const search = page.getByPlaceholder("Search names and extracted text");
    await search.fill("Synthetic");
    await search.press("Enter");
    await expect(
      page.getByRole("cell", { name: "content", exact: true }).first(),
    ).toBeVisible();
    await page.screenshot({
      path: searchResultsScreenshotPath,
      fullPage: true,
      animations: "disabled",
    });
    await search.fill("");
    await search.press("Enter");
    await expect(report).toBeVisible();
    await page.getByRole("button", { name: "Back to previous directory" }).click();
    await expect(
      page.getByRole("cell", { name: "Reports", exact: true }),
    ).toBeVisible();

    await page.getByRole("button", { name: "Storage status" }).click();
    const storage = page.getByRole("dialog", {
      name: "Physical storage status",
    });
    await expect(storage).toContainText("2 physical locations");
    await expect(storage).toContainText("archive");
    await expect(storage).toContainText("Sole copies");
    await page.screenshot({
      path: storageScreenshotPath,
      fullPage: true,
      animations: "disabled",
    });
    await storage
      .getByRole("button", { name: "Close storage status" })
      .click();

    await runDocbank(["storage", "pack", "--json"]);
    await page.getByRole("button", { name: "Storage status" }).click();
    const packedStorage = page.getByRole("dialog", {
      name: "Physical storage status",
    });
    await expect(packedStorage).toContainText("1 immutable pack contains");
    await page.screenshot({
      path: packedStorageScreenshotPath,
      fullPage: true,
      animations: "disabled",
    });
    await packedStorage
      .getByRole("button", { name: "Close storage status" })
      .click();

    await page
      .getByRole("button", { name: "Verify permanent audit evidence" })
      .click();
    const auditEvidence = page.getByRole("dialog", {
      name: "Permanent audit verification",
    });
    await expect(auditEvidence).toContainText(
      "Protected history and content agree",
    );
    await expect(auditEvidence).toContainText("1 protected scope");
    await page.screenshot({
      path: auditEvidenceScreenshotPath,
      fullPage: true,
      animations: "disabled",
    });
    await auditEvidence
      .getByRole("button", { name: "Close permanent audit verification" })
      .click();

    await page.getByRole("button", { name: "Manage tag definitions" }).click();
    const catalog = page.getByRole("dialog", {
      name: "Manage tag definitions",
    });
    await expect(catalog).toContainText("tax");
    await expect(catalog).toContainText("reviewed");
    await catalog.getByRole("textbox", { name: "New tag name" }).fill("archived");
    await catalog.getByRole("button", { name: "Create" }).click();
    await expect(catalog).toContainText("Created archived.");
    await page.screenshot({
      path: tagCatalogScreenshotPath,
      fullPage: true,
      animations: "disabled",
    });
    await catalog.getByRole("button", { name: "Done" }).click();

    await page.getByRole("cell", { name: "Reports", exact: true }).dblclick();
    const selectedReport = page.getByRole("cell", {
      name: "quarterly-tax-report.txt",
      exact: true,
    });
    await expect(selectedReport).toBeVisible();
    await selectedReport.click();

    await page.getByRole("button", { name: "Manage", exact: true }).click();
    const tags = page.getByRole("dialog", {
      name: "Manage tags for quarterly-tax-report.txt",
    });
    await expect(tags).toContainText("tax");
    await tags.getByRole("combobox", { name: "Tag to assign: Choose a tag…" }).click();
    await tags.getByRole("option", { name: "reviewed (0)" }).click();
    await tags.getByRole("button", { name: "Add tag" }).click();
    await expect(tags).toContainText("Added reviewed.");
    await page.screenshot({
      path: tagAssignmentScreenshotPath,
      fullPage: true,
      animations: "disabled",
    });
    await tags.getByRole("button", { name: "Done" }).click();

    await page.getByRole("button", { name: "Move to trash", exact: true }).click();

    const confirmation = page.getByRole("dialog", {
      name: "Move quarterly-tax-report.txt to trash",
    });
    await expect(confirmation).toBeVisible();
    await expect(confirmation).toContainText("It remains recoverable from trash.");
    await expect(confirmation).toContainText(
      "This does not empty trash, reclaim stored bytes, or erase permanent audited history.",
    );
    await page.screenshot({
      path: screenshotPath,
      fullPage: true,
      animations: "disabled",
    });

    await confirmation.getByRole("button", { name: "Move to trash" }).click();
    await expect(confirmation).not.toBeVisible();
    await page.getByRole("button", { name: "Recoverable trash" }).click();
    const trash = page.getByRole("dialog", { name: "Recoverable trash" });
    await expect(trash).toContainText("quarterly-tax-report.txt");
    await trash.getByRole("button", { name: "Restore" }).click();

    const restore = page.getByRole("dialog", {
      name: "Restore quarterly-tax-report.txt from trash",
    });
    await expect(restore).toBeVisible();
    await expect(restore).toContainText(
      "It does not roll back versions or alter permanent audited history.",
    );
    await page.screenshot({
      path: restoreScreenshotPath,
      fullPage: true,
      animations: "disabled",
    });
  });

  test("TUI storage operations", async ({ page }) => {
    const socket = `docbank-screenshot-${process.pid}`;
    const session = "docbank-tui";
    const tmuxEnv = {
      ...process.env,
      DOCBANK_HOME: vault,
      DOCBANK_SCREENSHOT_BINARY: binary,
      TERM: "xterm-256color",
    };
    const tmux = async (args: string[]): Promise<string> => {
      const result = await execFileAsync("tmux", ["-L", socket, ...args], {
        cwd: repositoryRoot,
        env: tmuxEnv,
        maxBuffer: 1024 * 1024,
        timeout: 15_000,
      });
      return result.stdout;
    };
    const capture = async (): Promise<string> =>
      tmux(["capture-pane", "-p", "-t", session, "-S", "0"]);
    await tmux([
      "new-session",
      "-d",
      "-x",
      "120",
      "-y",
      "38",
      "-s",
      session,
      'exec "$DOCBANK_SCREENSHOT_BINARY" tui',
    ]);
    try {
      await expect
        .poll(capture, { timeout: 15_000 })
        .toContain("documents for you and your agents");
      await tmux(["send-keys", "-t", session, "O"]);
      const terminal = await expect
        .poll(capture, { timeout: 15_000 })
        .toContain("Vault operations")
        .then(capture);

      await page.setViewportSize({ width: 1280, height: 760 });
      await page.setContent(`
        <!doctype html>
        <meta charset="utf-8">
        <style>
          html, body { margin: 0; background: #07100f; }
          .terminal {
            box-sizing: border-box;
            width: max-content;
            min-width: 100vw;
            min-height: 100vh;
            margin: 0;
            padding: 16px 18px;
            color: #e8f1ef;
            background: #07100f;
            font: 16px/1.25 ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
            white-space: pre;
          }
        </style>
        <pre class="terminal"></pre>
      `);
      await page.locator(".terminal").evaluate((element, text) => {
        element.textContent = String(text);
      }, terminal);
      await page.screenshot({
        path: tuiStorageScreenshotPath,
        fullPage: true,
        animations: "disabled",
      });
    } finally {
      await tmux(["kill-server"]).catch(() => undefined);
    }
  });
});
