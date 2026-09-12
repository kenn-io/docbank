import { APIError, requestResponse } from "./api.js";
import type { RelatedSelectedSource, SelectedSource } from "./selectedSource.js";

export interface AttachmentIdentity { node_id: number; version_id: string; sha256: string; size: number }
export interface AttachmentRelation {
  operation_id: string; order: number; parent: AttachmentIdentity; generation_id: string;
  attachment_id: string; part_path: string; sibling_order: number; filename: string;
  outcome: "decoded" | "unsupported" | "failed" | "encrypted" | "unavailable";
  child: AttachmentIdentity | null;
}
export interface RelationStatus { relation: AttachmentRelation; state: string; reason: string }
export interface RelationCursor { operationID: string; order: number }
export interface RelationPage { items: RelationStatus[]; total: number; next?: RelationCursor }
export interface PublicationReceipt {
  operation_id: string; request_digest: string; created_at: string;
  inventory_state: "complete" | "partial"; relations: AttachmentRelation[];
}
export type RelationDirection = "incoming" | "outgoing";
export const relationPageSize = 50;
export const relationDisplayLimit = 1000;
const jsonByteLimit = 2 * 1024 * 1024;
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const sha = /^[0-9a-f]{64}$/;
const operation = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$/;
const part = /^[1-9][0-9]{0,8}(?:\.[1-9][0-9]{0,8}){0,15}$/;

