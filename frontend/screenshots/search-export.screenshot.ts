import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const screenshots = process.env.DOCBANK_TERM_REPORT_SCREENSHOT_DIR;
test.skip(!screenshots, "DOCBANK_TERM_REPORT_SCREENSHOT_DIR enables PR-only report captures");

test("reports selected versions and preserves historical downloads", async ({ page }) => {
  test.setTimeout(240_000);
  const scratch = await mkdtemp(path.join(tmpdir(), "docbank-search-"));
  const vault = path.join(scratch, "vault");
  const run = async (...args: string[]) => (await exec(path.join(repository, "docbank"), args, {
    cwd: repository, env: { ...process.env, DOCBANK_HOME: vault }, timeout: 90_000,
  })).stdout.trim();
  let running = false;
  try {
    await mkdir(screenshots!, { recursive: true, mode: 0o700 });
    const source = path.join(scratch, "synthetic-review.txt");
    await writeFile(source, "Synthetic alpha review for a new source.\n", { mode: 0o600 });
    running = true;
    await run("add", source, "--dest", "/");
    const other = path.join(scratch, "other-review.txt");
    await writeFile(other, "Synthetic alpha review outside the selected scope.\n", { mode: 0o600 });
    await run("add", other, "--dest", "/");
    let searchable = false;
    for (let attempt = 0; attempt < 80; attempt += 1) {
      const search = JSON.parse(await run("search", "alpha", "--json"));
      if (search.hits?.length === 2) { searchable = true; break; }
      await delay(250);
    }
    expect(searchable).toBe(true);
    const url = await run("web", "--no-browser");
    await page.goto(url);
    await page.getByRole("button", { name: "Search exports", exact: true }).click();
    const drawer = page.getByRole("dialog", { name: "Search exports" });
    await expect(drawer).toBeVisible();
    await drawer.getByRole("textbox", { name: "Expression for term 1" }).fill("alpha");
    await drawer.getByRole("combobox", { name: "Coverage policy" }).selectOption("available_only");
    await drawer.getByRole("button", { name: "Create export" }).click();
    await expect(drawer.getByText("Counts ready", { exact: true }).first()).toBeVisible();
    await expect(drawer.getByText(/2 scoped · 2 searchable · 0 missing text/)).toBeVisible();
    await expect(drawer.getByRole("button", { name: "Download evidence ZIP" })).toBeVisible();
    await drawer.getByText("Current export", { exact: true }).scrollIntoViewIfNeeded();
    await page.screenshot({ path: path.join(screenshots!, "web-search-export-result.png"), animations: "disabled" });

    await drawer.getByRole("button", { name: "Use as draft" }).first().click();
    await expect(drawer.getByText(/Loaded the .* request/)).toBeVisible();
    await drawer.getByRole("radio", { name: "Selected collections" }).check();
    await expect(drawer.getByRole("checkbox").first()).toBeVisible();
    await drawer.getByRole("checkbox").first().check();
    await drawer.getByRole("button", { name: "Create export" }).click();
    await expect(drawer.getByRole("button", { name: "Use as draft" })).toHaveCount(2);
    await expect(drawer.getByText(/1 scoped · 1 searchable · 0 missing text/)).toBeVisible();
    await expect(drawer.getByText("1 collection").first()).toBeVisible();
    await drawer.getByText("Recent exports", { exact: true }).scrollIntoViewIfNeeded();
    await page.screenshot({ path: path.join(screenshots!, "web-recent-exports.png"), animations: "disabled" });

    await drawer.getByRole("button", { name: "Close search exports" }).click();
    await page.getByRole("checkbox", { name: "Select synthetic-review.txt", exact: true }).check();
    await page.getByRole("button", { name: "Report selected documents", exact: true }).click();
    await expect(drawer.getByText("Selected documents (1)", { exact: true })).toBeVisible();
    await drawer.getByRole("textbox", { name: "Expression for term 1" }).fill("alpha");
    await drawer.getByRole("combobox", { name: "Coverage policy" }).selectOption("available_only");
    await page.screenshot({ path: path.join(screenshots!, "selected-report-draft.png"), animations: "disabled" });
    // Check the small viewport in the same capture pass, using the real drawer.
    await page.setViewportSize({ width: 390, height: 844 });
    await expect(drawer.getByText("Selected documents (1)", { exact: true })).toBeVisible();
    await page.screenshot({ path: path.join(screenshots!, "selected-report-mobile.png"), animations: "disabled" });
    await page.setViewportSize({ width: 1440, height: 960 });
    const created = page.waitForResponse(response => response.url().endsWith("/api/v1/search-exports") && response.request().method() === "POST");
    await drawer.getByRole("button", { name: "Create export" }).click();
    const response = await created;
    expect(response.status()).toBe(200);
    const submitted = response.request().postDataJSON();
    const node = JSON.parse(await run("stat", "/synthetic-review.txt", "--json"));
    expect(submitted.selected_documents.documents).toEqual([{ node_id: node.id, version_id: node.current_version_id, sha256: node.blob_hash }]);
    const summary = await response.json();
    expect(summary.counts[0].hits).toBe(1);
    expect(summary.coverage.scoped).toBe(1);
    await expect(drawer.getByRole("button", { name: "Download evidence ZIP" })).toBeVisible();
    const firstDownload = page.waitForEvent("download");
    await drawer.getByRole("button", { name: "Download evidence ZIP" }).click();
    const download = await firstDownload;
    const packetPath = path.join(scratch, "captured.zip");
    await download.saveAs(packetPath);
    expect(await run("search-export", "verify", packetPath)).toContain("internally consistent: true");
    await drawer.getByText("Current export", { exact: true }).scrollIntoViewIfNeeded();
    await page.screenshot({ path: path.join(screenshots!, "selected-report-result.png"), animations: "disabled" });

    await writeFile(source, "Synthetic replacement content.\n", { mode: 0o600 });
    await run("add", source, "--dest", "/", "--replace");
    const secondDownload = page.waitForEvent("download");
    await drawer.getByRole("button", { name: "Download evidence ZIP" }).click();
    const afterChange = await secondDownload;
    const laterPath = path.join(scratch, "after-change.zip");
    await afterChange.saveAs(laterPath);
    expect(await readFile(laterPath)).toEqual(await readFile(packetPath));
    await drawer.getByRole("button", { name: "Use as draft" }).first().click();
    const rejected = page.waitForResponse(response => response.url().endsWith("/api/v1/search-exports") && response.request().method() === "POST");
    await drawer.getByRole("button", { name: "Create export" }).click();
    expect((await rejected).status()).toBe(409);
    await expect(drawer.getByRole("alert")).toHaveText("Selected documents changed. Refresh the workspace and reselect the intended current versions.");

  } finally {
    if (running) {
      await run("daemon", "stop");
      const status = JSON.parse(await run("daemon", "status", "--json"));
      if (status.running !== false) throw new Error("Synthetic report daemon did not stop; workspace retained.");
    }
    await rm(scratch, { recursive: true, force: true });
  }
});
