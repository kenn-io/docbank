import { requestJSON, requestResponse } from "./api.js";
import {
  canonicalHighlightSet, canonicalQuery, highlightSetFingerprint,
  parseHighlightSet, parseQuery, queryFingerprint,
  type HighlightSet, type Query,
} from "./query.js";

export type Definition =
  | { kind: "query"; payload: Query }
  | { kind: "highlight_set"; payload: HighlightSet };
export type SavedQueryKind = Definition["kind"];
export type SavedQuery = Definition & {
  id: string;
  name: string;
  description: string;
  fingerprint: string;
  revision: number;
  created_at: string;
  updated_at: string;
};
export interface SavedQueryPage {
  items: SavedQuery[];
  total: number;
  limit: number;
  offset: number;
}
export type SavedQueryCreate = Definition & { name: string; description?: string };
export interface SavedQueryPatch {
  name?: string;
  description?: string;
  payload?: Query | HighlightSet;
}

const route = "/api/v1/saved-queries";
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const encoder = new TextEncoder();

function requireReceipt(valid: unknown): asserts valid {
  if (!valid) throw new Error("Saved definition receipt does not match the request. Reload before continuing.");
}

function object(value: unknown): Record<string, unknown> {
  requireReceipt(value !== null && typeof value === "object" && !Array.isArray(value));
  return value as Record<string, unknown>;
}

function positiveInteger(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value > 0;
}

function validString(value: unknown, maxBytes: number): value is string {
  return typeof value === "string" && !/[\uD800-\uDFFF]/u.test(value) && encoder.encode(value).length <= maxBytes;
}

function nameValue(value: unknown): string {
  if (typeof value !== "string") throw new Error("A saved definition needs a name.");
  const name = value.normalize("NFC");
  if (!validString(name, 256) || !name.trim() || /[\p{Cc}]/u.test(name)) {
    throw new Error("Name must contain 1–256 UTF-8 bytes and no control characters.");
  }
  return name;
}

function descriptionValue(value: unknown): string {
  if (typeof value !== "string") throw new Error("Description must be text.");
  const description = value.replaceAll("\r\n", "\n");
  if (!validString(description, 4096) || /[\p{Cc}]/u.test(description.replace(/[\t\n]/g, ""))) {
    throw new Error("Description exceeds 4096 UTF-8 bytes or contains control characters.");
  }
  return description;
}

function definition(kind: unknown, payload: unknown): Definition {
  if (kind === "query") return { kind, payload: parseQuery(JSON.stringify(payload)) };
  if (kind === "highlight_set") return { kind, payload: parseHighlightSet(JSON.stringify(payload)) };
  throw new Error("Unknown saved definition kind.");
}

function canonical(value: Definition): string {
  return value.kind === "query" ? canonicalQuery(value.payload) : canonicalHighlightSet(value.payload);
}

async function receipt(value: unknown): Promise<SavedQuery> {
  const raw = object(value);
  requireReceipt(typeof raw.id === "string" && uuid.test(raw.id) && positiveInteger(raw.revision));
  requireReceipt(typeof raw.created_at === "string" && Number.isFinite(Date.parse(raw.created_at)) &&
    typeof raw.updated_at === "string" && Number.isFinite(Date.parse(raw.updated_at)));
  const name = nameValue(raw.name);
  const description = descriptionValue(raw.description);
  requireReceipt(name === raw.name && description === raw.description);
  const decoded = definition(raw.kind, raw.payload);
  const fingerprint = decoded.kind === "query"
    ? await queryFingerprint(decoded.payload) : await highlightSetFingerprint(decoded.payload);
  requireReceipt(raw.fingerprint === fingerprint);
  return {
    ...decoded, id: raw.id, name, description, fingerprint,
    revision: raw.revision, created_at: raw.created_at, updated_at: raw.updated_at,
  };
}

function path(id: string): string {
  if (!uuid.test(id)) throw new Error("Invalid saved definition ID.");
  return `${route}/${id}`;
}

