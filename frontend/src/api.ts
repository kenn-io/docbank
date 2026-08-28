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
	runtime_disclosure: ProcessingRuntimeDisclosure;
}

export interface ProcessingRuntimeDisclosure {
	immediate_processor: string;
	ultimate_processor: string;
	endpoint: string;
	deployment: string;
	model?: string;
	model_revision?: string;
	vector_space?: string;
	metadata_classes: string[];
	retained_artifact_roles: string[];
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

export type ProcessingState =
  | "queued"
  | "running"
  | "retry_wait"
  | "operator_required"
  | "completed"
  | "failed";

export interface ProcessingStatus {
  job_id: string;
  state: ProcessingState;
  phase: string;
  failure_code?: string;
  embedding_job_ids: string[];
  completed_bindings: number;
}

export interface ProcessingRun {
  job: ProcessingJob;
  status: ProcessingStatus;
}

export interface ProcessingJobEvent {
  sequence: number;
  type: "job" | "status";
  job?: ProcessingJob;
  status?: ProcessingStatus;
  terminal?: boolean;
}

export interface CoverageClass {
  name: string;
  required: boolean;
  state: string;
  complete: number;
  unavailable: number;
  stale: number;
  ineligible: number;
  rebuilding: number;
  previous_generation_serving: number;
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
  lexical_rank?: number;
  semantic_rank?: number;
  evidence: DocumentEvidenceReference[];
}

export interface DocumentEvidenceReference {
  kind: string;
  build_id?: string;
  segment_id?: string;
  vector_space_id?: string;
  embedding_set_id?: string;
  input_generation_id?: string;
  input_id?: string;
  input_kind?: string;
  source_manifest_checksum?: string;
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
  onProgress?: (event: ProcessingJobEvent) => void,
  signal?: AbortSignal,
): Promise<ProcessingRun> {
  const response = await requestResponse("/api/v1/processing/jobs", session, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/x-ndjson" },
    body: JSON.stringify({ selector, plan_fingerprint: planFingerprint, consent }),
    signal,
  });
  if (!(response.headers.get("Content-Type") ?? "").startsWith("application/x-ndjson")) {
    throw new Error("The daemon returned an invalid processing stream.");
  }
  if (!response.body) throw new Error("The daemon returned an invalid processing stream.");
  return readProcessingRun(response.body, onProgress);
}

const maxProcessingProgressBytes = 64 * 1024;

async function readProcessingRun(
  body: ReadableStream<Uint8Array>,
  onProgress?: (event: ProcessingJobEvent) => void,
): Promise<ProcessingRun> {
  const reader = body.getReader();
  const decoder = new TextDecoder("utf-8", { fatal: true });
  const events: ProcessingJobEvent[] = [];
  let buffered = "";
  let received = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (value) {
        received += value.byteLength;
        if (received > maxProcessingProgressBytes) {
          throw new Error("The processing progress stream is too large.");
        }
        buffered += decoder.decode(value, { stream: true });
        buffered = consumeProcessingLines(buffered, events, onProgress);
      }
      if (!done) continue;
      buffered += decoder.decode();
      if (buffered.length > 0) {
        acceptProcessingLine(buffered, events, onProgress);
        buffered = "";
      }
      if (events.length !== 2) {
        throw new Error("The processing stream did not end after its terminal status.");
      }
      const first = events[0];
      const second = events[1];
      if (!first?.job || !second?.status) {
        throw new Error("The daemon returned malformed processing progress.");
      }
      onProgress?.(second);
      return { job: first.job, status: second.status };
    }
  } catch (cause) {
    await reader.cancel(cause).catch(() => undefined);
    throw cause;
  } finally {
    reader.releaseLock();
  }
}

function consumeProcessingLines(
  value: string,
  events: ProcessingJobEvent[],
  onProgress?: (event: ProcessingJobEvent) => void,
): string {
  let newline = value.indexOf("\n");
  while (newline >= 0) {
    const raw = value.slice(0, newline).replace(/\r$/, "");
    value = value.slice(newline + 1);
    if (raw.length === 0) throw new Error("The daemon returned malformed processing progress.");
    acceptProcessingLine(raw, events, onProgress);
    newline = value.indexOf("\n");
  }
  return value;
}

