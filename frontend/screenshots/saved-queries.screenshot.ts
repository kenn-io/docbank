import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const screenshots = process.env.DOCBANK_SCREENSHOT_DIR!;

test("saved queries and literal highlight management", async ({ page }) => {
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-saved-query-"));
  const vault = path.join(workspace, "vault");
  const run = async (...args: string[]) =>
    (
      await exec(
        path.join(repository, "bin", process.platform === "win32" ? "docbank.exe" : "docbank"),
        args,
        {
          cwd: repository,
          env: { ...process.env, DOCBANK_HOME: vault },
          timeout: 60_000,
        },
      )
    ).stdout.trim();
  try {
    await mkdir(screenshots, { recursive: true, mode: 0o700 });
    const source = path.join(workspace, "review-notes.txt");
    await writeFile(source, "Synthetic notes: compare alpha and beta proposals.\n", {
      mode: 0o600,
    });
    await run("add", source, "--dest", "/");
    const query = {
      v: 1,
      text: "alpha OR beta",
      syntax: "advanced",
      mode: "lexical",
      filters: { extensions: ["pdf", "txt"], no_tags: true },
      sort: { field: "size", direction: "desc" },
    };
    const launch = new URL(await run("web", "--no-browser"));
    const fragment = new URLSearchParams(launch.hash.slice(1));
    fragment.set("query", JSON.stringify(query));
    launch.hash = fragment.toString();
    await page.goto(launch.toString());

    // Advanced hands its draft to the saved-search editor.
    await page
      .getByRole("dialog", { name: "Filters and sort" })
      .getByRole("button", { name: "Save…" })
      .click();
    const drawer = page.getByRole("dialog", { name: "Saved searches" });
    await expect(drawer).toBeVisible();
    await drawer.getByLabel("Definition name").fill("Unreviewed proposals");
    await drawer
      .getByLabel("Definition description")
      .fill("Compare proposals without changing the current live results.");
    await drawer.getByText("Edit as JSON", { exact: true }).click();
    await drawer.getByLabel("Complete query JSON").fill(JSON.stringify(query, null, 2));
    await drawer.getByRole("button", { name: "Save as new", exact: true }).click();
    await expect(drawer.getByText("Saved Unreviewed proposals.", { exact: true })).toBeVisible();
    await expect(
      drawer.getByRole("heading", { name: "Edit Unreviewed proposals", level: 3 }),
    ).toBeVisible();
    await drawer.getByRole("button", { name: "Keep query draft" }).click();
    await expect(drawer.getByText("Search draft saved.", { exact: true })).toBeVisible();
    const savedURL = new URL(page.url());
    const restored = JSON.parse(new URLSearchParams(savedURL.hash.slice(1)).get("query")!);
    expect(restored).toEqual(query);
    expect(savedURL.hash).not.toContain("web_session");
    expect(savedURL.hash).not.toContain("web_upload_secret");
    expect(
      await page.evaluate(() =>
        Object.keys(localStorage).filter((key) => /query|highlight|session/i.test(key)),
      ),
    ).toEqual([]);
    const updatedQuery = { ...query, text: "alpha AND beta" };
    await drawer.getByLabel("Complete query JSON").fill(JSON.stringify(updatedQuery, null, 2));
    await drawer.getByLabel("Definition name").fill("Proposal review");
    await drawer.getByRole("button", { name: "Save changes", exact: true }).click();
    await expect(drawer.getByText("Saved Proposal review.", { exact: true })).toBeVisible();
    await drawer.getByRole("button", { name: "Rename Proposal review", exact: true }).click();
    await drawer.getByText("Edit as JSON", { exact: true }).click();
    expect(JSON.parse(await drawer.getByLabel("Complete query JSON").inputValue())).toEqual(
      updatedQuery,
    );
    expect(page.url()).toBe(savedURL.href);
    await drawer.getByLabel("Complete query JSON").scrollIntoViewIfNeeded();
    await drawer.getByLabel("Complete query JSON").evaluate((editor) => {
      editor.scrollTop = 0;
    });
    await page.screenshot({ path: path.join(screenshots, "web-saved-queries.png") });

    await drawer.getByRole("button", { name: "New highlight list" }).click();
    await drawer.getByLabel("Definition name").fill("Proposal terms");
    await drawer.getByLabel("Term 1", { exact: true }).fill("alpha");
    await drawer.getByRole("button", { name: "Add term" }).click();
    await drawer.getByLabel("Term 2", { exact: true }).fill("beta");
    await drawer.getByLabel("Color 2", { exact: true }).fill("#88ccff");
    await drawer.getByRole("button", { name: "Save as new", exact: true }).click();
    await expect(drawer.getByText("Saved Proposal terms.", { exact: true })).toBeVisible();
    await expect(drawer.getByRole("button", { name: "Run Proposal terms" })).toHaveCount(0);
    await page.screenshot({ path: path.join(screenshots, "web-highlight-sets.png") });
    await drawer.getByRole("button", { name: "Delete Proposal terms", exact: true }).click();
    await drawer.getByRole("button", { name: "Confirm deletion" }).click();
    await expect(drawer.getByText("Deleted Proposal terms.", { exact: true })).toBeVisible();
    await expect(
      drawer.getByRole("button", { name: "Rename Proposal terms", exact: true }),
    ).toHaveCount(0);
    await drawer.getByRole("button", { name: "Close", exact: true }).click();
    await page.getByRole("button", { name: "Lock web session" }).click();
    await expect(page.getByText("Open your Docbank", { exact: true })).toBeVisible();
    expect(new URL(page.url()).hash).toBe("");
  } finally {
    await run("daemon", "stop");
    const status = JSON.parse(await run("daemon", "status", "--json"));
    if (status.running !== false)
      throw new Error("Synthetic saved-query daemon did not stop; workspace retained.");
    await rm(workspace, { recursive: true, force: true });
  }
});