function fence(value: SavedQuery): HeadersInit {
  if (!positiveInteger(value.revision)) throw new Error("A positive saved definition revision is required.");
  return { "Content-Type": "application/json", "If-Match": `"${value.revision}"` };
}

async function readResponse(response: Response): Promise<SavedQuery> {
  const saved = await receipt(await response.json());
  requireReceipt(response.headers.get("ETag") === `"${saved.revision}"`);
  return saved;
}

export async function listSavedQueries(
  session: string, kind?: SavedQueryKind, offset = 0, limit = 100,
): Promise<SavedQueryPage> {
  if (!Number.isSafeInteger(offset) || offset < 0 || !positiveInteger(limit) || limit > 1000 ||
    kind !== undefined && kind !== "query" && kind !== "highlight_set") throw new Error("Invalid saved definition page.");
  const params = new URLSearchParams({ limit: String(limit), offset: String(offset) });
  if (kind) params.set("kind", kind);
  const page = object(await requestJSON<unknown>(`${route}?${params}`, session));
  requireReceipt(page.limit === limit && page.offset === offset &&
    typeof page.total === "number" && Number.isSafeInteger(page.total) && page.total >= 0 && Array.isArray(page.items));
  requireReceipt(page.items.length === Math.min(limit, Math.max(0, page.total - offset)));
  const items = await Promise.all(page.items.map(receipt));
  requireReceipt(new Set(items.map((item) => item.id)).size === items.length && items.every((item) => !kind || item.kind === kind));
  return { items, total: page.total, limit, offset };
}

export async function getSavedQuery(session: string, id: string): Promise<SavedQuery> {
  const saved = await readResponse(await requestResponse(path(id), session));
  requireReceipt(saved.id === id);
  return saved;
}

export async function createSavedQuery(session: string, input: SavedQueryCreate): Promise<SavedQuery> {
  const expected = { ...definition(input.kind, input.payload), name: nameValue(input.name), description: descriptionValue(input.description ?? "") };
  const saved = await readResponse(await requestResponse(route, session, {
    method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(expected),
  }));
  requireReceipt(saved.revision === 1 && sameDefinition(saved, expected));
  return saved;
}

function sameDefinition(left: SavedQuery, right: Definition & { name: string; description: string }): boolean {
  return left.kind === right.kind && left.name === right.name && left.description === right.description && canonical(left) === canonical(right);
}

export async function updateSavedQuery(session: string, observed: SavedQuery, patch: SavedQueryPatch): Promise<SavedQuery> {
  const headers = fence(observed);
  if (Object.keys(patch).some((key) => !["name", "description", "payload"].includes(key))) throw new Error("Unknown saved definition patch field.");
  const body: SavedQueryPatch = {};
  if (patch.name !== undefined) body.name = nameValue(patch.name);
  if (patch.description !== undefined) body.description = descriptionValue(patch.description);
  if (patch.payload !== undefined) body.payload = definition(observed.kind, patch.payload).payload;
  if (!Object.keys(body).length) throw new Error("No changes supplied.");
  const expected = {
    ...definition(observed.kind, body.payload ?? observed.payload),
    name: body.name ?? observed.name, description: body.description ?? observed.description,
  };
  const revision = observed.revision + (sameDefinition(observed, expected) ? 0 : 1);
  requireReceipt(Number.isSafeInteger(revision));
  const saved = await readResponse(await requestResponse(path(observed.id), session, { method: "PATCH", headers, body: JSON.stringify(body) }));
  requireReceipt(saved.id === observed.id && saved.revision === revision && saved.created_at === observed.created_at && sameDefinition(saved, expected));
  return saved;
}

export async function deleteSavedQuery(session: string, observed: SavedQuery): Promise<SavedQuery> {
  const saved = await readResponse(await requestResponse(path(observed.id), session, { method: "DELETE", headers: fence(observed) }));
  requireReceipt(saved.id === observed.id && saved.revision === observed.revision &&
    saved.created_at === observed.created_at && saved.updated_at === observed.updated_at && sameDefinition(saved, observed));
  return saved;
}
