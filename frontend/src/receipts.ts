import type { Node, ProcessingSelector, ProcessingJob, ProcessingStatus, ProcessingJobEvent, DocumentSearchRequest, DocumentSearchReport, Tag, TagAssignmentReceipt, TagDeletionReceipt } from "./generated/docbank.js";
import * as generated from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex, utf8ToBytes } from "@noble/hashes/utils.js";
import { parseDocument } from "yaml";

export type ProcessingState =
  | "queued"
  | "running"
  | "retry_wait"
  | "operator_required"
  | "completed"
  | "failed"
  | "abandoned"
  | "partial";

export interface ProcessingRun {
  job: ProcessingJob;
  status: ProcessingStatus;
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





export async function startProcessing(
  session: string,
  selector: ProcessingSelector,
  planFingerprint: string,
  profileFingerprint: string,
  consent: boolean,
  onProgress?: (event: ProcessingJobEvent) => void,
  signal?: AbortSignal,
): Promise<ProcessingRun> {
  const response = await generated.startDocumentProcessing({ selector, plan_fingerprint: planFingerprint, consent }, { session, signal, headers: { Accept: "application/x-ndjson" } });
  if (!(response.headers.get("Content-Type") ?? "").startsWith("application/x-ndjson")) {
    throw new Error("The daemon returned an invalid processing stream.");
  }
  if (!response.body) throw new Error("The daemon returned an invalid processing stream.");
  return readProcessingRun(response.body, selector, profileFingerprint, onProgress);
}

const maxProcessingProgressBytes = 64 * 1024;

async function readProcessingRun(
  body: ReadableStream<Uint8Array>,
  selector: ProcessingSelector,
  profileFingerprint: string,
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
        buffered = consumeProcessingLines(buffered, events, selector, profileFingerprint, onProgress);
      }
      if (!done) continue;
      buffered += decoder.decode();
      if (buffered.length > 0) {
        acceptProcessingLine(buffered, events, selector, profileFingerprint, onProgress);
        buffered = "";
      }
      if (events.length !== 2) {
        throw new Error("The processing stream did not end after its terminal status.");
      }
      const first = events[0];
      const second = events[1];
      if (second?.error) {
        throw new APIError(second.error.detail ?? "Document processing status is unavailable.",
          second.error.status ?? 503, second.error.code ?? "");
      }
      if (!first?.job || !second?.status) {
        throw new Error("The daemon returned malformed processing progress.");
      }
      onProgress?.(second);
      return { job: { ...first.job, embedding_job_ids: second.status.embedding_job_ids }, status: second.status };
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
  selector: ProcessingSelector,
  profileFingerprint: string,
  onProgress?: (event: ProcessingJobEvent) => void,
): string {
  let newline = value.indexOf("\n");
  while (newline >= 0) {
    const raw = value.slice(0, newline).replace(/\r$/, "");
    value = value.slice(newline + 1);
    if (raw.length === 0) throw new Error("The daemon returned malformed processing progress.");
    acceptProcessingLine(raw, events, selector, profileFingerprint, onProgress);
    newline = value.indexOf("\n");
  }
  return value;
}

function acceptProcessingLine(
  value: string,
  events: ProcessingJobEvent[],
  selector: ProcessingSelector,
  profileFingerprint: string,
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
  const event = validateProcessingEvent(decoded, selector, profileFingerprint, events[0]?.job?.id);
  events.push(event);
  if (events.length === 1 || event.type === "error") onProgress?.(event);
}

function validateProcessingEvent(value: unknown, selector: ProcessingSelector, profileFingerprint: string, jobID?: string): ProcessingJobEvent {
  if (!isRecord(value)) throw new Error("The daemon returned malformed processing progress.");
  if (!jobID) {
    if (value.sequence !== 1 || value.type !== "job" || !isProcessingJob(value.job, selector, profileFingerprint) ||
        "status" in value || "error" in value || "terminal" in value) {
      throw new Error("The daemon returned malformed processing progress.");
    }
    return value as unknown as ProcessingJobEvent;
  }
  const validStatus = value.type === "status" && !("job" in value) && !("error" in value) &&
    isProcessingStatus(value.status) && value.status.job_id === jobID;
  const validError = value.type === "error" && !("status" in value) &&
    isProcessingJob(value.job, selector, profileFingerprint) && value.job.id === jobID && isRecord(value.error) &&
    (value.error.status === undefined || Number.isInteger(value.error.status)) &&
    (value.error.code === undefined || typeof value.error.code === "string") &&
    (value.error.detail === undefined || typeof value.error.detail === "string");
  if (value.sequence !== 2 || value.terminal !== true || (!validStatus && !validError)) {
    throw new Error("The daemon returned malformed processing progress.");
  }
  return value as unknown as ProcessingJobEvent;
}

