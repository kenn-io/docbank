import { requestResponse } from "./api.js";
import { canonicalQuery, parseQuery, type Query } from "./query.js";

export type SnapshotOptions = {
  profile?: string;
  page_size?: 50 | 100 | 250;
  facets?: string[];
};

export interface SnapshotRow {
  node_id: number;
  content_version_id: string;
  blob_hash: string;
  size: number;
  revision: number;
  name: string;
  path: string;
  mime_type: string;
  media_family: "email" | "document" | "spreadsheet" | "presentation" | "image" |
    "audio_video" | "text" | "source_code" | "web" | "calendar" | "archive" | "cad" | "unknown";
  modified_at: string;
  sort_key: string;
  tags: { id: string; name: string; revision: number }[];
  collection_ids: string[];
  display_collection_id?: string;
  display_collection_label?: string | null;
  coverage_state?: "complete" | "partial" | "failed" | "unprocessed" | "none" | "unavailable";
  coverage_build_id?: string;
  coverage_attachment_id?: string;
  excerpt?: string;
}

export interface WorkspaceQueryResponse {
  query: Query;
  dependencies: { kind: "tag" | "collection" | "saved"; id: string; revision: number }[];
  query_fingerprint: string;
  member_hash: string;
  snapshot_fingerprint: string;
  generation: { kind: "native" | "rendition"; generation_id?: string };
  coverage: {
    configuration: "configured" | "unconfigured" | "profile_required";
    profile_fingerprint?: string;
  };
  observed_at: string;
  page_size: 50 | 100 | 250;
  total: number;
  total_bytes: number;
  rows: SnapshotRow[];
  facets: {
    dimension: "collections" | "tags" | "media_family" | "extension" | "modified" |
      "size" | "text_coverage" | "duplicates";
    available: boolean;
    reason?: string;
    total?: number | null;
    values: { key: string; label: string; count: number; selected: boolean }[];
    missing?: number | null;
    other?: number | null;
  }[];
  snapshot: true;
  snapshot_id: string;
  created_at: string;
  expires_at: string;
  previous_cursor?: string;
  next_cursor?: string;
}

export type SnapshotPage = WorkspaceQueryResponse;
export type SnapshotMember = Pick<SnapshotRow,
  "node_id" | "content_version_id" | "blob_hash" | "size" | "revision">;
export type VerifiedSnapshotTargets = { snapshot: SnapshotPage; members: SnapshotMember[] };

export interface SavedQueryRunResult {
  run: {
    run_id: string;
    saved_query_id: string;
    saved_query_revision: number;
    query_fingerprint: string;
    snapshot_id: string;
    member_hash: string;
    total: number;
    total_bytes: number;
    ran_at: string;
    expires_at: string;
    previous_run_id?: string;
    previous_member_hash?: string;
    previous_total?: number | null;
    previous_query_fingerprint?: string;
    comparison: { hash_changed: boolean; total_delta: number; definition_changed: boolean };
  };
  snapshot: SnapshotPage;
}

const maxResponseBytes = 32 * 1024 * 1024;
const maxSnapshotMembers = 250_000;
const maxRowBytes = 64 * 1024;
const maxCursorBytes = 2048;
const snapshotAbsoluteLifetimeMilliseconds = 30 * 60 * 1000;
const uuidV4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const sha256 = /^[0-9a-f]{64}$/;
const prefixedSHA256 = /^sha256:[0-9a-f]{64}$/;
const snapshotIdentity = /^[0-9a-f]{32}$/;
const timestamp = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$/;
const mediaFamilies = new Set<SnapshotRow["media_family"]>([
  "email", "document", "spreadsheet", "presentation", "image", "audio_video", "text",
  "source_code", "web", "calendar", "archive", "cad", "unknown",
]);
const coverageStates = new Set<NonNullable<SnapshotRow["coverage_state"]>>([
  "complete", "partial", "failed", "unprocessed", "none", "unavailable",
]);
const dependencyKinds = new Set(["tag", "collection", "saved"]);
const facetDimensions = new Set<WorkspaceQueryResponse["facets"][number]["dimension"]>([
  "collections", "tags", "media_family", "extension", "modified", "size", "text_coverage", "duplicates",
]);
const encoder = new TextEncoder();

function malformed(reason: string): never {
  throw new Error(`Malformed snapshot receipt: ${reason}.`);
}

function savedMalformed(reason: string): never {
  throw new Error(`Malformed saved query run receipt: ${reason}.`);
}

