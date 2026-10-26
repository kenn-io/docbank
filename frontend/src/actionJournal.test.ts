// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { webcrypto } from "node:crypto";
import { IDBFactory, IDBKeyRange } from "fake-indexeddb";
import { ActionJournal } from "./actionJournal.js";
import { encodeRecovery, prepareAction } from "./actionRecovery.js";
import {
  snapshotMemberHash,
  type SnapshotMember,
  type VerifiedSnapshotTargets,
  type WorkspaceQueryResponse,
} from "./snapshots.js";

const vaultID = "11111111-1111-4111-8111-111111111111";
const tagID = "22222222-2222-4222-8222-222222222222";
const versionID = "33333333-3333-4333-8333-333333333333";

async function targets(members: SnapshotMember[]): Promise<VerifiedSnapshotTargets> {
  const totalBytes = members.reduce((sum, member) => sum + member.size, 0);
  return {
    snapshot: {
      query_fingerprint: `sha256:${"a".repeat(64)}`,
      member_hash: await snapshotMemberHash(members),
      snapshot_fingerprint: `sha256:${"c".repeat(64)}`,
      snapshot_id: "d".repeat(32),
      total: members.length,
      total_bytes: totalBytes,
    } as WorkspaceQueryResponse,
    members,
  };
}

function member(nodeID: number): SnapshotMember {
  return {
    node_id: nodeID,
    content_version_id: versionID,
    blob_hash: "e".repeat(64),
    size: 7,
    revision: nodeID,
  };
}

describe("action preparation", () => {
  it("freezes exact source members into canonical bounded requests", async () => {
    const action = await prepareAction(vaultID, await targets([member(2), member(1)]), tagID, true);

    expect(action.version).toBe(1);
    expect(action.total).toBe(2);
    expect(action.batches).toHaveLength(1);
    expect(action.batches[0].members.map((item) => item.node_id)).toEqual([1, 2]);
    expect(action.batches[0].request.nodes).toEqual([
      { node_id: 1, revision: 1 },
      { node_id: 2, revision: 2 },
    ]);
    expect(action.batches[0].request.operation_id).toBe(action.batches[0].operation_id);
    expect(action.batches[0].request_digest).toMatch(/^[0-9a-f]{64}$/);
    expect(action.plan_digest).toMatch(/^[0-9a-f]{64}$/);
  });

  it("rejects 250,001 members before preparing an action", async () => {
    const oversized = Array.from({ length: 250_001 }, (_, index) => member(index + 1));
    await expect(prepareAction(vaultID, await targets(oversized), tagID, true)).rejects.toThrow(
      "250,000",
    );
  });
});

beforeEach(() => {
  vi.stubGlobal("crypto", webcrypto);
  vi.stubGlobal("indexedDB", new IDBFactory());
  vi.stubGlobal("IDBKeyRange", IDBKeyRange);
});
afterEach(() => vi.unstubAllGlobals());

it.each(["unfinished", "replaced"])(
  "preserves a %s run during completed-run replacement",
  async (state) => {
    const first = await prepareAction(vaultID, await targets([member(7)]), tagID, true);
    const journal = await ActionJournal.open(vaultID);
    await journal.prepare(first);
    await journal.verifyCheckpoint(await encodeRecovery(first));
    await journal.markSending(0);
    let retained = first;
    if (state === "replaced") {
      const batch = first.batches[0];
      await journal.recordReceipt(0, {
        version: 1,
        operation_id: batch.operation_id,
        request_digest: batch.request_digest,
        tag_id: tagID,
        assign: true,
        tag_revision: 2,
        assignment_count: 1,
        completed_at: "2026-10-01T00:00:00.000000000Z",
        nodes: [{ node_id: 7, expected_revision: 7, revision: 8, changed: true }],
      });
      retained = await prepareAction(vaultID, await targets([member(7)]), tagID, true);
      await journal.prepare(retained, undefined, first.action_id);
      await journal.verifyCheckpoint(await encodeRecovery(retained));
      await journal.markSending(0);
    }
    const replacement = await prepareAction(vaultID, await targets([member(7)]), tagID, true);
    await expect(journal.prepare(replacement, undefined, first.action_id)).rejects.toThrow(
      "already has an action retained",
    );
    const stored = await ActionJournal.peek(vaultID);
    expect(stored?.action_id).toBe(retained.action_id);
    expect(stored?.batches[0].state).toBe("sending");
  },
);
