import { webcrypto } from "node:crypto";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import ActionRecoveryModal, { type ActionRecoveryJournal } from "./ActionRecoveryModal.svelte";
import { prepareAction, type PersistedAction } from "./actionJournal.js";
import { encodeRecovery } from "./actionRecovery.js";
import type { BatchTagReceipt } from "./batch-tags.js";
import { snapshotMemberHash, type SnapshotMember, type SnapshotPage } from "./snapshots.js";

const vaultID = "11111111-1111-4111-8111-111111111111";
const tag = { id: "22222222-2222-4222-8222-222222222222", name: "Review", revision: 2, assignment_count: 7 };
const member: SnapshotMember = {
  node_id: 7,
  content_version_id: "33333333-3333-4333-8333-333333333333",
  blob_hash: "a".repeat(64),
  size: 42,
  revision: 3,
};

async function prepared(): Promise<PersistedAction> {
  const memberHash = await snapshotMemberHash([member]);
  const action = await prepareAction(vaultID, {
    snapshot: {
      snapshot_id: "b".repeat(32), snapshot_fingerprint: `sha256:${"c".repeat(64)}`,
      query_fingerprint: `sha256:${"d".repeat(64)}`, member_hash: memberHash,
      total: 1, total_bytes: member.size,
    } as SnapshotPage,
    members: [member],
  }, tag.id, true);
  return {
    ...action,
    state: "prepared",
    checkpoint_verified: false,
    batches: action.batches.map((batch) => ({ ...batch, state: "prepared" })),
  };
}

class MemoryJournal implements ActionRecoveryJournal {
  action: PersistedAction | null;
  confirmation = "";
  batchCalls = 0;
  checkpointReads = 0;

  constructor(action: PersistedAction) { this.action = action; }
  async load() { return this.action; }
  async verifyCheckpoint(_bytes: Uint8Array) {
    this.checkpointReads++;
    this.action = { ...this.action!, checkpoint_verified: true };
  }
  async confirmResume(actionID: string) { this.confirmation = actionID; }
  consumeResumeConfirmation(actionID: string) {
    const confirmed = this.confirmation === actionID;
    this.confirmation = "";
    return confirmed;
  }
  async markSending(index: number) {
    this.batchCalls++;
    const action = this.action!;
    this.action = { ...action, state: "sending", batches: action.batches.map((batch) => batch.index === index ? { ...batch, state: "sending" } : batch) };
  }
  async recordReceipt(index: number, receipt: BatchTagReceipt) {
    const action = this.action!;
    this.action = { ...action, state: "complete", batches: action.batches.map((batch) => batch.index === index ? { ...batch, state: "complete", receipt } : batch) };
  }
  async markUncertain(index: number) {
    const action = this.action!;
    this.action = { ...action, state: "uncertain", batches: action.batches.map((batch) => batch.index === index ? { ...batch, state: "uncertain" } : batch) };
  }
  async markStale(index: number) {
    const action = this.action!;
    this.action = { ...action, state: "stale", batches: action.batches.map((batch) => batch.index === index ? { ...batch, state: "stale" } : batch) };
  }
  async pause() { this.action = { ...this.action!, state: "paused" }; }
  async abandon() { this.action = null; }
}

beforeEach(() => {
  vi.stubGlobal("crypto", webcrypto);
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  vi.stubGlobal("URL", { createObjectURL: vi.fn(() => "blob:checkpoint"), revokeObjectURL: vi.fn() });
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
});

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

it("saves a credential-free checkpoint and requires its readback before the first mutation", async () => {
  const action = await prepared();
  const journal = new MemoryJournal(action);
  let persistedText = "";
  vi.mocked(URL.createObjectURL).mockImplementation((blob) => {
    void (blob as Blob).text().then((text) => { persistedText = text; });
    return "blob:checkpoint";
  });
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    if (String(input) === "/api/v1/audit/status") {
      return new Response(JSON.stringify({ vault_id: vaultID }), { status: 200 });
    }
    const request = JSON.parse(String(init?.body));
    return new Response(JSON.stringify({
      version: 1, operation_id: request.operation_id, request_digest: action.batches[0].request_digest,
      tag_id: tag.id, assign: true, tag_revision: 2, assignment_count: 1,
      completed_at: "2026-09-11T00:00:00.000000000Z",
      nodes: [{ node_id: 7, expected_revision: 3, revision: 4, changed: true }],
    }), { status: 200 });
  });

  render(ActionRecoveryModal, {
    session: "browser-session-secret",
    sessionVaultID: vaultID,
    journal,
    initialAction: action,
    tag,
    onprogress: vi.fn(),
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });

  expect(screen.getByText(/Add “Review”/)).toBeTruthy();
  expect(screen.getByText(/1 exact document/)).toBeTruthy();
  expect((screen.getByRole("button", { name: /Confirm and run/ }) as HTMLButtonElement).disabled).toBe(true);
  await fireEvent.click(screen.getByRole("button", { name: "Save recovery checkpoint" }));
  await waitFor(() => expect(persistedText).not.toBe(""));
  expect(persistedText).not.toContain("browser-session-secret");
  expect(journal.batchCalls).toBe(0);

  const bytes = await encodeRecovery(action);
  const file = new File([Uint8Array.from(bytes)], "action.docbank-action.json", { type: "application/json" });
  await fireEvent.change(screen.getByLabelText("Select the saved recovery checkpoint"), {
    target: { files: [file] },
  });
  await screen.findByText("Recovery checkpoint verified.");
  expect(journal.checkpointReads).toBe(1);
  expect(journal.batchCalls).toBe(0);

  await fireEvent.click(screen.getByRole("checkbox", { name: /I confirm this vault, action, tag, operation, and exact target count/ }));
  await fireEvent.click(screen.getByRole("button", { name: /Confirm and run/ }));
  await screen.findByText(/Action complete/);
  expect(journal.batchCalls).toBe(1);
});

