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

  test.beforeAll(async () => {
    workspace = await mkdtemp(path.join(tmpdir(), "docbank-screenshot-"));
    vault = path.join(workspace, "vault");
    await mkdir(path.dirname(screenshotPath), { recursive: true, mode: 0o700 });
    await rm(screenshotPath, { force: true });
    await rm(restoreScreenshotPath, { force: true });
    await rm(tagAssignmentScreenshotPath, { force: true });
    await rm(tagCatalogScreenshotPath, { force: true });
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

    await runDocbank(["add", reports, "--dest", "/", "--progress", "plain"]);
    await runDocbank(["tag", "create", "tax"]);
    await runDocbank(["tag", "create", "reviewed"]);
    await runDocbank([
      "tag",
      "assign",
      "tax",
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
    const report = page.getByRole("cell", {
      name: "quarterly-tax-report.txt",
      exact: true,
    });
    await expect(report).toBeVisible();
    await report.click();

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
});
