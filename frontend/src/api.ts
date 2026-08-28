import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex, utf8ToBytes } from "@noble/hashes/utils.js";
import { parseDocument } from "yaml";

export interface Node {
  id: number;
  parent_id?: number;
  name: string;
  kind: "dir" | "file";
  current_version_id?: string;
  blob_hash?: string;
  size: number;
  mime_type?: string;
  revision: number;
  created_at: string;
  modified_at: string;
  trashed_at?: string;
  path?: string;
}

export interface NodePage {
  directory: Node;
  items: Node[];
  total: number;
  limit: number;
  offset: number;
}

export interface TrashPage {
  items: Node[];
  total: number;
  limit: number;
  offset: number;
}

export interface ContentVersion {
  id: string;
  node_id: number;
  blob_hash: string;
  size: number;
  mime_type?: string;
  recorded_at: string;
  node_revision: number;
  introduced_operation_id: string;
  transition_kind:
    | "content_create"
    | "content_replace"
    | "content_revert";
  source_version_id?: string;
}

export interface ContentVersionPage {
  items: ContentVersion[];
  total: number;
  limit: number;
  offset: number;
}

export interface ProvenanceFact {
  identity: string;
  node_id: number;
  ingest_id: string;
  ingest_started_at: string;
  source_kind: string;
  source_description: string;
  original_path: string;
  original_mtime?: string;
  supersedes?: string;
  active: boolean;
}

export interface ProvenancePage {
  node: Node;
  items: ProvenanceFact[];
  total: number;
  limit: number;
  offset: number;
}

export interface SearchHit {
  node: Node;
  path: string;
  match: "name" | "content";
}

export interface SearchReport {
  hits: SearchHit[];
  limit: number;
  truncated: boolean;
  tag_id?: string;
}

export interface ProcessingSelector {
  node_id: number;
  content_version_id: string;
  profile: string;
}

export interface ProcessingProfileSummary {
  name: string;
  fingerprint: string;
  rendition: boolean;
  embedding_bindings: string[];
}

export interface ProcessingFlowHop {
  capability: string;
  provider_id: string;
  trust_boundary: string;
  input_classes: string[];
}

export interface ProcessingPlan {
  fingerprint: string;
  vault_uid: string;
  selector: ProcessingSelector;
  profile_fingerprint: string;
  flow: ProcessingFlowHop[];
  disclosed_classes: string[];
  retained_classes: string[];
  estimate: { source_bytes: number; provider_calls: number; vector_spaces: number };
  consent_required: boolean;
  consent_state: "active" | "required" | "expired" | "revoked";
  backup_consequence: string;
}

export interface ProcessingJob {
  id: string;
  rendition_job_id?: string;
  attachment_id?: string;
  embedding_job_ids: string[];
  profile_fingerprint: string;
  content_version_id: string;
}

export interface ProcessingStatus {
  job_id: string;
  state: string;
  phase: string;
  failure_code?: string;
  embedding_job_ids: string[];
  completed_bindings: number;
}

export interface ProcessingRun {
  job: ProcessingJob;
  status: ProcessingStatus;
}

export interface CoverageClass {
  name: string;
  required: boolean;
  state: string;
  complete: number;
  unavailable: number;
  stale: number;
  ineligible: number;
  total: number;
}

export interface CoverageReport {
  vault_uid: string;
  profile_fingerprint: string;
  state: string;
  renditions: CoverageClass;
  embeddings: CoverageClass[];
}

export interface DocumentSearchRequest {
  query: string;
  mode: "auto" | "lexical" | "semantic" | "hybrid";
  limit: number;
  profile: string;
  binding_id?: string;
  fence: { vault_uid: string; content_version_ids: string[] };
  explain?: boolean;
}

export interface DocumentSearchResult {
  vault_uid: string;
  node_id: number;
  content_version_id: string;
  rank: number;
  score: number;
  path: string;
  excerpt?: string;
  evidence: Array<{ kind: string; build_id?: string; segment_id?: string; vector_space_id?: string }>;
}

