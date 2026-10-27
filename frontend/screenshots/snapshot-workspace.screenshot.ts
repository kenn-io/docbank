import { createHash, randomUUID } from "node:crypto";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

import { expect, test, type Page } from "@playwright/test";
import type { SnapshotPage as QuerySnapshotPage } from "../src/snapshots.js";

const execFileAsync = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const binary = path.join(
  repository,
  "bin",
  process.platform === "win32" ? "docbank.exe" : "docbank",
);
const screenshots = process.env.DOCBANK_SNAPSHOT_SCREENSHOT_DIR;
test.skip(
  !screenshots,
  "DOCBANK_SNAPSHOT_SCREENSHOT_DIR is required for PR-only snapshot captures",
);

type Query = {
  v: 1;
  text: string;
  syntax: "simple";
  mode: "lexical";
  filters: { no_tags?: true; tag_ids?: string[] };
  sort: { field: "path"; direction: "asc" };
};
type BatchRequest = {
  operation_id: string;
  tag_id: string;
  assign: boolean;
  nodes: { node_id: number; revision: number }[];
};
type BatchReceipt = {
  version: number;
  operation_id: string;
  request_digest: string;
  tag_id: string;
  assign: boolean;
  tag_revision: number;
  assignment_count: number;
  completed_at: string;
  nodes: { node_id: number; expected_revision: number; revision: number; changed: boolean }[];
};
type RecoveryAction = {
  action_id: string;
  vault_id: string;
  total: number;
  tag_id: string;
  batches: {
    index: number;
    request_digest: string;
    request: BatchRequest;
    members: { node_id: number; revision: number }[];
    receipt?: BatchReceipt;
    state?: string;
  }[];
};
type SnapshotPage = {
  snapshot_id: string;
  total: number;
  rows: { node_id: number; revision: number; path: string }[];
  next_cursor?: string;
};
type Tag = { id: string; name: string };

function persistedReceipt(receipt: BatchReceipt): BatchReceipt {
  return {
    version: receipt.version,
    operation_id: receipt.operation_id,
    request_digest: receipt.request_digest,
    tag_id: receipt.tag_id,
    assign: receipt.assign,
    tag_revision: receipt.tag_revision,
    assignment_count: receipt.assignment_count,
    completed_at: receipt.completed_at,
    nodes: receipt.nodes.map((node) => ({
      node_id: node.node_id,
      expected_revision: node.expected_revision,
      revision: node.revision,
      changed: node.changed,
    })),
  };
}

const untaggedQuery: Query = {
  v: 1,
  text: "",
  syntax: "simple",
  mode: "lexical",
  filters: { no_tags: true },
  sort: { field: "path", direction: "asc" },
};

function queryURL(raw: string, query: Query): string {
  const url = new URL(raw);
  const fragment = new URLSearchParams(url.hash.slice(1));
  fragment.set("query", JSON.stringify(query));
  url.hash = fragment.toString();
  return url.toString();
}

function sessionAPI(rawURL: string) {
  const url = new URL(rawURL);
  const session = new URLSearchParams(url.hash.slice(1)).get("web_session");
  if (!session) throw new Error("Synthetic browser URL omitted its session");
  const request = async (route: string, init: RequestInit = {}): Promise<Response> =>
    fetch(`http://127.0.0.1:${url.port}${route}`, {
      ...init,
      signal: init.signal ?? AbortSignal.timeout(60_000),
      headers: { Host: url.host, "X-Docbank-Web-Session": session, ...init.headers },
    });
  return {
    request,
    async json<T>(route: string, init: RequestInit = {}): Promise<T> {
      const response = await request(route, init);
      if (!response.ok)
        throw new Error(`Synthetic browser API ${route} failed with ${response.status}`);
      return response.json() as Promise<T>;
    },
  };
}

async function collectRows(rawURL: string, query: Query): Promise<SnapshotPage> {
  const api = sessionAPI(rawURL);
  const first = await api.json<SnapshotPage>("/api/v1/workspace/queries", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ query, page_size: 250, facets: [] }),
  });
  const rows = [...first.rows];
  let current = first;
  while (current.next_cursor) {
    current = await api.json<SnapshotPage>(`/api/v1/workspace/queries/${first.snapshot_id}/pages`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ cursor: current.next_cursor }),
    });
    rows.push(...current.rows);
  }
  return { ...first, rows, next_cursor: undefined };
}