function isProcessingJob(value: unknown, selector: ProcessingSelector, profileFingerprint: string): value is ProcessingJob {
  if (!isRecord(value)) return false;
  return canonicalHash(value.id) && optionalCanonicalHash(value.rendition_job_id) &&
    optionalCanonicalHash(value.attachment_id) && canonicalHashArray(value.embedding_job_ids) &&
    canonicalHash(value.profile_fingerprint) && value.profile_fingerprint === profileFingerprint &&
    canonicalUUID(value.content_version_id) && value.content_version_id === selector.content_version_id;
}

function isProcessingStatus(value: unknown): value is ProcessingStatus {
  if (!isRecord(value)) return false;
  if (!canonicalHash(value.job_id) || !processingState(value.state) ||
      !boundedSearchIdentity(value.phase, 128) || value.phase.trim() === "" ||
      (value.failure_code !== undefined && value.failure_code !== "" &&
        (!boundedSearchIdentity(value.failure_code, 128) || value.failure_code.trim() === "")) ||
      !canonicalHashArray(value.embedding_job_ids) || !nonnegativeInteger(value.completed_bindings) ||
      Number(value.completed_bindings) > value.embedding_job_ids.length) return false;
  const failureCode = value.failure_code ?? "";
  if (value.state === "completed") return failureCode === "" && value.completed_bindings === value.embedding_job_ids.length;
  if (["failed", "operator_required", "retry_wait"].includes(value.state)) return failureCode !== "";
  // Running retries can retain a failure code; abandoned work may have none.
  return true;
}