function record(value: unknown, field: string): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) malformed(`${field} must be an object`);
  return value as Record<string, unknown>;
}

function keys(
  value: Record<string, unknown>, required: readonly string[], optional: readonly string[], field: string,
): void {
  const allowed = new Set([...required, ...optional]);
  if (required.some((key) => !Object.hasOwn(value, key)) || Object.keys(value).some((key) => !allowed.has(key))) {
    malformed(`${field} has missing or unknown fields`);
  }
}

function validUnicode(value: string): boolean {
  for (let index = 0; index < value.length; index++) {
    const unit = value.charCodeAt(index);
    if (unit >= 0xd800 && unit <= 0xdbff) {
      const next = value.charCodeAt(++index);
      if (!(next >= 0xdc00 && next <= 0xdfff)) return false;
    } else if (unit >= 0xdc00 && unit <= 0xdfff) {
      return false;
    }
  }
  return true;
}

function string(value: unknown, field: string): string {
  if (typeof value !== "string" || !validUnicode(value)) malformed(`${field} must be a Unicode string`);
  return value;
}

function optionalString(value: unknown, field: string): string | undefined {
  return value === undefined ? undefined : string(value, field);
}

function integer(value: unknown, field: string, minimum = 0): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < minimum) {
    malformed(`${field} must be a safe integer of at least ${minimum}`);
  }
  return value;
}

function dateTime(value: unknown, field: string): string {
  const raw = string(value, field);
  const match = timestamp.exec(raw);
  if (!match) malformed(`${field} is not RFC3339`);
  const [, yearRaw, monthRaw, dayRaw, hourRaw, minuteRaw, secondRaw, fractionRaw = "", zone] = match;
  const [year, month, day, hour, minute, second] =
    [yearRaw, monthRaw, dayRaw, hourRaw, minuteRaw, secondRaw].map(Number);
  if (month < 1 || month > 12 || hour > 23 || minute > 59 || second > 59) malformed(`${field} is not RFC3339`);
  const local = new Date(0);
  local.setUTCHours(hour, minute, second, Number(fractionRaw.padEnd(3, "0").slice(0, 3)));
  local.setUTCFullYear(year, month - 1, day);
  if (local.getUTCFullYear() !== year || local.getUTCMonth() !== month - 1 || local.getUTCDate() !== day) {
    malformed(`${field} is not RFC3339`);
  }
  if (zone !== "Z") {
    const zoneHour = Number(zone.slice(1, 3));
    const zoneMinute = Number(zone.slice(4, 6));
    if (zoneHour > 23 || zoneMinute > 59) malformed(`${field} is not RFC3339`);
  }
  if (!Number.isFinite(Date.parse(raw))) malformed(`${field} is not RFC3339`);
  return raw;
}

function cursor(value: unknown, field: string): string | undefined {
  if (value === undefined) return undefined;
  const raw = string(value, field);
  if (raw.length === 0 || encoder.encode(raw).length > maxCursorBytes) malformed(`${field} is outside its byte bound`);
  return raw;
}

function parseTag(value: unknown, index: number): SnapshotRow["tags"][number] {
  const tag = record(value, `row.tags[${index}]`);
  keys(tag, ["id", "name", "revision"], [], `row.tags[${index}]`);
  const id = string(tag.id, `row.tags[${index}].id`);
  if (!uuidV4.test(id)) malformed(`row.tags[${index}].id is invalid`);
  return { id, name: string(tag.name, `row.tags[${index}].name`), revision: integer(tag.revision, `row.tags[${index}].revision`, 1) };
}