function invalid(reason: string): never { throw new Error(`Invalid attachment response: ${reason}.`); }
function object(raw: unknown): Record<string, unknown> {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) invalid("expected an object");
  return raw as Record<string, unknown>;
}
function keys(raw: Record<string, unknown>, fields: string[]): void {
  if (fields.some((key) => !Object.hasOwn(raw, key)) || Object.keys(raw).some((key) => !fields.includes(key))) invalid("missing or unknown fields");
}
function integer(raw: unknown, minimum: number, maximum = Number.MAX_SAFE_INTEGER): number {
  if (typeof raw !== "number" || !Number.isSafeInteger(raw) || raw < minimum || raw > maximum) invalid("integer exceeds bounds");
  return raw;
}
function text(raw: unknown, maximum = 4096): string {
  if (typeof raw !== "string" || raw.length > maximum || /[\uD800-\uDFFF]/u.test(raw)) invalid("invalid text");
  return raw;
}
function matching(raw: unknown, pattern: RegExp): string {
  const value = text(raw);
  if (!pattern.test(value)) invalid("invalid identity");
  return value;
}
function identity(raw: unknown): AttachmentIdentity {
  const value = object(raw);
  keys(value, ["node_id", "version_id", "sha256", "size"]);
  return { node_id: integer(value.node_id, 1), version_id: matching(value.version_id, uuid),
    sha256: matching(value.sha256, sha), size: integer(value.size, 0, 128 * 1024 * 1024) };
}
export function attachmentIdentity(source: SelectedSource): AttachmentIdentity {
  return { node_id: source.nodeID, version_id: source.versionID, sha256: source.blobHash, size: source.size };
}
function sameIdentity(a: AttachmentIdentity, b: AttachmentIdentity): boolean {
  return a.node_id === b.node_id && a.version_id === b.version_id && a.sha256 === b.sha256 && a.size === b.size;
}
function relation(raw: unknown): AttachmentRelation {
  const value = object(raw);
  keys(value, ["operation_id", "order", "parent", "generation_id", "attachment_id", "part_path", "sibling_order", "filename", "outcome", "child"]);
  const outcome = text(value.outcome) as AttachmentRelation["outcome"];
  if (!["decoded", "unsupported", "failed", "encrypted", "unavailable"].includes(outcome)) invalid("unknown MIME outcome");
  if ((outcome === "decoded") !== (value.child !== null)) invalid("MIME outcome disagrees with child identity");
  return { operation_id: matching(value.operation_id, operation), order: integer(value.order, 1, 1000),
    parent: identity(value.parent), generation_id: matching(value.generation_id, sha),
    attachment_id: matching(value.attachment_id, sha), part_path: matching(value.part_path, part),
    sibling_order: integer(value.sibling_order, 0, 1000), filename: text(value.filename, 240), outcome,
    child: value.child === null ? null : identity(value.child) };
}
export function relationKey(value: AttachmentRelation): string { return `${value.operation_id}:${value.order}`; }
function after(value: AttachmentRelation, cursor: RelationCursor): boolean {
  return value.operation_id > cursor.operationID || value.operation_id === cursor.operationID && value.order > cursor.order;
}
export function validateRelationPage(raw: unknown, selected: AttachmentIdentity, direction: RelationDirection,
  cursor?: RelationCursor, limit = relationPageSize): RelationPage {
  const value = object(raw);
  keys(value, ["items", "total", "next_operation_id", "next_order"]);
  const total = integer(value.total, 0);
  if (!Array.isArray(value.items) || value.items.length > limit || value.items.length > total) invalid("page exceeds bounds");
  let previous = cursor;
  const items = value.items.map((raw) => {
    const value = object(raw);
    keys(value, ["relation", "state", "reason"]);
    const entry = relation(value.relation);
    const selectedIdentity = direction === "outgoing" ? entry.parent : entry.child;
    if (!selectedIdentity || !sameIdentity(selectedIdentity, selected)) invalid("selected identity does not match relation");
    if (previous && !after(entry, previous)) invalid("occurrence order is not monotonic");
    previous = { operationID: entry.operation_id, order: entry.order };
    const state = text(value.state, 32);
    if (!["decoded", "pending", "indexed", "failed", "unsupported", "encrypted", "unavailable"].includes(state) ||
        entry.child === null && state !== entry.outcome) invalid("unknown or inconsistent processing state");
    return { relation: entry, state, reason: text(value.reason) };
  });
  const nextID = text(value.next_operation_id, 128);
  const nextOrder = integer(value.next_order, 0, 1000);
  if (!nextID) {
    if (nextOrder !== 0 || !cursor && items.length !== total) invalid("terminal page count disagrees");
    return { items, total };
  }
  if (!operation.test(nextID) || !previous || items.length !== limit || total <= items.length ||
      nextID !== previous.operationID || nextOrder !== previous.order) invalid("continuation order disagrees");
  return { items, total, next: { operationID: nextID, order: nextOrder } };
}
export function appendRelationPage(first: RelationPage, next: RelationPage): RelationPage {
  if (!first.next || first.total !== next.total || !next.items.length || !after(next.items[0].relation, first.next)) invalid("relations changed between pages; reload this view");
  const items = [...first.items, ...next.items];
  if (items.length > relationDisplayLimit || items.length > next.total || !next.next && items.length !== next.total) invalid("relations changed or exceeded the display limit");
  return { ...next, items };
}
export function validatePublication(raw: unknown, sample: AttachmentRelation): PublicationReceipt {
  const value = object(raw);
  keys(value, ["operation_id", "request_digest", "created_at", "inventory_state", "relations"]);
  if (matching(value.operation_id, operation) !== sample.operation_id) invalid("publication identity does not agree");
  const state = text(value.inventory_state);
  const created = text(value.created_at);
  if (!["complete", "partial"].includes(state) || !Number.isFinite(Date.parse(created)) ||
      !Array.isArray(value.relations) || value.relations.length > 1000) invalid("invalid publication inventory");
  const relations = value.relations.map((raw, index) => {
    const entry = relation(raw);
    if (entry.operation_id !== sample.operation_id || entry.order !== index + 1 ||
        !sameIdentity(entry.parent, sample.parent) || entry.generation_id !== sample.generation_id ||
        entry.attachment_id !== sample.attachment_id) invalid("publication relations do not agree");
    return entry;
  });
  const receipt: PublicationReceipt = { operation_id: sample.operation_id, request_digest: matching(value.request_digest, sha),
    created_at: created, inventory_state: state as PublicationReceipt["inventory_state"], relations };
  assertPublicationAgreement(receipt, [sample]);
  return receipt;
}
export function assertPublicationAgreement(receipt: PublicationReceipt, entries: AttachmentRelation[]): void {
  for (const entry of entries) {
    if (entry.operation_id === receipt.operation_id && JSON.stringify(receipt.relations[entry.order - 1]) !== JSON.stringify(entry)) invalid("publication and displayed occurrence do not agree");
  }
}
async function readBoundedJSON(session: string, route: string, signal: AbortSignal): Promise<unknown> {
  const response = await requestResponse(route, session, { signal });
  const reader = response.body?.getReader();
  if (!reader) invalid("missing response body");
  const chunks: Uint8Array[] = [];
  let size = 0;
  try {
    while (true) {
      signal.throwIfAborted();
      const chunk = await reader.read();
      if (chunk.done) break;
      size += chunk.value.byteLength;
      if (size > jsonByteLimit) invalid("JSON byte limit exceeded");
      chunks.push(chunk.value);
    }
    signal.throwIfAborted();
  } catch (cause) { await reader.cancel().catch(() => undefined); throw cause; }
  finally { reader.releaseLock(); }
  const bytes = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.byteLength; }
  return JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes)) as unknown;
}
export async function readRelationPage(session: string, selected: AttachmentIdentity, direction: RelationDirection,
  signal: AbortSignal, cursor?: RelationCursor): Promise<RelationPage> {
  const params = new URLSearchParams({ [direction === "outgoing" ? "parent_version_id" : "child_version_id"]: selected.version_id, limit: String(relationPageSize) });
  if (cursor) { params.set("after_operation_id", cursor.operationID); params.set("after_order", String(cursor.order)); }
  return validateRelationPage(await readBoundedJSON(session, `/api/v1/email-document-relations?${params}`, signal), selected, direction, cursor);
}
export async function readPublication(session: string, sample: AttachmentRelation, signal: AbortSignal): Promise<PublicationReceipt> {
  return validatePublication(await readBoundedJSON(session, `/api/v1/email-document-publications/${encodeURIComponent(sample.operation_id)}`, signal), sample);
}
export async function readRelatedSource(session: string, target: AttachmentIdentity, signal: AbortSignal): Promise<RelatedSelectedSource> {
  identity(target);
  const version = object(await readBoundedJSON(session, `/api/v1/versions/${target.version_id}`, signal));
  if (version.id !== target.version_id || version.node_id !== target.node_id || version.blob_hash !== target.sha256 || version.size !== target.size) invalid("exact version identity does not match the relation");
  const mimeType = version.mime_type === undefined ? "" : text(version.mime_type);
  const node = object(await readBoundedJSON(session, `/api/v1/nodes/${target.node_id}`, signal));
  if (node.id !== target.node_id || node.kind !== "file") invalid("current node identity does not match the relation");
  if (node.trashed_at !== undefined && node.trashed_at !== null) throw new APIError("The related document is in trash.", 404, "not_found");
  const name = text(node.name, 4096), path = text(node.path), modifiedAt = text(node.modified_at);
  if (!name || !path.startsWith("/") || !Number.isFinite(Date.parse(modifiedAt))) invalid("invalid current document observations");
  return { kind: "related", key: `related:${target.node_id}:${target.version_id}:${target.sha256}:${target.size}`,
    nodeID: target.node_id, versionID: target.version_id, blobHash: target.sha256, size: target.size,
    mutationRevision: integer(node.revision, 1), name, path, mimeType, modifiedAt };
}
