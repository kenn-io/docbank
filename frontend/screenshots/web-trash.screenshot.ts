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
    webURL = await runDocbank(["web", "--no-browser"]);
    if (!webURL.startsWith("http://127.0.0.1:")) {
      throw new Error("docbank web returned an unexpected browser URL");
    }
    await mkdir(path.dirname(screenshotPath), { recursive: true, mode: 0o700 });
  });

  test.afterAll(async () => {
    if (vault) {
      try {
        await runDocbank(["daemon", "stop"]);
      } catch {
        // Cleanup below removes only this synthetic workspace.
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
  });
});
