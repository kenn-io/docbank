import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const run = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const binary = path.join(
  repository,
  "bin",
  process.platform === "win32" ? "docbank.exe" : "docbank",
);
const output = process.env.DOCBANK_INSPECTOR_CONTROLS_PROOF_DIR;
test.skip(!output, "DOCBANK_INSPECTOR_CONTROLS_PROOF_DIR enables the inspector controls proof");

test("keeps inspector tabs stable and tag removal accessible", async ({ page }) => {
  page.setDefaultTimeout(15_000);
  const workspace = await mkdtemp(
    path.join(process.platform === "win32" ? tmpdir() : "/tmp", "docbank-inspector-controls-"),
  );
  const vault = path.join(workspace, "vault");
  let running = false;
  const docbank = async (...args: string[]) =>
    (
      await run(binary, args, {
        cwd: repository,
        env: { ...process.env, DOCBANK_HOME: vault },
        timeout: 60_000,
      })
    ).stdout.trim();
  try {
    await mkdir(output!, { recursive: true });
    const source = path.join(workspace, "Review.txt");
    await writeFile(source, "Synthetic document for inspector controls.\n");
    await docbank("add", source, "--dest", "/");
    await docbank("tag", "create", "Review");
    await docbank("tag", "assign", "Review", "/Review.txt");
    const webURL = await docbank("web", "--no-browser");
    running = true;
    await page.goto(webURL);
    await page.getByRole("row").filter({ hasText: "Review.txt" }).click();
    const inspector = page.getByRole("complementary", { name: "Document inspector" });
    const details = inspector.locator("summary", { hasText: "Details" });
    await details.click();
    await page.mouse.move(0, 0);
    expect(await details.evaluate((element) => getComputedStyle(element).outlineStyle)).toBe(
      "none",
    );
    await page.keyboard.press("Tab");
    await page.keyboard.press("Shift+Tab");
    await expect(details).toBeFocused();
    expect(await details.evaluate((element) => element.matches(":focus-visible"))).toBe(true);
    expect(await details.evaluate((element) => getComputedStyle(element).outlineStyle)).toBe(
      "solid",
    );
    await page.screenshot({ path: path.join(output!, "details-keyboard.png") });
    await page.getByRole("button", { name: "Imports", exact: true }).click();
    const imports = page.getByRole("main", { name: "Imports" });
    await imports
      .getByRole("button", { name: /Browse import/ })
      .first()
      .click();
    const quality = imports.getByRole("button", { name: "Quality", exact: true });
    await quality.click();
    await page.mouse.move(0, 0);
    expect(await quality.evaluate((element) => getComputedStyle(element).outlineStyle)).toBe(
      "none",
    );
    await page.screenshot({ path: path.join(output!, "imports-mouse.png") });
    await page.getByRole("button", { name: "All files", exact: true }).click();

    const preview = inspector.locator(".original-preview pre");
    await expect(preview).toHaveText("Synthetic document for inspector controls.");
    await preview.evaluate((element) => element.setAttribute("data-retained", "yes"));
    let requests = 0;
    page.on("request", (request) => {
      if (/web-download|duplicates|attachments|renditions/.test(request.url())) requests++;
    });
    let releaseDuplicates!: () => void;
    const pendingDuplicates = new Promise<void>((resolve) => {
      releaseDuplicates = resolve;
    });
    await page.route("**/api/v1/duplicates/by-hash?*", async (route) => {
      await pendingDuplicates;
      await route.continue();
    });
    for (const name of ["Text", "Duplicates", "Attachments"]) {
      await inspector.getByRole("tab", { name, exact: true }).click();
      if (name === "Text")
        await expect(inspector.locator(".verified-text pre")).toHaveText(
          "Synthetic document for inspector controls.",
        );
      if (name === "Duplicates") {
        await expect(inspector.locator(".tab-content .kit-spinner:visible")).toHaveCount(1);
        await page.screenshot({ path: path.join(output!, "duplicates-pending.png") });
        releaseDuplicates();
        await expect(inspector.getByText("No duplicates", { exact: true })).toBeVisible();
      }
      if (name === "Attachments")
        await expect(
          inspector.getByText("No published attachments", { exact: true }),
        ).toBeVisible();
      await expect(inspector.locator(".tab-content .kit-spinner:visible")).toHaveCount(0);
    }
    const loadedRequests = requests;
    for (const name of ["Preview", "Text", "Duplicates", "Attachments", "Preview"]) {
      await inspector.getByRole("tab", { name, exact: true }).click();
      await expect(inspector.locator(".tab-content .kit-spinner:visible")).toHaveCount(0);
    }
    expect(requests).toBe(loadedRequests);
    await expect(preview).toHaveAttribute("data-retained", "yes");
    await page.screenshot({ path: path.join(output!, "preview-retained.png") });

    const remove = inspector.getByRole("button", { name: "Remove tag Review" });
    await expect(remove).toHaveCSS("opacity", "0");
    await inspector.locator(".assigned-tag").hover();
    await expect(remove).toHaveCSS("opacity", "1");
    await page.screenshot({ path: path.join(output!, "tag-hover.png") });
    await page.mouse.move(0, 0);
    for (let step = 0; step < 40; step++) {
      await page.keyboard.press("Tab");
      if (await remove.evaluate((element) => element === document.activeElement)) break;
    }
    await expect(remove).toBeFocused();
    await expect(remove).toHaveCSS("opacity", "1");
    await page.screenshot({ path: path.join(output!, "tag-keyboard.png") });
    await page.keyboard.press("Space");
    await expect(remove).toHaveCount(0);
    await page.goto(webURL);
    await page.getByRole("row").filter({ hasText: "Review.txt" }).click();
    await expect(inspector.getByRole("button", { name: "Add tag", exact: true })).toBeVisible();
  } finally {
    if (running) {
      await docbank("daemon", "stop");
      const status = JSON.parse(await docbank("daemon", "status", "--json"));
      expect(status.running).toBe(false);
    }
    await rm(workspace, { recursive: true, force: true });
  }
});
