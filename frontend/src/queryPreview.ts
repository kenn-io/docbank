import { requestJSON } from "./api.js";
import {
  canonicalQuery,
  parseQuery,
  queryFingerprint,
  type Query,
} from "./query.js";

export interface QueryPreview {
  query: Query;
  query_fingerprint: string;
  dependencies: {
    kind: "tag" | "collection" | "saved";
    id: string;
    revision: number;
  }[];
}

const dependencyKinds = new Set(["tag", "collection", "saved"]);
const uuidV4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const encoder = new TextEncoder();

function malformed(reason: string): never {
  throw new Error(`Malformed query preview receipt: ${reason}.`);
}

function object(value: unknown, field: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    malformed(`${field} must be an object`);
  }
  return value as Record<string, unknown>;
}

function requireKeys(value: Record<string, unknown>, expected: readonly string[], field: string): void {
  const keys = Object.keys(value);
  if (keys.length !== expected.length || expected.some((key) => !Object.hasOwn(value, key))) {
    malformed(`${field} has missing or unknown fields`);
  }
}

export async function previewQuery(
  session: string,
  query: Query,
  signal: AbortSignal,
): Promise<QueryPreview> {
  const canonical = canonicalQuery(query);
  const expected = await queryFingerprint(query);
  const raw = await requestJSON<unknown>("/api/v1/queries/parse", session, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: canonical,
    signal,
  });

  const receipt = object(raw, "receipt");
  const receiptKeys = Object.hasOwn(receipt, "$schema")
    ? ["$schema", "query", "query_fingerprint", "dependencies"]
    : ["query", "query_fingerprint", "dependencies"];
  requireKeys(receipt, receiptKeys, "receipt");
  if (Object.hasOwn(receipt, "$schema") &&
    (typeof receipt.$schema !== "string" || receipt.$schema.length === 0)) {
    malformed("receipt.$schema is invalid");
  }

  const rawQuery = object(receipt.query, "query");
  requireKeys(rawQuery, ["v", "text", "syntax", "mode", "filters", "sort"], "query");
  object(rawQuery.filters, "query.filters");
  const rawSort = object(rawQuery.sort, "query.sort");
  requireKeys(rawSort, ["field", "direction"], "query.sort");

  let decoded: Query;
  try {
    decoded = parseQuery(JSON.stringify(rawQuery));
  } catch {
    malformed("query does not satisfy QueryV1");
  }
  if (canonicalQuery(decoded) !== canonical) malformed("query does not match the request");
  if (receipt.query_fingerprint !== expected) malformed("fingerprint does not match the request");
  if (!Array.isArray(receipt.dependencies) || receipt.dependencies.length > 256) {
    malformed("dependencies must be an array of at most 256 identities");
  }

  const identities = new Set<string>();
  const dependencies: QueryPreview["dependencies"] = receipt.dependencies.map((value, index) => {
    const dependency = object(value, `dependencies[${index}]`);
    requireKeys(dependency, ["kind", "id", "revision"], `dependencies[${index}]`);
    if (typeof dependency.kind !== "string" || !dependencyKinds.has(dependency.kind)) {
      malformed(`dependencies[${index}].kind is unknown`);
    }
    if (typeof dependency.id !== "string" || !uuidV4.test(dependency.id)) {
      malformed(`dependencies[${index}].id is invalid`);
    }
    if (typeof dependency.revision !== "number" ||
      !Number.isSafeInteger(dependency.revision) || dependency.revision < 1) {
      malformed(`dependencies[${index}].revision is invalid`);
    }
    const kind = dependency.kind as QueryPreview["dependencies"][number]["kind"];
    const identity = `${kind}\0${dependency.id}`;
    if (identities.has(identity)) malformed(`dependencies[${index}] repeats an identity`);
    identities.add(identity);
    return { kind, id: dependency.id, revision: dependency.revision };
  });

  return { query: decoded, query_fingerprint: expected, dependencies };
}

export function querySelection(
  text: string,
  position: unknown,
): { start: number; end: number } | null {
  if (position === null || typeof position !== "object" || Array.isArray(position)) return null;
  const span = position as Record<string, unknown>;
  if (!Number.isSafeInteger(span.offset) || !Number.isSafeInteger(span.end) ||
    typeof span.offset !== "number" || typeof span.end !== "number" ||
    span.offset < 0 || span.end < span.offset) return null;

  const boundaries = new Map<number, number>([[0, 0]]);
  let byteOffset = 0;
  let codeUnitOffset = 0;
  for (const point of text) {
    byteOffset += encoder.encode(point).length;
    codeUnitOffset += point.length;
    boundaries.set(byteOffset, codeUnitOffset);
  }
  const start = boundaries.get(span.offset);
  const end = boundaries.get(span.end);
  return start === undefined || end === undefined ? null : { start, end };
}
