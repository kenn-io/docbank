import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";
import { expect, test } from "@playwright/test";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const binary =
  process.env.DOCBANK_SCREENSHOT_BINARY ??
  path.join(repository, "bin", process.platform === "win32" ? "docbank.exe" : "docbank");
const output = process.env.DOCBANK_SEARCH_PREVIEW_PROOF_DIR;
test.skip(!output, "DOCBANK_SEARCH_PREVIEW_PROOF_DIR enables synthetic search preview proof");

test("search Preview centers verified content and keeps surrounding text scrollable", async ({
  page,
}) => {
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-search-preview-"));
  const vault = path.join(workspace, "vault");
  const docbank = async (...args: string[]) =>
    (
      await exec(binary, args, {
        cwd: repository,
        env: { ...process.env, DOCBANK_HOME: vault, DOCBANK_TELEMETRY_ENABLED: "0" },
        timeout: 60_000,
      })
    ).stdout.trim();
  const earlier = Array.from(
    { length: 100 },
    (_, i) => `Background section ${i + 1}: ordinary project context.`,
  ).join("\n");
  const content = `${earlier}\n\nThe lighthouse budget was approved after review.\nKeep the surrounding discussion visible for context.\n\n${earlier}\n\nThe lighthouse schedule follows the budget.\n\nEnd of the synthetic report.\n`;
  try {
    const file = path.join(workspace, "Project-notes.txt");
    await writeFile(file, content);
    await docbank("add", file, "--dest", "/");
    const rawURL = await docbank("web", "--no-browser");
    const url = new URL(rawURL);
    const fragment = new URLSearchParams(url.hash.slice(1));
    fragment.set(
      "query",
      JSON.stringify({
        v: 1,
        text: "lighthouse",
        syntax: "simple",
        mode: "lexical",
        filters: {},
        sort: { field: "path", direction: "asc" },
      }),
    );
    url.hash = fragment.toString();
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await page.addInitScript(() => localStorage.setItem("docbank-theme", "dark"));
    await page.goto(url.toString());
    const editor = page.getByRole("dialog", { name: "Filters and sort" });
    await editor.getByRole("button", { name: "Search", exact: true }).click();
    await page.getByRole("cell", { name: "/Project-notes.txt", exact: true }).click();
    const text = page.getByLabel("Verified text of Project-notes.txt");
    await expect(text).toHaveText(content);
    await expect(page.getByRole("tab", { name: "Preview" })).toHaveAttribute(
      "aria-selected",
      "true",
    );
    await expect(page.getByText("Search match 1 of 2", { exact: true })).toBeVisible();
    const match = text.locator("mark.active");
    await expect(match).toHaveText("lighthouse");
    await expect.poll(() => text.evaluate((element) => element.scrollTop)).toBeGreaterThan(100);
    const visibleMatch = await match.evaluate((element) => {
      const parent = element.parentElement!.getBoundingClientRect();
      const bounds = element.getBoundingClientRect();
      return bounds.top >= parent.top && bounds.bottom <= parent.bottom;
    });
    expect(visibleMatch).toBe(true);
    await mkdir(output!, { recursive: true });
    await page.screenshot({
      path: path.join(output!, "search-match-preview.png"),
      animations: "disabled",
    });
    const firstScroll = await text.evaluate((element) => element.scrollTop);
    await page.getByRole("button", { name: "Next search match" }).click();
    await expect(page.getByText("Search match 2 of 2", { exact: true })).toBeVisible();
    await expect
      .poll(() => text.evaluate((element) => element.scrollTop))
      .toBeGreaterThan(firstScroll);
    await text.evaluate((element) => element.scrollTo({ top: 0 }));
    await expect.poll(() => text.evaluate((element) => element.scrollTop)).toBe(0);
    await expect(text).toContainText("Background section 1");
    expect(errors).toEqual([]);
  } finally {
    await page.goto("about:blank");
    await docbank("daemon", "stop");
    await rm(workspace, { recursive: true, force: true });
  }
});
