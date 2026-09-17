import type { Collection, CollectionPage, CollectionMemberPage, CollectionLabel } from "./generated/docbank.js";
export type { Collection, CollectionPage, CollectionMemberPage, CollectionLabel } from "./generated/docbank.js";
import * as generated from "./generated/docbank.js";
import { type Node } from "./generated/docbank.js";









const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const encoder = new TextEncoder();

function check(valid: unknown): asserts valid {
  if (!valid) throw new Error("Collection receipt does not match the requested identity, page or revision. Reload before continuing.");
}

function object(value: unknown): Record<string, unknown> {
  check(value !== null && typeof value === "object" && !Array.isArray(value));
  return value as Record<string, unknown>;
}

function integer(value: unknown, min = 0): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= min;
}

function timestamp(value: unknown): value is string {
  return typeof value === "string" && Number.isFinite(Date.parse(value));
}

function normalizeLabel(value: unknown): string | null {
  if (value === null) return null;
  if (typeof value !== "string") throw new Error("A collection label must be text or null.");
  const normalized = value.normalize("NFC");
  if (!normalized.trim() || /[\p{Cc}\uD800-\uDFFF]/u.test(normalized) || encoder.encode(normalized).length > 256) {
    throw new Error("A collection label must contain 1–256 UTF-8 bytes without control characters.");
  }
  return normalized;
}

function validateID(id: string): void {
  if (!uuid.test(id)) throw new Error("Invalid collection ID.");
}

function validatePage(offset: number, limit: number): void {
  if (!integer(offset) || !integer(limit, 1) || limit > 1000) throw new Error("Invalid collection page.");
}

export function readCollection(value: unknown): Collection {
  const record = object(value);
  check(typeof record.id === "string" && uuid.test(record.id));
  check(typeof record.source_kind === "string" && record.source_kind.length > 0 &&
    typeof record.source_description === "string" && record.source_description.length > 0);
  check(timestamp(record.started_at) && timestamp(record.label_updated_at));
  check(integer(record.file_count) && integer(record.total_bytes) && integer(record.label_revision, 1));
  check(record.label === normalizeLabel(record.label));
  return record as unknown as Collection;
}

function readPage(value: unknown, offset: number, limit: number) {
  const page = object(value);
  check(page.offset === offset && page.limit === limit && integer(page.total) && Array.isArray(page.items));
  check(page.items.length === Math.min(limit, Math.max(0, page.total - offset)));
  return { items: page.items as unknown[], total: page.total, limit, offset };
}

export async function collections(session: string, offset = 0, limit = 100): Promise<CollectionPage> {
  validatePage(offset, limit);
  const raw = await generated.listCollections({ limit, offset }, { session });
  const page = readPage(raw, offset, limit);
  const items = page.items.map(readCollection);
  check(new Set(items.map((item) => item.id)).size === items.length && items.every((item) => item.file_count > 0));
  return { ...page, items };
}

export async function collectionByID(session: string, id: string): Promise<Collection> {
  validateID(id);
  const result = readCollection(await generated.getCollection(id, { session }));
  check(result.id === id);
  return result;
}

export async function collectionMembers(session: string, id: string, offset = 0, limit = 100): Promise<CollectionMemberPage> {
  validateID(id);
  validatePage(offset, limit);
  const raw = object(await generated.listCollectionMembers(id, { limit, offset }, { session }));
  const collection = readCollection(raw.collection);
  const page = readPage(raw, offset, limit);
  check(collection.id === id && page.total === collection.file_count);
  const items = page.items.map((value) => {
    const node = object(value);
    check(integer(node.id, 1) && integer(node.revision, 1) && integer(node.size) && node.kind === "file" && !node.trashed_at);
    check(typeof node.name === "string" && node.name.length > 0 && typeof node.path === "string" && node.path.startsWith("/") &&
      !node.path.includes("\0") && node.path.slice(1).split("/").every((segment) => segment && segment !== "." && segment !== ".."));
    check(typeof node.current_version_id === "string" && uuid.test(node.current_version_id) &&
      typeof node.blob_hash === "string" && /^[0-9a-f]{64}$/.test(node.blob_hash));
    check(timestamp(node.created_at) && timestamp(node.modified_at));
    return node as unknown as Node;
  });
  check(new Set(items.map((item) => item.id)).size === items.length);
  check(items.reduce((sum, item) => sum + item.size, 0) <= collection.total_bytes);
  return { ...page, collection, items };
}

async function readLabel(response: Response, id: string): Promise<CollectionLabel> {
  const label = object(await response.json());
  check(label.ingest_id === id && integer(label.revision, 1) && timestamp(label.updated_at));
  check(label.label === normalizeLabel(label.label));
  check(response.headers.get("ETag") === `"${label.revision}"`);
  return label as unknown as CollectionLabel;
}

export async function collectionLabel(session: string, id: string): Promise<CollectionLabel> {
  validateID(id);
  return readLabel(await generated.getCollectionLabel(id, { session }), id);
}

export async function setCollectionLabel(session: string, observed: CollectionLabel, value: string | null): Promise<CollectionLabel> {
  validateID(observed.ingest_id);
  if (!integer(observed.revision, 1)) throw new Error("A positive label revision is required.");
  const label = normalizeLabel(value);
  const expectedRevision = observed.revision + (label === observed.label ? 0 : 1);
  check(Number.isSafeInteger(expectedRevision));
  const response = await generated.setCollectionLabel(observed.ingest_id, { label }, { "If-Match": `"${observed.revision}"` }, { session });
  const result = await readLabel(response, observed.ingest_id);
  check(result.label === label && result.revision === expectedRevision && Date.parse(result.updated_at) >= Date.parse(observed.updated_at));
  return result;
}
