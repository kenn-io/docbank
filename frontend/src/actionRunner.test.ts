// @vitest-environment node
import { prepareAction } from "./actionRecovery.js";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { BatchTagReceipt } from "./batch-tags.js";
import { type ActionJournalAccess, type PersistedAction } from "./actionJournal.js";
import { runAction } from "./actionRunner.js";
import { snapshotMemberHash, type SnapshotMember, type VerifiedSnapshotTargets, type WorkspaceQueryResponse } from "./snapshots.js";

const vaultID = "11111111-1111-4111-8111-111111111111";
const tagID = "22222222-2222-4222-8222-222222222222";

async function persisted(count = 1): Promise<PersistedAction> {
  const members: SnapshotMember[] = Array.from({ length: count }, (_, index) => {
    const nodeID = index + 1;
    return { node_id: nodeID, content_version_id: `33333333-3333-4333-8333-${String(nodeID).padStart(12, "0")}`, blob_hash: "a".repeat(64), size: 11, revision: 4 };
  });
  const targets: VerifiedSnapshotTargets = { snapshot: {
    snapshot_id: "b".repeat(32), snapshot_fingerprint: `sha256:${"c".repeat(64)}`,
    query_fingerprint: `sha256:${"d".repeat(64)}`, member_hash: await snapshotMemberHash(members), total: count, total_bytes: count * 11,
  } as WorkspaceQueryResponse, members };
  const action = await prepareAction(vaultID, targets, tagID, true);
  return { ...action, state: "prepared", checkpoint_verified: true, batches: action.batches.map((batch) => ({ ...batch, state: "prepared" })) };
}

class MemoryJournal implements ActionJournalAccess {
  action: PersistedAction;
  confirmed = true;
  requests: string[] = [];
  constructor(action: PersistedAction) { this.action = action; }
  async load() { return this.action; }
  async loadBatch(index: number) {
    const action = this.action!;
    return { action_id: action.action_id, state: action.state,
      checkpoint_verified: action.checkpoint_verified, batch: action.batches[index] };
  }
  consumeResumeConfirmation(actionID: string) { const yes = this.confirmed && actionID === this.action.action_id; this.confirmed = false; return yes; }
  async markSending(index: number) { this.set(index, "sending", "sending"); }
  async recordReceipt(index: number, receipt: BatchTagReceipt) {
    const batches = this.action.batches.map((batch) => batch.index === index ? { ...batch, state: "complete" as const, receipt } : batch);
    const complete = batches.every((batch) => batch.state === "complete");
    this.action = { ...this.action, state: complete ? "complete" : (this.action.state === "paused" ? "paused" : "sending"), batches };
  }
  async markUncertain(index: number) { this.set(index, "uncertain", "uncertain"); }
  async markStale(index: number) { this.set(index, "stale", "stale"); }
  private set(index: number, batchState: PersistedAction["batches"][number]["state"], state: PersistedAction["state"]) {
    this.action = { ...this.action, state, batches: this.action.batches.map((batch) => batch.index === index ? { ...batch, state: batchState } : batch) };
  }
}

function audit(id = vaultID): Response { return new Response(JSON.stringify({ vault_id: id, unrelated: "discard" }), { status: 200 }); }
function receipt(action: PersistedAction, index = 0): BatchTagReceipt { const batch = action.batches[index]; return {
  version: 1, operation_id: batch.operation_id, request_digest: batch.request_digest, tag_id: tagID, assign: true,
  tag_revision: batch.request.nodes.length + 1, assignment_count: batch.request.nodes.length, completed_at: "2026-09-11T00:00:00.000000000Z",
  nodes: batch.request.nodes.map((node) => ({ node_id: node.node_id, expected_revision: node.revision, revision: node.revision + 1, changed: true })),
}; }

afterEach(() => vi.restoreAllMocks());

