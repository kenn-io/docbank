import { requestJSON } from "./api.js";
import { canonicalQuery } from "./query.js";
import type { SnapshotPage } from "./snapshots.js";

const uuidV4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const fingerprint = /^sha256:[0-9a-f]{64}$/;
const dependencyKinds = new Set(["tag", "collection", "saved"]);

function malformed(reason: string): never {
  throw new Error(`Malformed query highlight receipt: ${reason}.`);
}

function object(value: unknown, field: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    malformed(`${field} must be an object`);
  }
  return value as Record<string, unknown>;
}

function exactKeys(value: Record<string, unknown>, expected: readonly string[], optional: readonly string[], field: string): void {
  const allowed = new Set([...expected, ...optional]);
  if (expected.some((key) => !Object.hasOwn(value, key)) || Object.keys(value).some((key) => !allowed.has(key))) {
    malformed(`${field} has missing or unknown fields`);
  }
}

function unicode(value: unknown, field: string): string {
  if (typeof value !== "string" || /[\uD800-\uDFFF]/u.test(value)) malformed(`${field} is not Unicode text`);
  return value;
}

export async function readQueryHighlights(
  session: string,
  snapshot: SnapshotPage,
  signal: AbortSignal,
): Promise<string[]> {
  const raw = object(await requestJSON<unknown>("/api/v1/queries/highlights", session, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: canonicalQuery(snapshot.query),
    signal,
  }), "receipt");
  exactKeys(raw, ["query_fingerprint", "dependencies", "terms"], ["$schema"], "receipt");
  if (raw.$schema !== undefined && unicode(raw.$schema, "receipt.$schema").length === 0) {
    malformed("receipt.$schema is invalid");
  }
  if (typeof raw.query_fingerprint !== "string" || !fingerprint.test(raw.query_fingerprint) ||
      raw.query_fingerprint !== snapshot.query_fingerprint) {
    malformed("fingerprint does not match the accepted snapshot");
  }
  if (!Array.isArray(raw.dependencies) || raw.dependencies.length > 256 ||
      raw.dependencies.length !== snapshot.dependencies.length) {
    malformed("dependencies do not match the accepted snapshot");
  }
  raw.dependencies.forEach((value, index) => {
    const dependency = object(value, `dependencies[${index}]`);
    exactKeys(dependency, ["kind", "id", "revision"], [], `dependencies[${index}]`);
    const accepted = snapshot.dependencies[index];
    if (typeof dependency.kind !== "string" || !dependencyKinds.has(dependency.kind) ||
        typeof dependency.id !== "string" || !uuidV4.test(dependency.id) ||
        typeof dependency.revision !== "number" || !Number.isSafeInteger(dependency.revision) || dependency.revision < 1 ||
        dependency.kind !== accepted?.kind || dependency.id !== accepted.id || dependency.revision !== accepted.revision) {
      malformed(`dependencies[${index}] does not match the accepted snapshot`);
    }
  });
  if (!Array.isArray(raw.terms) || raw.terms.length > 64) malformed("terms exceed their bound");
  const seen = new Set<string>();
  return raw.terms.map((value, index) => {
    const term = unicode(value, `terms[${index}]`);
    const length = [...term].length;
    if (length < 1 || length > 256 || seen.has(term)) malformed(`terms[${index}] is invalid or duplicated`);
    seen.add(term);
    return term;
  });
}
