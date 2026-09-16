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
const binary = path.join(repository, "docbank");
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
  const request = async (route: string, init: RequestInit = {}): Promise<Response> => fetch(
    `http://127.0.0.1:${url.port}${route}`,
    {
      ...init,
      signal: init.signal ?? AbortSignal.timeout(60_000),
      headers: { Host: url.host, "X-Docbank-Web-Session": session, ...init.headers },
    },
  );
  return {
    request,
    async json<T>(route: string, init: RequestInit = {}): Promise<T> {
      const response = await request(route, init);
      if (!response.ok) throw new Error(`Synthetic browser API ${route} failed with ${response.status}`);
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

async function retainedAction(page: Page, vaultID?: string): Promise<{
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
      const getAll = <T>(request: IDBRequest<T>) => new Promise<T>((resolve, reject) => {
        request.onsuccess = () => resolve(request.result);
        request.onerror = () => reject(request.error);
      });
      const headers = (await getAll(transaction.objectStore("actions").getAll())).filter((header) => !vaultID || header.vault_id === vaultID);
      const batches = (await getAll(transaction.objectStore("batches").getAll())).filter((batch) => !vaultID || batch.vault_id === vaultID);
      if (headers.length !== 1) throw new Error(`Expected one retained action, got ${headers.length}`);
      return { header: headers[0], batches };
    } finally {
      database.close();
    }
  }, vaultID);
}

async function runSnapshot(page: Page): Promise<void> {
  const editor = page.getByRole("region", { name: "Query editor" });
  await expect(editor).toBeVisible();
  await editor.getByRole("button", { name: "Run query", exact: true }).click();
}

async function verifyCheckpoint(page: Page, checkpoint: string): Promise<void> {
  const recovery = page.getByRole("dialog", { name: "Recoverable snapshot action" });
  const downloadPromise = page.waitForEvent("download");
  await recovery.getByRole("button", { name: "Save recovery checkpoint", exact: true }).click();
  await (await downloadPromise).saveAs(checkpoint);
  await recovery.getByLabel("Select the saved recovery checkpoint", { exact: true }).setInputFiles(checkpoint);
  await expect(recovery.getByRole("status")).toHaveText("Recovery checkpoint verified.");
}

for (const outcome of ["success", "response loss"] as const) {
  test(`keeps an action stale across tabs after a late ${outcome}`, async ({ page, context }) => {
    const workspace = await mkdtemp(path.join(tmpdir(), "docbank-snapshot-workspace-"));
    const vault = path.join(workspace, "vault");
    const source = path.join(workspace, "synthetic");
    const checkpoint = path.join(workspace, "action.json");
    const run = async (...args: string[]) => (await execFileAsync(binary, args, {
      cwd: repository, env: { ...process.env, DOCBANK_HOME: vault }, timeout: 60_000,
    })).stdout.trim();
    let release!: () => void;
    const delayedResponse = new Promise<void>((resolve) => { release = resolve; });
    try {
      await mkdir(source);
      for (let index = 0; index < 3; index++) {
        await writeFile(path.join(source, `document-${index}.txt`), `Synthetic race document ${index}.\n`);
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
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ query: untaggedQuery, page_size: 50, facets: [] }),
      });
      // Three singleton batches leave one untouched batch beyond the revision conflict.
      const batches = snapshot.rows.map((row, index) => {
        const member = { node_id: row.node_id, content_version_id: row.content_version_id,
          blob_hash: row.blob_hash, size: row.size, revision: row.revision };
        const request = { operation_id: randomUUID(), tag_id: tag.id, assign: true,
          nodes: [{ node_id: member.node_id, revision: member.revision }] };
        return { index, operation_id: request.operation_id, members: [member], request,
          request_digest: createHash("sha256").update(`docbank-tag-batch-v1\n${tag.id}\n1\n${member.node_id}:${member.revision}\n`).digest("hex") };
      });
      const header = { version: 1, action_id: randomUUID(), vault_id: vaultID,
        source: { snapshot_id: snapshot.snapshot_id, snapshot_fingerprint: snapshot.snapshot_fingerprint,
          query_fingerprint: snapshot.query_fingerprint, member_hash: snapshot.member_hash },
        total: snapshot.total, total_bytes: snapshot.total_bytes, tag_id: tag.id, assign: true,
        created_at: new Date().toISOString() };
      const plan_digest = createHash("sha256").update(JSON.stringify({ ...header, batches })).digest("hex");
      await writeFile(checkpoint, JSON.stringify({ ...header, plan_digest, batches }));
      await api.json("/api/v1/batch/tags", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ ...batches[1].request, operation_id: randomUUID(), tag_id: fenceTag.id }),
      });

      const sent: string[] = [];
      let responseReady!: () => void;
      const committed = new Promise<void>((resolve) => { responseReady = resolve; });
      await page.route("**/api/v1/batch/tags", async (route) => {
        sent.push((route.request().postDataJSON() as BatchRequest).operation_id);
        const response = await route.fetch();
        expect(response.status()).toBe(200);
        responseReady();
        await delayedResponse;
        if (outcome === "success") await route.fulfill({ response });
        else await route.fulfill({ status: 502, contentType: "application/problem+json", body: "{}" });
      });
      await page.goto(rawURL);
      await page.getByRole("button", { name: "Snapshot actions", exact: true }).click();
      await page.getByLabel("Import action recovery file", { exact: true }).setInputFiles(checkpoint);
      const firstRecovery = page.getByRole("dialog", { name: "Recoverable snapshot action" });
      await firstRecovery.getByRole("checkbox", { name: /I confirm this vault/ }).check();
      await firstRecovery.getByRole("button", { name: "Confirm and run action", exact: true }).click();
      await committed;

      // Opening in another tab recovers the in-flight batch as uncertain. Its
      // retry receives the committed receipt, then the next batch gets a real 412.
      const other = await context.newPage();
      other.on("request", (request) => {
        if (new URL(request.url()).pathname === "/api/v1/batch/tags") {
          sent.push((request.postDataJSON() as BatchRequest).operation_id);
        }
      });
      await other.goto(rawURL);
      await other.getByRole("button", { name: "Snapshot actions", exact: true }).click();
      await other.getByRole("button", { name: "Resume retained action", exact: true }).click();
      const otherRecovery = other.getByRole("dialog", { name: "Recoverable snapshot action" });
      await otherRecovery.getByRole("checkbox", { name: /I confirm this vault/ }).check();
      await otherRecovery.getByRole("button", { name: "Retry same operation", exact: true }).click();
      await expect(otherRecovery.getByText(/This action is stale/)).toBeVisible();
      expect((await retainedAction(other)).header.state).toBe("stale");

      release();
      await expect(firstRecovery.getByText("Close", { exact: true })).toBeEnabled();
      const retained = await retainedAction(page);
      expect(retained.header.state).toBe("stale");
      expect(retained.batches.map((batch) => batch.state)).toEqual(["complete", "stale", "prepared"]);
      expect(retained.batches[0].receipt?.operation_id).toBe(batches[0].operation_id);
      expect(sent).toEqual([batches[0].operation_id, batches[0].operation_id, batches[1].operation_id]);
      await expect(firstRecovery.getByText(/This action is stale/)).toBeVisible();
      await expect(firstRecovery.getByRole("button", { name: /run action|same operation|resume action/ })).toHaveCount(0);
    } finally {
      release();
      await run("daemon", "stop");
      expect(JSON.parse(await run("daemon", "status", "--json")).running).toBe(false);
      await rm(workspace, { recursive: true, force: true });
    }
  });
}