async function retainedAction(
  page: Page,
  vaultID?: string,
): Promise<{
  header: RecoveryAction & { state: string; checkpoint_verified: boolean };
  batches: (RecoveryAction["batches"][number] & { state: string })[];
}> {
  return page.evaluate(async (vaultID) => {
    const database = await new Promise<IDBDatabase>((resolve, reject) => {
      const request = indexedDB.open("docbank-action-journal-v1", 1);
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error);
    });
    try {
      const transaction = database.transaction(["actions", "batches"], "readonly");
      const getAll = <T>(request: IDBRequest<T>) =>
        new Promise<T>((resolve, reject) => {
          request.onsuccess = () => resolve(request.result);
          request.onerror = () => reject(request.error);
        });
      const headers = (await getAll(transaction.objectStore("actions").getAll())).filter(
        (header) => !vaultID || header.vault_id === vaultID,
      );
      const batches = (await getAll(transaction.objectStore("batches").getAll())).filter(
        (batch) => !vaultID || batch.vault_id === vaultID,
      );
      if (headers.length !== 1)
        throw new Error(`Expected one retained action, got ${headers.length}`);
      return { header: headers[0], batches };
    } finally {
      database.close();
    }
  }, vaultID);
}

// A query in the URL opens Advanced to run it; a page that already ran it shows its results.
async function runSnapshot(page: Page): Promise<void> {
  const editor = page.getByRole("dialog", { name: "Filters and sort" });
  const results = page.getByRole("button", { name: "Tag all…", exact: true });
  await expect(editor.or(results)).toBeVisible();
  if (await editor.isVisible()) {
    await editor.getByRole("button", { name: "Search", exact: true }).click();
    await expect(editor).toBeHidden();
  }
  await expect(results).toBeEnabled();
}