function parseRow(value: unknown, index: number): SnapshotRow {
  const raw = record(value, `rows[${index}]`);
  keys(raw, [
    "node_id", "content_version_id", "blob_hash", "size", "revision", "name", "path", "mime_type",
    "media_family", "modified_at", "sort_key", "tags", "collection_ids",
  ], [
    "display_collection_id", "display_collection_label", "coverage_state", "coverage_build_id",
    "coverage_attachment_id", "excerpt",
  ], `rows[${index}]`);
  if (encoder.encode(JSON.stringify(raw)).length > maxRowBytes) malformed(`rows[${index}] exceeds 64 KiB`);
  const contentVersionID = string(raw.content_version_id, `rows[${index}].content_version_id`);
  const blobHash = string(raw.blob_hash, `rows[${index}].blob_hash`);
  const mediaFamily = string(raw.media_family, `rows[${index}].media_family`) as SnapshotRow["media_family"];
  if (!uuidV4.test(contentVersionID)) malformed(`rows[${index}].content_version_id is invalid`);
  if (!sha256.test(blobHash)) malformed(`rows[${index}].blob_hash is invalid`);
  if (!mediaFamilies.has(mediaFamily)) malformed(`rows[${index}].media_family is invalid`);
  if (!Array.isArray(raw.tags)) malformed(`rows[${index}].tags must be an array`);
  const tags = raw.tags.map((tag, tagIndex) => parseTag(tag, tagIndex));
  if (new Set(tags.map((tag) => tag.id)).size !== tags.length) malformed(`rows[${index}].tags repeat an identity`);
  if (!Array.isArray(raw.collection_ids)) malformed(`rows[${index}].collection_ids must be an array`);
  const collectionIDs = raw.collection_ids.map((id, collectionIndex) => {
    const parsed = string(id, `rows[${index}].collection_ids[${collectionIndex}]`);
    if (!uuidV4.test(parsed)) malformed(`rows[${index}].collection_ids[${collectionIndex}] is invalid`);
    return parsed;
  });
  if (new Set(collectionIDs).size !== collectionIDs.length) malformed(`rows[${index}].collection_ids repeat an identity`);
  const displayCollectionID = optionalString(raw.display_collection_id, `rows[${index}].display_collection_id`);
  if (displayCollectionID !== undefined && !uuidV4.test(displayCollectionID)) {
    malformed(`rows[${index}].display_collection_id is invalid`);
  }
  const displayCollectionLabel = raw.display_collection_label === null
    ? null : optionalString(raw.display_collection_label, `rows[${index}].display_collection_label`);
  const coverageState = optionalString(raw.coverage_state, `rows[${index}].coverage_state`) as SnapshotRow["coverage_state"];
  if (coverageState !== undefined && !coverageStates.has(coverageState)) malformed(`rows[${index}].coverage_state is invalid`);
  return {
    node_id: integer(raw.node_id, `rows[${index}].node_id`, 1),
    content_version_id: contentVersionID,
    blob_hash: blobHash,
    size: integer(raw.size, `rows[${index}].size`),
    revision: integer(raw.revision, `rows[${index}].revision`, 1),
    name: string(raw.name, `rows[${index}].name`),
    path: string(raw.path, `rows[${index}].path`),
    mime_type: string(raw.mime_type, `rows[${index}].mime_type`),
    media_family: mediaFamily,
    modified_at: dateTime(raw.modified_at, `rows[${index}].modified_at`),
    sort_key: string(raw.sort_key, `rows[${index}].sort_key`),
    tags,
    collection_ids: collectionIDs,
    ...(displayCollectionID === undefined ? {} : { display_collection_id: displayCollectionID }),
    ...(displayCollectionLabel === undefined ? {} : { display_collection_label: displayCollectionLabel }),
    ...(coverageState === undefined ? {} : { coverage_state: coverageState }),
    ...optionalMappedString(raw, "coverage_build_id", `rows[${index}].coverage_build_id`),
    ...optionalMappedString(raw, "coverage_attachment_id", `rows[${index}].coverage_attachment_id`),
    ...optionalMappedString(raw, "excerpt", `rows[${index}].excerpt`),
  };
}

function optionalMappedString(
  raw: Record<string, unknown>, key: "coverage_build_id" | "coverage_attachment_id" | "excerpt", field: string,
): Partial<Pick<SnapshotRow, "coverage_build_id" | "coverage_attachment_id" | "excerpt">> {
  const value = optionalString(raw[key], field);
  return value === undefined ? {} : { [key]: value };
}