test("snapshot workspace recovers exact real-daemon actions without changing frozen membership", async ({ page, context }) => {
  test.setTimeout(900_000);
  const workspace = await mkdtemp(path.join(tmpdir(), "docbank-snapshot-workspace-"));
  const vault = path.join(workspace, "vault");
  const wrongVault = path.join(workspace, "wrong-vault");
  const source = path.join(workspace, "synthetic", "Workspace review");
  const checkpoint = path.join(workspace, "snapshot-action.json");
  const staleCheckpoint = path.join(workspace, "stale-action.json");
  let mainRunning = false;
  let wrongRunning = false;
  const command = (home: string) => async (...args: string[]) => (await execFileAsync(binary, args, {
    cwd: repository,
    env: { ...process.env, DOCBANK_HOME: home },
    maxBuffer: 4 * 1024 * 1024,
    timeout: 300_000,
  })).stdout.trim();
  const run = command(vault);
  const runWrong = command(wrongVault);

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

    const originalRawURL = await run("web", "--no-browser");
    mainRunning = true;
    const originalURL = queryURL(originalRawURL, untaggedQuery);
    const originalOrigin = new URL(originalURL).origin;
    const originalAPI = sessionAPI(originalURL);
    const tags = await originalAPI.json<{ items: Tag[] }>("/api/v1/tags?limit=1000&offset=0");
    const reviewTag = tags.items.find((tag) => tag.name === "Review checkpoint");
    const fenceTag = tags.items.find((tag) => tag.name === "Fence marker");
    if (!reviewTag || !fenceTag) throw new Error("Synthetic acceptance tags are missing");

    // A revoked capture owner cannot lend its cursor to a new browser session.
    const expiryRawURL = await run("web", "--no-browser");
    const expiryAPI = sessionAPI(expiryRawURL);
    const expiring = await expiryAPI.json<SnapshotPage>("/api/v1/workspace/queries", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ query: untaggedQuery, page_size: 50, facets: [] }),
    });
    if (!expiring.next_cursor) throw new Error("Synthetic expiry snapshot omitted its next cursor");
    const revoked = await expiryAPI.request("/api/daemon/web-session", { method: "DELETE" });
    expect(revoked.status).toBe(204);
    const replacementAPI = sessionAPI(await run("web", "--no-browser"));
    const expired = await replacementAPI.request(`/api/v1/workspace/queries/${expiring.snapshot_id}/pages`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ cursor: expiring.next_cursor }),
    });
    expect(expired.status).toBe(410);
    expect((await expired.json() as { code?: unknown }).code).toBe("snapshot_gone");

    await page.addInitScript(() => localStorage.setItem("docbank-theme", "dark"));
    await page.goto(originalURL);
    await runSnapshot(page);
    await expect(page.getByRole("status").filter({ hasText: "1–100 of 1,001" })).toBeVisible();
    await expect(page.getByRole("complementary", { name: "Snapshot facets" })).toBeVisible();
    const editor = page.getByRole("region", { name: "Query editor" });
    const runQuery = editor.getByRole("button", { name: "Run query", exact: true });
    await editor.getByLabel("Structured facets JSON").fill(JSON.stringify({
      tag_ids: ["77777777-7777-4777-8777-777777777777"],
    }));
    await runQuery.click();
    await expect(page.locator(".browser .banner[role=alert]")).toBeVisible();
    await expect(page.getByRole("status").filter({ hasText: "1–100 of 1,001" })).toBeVisible();
    await page.getByRole("button", { name: "Next page", exact: true }).click();
    await expect(page.getByRole("status").filter({ hasText: "101–200 of 1,001" })).toBeVisible();
    await page.getByRole("button", { name: "Previous page", exact: true }).click();
    await expect(page.getByRole("status").filter({ hasText: "1–100 of 1,001" })).toBeVisible();
    await editor.getByLabel("Structured facets JSON").fill(JSON.stringify(untaggedQuery.filters));
    for (const mode of ["semantic", "hybrid"]) {
      await editor.getByRole("combobox", { name: /^Query mode:/ }).click();
      await page.getByRole("option", { name: mode, exact: true }).click();
      await expect(editor.getByText("Frozen snapshots support lexical mode only.")).toBeVisible();
      await expect(runQuery).toBeDisabled();
    }
    await editor.getByRole("combobox", { name: /^Query mode:/ }).click();
    await page.getByRole("option", { name: "lexical", exact: true }).click();
    await editor.getByRole("combobox", { name: /^Query sort:/ }).click();
    await page.getByRole("option", { name: "relevance", exact: true }).click();
    await expect(editor.getByText(/Frozen snapshots do not support relevance sorting/)).toBeVisible();
    await expect(runQuery).toBeDisabled();
    await editor.getByRole("combobox", { name: /^Query sort:/ }).click();
    await page.getByRole("option", { name: "path", exact: true }).click();
    await expect(runQuery).toBeEnabled();
    await page.getByRole("button", { name: "Close query editor", exact: true }).click();
    await page.screenshot({ path: path.join(screenshots!, "web-snapshot-workspace.png"), animations: "disabled" });

    await page.getByRole("checkbox", { name: "Select /Workspace review/workspace-0000.txt", exact: true }).check();
    await page.getByRole("button", { name: "Edit query", exact: true }).click();
    let releaseRerun!: () => void;
    const rerunGate = new Promise<void>((resolve) => { releaseRerun = resolve; });
    let rerunReady!: () => void;
    const rerunWaiting = new Promise<void>((resolve) => { rerunReady = resolve; });
    await page.route("**/api/v1/workspace/queries", async (route) => {
      const response = await route.fetch();
      rerunReady();
      await rerunGate;
      await route.fulfill({ response });
    }, { times: 1 });
    await runSnapshot(page);
    await rerunWaiting;
    try {
      for (const name of ["Tag visible selection", "Tag whole query", "Tag or recover", "Snapshot actions"]) {
        await expect(page.getByRole("button", { name, exact: true })).toBeDisabled();
      }
    } finally {
      releaseRerun();
    }
    await expect(page.getByRole("button", { name: "Tag or recover", exact: true })).toBeEnabled();
    await expect(page.getByRole("region", { name: "Selected documents", exact: true })).toHaveCount(0);
    await page.getByRole("button", { name: "Close query editor", exact: true }).click();
    console.log("snapshot acceptance: all tagging controls stay disabled during a rerun");

    await page.getByRole("button", { name: "txt, 501 documents", exact: true }).click();
    await expect(page.getByRole("status").filter({ hasText: "1–100 of 501" })).toBeVisible();
    await page.getByRole("button", { name: "txt, 501 documents, selected", exact: true }).click();
    await expect(page.getByRole("status").filter({ hasText: "1–100 of 1,001" })).toBeVisible();
    await page.getByRole("button", { name: "Next page", exact: true }).click();
    await expect(page.getByRole("status").filter({ hasText: "101–200 of 1,001" })).toBeVisible();
    await page.setViewportSize({ width: 390, height: 844 });
    const pager = page.getByRole("navigation", { name: "Snapshot pages" });
    await pager.scrollIntoViewIfNeeded();
    await expect(page.getByRole("cell", {
      name: "/Workspace review/workspace-0100.txt",
      exact: true,
    })).toBeVisible();
    await page.screenshot({ path: path.join(screenshots!, "web-snapshot-pager-narrow.png"), animations: "disabled" });
    await page.setViewportSize({ width: 1440, height: 960 });
    await page.getByRole("button", { name: "Previous page", exact: true }).click();
    await expect(page.getByRole("status").filter({ hasText: "1–100 of 1,001" })).toBeVisible();

    // Keep a second live tab on the frozen untagged population.
    const replayPage = await context.newPage();
    replayPage.setDefaultTimeout(30_000);
    replayPage.setDefaultNavigationTimeout(30_000);
    await replayPage.addInitScript(() => localStorage.setItem("docbank-theme", "dark"));
    await replayPage.goto(originalURL);
    await runSnapshot(replayPage);
    await expect(replayPage.getByRole("status").filter({ hasText: "1–100 of 1,001" })).toBeVisible();
    await replayPage.getByRole("button", { name: "Close query editor", exact: true }).click();

    // Cancel while enumeration is waiting on a real page response, then run
    // a new snapshot before that old response is delivered.
    let releaseCapture!: () => void;
    const captureGate = new Promise<void>((resolve) => { releaseCapture = resolve; });
    let captureReady!: () => void;
    const captureWaiting = new Promise<void>((resolve) => { captureReady = resolve; });
    await page.route("**/api/v1/workspace/queries/*/pages", async (route) => {
      const response = await route.fetch();
      captureReady();
      await captureGate;
      await route.fulfill({ response });
    }, { times: 1 });
    await page.getByRole("button", { name: "Tag or recover", exact: true }).click();
    const actions = page.getByRole("dialog", { name: "Tag frozen snapshot" });
    await actions.getByRole("combobox", { name: "Tag for snapshot action" }).click();
    await page.getByRole("option", { name: "Review checkpoint", exact: true }).click();
    await actions.getByRole("button", { name: "Add tag to whole query", exact: true }).click();
    await captureWaiting;
    try {
      const aborted = page.waitForEvent("requestfailed", (request) => new URL(request.url()).pathname.endsWith("/pages"));
      await actions.getByRole("button", { name: "Cancel", exact: true }).click();
      await aborted;
      await page.getByRole("button", { name: "Back to live folder", exact: true }).click();
      await page.getByRole("button", { name: "Edit query", exact: true }).click();
      await runSnapshot(page);
      await expect(page.getByRole("status").filter({ hasText: "1–100 of 1,001" })).toBeVisible();
      await page.getByRole("button", { name: "Close query editor", exact: true }).click();
    } finally {
      releaseCapture();
    }
    await expect(page.getByRole("dialog", { name: "Recoverable snapshot action" })).toHaveCount(0);
    console.log("snapshot acceptance: discarded capture aborted before a new snapshot action");

    await page.getByRole("button", { name: "Tag or recover", exact: true }).click();
    await actions.getByRole("combobox", { name: "Tag for snapshot action" }).click();
    await page.getByRole("option", { name: "Review checkpoint", exact: true }).click();
    await actions.getByRole("button", { name: "Add tag to whole query", exact: true }).click();
    const recovery = page.getByRole("dialog", { name: "Recoverable snapshot action" });
    await expect(recovery.getByText("1001 exact documents", { exact: true })).toBeVisible();
    await expect(recovery.getByText("0 completed · 2 remaining batches", { exact: true })).toBeVisible();
    await verifyCheckpoint(page, checkpoint);

    const checkpointText = await readFile(checkpoint, "utf8");
    expect(checkpointText).not.toContain("web_session");
    expect(checkpointText).not.toContain("web_upload_secret");
    expect(checkpointText).not.toContain("workspace-0000");
    expect(checkpointText).not.toContain("Workspace review");
    const saved = JSON.parse(checkpointText) as RecoveryAction;
    expect(saved.total).toBe(1001);
    expect(saved.batches).toHaveLength(2);

    // A different real vault rejects the same checkpoint before staging it.
    const wrongSource = path.join(workspace, "wrong.txt");
    await writeFile(wrongSource, "Synthetic wrong-vault document.\n", { mode: 0o600 });
    await runWrong("add", wrongSource, "--dest", "/");
    const wrongURL = await runWrong("web", "--no-browser");
    wrongRunning = true;
    const wrongPage = await context.newPage();
    await wrongPage.goto(wrongURL);
    await wrongPage.getByRole("button", { name: "Snapshot actions", exact: true }).click();
    const wrongActions = wrongPage.getByRole("dialog", { name: "Tag frozen snapshot" });
    await wrongActions.getByLabel("Import action recovery file", { exact: true }).setInputFiles(checkpoint);
    await expect(wrongActions.getByRole("alert")).toHaveText("The recovery action belongs to a different vault.");
    await wrongPage.close();
    await runWrong("daemon", "stop");
    wrongRunning = false;
    expect(JSON.parse(await runWrong("daemon", "status", "--json")).running).toBe(false);

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
      droppedReceipt = await response.json() as BatchReceipt;
      await route.fulfill({ status: 502, contentType: "application/problem+json", body: "{}" });
    });
    await recovery.getByRole("checkbox", {
      name: "I confirm this vault, action, tag, operation, and exact targets",
    }).check();
    await recovery.getByRole("button", { name: "Confirm and run action", exact: true }).click();
    await expect(recovery.getByText(/The result is uncertain/)).toBeVisible();
    await page.screenshot({ path: path.join(screenshots!, "web-snapshot-action-recovery.png"), animations: "disabled" });
    expect(droppedRequest).toEqual(saved.batches[0]?.request);
    if (!droppedReceipt) throw new Error("The injected transport failure did not retain the committed receipt");
    expect(droppedReceipt.operation_id).toBe(saved.batches[0]?.request.operation_id);
    const expectedDroppedReceipt = persistedReceipt(droppedReceipt);
    console.log("snapshot acceptance: committed response lost and journal uncertain");

    // Reload through the same origin and inspect the retained real IndexedDB
    // transaction independently from the UI.
    const reloadURL = new URL(originalURL);
    reloadURL.searchParams.set("acceptance_reload", "1");
    await page.goto(reloadURL.toString());
    await page.getByRole("button", { name: "Snapshot actions", exact: true }).click();
    await page.getByRole("dialog", { name: "Tag frozen snapshot" })
      .getByRole("button", { name: "Resume retained action", exact: true }).click();
    const reloadedRecovery = page.getByRole("dialog", { name: "Recoverable snapshot action" });
    await expect(reloadedRecovery.getByText(/The result is uncertain/)).toBeVisible();
    const afterReload = await retainedAction(page);
    expect(afterReload.header.state).toBe("uncertain");
    expect(afterReload.header.checkpoint_verified).toBe(true);
    expect(afterReload.batches.map((batch) => batch.state)).toEqual(["uncertain", "prepared"]);
    console.log("snapshot acceptance: same-origin IndexedDB reload recovered");

    // The other live tab retries the exact original operation and sends the
    // untouched second batch. The first receipt prevents a duplicate change.
    const replayRequests: BatchRequest[] = [];
    replayPage.on("request", (request) => {
      if (request.method() === "POST" && new URL(request.url()).pathname === "/api/v1/batch/tags") {
        replayRequests.push(request.postDataJSON() as BatchRequest);
      }
    });
    await replayPage.getByRole("button", { name: "Tag or recover", exact: true }).click();
    await replayPage.getByRole("dialog", { name: "Tag frozen snapshot" })
      .getByRole("button", { name: "Resume retained action", exact: true }).click();
    const replayRecovery = replayPage.getByRole("dialog", { name: "Recoverable snapshot action" });
    await expect(replayRecovery.getByText(/The result is uncertain/)).toBeVisible();
    await replayRecovery.getByRole("checkbox", {
      name: "I confirm this vault, action, tag, operation, and exact targets",
    }).check();
    await replayRecovery.getByRole("button", { name: "Retry same operation", exact: true }).click();
    await expect(replayRecovery.getByText("Action complete. All batches have validated receipts.", { exact: true })).toBeVisible();
    expect(replayRequests).toEqual(saved.batches.map((batch) => batch.request));
    const completed = await retainedAction(replayPage);
    expect(completed.header.state).toBe("complete");
    expect(completed.batches[0]?.receipt).toEqual(expectedDroppedReceipt);
    console.log("snapshot acceptance: second tab replayed original requests");
    await replayRecovery.getByText("Close", { exact: true }).click();
    await expect(replayPage.getByRole("status").filter({ hasText: "1–100 of 1,001" })).toBeVisible();
    await expect(replayPage.getByText("Added: Review checkpoint", { exact: true })).toBeVisible();
    await expect(replayPage.getByText("No tags were recorded in this snapshot.", { exact: true })).toBeVisible();

    const reviewQuery: Query = { ...untaggedQuery, filters: { tag_ids: [reviewTag.id] } };
    const taggedAfterCommit = await collectRows(originalURL, reviewQuery);
    expect(taggedAfterCommit.total).toBe(1001);
    const originalRevisions = new Map(saved.batches.flatMap((batch) => batch.members)
      .map((member) => [member.node_id, member.revision]));
    for (const row of taggedAfterCommit.rows) {
      expect(row.revision).toBe((originalRevisions.get(row.node_id) ?? -1) + 1);
    }

    // Restarting the same vault rotates origin and session. Importing the
    // original checkpoint replays retained server receipts with no new change.
    await run("daemon", "stop");
    mainRunning = false;
    expect(JSON.parse(await run("daemon", "status", "--json")).running).toBe(false);
    const restartedRawURL = await run("web", "--no-browser");
    mainRunning = true;
    expect(new URL(restartedRawURL).origin).not.toBe(originalOrigin);
    const restartedRequests: BatchRequest[] = [];
    const restartedPage = await context.newPage();
    restartedPage.setDefaultTimeout(30_000);
    restartedPage.setDefaultNavigationTimeout(30_000);
    restartedPage.on("request", (request) => {
      if (request.method() === "POST" && new URL(request.url()).pathname === "/api/v1/batch/tags") {
        restartedRequests.push(request.postDataJSON() as BatchRequest);
      }
    });
    await restartedPage.goto(restartedRawURL);
    await restartedPage.getByRole("button", { name: "Snapshot actions", exact: true }).click();
    await restartedPage.getByRole("dialog", { name: "Tag frozen snapshot" })
      .getByLabel("Import action recovery file", { exact: true }).setInputFiles(checkpoint);
    const restartedRecovery = restartedPage.getByRole("dialog", { name: "Recoverable snapshot action" });
    await expect(restartedRecovery.getByText("0 completed · 2 remaining batches", { exact: true })).toBeVisible();
    const restartedRun = restartedRecovery.getByRole("button", { name: "Confirm and run action", exact: true });
    await expect(restartedRun).toBeDisabled();
    await restartedRecovery.getByRole("checkbox", {
      name: "I confirm this vault, action, tag, operation, and exact targets",
    }).check();
    await expect(restartedRun).toBeEnabled();
    await restartedRun.click();
    await expect(restartedRecovery.getByText("Action complete. All batches have validated receipts.", { exact: true })).toBeVisible();
    expect(restartedRequests).toEqual(saved.batches.map((batch) => batch.request));
    const restartedStored = await retainedAction(restartedPage);
    expect(restartedStored.batches[0]?.receipt).toEqual(expectedDroppedReceipt);
    const taggedAfterRestart = await collectRows(restartedRawURL, reviewQuery);
    expect(taggedAfterRestart.rows.map((row) => [row.node_id, row.revision]))
      .toEqual(taggedAfterCommit.rows.map((row) => [row.node_id, row.revision]));
    console.log("snapshot acceptance: fresh-origin import recovered receipts without revision changes");

    // A later revision fences a new exact action. It is never refreshed to
    // make the original request apply to changed authority.
    await restartedRecovery.getByRole("button", { name: "Abandon action…", exact: true }).click();
    await restartedRecovery.getByRole("button", { name: "Abandon action without rollback", exact: true }).click();
    await restartedPage.getByRole("button", { name: "Edit query", exact: true }).click();
    const staleEditor = restartedPage.getByRole("region", { name: "Query editor" });
    await staleEditor.getByLabel("Query expression", { exact: true }).fill("workspace-0000");
    await staleEditor.getByLabel("Structured facets JSON", { exact: true }).fill("{}");
    await staleEditor.getByRole("button", { name: "Run query", exact: true }).click();
    await expect(restartedPage.getByRole("status").filter({ hasText: "1–1 of 1" })).toBeVisible();
    await restartedPage.getByRole("button", { name: "Tag or recover", exact: true }).click();
    const staleActions = restartedPage.getByRole("dialog", { name: "Tag frozen snapshot" });
    await staleActions.getByRole("combobox", { name: "Tag for snapshot action" }).click();
    await restartedPage.getByRole("option", { name: "Review checkpoint", exact: true }).click();
    await staleActions.getByRole("button", { name: "Remove tag from whole query", exact: true }).click();
    await verifyCheckpoint(restartedPage, staleCheckpoint);
    const staleRecovery = restartedPage.getByRole("dialog", { name: "Recoverable snapshot action" });
    const staleSaved = JSON.parse(await readFile(staleCheckpoint, "utf8")) as RecoveryAction;
    const staleMember = staleSaved.batches[0]?.members[0];
    if (!staleMember) throw new Error("Synthetic stale action omitted its member");
    const restartedAPI = sessionAPI(restartedRawURL);
    await restartedAPI.json<BatchReceipt>("/api/v1/batch/tags", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        operation_id: randomUUID(),
        tag_id: fenceTag.id,
        assign: true,
        nodes: [{ node_id: staleMember.node_id, revision: staleMember.revision }],
      }),
    });
    await staleRecovery.getByRole("checkbox", {
      name: "I confirm this vault, action, tag, operation, and exact targets",
    }).check();
    await staleRecovery.getByRole("button", { name: "Confirm and run action", exact: true }).click();
    await expect(staleRecovery.getByText(/This action is stale/)).toBeVisible();
    await expect(staleRecovery.getByRole("button", { name: /run action|same operation|resume action/ })).toHaveCount(0);
    await restartedPage.screenshot({ path: path.join(screenshots!, "web-snapshot-action-stale.png"), animations: "disabled" });
    const taggedAfterFence = await collectRows(restartedRawURL, reviewQuery);
    expect(taggedAfterFence.total).toBe(1001);
    const changed = taggedAfterFence.rows.find((row) => row.node_id === staleMember.node_id);
    expect(changed?.revision).toBe(staleMember.revision + 1);
    console.log("snapshot acceptance: stale revision fenced without partial removal");

    // A selection receipt advances the next whole-query plan, without
    // changing frozen rows or requiring a rerun between the two actions.
    await staleRecovery.getByRole("button", { name: "Abandon action…", exact: true }).click();
    await staleRecovery.getByRole("button", { name: "Abandon action without rollback", exact: true }).click();
    await staleEditor.getByLabel("Query expression", { exact: true }).fill("");
    await staleEditor.getByRole("button", { name: "Run query", exact: true }).click();
    await expect(restartedPage.getByRole("status").filter({ hasText: "1–100 of 1,001" })).toBeVisible();
    await restartedPage.getByRole("button", { name: "Close query editor", exact: true }).click();
    await restartedPage.getByRole("checkbox", { name: "Select /Workspace review/workspace-0000.txt", exact: true }).check();
    await restartedPage.getByRole("button", { name: "Tag or recover", exact: true }).click();
    await staleActions.getByRole("combobox", { name: "Tag for snapshot action" }).click();
    await restartedPage.getByRole("option", { name: "Review checkpoint", exact: true }).click();
    await staleActions.getByRole("button", { name: "Remove tag from visible selection", exact: true }).click();
    const selection = restartedPage.getByRole("dialog", { name: "Tag selected documents" });
    await expect(selection.getByText("1 of 1 selected documents have this tag.", { exact: true })).toBeVisible();
    await selection.getByRole("button", { name: "Remove from all", exact: true }).click();
    await expect(selection.getByText("0 of 1 selected documents have this tag.", { exact: true })).toBeVisible();
    await selection.getByRole("button", { name: "Done", exact: true }).click();
    await restartedPage.getByRole("button", { name: "Tag or recover", exact: true }).click();
    await staleActions.getByRole("combobox", { name: "Tag for snapshot action" }).click();
    await restartedPage.getByRole("option", { name: "Review checkpoint", exact: true }).click();
    await staleActions.getByRole("button", { name: "Remove tag from whole query", exact: true }).click();
    await verifyCheckpoint(restartedPage, staleCheckpoint);
    await staleRecovery.getByText("Review exact targets", { exact: true }).click();
    await expect(staleRecovery.getByRole("table", { name: "Exact action targets" }).getByRole("row")).toHaveCount(51);
    await staleRecovery.getByRole("button", { name: "Next targets", exact: true }).click();
    await expect(staleRecovery.getByText("51–100 of 1000 in this batch", { exact: true })).toBeVisible();
    await staleRecovery.getByLabel("Target batch", { exact: true }).selectOption("1");
    await expect(staleRecovery.getByRole("table", { name: "Exact action targets" }).getByRole("row")).toHaveCount(2);
    await restartedPage.screenshot({ path: path.join(screenshots!, "web-snapshot-exact-targets.png"), animations: "disabled" });
    await staleRecovery.getByRole("checkbox", { name: /I confirm this vault/ }).check();
    await staleRecovery.getByRole("button", { name: "Confirm and run action", exact: true }).click();
    await expect(staleRecovery.getByText("Action complete. All batches have validated receipts.", { exact: true })).toBeVisible();
    expect((await collectRows(restartedRawURL, reviewQuery)).total).toBe(0);
    console.log("snapshot acceptance: selection revision reused across a two-batch whole-query action");

    // A deleted tag still permits recovery export and explicit abandonment.
    await staleRecovery.getByText("Close", { exact: true }).click();
    const currentTag = await restartedAPI.json<{ revision: number }>(`/api/v1/tags/${reviewTag.id}`);
    const deletion = await restartedAPI.request(`/api/v1/tags/${reviewTag.id}`, {
      method: "DELETE", headers: { "If-Match": String(currentTag.revision) },
    });
    expect(deletion.ok).toBe(true);
    await restartedPage.getByRole("button", { name: "Snapshot actions", exact: true }).click();
    await staleActions.getByRole("button", { name: "Resume retained action", exact: true }).click();
    await expect(staleRecovery.getByText(/tag is no longer available/)).toBeVisible();
    await expect(staleRecovery.getByRole("button", { name: "Save recovery checkpoint", exact: true })).toBeEnabled();
    await staleRecovery.getByRole("button", { name: "Abandon action…", exact: true }).click();
    await staleRecovery.getByRole("button", { name: "Abandon action without rollback", exact: true }).click();

    // Clear an unreadable journal without parsing it, preserving another vault.
    await restartedPage.evaluate(async (vaultID) => {
      const database = await new Promise<IDBDatabase>((resolve, reject) => {
        const request = indexedDB.open("docbank-action-journal-v1", 1);
        request.onsuccess = () => resolve(request.result);
        request.onerror = () => reject(request.error);
      });
      const transaction = database.transaction(["actions", "batches"], "readwrite");
      transaction.objectStore("actions").put({ vault_id: vaultID, version: 99 });
      transaction.objectStore("batches").put({ vault_id: vaultID, index: 999 });
      transaction.objectStore("actions").put({ vault_id: "99999999-9999-4999-8999-999999999999", version: 99 });
      await new Promise<void>((resolve, reject) => {
        transaction.oncomplete = () => resolve();
        transaction.onabort = () => reject(transaction.error);
      });
      database.close();
    }, saved.vault_id);
    await restartedPage.getByRole("button", { name: "Snapshot actions", exact: true }).click();
    await staleActions.getByRole("button", { name: "Resume retained action", exact: true }).click();
    await expect(staleActions.getByRole("alert")).toContainText("journal is inconsistent");
    await staleActions.getByRole("button", { name: "Abandon retained action…", exact: true }).click();
    await staleActions.getByRole("button", { name: "Abandon retained action without rollback", exact: true }).click();
    await expect(staleActions).toHaveCount(0);
    const remaining = await retainedAction(restartedPage);
    expect(remaining.header.vault_id).toBe("99999999-9999-4999-8999-999999999999");
    expect(remaining.batches).toHaveLength(0);
    // Dismiss after IndexedDB accepts the header write but before its
    // transaction commits. The pending preparation must roll back as a unit.
    await restartedPage.evaluate(() => {
      const add = IDBObjectStore.prototype.add;
      IDBObjectStore.prototype.add = function (...args) {
        const request = add.apply(this, args);
        if (this.name === "actions") {
          IDBObjectStore.prototype.add = add;
          request.addEventListener("success", () => {
            const dialog = document.querySelector('[aria-label="Tag frozen snapshot"]');
            const cancel = Array.from(dialog!.querySelectorAll("button")).find((button) => button.textContent?.trim() === "Cancel");
            if (!cancel) throw new Error("Capture cancellation control is missing");
            cancel.click();
          }, { once: true });
        }
        return request;
      };
    });
    await restartedPage.getByRole("button", { name: "Tag or recover", exact: true }).click();
    await staleActions.getByRole("combobox", { name: "Tag for snapshot action" }).click();
    await restartedPage.getByRole("option", { name: "Fence marker", exact: true }).click();
    await staleActions.getByRole("button", { name: "Add tag to whole query", exact: true }).click();
    await expect(staleActions).toHaveCount(0);
    const afterCancel = await retainedAction(restartedPage);
    expect(afterCancel.header.vault_id).toBe("99999999-9999-4999-8999-999999999999");
    expect(afterCancel.batches).toHaveLength(0);
    console.log("snapshot acceptance: cancellation aborts pending journal writes");
    await restartedPage.getByRole("button", { name: "Tag or recover", exact: true }).click();
    await staleActions.getByRole("combobox", { name: "Tag for snapshot action" }).click();
    await restartedPage.getByRole("option", { name: "Fence marker", exact: true }).click();
    await staleActions.getByRole("button", { name: "Add tag to whole query", exact: true }).click();
    await expect(staleRecovery.getByText("1001 exact documents", { exact: true })).toBeVisible();
    console.log("snapshot acceptance: deleted tag and corrupt journal can be abandoned; new action prepared");
    await verifyCheckpoint(restartedPage, checkpoint);
    const finalTag = await restartedAPI.json<{ revision: number }>(`/api/v1/tags/${fenceTag.id}`);
    const finalDeletion = await restartedAPI.request(`/api/v1/tags/${fenceTag.id}`, {
      method: "DELETE", headers: { "If-Match": String(finalTag.revision) },
    });
    expect(finalDeletion.ok).toBe(true);
    await staleRecovery.getByRole("checkbox", { name: /I confirm this vault/ }).check();
    await staleRecovery.getByRole("button", { name: "Confirm and run action", exact: true }).click();
    await expect(staleRecovery.getByText(/Abandon this action and create a new one with a fresh selection/)).toBeVisible();
    expect((await retainedAction(restartedPage, saved.vault_id)).header.state).toBe("stale");
    await expect(staleRecovery.getByRole("button", { name: /run action|same operation|resume action/ })).toHaveCount(0);
    await staleRecovery.getByRole("button", { name: "Abandon action…", exact: true }).click();
    await staleRecovery.getByRole("button", { name: "Abandon action without rollback", exact: true }).click();
    await expect(staleRecovery).toHaveCount(0);
    console.log("snapshot acceptance: tag deleted after recovery opened stops the action without retry");

  } finally {
    const cleanupFailures: string[] = [];
    for (const [label, runner] of [["wrong-vault", runWrong], ["main", run]] as const) {
      try {
        if (JSON.parse(await runner("daemon", "status", "--json")).running === true) {
          await runner("daemon", "stop");
        }
        if (JSON.parse(await runner("daemon", "status", "--json")).running !== false) {
          cleanupFailures.push(`${label} daemon still running`);
        }
      } catch (error) {
        cleanupFailures.push(`${label} daemon: ${String(error)}`);
      }
    }
    try {
      await rm(workspace, { recursive: true, force: true });
    } catch (error) {
      cleanupFailures.push(`workspace ${workspace}: ${String(error)}`);
    }
    if (cleanupFailures.length) throw new Error(`Synthetic snapshot cleanup failed: ${cleanupFailures.join("; ")}`);
  }
});
