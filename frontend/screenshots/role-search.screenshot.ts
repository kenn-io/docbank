import { expect, test, type Locator, type Page, type TestInfo } from "@playwright/test";
import { execFile } from "node:child_process";
import { access, mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { inspect, promisify } from "node:util";
import { fileURLToPath } from "node:url";
import { APIError } from "../src/api-transport.js";
import {
  createWorkspaceQuery,
  type WorkspaceQueryCreateRequestFacetsItem,
} from "../src/generated/docbank.js";
import type { SnapshotPage } from "../src/snapshots.js";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const proof = process.env.DOCBANK_ROLE_SEARCH_PROOF;
test.skip(!proof, "DOCBANK_ROLE_SEARCH_PROOF enables this proof");

// Invented people from the approved prototype; every address is reserved.
const mara = '"Mara Okonkwo-Fell" <mara@okonkwo.example.test>';
const ines = "=?UTF-8?Q?Ines_Albarr=C3=A1n?= <ines@albarran.example.test>";
const teodor = '"Teodor Vask" <teodor@vask.example.test>';
const jory = '"Jory Pellham" <jory@pellham.example.test>';
const hidden = '"Hidden Recipient" <hidden@example.test>';
const emails: [string, Record<string, string>, string][] = [
  [
    "board-update.eml",
    { From: mara, To: ines, Cc: teodor, Bcc: hidden, Date: "Wed, 4 Mar 2026 09:15:00 +0000" },
    "Board update for the March meeting.",
  ],
  [
    "budget-follow-up.eml",
    { From: mara, To: teodor, Cc: jory, Date: "Thu, 12 Mar 2026 14:30:00 +0000" },
    "Budget follow-up with the revised figures.",
  ],
  [
    "site-visit.eml",
    { From: ines, To: mara, Date: "Mon, 16 Mar 2026 08:00:00 +0000" },
    "Site visit notes.",
  ],
  [
    "vendor-notes.eml",
    { From: jory, To: mara, Cc: ines, Date: "Fri, 20 Mar 2026 16:45:00 +0000" },
    "Vendor notes for review.",
  ],
];
const advanced = (text: string, filters: Record<string, unknown> = {}) => ({
  v: 1 as const,
  text,
  syntax: "advanced" as const,
  mode: "lexical" as const,
  filters,
  sort: { field: "name", direction: "asc" } as const,
});
const query = advanced('from:"Mara Okonkwo-Fell"');

// Attachments are named
// role-search-branch-<state>-<wide|narrow|mobile-<part>>-<theme>.png.
async function capture(page: Page, testInfo: TestInfo, name: string, theme: "light" | "dark") {
  await page.evaluate(async (value) => {
    localStorage.setItem("docbank-theme", value);
    document.documentElement.classList.toggle("dark", value === "dark");
    await document.fonts.ready;
  }, theme);
  // Park the pointer away from controls after scrolling each view.
  const corner = await page.evaluate(() =>
    [
      [0, 0],
      [innerWidth - 1, 0],
      [0, innerHeight - 1],
      [innerWidth - 1, innerHeight - 1],
    ].find(
      ([x, y]) =>
        !document
          .elementFromPoint(x!, y!)
          ?.closest(
            "a[href], button, input, label, select, summary, textarea, tr, [role=button], [role=link], [role=tab]",
          ),
    ),
  );
  expect(corner).toBeDefined();
  await page.mouse.move(corner![0]!, corner![1]!);
  if (name.endsWith("-wide")) {
    const row = page
      .getByRole("region", { name: "Frozen query results" })
      .locator("tbody tr")
      .first();
    const title = row.locator(".title");
    await expect(row.locator(".col-modified")).toBeVisible();
    const chip = row.locator(".roles .kit-chip").first();
    const titleBox = await title.boundingBox();
    const chipBox = await chip.boundingBox();
    expect(titleBox).not.toBeNull();
    expect(chipBox).not.toBeNull();
    expect(Math.abs(titleBox!.y - chipBox!.y)).toBeLessThan(5);
    expect(await title.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(
      true,
    );
    const titles = page
      .getByRole("region", { name: "Frozen query results" })
      .locator("tbody .title");
    for (const element of await titles.all()) {
      expect(await element.evaluate((title) => title.scrollWidth <= title.clientWidth)).toBe(true);
    }
    const contrast = await chip.evaluate((element) => {
      const color = (value: string) =>
        value
          .match(/[\d.]+/g)!
          .slice(0, 3)
          .map(Number);
      const luminance = (rgb: number[]) =>
        rgb
          .map((v) => {
            const channel = v / 255;
            return channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4;
          })
          .reduce((sum, value, i) => sum + value * [0.2126, 0.7152, 0.0722][i]!, 0);
      const style = getComputedStyle(element);
      const a = luminance(color(style.color));
      const b = luminance(color(style.backgroundColor));
      return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
    });
    expect(contrast).toBeGreaterThanOrEqual(4.5);
  }
  if (name.endsWith("-narrow")) {
    const aside = await page.getByRole("complementary", { name: "Snapshot facets" }).boundingBox();
    const people = await page.getByRole("group", { name: "People facet" }).boundingBox();
    const results = await page.getByRole("region", { name: "Frozen query results" }).boundingBox();
    expect(aside).not.toBeNull();
    expect(people).not.toBeNull();
    expect(results).not.toBeNull();
    if (
      (await page
        .getByRole("complementary", { name: "Snapshot facets" })
        .locator("section")
        .count()) === 1
    ) {
      expect(aside!.width - people!.width).toBeLessThan(24);
    }
    expect(results!.y - (aside!.y + aside!.height)).toBeLessThan(24);
  }
  // The JSON reporter embeds the body as base64 in the uploaded CI report.
  const body = await page.screenshot({ animations: "disabled" });
  await testInfo.attach(`role-search-${proof}-${name}-${theme}.png`, {
    body,
    contentType: "image/png",
  });
}

// The first result is selected on load, so the inspector reads its email and
// history; a capture taken earlier shows their loading states.
async function inspectorLoaded(page: Page) {
  const inspector = page.getByRole("complementary", { name: "Document inspector" });
  await expect(
    inspector.getByRole("region", { name: "Email reader" }).getByLabel("Decoded email headers"),
  ).toBeAttached();
  await expect(
    inspector.getByRole("region", { name: "History", exact: true }).locator(":scope > ol"),
  ).toBeAttached();
  await expect(page.getByText("Loading and verifying the email…")).toHaveCount(0);
  await expect(page.getByText("Loading history…")).toHaveCount(0);
}

async function captureStates(
  page: Page,
  testInfo: TestInfo,
  state: string,
  ready: () => Promise<void>,
  parts: [string, Locator][],
) {
  await page.setViewportSize({ width: 1440, height: 960 });
  await ready();
  await inspectorLoaded(page);
  await capture(page, testInfo, `${state}-wide`, "dark");
  await capture(page, testInfo, `${state}-wide`, "light");
  await page.setViewportSize({ width: 860, height: 1100 });
  await ready();
  await inspectorLoaded(page);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await capture(page, testInfo, `${state}-narrow`, "dark");
  await capture(page, testInfo, `${state}-narrow`, "light");
  // Phone width puts the facets in one column above the results. The app shell
  // scrolls inside a fixed-height frame, so each part is brought wholly into view.
  await page.setViewportSize({ width: 390, height: 844 });
  await ready();
  await inspectorLoaded(page);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  for (const [part, target] of parts) {
    await target.evaluate((element) => element.scrollIntoView({ block: "start" }));
    await expect(target).toBeInViewport({ ratio: 0.99 });
    await capture(page, testInfo, `${state}-mobile-${part}`, "dark");
    await capture(page, testInfo, `${state}-mobile-${part}`, "light");
  }
  await page.setViewportSize({ width: 1440, height: 960 });
}

test("finds documents by sender with role chips and a People facet", async ({ page }, testInfo) => {
  test.setTimeout(240_000);
  const binary = process.env.DOCBANK_SCREENSHOT_BINARY;
  if (!binary || !path.isAbsolute(binary)) {
    throw new Error("DOCBANK_SCREENSHOT_BINARY must name an absolute branch binary");
  }
  await access(binary);
  const scratch = await mkdtemp(path.join(tmpdir(), "docbank-role-search-"));
  const vault = path.join(scratch, "vault");
  const run = async (...args: string[]) =>
    (
      await exec(binary, args, {
        cwd: repository,
        env: { ...process.env, DOCBANK_HOME: vault },
        timeout: 90_000,
      })
    ).stdout.trim();
  let started = false;
  let failure: unknown;
  const originalFetch = globalThis.fetch;
  let baseURL = "";
  globalThis.fetch = (input, init) =>
    originalFetch(
      typeof input === "string" && input.startsWith("/") ? baseURL + input : input,
      init,
    );
  const stop = async () => {
    if (!started) return;
    const before = JSON.parse(await run("daemon", "status", "--json"));
    if (before.running) await run("daemon", "stop");
    const after = JSON.parse(await run("daemon", "status", "--json"));
    if (after.running !== false) {
      throw new Error(
        `Synthetic daemon ${before.pid ?? "unknown"} did not stop; retained ${scratch}`,
      );
    }
    started = false;
  };
  try {
    await mkdir(vault, { mode: 0o700 });
    for (const [name, headers, body] of emails) {
      const source = path.join(scratch, name);
      const lines = Object.entries({ ...headers, Subject: name.replace(".eml", "") }).map(
        ([key, value]) => `${key}: ${value}`,
      );
      await writeFile(source, `${lines.join("\r\n")}\r\n\r\n${body}\r\n`, { mode: 0o600 });
      started = true; // add may start the daemon even if the command fails.
      await run("add", source, "--dest", "/");
    }
    const issued = new URL(await run("web", "--no-browser"));
    const session = new URLSearchParams(issued.hash.slice(1)).get("web_session");
    if (!session) throw new Error("Synthetic web session missing");
    baseURL = `http://127.0.0.1:${issued.port}`;
    // People are provisioned from the addresses in the background; wait for the
    // browser session's own frozen result before driving the page.
    const freeze = async (
      value: ReturnType<typeof advanced>,
      facets: WorkspaceQueryCreateRequestFacetsItem[],
    ): Promise<SnapshotPage | undefined> => {
      const response = await createWorkspaceQuery(
        { query: value, page_size: 50, facets },
        { session, signal: AbortSignal.timeout(30_000), headers: { Host: issued.host } },
      ).catch((cause: unknown) => {
        if (cause instanceof APIError) return undefined;
        throw cause;
      });
      return response ? ((await response.json()) as SnapshotPage) : undefined;
    };
    const summary = (page?: SnapshotPage) => ({
      rows: page?.rows.map((row) => [row.name, row.matched_roles]),
      people: Object.fromEntries(
        page?.facets[0]?.values.map((value) => [value.label, [value.count, value.roles]]) ?? [],
      ),
    });
    const results = page.getByRole("region", { name: "Frozen query results" });
    const people = page.getByRole("group", { name: "People facet" });
    const parts: [string, Locator][] = [
      ["people", people],
      ["results", results.locator("tbody tr").first()],
    ];
    const open = async (value: ReturnType<typeof advanced>) => {
      const launch = new URL(issued);
      const fragment = new URLSearchParams(launch.hash.slice(1));
      fragment.set("query", JSON.stringify(value));
      launch.hash = fragment.toString();
      await page.goto("about:blank");
      await page.goto(launch.toString());
      const editor = page.getByRole("dialog", { name: "Filters and sort" });
      await editor.getByRole("button", { name: "Search", exact: true }).click();
      await expect(editor).toBeHidden();
    };
    const expected = {
      rows: [
        ["board-update.eml", ["sender"]],
        ["budget-follow-up.eml", ["sender"]],
      ],
      // The People facet excludes the Bcc-only reviewer.
      people: {
        "Mara Okonkwo-Fell": [2, ["sender"]],
        "Teodor Vask": [2, ["copied", "recipient"]],
        "Ines Albarrán": [1, ["recipient"]],
        "Jory Pellham": [1, ["copied"]],
      },
    };
    await expect
      .poll(async () => summary(await freeze(query, ["person_role"])), {
        timeout: 120_000,
        intervals: [1_000],
      })
      .toEqual(expected);

    await open(query);
    const narrowing = page.waitForResponse(
      (response) =>
        new URL(response.url()).pathname === "/api/v1/workspace/queries" &&
        response.request().method() === "POST",
    );
    await people.getByRole("button", { name: "Teodor Vask, Cc, Recipient, 2 documents" }).click();
    const response = await narrowing;
    expect(response.status()).toBe(200);
    const sent = response.request().postDataJSON();
    expect(sent.query.text).toBe(query.text);
    expect(sent.query.filters.people).toEqual([
      { person_id: expect.any(String), roles: ["copied", "recipient"] },
    ]);

    await captureStates(
      page,
      testInfo,
      "from-query-person-selected",
      async () => {
        await expect(
          results.getByRole("cell", {
            name: "/board-update.eml, people evidence: Cc, Sender",
          }),
        ).toBeVisible();
        await expect(
          results.getByRole("cell", {
            name: "/budget-follow-up.eml, people evidence: Recipient, Sender",
          }),
        ).toBeVisible();
        await expect(
          people.getByRole("button", { name: "Teodor Vask, Cc, Recipient, 2 documents, selected" }),
        ).toHaveAttribute("aria-pressed", "true");
      },
      parts,
    );

    // Merge the provisioned Teodor into a new survivor. A query that still names
    // the retired ID shows the survivor selected in the People facet.
    const provisioned = await freeze(query, ["person_role"]);
    const retiredID = provisioned?.facets[0]?.values.find(
      (value) => value.label === "Teodor Vask",
    )?.key;
    if (!retiredID) throw new Error("Synthetic Teodor person missing");
    const retired = JSON.parse(await run("people", "show", retiredID));
    const survivor = JSON.parse(await run("people", "create", "Teodor Vask-Ruiz"));
    await run(
      "people",
      "merge",
      survivor.person_id,
      retiredID,
      "--revision",
      String(survivor.revision),
      "--absorbed-revision",
      String(retired.revision),
    );
    const retiredQuery = advanced(query.text, {
      people: [{ person_id: retiredID, roles: ["copied", "recipient"] }],
    });
    await expect
      .poll(
        async () => summary(await freeze(retiredQuery, ["person_role"])).people["Teodor Vask-Ruiz"],
        { timeout: 120_000, intervals: [1_000] },
      )
      .toEqual([2, ["copied", "recipient"]]);

    await open(retiredQuery);
    const survivorSelected = people.getByRole("button", {
      name: "Teodor Vask-Ruiz, Cc, Recipient, 2 documents, selected",
    });
    await captureStates(
      page,
      testInfo,
      "merged-survivor-selected",
      async () => {
        await expect(
          results.getByRole("cell", {
            name: "/board-update.eml, people evidence: Cc, Sender",
          }),
        ).toBeVisible();
        await expect(
          results.getByRole("cell", {
            name: "/budget-follow-up.eml, people evidence: Recipient, Sender",
          }),
        ).toBeVisible();
        await expect(survivorSelected).toHaveAttribute("aria-pressed", "true");
      },
      parts,
    );
  } catch (error) {
    failure = error;
    throw error;
  } finally {
    globalThis.fetch = originalFetch;
    try {
      await stop();
      await rm(scratch, { recursive: true, force: true });
    } catch (cleanupError) {
      if (failure !== undefined) {
        throw new Error(`Role search proof cleanup failed: ${inspect(cleanupError)}`, {
          cause: failure,
        });
      }
      throw cleanupError;
    }
  }
});
