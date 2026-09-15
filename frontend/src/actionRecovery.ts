import { batchTagRequestDigest, validateBatchTagReceipt, type BatchTagReceipt, type BatchTagRequest } from "./batch-tags.js";
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

const encoder = new TextEncoder();
const decoder = new TextDecoder("utf-8", { fatal: true });
const uuidV4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const sha256 = /^[0-9a-f]{64}$/;
const prefixedSHA256 = /^sha256:[0-9a-f]{64}$/;
const snapshotID = /^[0-9a-f]{32}$/;

function digestBytes(value: unknown): Uint8Array {
  return encoder.encode(JSON.stringify(value));
}

async function sha256Hex(bytes: Uint8Array): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", Uint8Array.from(bytes));
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("");
}

function immutablePlanValue(action: Omit<PreparedAction, "plan_digest"> | PreparedAction): unknown {
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
      !uuidV4.test(member.content_version_id) || !sha256.test(member.blob_hash) ||
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
  if (!uuidV4.test(vaultID) || !uuidV4.test(tagID) || typeof assign !== "boolean") {
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

function malformed(reason: string): never {
  throw new Error(`Malformed action recovery file: ${reason}.`);
}

function object(value: unknown, field: string): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) malformed(`${field} must be an object`);
  return value as Record<string, unknown>;
}

function exactKeys(raw: Record<string, unknown>, required: readonly string[], optional: readonly string[], field: string): void {
  const allowed = new Set([...required, ...optional]);
  if (required.some((key) => !Object.hasOwn(raw, key)) || Object.keys(raw).some((key) => !allowed.has(key))) {
    malformed(`${field} has missing or unknown fields`);
  }
}

function integer(value: unknown, field: string, minimum = 0): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < minimum) malformed(`${field} is not a safe integer`);
  return value;
}

function string(value: unknown, field: string): string {
  if (typeof value !== "string") malformed(`${field} is not a string`);
  return value;
}

function uuid(value: unknown, field: string): string {
  const parsed = string(value, field);
  if (!uuidV4.test(parsed)) malformed(`${field} is not a UUID`);
  return parsed;
}

function hash(value: unknown, field: string, prefixed = false): string {
  const parsed = string(value, field);
  if (!(prefixed ? prefixedSHA256 : sha256).test(parsed)) malformed(`${field} is not a SHA-256 identity`);
  return parsed;
}

function member(value: unknown, field: string): SnapshotMember {
  const raw = object(value, field);
  exactKeys(raw, ["node_id", "content_version_id", "blob_hash", "size", "revision"], [], field);
  return {
    node_id: integer(raw.node_id, `${field}.node_id`, 1),
    content_version_id: uuid(raw.content_version_id, `${field}.content_version_id`),
    blob_hash: hash(raw.blob_hash, `${field}.blob_hash`),
    size: integer(raw.size, `${field}.size`),
    revision: integer(raw.revision, `${field}.revision`, 1),
  };
}

function request(value: unknown, field: string): BatchTagRequest {
  const raw = object(value, field);
  exactKeys(raw, ["operation_id", "tag_id", "assign", "nodes"], [], field);
  if (typeof raw.assign !== "boolean" || !Array.isArray(raw.nodes) || raw.nodes.length < 1 || raw.nodes.length > ACTION_MAX_BATCH_MEMBERS) {
    malformed(`${field} is invalid`);
  }
  return {
    operation_id: uuid(raw.operation_id, `${field}.operation_id`),
    tag_id: uuid(raw.tag_id, `${field}.tag_id`),
    assign: raw.assign,
    nodes: raw.nodes.map((value, index) => {
      const node = object(value, `${field}.nodes[${index}]`);
      exactKeys(node, ["node_id", "revision"], [], `${field}.nodes[${index}]`);
      return { node_id: integer(node.node_id, `${field}.nodes[${index}].node_id`, 1), revision: integer(node.revision, `${field}.nodes[${index}].revision`, 1) };
    }),
  };
}

function rawReceipt(value: unknown, field: string, strict: boolean): unknown {
  if (!strict) return value;
  const raw = object(value, field);
  exactKeys(raw, [
    "version", "operation_id", "request_digest", "tag_id", "assign", "tag_revision",
    "assignment_count", "completed_at", "nodes",
  ], [], field);
  if (!Array.isArray(raw.nodes)) malformed(`${field}.nodes is not an array`);
  raw.nodes.forEach((value, index) => {
    const node = object(value, `${field}.nodes[${index}]`);
    exactKeys(node, ["node_id", "expected_revision", "revision", "changed"], [], `${field}.nodes[${index}]`);
  });
  return raw;
}