// Opens the Tag all dialog, picks the tag, and continues to the run dialog.
async function tagAll(page: Page, tagName: string, assign = true): Promise<void> {
  await page.getByRole("button", { name: "Tag all…", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Tag all results" });
  await dialog.getByRole("combobox", { name: "Tag", exact: true }).fill(tagName);
  await page.getByRole("option", { name: new RegExp(`^${tagName}`) }).click();
  if (!assign) await dialog.getByRole("radio", { name: "Remove", exact: true }).click();
  await dialog.getByRole("button", { name: "Continue", exact: true }).click();
}

// A retained run shows its notice in the results header, in any tab.
async function resumeRetained(page: Page): Promise<void> {
  await page.getByRole("button", { name: "Resume", exact: true }).click();
}

test("retries an interrupted send from an already-open recovery tab", async ({ page, context }) => {
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-snapshot-workspace-"));
  const vault = path.join(workspace, "vault");
  const source = path.join(workspace, "synthetic.txt");
  const run = async (...args: string[]) =>
    (
      await execFileAsync(binary, args, {
        cwd: repository,
        env: { ...process.env, DOCBANK_HOME: vault },
        timeout: 60_000,
      })
    ).stdout.trim();
  let release!: () => void;
  const delayedResponse = new Promise<void>((resolve) => {
    release = resolve;
  });
  try {
    await writeFile(source, "Synthetic interrupted-send document.\n");
    await run("add", source, "--dest", "/");
    await run("tag", "create", "Review");
    const rawURL = await run("web", "--no-browser");
    const snapshotURL = queryURL(rawURL, untaggedQuery);
    await page.goto(snapshotURL);
    await runSnapshot(page);

    // Both journals must be open before sending; opening one later already
    // recovers an interrupted batch as uncertain. Preparing the run keeps it
    // for the other tab to resume.
    await tagAll(page, "Review");
    const recovery = page.getByRole("dialog", { name: "Tag all results" });
    await expect(recovery.getByRole("button", { name: "Apply", exact: true })).toBeVisible();
    const other = await context.newPage();
    const retries: BatchRequest[] = [];
    other.on("request", (request) => {
      if (new URL(request.url()).pathname === "/api/v1/batch/tags")
        retries.push(request.postDataJSON() as BatchRequest);
    });
    await other.goto(snapshotURL);
    await runSnapshot(other);
    await resumeRetained(other);
    const otherRecovery = other.getByRole("dialog", { name: "Tag all results" });
    await expect(otherRecovery.getByRole("button", { name: "Apply", exact: true })).toBeVisible();

    let responseReady!: () => void;
    const committed = new Promise<void>((resolve) => {
      responseReady = resolve;
    });
    let originalRequest!: BatchRequest;
    let originalReceipt!: BatchReceipt;
    await page.route("**/api/v1/batch/tags", async (route) => {
      originalRequest = route.request().postDataJSON() as BatchRequest;
      const response = await route.fetch();
      expect(response.status()).toBe(200);
      originalReceipt = (await response.json()) as BatchReceipt;
      responseReady();
      await delayedResponse;
    });
    await recovery.getByRole("button", { name: "Apply", exact: true }).click();
    await committed;
    await page.close();
    release();
    expect((await retainedAction(other)).batches[0].state).toBe("sending");

    await otherRecovery.getByRole("button", { name: "Apply", exact: true }).click();
    await expect(otherRecovery.getByText("Added “Review” to 1 document.")).toBeVisible();
    expect(retries).toEqual([originalRequest]);
    expect((await retainedAction(other)).batches[0].receipt).toEqual(
      persistedReceipt(originalReceipt),
    );
    const tagged = await collectRows(rawURL, {
      ...untaggedQuery,
      filters: { tag_ids: [originalRequest.tag_id] },
    });
    expect(tagged.rows.map((row) => row.revision)).toEqual(
      originalReceipt.nodes.map((node) => node.revision),
    );
  } finally {
    release();
    await run("daemon", "stop");
    expect(JSON.parse(await run("daemon", "status", "--json")).running).toBe(false);
    await rm(workspace, { recursive: true, force: true });
  }
});

for (const outcome of ["success", "response loss"] as const) {
  test(`keeps an action stale across tabs after a late ${outcome}`, async ({ page, context }) => {
    const workspace = await mkdtemp(path.join(tmpdir(), "docbank-snapshot-workspace-"));
    const vault = path.join(workspace, "vault");
    const source = path.join(workspace, "synthetic");
    const checkpoint = path.join(workspace, "action.json");
    const run = async (...args: string[]) =>
      (
        await execFileAsync(binary, args, {
          cwd: repository,
          env: { ...process.env, DOCBANK_HOME: vault },
          timeout: 60_000,
        })
      ).stdout.trim();
    let release!: () => void;
    const delayedResponse = new Promise<void>((resolve) => {
      release = resolve;
    });
    try {
      await mkdir(source);
      for (let index = 0; index < 3; index++) {
        await writeFile(
          path.join(source, `document-${index}.txt`),
          `Synthetic race document ${index}.\n`,
        );
      }
      await run("add", source, "--dest", "/");
      await run("tag", "create", "Review");
      await run("tag", "create", "Revision change");
      const rawURL = await run("web", "--no-browser");
      const api = sessionAPI(rawURL);
      const { items: tags } = await api.json<{ items: Tag[] }>("/api/v1/tags?limit=1000&offset=0");
      const tag = tags.find((tag) => tag.name === "Review")!;
      const fenceTag = tags.find((tag) => tag.name === "Revision change")!;
      const { vault_id: vaultID } = await api.json<{ vault_id: string }>("/api/v1/audit/status");
      const snapshot = await api.json<QuerySnapshotPage>("/api/v1/workspace/queries", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ query: untaggedQuery, page_size: 50, facets: [] }),
      });
      // Three singleton batches leave one untouched batch beyond the revision conflict.
      const batches = snapshot.rows.map((row, index) => {
        const member = {
          node_id: row.node_id,
          content_version_id: row.content_version_id,
          blob_hash: row.blob_hash,
          size: row.size,
          revision: row.revision,
        };
        const request = {
          operation_id: randomUUID(),
          tag_id: tag.id,
          assign: true,
          nodes: [{ node_id: member.node_id, revision: member.revision }],
        };
        return {
          index,
          operation_id: request.operation_id,
          members: [member],
          request,
          request_digest: createHash("sha256")
            .update(`docbank-tag-batch-v1\n${tag.id}\n1\n${member.node_id}:${member.revision}\n`)
            .digest("hex"),
        };
      });
      const header = {
        version: 1,
        action_id: randomUUID(),
        vault_id: vaultID,
        source: {
          snapshot_id: snapshot.snapshot_id,
          snapshot_fingerprint: snapshot.snapshot_fingerprint,
          query_fingerprint: snapshot.query_fingerprint,
          member_hash: snapshot.member_hash,
        },
        total: snapshot.total,
        total_bytes: snapshot.total_bytes,
        tag_id: tag.id,
        assign: true,
        created_at: new Date().toISOString(),
      };
      const plan_digest = createHash("sha256")
        .update(JSON.stringify({ ...header, batches }))
        .digest("hex");
      await writeFile(checkpoint, JSON.stringify({ ...header, plan_digest, batches }));
      await api.json("/api/v1/batch/tags", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          ...batches[1].request,
          operation_id: randomUUID(),
          tag_id: fenceTag.id,
        }),
      });

      const sent: string[] = [];
      let responseReady!: () => void;
      const committed = new Promise<void>((resolve) => {
        responseReady = resolve;
      });
      await page.route("**/api/v1/batch/tags", async (route) => {
        sent.push((route.request().postDataJSON() as BatchRequest).operation_id);
        const response = await route.fetch();
        expect(response.status()).toBe(200);
        responseReady();
        await delayedResponse;
        if (outcome === "success") await route.fulfill({ response });
        else
          await route.fulfill({ status: 502, contentType: "application/problem+json", body: "{}" });
      });
      const snapshotURL = queryURL(rawURL, untaggedQuery);
      await page.goto(snapshotURL);
      await runSnapshot(page);
      await page.getByRole("button", { name: "Tag all…", exact: true }).click();
      await page
        .getByRole("dialog", { name: "Tag all results" })
        .getByLabel("Resume from a recovery file", { exact: true })
        .setInputFiles(checkpoint);
      const firstRecovery = page.getByRole("dialog", { name: "Tag all results" });
      await firstRecovery.getByRole("button", { name: "Apply", exact: true }).click();
      await committed;

      // Opening in another tab recovers the in-flight batch as uncertain. Its
      // retry receives the committed receipt, then the next batch gets a real 412.
      const other = await context.newPage();
      other.on("request", (request) => {
        if (new URL(request.url()).pathname === "/api/v1/batch/tags") {
          sent.push((request.postDataJSON() as BatchRequest).operation_id);
        }
      });
      await other.goto(snapshotURL);
      await runSnapshot(other);
      await resumeRetained(other);
      const otherRecovery = other.getByRole("dialog", { name: "Tag all results" });
      await otherRecovery.getByRole("button", { name: /^(Retry|Resume)$/ }).click();
      await expect(otherRecovery.getByText(/Some documents changed/)).toBeVisible();
      expect((await retainedAction(other)).header.state).toBe("stale");

      release();
      await expect(firstRecovery.getByRole("button", { name: "Finish later" })).toBeEnabled();
      const retained = await retainedAction(page);
      expect(retained.header.state).toBe("stale");
      expect(retained.batches.map((batch) => batch.state)).toEqual([
        "complete",
        "stale",
        "prepared",
      ]);
      expect(retained.batches[0].receipt?.operation_id).toBe(batches[0].operation_id);
      expect(sent).toEqual([
        batches[0].operation_id,
        batches[0].operation_id,
        batches[1].operation_id,
      ]);
      await expect(firstRecovery.getByText(/Some documents changed/)).toBeVisible();
      await expect(
        firstRecovery.getByRole("button", { name: /^(Apply|Retry|Resume)$/ }),
      ).toHaveCount(0);
    } finally {
      release();
      await run("daemon", "stop");
      expect(JSON.parse(await run("daemon", "status", "--json")).running).toBe(false);
      await rm(workspace, { recursive: true, force: true });
    }
  });
}

