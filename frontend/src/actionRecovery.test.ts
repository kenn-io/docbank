// @vitest-environment node
import { describe, expect, it } from "vitest";
import { prepareAction, ACTION_MAX_BYTES, type PreparedAction } from "./actionJournal.js";
import { decodeRecovery, encodeRecovery } from "./actionRecovery.js";
import { snapshotMemberHash, type SnapshotMember, type VerifiedSnapshotTargets, type WorkspaceQueryResponse } from "./snapshots.js";

const vaultID = "11111111-1111-4111-8111-111111111111";
const tagID = "22222222-2222-4222-8222-222222222222";

async function fixture(): Promise<PreparedAction> {
  const members: SnapshotMember[] = [{
    node_id: 7, content_version_id: "33333333-3333-4333-8333-333333333333",
    blob_hash: "a".repeat(64), size: 11, revision: 4,
  }];
  const targets: VerifiedSnapshotTargets = { snapshot: {
    snapshot_id: "b".repeat(32), snapshot_fingerprint: `sha256:${"c".repeat(64)}`,
    query_fingerprint: `sha256:${"d".repeat(64)}`, member_hash: await snapshotMemberHash(members),
    total: 1, total_bytes: 11,
  } as WorkspaceQueryResponse, members };
  return prepareAction(vaultID, targets, tagID, true);
}

function bytes(value: unknown): Uint8Array {
  return new TextEncoder().encode(JSON.stringify(value));
}

describe("recovery file codec", () => {
  it("round trips canonical actions with missing receipts", async () => {
    const action = await fixture();
    const encoded = await encodeRecovery(action);
    await expect(decodeRecovery(encoded)).resolves.toEqual(action);
  });

  it.each([
    ["operation", (raw: any) => { raw.batches[0].request.operation_id = tagID; }],
    ["digest", (raw: any) => { raw.batches[0].request_digest = "0".repeat(64); }],
    ["member", (raw: any) => { raw.batches[0].members[0].revision = 5; }],
    ["unsafe number", (raw: any) => { raw.total = Number.MAX_SAFE_INTEGER + 1; }],
    ["unknown field", (raw: any) => { raw.payload = "malicious"; }],
  ])("rejects altered %s identity", async (_name, mutate) => {
    const raw: any = JSON.parse(new TextDecoder().decode(await encodeRecovery(await fixture())));
    mutate(raw);
    await expect(decodeRecovery(bytes(raw))).rejects.toThrow();
  });

  it("rejects duplicate keys and alternate encodings", async () => {
    const text = new TextDecoder().decode(await encodeRecovery(await fixture()));
    const duplicate = text.replace("{", `{"version":1,`);
    await expect(decodeRecovery(new TextEncoder().encode(duplicate))).rejects.toThrow();
    await expect(decodeRecovery(new TextEncoder().encode(` ${text}`))).rejects.toThrow();
  });

  it("rejects recovery payloads over 128 MiB before parsing", async () => {
    await expect(decodeRecovery(new Uint8Array(ACTION_MAX_BYTES + 1))).rejects.toThrow("128 MiB");
  });

  it("exports only the minimal validated receipt projection", async () => {
    const action = await fixture();
    const request = action.batches[0].request;
    const withReceipt = { ...action, batches: [{ ...action.batches[0], receipt: {
      version: 1, operation_id: request.operation_id, request_digest: action.batches[0].request_digest,
      tag_id: tagID, assign: true, tag_revision: 2, assignment_count: 1,
      completed_at: "2026-09-11T00:00:00.000000000Z",
      nodes: [{ node_id: 7, expected_revision: 4, revision: 4, changed: false }],
      transport_debug: "must-not-leak",
    } }] } as unknown as PreparedAction;
    const encoded = await encodeRecovery(withReceipt);
    expect(new TextDecoder().decode(encoded)).not.toContain("transport_debug");
    expect((await decodeRecovery(encoded)).batches[0].receipt).toBeDefined();
  });
});
