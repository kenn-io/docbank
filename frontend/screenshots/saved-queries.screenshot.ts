import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const screenshots = path.join(repository, ".superpowers/screenshots");

test("saved queries and literal highlight management", async ({ page }) => {
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-saved-query-"));
  const vault = path.join(workspace, "vault");
  const run = async (...args: string[]) => (await exec(path.join(repository, "docbank"), args, {
    cwd: repository, env: { ...process.env, DOCBANK_HOME: vault }, timeout: 60_000,
  })).stdout.trim();
  try {
    await mkdir(screenshots, { recursive: true, mode: 0o700 });
    const source = path.join(workspace, "review-notes.txt");
    await writeFile(source, "Synthetic notes: compare alpha and beta proposals.\n", { mode: 0o600 });
    await run("add", source, "--dest", "/");
    const webURL = await run("web", "--no-browser");
    await page.goto(webURL);
    await page.getByRole("button", { name: "Saved queries and highlights", exact: true }).click();
    const drawer = page.getByRole("dialog", { name: "Saved queries and highlights" });
    await expect(drawer).toBeVisible();
    const query = { v: 1, text: "alpha OR beta", syntax: "advanced", mode: "lexical", filters: { extensions: ["pdf", "txt"], no_tags: true }, sort: { field: "size", direction: "desc" } };
    await drawer.getByLabel("Definition name").fill("Unreviewed proposals");
    await drawer.getByLabel("Definition description").fill("Compare proposals without changing the current live results.");
    await drawer.getByLabel("Complete query JSON").fill(JSON.stringify(query, null, 2));
    await drawer.getByRole("button", { name: "Save as new", exact: true }).click();
    await expect(drawer.getByText("Saved Unreviewed proposals.", { exact: true })).toBeVisible();
    await expect(drawer.getByRole("button", { name: "Edit Unreviewed proposals", exact: true })).toBeVisible();
    await expect(drawer.getByText(/Saved-query execution is unavailable/)).toBeVisible();
    expect(new URL(page.url()).hash).toBe("");
    await drawer.getByRole("button", { name: "Keep query draft" }).click();
    const savedURL = new URL(page.url());
    const restored = JSON.parse(new URLSearchParams(savedURL.hash.slice(1)).get("query")!);
    expect(restored).toEqual(query);
    expect(savedURL.hash).not.toContain("web_session");
    expect(savedURL.hash).not.toContain("web_upload_secret");
    expect(await page.evaluate(() => Object.keys(localStorage).filter((key) => /query|highlight|session/i.test(key)))).toEqual([]);
    const updatedQuery = { ...query, text: "alpha AND beta" };
    await drawer.getByLabel("Complete query JSON").fill(JSON.stringify(updatedQuery, null, 2));
    await drawer.getByLabel("Definition name").fill("Proposal review");
    await drawer.getByRole("button", { name: "Save changes", exact: true }).click();
    await expect(drawer.getByText("Saved Proposal review.", { exact: true })).toBeVisible();
    await drawer.getByRole("button", { name: "Edit Proposal review", exact: true }).click();
    expect(JSON.parse(await drawer.getByLabel("Complete query JSON").inputValue())).toEqual(updatedQuery);
    expect(page.url()).toBe(savedURL.href);
    await drawer.getByLabel("Complete query JSON").scrollIntoViewIfNeeded();
    await drawer.getByLabel("Complete query JSON").evaluate((editor) => { editor.scrollTop = 0; });
    await page.screenshot({ path: path.join(screenshots, "web-saved-queries.png") });

    await drawer.getByRole("button", { name: "New highlight set" }).click();
    await drawer.getByLabel("Definition name").fill("Proposal terms");
    await drawer.getByLabel("Term 1", { exact: true }).fill("alpha");
    await drawer.getByRole("button", { name: "Add term" }).click();
    await drawer.getByLabel("Term 2", { exact: true }).fill("beta");
    await drawer.getByLabel("Color 2", { exact: true }).fill("#88ccff");
    await drawer.getByRole("button", { name: "Save as new", exact: true }).click();
    await expect(drawer.getByText("Saved Proposal terms.", { exact: true })).toBeVisible();
    await expect(drawer.getByRole("button", { name: /run/i })).toHaveCount(0);
    await page.screenshot({ path: path.join(screenshots, "web-highlight-sets.png") });
    await drawer.getByRole("button", { name: "Delete Proposal terms", exact: true }).click();
    await expect(drawer.getByText(/Revision 1/)).toBeVisible();
    await drawer.getByRole("button", { name: "Confirm deletion" }).click();
    await expect(drawer.getByText("Deleted Proposal terms. Documents were not changed.", { exact: true })).toBeVisible();
    await expect(drawer.getByRole("button", { name: "Edit Proposal terms", exact: true })).toHaveCount(0);
    await drawer.getByRole("button", { name: "Close", exact: true }).click();
    await page.getByRole("button", { name: "Lock web session" }).click();
    await expect(page.getByText("Open your Docbank", { exact: true })).toBeVisible();
    expect(new URL(page.url()).hash).toBe("");
  } finally {
    await run("daemon", "stop");
    const status = JSON.parse(await run("daemon", "status", "--json"));
    if (status.running !== false) throw new Error("Synthetic saved-query daemon did not stop; workspace retained.");
    await rm(workspace, { recursive: true, force: true });
  }
});