export interface DocumentSearchReport {
  requested_mode: string;
  actual_mode: string;
  coverage: { binding_required: boolean; scoped_documents: number; complete_documents: number; state: string };
  degradations: string[];
  results: DocumentSearchResult[];
  truncated: boolean;
  trace: Array<{ code: string; count: number }>;
}

export interface RenditionArtifact {
  attachmentID: string;
  buildID: string;
  artifactID: string;
  contentVersionID: string;
  blobHash: string;
  profileFingerprint: string;
  frontmatter: string;
  markdown: string;
  completeness: string;
  warnings: string[];
  source: { sha256: string; format: string; mediaType: string };
  document: { title: string; language: string; unitKind: string; unitCount: number };
  navigation: {
    complete: boolean;
    entries: Array<{ key: string; kind: string; title: string; line: number; byte: number }>;
  };
}

export interface Tag {
  id: string;
  name: string;
  revision: number;
  assignment_count: number;
}

export interface TagPage {
  items: Tag[];
  total: number;
  limit: number;
  offset: number;
}

export interface TagAssignmentReceipt {
  tag: Tag;
  node: Node;
  changed: boolean;
}

export interface TagDeletionReceipt {
  tag: Tag;
  removed_assignments: number;
}

export interface TaggedNode {
  node: Node;
  path?: string;
}

export interface TaggedNodePage {
  items: TaggedNode[];
  total: number;
  limit: number;
  offset: number;
  omitted_trashed?: number;
}

export interface BackupRepository {
  id: string;
  path: string;
}

export interface BackupSnapshot {
  id: string;
  parent_id?: string;
  created_at: string;
  tag?: string;
  metadata_format: string;
  nodes: number;
  files: number;
  blobs: number;
  blob_bytes: number;
  packs_added: number;
  bytes_added: number;
  duration_seconds: number;
}

export interface BackupSnapshotList {
  repository: BackupRepository;
  items: BackupSnapshot[];
}

export interface UploadReceipt {
  status: "added" | "skipped";
  node: Node;
  computed_hash: string;
  computed_size: number;
}

export interface AuditScopeStatus {
  id: string;
  target_node_id: number;
  target_path?: string;
  target_trashed: boolean;
  enable_operation_id: string;
  baseline_digest: string;
  member_count: number;
  entry_count: number;
  chain_head: string;
}

export interface AuditMembershipStatus {
  node_id: number;
  path?: string;
  trashed: boolean;
  protected: boolean;
  scope_ids: string[];
  baseline_digests: string[];
}

export interface AuditStatus {
  enabled: boolean;
  enabled_scope_id?: string;
  vault_id: string;
  lineage_id?: string;
  operation_sequence_high_water: number;
  allocation_entry_count: number;
  allocation_head?: string;
  scopes: AuditScopeStatus[];
  membership?: AuditMembershipStatus;
}

export interface AuditScopeEvidence {
  id: string;
  entry_count: number;
  chain_head: string;
}

export interface AuditEvidence {
  vault_id: string;
  lineage_id: string;
  operation_sequence_high_water: number;
  allocation_entry_count: number;
  allocation_head: string;
  scopes: AuditScopeEvidence[];
}

export interface VerifyProblem {
  hash: string;
  problem: "missing" | "corrupt" | "unreadable";
}

export interface AuditVerifyReport {
  enabled: boolean;
  evidence?: AuditEvidence;
  protected_blobs: number;
  protected_bytes: number;
  verified_blobs: number;
  problems?: VerifyProblem[];
  metadata_problems?: string[];
}

export interface AuditPathState {
  path: string;
  state: "live" | "trash";
}

export interface AuditAttachmentIdentity {
  tag_id?: string;
  node_id?: number;
  provenance_id?: string;
}

