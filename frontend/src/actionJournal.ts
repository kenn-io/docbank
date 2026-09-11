import {
  batchTagRequestDigest,
  type BatchTagReceipt,
  type BatchTagRequest,
} from "./batch-tags.js";
import { snapshotMemberHash, type SnapshotMember, type VerifiedSnapshotTargets } from "./snapshots.js";

export const ACTION_MAX_MEMBERS = 250_000;
export const ACTION_MAX_BATCHES = 250;
export const ACTION_MAX_BATCH_MEMBERS = 1_000;
export const ACTION_MAX_BYTES = 128 * 1024 * 1024;

export interface ActionSource {
  readonly snapshot_id: string;
  readonly snapshot_fingerprint: string;
  readonly query_fingerprint: string;
  readonly member_hash: string;
}

export interface PreparedActionBatch {
  readonly index: number;
  readonly operation_id: string;
  readonly members: readonly SnapshotMember[];
  readonly request: Readonly<BatchTagRequest>;
  readonly request_digest: string;
  readonly receipt?: Readonly<BatchTagReceipt>;
}

export interface PreparedAction {
  readonly version: 1;
  readonly action_id: string;
  readonly vault_id: string;
  readonly source: Readonly<ActionSource>;
  readonly total: number;
  readonly total_bytes: number;
  readonly tag_id: string;
  readonly assign: boolean;
  readonly created_at: string;
  readonly plan_digest: string;
  readonly batches: readonly PreparedActionBatch[];
}

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
  markSending(index: number): Promise<void>;
  recordReceipt(index: number, receipt: BatchTagReceipt): Promise<void>;
  markUncertain(index: number): Promise<void>;
  markStale(index: number): Promise<void>;
  consumeResumeConfirmation(actionID: string): boolean;
}

const uuidV4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const sha256 = /^[0-9a-f]{64}$/;
const prefixedSHA256 = /^sha256:[0-9a-f]{64}$/;
const snapshotID = /^[0-9a-f]{32}$/;
const encoder = new TextEncoder();

function validUUID(value: string): boolean {
  return uuidV4.test(value);
}

function digestBytes(value: unknown): Uint8Array {
  return encoder.encode(JSON.stringify(value));
}

async function sha256Hex(bytes: Uint8Array): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", Uint8Array.from(bytes));
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("");
}

export function immutablePlanValue(action: Omit<PreparedAction, "plan_digest"> | PreparedAction): unknown {
  return {
    version: action.version,
    action_id: action.action_id,
    vault_id: action.vault_id,
    source: {
      snapshot_id: action.source.snapshot_id,
      snapshot_fingerprint: action.source.snapshot_fingerprint,
      query_fingerprint: action.source.query_fingerprint,
      member_hash: action.source.member_hash,
    },
    total: action.total,
    total_bytes: action.total_bytes,
    tag_id: action.tag_id,
    assign: action.assign,
    created_at: action.created_at,
    batches: action.batches.map((batch) => ({
      index: batch.index,
      operation_id: batch.operation_id,
      members: batch.members.map((member) => ({
        node_id: member.node_id,
        content_version_id: member.content_version_id,
        blob_hash: member.blob_hash,
        size: member.size,
        revision: member.revision,
      })),
      request: {
        operation_id: batch.request.operation_id,
        tag_id: batch.request.tag_id,
        assign: batch.request.assign,
        nodes: batch.request.nodes.map((node) => ({ node_id: node.node_id, revision: node.revision })),
      },
      request_digest: batch.request_digest,
    })),
  };
}

export async function actionPlanDigest(action: Omit<PreparedAction, "plan_digest"> | PreparedAction): Promise<string> {
  return sha256Hex(digestBytes(immutablePlanValue(action)));
}

function exactMember(member: SnapshotMember): SnapshotMember {
  if (!Number.isSafeInteger(member.node_id) || member.node_id < 1 ||
      !validUUID(member.content_version_id) || !sha256.test(member.blob_hash) ||
      !Number.isSafeInteger(member.size) || member.size < 0 ||
      !Number.isSafeInteger(member.revision) || member.revision < 1) {
    throw new Error("Snapshot action contains an invalid exact source member.");
  }
  return {
    node_id: member.node_id,
    content_version_id: member.content_version_id,
    blob_hash: member.blob_hash,
    size: member.size,
    revision: member.revision,
  };
}

