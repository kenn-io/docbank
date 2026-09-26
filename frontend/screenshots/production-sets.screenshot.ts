import { expect, test } from "@playwright/test";
import { execFile, spawn } from "node:child_process";
import { cp, mkdir, mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const run = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const binary = path.join(repository, "docbank");
const output = process.env.DOCBANK_PRODUCTION_SCREENSHOT_DIR;
test.skip(!output, "DOCBANK_PRODUCTION_SCREENSHOT_DIR enables the synthetic production screenshot");

test("creates and inspects an exact production review through the real daemon", async ({ page }) => {
  test.setTimeout(180_000);
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-production-screenshot-"));
  const vault = path.join(workspace, "vault");
  const docbank = async (...args: string[]): Promise<string> => (await run(binary, args, {
    cwd: repository, env: { ...process.env, DOCBANK_HOME: vault }, timeout: 60_000, maxBuffer: 4 * 1024 * 1024,
  })).stdout.trim();
  try {
    await mkdir(output!, { recursive: true, mode: 0o700 });
    await docbank("production", "sets", "create", "--name", "Synthetic review", "--json");
    const webURL = await docbank("web", "--no-browser");
    await page.setViewportSize({ width: 1440, height: 1100 });
    await page.addInitScript(() => localStorage.setItem("docbank-theme", "dark"));
    await page.goto(webURL, { waitUntil: "domcontentloaded" });
    await page.getByRole("button", { name: "Production sets" }).click();
    const drawer = page.getByRole("dialog", { name: "Production sets" });
    await expect(drawer.getByRole("button", { name: "Synthetic review" })).toBeVisible();
    await drawer.getByRole("textbox", { name: "Set name" }).fill("Synthetic follow-up");
    await drawer.getByRole("textbox", { name: "Instructions" }).fill("Review selected synthetic pages.");
    await drawer.getByRole("button", { name: "Create draft" }).click();
    await expect(drawer.getByText("Draft revision 1")).toBeVisible();
    await expect(drawer.getByText("Membership open")).toBeVisible();
    await expect(drawer.getByText("No members yet")).toBeVisible();
    await expect(drawer.getByText("No flagged passages")).toBeVisible();
    await page.screenshot({ path: path.join(output!, "web-production-review.png"), fullPage: true, animations: "disabled" });
  } finally {
    try { await docbank("daemon", "stop"); } catch { /* daemon may not have started */ }
    await rm(workspace, { recursive: true, force: true });
    await expect(stat(workspace)).rejects.toMatchObject({ code: "ENOENT" });
  }
});

test("selects a rectangle on a retained synthetic member PDF through the real daemon", async ({ page }) => {
  test.setTimeout(300_000);
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-editor-screenshot-"));
  const ready = path.join(workspace, "fixture-ready.json");
  const done = path.join(workspace, "fixture-done");
  const vault = path.join(workspace, "vault");
  const fixture = spawn("go", ["test", "-tags", "fts5", "./internal/store", "-run", "^TestProductionEditorScreenshotFixture$", "-count=1"], {
    cwd: repository, env: { ...process.env, GOTOOLCHAIN: "go1.27.0",
      DOCBANK_PRODUCTION_EDITOR_FIXTURE_READY: ready, DOCBANK_PRODUCTION_EDITOR_FIXTURE_DONE: done },
  });
  let fixtureOutput = "";
  fixture.stdout.on("data", chunk => { fixtureOutput += String(chunk); });
  fixture.stderr.on("data", chunk => { fixtureOutput += String(chunk); });
  const fixtureExit = new Promise<number | null>(resolve => fixture.on("exit", resolve));
  let daemonStarted = false;
  const docbank = async (...args: string[]): Promise<string> => (await run(binary, args, {
    cwd: repository, env: { ...process.env, DOCBANK_HOME: vault }, timeout: 60_000, maxBuffer: 4 * 1024 * 1024,
  })).stdout.trim();
  try {
    await expect.poll(async () => {
      try { return JSON.parse(await readFile(ready, "utf8")) as { root: string; set_id: string }; }
      catch { return null; }
    }, { timeout: 180_000, message: `synthetic fixture did not appear: ${fixtureOutput}` }).not.toBeNull();
    const source = JSON.parse(await readFile(ready, "utf8")) as { root: string; set_id: string };
    await cp(source.root, vault, { recursive: true });
    await writeFile(done, "copied", { mode: 0o600 });
    expect(await fixtureExit, fixtureOutput).toBe(0);

    await mkdir(output!, { recursive: true, mode: 0o700 });
    const webURL = await docbank("web", "--no-browser");
    daemonStarted = true;
    await page.setViewportSize({ width: 1440, height: 1100 });
    await page.addInitScript(() => localStorage.setItem("docbank-theme", "dark"));
    const outside: string[] = [];
    const browserErrors: string[] = [];
    page.on("request", request => {
      if (/^https?:/.test(request.url()) && new URL(request.url()).origin !== new URL(webURL).origin) outside.push(request.url());
    });
    page.on("pageerror", error => browserErrors.push(error.message));
    page.on("console", message => { if (message.type() === "error") browserErrors.push(message.text()); });
    page.on("response", response => { if (response.status() >= 400) browserErrors.push(`${response.status()} ${response.url()}`); });
    await page.goto(webURL, { waitUntil: "domcontentloaded" });
    await page.getByRole("button", { name: "Production sets" }).click();
    const drawer = page.getByRole("dialog", { name: "Production sets" });
    await drawer.getByRole("button", { name: "Synthetic preview" }).click();
    await expect(drawer.getByRole("button", { name: "Open original PDF for member 1" })).toBeVisible();
    await drawer.getByRole("button", { name: "Open original PDF for member 1" }).click();
    await expect(drawer.getByRole("region", { name: "Original PDF for member 1" })).toBeVisible();
    try { await expect(drawer.getByRole("img", { name: "Original page 1" })).toBeVisible(); }
    catch (error) {
      const dom = await drawer.getByRole("region", { name: "Original PDF for member 1" }).evaluate(element => element.outerHTML.slice(0, 5000));
      throw new Error(`${String(error)}\nBrowser errors: ${browserErrors.join(" | ")}\nViewer DOM: ${dom}`);
    }
    await expect(drawer.getByRole("combobox", { name: "PDF page" })).toBeVisible();
    await drawer.getByRole("button", { name: "Select whole page", exact: true }).click();
    await expect(drawer.getByRole("region", { name: "Selected region on page 1" })).toBeVisible();
    await drawer.getByRole("button", { name: "Clear selection" }).click();
    const pageBox = await drawer.getByRole("img", { name: "Original page 1" }).boundingBox();
    expect(pageBox).not.toBeNull();
    await page.mouse.move(pageBox!.x + 8, pageBox!.y + 10);
    await page.mouse.down();
    await page.mouse.move(pageBox!.x + 38, pageBox!.y + 42, { steps: 5 });
    await page.mouse.up();
    try { await expect(drawer.getByRole("region", { name: "Selected region on page 1" })).toBeVisible(); }
    catch (error) {
      const dom = await drawer.getByRole("region", { name: "Original PDF for member 1" }).evaluate(element => element.outerHTML.slice(0, 8000));
      throw new Error(`${String(error)}\nBrowser errors: ${browserErrors.join(" | ")}\nViewer DOM: ${dom}`);
    }
    const selectionBox = await drawer.getByRole("region", { name: "Selected region on page 1" }).boundingBox();
    const visiblePageBox = await drawer.getByRole("img", { name: "Original page 1" }).boundingBox();
    expect(Math.abs(selectionBox!.y - visiblePageBox!.y)).toBeLessThan(500);
    expect(selectionBox!.height).toBeLessThan(360);
    const overlay = drawer.locator(".source-selection-overlay");
    await expect(overlay).toBeVisible();
    const overlayBox = await overlay.boundingBox();
    expect(overlayBox!.width).toBeGreaterThan(5);
    expect(overlayBox!.width).toBeLessThan(visiblePageBox!.width);
    expect(outside).toEqual([]);
    expect(browserErrors).toEqual([]);
    await page.screenshot({ path: path.join(output!, "web-production-rectangle-selection.png"), fullPage: true, animations: "disabled" });
  } finally {
    await writeFile(done, "stopped", { mode: 0o600 });
    if (daemonStarted) await docbank("daemon", "stop");
    if (fixture.exitCode === null) fixture.kill();
    await fixtureExit;
    await rm(workspace, { recursive: true, force: true });
    await expect(stat(workspace)).rejects.toMatchObject({ code: "ENOENT" });
  }
});
