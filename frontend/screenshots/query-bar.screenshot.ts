import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const screenshots = process.env.DOCBANK_QUERY_BAR_SCREENSHOT_DIR;
test.skip(
  !screenshots,
  "DOCBANK_QUERY_BAR_SCREENSHOT_DIR is required for PR-only query-bar captures",
);
test("complete query validation uses the real compiler without executing", async ({ page }) => {
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-query-bar-"));
  const run = async (...args: string[]) =>
    (
      await exec(
        path.join(repository, "bin", process.platform === "win32" ? "docbank.exe" : "docbank"),
        args,
        {
          cwd: repository,
          env: { ...process.env, DOCBANK_HOME: path.join(workspace, "vault") },
          timeout: 60_000,
        },
      )
    ).stdout.trim();
  const searches: string[] = [];
  page.on("request", (request) => {
    const pathname = new URL(request.url()).pathname;
    if (
      pathname === "/api/v1/search" ||
      (pathname === "/api/v1/workspace/queries" && request.method() === "POST")
    )
      searches.push(request.url());
  });
  try {
    await writeFile(
      path.join(workspace, "manual.txt"),
      "Synthetic field manual and archive notes.\n",
      { mode: 0o600 },
    );
    await run("add", path.join(workspace, "manual.txt"), "--dest", "/");
    await page.goto(await run("web", "--no-browser"));
    await page.getByRole("button", { name: "Import history", exact: true }).click();
    const collections = page.getByRole("dialog", { name: "Import history" });
    await collections
      .getByRole("button", { name: /Browse import/ })
      .first()
      .click();
    await collections.getByRole("button", { name: "Search this import" }).click();
    const editor = page.getByRole("dialog", { name: "Filters and sort" });
    await expect(editor.getByRole("status", { name: "Query valid" })).toBeVisible();
    const initial = JSON.parse(
      new URLSearchParams(new URL(page.url()).hash.slice(1)).get("query")!,
    );
    expect(initial.filters.collection_ids).toHaveLength(1);
    // Syntax lives in the top bar, so close the editor, switch it, and reopen.
    await page.keyboard.press("Escape");
    await expect(editor).toBeHidden();
    await page.getByRole("combobox", { name: /^Search syntax/ }).click();
    await page.getByRole("option", { name: "Query syntax", exact: true }).click();
    await page.getByRole("button", { name: "Advanced", exact: true }).click();
    await editor.getByLabel("Query syntax", { exact: true }).fill("name:manual OR archive");
    await expect(editor.getByRole("status", { name: "Query valid" })).toBeVisible();
    await mkdir(screenshots!, { recursive: true, mode: 0o700 });
    await page.screenshot({ path: path.join(screenshots!, "web-query-bar.png"), fullPage: true });
    await editor.getByLabel("Query syntax", { exact: true }).fill("NOT tag:missing-tag");
    await expect(editor.getByRole("alert")).toBeVisible();
    await editor.getByRole("button", { name: "Focus query error" }).click();
    await expect(editor.getByLabel("Query syntax", { exact: true })).toBeFocused();
    await page.screenshot({ path: path.join(screenshots!, "web-query-error.png"), fullPage: true });
    await editor.getByRole("button", { name: "Save…" }).click();
    const saved = page.getByRole("dialog", { name: "Saved searches" });
    await saved.getByLabel("Definition name", { exact: true }).fill("Synthetic review");
    await saved.getByRole("button", { name: "Save as new", exact: true }).click();
    await expect(saved.getByText("Saved Synthetic review.", { exact: true })).toBeVisible();
    expect(searches).toEqual([]);
    await saved.getByRole("button", { name: "Run Synthetic review" }).click();
    await expect(saved).toBeHidden();
    const retained = JSON.parse(
      new URLSearchParams(new URL(page.url()).hash.slice(1)).get("query")!,
    );
    expect(retained.text).toBe("NOT tag:missing-tag");
    expect(retained.filters).toEqual(initial.filters);
    await expect.poll(() => searches.length).toBe(1);
    expect(new URL(searches[0]).pathname).toBe("/api/v1/workspace/queries");
  } finally {
    await run("daemon", "stop");
    const status = JSON.parse(await run("daemon", "status", "--json"));
    if (status.running !== false)
      throw new Error("Synthetic query daemon remains running; workspace retained.");
    await rm(workspace, { recursive: true, force: true });
  }
});