it("refuses a mismatched vault and explains stale actions require a new action", async () => {
  const action = await prepared();
  const stale = { ...action, state: "stale" as const, checkpoint_verified: true };
  const journal = new MemoryJournal(stale);
  render(ActionRecoveryModal, {
    session: "fresh-session",
    sessionVaultID: "99999999-9999-4999-8999-999999999999",
    journal,
    initialAction: stale,
    tag,
    onprogress: vi.fn(),
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });

  expect(screen.getByRole("alert").textContent).toContain("different vault");
  expect(screen.getByText(/Create an explicit new selection and action/)).toBeTruthy();
  expect(screen.queryByRole("button", { name: /Confirm and run/ })).toBeNull();
});

it("retries an uncertain batch with the exact retained operation and revisions", async () => {
  const base = await prepared();
  const uncertain: PersistedAction = {
    ...base,
    state: "uncertain",
    checkpoint_verified: true,
    batches: base.batches.map((batch) => ({ ...batch, state: "uncertain" })),
  };
  const journal = new MemoryJournal(uncertain);
  let sent: unknown;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    if (String(input) === "/api/v1/audit/status") {
      return new Response(JSON.stringify({ vault_id: vaultID }), { status: 200 });
    }
    sent = JSON.parse(String(init?.body));
    return new Response(JSON.stringify({
      version: 1, operation_id: uncertain.batches[0].operation_id,
      request_digest: uncertain.batches[0].request_digest, tag_id: tag.id, assign: true,
      tag_revision: 2, assignment_count: 1, completed_at: "2026-09-11T00:00:00.000000000Z",
      nodes: [{ node_id: 7, expected_revision: 3, revision: 4, changed: true }],
    }), { status: 200 });
  });
  render(ActionRecoveryModal, { session: "fresh-session", sessionVaultID: vaultID,
    journal, initialAction: uncertain, tag, onprogress: vi.fn(), onclose: vi.fn(), onauthfailure: vi.fn() });

  expect(screen.getByText(/result is uncertain/)).toBeTruthy();
  await fireEvent.click(screen.getByRole("checkbox", { name: /I confirm this vault/ }));
  await fireEvent.click(screen.getByRole("button", { name: "Retry same operation" }));
  await screen.findByText(/Action complete/);
  expect(sent).toEqual(uncertain.batches[0].request);
});

it("refuses a tampered checkpoint before scheduling any batch", async () => {
  const action = await prepared();
  const journal = new MemoryJournal(action);
  journal.verifyCheckpoint = async () => { journal.checkpointReads++; throw new Error("The selected checkpoint does not match the prepared action and vault."); };
  render(ActionRecoveryModal, { session: "fresh-session", sessionVaultID: vaultID,
    journal, initialAction: action, tag, onprogress: vi.fn(), onclose: vi.fn(), onauthfailure: vi.fn() });

  const file = new File([new Uint8Array([9, 9, 9])], "tampered.json", { type: "application/json" });
  await fireEvent.change(screen.getByLabelText("Select the saved recovery checkpoint"), { target: { files: [file] } });
  await screen.findByText(/does not match the prepared action and vault/);
  expect(journal.checkpointReads).toBe(1);
  expect(journal.batchCalls).toBe(0);
});

it("shows a paused reload as retained work that needs explicit resume", async () => {
  const base = await prepared();
  const paused: PersistedAction = { ...base, state: "paused", checkpoint_verified: true };
  const journal = new MemoryJournal(paused);
  render(ActionRecoveryModal, { session: "fresh-session", sessionVaultID: vaultID,
    journal, initialAction: paused, tag, onprogress: vi.fn(), onclose: vi.fn(), onauthfailure: vi.fn() });

  expect(screen.getByText(/action is paused/i)).toBeTruthy();
  expect(screen.getByRole("button", { name: "Confirm and resume action" })).toBeTruthy();
  expect(journal.batchCalls).toBe(0);
});