function acceptProcessingLine(
  value: string,
  events: ProcessingJobEvent[],
  onProgress?: (event: ProcessingJobEvent) => void,
): void {
  if (events.length >= 2) {
    throw new Error("The processing stream did not end after its terminal status.");
  }
  let decoded: unknown;
  try {
    decoded = JSON.parse(value);
  } catch {
    throw new Error("The daemon returned malformed processing progress.");
  }
  const event = validateProcessingEvent(decoded, events[0]?.job?.id);
  events.push(event);
  if (events.length === 1) onProgress?.(event);
}

function validateProcessingEvent(value: unknown, jobID?: string): ProcessingJobEvent {
  if (!isRecord(value)) throw new Error("The daemon returned malformed processing progress.");
  if (!jobID) {
    if (value.sequence !== 1 || value.type !== "job" || !isProcessingJob(value.job) ||
        "status" in value || "terminal" in value) {
      throw new Error("The daemon returned malformed processing progress.");
    }
    return value as unknown as ProcessingJobEvent;
  }
  if (value.sequence !== 2 || value.type !== "status" || "job" in value ||
      !isProcessingStatus(value.status) || value.terminal !== true || value.status.job_id !== jobID) {
    throw new Error("The daemon returned malformed processing progress.");
  }
  return value as unknown as ProcessingJobEvent;
}

function isProcessingJob(value: unknown): value is ProcessingJob {
  if (!isRecord(value)) return false;
  return canonicalHash(value.id) && optionalCanonicalHash(value.rendition_job_id) &&
    optionalCanonicalHash(value.attachment_id) && stringArray(value.embedding_job_ids) &&
    canonicalHash(value.profile_fingerprint) && typeof value.content_version_id === "string";
}

function isProcessingStatus(value: unknown): value is ProcessingStatus {
  if (!isRecord(value)) return false;
  return canonicalHash(value.job_id) && processingState(value.state) &&
    typeof value.phase === "string" && value.phase.length > 0 &&
    (value.failure_code === undefined || typeof value.failure_code === "string") &&
    stringArray(value.embedding_job_ids) && Number.isInteger(value.completed_bindings) &&
    Number(value.completed_bindings) >= 0;
}

function processingState(value: unknown): value is ProcessingState {
  return typeof value === "string" &&
    ["queued", "running", "retry_wait", "operator_required", "completed", "failed"].includes(value);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function canonicalHash(value: unknown): value is string {
  return typeof value === "string" && /^[0-9a-f]{64}$/.test(value);
}

function optionalCanonicalHash(value: unknown): boolean {
  return value === undefined || canonicalHash(value);
}

function stringArray(value: unknown): value is string[] {
  return Array.isArray(value) && value.every((item) => typeof item === "string");
}

export async function documentCoverage(
  session: string,
  profile: string,
  vaultUID: string,
  contentVersionIDs: string[],
  signal?: AbortSignal,
): Promise<CoverageReport> {
  const params = new URLSearchParams({ profile, vault_uid: vaultUID });
  for (const id of contentVersionIDs) params.append("content_version_id", id);
  return requestJSON<CoverageReport>(`/api/v1/coverage?${params.toString()}`, session, { signal });
}

export async function documentSearch(
  session: string,
  request: DocumentSearchRequest,
): Promise<DocumentSearchReport> {
  const response = await requestJSON<unknown>("/api/v1/search", session, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(request),
  });
  return validateDocumentSearchReport(response, request);
}

