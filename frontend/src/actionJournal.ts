import { validateBatchTagReceipt, type BatchTagReceipt, type BatchTagRequest } from "./batch-tags.js";
import type { SnapshotMember } from "./snapshots.js";
import { ACTION_MAX_BATCHES, decodeRecovery, encodeRecovery, receiptProjection,
  type ActionSource, type PreparedAction, type PreparedActionBatch } from "./actionRecovery.js";

export type ActionState = "prepared" | "sending" | "paused" | "uncertain" | "stale" | "complete";
export type ActionBatchState = "prepared" | "sending" | "uncertain" | "stale" | "complete";

export interface PersistedActionBatch extends PreparedActionBatch {
  readonly state: ActionBatchState;
}

export interface PersistedAction extends Omit<PreparedAction, "batches"> {
  readonly state: ActionState;
  readonly checkpoint_verified: boolean;
  readonly batches: readonly PersistedActionBatch[];
}

export interface ActionJournalAccess {
  load(): Promise<PersistedAction | null>;
  loadBatch(index: number): Promise<Pick<PersistedAction, "action_id" | "state" | "checkpoint_verified"> & { batch: PersistedActionBatch }>;
  markSending(index: number): Promise<void>;
  recordReceipt(index: number, receipt: BatchTagReceipt): Promise<void>;
  markUncertain(index: number): Promise<void>;
  markStale(index: number): Promise<void>;
  consumeResumeConfirmation(actionID: string): boolean;
}

const uuidV4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

type StoredHeader = {
  version: 1; action_id: string; vault_id: string; source: ActionSource; total: number; total_bytes: number;
  tag_id: string; assign: boolean; created_at: string; plan_digest: string; state: ActionState;
  checkpoint_verified: boolean; batch_count: number; completed_batches: number;
};
type StoredBatch = {
  vault_id: string; index: number; operation_id: string; members: SnapshotMember[]; request: BatchTagRequest;
  request_digest: string; receipt?: BatchTagReceipt; state: ActionBatchState;
};

const databaseName = "docbank-action-journal-v1";
const databaseVersion = 1;
const headerStore = "actions";
const batchStore = "batches";

function idbRequest<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error ?? new Error("IndexedDB request failed."));
  });
}

function transactionDone(transaction: IDBTransaction): Promise<void> {
  return new Promise((resolve, reject) => {
    transaction.oncomplete = () => resolve();
    transaction.onabort = () => reject(transaction.error ?? new Error("IndexedDB transaction aborted."));
    transaction.onerror = () => reject(transaction.error ?? new Error("IndexedDB transaction failed."));
  });
}

function abort(transaction: IDBTransaction, message: string): never {
  transaction.abort();
  throw new Error(message);
}

function persistedFrom(header: StoredHeader, batches: StoredBatch[]): PersistedAction {
  const { completed_batches: _completedBatches, batch_count: _batchCount, ...action } = header;
  return { ...action, batches: batches.sort((left, right) => left.index - right.index).map(({ vault_id: _vaultID, ...batch }) => batch) };
}

export class ActionJournal implements ActionJournalAccess {
  readonly vaultID: string;
  readonly #database: IDBDatabase;
  #confirmedActionID?: string;

  private constructor(vaultID: string, database: IDBDatabase) {
    this.vaultID = vaultID;
    this.#database = database;
  }

  static async open(vaultID: string): Promise<ActionJournal> {
    const journal = await this.#connect(vaultID);
    try {
      await journal.#validateStoredAction();
      await journal.#recoverInterruptedSend();
      return journal;
    } catch (cause) {
      journal.#database.close();
      throw cause;
    }
  }

  static async abandon(vaultID: string): Promise<void> {
    const journal = await this.#connect(vaultID);
    try { await journal.abandon(); } finally { journal.#database.close(); }
  }

  static async #connect(vaultID: string): Promise<ActionJournal> {
    if (!uuidV4.test(vaultID)) throw new Error("The action journal requires a valid vault identity.");
    if (typeof indexedDB === "undefined") throw new Error("This browser does not provide durable action storage.");
    const request = indexedDB.open(databaseName, databaseVersion);
    request.onupgradeneeded = () => {
      const database = request.result;
      if (!database.objectStoreNames.contains(headerStore)) database.createObjectStore(headerStore, { keyPath: "vault_id" });
      if (!database.objectStoreNames.contains(batchStore)) database.createObjectStore(batchStore, { keyPath: ["vault_id", "index"] });
    };
    return new ActionJournal(vaultID, await idbRequest(request));
  }