export interface AuditAttachmentState {
  tag_id?: string;
  node_id?: number;
  tag_name?: string;
  provenance_id?: string;
  ingest_id?: string;
  original_path?: string | null;
  original_mtime?: string | null;
  supersedes?: string | null;
}

export interface AuditAttachmentChange {
  kind: "tag_definition" | "tag_assignment" | "provenance";
  identity: AuditAttachmentIdentity;
  before?: AuditAttachmentState;
  after?: AuditAttachmentState;
}

export interface AuditEvent {
  id: string;
  operation_id: string;
  operation_sequence: number;
  ordinal: number;
  node_id: number;
  kind: string;
  scope_id: string;
  recorded_at: string;
  origin: string;
  agent_label?: string;
  prior_node_revision: number;
  resulting_node_revision: number;
  prior_current_version_id?: string;
  resulting_current_version_id?: string;
  source_version_id?: string;
  target_node_id?: number;
  baseline_digest?: string;
  attachment?: AuditAttachmentChange;
  old_path?: AuditPathState;
  new_path?: AuditPathState;
}

export interface AuditEventPage {
  node: Node;
  path?: string;
  items: AuditEvent[];
  total: number;
  limit: number;
  cursor?: string;
  next_cursor?: string;
}

export interface Job {
  name: string;
  status: "queued" | "running" | "completed" | "failed" | "cancelled";
  started_at: string;
  finished_at?: string;
  error?: string;
}

export interface JobList {
  items: Job[];
}

export interface StorageStatus {
  loose_blobs: number;
  loose_bytes: number;
  packs: number;
  pack_stored_bytes: number;
  packed_blobs: number;
  packed_raw_bytes: number;
  packed_stored_bytes: number;
  dead_packed_bytes: number;
  stores: StorageStoreStatus[];
}

export interface StorageStoreStatus {
  id: string;
  name: string;
  kind: string;
  role: string;
  lifecycle: string;
  state: string;
  priority: number;
  authoritative_objects: number;
  logical_bytes: number;
  stored_bytes: number;
  pack_count: number;
  dead_packed_bytes: number;
  sole_authority_objects: number;
  affected_documents: number;
  unreadable_objects: number;
  observed_at?: string;
}

export interface Problem {
  status?: number;
  code?: string;
  detail?: string;
  title?: string;
}

export class APIError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code: string,
  ) {
    super(message);
    this.name = "APIError";
  }
}

export interface BrowserSession {
  token: string;
  uploadSecret: string;
}