test("snapshot workspace recovers exact real-daemon actions without changing frozen membership", async ({
  page,
  context,
}) => {
  test.setTimeout(900_000);
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-snapshot-workspace-"));
  const vault = path.join(workspace, "vault");
  const source = path.join(workspace, "synthetic", "Workspace review");
  const run = async (...args: string[]) =>
    (
      await execFileAsync(binary, args, {
        cwd: repository,
        env: { ...process.env, DOCBANK_HOME: vault },
        maxBuffer: 4 * 1024 * 1024,
        timeout: 300_000,
      })
    ).stdout.trim();
  const rangeText = (from: string) => page.getByRole("status").filter({ hasText: from });

  try {
    page.setDefaultTimeout(30_000);
    page.setDefaultNavigationTimeout(30_000);
    await mkdir(source, { recursive: true, mode: 0o700 });
    for (let index = 0; index < 1001; index += 1) {
      const suffix = index % 2 === 0 ? "txt" : "md";
      await writeFile(
        path.join(source, `workspace-${String(index).padStart(4, "0")}.${suffix}`),
        `Synthetic workspace acceptance document ${index}.\n`,
        { mode: 0o600 },
      );
    }
    await run("add", source, "--dest", "/");
    await run("tag", "create", "Review checkpoint");
    await run("tag", "create", "Fence marker");
    await mkdir(screenshots!, { recursive: true, mode: 0o700 });

    const rawURL = await run("web", "--no-browser");
    const originalURL = queryURL(rawURL, untaggedQuery);
    const api = sessionAPI(originalURL);
    const tags = await api.json<{ items: Tag[] }>("/api/v1/tags?limit=1000&offset=0");
    const reviewTag = tags.items.find((tag) => tag.name === "Review checkpoint");
    const fenceTag = tags.items.find((tag) => tag.name === "Fence marker");
    if (!reviewTag || !fenceTag) throw new Error("Synthetic acceptance tags are missing");

    // A revoked capture owner cannot lend its cursor to a new browser session.
    const expiryAPI = sessionAPI(await run("web", "--no-browser"));
    const expiring = await expiryAPI.json<SnapshotPage>("/api/v1/workspace/queries", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ query: untaggedQuery, page_size: 50, facets: [] }),
    });
    if (!expiring.next_cursor) throw new Error("Synthetic expiry snapshot omitted its next cursor");
    expect((await expiryAPI.request("/api/daemon/web-session", { method: "DELETE" })).status).toBe(
      204,
    );
    const expired = await sessionAPI(await run("web", "--no-browser")).request(
      `/api/v1/workspace/queries/${expiring.snapshot_id}/pages`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ cursor: expiring.next_cursor }),
      },
    );
    expect(expired.status).toBe(410);
    expect(((await expired.json()) as { code?: unknown }).code).toBe("snapshot_gone");

    await page.addInitScript(() => localStorage.setItem("docbank-theme", "dark"));
    await page.goto(originalURL);
    await runSnapshot(page);
    await expect(rangeText("1–100 of 1,001")).toBeVisible();
    await expect(page.getByRole("complementary", { name: "Snapshot facets" })).toBeVisible();

    // A filter the daemon refuses leaves the frozen results in place.
    await page.getByRole("button", { name: "Advanced", exact: true }).click();
    const editor = page.getByRole("dialog", { name: "Filters and sort" });
    await editor.getByText("More", { exact: true }).click();
    await editor
      .getByLabel("Filters JSON")
      .fill(JSON.stringify({ tag_ids: ["77777777-7777-4777-8777-777777777777"] }));
    await editor.getByRole("button", { name: "Search", exact: true }).click();
    await expect(page.locator(".browser .banner[role=alert]")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(rangeText("1–100 of 1,001")).toBeVisible();
    await page.getByRole("button", { name: "Next page", exact: true }).click();
    await expect(rangeText("101–200 of 1,001")).toBeVisible();
    await page.getByRole("button", { name: "Previous page", exact: true }).click();
    await expect(rangeText("1–100 of 1,001")).toBeVisible();

    await page.getByRole("button", { name: "txt, 501 documents", exact: true }).click();
    await expect(rangeText("1–100 of 501")).toBeVisible();
    await page.getByRole("button", { name: "txt, 501 documents, selected", exact: true }).click();
    await expect(rangeText("1–100 of 1,001")).toBeVisible();
    await page.getByRole("button", { name: "Next page", exact: true }).click();
    await expect(rangeText("101–200 of 1,001")).toBeVisible();
    await page.setViewportSize({ width: 390, height: 844 });
    await page.getByRole("navigation", { name: "Result pages" }).scrollIntoViewIfNeeded();
    await expect(
      page.getByRole("cell", { name: "/Workspace review/workspace-0100.txt", exact: true }),
    ).toBeVisible();
    await page.setViewportSize({ width: 1440, height: 960 });
    await page.getByRole("button", { name: "Previous page", exact: true }).click();
    await expect(rangeText("1–100 of 1,001")).toBeVisible();
    await page.getByRole("region", { name: "Frozen query results" }).scrollIntoViewIfNeeded();
    await page.screenshot({
      path: path.join(screenshots!, "web-snapshot-workspace.png"),
      animations: "disabled",
    });

    // Keep a second live tab on the frozen untagged population.
    const replayPage = await context.newPage();
    replayPage.setDefaultTimeout(30_000);
    replayPage.setDefaultNavigationTimeout(30_000);
    await replayPage.addInitScript(() => localStorage.setItem("docbank-theme", "dark"));
    await replayPage.goto(originalURL);
    await runSnapshot(replayPage);

    // Cancel while enumeration is waiting on a real page response.
    let releaseCapture!: () => void;
    const captureGate = new Promise<void>((resolve) => {
      releaseCapture = resolve;
    });
    let captureReady!: () => void;
    const captureWaiting = new Promise<void>((resolve) => {
      captureReady = resolve;
    });
    await page.route(
      "**/api/v1/workspace/queries/*/pages",
      async (route) => {
        const response = await route.fetch();
        captureReady();
        await captureGate;
        await route.fulfill({ response });
      },
      { times: 1 },
    );
    await tagAll(page, "Review checkpoint");
    await captureWaiting;
    const actions = page.getByRole("dialog", { name: "Tag all results" });
    try {
      const aborted = page.waitForEvent("requestfailed", (request) =>
        new URL(request.url()).pathname.endsWith("/pages"),
      );
      await actions.getByRole("button", { name: "Cancel", exact: true }).click();
      await aborted;
    } finally {
      releaseCapture();
    }
    await expect(actions).toHaveCount(0);
    await expect(rangeText("1–100 of 1,001")).toBeVisible();

    // Commit batch one on the server, then lose only its browser response.
    let droppedRequest: BatchRequest | undefined;
    let droppedReceipt: BatchReceipt | undefined;
    await page.route("**/api/v1/batch/tags", async (route) => {
      if (droppedRequest) {
        await route.continue();
        return;
      }
      droppedRequest = route.request().postDataJSON() as BatchRequest;
      const response = await route.fetch();
      expect(response.status()).toBe(200);
      droppedReceipt = (await response.json()) as BatchReceipt;
      await route.fulfill({ status: 502, contentType: "application/problem+json", body: "{}" });
    });
    await tagAll(page, "Review checkpoint");
    const recovery = page.getByRole("dialog", { name: "Tag all results" });
    await expect(recovery.getByText("Adds “Review checkpoint” to 1,001 documents.")).toBeVisible();
    await recovery.getByRole("button", { name: "Apply", exact: true }).click();
    await expect(recovery.getByText(/The last request's result is unknown/)).toBeVisible();
    await page.screenshot({
      path: path.join(screenshots!, "web-snapshot-action-recovery.png"),
      animations: "disabled",
    });
    const afterLoss = await retainedAction(page);
    expect(afterLoss.header.state).toBe("uncertain");
    expect(afterLoss.header.checkpoint_verified).toBe(false);
    expect(afterLoss.batches.map((batch) => batch.state)).toEqual(["uncertain", "prepared"]);
    const savedRequests = afterLoss.batches.map((batch) => batch.request);
    expect(droppedRequest).toEqual(savedRequests[0]);
    if (!droppedReceipt)
      throw new Error("The injected transport failure did not retain the committed receipt");
    expect(droppedReceipt.operation_id).toBe(savedRequests[0]?.operation_id);
    const expectedDroppedReceipt = persistedReceipt(droppedReceipt);

    // Reload through the same origin: the unfinished run is still there to resume.
    const reloadURL = new URL(originalURL);
    reloadURL.searchParams.set("acceptance_reload", "1");
    await page.goto(reloadURL.toString());
    await runSnapshot(page);
    await expect(
      page.getByText(/Adding “Review checkpoint” to 1,001 documents didn't finish/),
    ).toBeVisible();
    await resumeRetained(page);
    await expect(
      page.getByRole("dialog", { name: "Tag all results" }).getByText(/result is unknown/),
    ).toBeVisible();
    await page.keyboard.press("Escape");

    // The other live tab retries the exact original operation and sends the
    // untouched second batch. The first receipt prevents a duplicate change.
    const replayRequests: BatchRequest[] = [];
    replayPage.on("request", (request) => {
      if (request.method() === "POST" && new URL(request.url()).pathname === "/api/v1/batch/tags") {
        replayRequests.push(request.postDataJSON() as BatchRequest);
      }
    });
    const replayReload = new URL(originalURL);
    replayReload.searchParams.set("acceptance_reload", "2");
    await replayPage.goto(replayReload.toString());
    await runSnapshot(replayPage);
    await resumeRetained(replayPage);
    const replayRecovery = replayPage.getByRole("dialog", { name: "Tag all results" });
    await expect(replayRecovery.getByText(/result is unknown/)).toBeVisible();
    await replayRecovery.getByRole("button", { name: "Retry", exact: true }).click();
    await expect(
      replayRecovery.getByText("Added “Review checkpoint” to 1,001 documents."),
    ).toBeVisible();
    expect(replayRequests).toEqual(savedRequests);
    const completed = await retainedAction(replayPage);
    expect(completed.header.state).toBe("complete");
    expect(completed.batches[0]?.receipt).toEqual(expectedDroppedReceipt);
    await replayRecovery.getByRole("button", { name: "Done", exact: true }).click();

    const reviewQuery: Query = { ...untaggedQuery, filters: { tag_ids: [reviewTag.id] } };
    const taggedAfterCommit = await collectRows(originalURL, reviewQuery);
    expect(taggedAfterCommit.total).toBe(1001);
    const originalRevisions = new Map(
      afterLoss.batches
        .flatMap((batch) => batch.members)
        .map((member) => [member.node_id, member.revision]),
    );
    for (const row of taggedAfterCommit.rows) {
      expect(row.revision).toBe((originalRevisions.get(row.node_id) ?? -1) + 1);
    }

    // A later revision fences a new exact run. It is never refreshed to make
    // the original request apply to changed documents.
    await page.goto(queryURL(rawURL, reviewQuery));
    await runSnapshot(page);
    await expect(rangeText("1–100 of 1,001")).toBeVisible();
    await tagAll(page, "Review checkpoint", false);
    const staleRecovery = page.getByRole("dialog", { name: "Tag all results" });
    await expect(
      staleRecovery.getByText("Removes “Review checkpoint” from 1,001 documents."),
    ).toBeVisible();
    const staleMember = taggedAfterCommit.rows[0]!;
    await api.json<BatchReceipt>("/api/v1/batch/tags", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        operation_id: randomUUID(),
        tag_id: fenceTag.id,
        assign: true,
        nodes: [{ node_id: staleMember.node_id, revision: staleMember.revision }],
      }),
    });
    await staleRecovery.getByRole("button", { name: "Apply", exact: true }).click();
    await expect(staleRecovery.getByText(/Some documents changed/)).toBeVisible();
    await expect(staleRecovery.getByRole("button", { name: /^(Apply|Retry|Resume)$/ })).toHaveCount(
      0,
    );
    await page.screenshot({
      path: path.join(screenshots!, "web-snapshot-action-stale.png"),
      animations: "disabled",
    });
    const taggedAfterFence = await collectRows(originalURL, reviewQuery);
    expect(taggedAfterFence.total).toBe(1001);
    expect(taggedAfterFence.rows.find((row) => row.node_id === staleMember.node_id)?.revision).toBe(
      staleMember.revision + 1,
    );

    // The run lists the exact documents it covers, in pages of 50 per batch.
    await staleRecovery.getByText("Review documents", { exact: true }).click();
    await expect(
      staleRecovery.getByRole("table", { name: "Documents in this run" }).getByRole("row"),
    ).toHaveCount(51);
    await staleRecovery.getByRole("button", { name: "Next", exact: true }).click();
    await expect(staleRecovery.getByText("51–100 of 1000", { exact: true })).toBeVisible();
    await staleRecovery.locator(".batch-select select").selectOption("1");
    await expect(
      staleRecovery.getByRole("table", { name: "Documents in this run" }).getByRole("row"),
    ).toHaveCount(2);
    await page.screenshot({
      path: path.join(screenshots!, "web-snapshot-exact-targets.png"),
      animations: "disabled",
    });
    await staleRecovery.getByRole("button", { name: "Discard run…", exact: true }).click();
    await staleRecovery.getByRole("button", { name: "Discard run", exact: true }).click();
    await expect(staleRecovery).toHaveCount(0);
  } finally {
    try {
      if (JSON.parse(await run("daemon", "status", "--json")).running === true) {
        await run("daemon", "stop");
      }
    } catch (error) {
      throw new Error(`Synthetic snapshot daemon cleanup failed: ${String(error)}`);
    }
    await rm(workspace, { recursive: true, force: true });
  }
});