function parseFacet(value: unknown, index: number): WorkspaceQueryResponse["facets"][number] {
  const raw = record(value, `facets[${index}]`);
  keys(raw, ["dimension", "available", "values"], ["reason", "total", "missing", "other"], `facets[${index}]`);
  const dimension = string(raw.dimension, `facets[${index}].dimension`) as WorkspaceQueryResponse["facets"][number]["dimension"];
  if (!facetDimensions.has(dimension)) malformed(`facets[${index}].dimension is unknown`);
  if (typeof raw.available !== "boolean") malformed(`facets[${index}].available must be boolean`);
  if (!Array.isArray(raw.values) || raw.values.length > 114) malformed(`facets[${index}].values exceeds its bound`);
  const values = raw.values.map((value, valueIndex) => {
    const facetValue = record(value, `facets[${index}].values[${valueIndex}]`);
    keys(facetValue, ["key", "label", "count", "selected"], [], `facets[${index}].values[${valueIndex}]`);
    if (typeof facetValue.selected !== "boolean") malformed(`facets[${index}].values[${valueIndex}].selected must be boolean`);
    return {
      key: string(facetValue.key, `facets[${index}].values[${valueIndex}].key`),
      label: string(facetValue.label, `facets[${index}].values[${valueIndex}].label`),
      count: integer(facetValue.count, `facets[${index}].values[${valueIndex}].count`),
      selected: facetValue.selected,
    };
  });
  if (new Set(values.map((entry) => entry.key)).size !== values.length) malformed(`facets[${index}].values repeat a key`);
  const reason = optionalString(raw.reason, `facets[${index}].reason`);
  const count = (key: "total" | "missing" | "other"): number | null | undefined =>
    raw[key] === undefined || raw[key] === null ? raw[key] as null | undefined : integer(raw[key], `facets[${index}].${key}`);
  const total = count("total");
  const missing = count("missing");
  const other = count("other");
  if (raw.available) {
    if (reason !== undefined && reason !== "") malformed(`facets[${index}] has an unavailable reason`);
    if (total === undefined || total === null || missing === undefined || missing === null || other === undefined || other === null) {
      malformed(`facets[${index}] lacks available counts`);
    }
  } else if (!reason || values.length !== 0 || total != null || missing != null || other != null) {
    malformed(`facets[${index}] fabricates unavailable counts`);
  }
  return {
    dimension, available: raw.available, values,
    ...(reason === undefined ? {} : { reason }),
    ...(total === undefined ? {} : { total }),
    ...(missing === undefined ? {} : { missing }),
    ...(other === undefined ? {} : { other }),
  };
}