describe("recoverable action runner", () => {
  it("loads the full plan only at the start and end of a multi-batch run", async () => {
    const action = await persisted(2_001);
    const journal = new MemoryJournal(action);
    const load = vi.spyOn(journal, "load");
    let index = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => input === "/api/v1/audit/status"
      ? audit() : new Response(JSON.stringify(receipt(action, index++))));
    const completed = await runAction("session", journal, new AbortController().signal, () => {});
    expect(completed.state).toBe("complete");
    expect(completed.batches).toHaveLength(3);
    expect(load).toHaveBeenCalledTimes(2);
  });

  it("publishes revision evidence only for receipts returned by the daemon", async () => {
    const base = await persisted(1_001);
    const imported = receipt(base, 0);
    const action: PersistedAction = { ...base, batches: base.batches.map((batch) => batch.index === 0
      ? { ...batch, state: "complete", receipt: imported } : batch) };
    const journal = new MemoryJournal(action);
    const confirmed = receipt(base, 1);
    const observed: BatchTagReceipt[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => input === "/api/v1/audit/status"
      ? audit() : new Response(JSON.stringify(confirmed)));
    await runAction("session", journal, new AbortController().signal, (_action, receipt) => {
      if (receipt) observed.push(receipt);
    });
    expect(observed).toEqual([confirmed]);
    expect(journal.action.state).toBe("complete");
  });

  it.each([
    { status: 412, code: "stale_revision", detail: "Document changed" },
    { status: 404, code: "not_found", detail: "Tag not found" },
    { status: 404, code: "not_found", detail: "Batch target is missing or trashed" },
  ])("stops a batch after $detail without changing its captured inputs", async ({ status, code, detail }) => {
    const journal = new MemoryJournal(await persisted(1_001));
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      if (input === "/api/v1/audit/status") return audit();
      return new Response(JSON.stringify({ code, detail }), { status });
    });

    await expect(runAction("fresh-session", journal, new AbortController().signal, () => {})).rejects.toThrow(detail);
    expect(journal.action.state).toBe("stale");
    expect(journal.action.batches.map((batch) => batch.state)).toEqual(["stale", "prepared"]);
  });

  it("does not mutate without checkpoint verification and explicit fresh confirmation", async () => {
    const action = { ...await persisted(), checkpoint_verified: false };
    const journal = new MemoryJournal(action);
    const fetch = vi.spyOn(globalThis, "fetch");
    await expect(runAction("fresh-session", journal, new AbortController().signal, () => {})).rejects.toThrow("checkpoint");
    expect(fetch).not.toHaveBeenCalled();

    journal.action = { ...journal.action, checkpoint_verified: true };
    journal.confirmed = false;
    await expect(runAction("fresh-session", journal, new AbortController().signal, () => {})).rejects.toThrow("confirm");
    expect(fetch).not.toHaveBeenCalled();
  });

  it("rejects a wrong current vault before mutation", async () => {
    const journal = new MemoryJournal(await persisted());
    const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(audit("99999999-9999-4999-8999-999999999999"));
    await expect(runAction("fresh-session", journal, new AbortController().signal, () => {})).rejects.toThrow("different vault");
    expect(fetch.mock.calls.filter(([url]) => url === "/api/v1/batch/tags")).toHaveLength(0);
  });

  it("replays the byte-identical original request after response loss", async () => {
    const journal = new MemoryJournal(await persisted());
    const sent: string[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      if (input === "/api/v1/audit/status") return audit();
      sent.push(String(init?.body));
      if (sent.length === 1) throw new TypeError("Synthetic response loss");
      return new Response(JSON.stringify(receipt(journal.action)), { status: 200 });
    });

    await expect(runAction("fresh-session", journal, new AbortController().signal, () => {})).rejects.toThrow("response loss");
    expect(await journal.load()).toMatchObject({ state: "uncertain" });
    journal.confirmed = true;
    await runAction("fresh-session", journal, new AbortController().signal, () => {});

    const firstRequest = JSON.parse(sent[0]);
    const retryRequest = JSON.parse(sent[1]);
    expect(firstRequest.operation_id).toBe(retryRequest.operation_id);
    expect(JSON.stringify(firstRequest)).toBe(JSON.stringify(retryRequest));
    expect(journal.action.state).toBe("complete");
  });

  it("fails closed when the durable sending marker cannot be written", async () => {
    const journal = new MemoryJournal(await persisted());
    journal.markSending = async () => { throw new Error("Synthetic storage failure"); };
    let networkCalls = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      if (input === "/api/v1/audit/status") return audit();
      networkCalls++;
      return new Response();
    });
    await expect(runAction("fresh-session", journal, new AbortController().signal, () => {})).rejects.toThrow("storage failure");
    expect(networkCalls).toBe(0);
  });

  it("records an in-flight receipt after pause and schedules no next batch", async () => {
    const journal = new MemoryJournal(await persisted(1_001));
    let networkCalls = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      if (input === "/api/v1/audit/status") return audit();
      const index = networkCalls++;
      journal.action = { ...journal.action, state: "paused" };
      return new Response(JSON.stringify(receipt(journal.action, index)), { status: 200 });
    });
    const result = await runAction("fresh-session", journal, new AbortController().signal, () => {});
    expect(networkCalls).toBe(1);
    expect(result.state).toBe("paused");
    expect(result.batches[0].receipt).toBeDefined();
    expect(result.batches[1].receipt).toBeUndefined();
  });
});

it("does not send a batch cancelled while its durable sending marker is being written", async () => {
  const journal = new MemoryJournal(await persisted());
  const markSending = journal.markSending.bind(journal);
  let release!: () => void;
  const pending = new Promise<void>((resolve) => { release = resolve; });
  const sending = vi.spyOn(journal, "markSending").mockImplementation(async (index) => {
    await pending;
    await markSending(index);
  });
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => input === "/api/v1/audit/status"
    ? audit() : new Response(JSON.stringify(receipt(journal.action))));
  const controller = new AbortController();
  const run = runAction("session", journal, controller.signal, () => {});
  await vi.waitFor(() => expect(sending).toHaveBeenCalledOnce());
  controller.abort();
  release();
  await run;
  expect(fetch.mock.calls.map(([url]) => url)).toEqual(["/api/v1/audit/status"]);
  expect(journal.action.batches[0].receipt).toBeUndefined();
});
