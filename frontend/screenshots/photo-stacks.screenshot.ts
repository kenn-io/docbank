import { expect, test, type Page } from "@playwright/test";
import { mkdir, mkdtemp } from "node:fs/promises";
import path from "node:path";
import { photoFixture, repository } from "./photo-fixture.js";

const output = process.env.DOCBANK_STACKS_SCREENSHOT_DIR;
test.skip(!output, "DOCBANK_STACKS_SCREENSHOT_DIR enables synthetic burst proof");

async function captureThemes(page: Page, name: string) {
  for (const theme of ["light", "dark"]) {
    await page.evaluate(async (value) => {
      document.documentElement.classList.toggle("dark", value === "dark");
      await document.fonts.ready;
    }, theme);
    await page.screenshot({
      path: path.join(output!, `${name}-${theme}.png`),
      animations: "disabled",
    });
  }
}

test("collapses bursts, selects members, changes the gap and culls stacks", async ({ page }) => {
  test.setTimeout(300_000);
  await mkdir(path.join(repository, ".superpowers"), { recursive: true });
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "stack-proof-"));
  const fixture = photoFixture(workspace);
  try {
    await fixture.seed(12, "-bursts");
    const url = new URL(await fixture.run("web", "--no-browser"));
    url.pathname = "/photos";
    page.on("dialog", (dialog) => dialog.accept());
    await page.goto(url.href);
    await expect(page.locator(".photo-cell")).toHaveCount(4);
    await expect(page.locator(".photo-cell img").first()).toBeVisible();
    const preparation = page.waitForResponse(
      (response) =>
        response.url().endsWith("/photos/authored/prepare") &&
        response.request().postDataJSON().stack,
    );
    await page.locator(".photo-image").first().click();
    await expect(page.getByText("3 selected photos", { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Keep all", exact: true })).toBeVisible();
    await mkdir(output!, { recursive: true });
    const prepared = await preparation;
    const { targets } = await prepared.json();
    const { asset_id, asset_revision, file_id, revision, node_revision } = targets[0];
    const externalEdit = await page.request.post(
      new URL("/api/v1/photos/authored/edit", url).href,
      {
        headers: {
          "X-Docbank-Web-Session": prepared.request().headers()["x-docbank-web-session"]!,
          Origin: url.origin,
        },
        data: {
          targets: [{ asset_id, asset_revision, file_id, revision, node_revision }],
          patch: { flag: "pick" },
        },
      },
    );
    expect(externalEdit.ok(), await externalEdit.text()).toBe(true);
    await page.getByRole("button", { name: "Keep all", exact: true }).click();
    await expect(page.getByRole("button", { name: "Reload stack", exact: true })).toBeVisible();
    await captureThemes(page, "web-photo-stack-conflict");
    await page.getByRole("button", { name: "Reload stack", exact: true }).click();
    await expect(page.getByRole("button", { name: "Reload stack", exact: true })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Keep all", exact: true })).toBeEnabled();
    await expect(page.getByText("3 selected photos", { exact: true })).toBeVisible();
    await captureThemes(page, "web-photo-stack-reloaded");
    await page.screenshot({
      path: path.join(output!, "web-photo-stacks.png"),
      animations: "disabled",
    });
    await page.getByRole("button", { name: /Add to album/ }).click();
    await page.getByRole("combobox", { name: "Find or create an album" }).fill("Burst picks");
    await page.getByRole("option", { name: 'Create album "Burst picks"' }).click();
    await expect(page.getByText("Added to Burst picks")).toBeVisible();
    await page.getByRole("button", { name: "Clear selection", exact: true }).click();
    const added = page.waitForResponse((response) => response.url().endsWith("/members/add"));
    await page
      .locator(".photo-image")
      .first()
      .dragTo(
        page
          .getByRole("navigation", { name: "Docbank navigation" })
          .getByRole("button", { name: /Burst picks/ }),
      );
    const add = await added;
    expect(add.ok(), await add.text()).toBe(true);
    expect(add.request().postDataJSON().asset_ids).toHaveLength(3);
    await page.screenshot({
      path: path.join(output!, "web-photo-stack-album.png"),
      animations: "disabled",
    });
    await page.getByRole("button", { name: "Expand stack of 3 photos" }).first().click();
    await expect(page.locator(".photo-cell")).toHaveCount(6);
    await page.locator(".photo-image").nth(1).click();
    await expect(page.getByText("1 selected photo", { exact: true })).toBeVisible();
    const chosen = await page.locator(".photo-cell img").nth(1).getAttribute("alt");
    const decided = page.waitForResponse((response) =>
      response.url().endsWith("/photos/authored/stack"),
    );
    await page.getByRole("button", { name: "Choose representative", exact: true }).click();
    const decision = await decided;
    expect(decision.ok(), await decision.text()).toBe(true);
    // The choice is stored separately; no frame's flag changes.
    const receipt = await decision.json();
    expect(receipt.after).toEqual([]);
    expect(receipt.representatives).toHaveLength(1);
    await expect(
      page.getByRole("button", { name: "Undo last photo edit", exact: true }),
    ).toBeVisible();
    const gap = page.getByRole("spinbutton", { name: "Burst gap (seconds)" });
    await gap.fill("1");
    await gap.dispatchEvent("change");
    await expect(page.locator(".photo-cell")).toHaveCount(12);
    await gap.fill("2");
    await gap.dispatchEvent("change");
    await expect(page.locator(".photo-cell")).toHaveCount(4);
    await expect(page.locator(".photo-cell img").first()).toHaveAttribute("alt", chosen!);
    await page.screenshot({
      path: path.join(output!, "web-photo-stack-representative.png"),
      animations: "disabled",
    });
    await page.route("**/photos/authored/prepare", async (route) => {
      if (route.request().postDataJSON().stack) {
        await route.abort("failed");
        await page.unroute("**/photos/authored/prepare");
      } else await route.continue();
    });
    await page.getByRole("button", { name: "Cull", exact: true }).click();
    const cull = page.getByRole("dialog", { name: "Cull photos" });
    await expect(
      cull.getByRole("button", { name: "Keep current (Return)", exact: true }),
    ).toBeVisible();
    await expect(cull.locator(".photo-stage img")).toBeVisible();
    await expect(cull.getByRole("button", { name: "Retry", exact: true })).toBeVisible();
    await captureThemes(page, "web-photo-cull-preparation-retry");
    await cull.getByRole("button", { name: "Retry", exact: true }).click();
    await expect(
      cull.getByRole("button", { name: "Keep current (Return)", exact: true }),
    ).toBeEnabled();
    const first = await cull.locator("header strong").innerText();
    await page.keyboard.press("j");
    await expect(cull.locator("header strong")).not.toHaveText(first);
    await page.keyboard.press("k");
    await expect(cull.locator("header strong")).toHaveText(first);
    await page.keyboard.press("l");
    await expect(cull.locator("header strong")).not.toHaveText(first);
    await expect(
      cull.getByRole("button", { name: "Keep current (Return)", exact: true }),
    ).toBeEnabled();
    await page.keyboard.press("h");
    await expect(cull.locator("header strong")).toHaveText(first);
    await expect(
      cull.getByRole("button", { name: "Keep current (Return)", exact: true }),
    ).toBeEnabled();
    await expect(cull.locator(".stack-divider").first()).toBeVisible();
    await captureThemes(page, "web-photo-cull-prepared");
    await page.screenshot({
      path: path.join(output!, "web-photo-cull-stacks.png"),
      animations: "disabled",
    });
    const kept = page.waitForResponse(
      (response) => response.url().endsWith("/photos/authored/stack"),
      { timeout: 15000 },
    );
    await page.keyboard.press("Enter");
    const keptResponse = await kept;
    expect(keptResponse.ok(), await keptResponse.text()).toBe(true);
    await expect(cull.getByRole("button", { name: "Undo", exact: true })).toBeVisible();
    await expect(cull.locator("header strong")).not.toHaveText(first);
    await cull.getByRole("button", { name: "Undo", exact: true }).click();
    await cull.getByRole("button", { name: "Exit cull", exact: true }).click();
    await page.getByRole("button", { name: "Resume cull", exact: true }).click();
    await expect(
      cull.getByRole("button", { name: "Keep current (Return)", exact: true }),
    ).toBeVisible();
  } finally {
    await fixture.dispose();
  }
});
