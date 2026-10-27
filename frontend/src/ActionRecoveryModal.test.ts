import { prepareAction } from "./actionRecovery.js";
import { webcrypto } from "node:crypto";
import { IDBFactory, IDBKeyRange } from "fake-indexeddb";
import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import ActionRecoveryModal, { type ActionRecoveryJournal } from "./ActionRecoveryModal.svelte";
import { APIError } from "./api-transport.js";
import { ActionJournal, type PersistedAction } from "./actionJournal.js";
import { encodeRecovery } from "./actionRecovery.js";
import type { BatchTagReceipt } from "./batch-tags.js";
import { snapshotMemberHash, type SnapshotMember, type SnapshotPage } from "./snapshots.js";

const vaultID = "11111111-1111-4111-8111-111111111111";
const tag = {
  id: "22222222-2222-4222-8222-222222222222",
  name: "Review",
  revision: 2,
  assignment_count: 7,
};
const member: SnapshotMember = {
  node_id: 7,
  content_version_id: "33333333-3333-4333-8333-333333333333",
  blob_hash: "a".repeat(64),
  size: 42,
  revision: 3,
};

async function prepared(members = [member]): Promise<PersistedAction> {
  const memberHash = await snapshotMemberHash(members);
  const action = await prepareAction(
    vaultID,
    {
      snapshot: {
        snapshot_id: "b".repeat(32),
        snapshot_fingerprint: `sha256:${"c".repeat(64)}`,
        query_fingerprint: `sha256:${"d".repeat(64)}`,
        member_hash: memberHash,
        total: members.length,
        total_bytes: members.reduce((sum, item) => sum + item.size, 0),
      } as SnapshotPage,
      members,
    },
    tag.id,
    true,
  );
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

  constructor(action: PersistedAction) {
    this.action = action;
  }
  async load() {
    return this.action;
  }
  async loadBatch(index: number) {
    const action = this.action!;
    return {
      action_id: action.action_id,
      state: action.state,
      checkpoint_verified: action.checkpoint_verified,
      batch: action.batches[index],
    };
  }
  async verifyCheckpoint(_bytes: Uint8Array) {
    this.checkpointReads++;
    this.action = { ...this.action!, checkpoint_verified: true };
  }
  async confirmResume(actionID: string) {
    this.confirmation = actionID;
  }
  consumeResumeConfirmation(actionID: string) {
    const confirmed = this.confirmation === actionID;
    this.confirmation = "";
    return confirmed;
  }
  async markSending(index: number) {
    this.batchCalls++;
    const action = this.action!;
    this.action = {
      ...action,
      state: "sending",
      batches: action.batches.map((batch) =>
        batch.index === index ? { ...batch, state: "sending" } : batch,
      ),
    };
  }
  async recordReceipt(index: number, receipt: BatchTagReceipt) {
    const action = this.action!;
    this.action = {
      ...action,
      state: "complete",
      batches: action.batches.map((batch) =>
        batch.index === index ? { ...batch, state: "complete", receipt } : batch,
      ),
    };
  }
  async markUncertain(index: number) {
    const action = this.action!;
    this.action = {
      ...action,
      state: "uncertain",
      batches: action.batches.map((batch) =>
        batch.index === index ? { ...batch, state: "uncertain" } : batch,
      ),
    };
  }
  async markStale(index: number) {
    const action = this.action!;
    this.action = {
      ...action,
      state: "stale",
      batches: action.batches.map((batch) =>
        batch.index === index ? { ...batch, state: "stale" } : batch,
      ),
    };
  }
  async pause() {
    this.action = { ...this.action!, state: "paused" };
  }
  async abandon() {
    this.action = null;
  }
  async discardUnsent(actionID: string) {
    if (
      this.action?.action_id === actionID &&
      this.action.state === "prepared" &&
      this.action.batches.every((batch) => batch.state === "prepared")
    )
      this.action = null;
  }
}

