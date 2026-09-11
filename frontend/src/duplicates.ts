import { requestJSON } from "./api.js";
import { selectedSourceFromNode, type LiveSelectedSource, type SelectedSource } from "./selectedSource.js";

export interface DuplicateContext {
  referenceCount: number;
  references: LiveSelectedSource[];
  truncated: boolean;
}

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const sha = /^[0-9a-f]{64}$/;

function malformed(reason: string): never { throw new Error(`Malformed duplicate context: ${reason}.`); }
function object(value: unknown, field: string): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) malformed(`${field} must be an object`);
  return value as Record<string, unknown>;
}
function keys(value: Record<string, unknown>, expected: readonly string[], optional: readonly string[], field: string): void {
  const allowed = new Set([...expected, ...optional]);
  if (expected.some((key) => !Object.hasOwn(value, key)) || Object.keys(value).some((key) => !allowed.has(key))) {
    malformed(`${field} has missing or unknown fields`);
  }
}
function string(value: unknown, field: string): string {
  if (typeof value !== "string" || /[\uD800-\uDFFF]/u.test(value)) malformed(`${field} is invalid`);
  return value;
}
function integer(value: unknown, field: string, minimum: number): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < minimum) malformed(`${field} is invalid`);
  return value;
}

export async function readDuplicateContext(
  session: string, source: SelectedSource, signal: AbortSignal,
): Promise<DuplicateContext> {
  const params = new URLSearchParams({ sha256: source.blobHash, size: String(source.size) });
  const raw = object(await requestJSON<unknown>(`/api/v1/duplicates/by-hash?${params}`, session, { signal }), "receipt");
  keys(raw, ["sha256", "size", "reference_count", "references", "references_truncated"], ["$schema"], "receipt");
  if (raw.$schema !== undefined && string(raw.$schema, "receipt.$schema").length === 0) {
    malformed("receipt.$schema is invalid");
  }
  if (string(raw.sha256, "sha256") !== source.blobHash || !sha.test(source.blobHash) ||
      integer(raw.size, "size", 0) !== source.size) malformed("content identity does not match the selection");
  const referenceCount = integer(raw.reference_count, "reference_count", 2);
  if (!Array.isArray(raw.references) || raw.references.length !== Math.min(referenceCount, 16)) malformed("references are not the bounded group");
  if (typeof raw.references_truncated !== "boolean" || raw.references_truncated !== (referenceCount > 16)) malformed("truncation disagrees with the group count");
  const seen = new Set<number>();
  const references = raw.references.map((entry, index) => {
    const item = object(entry, `references[${index}]`);
    keys(item, ["node_id", "revision", "version_id", "sha256", "size", "name", "path", "media_type", "modified_at"], [], `references[${index}]`);
    const nodeID = integer(item.node_id, `references[${index}].node_id`, 1);
    const revision = integer(item.revision, `references[${index}].revision`, 1);
    const versionID = string(item.version_id, `references[${index}].version_id`);
    const hash = string(item.sha256, `references[${index}].sha256`);
    const size = integer(item.size, `references[${index}].size`, 0);
    const name = string(item.name, `references[${index}].name`);
    const path = string(item.path, `references[${index}].path`);
    const mimeType = string(item.media_type, `references[${index}].media_type`);
    const modifiedAt = string(item.modified_at, `references[${index}].modified_at`);
    if (seen.has(nodeID) || !uuid.test(versionID) || hash !== source.blobHash || size !== source.size ||
        !name || !path.startsWith("/") || !Number.isFinite(Date.parse(modifiedAt))) malformed(`references[${index}] has invalid authority`);
    seen.add(nodeID);
    return selectedSourceFromNode({ id: nodeID, name, kind: "file", current_version_id: versionID,
      blob_hash: hash, size, mime_type: mimeType, revision, created_at: modifiedAt, modified_at: modifiedAt,
      path }, path);
  });
  return { referenceCount, references, truncated: raw.references_truncated, };
}
