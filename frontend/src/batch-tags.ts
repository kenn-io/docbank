import { requestJSON } from "./api.js";
import type { SelectionTarget } from "./selection.js";

export interface BatchTagRequest {
  operation_id: string;
  tag_id: string;
  assign: boolean;
  nodes: SelectionTarget[];
}
export interface BatchTagNodeResult {
  node_id: number;
  expected_revision: number;
  revision: number;
  changed: boolean;
}
export interface BatchTagReceipt {
  version: number;
  operation_id: string;
  request_digest: string;
  tag_id: string;
  assign: boolean;
  tag_revision: number;
  assignment_count: number;
  completed_at: string;
  nodes: BatchTagNodeResult[];
}
export interface BatchTagPreview {
  tag_id: string;
  tag_revision: number;
  nodes: { node_id: number; revision: number; assigned: boolean }[];
}

const uuidV4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

function positiveInteger(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value > 0;
}

function record(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function validUUID(value: unknown): value is string {
  return typeof value === "string" && uuidV4.test(value);
}

function canonicalTargets(nodes: readonly SelectionTarget[]): SelectionTarget[] {
  if (!Array.isArray(nodes) || nodes.length < 1 || nodes.length > 1000) {
    throw new Error("Choose between 1 and 1,000 documents.");
  }
  const seen = new Set<number>();
  const result = nodes.map((node) => {
    if (!record(node) || !positiveInteger(node.node_id) || !positiveInteger(node.revision) || seen.has(node.node_id)) {
      throw new Error("Selected documents need unique identities and valid revisions.");
    }
    seen.add(node.node_id);
    return { node_id: node.node_id, revision: node.revision };
  });
  return result.sort((left, right) => left.node_id - right.node_id);
}

function canonicalRequest(request: BatchTagRequest): BatchTagRequest {
  if (!record(request) || !validUUID(request.operation_id) || !validUUID(request.tag_id) || typeof request.assign !== "boolean") {
    throw new Error("The tag operation has an invalid identity or action.");
  }
  return { operation_id: request.operation_id, tag_id: request.tag_id,
    assign: request.assign, nodes: canonicalTargets(request.nodes) };
}

export async function batchTagRequestDigest(request: BatchTagRequest): Promise<string> {
  const normalized = canonicalRequest(request);
  const text = `docbank-tag-batch-v1\n${normalized.tag_id}\n${normalized.assign ? "1" : "0"}\n` +
    normalized.nodes.map((node) => `${node.node_id}:${node.revision}\n`).join("");
  const hash = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text));
  return Array.from(new Uint8Array(hash), (byte) => byte.toString(16).padStart(2, "0")).join("");
}

function canonicalTimestamp(value: unknown): value is string {
  if (typeof value !== "string" || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{9}Z$/.test(value)) return false;
  const milliseconds = value.slice(0, 23) + "Z";
  const parsed = new Date(milliseconds);
  return Number.isFinite(parsed.getTime()) && parsed.toISOString() === milliseconds;
}

export async function validateBatchTagReceipt(request: BatchTagRequest, value: unknown): Promise<BatchTagReceipt> {
  const normalized = canonicalRequest(request);
  const invalid = () => new Error("The daemon returned an invalid batch tag receipt. The outcome is not confirmed; retry the same operation.");
  if (!record(value) || value.version !== 1 || value.operation_id !== normalized.operation_id ||
      value.tag_id !== normalized.tag_id || value.assign !== normalized.assign ||
      !positiveInteger(value.tag_revision) || typeof value.assignment_count !== "number" ||
      !Number.isSafeInteger(value.assignment_count) || value.assignment_count < 0 ||
      !canonicalTimestamp(value.completed_at) || !Array.isArray(value.nodes) ||
      value.nodes.length !== normalized.nodes.length) throw invalid();
  let changed = 0;
  for (let index = 0; index < normalized.nodes.length; index++) {
    const expected = normalized.nodes[index];
    const actual: unknown = value.nodes[index];
    if (!record(actual) || actual.node_id !== expected.node_id ||
        actual.expected_revision !== expected.revision || typeof actual.changed !== "boolean" ||
        !positiveInteger(actual.revision) || actual.revision !== expected.revision + (actual.changed ? 1 : 0)) throw invalid();
    if (actual.changed) changed++;
  }
  if (value.tag_revision <= changed || (normalized.assign && value.assignment_count < normalized.nodes.length) ||
      value.request_digest !== await batchTagRequestDigest(normalized)) throw invalid();
  return value as unknown as BatchTagReceipt;
}

export async function changeBatchTags(session: string, request: BatchTagRequest): Promise<BatchTagReceipt> {
  const normalized = canonicalRequest(request);
  // Compute before egress so unavailable local crypto cannot turn a committed
  // operation into an avoidable client-validation failure.
  await batchTagRequestDigest(normalized);
  const value = await requestJSON<unknown>("/api/v1/batch/tags", session, {
    method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(normalized),
  });
  return validateBatchTagReceipt(normalized, value);
}

export async function previewBatchTags(session: string, tagID: string, targets: readonly SelectionTarget[]): Promise<BatchTagPreview> {
  if (!validUUID(tagID)) throw new Error("Choose a valid tag identity.");
  const nodes = canonicalTargets(targets);
  const value = await requestJSON<unknown>("/api/v1/batch/tags/preview", session, {
    method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ tag_id: tagID, nodes }),
  });
  const invalid = () => new Error("The daemon returned an incomplete or invalid tag membership preview.");
  if (!record(value) || value.tag_id !== tagID || !positiveInteger(value.tag_revision) ||
      !Array.isArray(value.nodes) || value.nodes.length !== nodes.length) throw invalid();
  for (let index = 0; index < nodes.length; index++) {
    const actual: unknown = value.nodes[index];
    if (!record(actual) || actual.node_id !== nodes[index].node_id ||
        actual.revision !== nodes[index].revision || typeof actual.assigned !== "boolean") throw invalid();
  }
  return value as unknown as BatchTagPreview;
}