function validateDocumentSearchReport(value: unknown, request: DocumentSearchRequest): DocumentSearchReport {
  const invalid = (): never => { throw new Error("The daemon returned an invalid search response."); };
  if (!canonicalUUID(request.fence.vault_uid) || request.fence.content_version_ids.length < 1 ||
      request.fence.content_version_ids.length > 4096) invalid();
  const versions = new Set<string>();
  for (const versionID of request.fence.content_version_ids) {
    if (!canonicalUUID(versionID) || versions.has(versionID)) invalid();
    versions.add(versionID);
  }
  if (!isRecord(value)) invalid();
  const report = value as UnknownRecord;
  const requestedMode = request.mode || "auto";
  const actualMode = report.actual_mode;
  if (report.requested_mode !== requestedMode || typeof actualMode !== "string" ||
      !["lexical", "semantic", "hybrid"].includes(actualMode) ||
      (requestedMode !== "auto" && requestedMode !== actualMode)) invalid();
  const limit = request.limit || 20;
  if (!Number.isSafeInteger(limit) || limit < 1 || limit > 100 || !Array.isArray(report.results) ||
      report.results.length > limit) invalid();

  const coverage = report.coverage;
  if (!isRecord(coverage) || typeof coverage.binding_required !== "boolean" ||
      !nonnegativeInteger(coverage.scoped_documents) || Number(coverage.scoped_documents) > versions.size ||
      !nonnegativeInteger(coverage.complete_documents) ||
      Number(coverage.complete_documents) > Number(coverage.scoped_documents) ||
      typeof coverage.state !== "string" || !["unknown", "complete", "incomplete"].includes(coverage.state) ||
      (coverage.state === "complete" && coverage.complete_documents !== coverage.scoped_documents)) invalid();
  if (!Array.isArray(report.degradations) || report.degradations.some((item: unknown) => !boundedSearchIdentity(item, 128)) ||
      typeof report.truncated !== "boolean" || !Array.isArray(report.trace) ||
      (!request.explain && report.trace.length !== 0)) invalid();
  const trace = report.trace as unknown[];
  for (const rawTrace of trace) {
    if (!isRecord(rawTrace) || !boundedSearchIdentity(rawTrace.code, 128) ||
        !nonnegativeInteger(rawTrace.count)) invalid();
  }

  const documents = new Set<string>();
  const lexicalRanks = new Set<number>();
  const semanticRanks = new Set<number>();
  const results = report.results as unknown[];
  for (let index = 0; index < results.length; index += 1) {
    const result = results[index];
    if (!isRecord(result) || result.vault_uid !== request.fence.vault_uid ||
        !canonicalUUID(result.vault_uid) || typeof result.content_version_id !== "string" ||
        !versions.has(result.content_version_id) || !canonicalUUID(result.content_version_id) ||
        !positiveInteger(result.node_id) || result.rank !== index + 1 ||
        typeof result.score !== "number" || !Number.isFinite(result.score) ||
        !boundedDocumentSearchPath(result.path) ||
        (result.excerpt !== undefined && !boundedDocumentSearchExcerpt(result.excerpt))) invalid();
    const item = result as UnknownRecord;
    const documentKey = String(item.content_version_id);
    if (documents.has(documentKey)) invalid();
    documents.add(documentKey);
    const rawLexicalRank = item.lexical_rank === undefined ? 0 : item.lexical_rank;
    const rawSemanticRank = item.semantic_rank === undefined ? 0 : item.semantic_rank;
    if (!boundedLaneRank(rawLexicalRank) || !boundedLaneRank(rawSemanticRank)) invalid();
    const lexicalRank = Number(rawLexicalRank);
    const semanticRank = Number(rawSemanticRank);
    if (
        (actualMode === "lexical" && (lexicalRank === 0 || semanticRank !== 0)) ||
        (actualMode === "semantic" && (semanticRank === 0 || lexicalRank !== 0)) ||
        (actualMode === "hybrid" && lexicalRank === 0 && semanticRank === 0) ||
        (lexicalRank > 0 && lexicalRanks.has(lexicalRank)) ||
        (semanticRank > 0 && semanticRanks.has(semanticRank))) invalid();
    if (lexicalRank > 0) lexicalRanks.add(lexicalRank);
    if (semanticRank > 0) semanticRanks.add(semanticRank);
    if (!Array.isArray(item.evidence) || item.evidence.length < 1 || item.evidence.length > 32) invalid();
    const evidenceItems = item.evidence as unknown[];
    const evidenceIdentities = new Set<string>();
    for (const evidence of evidenceItems) {
      if (!isRecord(evidence) || !validateDocumentEvidenceIdentity(evidence)) invalid();
      const evidenceItem = evidence as UnknownRecord;
      const identity = JSON.stringify([evidenceItem.kind, evidenceItem.build_id ?? "", evidenceItem.segment_id ?? "",
        evidenceItem.vector_space_id ?? "", evidenceItem.embedding_set_id ?? "", evidenceItem.input_generation_id ?? "",
        evidenceItem.input_id ?? "", evidenceItem.input_kind ?? "", evidenceItem.source_manifest_checksum ?? ""]);
      if (evidenceIdentities.has(identity)) invalid();
      evidenceIdentities.add(identity);
    }
  }
  return value as unknown as DocumentSearchReport;
}

