// @vitest-environment node
import { webcrypto } from "node:crypto";
import { IDBFactory, IDBKeyRange } from "fake-indexeddb";
import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { ActionJournal } from "./actionJournal.js";
import { encodeRecovery, prepareAction } from "./actionRecovery.js";
import {
  snapshotMemberHash,
  type SnapshotMember,
  type WorkspaceQueryResponse,
} from "./snapshots.js";
import { SnapshotWorkspace } from "./snapshotWorkspace.svelte.js";
import { snapshotEnvelope } from "./snapshot-test-fixtures.js";
import { parseQuery } from "./query.js";

const vaultID = "11111111-1111-4111-8111-111111111111";
const member: SnapshotMember = {
  node_id: 7,
  content_version_id: "33333333-3333-4333-8333-333333333333",
  blob_hash: "a".repeat(64),
  size: 42,
  revision: 3,
};

beforeEach(() => {
  vi.stubGlobal("crypto", webcrypto);
  vi.stubGlobal("indexedDB", new IDBFactory());
  vi.stubGlobal("IDBKeyRange", IDBKeyRange);
  vi.spyOn(globalThis, "fetch").mockImplementation(async () =>
    Response.json({ vault_id: vaultID }),
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

async function prepareOne() {
  return prepareAction(
    vaultID,
    {
      snapshot: {
        snapshot_id: "b".repeat(32),
        snapshot_fingerprint: `sha256:${"c".repeat(64)}`,
        query_fingerprint: `sha256:${"d".repeat(64)}`,
        member_hash: await snapshotMemberHash([member]),
        total: 1,
        total_bytes: member.size,
      } as WorkspaceQueryResponse,
      members: [member],
    },
    "22222222-2222-4222-8222-222222222222",
    true,
  );
}

async function retainSendingAction(): Promise<string> {
  const prepared = await prepareOne();
  const journal = await ActionJournal.open(vaultID);
  await journal.prepare(prepared);
  await journal.verifyCheckpoint(await encodeRecovery(prepared));
  await journal.markSending(0);
  return prepared.action_id;
}

it("reports a retained run with unknown progress state as unreadable", async () => {
  await retainSendingAction();
  await new Promise<void>((resolve, reject) => {
    const open = indexedDB.open("docbank-action-journal-v1", 1);
    open.onerror = () => reject(open.error);
    open.onsuccess = () => {
      const database = open.result;
      const transaction = database.transaction("actions", "readwrite");
      const store = transaction.objectStore("actions");
      const read = store.get(vaultID);
      read.onsuccess = () => store.put({ ...read.result, state: "from-a-newer-build" });
      transaction.oncomplete = () => {
        database.close();
        resolve();
      };
      transaction.onerror = () => reject(transaction.error);
    };
  });
  const workspace = new SnapshotWorkspace(() => "session", vi.fn(), vi.fn());

  await workspace.refreshRetained();

  expect(workspace.retained).toBe("unreadable");
});

it("notices an unfinished run without touching its in-flight batch", async () => {
  const actionID = await retainSendingAction();
  const workspace = new SnapshotWorkspace(() => "session", vi.fn(), vi.fn());

  await workspace.refreshRetained();

  const retained = workspace.retained;
  expect(retained !== null && retained !== "unreadable" && retained.action_id).toBe(actionID);
  // Another tab may still be sending; only opening the run for resume may mark it uncertain.
  expect((await ActionJournal.peek(vaultID))?.batches[0].state).toBe("sending");

  await workspace.abandonAction();
  expect(workspace.retained).toBeNull();
  await workspace.refreshRetained();
  expect(workspace.retained).toBeNull();
});

it("shows a retained run instead of enumerating results for a new Tag all", async () => {
  const actionID = await retainSendingAction();
  const workspace = new SnapshotWorkspace(() => "session", vi.fn(), vi.fn());
  const query = parseQuery(JSON.stringify({ text: "report" }));
  const rows = Array.from({ length: 100 }, (_, index) => ({
    ...member,
    node_id: index + 1,
    name: `report-${index + 1}.txt`,
    path: `/report-${index + 1}.txt`,
    mime_type: "text/plain",
    media_family: "text" as const,
    modified_at: "2026-10-01T00:00:00Z",
    sort_key: `report-${index + 1}.txt`,
    tags: [],
    collection_ids: [],
  }));
  vi.mocked(fetch).mockImplementation(async (input) =>
    String(input) === "/api/v1/workspace/queries"
      ? Response.json(
          await snapshotEnvelope(query, rows, {
            page_size: 100,
            total: 101,
            total_bytes: 101 * member.size,
            member_hash: await snapshotMemberHash([...rows, { ...member, node_id: 101 }]),
            next_cursor: "next-page",
          }),
        )
      : Response.json({ vault_id: vaultID }),
  );
  await workspace.run(query, { page_size: 100 });
  await workspace.refreshRetained();
  vi.mocked(fetch).mockClear();
  workspace.openActions("query");

  await workspace.startAction({
    scope: "query",
    tagID: "22222222-2222-4222-8222-222222222222",
    assign: true,
  });

  const retained = workspace.retained;
  expect(retained !== null && retained !== "unreadable" && retained.action_id).toBe(actionID);
  expect(workspace.actionsOpen).toBe(false);
  expect(workspace.actionError).toBe("");
  expect(workspace.recoveryAction).toBeNull();
  // Only the vault identity was read; no result page was enumerated.
  expect(vi.mocked(fetch).mock.calls.map(([url]) => String(url))).toEqual(["/api/v1/audit/status"]);
  // The retained run is untouched.
  expect((await ActionJournal.peek(vaultID))?.action_id).toBe(actionID);
});

it("preserves another tab's sending run when completed-run replacement pauses", async () => {
  await retainSendingAction();
  const journal = await ActionJournal.open(vaultID);
  const action = (await journal.load())!;
  const batch = action.batches[0];
  await journal.recordReceipt(0, {
    version: 1,
    operation_id: batch.operation_id,
    request_digest: batch.request_digest,
    tag_id: action.tag_id,
    assign: true,
    tag_revision: 2,
    assignment_count: 1,
    completed_at: "2026-10-01T00:00:00.000000000Z",
    nodes: [{ node_id: 7, expected_revision: 3, revision: 4, changed: true }],
  });
  const query = parseQuery(JSON.stringify({ text: "report" }));
  const page = await snapshotEnvelope(query, [
    {
      ...member,
      name: "report.txt",
      path: "/report.txt",
      mime_type: "text/plain",
      media_family: "text" as const,
      modified_at: "2026-10-01T00:00:00Z",
      sort_key: "report.txt",
      tags: [],
      collection_ids: [],
    },
  ]);
  vi.mocked(fetch).mockImplementation(async (input) =>
    String(input) === "/api/v1/workspace/queries"
      ? Response.json(page)
      : Response.json({ vault_id: vaultID }),
  );
  const tabA = new SnapshotWorkspace(() => "session", vi.fn(), vi.fn());
  const tabB = new SnapshotWorkspace(() => "session", vi.fn(), vi.fn());
  await tabA.run(query, { page_size: 250 });
  await tabB.run(query, { page_size: 250 });
  let notifyPaused!: () => void;
  const paused = new Promise<void>((resolve) => {
    notifyPaused = resolve;
  });
  let resume!: () => void;
  const resumed = new Promise<void>((resolve) => {
    resume = resolve;
  });
  const open = ActionJournal.open.bind(ActionJournal);
  vi.spyOn(ActionJournal, "open").mockImplementationOnce(async (id) => {
    const opened = await open(id);
    const load = opened.load.bind(opened);
    vi.spyOn(opened, "load").mockImplementationOnce(async () => {
      const observed = await load();
      notifyPaused();
      await resumed;
      return observed;
    });
    return opened;
  });
  const choice = { scope: "query" as const, tagID: action.tag_id, assign: true };
  const startingA = tabA.startAction(choice);
  await paused;
  await tabB.startAction(choice);
  expect(tabB.actionError).toBe("");
  const next = (await ActionJournal.peek(vaultID))!;
  expect(next.action_id).not.toBe(action.action_id);
  const sending = await open(vaultID);
  await sending.verifyCheckpoint(await encodeRecovery(next));
  await sending.markSending(0);
  resume();
  await startingA;

  const retained = (await ActionJournal.peek(vaultID))!;
  expect(retained.action_id).toBe(next.action_id);
  expect(retained.batches[0].state).toBe("sending");
  expect(tabA.recoveryAction).toBeNull();
  expect(tabA.actionError).not.toBe("");
});

it("opens an imported run in place of a finished one", async () => {
  await retainSendingAction();
  const journal = await ActionJournal.open(vaultID);
  const finished = (await journal.load())!;
  const batch = finished.batches[0];
  await journal.recordReceipt(0, {
    version: 1,
    operation_id: batch.operation_id,
    request_digest: batch.request_digest,
    tag_id: finished.tag_id,
    assign: true,
    tag_revision: 2,
    assignment_count: 1,
    completed_at: "2026-10-01T00:00:00.000000000Z",
    nodes: [{ node_id: 7, expected_revision: 3, revision: 4, changed: true }],
  });
  expect((await journal.load())?.state).toBe("complete");
  const imported = await prepareOne();
  const workspace = new SnapshotWorkspace(() => "session", vi.fn(), vi.fn());

  await workspace.importAction(await encodeRecovery(imported));

  expect(workspace.actionError).toBe("");
  expect(workspace.recoveryAction?.action_id).toBe(imported.action_id);
  expect((await ActionJournal.peek(vaultID))?.action_id).toBe(imported.action_id);
});

it("refuses an imported run while another is unfinished", async () => {
  const actionID = await retainSendingAction();
  const workspace = new SnapshotWorkspace(() => "session", vi.fn(), vi.fn());

  await workspace.importAction(await encodeRecovery(await prepareOne()));

  expect(workspace.actionError).toContain("already has an action retained");
  expect(workspace.recoveryAction).toBeNull();
  expect((await ActionJournal.peek(vaultID))?.action_id).toBe(actionID);
});