function processingState(value: unknown): value is ProcessingState {
  return typeof value === "string" &&
    ["queued", "running", "retry_wait", "operator_required", "completed", "failed", "abandoned", "partial"].includes(value);
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

function canonicalHashArray(value: unknown): value is string[] {
  return Array.isArray(value) && value.every(canonicalHash) && new Set(value).size === value.length;
}

export async function documentSearch(
  session: string,
  request: DocumentSearchRequest,
): Promise<DocumentSearchReport> {
  const response = await generated.searchDocuments(request, { session });
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
        !validDocumentSearchPath(result.path) ||
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
        evidenceItem.input_id ?? "", evidenceItem.input_kind ?? "", evidenceItem.source_manifest_checksum ?? "",
        isRecord(evidenceItem.time_span) ? evidenceItem.time_span.start_ms : null,
        isRecord(evidenceItem.time_span) ? evidenceItem.time_span.end_ms : null]);
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
  if (evidence.time_span !== undefined && (!isRecord(evidence.time_span) ||
      !nonnegativeInteger(evidence.time_span.start_ms) || !positiveInteger(evidence.time_span.end_ms) ||
      Number(evidence.time_span.end_ms) <= Number(evidence.time_span.start_ms))) return false;
  const field = (name: string): string => String(evidence[name] ?? "");
  const embeddingEmpty = ["vector_space_id", "embedding_set_id", "input_generation_id", "input_id",
    "input_kind", "source_manifest_checksum"].every((name) => field(name) === "");
  const renditionEmpty = field("build_id") === "" && field("segment_id") === "";
  if (evidence.kind === "node_name" || evidence.kind === "content_blob") {
    return embeddingEmpty && renditionEmpty && evidence.time_span === undefined;
  }
  if (evidence.kind === "rendition_segment") {
    return embeddingEmpty && canonicalHash(field("build_id")) && boundedSearchIdentity(field("segment_id"), 1024);
  }
  if (evidence.kind === "embedding") {
    const renditionChunk = field("input_kind") === "rendition_chunk";
    return field("segment_id") === "" &&
      (renditionChunk ? canonicalHash(field("build_id")) : field("build_id") === "") &&
      (evidence.time_span === undefined || (renditionChunk && canonicalHash(field("build_id")))) &&
      canonicalHash(field("vector_space_id")) && canonicalHash(field("embedding_set_id")) &&
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

function validDocumentSearchPath(value: unknown): value is string {
  if (typeof value !== "string" || value.length < 2 ||
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

// Shared Go/browser response examples check both sides of document.MaxRetrievalCandidateLimit.
const maxRetrievalCandidateLimit = 1000;

function boundedLaneRank(value: unknown): boolean {
  return Number.isSafeInteger(value) && Number(value) >= 0 && Number(value) <= maxRetrievalCandidateLimit;
}

export async function renditionArtifact(session: string, attachmentID: string): Promise<RenditionArtifact> {
  const response = await generated.getDocumentRendition(attachmentID, undefined, undefined, { session, headers: { Accept: "text/markdown" } });
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
  const metadata = parseRenditionMarkdown(artifact);
  if (metadata.buildID !== buildID || metadata.completeness !== transportCompleteness) {
    throw new Error("The rendition transport metadata does not match its frontmatter.");
  }
  return { attachmentID, buildID, artifactID, contentVersionID, blobHash, profileFingerprint,
    frontmatter: metadata.frontmatter, markdown: metadata.markdown, completeness: metadata.completeness, warnings, source: metadata.source,
    document: metadata.document, navigation: metadata.navigation };
}

export function parseRenditionMarkdown(artifact: string) {
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
  return { frontmatter, markdown, ...metadata };
}

async function readBoundedRendition(response: Response, declaredSize: number): Promise<Uint8Array> {
  if (!response.body) throw new Error("The daemon returned an empty rendition body.");
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let received = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
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
  let scannedByte = 0;
  let currentLine = 1;
  const entries = rawEntries.map((rawEntry) => {
    const entry = record(rawEntry, "navigation entry");
    exactKeys(entry, ["key", "kind", "line", "byte"], ["title"]);
    const key = textField(entry, "key");
    const kind = textField(entry, "kind");
    const title = textField(entry, "title", true);
    const line = integerField(entry, "line", 1);
    const byte = integerField(entry, "byte", 0);
    if (!["generic", "line", "message", "page", "record", "section", "sheet", "slide", "spine"].includes(kind) ||
        seen.has(key) || byte < priorByte || byte >= bodyBytes.length || (bodyBytes[byte]! & 0xc0) === 0x80) {
      throw new Error("The rendition navigation entry is invalid.");
    }
    while (scannedByte < byte) {
      if (bodyBytes[scannedByte++] === 0x0a) currentLine += 1;
    }
    if (line !== currentLine) throw new Error("The rendition navigation entry is invalid.");
    seen.add(key);
    priorByte = byte;
    return { key, kind, title, line, byte };
  });
  return { buildID, bodyHash, completeness, source, document,
    navigation: { complete: booleanField(navigationValue, "complete"), entries } };
}

export async function createTag(session: string, name: string): Promise<Tag> {
  const normalized = name.normalize("NFC");
  const tag = await generated.createTag({ name }, { session });
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
  const tag = await generated.renameTag(tagID, { name }, { "If-Match": String(revision) }, { session });
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
  const receipt = await generated.deleteTag(tagID, { "If-Match": String(revision) }, { session });
  if (
    receipt.tag.id !== tagID ||
    receipt.tag.revision !== revision ||
    receipt.removed_assignments !== receipt.tag.assignment_count
  ) {
    throw new Error("The daemon returned an invalid deleted-tag receipt.");
  }
  return receipt;
}

export async function liveNodeTags(
  session: string,
  nodeID: number,
): Promise<{ node: Node; items: Tag[]; total: number }> {
  const before = await generated.getNode(nodeID, { session });
  if (before.id !== nodeID || before.revision < 1 || before.trashed_at) {
    throw new Error("The daemon returned invalid live node authority for tag inspection.");
  }
  const items: Tag[] = [];
  const identities = new Set<string>();
  let total: number | undefined;
  while (total === undefined || items.length < total) {
    const page = await generated.listNodeTags(nodeID, { limit: 1000, offset: items.length }, { session });
    if (
      !Number.isSafeInteger(page.total) || page.total < 0 ||
      page.limit !== 1000 || page.offset !== items.length ||
      (total !== undefined && page.total !== total) ||
      page.items.length !== Math.min(1000, page.total - items.length)
    ) {
      throw new Error("The live tag listing was incomplete or inconsistent.");
    }
    total = page.total;
    for (const tag of page.items) {
      if (!tag.id || identities.has(tag.id)) {
        throw new Error("The live tag listing was incomplete or inconsistent.");
      }
      identities.add(tag.id);
      items.push(tag);
    }
  }
  const after = await generated.getNode(nodeID, { session });
  if (after.id !== before.id || after.revision !== before.revision || after.trashed_at) {
    throw new Error("The selected node changed while tags were loading; refresh and try again.");
  }
  if (items.length !== total || identities.size !== total) {
    throw new Error("The live tag listing was incomplete or inconsistent.");
  }
  return { node: after, items, total };
}

export async function changeNodeTag(
  session: string,
  nodeID: number,
  revision: number,
  tagID: string,
  assign: boolean,
): Promise<TagAssignmentReceipt> {
  const receipt = await (assign ? generated.assignTag : generated.unassignTag)(nodeID, tagID, { "If-Match": String(revision) }, { session });
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
  const node = await generated.trashNode(nodeID, { "If-Match": String(revision) }, { session });
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

export async function restoreNode(
  session: string,
  nodeID: number,
  revision: number,
): Promise<Node> {
  const node = await generated.restoreNode(nodeID, { "If-Match": String(revision) }, { session });
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