beforeEach(() => {
  vi.stubGlobal("crypto", webcrypto);
  vi.stubGlobal("indexedDB", new IDBFactory());
  vi.stubGlobal("IDBKeyRange", IDBKeyRange);
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  vi.stubGlobal("URL", {
    createObjectURL: vi.fn(() => "blob:checkpoint"),
    revokeObjectURL: vi.fn(),
  });
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it.each([
  { cause: new APIError("Access denied", 401, "unauthorized"), expired: true },
  { cause: new Error("Confirm with the fresh session"), expired: false },
])("classifies authentication errors by HTTP status: $expired", async ({ cause, expired }) => {
  const action = { ...(await prepared()), checkpoint_verified: true };
  const journal = new MemoryJournal(action);
  journal.confirmResume = async () => {
    throw cause;
  };
  const onauthfailure = vi.fn();
  render(ActionRecoveryModal, {
    session: "session",
    sessionVaultID: vaultID,
    journal,
    initialAction: action,
    tag,
    onprogress: vi.fn(),
    onclose: vi.fn(),
    onauthfailure,
  });
  await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  await screen.findByText(cause.message);
  expect(onauthfailure).toHaveBeenCalledTimes(expired ? 1 : 0);
});

it("applies a fresh action without downloading or reading a recovery file", async () => {
  const action = await prepared();
  const journal = new MemoryJournal(action);
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    if (String(input) === "/api/v1/audit/status") {
      return new Response(JSON.stringify({ vault_id: vaultID }), { status: 200 });
    }
    const request = JSON.parse(String(init?.body));
    return new Response(
      JSON.stringify({
        version: 1,
        operation_id: request.operation_id,
        request_digest: action.batches[0].request_digest,
        tag_id: tag.id,
        assign: true,
        tag_revision: 2,
        assignment_count: 1,
        completed_at: "2026-09-11T00:00:00.000000000Z",
        nodes: [{ node_id: 7, expected_revision: 3, revision: 4, changed: true }],
      }),
      { status: 200 },
    );
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

  expect(screen.getByText("Adds “Review” to 1 document.")).toBeTruthy();
  expect(screen.queryByText(/recovery/i)).toBeNull();
  expect(screen.queryByRole("button", { name: "Save recovery file" })).toBeNull();
  expect((screen.getByRole("button", { name: "Apply" }) as HTMLButtonElement).disabled).toBe(false);
  expect(journal.batchCalls).toBe(0);

  await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  await screen.findByText("Added “Review” to 1 document.");
  expect(journal.batchCalls).toBe(1);
  expect(journal.checkpointReads).toBe(0);
  expect(URL.createObjectURL).not.toHaveBeenCalled();
  expect(journal.action?.checkpoint_verified).toBe(false);
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

  expect(screen.getByText("This run belongs to a different vault.")).toBeTruthy();
  expect(screen.getByText(/so tagging stopped/)).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Apply" })).toBeNull();
  expect(screen.getByRole("button", { name: "Discard run…" })).toBeTruthy();
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
    return new Response(
      JSON.stringify({
        version: 1,
        operation_id: uncertain.batches[0].operation_id,
        request_digest: uncertain.batches[0].request_digest,
        tag_id: tag.id,
        assign: true,
        tag_revision: 2,
        assignment_count: 1,
        completed_at: "2026-09-11T00:00:00.000000000Z",
        nodes: [{ node_id: 7, expected_revision: 3, revision: 4, changed: true }],
      }),
      { status: 200 },
    );
  });
  render(ActionRecoveryModal, {
    session: "fresh-session",
    sessionVaultID: vaultID,
    journal,
    initialAction: uncertain,
    tag,
    onprogress: vi.fn(),
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });

  expect(screen.getByText(/result is unknown/)).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await screen.findByText("Added “Review” to 1 document.");
  expect(sent).toEqual(uncertain.batches[0].request);
});

