import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const screenshots = process.env.DOCBANK_TERM_REPORT_SCREENSHOT_DIR;
test.skip(!screenshots, "DOCBANK_TERM_REPORT_SCREENSHOT_DIR enables PR-only report captures");

test("saved search reruns against a changed source scope", async ({ page }) => {
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
    await run("add", source, "--dest", "/");
    let searchable = false;
    for (let attempt = 0; attempt < 80; attempt += 1) {
      const search = JSON.parse(await run("search", "alpha", "--json"));
      if (search.hits?.length > 0) { searchable = true; break; }
      await delay(250);
    }
    expect(searchable).toBe(true);
    const url = await run("web", "--no-browser");
    running = true;
    await page.goto(url);
    await page.getByRole("button", { name: "Search exports", exact: true }).click();
    const drawer = page.getByRole("dialog", { name: "Search exports" });
    await expect(drawer).toBeVisible();
    await drawer.getByRole("textbox", { name: "Expression for term 1" }).fill("alpha");
    await drawer.getByRole("combobox", { name: "Coverage policy" }).selectOption("available_only");
    await drawer.getByRole("button", { name: "Create export" }).click();
    await expect(drawer.getByText("Counts ready", { exact: true }).first()).toBeVisible();
    await expect(drawer.getByText(/1 scoped · 1 searchable · 0 missing text/)).toBeVisible();
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
    await drawer.getByText("Saved searches", { exact: true }).scrollIntoViewIfNeeded();
    await page.screenshot({ path: path.join(screenshots!, "web-saved-searches.png"), animations: "disabled" });
  } finally {
    if (running) {
      await run("daemon", "stop");
      const status = JSON.parse(await run("daemon", "status", "--json"));
      if (status.running !== false) throw new Error("Synthetic report daemon did not stop; workspace retained.");
    }
    await rm(scratch, { recursive: true, force: true });
  }
});
