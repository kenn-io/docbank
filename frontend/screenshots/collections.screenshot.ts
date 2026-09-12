import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const screenshots = process.env.DOCBANK_SCREENSHOT_DIR;
if (!screenshots) throw new Error("DOCBANK_SCREENSHOT_DIR is required");

test("import collections, label conflicts and stable document navigation", async ({ page }) => {
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-collections-"));
  const vault = path.join(workspace, "vault");
  const run = async (...args: string[]) => (await exec(path.join(repository, "docbank"), args, {
    cwd: repository, env: { ...process.env, DOCBANK_HOME: vault }, timeout: 60_000,
  })).stdout.trim();
  try {
    await mkdir(screenshots, { recursive: true, mode: 0o700 });
    const source = path.join(workspace, "Discovery");
    await mkdir(source, { mode: 0o700 });
    await writeFile(path.join(source, "review-notes.txt"), "Synthetic notes: compare alpha and beta proposals.\n", { mode: 0o600 });
    await writeFile(path.join(source, "meeting-summary.txt"), "Synthetic meeting: review the two proposals on Friday.\n", { mode: 0o600 });
    await run("add", source, "--dest", "/");
    const webURL = new URL(await run("web", "--no-browser"));
    if (webURL.protocol !== "http:" || !/^docbank-[0-9a-f]{32}\.localhost$/.test(webURL.hostname) ||
      !webURL.port || webURL.pathname !== "/" || webURL.username || webURL.password || webURL.search) {
      throw new Error("Unexpected synthetic browser origin");
    }
    const session = new URLSearchParams(webURL.hash.slice(1)).get("web_session");
    if (!session) throw new Error("Missing synthetic browser session");
    // Keep scoped credentials in memory; neither URLs nor headers are printed.
    const api = async (route: string, init: RequestInit = {}) => {
      const response = await fetch(`http://127.0.0.1:${webURL.port}${route}`, {
        ...init, headers: { Host: webURL.host, "X-Docbank-Web-Session": session, ...init.headers },
      });
      if (!response.ok) throw new Error(`Synthetic collection request failed: ${response.status}`);
      return response;
    };
    const listed = await (await api("/api/v1/collections?limit=100&offset=0")).json() as { items: { id: string }[] };
    expect(listed.items).toHaveLength(1);
    const collectionID = listed.items[0]!.id;
    const labelRoute = `/api/v1/collections/${collectionID}/label`;
    const initial = await api(labelRoute);
    await api(labelRoute, {
      method: "PUT", headers: { "Content-Type": "application/json", "If-Match": initial.headers.get("ETag")! },
      body: JSON.stringify({ label: "Discovery batch" }),
    });

    await page.addInitScript(() => localStorage.setItem("docbank-theme", "dark"));
    await page.goto(webURL.toString()).catch(() => {
      throw new Error("Synthetic browser navigation failed");
    });
    await page.getByRole("button", { name: "Import collections", exact: true }).click();
    const drawer = page.getByRole("dialog", { name: "Import collections" });
    await drawer.getByRole("button", { name: "Browse collection Discovery batch", exact: true }).click();
    await expect(drawer.getByRole("button", { name: "Open document /Discovery/review-notes.txt", exact: true })).toBeVisible();
    await expect(drawer.getByText(/Quality.*unavailable/i)).toBeVisible();
    await drawer.getByRole("textbox", { name: "Collection label", exact: true }).fill("Proposal review");
    await drawer.getByRole("button", { name: "Save label", exact: true }).click();
    await expect(drawer.getByRole("button", { name: "Browse collection Proposal review", exact: true })).toBeVisible();
    expect(new URL(page.url()).hash).not.toContain("web_session");
    await page.screenshot({ path: path.join(screenshots, "web-collections.png"), animations: "disabled" });

    // An independent API writer advances the same label revision.
    const beforeConflict = await api(labelRoute);
    await api(labelRoute, {
      method: "PUT", headers: { "Content-Type": "application/json", "If-Match": beforeConflict.headers.get("ETag")! },
      body: JSON.stringify({ label: "Externally labeled" }),
    });
    await drawer.getByRole("textbox", { name: "Collection label", exact: true }).fill("Keep this draft");
    await drawer.getByRole("button", { name: "Save label", exact: true }).click();
    await expect(drawer.getByRole("alert")).toBeVisible();
    await expect(drawer.getByRole("textbox", { name: "Collection label", exact: true })).toHaveValue("Keep this draft");
    await page.screenshot({ path: path.join(screenshots, "web-collection-label-conflict.png"), animations: "disabled" });
    await drawer.getByRole("button", { name: "Reload label", exact: true }).click();
    await expect(drawer.getByRole("button", { name: "Clear label", exact: true })).toBeEnabled();
    await drawer.getByRole("button", { name: "Clear label", exact: true }).click();
    await expect.poll(async () => (await (await api(labelRoute)).json()).label).toBeNull();
    await drawer.getByRole("button", { name: "Open document /Discovery/review-notes.txt", exact: true }).click();
    await expect(drawer).not.toBeVisible();
    await expect(page.getByRole("cell", { name: "review-notes.txt", exact: true })).toBeVisible();
    await expect(page.getByRole("heading", { name: "review-notes.txt", exact: true })).toBeVisible();
  } finally {
    await run("daemon", "stop");
    const status = JSON.parse(await run("daemon", "status", "--json"));
    if (status.running !== false) throw new Error("Synthetic collections daemon did not stop; workspace retained.");
    await rm(workspace, { recursive: true, force: true });
  }
});
