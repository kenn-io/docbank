import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const execFileAsync = promisify(execFile);
const here = path.dirname(fileURLToPath(import.meta.url));
const repositoryRoot = path.resolve(here, "..", "..");
const binary = path.join(repositoryRoot, "docbank");
const screenshotDirectory = process.env.DOCBANK_SCREENSHOT_DIR;
if (!screenshotDirectory) throw new Error("DOCBANK_SCREENSHOT_DIR is required");
const screenshotPathFor = (name: string): string =>
  path.join(screenshotDirectory, name);
const screenshotPath = screenshotPathFor("web-trash-confirmation.png");
const restoreScreenshotPath = screenshotPathFor(
  "web-trash-restore-confirmation.png",
);
const tagAssignmentScreenshotPath = screenshotPathFor("web-tag-assignment.png");
const tagCatalogScreenshotPath = screenshotPathFor("web-tag-catalog.png");
const auditEvidenceScreenshotPath = screenshotPathFor("web-audit-evidence.png");
const storageScreenshotPath = screenshotPathFor("web-multi-store-storage.png");
const tuiStorageScreenshotPath = screenshotPathFor("tui-multi-store-storage.png");
const vaultBrowserScreenshotPath = screenshotPathFor("web-vault-browser.png");
const searchResultsScreenshotPath = screenshotPathFor("web-search-results.png");
const retainedVersionScreenshotPath = screenshotPathFor(
  "web-retained-version-download.png",
);
const packedStorageScreenshotPath = screenshotPathFor("web-storage-status.png");

test.describe("Docbank web screenshots", () => {
  let workspace = "";
  let vault = "";
  let webURL = "";

  async function runDocbank(args: string[]): Promise<string> {
    const result = await execFileAsync(binary, args, {
      cwd: repositoryRoot,
      env: {
        ...process.env,
        DOCBANK_HOME: vault,
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
    const archive = path.join(workspace, "archive-store");
    const backup = path.join(workspace, "backup-repository");
    await mkdir(vault, { recursive: true, mode: 0o700 });
    await mkdir(archive, { recursive: true, mode: 0o700 });
    await writeFile(
      path.join(vault, "config.toml"),
      `[backup]\nrepo = ${JSON.stringify(backup)}\n\n` +
        `[store_bindings.archive]\nkind = "filesystem"\npath = ${JSON.stringify(archive)}\npriority = 20\n`,
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
    await runDocbank(["backup", "init"]);
    await runDocbank([
      "backup",
      "create",
      "--tag",
      "before-review",
      "--jobs",
      "1",
    ]);
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
    await runDocbank([
      "backup",
      "create",
      "--tag",
      "reviewed",
      "--jobs",
      "1",
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
      await expect
        .poll(capture, { timeout: 15_000 })
        .toContain("2 recovery point(s)");
      const terminal = await capture();

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