export function takeFragmentSession(
  location: Location = window.location,
  history: History = window.history,
): BrowserSession | null {
  const params = new URLSearchParams(location.hash.replace(/^#/, ""));
  const token = params.get("web_session") ?? "";
  const uploadSecret = params.get("web_upload_secret") ?? "";
  if (token || uploadSecret) {
    history.replaceState(null, "", `${location.pathname}${location.search}`);
  }
  return token && uploadSecret ? { token, uploadSecret } : null;
}

async function decodeProblem(response: Response): Promise<Problem> {
  try {
    return (await response.json()) as Problem;
  } catch {
    return {};
  }
}

export async function requestResponse(
  path: string,
  session: string,
  init: RequestInit = {},
): Promise<Response> {
  const headers = new Headers(init.headers);
  if (!headers.has("Accept")) headers.set("Accept", "application/json");
  headers.set("X-Docbank-Web-Session", session);
  const response = await fetch(path, {
    ...init,
    headers,
    credentials: "same-origin",
  });
  if (!response.ok) {
    const problem = await decodeProblem(response);
    const detail = problem.detail || problem.title || `HTTP ${response.status}`;
    throw new APIError(detail, response.status, problem.code ?? "");
  }
  return response;
}

export async function requestJSON<T>(
  path: string,
  session: string,
  init: RequestInit = {},
): Promise<T> {
  const response = await requestResponse(path, session, init);
  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

export async function revokeSession(session: string): Promise<void> {
  if (!session) return;
  await requestJSON<void>("/api/daemon/web-session", session, { method: "DELETE" });
}

export async function statPath(session: string, path: string): Promise<Node> {
  return requestJSON<Node>(`/api/v1/path?path=${encodeURIComponent(path)}`, session);
}

export async function children(session: string, nodeID: number): Promise<NodePage> {
  return requestJSON<NodePage>(
    `/api/v1/nodes/${nodeID}/children?limit=1000&offset=0`,
    session,
  );
}

export async function contentVersions(
  session: string,
  nodeID: number,
): Promise<ContentVersionPage> {
  return requestJSON<ContentVersionPage>(
    `/api/v1/nodes/${nodeID}/versions?limit=1000&offset=0`,
    session,
  );
}

export async function provenance(
  session: string,
  nodeID: number,
): Promise<ProvenancePage> {
  return requestJSON<ProvenancePage>(
    `/api/v1/nodes/${nodeID}/provenance?limit=1000&offset=0`,
    session,
  );
}

export async function search(
  session: string,
  query: string,
  tagID = "",
): Promise<SearchReport> {
  const params = new URLSearchParams({ q: query, limit: "1000" });
  if (tagID) params.set("tag_id", tagID);
  return requestJSON<SearchReport>(
    `/api/v1/search?${params.toString()}`,
    session,
  );
}

export async function processingProfiles(session: string): Promise<ProcessingProfileSummary[]> {
  return requestJSON<ProcessingProfileSummary[]>("/api/v1/processing/profiles", session);
}

export async function processingPlan(
  session: string,
  selector: ProcessingSelector,
): Promise<ProcessingPlan> {
  return requestJSON<ProcessingPlan>("/api/v1/processing/plans", session, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ selector }),
  });
}

export async function startProcessing(
  session: string,
  selector: ProcessingSelector,
  planFingerprint: string,
  consent: boolean,
): Promise<ProcessingRun> {
  const response = await requestResponse("/api/v1/processing/jobs", session, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/x-ndjson" },
    body: JSON.stringify({ selector, plan_fingerprint: planFingerprint, consent }),
  });
  if (!(response.headers.get("Content-Type") ?? "").startsWith("application/x-ndjson")) {
    throw new Error("The daemon returned an invalid processing stream.");
  }
  const lines = (await response.text()).trimEnd().split("\n");
  if (lines.length !== 2) throw new Error("The processing stream did not end after its terminal status.");
  const first = JSON.parse(lines[0] ?? "null") as { sequence?: number; type?: string; job?: ProcessingJob };
  const second = JSON.parse(lines[1] ?? "null") as { sequence?: number; type?: string; status?: ProcessingStatus; terminal?: boolean };
  if (first.sequence !== 1 || first.type !== "job" || !first.job || second.sequence !== 2 ||
      second.type !== "status" || !second.status || !second.terminal || second.status.job_id !== first.job.id) {
    throw new Error("The daemon returned malformed processing progress.");
  }
  return { job: first.job, status: second.status };
}

export async function documentCoverage(
  session: string,
  profile: string,
  vaultUID: string,
  contentVersionIDs: string[],
): Promise<CoverageReport> {
  const params = new URLSearchParams({ profile, vault_uid: vaultUID });
  for (const id of contentVersionIDs) params.append("content_version_id", id);
  return requestJSON<CoverageReport>(`/api/v1/coverage?${params.toString()}`, session);
}

export async function documentSearch(
  session: string,
  request: DocumentSearchRequest,
): Promise<DocumentSearchReport> {
  return requestJSON<DocumentSearchReport>("/api/v1/search", session, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(request),
  });
}