async function queryDigest(value: Query): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", encoder.encode(canonicalQuery(value)));
  return `sha256:${Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

async function parseSnapshot(value: unknown, expectedQuery?: Query): Promise<SnapshotPage> {
  const raw = record(value, "receipt");
  keys(raw, [
    "query", "dependencies", "query_fingerprint", "member_hash", "snapshot_fingerprint", "generation",
    "coverage", "observed_at", "page_size", "total", "total_bytes", "rows", "facets", "snapshot",
    "snapshot_id", "created_at", "expires_at",
  ], ["$schema", "previous_cursor", "next_cursor"], "receipt");
  if (raw.$schema !== undefined && string(raw.$schema, "receipt.$schema").length === 0) malformed("receipt.$schema is invalid");
  const queryRaw = record(raw.query, "query");
  let decodedQuery: Query;
  try {
    decodedQuery = parseQuery(JSON.stringify(queryRaw));
  } catch {
    malformed("query does not satisfy QueryV1");
  }
  if (expectedQuery !== undefined && canonicalQuery(decodedQuery) !== canonicalQuery(expectedQuery)) {
    malformed("query does not match the request");
  }
  const queryFingerprint = string(raw.query_fingerprint, "query_fingerprint");
  if (!prefixedSHA256.test(queryFingerprint) || queryFingerprint !== await queryDigest(decodedQuery)) {
    malformed("query fingerprint is inconsistent");
  }
  if (!Array.isArray(raw.dependencies) || raw.dependencies.length > 256) malformed("dependencies exceeds its bound");
  const identities = new Set<string>();
  const dependencies = raw.dependencies.map((value, index) => {
    const dependency = record(value, `dependencies[${index}]`);
    keys(dependency, ["kind", "id", "revision"], [], `dependencies[${index}]`);
    const kind = string(dependency.kind, `dependencies[${index}].kind`) as WorkspaceQueryResponse["dependencies"][number]["kind"];
    const id = string(dependency.id, `dependencies[${index}].id`);
    if (!dependencyKinds.has(kind)) malformed(`dependencies[${index}].kind is unknown`);
    if (!uuidV4.test(id)) malformed(`dependencies[${index}].id is invalid`);
    const identity = `${kind}\0${id}`;
    if (identities.has(identity)) malformed(`dependencies[${index}] repeats an identity`);
    identities.add(identity);
    return { kind, id, revision: integer(dependency.revision, `dependencies[${index}].revision`, 1) };
  });
  const generationRaw = record(raw.generation, "generation");
  keys(generationRaw, ["kind"], ["generation_id"], "generation");
  const generationKind = string(generationRaw.kind, "generation.kind");
  const generationID = optionalString(generationRaw.generation_id, "generation.generation_id");
  if (generationKind !== "native" && generationKind !== "rendition") malformed("generation.kind is unknown");
  if ((generationKind === "native" && generationID !== undefined) || (generationKind === "rendition" && !generationID)) {
    malformed("generation authority is inconsistent");
  }
  const coverageRaw = record(raw.coverage, "coverage");
  keys(coverageRaw, ["configuration"], ["profile_fingerprint"], "coverage");
  const configuration = string(coverageRaw.configuration, "coverage.configuration");
  const profileFingerprint = optionalString(coverageRaw.profile_fingerprint, "coverage.profile_fingerprint");
  if (!["configured", "unconfigured", "profile_required"].includes(configuration)) malformed("coverage.configuration is unknown");
  if ((configuration === "configured" && (profileFingerprint === undefined || !sha256.test(profileFingerprint))) ||
      (configuration !== "configured" && profileFingerprint !== undefined)) malformed("coverage authority is inconsistent");
  const pageSize = integer(raw.page_size, "page_size", 1);
  if (pageSize !== 50 && pageSize !== 100 && pageSize !== 250) malformed("page_size is unsupported");
  const total = integer(raw.total, "total");
  const totalBytes = integer(raw.total_bytes, "total_bytes");
  if (total > maxSnapshotMembers) malformed("total exceeds 250,000 members");
  if (!Array.isArray(raw.rows) || raw.rows.length > pageSize || raw.rows.length > total) malformed("rows exceeds its page bounds");
  if (total === 0 ? raw.rows.length !== 0 : raw.rows.length === 0) malformed("rows is inconsistent with total");
  const rows = raw.rows.map((value, index) => parseRow(value, index));
  if (!Array.isArray(raw.facets) || raw.facets.length > 8) malformed("facets exceeds its bound");
  const facets = raw.facets.map((value, index) => parseFacet(value, index));
  if (new Set(facets.map((facet) => facet.dimension)).size !== facets.length) malformed("facets repeat a dimension");
  if (raw.snapshot !== true) malformed("snapshot authority is absent");
  const memberHash = string(raw.member_hash, "member_hash");
  const snapshotFingerprint = string(raw.snapshot_fingerprint, "snapshot_fingerprint");
  const snapshotID = string(raw.snapshot_id, "snapshot_id");
  if (!sha256.test(memberHash) || !prefixedSHA256.test(snapshotFingerprint) || !snapshotIdentity.test(snapshotID)) {
    malformed("snapshot identity is invalid");
  }
  const createdAt = dateTime(raw.created_at, "created_at");
  const expiresAt = dateTime(raw.expires_at, "expires_at");
  const lifetime = Date.parse(expiresAt) - Date.parse(createdAt);
  if (lifetime < 0 || lifetime > snapshotAbsoluteLifetimeMilliseconds) malformed("expiry exceeds the snapshot lifetime");
  return {
    query: decodedQuery,
    dependencies,
    query_fingerprint: queryFingerprint,
    member_hash: memberHash,
    snapshot_fingerprint: snapshotFingerprint,
    generation: generationKind === "native" ? { kind: "native" } : { kind: "rendition", generation_id: generationID! },
    coverage: configuration === "configured"
      ? { configuration, profile_fingerprint: profileFingerprint! }
      : { configuration: configuration as "unconfigured" | "profile_required" },
    observed_at: dateTime(raw.observed_at, "observed_at"),
    page_size: pageSize,
    total,
    total_bytes: totalBytes,
    rows,
    facets,
    snapshot: true,
    snapshot_id: snapshotID,
    created_at: createdAt,
    expires_at: expiresAt,
    ...(cursor(raw.previous_cursor, "previous_cursor") === undefined ? {} : { previous_cursor: cursor(raw.previous_cursor, "previous_cursor")! }),
    ...(cursor(raw.next_cursor, "next_cursor") === undefined ? {} : { next_cursor: cursor(raw.next_cursor, "next_cursor")! }),
  };
}

async function boundedJSON(response: Response): Promise<unknown> {
  const length = response.headers.get("Content-Length");
  if (length !== null && /^\d+$/.test(length) && Number(length) > maxResponseBytes) {
    throw new Error("Snapshot response exceeds 32 MiB.");
  }
  if (response.body === null) throw new Error("Malformed snapshot receipt: response body is missing.");
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.byteLength;
      if (total > maxResponseBytes) {
        await reader.cancel();
        throw new Error("Snapshot response exceeds 32 MiB.");
      }
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }
  const bytes = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  let text: string;
  try {
    text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
    return JSON.parse(text) as unknown;
  } catch {
    malformed("response is not valid UTF-8 JSON");
  }
}

function normalizedOptions(options: SnapshotOptions): SnapshotOptions {
  if (typeof options !== "object" || options === null || Array.isArray(options)) throw new Error("Snapshot options are invalid.");
  const profile = options.profile;
  if (profile !== undefined && (typeof profile !== "string" || !validUnicode(profile) || encoder.encode(profile).length > 128)) {
    throw new Error("Snapshot profile exceeds its bound.");
  }
  if (options.page_size !== undefined && ![50, 100, 250].includes(options.page_size)) throw new Error("Snapshot page size is invalid.");
  if (options.facets !== undefined && (!Array.isArray(options.facets) || options.facets.length > 8)) {
    throw new Error("Snapshot facets exceed their bound.");
  }
  const facets = options.facets?.map((value) => {
    if (typeof value !== "string" || !facetDimensions.has(value as WorkspaceQueryResponse["facets"][number]["dimension"])) {
      throw new Error("Snapshot facets contain an unknown dimension.");
    }
    return value;
  });
  if (facets !== undefined && new Set(facets).size !== facets.length) throw new Error("Snapshot facets repeat a dimension.");
  return {
    ...(profile === undefined ? {} : { profile }),
    ...(options.page_size === undefined ? {} : { page_size: options.page_size }),
    ...(facets === undefined ? {} : { facets }),
  };
}

function validateFirstPage(page: SnapshotPage, options: SnapshotOptions): void {
  if (page.previous_cursor !== undefined) malformed("first page has a previous cursor");
  const expectedRows = Math.min(page.page_size, page.total);
  if (page.rows.length !== expectedRows || (page.next_cursor !== undefined) !== (expectedRows < page.total)) {
    malformed("first page rows and cursor are inconsistent");
  }
  if (page.page_size !== (options.page_size ?? 100)) malformed("page_size does not match the request");
  const requestedFacets = options.facets ?? [];
  if (page.facets.length !== requestedFacets.length ||
      page.facets.some((facet, index) => facet.dimension !== requestedFacets[index])) {
    malformed("facets do not match the request");
  }
}

export async function createSnapshot(
  session: string, query: Query, options: SnapshotOptions, signal: AbortSignal,
): Promise<SnapshotPage> {
  const normalized = normalizedOptions(options);
  const canonical = canonicalQuery(query);
  const response = await requestResponse("/api/v1/workspace/queries", session, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ query: JSON.parse(canonical), ...normalized }),
    signal,
  });
  const result = await parseSnapshot(await boundedJSON(response), query);
  validateFirstPage(result, normalized);
  return result;
}

function sameAuthority(left: SnapshotPage, right: SnapshotPage): boolean {
  return canonicalQuery(left.query) === canonicalQuery(right.query) &&
    JSON.stringify(left.dependencies) === JSON.stringify(right.dependencies) &&
    left.query_fingerprint === right.query_fingerprint && left.member_hash === right.member_hash &&
    left.snapshot_fingerprint === right.snapshot_fingerprint &&
    JSON.stringify(left.generation) === JSON.stringify(right.generation) &&
    JSON.stringify(left.coverage) === JSON.stringify(right.coverage) && left.observed_at === right.observed_at &&
    left.page_size === right.page_size && left.total === right.total && left.total_bytes === right.total_bytes &&
    JSON.stringify(left.facets) === JSON.stringify(right.facets) && left.snapshot_id === right.snapshot_id &&
    left.created_at === right.created_at;
}

export async function readSnapshotPage(
  session: string, snapshot: SnapshotPage, requestedCursor: string, signal: AbortSignal,
): Promise<SnapshotPage> {
  if (typeof requestedCursor !== "string" || requestedCursor.length === 0 || encoder.encode(requestedCursor).length > maxCursorBytes) {
    throw new Error("Snapshot cursor is outside its bound.");
  }
  const forward = requestedCursor === snapshot.next_cursor;
  const backward = requestedCursor === snapshot.previous_cursor;
  if (forward === backward) throw new Error("Snapshot cursor does not belong to the supplied page.");
  const response = await requestResponse(
    `/api/v1/workspace/queries/${encodeURIComponent(snapshot.snapshot_id)}/pages`, session,
    {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ cursor: requestedCursor }), signal,
    },
  );
  const result = await parseSnapshot(await boundedJSON(response), snapshot.query);
  if (!sameAuthority(snapshot, result)) malformed("page does not match immutable snapshot authority");
  if (Date.parse(result.expires_at) < Date.parse(snapshot.expires_at)) malformed("page expiry moved backward");
  if ((forward && result.previous_cursor === undefined) || (backward && result.next_cursor === undefined)) {
    malformed("page cursor direction is inconsistent");
  }
  return result;
}

function mapRun(value: unknown): SavedQueryRunResult["run"] {
  const raw = record(value, "run");
  keys(raw, [
    "run_id", "saved_query_id", "saved_query_revision", "query_fingerprint", "snapshot_id", "member_hash",
    "total", "total_bytes", "ran_at", "expires_at", "comparison",
  ], ["previous_run_id", "previous_member_hash", "previous_total", "previous_query_fingerprint"], "run");
  const comparisonRaw = record(raw.comparison, "run.comparison");
  keys(comparisonRaw, ["hash_changed", "total_delta", "definition_changed"], [], "run.comparison");
  if (typeof comparisonRaw.hash_changed !== "boolean" || typeof comparisonRaw.definition_changed !== "boolean") {
    savedMalformed("comparison flags are invalid");
  }
  const runID = string(raw.run_id, "run.run_id");
  const savedQueryID = string(raw.saved_query_id, "run.saved_query_id");
  const queryFingerprint = string(raw.query_fingerprint, "run.query_fingerprint");
  const snapshotID = string(raw.snapshot_id, "run.snapshot_id");
  const memberHash = string(raw.member_hash, "run.member_hash");
  const previousRunID = optionalString(raw.previous_run_id, "run.previous_run_id");
  const previousMemberHash = optionalString(raw.previous_member_hash, "run.previous_member_hash");
  const previousQueryFingerprint = optionalString(raw.previous_query_fingerprint, "run.previous_query_fingerprint");
  const previousTotal = raw.previous_total === undefined || raw.previous_total === null
    ? raw.previous_total as null | undefined : integer(raw.previous_total, "run.previous_total");
  if (!uuidV4.test(runID) || !uuidV4.test(savedQueryID) || !prefixedSHA256.test(queryFingerprint) ||
      !snapshotIdentity.test(snapshotID) || !sha256.test(memberHash)) savedMalformed("run identity is invalid");
  const hasPrevious = previousRunID !== undefined;
  if (hasPrevious !== (previousMemberHash !== undefined) || hasPrevious !== (previousQueryFingerprint !== undefined) ||
      hasPrevious !== (previousTotal !== undefined && previousTotal !== null)) savedMalformed("previous run fields are inconsistent");
  if (hasPrevious && (!uuidV4.test(previousRunID!) || previousRunID === runID || !sha256.test(previousMemberHash!) ||
      !prefixedSHA256.test(previousQueryFingerprint!))) savedMalformed("previous run identity is invalid");
  const total = integer(raw.total, "run.total");
  const totalDelta = integer(raw.comparison && comparisonRaw.total_delta, "run.comparison.total_delta", Number.MIN_SAFE_INTEGER);
  if (hasPrevious && (totalDelta !== total - previousTotal! || comparisonRaw.hash_changed !== (memberHash !== previousMemberHash) ||
      comparisonRaw.definition_changed !== (queryFingerprint !== previousQueryFingerprint))) {
    savedMalformed("comparison is inconsistent");
  }
  if (!hasPrevious && (totalDelta !== 0 || comparisonRaw.hash_changed || comparisonRaw.definition_changed)) {
    savedMalformed("comparison invents a previous run");
  }
  return {
    run_id: runID,
    saved_query_id: savedQueryID,
    saved_query_revision: integer(raw.saved_query_revision, "run.saved_query_revision", 1),
    query_fingerprint: queryFingerprint,
    snapshot_id: snapshotID,
    member_hash: memberHash,
    total,
    total_bytes: integer(raw.total_bytes, "run.total_bytes"),
    ran_at: dateTime(raw.ran_at, "run.ran_at"),
    expires_at: dateTime(raw.expires_at, "run.expires_at"),
    ...(previousRunID === undefined ? {} : { previous_run_id: previousRunID }),
    ...(previousMemberHash === undefined ? {} : { previous_member_hash: previousMemberHash }),
    ...(previousTotal === undefined ? {} : { previous_total: previousTotal }),
    ...(previousQueryFingerprint === undefined ? {} : { previous_query_fingerprint: previousQueryFingerprint }),
    comparison: {
      hash_changed: comparisonRaw.hash_changed,
      total_delta: totalDelta,
      definition_changed: comparisonRaw.definition_changed,
    },
  };
}

export async function runSavedSnapshot(
  session: string, id: string, revision: number, query: Query, options: SnapshotOptions, signal: AbortSignal,
): Promise<SavedQueryRunResult> {
  if (!uuidV4.test(id) || !Number.isSafeInteger(revision) || revision < 1) throw new Error("Saved query identity or revision is invalid.");
  const normalized = normalizedOptions(options);
  const response = await requestResponse(`/api/v1/saved-queries/${encodeURIComponent(id)}/runs`, session, {
    method: "POST",
    headers: { "Content-Type": "application/json", "If-Match": String(revision) },
    body: JSON.stringify(normalized),
    signal,
  });
  const raw = record(await boundedJSON(response), "saved query run receipt");
  keys(raw, ["run", "snapshot"], ["$schema"], "saved query run receipt");
  if (raw.$schema !== undefined && string(raw.$schema, "saved query run receipt.$schema").length === 0) {
    savedMalformed("$schema is invalid");
  }
  const run = mapRun(raw.run);
  const snapshot = await parseSnapshot(raw.snapshot, query);
  validateFirstPage(snapshot, normalized);
  if (run.saved_query_id !== id || run.saved_query_revision !== revision ||
      run.query_fingerprint !== snapshot.query_fingerprint || run.snapshot_id !== snapshot.snapshot_id ||
      run.member_hash !== snapshot.member_hash || run.total !== snapshot.total || run.total_bytes !== snapshot.total_bytes ||
      run.ran_at !== snapshot.created_at || run.expires_at !== snapshot.expires_at) {
    savedMalformed("run and snapshot authority are inconsistent");
  }
  return { run, snapshot };
}

export async function snapshotMemberHash(
  members: readonly Pick<SnapshotMember, "node_id" | "content_version_id">[],
): Promise<string> {
  const ordered = members.map((member) => ({
    node_id: integer(member.node_id, "member.node_id", 1),
    content_version_id: string(member.content_version_id, "member.content_version_id"),
  })).sort((left, right) => left.node_id - right.node_id || left.content_version_id.localeCompare(right.content_version_id));
  const text = ordered.map((member) => `${member.node_id}:${member.content_version_id}\n`).join("");
  const digest = await crypto.subtle.digest("SHA-256", encoder.encode(text));
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("");
}

export async function captureSnapshotTargets(
  session: string, first: SnapshotPage, signal: AbortSignal,
): Promise<VerifiedSnapshotTargets> {
  if (first.previous_cursor !== undefined) throw new Error("Snapshot capture requires the accepted first page.");
  if (first.total > maxSnapshotMembers) throw new Error("Snapshot capture is limited to 250,000 members.");
  const members: SnapshotMember[] = [];
  const identities = new Set<string>();
  const cursors = new Set<string>();
  let page = first;
  let bytes = 0;
  for (;;) {
    const expectedRows = Math.min(page.page_size, first.total - members.length);
    if (page.rows.length !== expectedRows) throw new Error("Snapshot page row count is inconsistent.");
    for (const row of page.rows) {
      const identity = `${row.node_id}\0${row.content_version_id}`;
      if (identities.has(identity)) throw new Error("Snapshot capture contains a duplicate member.");
      identities.add(identity);
      members.push({
        node_id: row.node_id,
        content_version_id: row.content_version_id,
        blob_hash: row.blob_hash,
        size: row.size,
        revision: row.revision,
      });
      bytes += row.size;
      if (!Number.isSafeInteger(bytes)) throw new Error("Snapshot capture byte total is unsafe.");
    }
    if (members.length === first.total) {
      if (page.next_cursor !== undefined) throw new Error("Snapshot final page has a next cursor.");
      break;
    }
    if (page.next_cursor === undefined || cursors.has(page.next_cursor)) throw new Error("Snapshot page cursor is inconsistent.");
    cursors.add(page.next_cursor);
    page = await readSnapshotPage(session, page, page.next_cursor, signal);
  }
  if (bytes !== first.total_bytes) throw new Error("Snapshot capture byte total does not match its receipt.");
  if (await snapshotMemberHash(members) !== first.member_hash) throw new Error("Snapshot capture member hash does not match its receipt.");
  return { snapshot: first, members };
}