it("shows a paused reload as retained work that needs explicit resume", async () => {
  const base = await prepared();
  const paused: PersistedAction = { ...base, state: "paused", checkpoint_verified: true };
  const journal = new MemoryJournal(paused);
  render(ActionRecoveryModal, {
    session: "fresh-session",
    sessionVaultID: vaultID,
    journal,
    initialAction: paused,
    tag,
    onprogress: vi.fn(),
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });

  expect(screen.getByText("Paused · 0 of 1 document done")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Resume" })).toBeTruthy();
  expect(journal.batchCalls).toBe(0);
  await fireEvent.click(screen.getByRole("button", { name: "Save recovery file" }));
  await waitFor(() => expect(URL.createObjectURL).toHaveBeenCalledOnce());
});

it("keeps a deleted-tag action available for checkpoint export and abandonment", async () => {
  const action: PersistedAction = {
    ...(await prepared()),
    state: "paused",
    checkpoint_verified: true,
  };
  const journal = new MemoryJournal(action);
  const onclose = vi.fn();
  render(ActionRecoveryModal, {
    session: "session",
    sessionVaultID: vaultID,
    journal,
    initialAction: action,
    tag: null,
    onprogress: vi.fn(),
    onclose,
    onauthfailure: vi.fn(),
  });
  expect(screen.getByText("This tag no longer exists.")).toBeTruthy();
  expect((screen.getByRole("button", { name: "Resume" }) as HTMLButtonElement).disabled).toBe(true);
  expect(
    (screen.getByRole("button", { name: "Save recovery file" }) as HTMLButtonElement).disabled,
  ).toBe(false);
  await fireEvent.click(screen.getByRole("button", { name: "Discard run…" }));
  await fireEvent.click(screen.getByRole("button", { name: "Discard run" }));
  await waitFor(() => expect(onclose).toHaveBeenCalledOnce());
  expect(journal.action).toBeNull();
});

it("drops a never-sent action on Cancel and keeps a started one for later", async () => {
  vi.stubGlobal("Uint8Array", new TextEncoder().encode("").constructor);
  const untouched = await prepared();
  const fresh = await ActionJournal.open(vaultID);
  await fresh.prepare(untouched);
  const onclose = vi.fn();
  render(ActionRecoveryModal, {
    session: "session",
    sessionVaultID: vaultID,
    journal: fresh,
    initialAction: untouched,
    tag,
    onprogress: vi.fn(),
    onclose,
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  await waitFor(() => expect(onclose).toHaveBeenCalledOnce());
  expect(await fresh.load()).toBeNull();
  cleanup();

  const paused: PersistedAction = { ...untouched, state: "paused", checkpoint_verified: true };
  const started = new MemoryJournal(paused);
  const closeStarted = vi.fn();
  render(ActionRecoveryModal, {
    session: "session",
    sessionVaultID: vaultID,
    journal: started,
    initialAction: paused,
    tag,
    onprogress: vi.fn(),
    onclose: closeStarted,
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("button", { name: "Finish later" }));
  expect(closeStarted).toHaveBeenCalledOnce();
  expect(started.action).not.toBeNull();
});

it.each(["sending", "uncertain", "resumed", "replaced"] as const)(
  "Cancel keeps the journal when another tab has %s work",
  async (progress) => {
    // Keep the encoder and byte checks in the same realm under jsdom.
    vi.stubGlobal("Uint8Array", new TextEncoder().encode("").constructor);
    const action = await prepared(
      progress === "resumed"
        ? Array.from({ length: 1_001 }, (_, index) => ({ ...member, node_id: index + 1 }))
        : [member],
    );
    const journal = await ActionJournal.open(vaultID);
    await journal.prepare(action);
    const otherTab = await ActionJournal.open(vaultID);
    const onclose = vi.fn();
    render(ActionRecoveryModal, {
      session: "session",
      sessionVaultID: vaultID,
      journal,
      initialAction: action,
      tag,
      onprogress: vi.fn(),
      onclose,
      onauthfailure: vi.fn(),
    });
    if (progress === "replaced") {
      await otherTab.abandon();
      await otherTab.prepare(await prepared());
    } else {
      await otherTab.verifyCheckpoint(await encodeRecovery(action));
      const batchIndex = progress === "resumed" ? 1 : 0;
      await otherTab.markSending(batchIndex);
      if (progress !== "sending") await otherTab.markUncertain(batchIndex);
      if (progress === "resumed") {
        await otherTab.pause();
        await otherTab.confirmResume(action.action_id);
      }
    }
    const retained = await otherTab.load();

    await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    await waitFor(() => expect(onclose).toHaveBeenCalledOnce());
    expect(await otherTab.load()).toEqual(retained);
  },
);

it("shows exact file-supplied identities before target confirmation", async () => {
  const action = { ...(await prepared()), checkpoint_verified: true };
  render(ActionRecoveryModal, {
    session: "session",
    sessionVaultID: vaultID,
    journal: new MemoryJournal(action),
    initialAction: action,
    tag,
    onprogress: vi.fn(),
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(screen.getByText("Review documents"));
  expect(screen.getByRole("cell", { name: "7" })).toBeTruthy();
  expect(screen.getByText(member.content_version_id)).toBeTruthy();
  expect(screen.getByText(member.blob_hash)).toBeTruthy();
  expect(screen.getByText(/can come from anyone/)).toBeTruthy();
});

it("retains an in-flight receipt without publishing to a replaced recovery view", async () => {
  const action = { ...(await prepared()), checkpoint_verified: true };
  const journal = new MemoryJournal(action);
  const onprogress = vi.fn();
  let finish!: (response: Response) => void;
  const response = new Promise<Response>((resolve) => {
    finish = resolve;
  });
  let sent = false;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    if (String(input) === "/api/v1/audit/status")
      return new Response(JSON.stringify({ vault_id: vaultID }));
    sent = true;
    return response;
  });
  const view = render(ActionRecoveryModal, {
    session: "session",
    sessionVaultID: vaultID,
    journal,
    initialAction: action,
    tag,
    onprogress,
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  await waitFor(() => expect(sent).toBe(true));
  await view.unmount();
  onprogress.mockClear();
  finish(
    new Response(
      JSON.stringify({
        version: 1,
        operation_id: action.batches[0].operation_id,
        request_digest: action.batches[0].request_digest,
        tag_id: tag.id,
        assign: true,
        tag_revision: 2,
        assignment_count: 1,
        completed_at: "2026-09-11T00:00:00.000000000Z",
        nodes: [{ node_id: 7, expected_revision: 3, revision: 4, changed: true }],
      }),
    ),
  );
  await waitFor(() => expect(journal.action?.batches[0].receipt).toBeDefined());
  expect(onprogress).not.toHaveBeenCalled();
});

it("does not expire the session from a replaced view's pending error reload", async () => {
  const action = { ...(await prepared()), checkpoint_verified: true };
  const journal = new MemoryJournal(action);
  let release!: (action: PersistedAction) => void;
  const reloading = new Promise<PersistedAction>((resolve) => {
    release = resolve;
  });
  const load = vi.spyOn(journal, "load").mockResolvedValueOnce(action).mockReturnValue(reloading);
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) =>
    String(input) === "/api/v1/audit/status"
      ? new Response(JSON.stringify({ vault_id: vaultID }))
      : new Response(JSON.stringify({ code: "unauthorized", detail: "Session expired" }), {
          status: 401,
        }),
  );
  const onauthfailure = vi.fn();
  const view = render(ActionRecoveryModal, {
    session: "session",
    sessionVaultID: vaultID,
    journal,
    initialAction: action,
    tag,
    onprogress: vi.fn(),
    onclose: vi.fn(),
    onauthfailure,
  });
  await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  await waitFor(() => expect(load).toHaveBeenCalledTimes(2));
  await view.unmount();
  release(journal.action!);
  await reloading;
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(onauthfailure).not.toHaveBeenCalled();
});