export async function renditionArtifact(session: string, attachmentID: string): Promise<RenditionArtifact> {
  const response = await requestResponse(`/api/v1/renditions/${encodeURIComponent(attachmentID)}`, session, {
    headers: { Accept: "text/markdown" },
  });
  if (!(response.headers.get("Content-Type") ?? "").toLowerCase().startsWith("text/markdown")) {
    throw new Error("The daemon returned an invalid rendition content type.");
  }
  const receivedAttachment = response.headers.get("X-Docbank-Rendition-Attachment") ?? "";
  const buildID = response.headers.get("X-Docbank-Rendition-Build") ?? "";
  const artifactID = response.headers.get("X-Docbank-Rendition-Artifact") ?? "";
  const contentVersionID = response.headers.get("X-Docbank-Content-Version") ?? "";
  const blobHash = response.headers.get("X-Docbank-Blob-Hash") ?? "";
  const profileFingerprint = response.headers.get("X-Docbank-Rendition-Profile") ?? "";
  const transportCompleteness = response.headers.get("X-Docbank-Rendition-Completeness") ?? "";
  const warnings = parseRenditionWarnings(response.headers.get("X-Docbank-Rendition-Warnings") ?? "");
  const declaredSize = Number(response.headers.get("X-Docbank-Blob-Size"));
  if (receivedAttachment !== attachmentID || !/^[0-9a-f]{64}$/.test(buildID) ||
      !/^[0-9a-f]{64}$/.test(artifactID) || !/^[0-9a-f]{64}$/.test(blobHash) ||
      !/^[0-9a-f]{64}$/.test(profileFingerprint) ||
      !/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(contentVersionID) ||
      !["complete", "partial", "degraded_provenance"].includes(transportCompleteness)) {
    throw new Error("The daemon returned invalid rendition identities.");
  }
  const bytes = new Uint8Array(await response.arrayBuffer());
  if (!Number.isSafeInteger(declaredSize) || declaredSize < 1 || declaredSize !== bytes.length || bytes.length > 64 * 1024 * 1024) {
    throw new Error("The daemon returned an invalid rendition size.");
  }
  if (bytesToHex(sha256(bytes)) !== blobHash) throw new Error("The rendition transport checksum does not match.");
  const artifact = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  if (!artifact.startsWith("---\n")) throw new Error("The rendition is missing canonical frontmatter.");
  const closing = artifact.indexOf("\n---\n", 4);
  if (closing < 0 || closing + 5 > 256 * 1024) throw new Error("The rendition frontmatter is incomplete or too large.");
  const frontmatter = artifact.slice(4, closing);
  const markdown = artifact.slice(closing + 5);
  if (!markdown) throw new Error("The rendition Markdown body is empty.");
  const metadata = parseRenditionFrontmatter(frontmatter, markdown);
  const bodyHash = metadata.bodyHash;
  if (bytesToHex(sha256(utf8ToBytes(markdown))) !== bodyHash) {
    throw new Error("The rendition body checksum does not match.");
  }
  if (metadata.buildID !== buildID || metadata.completeness !== transportCompleteness) {
    throw new Error("The rendition transport metadata does not match its frontmatter.");
  }
  return { attachmentID, buildID, artifactID, contentVersionID, blobHash, profileFingerprint,
    frontmatter, markdown, completeness: metadata.completeness, warnings, source: metadata.source,
    document: metadata.document, navigation: metadata.navigation };
}

type UnknownRecord = Record<string, unknown>;

function parseRenditionWarnings(value: string): string[] {
  if (!value) return [];
  const warnings = value.split(",");
  if (warnings.length > 64 || new Set(warnings).size !== warnings.length ||
      warnings.some((warning) => !/^[a-z0-9_.-]{1,63}$/.test(warning))) {
    throw new Error("The daemon returned invalid rendition warnings.");
  }
  return warnings;
}

function record(value: unknown, subject: string): UnknownRecord {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(`The rendition ${subject} is invalid.`);
  return value as UnknownRecord;
}

function exactKeys(value: UnknownRecord, required: string[], optional: string[] = []): void {
  const allowed = new Set([...required, ...optional]);
  if (required.some((key) => !(key in value)) || Object.keys(value).some((key) => !allowed.has(key))) {
    throw new Error("The rendition frontmatter contains an invalid field set.");
  }
}

