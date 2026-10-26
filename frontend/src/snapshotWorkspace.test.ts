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

async function retainSendingAction(): Promise<string> {
  const prepared = await prepareAction(
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
  workspace.state = {
    status: "ready",
    firstPage: { snapshot_id: "b".repeat(32), total: 101 },
  } as unknown as SnapshotWorkspace["state"];
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