  async prepare(action: PreparedAction): Promise<void> {
    const bytes = await encodeRecovery(action);
    const normalized = await decodeRecovery(bytes);
    if (normalized.vault_id !== this.vaultID) throw new Error("The recovery action belongs to a different vault.");
    const transaction = this.#database.transaction([headerStore, batchStore], "readwrite", { durability: "strict" });
    const actions = transaction.objectStore(headerStore);
    const existing = await idbRequest(actions.get(this.vaultID) as IDBRequest<StoredHeader | undefined>);
    if (existing) {
      if (existing.action_id === normalized.action_id && existing.plan_digest === normalized.plan_digest) {
        await transactionDone(transaction);
        return;
      }
      abort(transaction, "This vault already has an action retained in the journal. Abandon it explicitly before preparing another.");
    }
    const { batches, ...preparedHeader } = normalized;
    actions.add({
      ...preparedHeader,
      state: batches.every((batch) => batch.receipt !== undefined) ? "complete" : "prepared",
      checkpoint_verified: false,
      batch_count: batches.length,
      completed_batches: batches.filter((batch) => batch.receipt !== undefined).length,
    } satisfies StoredHeader);
    const batchRecords = transaction.objectStore(batchStore);
    for (const batch of batches) batchRecords.add({
      ...batch,
      members: batch.members.map((member) => ({ ...member })),
      request: { ...batch.request, nodes: batch.request.nodes.map((node) => ({ ...node })) },
      ...(batch.receipt ? { receipt: { ...batch.receipt, nodes: batch.receipt.nodes.map((node) => ({ ...node })) } } : {}),
      vault_id: this.vaultID,
      state: batch.receipt ? "complete" : "prepared",
    } satisfies StoredBatch);
    await transactionDone(transaction);
  }

  async load(): Promise<PersistedAction | null> {
    const transaction = this.#database.transaction([headerStore, batchStore], "readonly");
    const actions = transaction.objectStore(headerStore);
    const header = await idbRequest(actions.get(this.vaultID) as IDBRequest<StoredHeader | undefined>);
    if (!header) {
      await transactionDone(transaction);
      return null;
    }
    const range = IDBKeyRange.bound([this.vaultID, 0], [this.vaultID, ACTION_MAX_BATCHES]);
    const batches = await idbRequest(transaction.objectStore(batchStore).getAll(range) as IDBRequest<StoredBatch[]>);
    if (batches.length !== header.batch_count) abort(transaction, "The durable action journal is inconsistent.");
    await transactionDone(transaction);
    return persistedFrom(header, batches);
  }

  async loadBatch(index: number): ReturnType<ActionJournalAccess["loadBatch"]> {
    const transaction = this.#database.transaction([headerStore, batchStore], "readonly");
    const header = await idbRequest(transaction.objectStore(headerStore).get(this.vaultID) as IDBRequest<StoredHeader | undefined>);
    const stored = await idbRequest(transaction.objectStore(batchStore).get([this.vaultID, index]) as IDBRequest<StoredBatch | undefined>);
    if (!header || !stored) abort(transaction, "The durable action batch is missing.");
    await transactionDone(transaction);
    const { vault_id: _vaultID, ...batch } = stored;
    return { action_id: header.action_id, state: header.state, checkpoint_verified: header.checkpoint_verified, batch };
  }