function textField(value: UnknownRecord, key: string, optional = false): string {
  const field = value[key];
  if (optional && field === undefined) return "";
  if (typeof field !== "string" || !field || field.length > 1024) throw new Error(`The rendition ${key} is invalid.`);
  return field;
}

function integerField(value: UnknownRecord, key: string, minimum: number): number {
  const field = value[key];
  if (!Number.isSafeInteger(field) || Number(field) < minimum) throw new Error(`The rendition ${key} is invalid.`);
  return Number(field);
}

function booleanField(value: UnknownRecord, key: string): boolean {
  const field = value[key];
  if (typeof field !== "boolean") throw new Error(`The rendition ${key} is invalid.`);
  return field;
}

function digestField(value: UnknownRecord, key: string): string {
  const field = textField(value, key);
  if (!/^[0-9a-f]{64}$/.test(field)) throw new Error(`The rendition ${key} is invalid.`);
  return field;
}

function parseRenditionFrontmatter(frontmatter: string, markdown: string): {
  buildID: string;
  bodyHash: string;
  completeness: string;
  source: RenditionArtifact["source"];
  document: RenditionArtifact["document"];
  navigation: RenditionArtifact["navigation"];
} {
  const parsed = parseDocument(frontmatter, { schema: "core", strict: true, uniqueKeys: true });
  if (parsed.errors.length > 0 || parsed.warnings.length > 0) throw new Error("The rendition frontmatter is invalid YAML.");
  const root = record(parsed.toJS({ maxAliasCount: 0 }), "frontmatter");
  exactKeys(root, ["docbank"]);
  const docbank = record(root.docbank, "docbank envelope");
  exactKeys(docbank, ["contract", "source", "rendition", "document", "navigation"]);
  if (textField(docbank, "contract") !== "docbank-sanitized-markdown/v1") throw new Error("The rendition contract is unsupported.");

  const sourceValue = record(docbank.source, "source identity");
  exactKeys(sourceValue, ["sha256", "format", "media_type"]);
  const source = { sha256: digestField(sourceValue, "sha256"), format: textField(sourceValue, "format"), mediaType: textField(sourceValue, "media_type") };

  const rendition = record(docbank.rendition, "build identity");
  exactKeys(rendition, ["build_id", "rendition_request_fingerprint", "evidence_lexical_fingerprint", "normalized_evidence_contract", "body_sha256", "completeness", "truncated"]);
  const buildID = digestField(rendition, "build_id");
  digestField(rendition, "rendition_request_fingerprint");
  digestField(rendition, "evidence_lexical_fingerprint");
  if (textField(rendition, "normalized_evidence_contract") !== "normalized-evidence/v1") throw new Error("The rendition evidence contract is unsupported.");
  const bodyHash = digestField(rendition, "body_sha256");
  const completeness = textField(rendition, "completeness");
  if (!["complete", "partial", "degraded_provenance"].includes(completeness)) throw new Error("The rendition completeness is invalid.");
  booleanField(rendition, "truncated");

  const documentValue = record(docbank.document, "document identity");
  exactKeys(documentValue, ["unit_kind", "unit_count"], ["title", "language"]);
  const unitKind = textField(documentValue, "unit_kind");
  if (!["generic", "line", "message", "page", "record", "section", "sheet", "slide", "spine"].includes(unitKind)) throw new Error("The rendition unit kind is invalid.");
  const document = { title: textField(documentValue, "title", true), language: textField(documentValue, "language", true), unitKind, unitCount: integerField(documentValue, "unit_count", 1) };

  const navigationValue = record(docbank.navigation, "navigation");
  exactKeys(navigationValue, ["offset_base", "complete", "entries"]);
  if (textField(navigationValue, "offset_base") !== "body") throw new Error("The rendition navigation offset base is invalid.");
  const rawEntries = navigationValue.entries;
  if (!Array.isArray(rawEntries) || rawEntries.length > 1024 || rawEntries.length > document.unitCount) throw new Error("The rendition navigation entries are invalid.");
  const bodyBytes = utf8ToBytes(markdown);
  const seen = new Set<string>();
  let priorByte = -1;
  const entries = rawEntries.map((rawEntry) => {
    const entry = record(rawEntry, "navigation entry");
    exactKeys(entry, ["key", "kind", "line", "byte"], ["title"]);
    const key = textField(entry, "key");
    const kind = textField(entry, "kind");
    const title = textField(entry, "title", true);
    const line = integerField(entry, "line", 1);
    const byte = integerField(entry, "byte", 0);
    if (!["generic", "line", "message", "page", "record", "section", "sheet", "slide", "spine"].includes(kind) ||
        seen.has(key) || byte < priorByte || byte >= bodyBytes.length || (bodyBytes[byte]! & 0xc0) === 0x80 ||
        line !== 1 + bodyBytes.slice(0, byte).filter((value) => value === 0x0a).length) {
      throw new Error("The rendition navigation entry is invalid.");
    }
    seen.add(key);
    priorByte = byte;
    return { key, kind, title, line, byte };
  });
  return { buildID, bodyHash, completeness, source, document,
    navigation: { complete: booleanField(navigationValue, "complete"), entries } };
}

