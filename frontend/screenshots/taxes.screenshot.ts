import { expect, test } from "@playwright/test";
import { mkdir, mkdtemp } from "node:fs/promises";
import path from "node:path";
import type { TaxRecordPage } from "../src/generated/docbank.js";
import { captureThemes } from "./capture-themes.js";
import { photoFixture, repository } from "./photo-fixture.js";

const output = process.env.DOCBANK_TAXES_SCREENSHOT_DIR;
test.skip(!output, "DOCBANK_TAXES_SCREENSHOT_DIR enables synthetic Taxes proof");

test("Taxes page groups synthetic records by filer", async ({ page }) => {
  test.setTimeout(300_000);
  await mkdir(path.join(repository, ".superpowers"), { recursive: true });
  const fixture = photoFixture(
    process.env.DOCBANK_TAXES_WORKSPACE ??
      (await mkdtemp(path.join(repository, ".superpowers", "taxes-proof-"))),
  );
  try {
    await fixture.seed(1);
    const json = async (...args: string[]) => JSON.parse(await fixture.run(...args, "--json"));
    // Canonical parties reject ZZ, so they carry no country; tax rows use ZZ.
    const party = async (name: string, ...flags: string[]) =>
      (await json("party", "create", name, "--kind", "organization", ...flags)).party_id as string;
    const holdings = await party("Example Holdings Ltd.", "--own");
    const ventures = await party("Example Ventures Ltd.", "--own");
    const agency = await party("Example Revenue Agency");
    const { account_id: account } = await json(
      ...["tax", "account", "add", "--filer", holdings, "--authority", agency, "--country", "ZZ"],
      ...["--tax-kind", "sales-tax", "--cadence", "quarterly", "--currency", "USD"],
    );
    const add = (...flags: string[]) => json("tax", "record", "add", ...flags);
    await add(
      ...["--account", account, "--kind", "filing", "--title", "Sales tax return, Q1 2031"],
      ...["--form-code", "Form 100", "--period-start", "2031-01-01", "--period-end", "2031-03-31"],
      ...["--due-date", "2031-04-30", "--direction", "payable", "--amount", "1234.00"],
    );
    await add(
      ...["--filer", ventures, "--country", "ZZ", "--tax-kind", "payroll", "--kind", "notice"],
      ...["--title", "Notice of assessment 2031", "--tax-year", "2031", "--currency", "JPY"],
      ...["--direction", "payable", "--amount", "5000"],
    );
    await add(
      ...["--account", account, "--kind", "filing", "--title", "Sales tax return, Q2 2031"],
      ...["--form-code", "Form 100", "--period-start", "2031-04-01", "--period-end", "2031-06-30"],
      ...["--due-date", "2031-07-31", "--direction", "unknown"],
    );
    await add(
      ...["--account", account, "--kind", "notice", "--title", "Refund notice 2031"],
      ...["--tax-year", "2031", "--due-date", "2032-03-31", "--direction", "refundable"],
      ...["--amount", "250.00"],
    );

    const url = new URL(await fixture.run("web", "--no-browser"));
    url.pathname = "/taxes";
    await page.setViewportSize({ width: 1440, height: 800 });
    const listed = page.waitForResponse(
      (response) => new URL(response.url()).pathname === "/api/v1/tax/records",
    );
    await page.goto(url.href);
    // Record IDs are random, and the list returns them in ID order, so the
    // expected order comes from the page's own response, not creation order.
    const { items } = (await (await listed).json()) as TaxRecordPage;
    expect(items).toHaveLength(4);
    const names: Record<string, string> = {
      [holdings]: "Example Holdings Ltd.",
      [ventures]: "Example Ventures Ltd.",
    };
    const titlesOf = (filer: string) =>
      items.filter((item) => item.filer_party_id === filer).map((item) => item.title);
    const main = page.getByRole("main", { name: "Taxes" });
    const rowTitles = (name: string) =>
      main
        .getByRole("table", { name, exact: true })
        .locator("tbody tr td:first-child > span:first-child");
    await expect(main.locator("tbody tr")).toHaveCount(4);
    await expect(main.getByRole("heading", { level: 3 })).toHaveText(
      [...new Set(items.map((item) => item.filer_party_id))].map((id) => names[id]!),
    );
    for (const filer of [holdings, ventures])
      await expect(rowTitles(`Tax records for ${names[filer]}`)).toHaveText(titlesOf(filer));
    await expect(main.getByText("123400 USD", { exact: true })).toBeVisible();
    await expect(main.getByText("5000 JPY", { exact: true })).toBeVisible();
    await mkdir(output!, { recursive: true });
    await captureThemes(page, output!, "web-taxes-all");

    await main
      .getByRole("navigation", { name: "Tax filters" })
      .getByRole("button", { name: "Example Holdings Ltd." })
      .click();
    await expect(rowTitles("Tax records")).toHaveText(titlesOf(holdings));
    await expect(main.getByRole("heading", { level: 3 })).toHaveCount(0);
    await captureThemes(page, output!, "web-taxes-filer");

    const year = main.getByLabel("Tax year");
    await year.fill("2032");
    await year.press("Enter");
    await expect(main.getByText("No records match these filters")).toBeVisible();
  } finally {
    await fixture.dispose();
  }
});