  async #recoverInterruptedSend(): Promise<void> {
    const transaction = this.#database.transaction([headerStore, batchStore], "readwrite", { durability: "strict" });
    const actions = transaction.objectStore(headerStore);
    const header = await idbRequest(actions.get(this.vaultID) as IDBRequest<StoredHeader | undefined>);
    if (!header) {
      await transactionDone(transaction);
      return;
    }
    const range = IDBKeyRange.bound([this.vaultID, 0], [this.vaultID, ACTION_MAX_BATCHES]);
    const batches = await idbRequest(transaction.objectStore(batchStore).getAll(range) as IDBRequest<StoredBatch[]>);
    let recovered = false;
    for (const batch of batches) {
      if (batch.state === "sending") {
        batch.state = "uncertain";
        transaction.objectStore(batchStore).put(batch);
        recovered = true;
      }
    }
    if (recovered && header.state === "sending") {
      header.state = "uncertain";
    }
    header.completed_batches = batches.filter((batch) => batch.state === "complete").length;
    actions.put(header);
    await transactionDone(transaction);
  }

  async #validateStoredAction(): Promise<void> {
    const action = await this.load();
    if (!action) return;
    if (typeof action.checkpoint_verified !== "boolean" ||
        !new Set<ActionState>(["prepared", "sending", "paused", "uncertain", "stale", "complete"]).has(action.state) ||
        action.batches.some((batch) => !new Set<ActionBatchState>(["prepared", "sending", "uncertain", "stale", "complete"]).has(batch.state) ||
          (batch.state === "complete") !== (batch.receipt !== undefined))) {
      throw new Error("The durable action journal contains invalid progress state.");
    }
    await decodeRecovery(await encodeRecovery(action));
  }

  async verifyCheckpoint(bytes: Uint8Array): Promise<void> {
    const checkpoint = await decodeRecovery(bytes);
    const transaction = this.#database.transaction(headerStore, "readwrite", { durability: "strict" });
    const store = transaction.objectStore(headerStore);
    const header = await idbRequest(store.get(this.vaultID) as IDBRequest<StoredHeader | undefined>);
    if (!header || checkpoint.vault_id !== this.vaultID || checkpoint.action_id !== header.action_id || checkpoint.plan_digest !== header.plan_digest) {
      abort(transaction, "The selected checkpoint does not match the prepared action and vault.");
    }
    header.checkpoint_verified = true;
    store.put(header);
    await transactionDone(transaction);
  }

  async confirmResume(actionID: string): Promise<void> {
    const action = await this.load();
    if (!action || action.action_id !== actionID) throw new Error("The confirmed action is no longer available.");
    if (action.state === "paused") {
      const transaction = this.#database.transaction(headerStore, "readwrite", { durability: "strict" });
      const store = transaction.objectStore(headerStore);
      const header = await idbRequest(store.get(this.vaultID) as IDBRequest<StoredHeader | undefined>);
      if (!header || header.action_id !== actionID) abort(transaction, "The confirmed action is no longer available.");
      if (header.state === "paused") {
        header.state = "prepared";
        store.put(header);
      }
      await transactionDone(transaction);
    }
    this.#confirmedActionID = actionID;
  }

  consumeResumeConfirmation(actionID: string): boolean {
    const confirmed = this.#confirmedActionID === actionID;
    this.#confirmedActionID = undefined;
    return confirmed;
  }

  async markSending(index: number): Promise<void> {
    await this.#update(index, (header, batch, transaction) => {
      if (!header.checkpoint_verified) abort(transaction, "Select and verify the saved recovery file before mutation.");
      if (header.state === "paused" || header.state === "stale" || header.state === "complete") abort(transaction, "The action is not schedulable.");
      if (batch.state !== "prepared" && batch.state !== "uncertain") abort(transaction, "The batch is not schedulable.");
      batch.state = "sending";
      header.state = "sending";
    });
  }

  async #update(index: number, update: (header: StoredHeader, batch: StoredBatch, transaction: IDBTransaction) => void): Promise<void> {
    if (!Number.isSafeInteger(index) || index < 0 || index >= ACTION_MAX_BATCHES) throw new Error("Action batch index is invalid.");
    const transaction = this.#database.transaction([headerStore, batchStore], "readwrite", { durability: "strict" });
    const actions = transaction.objectStore(headerStore);
    const batches = transaction.objectStore(batchStore);
    const header = await idbRequest(actions.get(this.vaultID) as IDBRequest<StoredHeader | undefined>);
    const batch = await idbRequest(batches.get([this.vaultID, index]) as IDBRequest<StoredBatch | undefined>);
    if (!header || !batch) abort(transaction, "The durable action batch is missing.");
    update(header, batch, transaction);
    actions.put(header);
    batches.put(batch);
    await transactionDone(transaction);
  }

  async recordReceipt(index: number, receipt: BatchTagReceipt): Promise<void> {
    const { batch } = await this.loadBatch(index);
    const projected = receiptProjection(await validateBatchTagReceipt(batch.request, receipt));
    await this.#update(index, (header, stored, transaction) => {
      if (stored.operation_id !== batch.operation_id || stored.request_digest !== batch.request_digest) abort(transaction, "The durable action batch changed during receipt validation.");
      if (stored.state !== "complete") header.completed_batches++;
      stored.receipt = projected;
      stored.state = "complete";
      header.state = header.completed_batches === header.batch_count ? "complete" : (header.state === "paused" ? "paused" : "sending");
    });
  }

  async markUncertain(index: number): Promise<void> {
    await this.#update(index, (header, batch) => {
      if (batch.state !== "complete") batch.state = "uncertain";
      if (header.state !== "paused" && header.state !== "complete") header.state = "uncertain";
    });
  }

  async markStale(index: number): Promise<void> {
    await this.#update(index, (header, batch) => {
      if (batch.state !== "complete") batch.state = "stale";
      header.state = "stale";
    });
  }

  async pause(): Promise<void> {
    const transaction = this.#database.transaction(headerStore, "readwrite", { durability: "strict" });
    const store = transaction.objectStore(headerStore);
    const header = await idbRequest(store.get(this.vaultID) as IDBRequest<StoredHeader | undefined>);
    if (!header) abort(transaction, "No durable action is available to pause.");
    if (header.state !== "complete" && header.state !== "stale") {
      header.state = "paused";
      store.put(header);
    }
    await transactionDone(transaction);
  }

  async abandon(): Promise<void> {
    const transaction = this.#database.transaction([headerStore, batchStore], "readwrite", { durability: "strict" });
    transaction.objectStore(headerStore).delete(this.vaultID);
    transaction.objectStore(batchStore).delete(IDBKeyRange.bound([this.vaultID], [this.vaultID, []]));
    await transactionDone(transaction);
    this.#confirmedActionID = undefined;
  }
}