export async function tags(session: string): Promise<TagPage> {
  return requestJSON<TagPage>("/api/v1/tags?limit=1000&offset=0", session);
}

export async function tagByID(session: string, tagID: string): Promise<Tag> {
  return requestJSON<Tag>(`/api/v1/tags/${encodeURIComponent(tagID)}`, session);
}

export async function createTag(session: string, name: string): Promise<Tag> {
  const normalized = name.normalize("NFC");
  const tag = await requestJSON<Tag>("/api/v1/tags", session, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name }),
  });
  if (
    !tag.id ||
    tag.name !== normalized ||
    tag.revision !== 1 ||
    tag.assignment_count !== 0
  ) {
    throw new Error("The daemon returned an invalid created-tag receipt.");
  }
  return tag;
}

export async function renameTag(
  session: string,
  tagID: string,
  revision: number,
  name: string,
): Promise<Tag> {
  const normalized = name.normalize("NFC");
  const tag = await requestJSON<Tag>(
    `/api/v1/tags/${encodeURIComponent(tagID)}`,
    session,
    {
      method: "PATCH",
      headers: {
        "Content-Type": "application/json",
        "If-Match": String(revision),
      },
      body: JSON.stringify({ name }),
    },
  );
  if (tag.id !== tagID || tag.name !== normalized || tag.revision < revision) {
    throw new Error("The daemon returned an invalid renamed-tag receipt.");
  }
  return tag;
}

export async function deleteTag(
  session: string,
  tagID: string,
  revision: number,
): Promise<TagDeletionReceipt> {
  const receipt = await requestJSON<TagDeletionReceipt>(
    `/api/v1/tags/${encodeURIComponent(tagID)}`,
    session,
    {
      method: "DELETE",
      headers: { "If-Match": String(revision) },
    },
  );
  if (
    receipt.tag.id !== tagID ||
    receipt.tag.revision !== revision ||
    receipt.removed_assignments !== receipt.tag.assignment_count
  ) {
    throw new Error("The daemon returned an invalid deleted-tag receipt.");
  }
  return receipt;
}

export async function taggedNodes(
  session: string,
  tagID: string,
  offset = 0,
): Promise<TaggedNodePage> {
  return requestJSON<TaggedNodePage>(
    `/api/v1/tags/${encodeURIComponent(tagID)}/nodes?limit=1000&offset=${offset}`,
    session,
  );
}

export async function liveTaggedNodes(
  session: string,
  tagID: string,
): Promise<TaggedNodePage> {
  return requestJSON<TaggedNodePage>(
    `/api/v1/tags/${encodeURIComponent(tagID)}/nodes?limit=1000&offset=0&live_only=true`,
    session,
  );
}