export async function prepareAction(
  vaultID: string,
  targets: VerifiedSnapshotTargets,
  tagID: string,
  assign: boolean,
): Promise<PreparedAction> {
  if (!validUUID(vaultID) || !validUUID(tagID) || typeof assign !== "boolean") {
    throw new Error("The action has an invalid vault, tag identity, or operation.");
  }
  if (!Array.isArray(targets.members) || targets.members.length < 1 || targets.members.length > ACTION_MAX_MEMBERS) {
    throw new Error("A recoverable action requires between 1 and 250,000 members.");
  }
  const snapshot = targets.snapshot;
  if (!snapshotID.test(snapshot.snapshot_id) || !prefixedSHA256.test(snapshot.snapshot_fingerprint) ||
      !prefixedSHA256.test(snapshot.query_fingerprint) || !sha256.test(snapshot.member_hash) ||
      snapshot.total !== targets.members.length || !Number.isSafeInteger(snapshot.total_bytes) || snapshot.total_bytes < 0) {
    throw new Error("The action source identity is invalid.");
  }
  const members = targets.members.map(exactMember).sort((left, right) =>
    left.node_id - right.node_id || left.content_version_id.localeCompare(right.content_version_id));
  if (new Set(members.map((member) => member.node_id)).size !== members.length) {
    throw new Error("Snapshot action members repeat a node identity.");
  }
  const totalBytes = members.reduce((sum, member) => sum + member.size, 0);
  if (!Number.isSafeInteger(totalBytes) || totalBytes !== snapshot.total_bytes ||
      await snapshotMemberHash(members) !== snapshot.member_hash) {
    throw new Error("The action source members do not match the snapshot receipt.");
  }

  const batches: PreparedActionBatch[] = [];
  for (let offset = 0; offset < members.length; offset += ACTION_MAX_BATCH_MEMBERS) {
    const batchMembers = members.slice(offset, offset + ACTION_MAX_BATCH_MEMBERS);
    const operationID = crypto.randomUUID();
    const request: BatchTagRequest = {
      operation_id: operationID,
      tag_id: tagID,
      assign,
      nodes: batchMembers.map((member) => ({ node_id: member.node_id, revision: member.revision })),
    };
    batches.push({
      index: batches.length,
      operation_id: operationID,
      members: batchMembers,
      request,
      request_digest: await batchTagRequestDigest(request),
    });
  }
  if (batches.length > ACTION_MAX_BATCHES) throw new Error("A recoverable action is limited to 250 batches.");

  const withoutDigest: Omit<PreparedAction, "plan_digest"> = {
    version: 1,
    action_id: crypto.randomUUID(),
    vault_id: vaultID,
    source: {
      snapshot_id: snapshot.snapshot_id,
      snapshot_fingerprint: snapshot.snapshot_fingerprint,
      query_fingerprint: snapshot.query_fingerprint,
      member_hash: snapshot.member_hash,
    },
    total: members.length,
    total_bytes: totalBytes,
    tag_id: tagID,
    assign,
    created_at: new Date().toISOString(),
    batches,
  };
  const action: PreparedAction = { ...withoutDigest, plan_digest: await actionPlanDigest(withoutDigest) };
  if (digestBytes(action).length > ACTION_MAX_BYTES) throw new Error("The recoverable action exceeds 128 MiB.");
  return action;
}

type StoredHeader = {
  version: 1; action_id: string; vault_id: string; source: ActionSource; total: number; total_bytes: number;
  tag_id: string; assign: boolean; created_at: string; plan_digest: string; state: ActionState;
  checkpoint_verified: boolean; batch_count: number; serialized_bytes: number;
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
  const { serialized_bytes: _serializedBytes, batch_count: _batchCount, ...action } = header;
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
    if (!validUUID(vaultID)) throw new Error("The action journal requires a valid vault identity.");
    if (typeof indexedDB === "undefined") throw new Error("This browser does not provide durable action storage.");
    const request = indexedDB.open(databaseName, databaseVersion);
    request.onupgradeneeded = () => {
      const database = request.result;
      if (!database.objectStoreNames.contains(headerStore)) database.createObjectStore(headerStore, { keyPath: "vault_id" });
      if (!database.objectStoreNames.contains(batchStore)) database.createObjectStore(batchStore, { keyPath: ["vault_id", "index"] });
    };
    const journal = new ActionJournal(vaultID, await idbRequest(request));
    await journal.#recoverInterruptedSend();
    await journal.#validateStoredAction();
    return journal;
  }

  async prepare(action: PreparedAction): Promise<void> {
    const { decodeRecovery, encodeRecovery } = await import("./actionRecovery.js");
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
      serialized_bytes: bytes.length,
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
      actions.put(header);
    }
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
    const { decodeRecovery, encodeRecovery } = await import("./actionRecovery.js");
    await decodeRecovery(await encodeRecovery(action));
  }

  async verifyCheckpoint(bytes: Uint8Array): Promise<void> {
    const { decodeRecovery } = await import("./actionRecovery.js");
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
    const current = await this.load();
    const batch = current?.batches[index];
    if (!current || !batch) throw new Error("The durable action batch is missing.");
    const { encodeRecovery, decodeRecovery } = await import("./actionRecovery.js");
    const candidate: PreparedAction = { ...current, batches: current.batches.map((item) => item.index === index ? { ...item, receipt } : item) };
    const bytes = await encodeRecovery(candidate);
    const projected = (await decodeRecovery(bytes)).batches[index].receipt!;
    await this.#update(index, (header, stored, transaction) => {
      if (stored.operation_id !== batch.operation_id || stored.request_digest !== batch.request_digest) abort(transaction, "The durable action batch changed during receipt validation.");
      stored.receipt = projected;
      stored.state = "complete";
      header.serialized_bytes = bytes.length;
      const allComplete = current.batches.every((item) => item.index === index || item.state === "complete");
      header.state = allComplete ? "complete" : (header.state === "paused" ? "paused" : "sending");
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
    transaction.objectStore(batchStore).delete(IDBKeyRange.bound([this.vaultID, 0], [this.vaultID, ACTION_MAX_BATCHES]));
    await transactionDone(transaction);
    this.#confirmedActionID = undefined;
  }
}