export function receiptProjection(receipt: BatchTagReceipt): BatchTagReceipt {
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

async function normalize(value: unknown, strict: boolean): Promise<PreparedAction> {
  const raw = object(value, "action");
  if (strict) exactKeys(raw, [
    "version", "action_id", "vault_id", "source", "total", "total_bytes", "tag_id", "assign",
    "created_at", "plan_digest", "batches",
  ], [], "action");
  if (raw.version !== 1 || typeof raw.assign !== "boolean" || !Array.isArray(raw.batches)) malformed("action header is invalid");
  const source = object(raw.source, "action.source");
  if (strict) exactKeys(source, ["snapshot_id", "snapshot_fingerprint", "query_fingerprint", "member_hash"], [], "action.source");
  const createdAt = string(raw.created_at, "action.created_at");
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/.test(createdAt) || !Number.isFinite(Date.parse(createdAt))) {
    malformed("action.created_at is not canonical UTC time");
  }
  if (raw.batches.length < 1 || raw.batches.length > ACTION_MAX_BATCHES) malformed("batch count is outside its bound");
  const tagID = uuid(raw.tag_id, "action.tag_id");
  const batches: PreparedActionBatch[] = [];
  const allMembers: SnapshotMember[] = [];
  for (let index = 0; index < raw.batches.length; index++) {
    const batchRaw = object(raw.batches[index], `action.batches[${index}]`);
    if (strict) exactKeys(batchRaw, ["index", "operation_id", "members", "request", "request_digest"], ["receipt"], `action.batches[${index}]`);
    if (batchRaw.index !== index || !Array.isArray(batchRaw.members) || batchRaw.members.length < 1 || batchRaw.members.length > ACTION_MAX_BATCH_MEMBERS) {
      malformed(`action.batches[${index}] is outside its bound`);
    }
    const members = batchRaw.members.map((value, memberIndex) => member(value, `action.batches[${index}].members[${memberIndex}]`));
    const originalRequest = request(batchRaw.request, `action.batches[${index}].request`);
    const operationID = uuid(batchRaw.operation_id, `action.batches[${index}].operation_id`);
    const requestDigest = hash(batchRaw.request_digest, `action.batches[${index}].request_digest`);
    if (operationID !== originalRequest.operation_id || tagID !== originalRequest.tag_id || raw.assign !== originalRequest.assign ||
        JSON.stringify(originalRequest.nodes) !== JSON.stringify(members.map((item) => ({ node_id: item.node_id, revision: item.revision }))) ||
        requestDigest !== await batchTagRequestDigest(originalRequest)) {
      malformed(`action.batches[${index}] does not bind its original request`);
    }
    let receipt: BatchTagReceipt | undefined;
    if (batchRaw.receipt !== undefined) {
      receipt = receiptProjection(await validateBatchTagReceipt(originalRequest, rawReceipt(batchRaw.receipt, `action.batches[${index}].receipt`, strict)));
    }
    batches.push({ index, operation_id: operationID, members, request: originalRequest, request_digest: requestDigest, ...(receipt ? { receipt } : {}) });
    allMembers.push(...members);
  }
  const total = integer(raw.total, "action.total", 1);
  const totalBytes = integer(raw.total_bytes, "action.total_bytes");
  if (total > ACTION_MAX_MEMBERS || total !== allMembers.length || new Set(allMembers.map((item) => item.node_id)).size !== allMembers.length ||
      totalBytes !== allMembers.reduce((sum, item) => sum + item.size, 0) || !Number.isSafeInteger(totalBytes)) malformed("member totals are inconsistent");
  const normalized: PreparedAction = {
    version: 1,
    action_id: uuid(raw.action_id, "action.action_id"),
    vault_id: uuid(raw.vault_id, "action.vault_id"),
    source: {
      snapshot_id: string(source.snapshot_id, "action.source.snapshot_id"),
      snapshot_fingerprint: hash(source.snapshot_fingerprint, "action.source.snapshot_fingerprint", true),
      query_fingerprint: hash(source.query_fingerprint, "action.source.query_fingerprint", true),
      member_hash: hash(source.member_hash, "action.source.member_hash"),
    },
    total,
    total_bytes: totalBytes,
    tag_id: tagID,
    assign: raw.assign,
    created_at: createdAt,
    plan_digest: hash(raw.plan_digest, "action.plan_digest"),
    batches,
  };
  if (!snapshotID.test(normalized.source.snapshot_id) || await snapshotMemberHash(allMembers) !== normalized.source.member_hash ||
      await actionPlanDigest(normalized) !== normalized.plan_digest) malformed("immutable action identity does not match its digest");
  return normalized;
}

function recoveryValue(action: PreparedAction): unknown {
  const plan = immutablePlanValue(action) as Record<string, unknown>;
  const planBatches = plan.batches as Record<string, unknown>[];
  return {
    version: plan.version,
    action_id: plan.action_id,
    vault_id: plan.vault_id,
    source: plan.source,
    total: plan.total,
    total_bytes: plan.total_bytes,
    tag_id: plan.tag_id,
    assign: plan.assign,
    created_at: plan.created_at,
    plan_digest: action.plan_digest,
    batches: planBatches.map((batch, index) => ({ ...batch, ...(action.batches[index].receipt ? { receipt: receiptProjection(action.batches[index].receipt!) } : {}) })),
  };
}

export async function encodeRecovery(action: PreparedAction): Promise<Uint8Array> {
  const normalized = await normalize(action, false);
  const bytes = encoder.encode(JSON.stringify(recoveryValue(normalized)));
  if (bytes.length > ACTION_MAX_BYTES) throw new Error("The action recovery file exceeds 128 MiB.");
  return bytes;
}

export async function decodeRecovery(bytes: Uint8Array): Promise<PreparedAction> {
  if (!(bytes instanceof Uint8Array) || bytes.length > ACTION_MAX_BYTES) throw new Error("The action recovery file exceeds 128 MiB.");
  let raw: unknown;
  try {
    raw = JSON.parse(decoder.decode(bytes));
  } catch {
    malformed("content is not UTF-8 JSON");
  }
  const action = await normalize(raw, true);
  const canonical = await encodeRecovery(action);
  if (canonical.length !== bytes.length || canonical.some((byte, index) => byte !== bytes[index])) {
    malformed("bytes are not the canonical encoding");
  }
  return action;
}