export async function nodeTags(session: string, nodeID: number): Promise<TagPage> {
  return requestJSON<TagPage>(
    `/api/v1/nodes/${nodeID}/tags?limit=1000&offset=0`,
    session,
  );
}

export async function changeNodeTag(
  session: string,
  nodeID: number,
  revision: number,
  tagID: string,
  assign: boolean,
): Promise<TagAssignmentReceipt> {
  const receipt = await requestJSON<TagAssignmentReceipt>(
    `/api/v1/nodes/${nodeID}/tags/${encodeURIComponent(tagID)}`,
    session,
    {
      method: assign ? "PUT" : "DELETE",
      headers: { "If-Match": String(revision) },
    },
  );
  if (
    receipt.node.id !== nodeID ||
    receipt.node.revision < revision ||
    receipt.node.trashed_at ||
    !receipt.node.path?.startsWith("/") ||
    receipt.tag.id !== tagID
  ) {
    throw new Error("The daemon returned an invalid tag-assignment receipt.");
  }
  return receipt;
}

export async function trashNode(
  session: string,
  nodeID: number,
  revision: number,
): Promise<Node> {
  const node = await requestJSON<Node>(
    `/api/v1/nodes/${nodeID}/trash`,
    session,
    {
      method: "POST",
      headers: { "If-Match": String(revision) },
    },
  );
  if (
    node.id !== nodeID ||
    node.revision <= revision ||
    !node.trashed_at ||
    !node.path?.startsWith("/")
  ) {
    throw new Error("The daemon returned an invalid trash receipt.");
  }
  return node;
}

export async function trashRoots(session: string): Promise<TrashPage> {
  return requestJSON<TrashPage>("/api/v1/trash?limit=1000&offset=0", session);
}

export async function restoreNode(
  session: string,
  nodeID: number,
  revision: number,
): Promise<Node> {
  const node = await requestJSON<Node>(
    `/api/v1/nodes/${nodeID}/restore`,
    session,
    {
      method: "POST",
      headers: { "If-Match": String(revision) },
    },
  );
  if (
    node.id !== nodeID ||
    node.revision <= revision ||
    node.trashed_at ||
    !node.path?.startsWith("/")
  ) {
    throw new Error("The daemon returned an invalid restore receipt.");
  }
  return node;
}

export async function auditStatusForNode(
  session: string,
  nodeID: number,
): Promise<AuditStatus> {
  return requestJSON<AuditStatus>(
    `/api/v1/audit/status?node_id=${encodeURIComponent(nodeID)}`,
    session,
  );
}

export async function auditHistory(
  session: string,
  nodeID: number,
  cursor = "",
): Promise<AuditEventPage> {
  const query = new URLSearchParams({
    node_id: String(nodeID),
    limit: "50",
  });
  if (cursor) query.set("cursor", cursor);
  return requestJSON<AuditEventPage>(
    `/api/v1/audit/history?${query.toString()}`,
    session,
  );
}

export async function verifyAudit(
  session: string,
  signal?: AbortSignal,
): Promise<AuditVerifyReport> {
  return requestJSON<AuditVerifyReport>("/api/v1/audit/verify", session, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: "{}",
    signal,
  });
}

export async function listJobs(session: string): Promise<Job[]> {
  const result = await requestJSON<JobList>("/api/v1/jobs", session);
  return result.items;
}

export async function storageStatus(
  session: string,
  refresh = false,
): Promise<StorageStatus> {
  const suffix = refresh ? "?refresh=true" : "";
  return requestJSON<StorageStatus>(`/api/v1/storage${suffix}`, session);
}

export async function backupSnapshots(
  session: string,
): Promise<BackupSnapshotList> {
  return requestJSON<BackupSnapshotList>("/api/v1/backup/snapshots", session);
}
