import { expect, test, type Page, type TestInfo } from "@playwright/test";
import { execFile } from "node:child_process";
import { access, mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { inspect, promisify } from "node:util";
import { fileURLToPath } from "node:url";
import type { SnapshotPage } from "../src/snapshots.js";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
// "branch" proves this checkout; "base" captures the same fixtures with the
// pull request's base binary, which predates role operands and the People facet.
const proof = process.env.DOCBANK_ROLE_SEARCH_PROOF;
test.skip(!proof, "DOCBANK_ROLE_SEARCH_PROOF enables this proof");

// Invented people from the approved prototype; every address is reserved.
const mara = '"Mara Okonkwo-Fell" <mara@okonkwo.example.test>';
const ines = "=?UTF-8?Q?Ines_Albarr=C3=A1n?= <ines@albarran.example.test>";
const teodor = '"Teodor Vask" <teodor@vask.example.test>';
const jory = '"Jory Pellham" <jory@pellham.example.test>';
const hidden = '"Private Reviewer" <reviewer@private.example.test>';
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
const advanced = (text: string) =>
  ({
    v: 1,
    text,
    syntax: "advanced",
    mode: "lexical",
    filters: {},
    sort: { field: "name", direction: "asc" },
  }) as const;
const query = advanced('from:"Mara Okonkwo-Fell"');
// Any role: the one person operand the base branch already has.
const anyRole = advanced('person:"Mara Okonkwo-Fell"');

// Attachments are named role-search-<base|branch>-<state>-<wide|narrow>-<theme>.png.
async function capture(page: Page, testInfo: TestInfo, name: string, theme: "light" | "dark") {
  await page.evaluate(async (value) => {
    localStorage.setItem("docbank-theme", value);
    document.documentElement.classList.toggle("dark", value === "dark");
    await document.fonts.ready;
  }, theme);
  // The JSON reporter embeds the body as base64 in the uploaded CI report.
  const body = await page.screenshot({ animations: "disabled" });
  await testInfo.attach(`role-search-${proof}-${name}-${theme}.png`, {
    body,
    contentType: "image/png",
  });
}

async function captureStates(
  page: Page,
  testInfo: TestInfo,
  state: string,
  ready: () => Promise<void>,
) {
  await page.setViewportSize({ width: 1440, height: 960 });
  await ready();
  await capture(page, testInfo, `${state}-wide`, "dark");
  await capture(page, testInfo, `${state}-wide`, "light");
  await page.setViewportSize({ width: 860, height: 1100 });
  await ready();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await capture(page, testInfo, `${state}-narrow`, "dark");
  await capture(page, testInfo, `${state}-narrow`, "light");
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
    // People are provisioned from the addresses in the background; wait for the
    // browser session's own frozen result before driving the page.
    const freeze = async (
      value: ReturnType<typeof advanced>,
      facets: string[],
    ): Promise<SnapshotPage | undefined> => {
      const response = await fetch(`http://127.0.0.1:${issued.port}/api/v1/workspace/queries`, {
        method: "POST",
        signal: AbortSignal.timeout(30_000),
        headers: {
          Host: issued.host,
          "Content-Type": "application/json",
          "X-Docbank-Web-Session": session,
        },
        body: JSON.stringify({ query: value, page_size: 50, facets }),
      });
      return response.ok ? ((await response.json()) as SnapshotPage) : undefined;
    };
    const summary = (page?: SnapshotPage) => ({
      rows: page?.rows.map((row) => [row.name, row.matched_roles]),
      people: Object.fromEntries(
        page?.facets[0]?.values.map((value) => [value.label, [value.count, value.roles]]) ?? [],
      ),
    });
    const results = page.getByRole("region", { name: "Frozen query results" });
    const people = page.getByRole("group", { name: "People facet" });
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
    const everyEmail = emails.map(([name]) => name).sort();
    if (proof === "base") {
      await expect
        .poll(async () => (await freeze(anyRole, []))?.rows.map((row) => row.name), {
          timeout: 120_000,
          intervals: [1_000],
        })
        .toEqual(everyEmail);
      await open(anyRole);
      await captureStates(page, testInfo, "person-query", async () => {
        await expect(results.getByRole("cell", { name: "/vendor-notes.eml" })).toBeVisible();
        await expect(people).toHaveCount(0);
      });
      return;
    }
    const expected = {
      rows: [
        ["board-update.eml", ["sender"]],
        ["budget-follow-up.eml", ["sender"]],
      ],
      // The Bcc-only reviewer never reaches a browser session.
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

    // The base comparison: any role, no role chips, but now a People group.
    await open(anyRole);
    await captureStates(page, testInfo, "person-query", async () => {
      await expect(results.getByRole("cell", { name: "/vendor-notes.eml" })).toBeVisible();
      await expect(
        people.getByRole("button", { name: "Teodor Vask, Cc, Recipient, 2 documents" }),
      ).toBeVisible();
    });

    await page.setViewportSize({ width: 1440, height: 960 });
    await open(query);
    await expect(
      results.getByRole("cell", { name: "/budget-follow-up.eml, matched as Sender" }),
    ).toBeVisible();
    await expect(
      people.getByRole("button", { name: "Teodor Vask, Cc, Recipient, 2 documents" }),
    ).toBeVisible();
    await expect(people.getByText("Private Reviewer")).toHaveCount(0);

    const narrowing = page.waitForResponse(
      (response) =>
        new URL(response.url()).pathname === "/api/v1/workspace/queries" &&
        response.request().method() === "POST",
    );
    await people.getByRole("button", { name: "Ines Albarrán, Recipient, 1 documents" }).click();
    const response = await narrowing;
    expect(response.status()).toBe(200);
    const sent = response.request().postDataJSON();
    expect(sent.query.text).toBe(query.text);
    expect(sent.query.filters.people).toEqual([
      { person_id: expect.any(String), roles: ["recipient"] },
    ]);
    await expect(
      results.getByRole("cell", { name: "/board-update.eml, matched as Recipient, Sender" }),
    ).toBeVisible();
    await expect(results.getByRole("cell", { name: /budget-follow-up\.eml/ })).toHaveCount(0);
    await expect(
      people.getByRole("button", { name: "Ines Albarrán, Recipient, 1 documents, selected" }),
    ).toHaveAttribute("aria-pressed", "true");

    await captureStates(page, testInfo, "from-query-person-selected", async () => {
      await expect(
        results.getByRole("cell", { name: "/board-update.eml, matched as Recipient, Sender" }),
      ).toBeVisible();
      await expect(people).toBeVisible();
    });
  } catch (error) {
    failure = error;
    throw error;
  } finally {
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