function validateDocumentEvidenceIdentity(evidence: UnknownRecord): boolean {
  const optionalFields = ["build_id", "segment_id", "vector_space_id", "embedding_set_id",
    "input_generation_id", "input_id", "input_kind", "source_manifest_checksum"];
  if (typeof evidence.kind !== "string" || optionalFields.some((field) =>
    evidence[field] !== undefined && typeof evidence[field] !== "string")) return false;
  const field = (name: string): string => String(evidence[name] ?? "");
  const embeddingEmpty = ["vector_space_id", "embedding_set_id", "input_generation_id", "input_id",
    "input_kind", "source_manifest_checksum"].every((name) => field(name) === "");
  const renditionEmpty = field("build_id") === "" && field("segment_id") === "";
  if (evidence.kind === "node_name" || evidence.kind === "content_blob") {
    return embeddingEmpty && renditionEmpty;
  }
  if (evidence.kind === "rendition_segment") {
    return embeddingEmpty && canonicalHash(field("build_id")) && boundedSearchIdentity(field("segment_id"), 1024);
  }
  if (evidence.kind === "embedding") {
    return renditionEmpty && canonicalHash(field("vector_space_id")) && canonicalHash(field("embedding_set_id")) &&
      canonicalHash(field("input_generation_id")) && boundedSearchIdentity(field("input_id"), 1024) &&
      ["rendition_chunk", "original_file"].includes(field("input_kind")) &&
      canonicalHash(field("source_manifest_checksum"));
  }
  return false;
}

function canonicalUUID(value: unknown): value is string {
  return typeof value === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(value);
}

function boundedSearchIdentity(value: unknown, maximum: number): value is string {
  return typeof value === "string" && value.length > 0 && utf8ToBytes(value).length <= maximum;
}

function boundedDocumentSearchPath(value: unknown): value is string {
  if (typeof value !== "string" || value.length < 2 || utf8ToBytes(value).length > 16 * 1024 ||
      !value.startsWith("/") || value.endsWith("/")) return false;
  return value.slice(1).split("/").every((part) => part.length > 0 && part !== "." && part !== "..");
}

function boundedDocumentSearchExcerpt(value: unknown): value is string {
  return typeof value === "string" && Array.from(value).length <= 512 && utf8ToBytes(value).length <= 4 * 512;
}

function positiveInteger(value: unknown): boolean {
  return Number.isSafeInteger(value) && Number(value) > 0;
}

function nonnegativeInteger(value: unknown): boolean {
  return Number.isSafeInteger(value) && Number(value) >= 0;
}

function boundedLaneRank(value: unknown): boolean {
  return Number.isSafeInteger(value) && Number(value) >= 0 && Number(value) <= 1000;
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
  const maximumSize = 64 * 1024 * 1024;
  if (!Number.isSafeInteger(declaredSize) || declaredSize < 1 || declaredSize > maximumSize) {
    await response.body?.cancel();
    throw new Error("The daemon returned an invalid rendition size.");
  }
  const bytes = await readBoundedRendition(response, declaredSize);
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

async function readBoundedRendition(response: Response, declaredSize: number): Promise<Uint8Array> {
  if (!response.body) throw new Error("The daemon returned an empty rendition body.");
  let reader: ReadableStreamBYOBReader;
  try {
    reader = response.body.getReader({ mode: "byob" });
  } catch (error) {
    await response.body.cancel(error);
    throw new Error("The daemon returned a rendition stream that cannot be read safely.");
  }
  const chunks: Uint8Array[] = [];
  let received = 0;
  try {
    while (true) {
      const remaining = declaredSize + 1 - received;
      const { done, value } = await reader.read(new Uint8Array(Math.min(64 * 1024, remaining)));
      if (value && value.byteLength > 0) {
        received += value.byteLength;
      }
      if (received > declaredSize) {
        await reader.cancel("rendition exceeds its declared size");
        throw new Error("The daemon returned an invalid rendition size.");
      }
      if (value && value.byteLength > 0) chunks.push(value);
      if (done) break;
    }
  } catch (error) {
    try {
      await reader.cancel(error);
    } catch {
      // Preserve the integrity or transport error that caused cancellation.
    }
    throw error;
  } finally {
    reader.releaseLock();
  }
  if (received !== declaredSize) throw new Error("The daemon returned an invalid rendition size.");
  const bytes = new Uint8Array(received);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return bytes;
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
