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
    await mkdtemp(path.join(repository, ".superpowers", "taxes-proof-")),
  );
  try {
    await fixture.seed(1);
    const json = async (...args: string[]) => JSON.parse(await fixture.run(...args, "--json"));
    // Canonical parties reject ZZ, so they carry no country; tax rows use ZZ.
    const party = async (name: string, ...flags: string[]) =>
      (await json("party", "create", name, "--kind", "organization", ...flags)).party_id as string;
    // Long enough to wrap in the Title column; only its ending tells it apart.
    const longTitle =
      "Notice of assessment 2031 for the Example Ventures Ltd. payroll account, " +
      "second revised notice after the review of the April and May filings";
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
      ...["--title", longTitle, "--tax-year", "2031", "--currency", "JPY"],
      ...["--direction", "payable", "--amount", "5000"],
    );
    await add(
      ...["--account", account, "--kind", "filing", "--title", "Sales tax return, Q2 2031"],
      ...["--form-code", "Form 100", "--period-start", "2031-04-01", "--period-end", "2031-06-30"],
      ...["--due-date", "2031-07-31", "--direction", "unknown"],
    );
    // 9007199254740993 minor units is above 2^53 - 1, so the page withholds
    // the digits and shows this record's CLI command instead.
    const { record_id: unsafe } = await add(
      ...["--account", account, "--kind", "notice", "--title", "Refund notice 2031"],
      ...["--tax-year", "2031", "--due-date", "2032-03-31", "--direction", "refundable"],
      ...["--amount", "90071992547409.93"],
    );

    const url = new URL(await fixture.run("web", "--no-browser"));
    url.pathname = "/taxes";
    await page.setViewportSize({ width: 1440, height: 800 });
    const read = (pathname: string) =>
      page.waitForResponse((response) => new URL(response.url()).pathname === pathname);
    const [listed, named] = [read("/api/v1/tax/records"), read("/api/v1/parties")];
    await page.goto(url.href);
    // The browser session must be admitted to both list reads; a refusal is
    // a problem document, so report it instead of reading a missing page.
    for (const response of [await listed, await named])
      expect(response.status(), await response.text()).toBe(200);
    // Record IDs are random, and the list returns them in ID order, so the
    // expected row order comes from the page's own response, not creation
    // order. Filer headings are sorted by name.
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
    await expect(main.getByRole("heading", { level: 3 })).toHaveText([
      "Example Holdings Ltd.",
      "Example Ventures Ltd.",
    ]);
    for (const filer of [holdings, ventures])
      await expect(rowTitles(`Tax records for ${names[filer]}`)).toHaveText(titlesOf(filer));
    await expect(main.getByText("123400 USD", { exact: true })).toBeVisible();
    await expect(main.getByText("5000 JPY", { exact: true })).toBeVisible();
    // The long title is shown whole on several lines inside its fixed column,
    // and a short title still takes one line.
    const long = main.getByText(longTitle, { exact: true });
    await expect(long).toBeVisible();
    const lines = (text: string) =>
      main.getByText(text, { exact: true }).evaluate((span) => span.getClientRects().length);
    expect(await lines(longTitle)).toBeGreaterThan(1);
    expect(await lines("Sales tax return, Q1 2031")).toBe(1);
    const yearHeader = await main
      .getByRole("table", { name: "Tax records for Example Ventures Ltd.", exact: true })
      .getByRole("columnheader", { name: "Year" })
      .boundingBox();
    const title = await long.boundingBox();
    expect(title!.x + title!.width).toBeLessThanOrEqual(yearHeader!.x);
    await mkdir(output!, { recursive: true });
    await captureThemes(page, output!, "web-taxes-all");

    await main
      .getByRole("navigation", { name: "Tax filters" })
      .getByRole("button", { name: "Example Holdings Ltd." })
      .click();
    await expect(rowTitles("Tax records")).toHaveText(titlesOf(holdings));
    await expect(main.getByRole("heading", { level: 3 })).toHaveCount(0);
    await expect(main.getByText("Too large to show", { exact: true })).toBeVisible();
    await expect(
      main.getByText(`Run docbank tax record show ${unsafe} to see the exact amount`),
    ).toBeVisible();
    // Hover outranks the current-item fill, so show the selection as a user
    // sees it once the pointer leaves the rail.
    await page.mouse.move(0, 0);
    await captureThemes(page, output!, "web-taxes-filer");

    const year = main.getByLabel("Tax year");
    await year.fill("2032");
    await year.press("Enter");
    await expect(main.getByText("No records match these filters")).toBeVisible();
  } finally {
    await fixture.dispose();
  }
});
