import { sessionJSON } from "./api-transport.js";
import * as generated from "./generated/docbank.js";

export interface BatesSource {
  package_id: string;
  package_name: string;
  snapshot_id: string;
  page_count: number;
}

export interface BatesNamespace {
  namespace_id: string;
  prefix: string;
  suffix: string;
  padding: number;
  created_at: string;
}

export interface BatesPageLabel {
  ordinal: number;
  occurrence_id: string;
  source_page: number;
  output_page: number;
  label: string;
}

export interface BatesPlan {
  namespace: BatesNamespace;
  start_sequence: number;
  end_sequence: number;
  labels: BatesPageLabel[];
  stamped_nothing: boolean;
}

export interface BatesAllocation {
  allocation_id: string;
  namespace_id: string;
  snapshot_id: string;
  recipe_sha256: string;
  state: "reserved" | "committed" | "abandoned";
  start_sequence: number;
  end_sequence: number;
  labels: BatesPageLabel[];
  created_at: string;
  committed_at?: string;
}

export interface BatesEngineIdentity {
  name: "pdfcpu";
  version: "v0.15.0";
  api: "AddWatermarksMap";
  options: ["onTop=true", "update=restamp"];
}

export interface BatesRecipe {
  contract: "bates-stamp/v1";
  namespace_id: string;
  prefix: string;
  suffix: string;
  padding: number;
  start_at: number;
  position: BatesPosition;
  margin_points: number;
  font_name: "Helvetica";
  font_size_points: 9;
  color: "#000000";
  opacity: 1;
  units: "point";
  rotation_policy: "follow_page";
  restamp: false;
  engine_identity: BatesEngineIdentity;
}

export type BatesPosition =
  | "top-left" | "top-center" | "top-right"
  | "middle-left" | "middle-center" | "middle-right"
  | "bottom-left" | "bottom-center" | "bottom-right";

export interface BatesExportPageReceipt {
  ordinal: number;
  occurrence_id: string;
  source_blob_sha256: string;
  source_page: number;
  output_page: number;
  label: string;
}

export interface BatesExport {
  artifact_id: string;
  allocation_id: string;
  state: "verified";
  blob_sha256: string;
  size: number;
  media_type: "application/pdf";
  page_count: number;
  recipe_sha256: string;
  manifest_sha256: string;
  created_at: string;
  pages: BatesExportPageReceipt[];
}

export interface BatesExportPage {
  items: BatesExport[];
  total: number;
  next_after?: string;
}

export interface PreparedBatesDownload {
  url: string;
  name: string;
  allocation_id: string;
  blob_sha256: string;
  size: number;
}

interface BatesPlanRequest {
  operation_id: string;
  namespace_id: string;
  snapshot_id: string;
  recipe_sha256: string;
  start_at: number;
}

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const hash = /^[0-9a-f]{64}$/;
const maxLabels = 250;

function valid(condition: unknown, message = "The Bates response did not match the requested export."): asserts condition {
  if (!condition) throw new Error(message);
}

function record(value: unknown): Record<string, unknown> {
  valid(value !== null && typeof value === "object" && !Array.isArray(value));
  return value as Record<string, unknown>;
}

function safeInteger(value: unknown, minimum = 0): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= minimum;
}

function timestamp(value: unknown): value is string {
  return typeof value === "string" && Number.isFinite(Date.parse(value));
}

function readNamespace(value: unknown): BatesNamespace {
  const item = record(value);
  valid(typeof item.namespace_id === "string" && uuid.test(item.namespace_id));
  valid(typeof item.prefix === "string" && typeof item.suffix === "string");
  valid(safeInteger(item.padding, 1) && item.padding <= 10 && timestamp(item.created_at));
  return item as unknown as BatesNamespace;
}

function readLabels(value: unknown): BatesPageLabel[] {
  valid(Array.isArray(value) && value.length > 0 && value.length <= maxLabels);
  return value.map((raw, index) => {
    const item = record(raw);
    valid(item.ordinal === index + 1 && typeof item.occurrence_id === "string" && item.occurrence_id.length > 0);
    valid(safeInteger(item.source_page, 1) && safeInteger(item.output_page, 1) && typeof item.label === "string" && item.label.length > 0);
    return item as unknown as BatesPageLabel;
  });
}

function readAllocation(value: unknown, request?: BatesPlanRequest): BatesAllocation {
  const item = record(value);
  valid(typeof item.allocation_id === "string" && uuid.test(item.allocation_id));
  valid(typeof item.namespace_id === "string" && uuid.test(item.namespace_id));
  valid(typeof item.snapshot_id === "string" && uuid.test(item.snapshot_id));
  valid(typeof item.recipe_sha256 === "string" && hash.test(item.recipe_sha256));
  valid(["reserved", "committed", "abandoned"].includes(String(item.state)));
  valid(safeInteger(item.start_sequence, 1) && safeInteger(item.end_sequence, item.start_sequence));
  valid(timestamp(item.created_at));
  const labels = readLabels(item.labels);
  valid(labels.length === item.end_sequence - item.start_sequence + 1);
  if (request) {
    valid(item.namespace_id === request.namespace_id && item.snapshot_id === request.snapshot_id &&
      item.recipe_sha256 === request.recipe_sha256 && item.start_sequence === request.start_at);
  }
  return { ...item, labels } as unknown as BatesAllocation;
}

function readExport(value: unknown): BatesExport {
  const item = record(value);
  valid(typeof item.artifact_id === "string" && uuid.test(item.artifact_id));
  valid(typeof item.allocation_id === "string" && uuid.test(item.allocation_id));
  valid(item.state === "verified" && item.media_type === "application/pdf");
  valid(safeInteger(item.size, 1) && safeInteger(item.page_count, 1) && timestamp(item.created_at));
  valid(typeof item.blob_sha256 === "string" && hash.test(item.blob_sha256));
  valid(typeof item.recipe_sha256 === "string" && hash.test(item.recipe_sha256));
  valid(typeof item.manifest_sha256 === "string" && hash.test(item.manifest_sha256));
  valid(Array.isArray(item.pages) && item.pages.length === item.page_count && item.pages.length <= maxLabels);
  const pages = item.pages.map((raw, index) => {
    const page = record(raw);
    valid(page.ordinal === index + 1 && typeof page.occurrence_id === "string" && page.occurrence_id.length > 0);
    valid(typeof page.source_blob_sha256 === "string" && hash.test(page.source_blob_sha256));
    valid(safeInteger(page.source_page, 1) && safeInteger(page.output_page, 1) && typeof page.label === "string" && page.label.length > 0);
    return page as unknown as BatesExportPageReceipt;
  });
  return { ...item, pages } as unknown as BatesExport;
}

export async function listBatesSources(session: string, signal?: AbortSignal): Promise<BatesSource[]> {
  const page = await generated.listPackages({ limit: 250 }, { session, signal });
  return page.items.flatMap((item) => ((item.direction === "received" && ["complete", "partial"].includes(item.state)) ||
    (item.direction === "produced" && item.state === "sealed")) &&
    typeof item.snapshot_id === "string" && uuid.test(item.snapshot_id) && safeInteger(item.page_count, 1)
    ? [{ package_id: item.package_id, package_name: item.package_name, snapshot_id: item.snapshot_id, page_count: item.page_count }]
    : []);
}

export async function listBatesNamespaces(session: string, signal?: AbortSignal): Promise<BatesNamespace[]> {
  const page = record(await sessionJSON<unknown>("/api/v1/bates/namespaces?limit=250", { session, signal }));
  valid(Array.isArray(page.items) && safeInteger(page.total) && page.items.length <= 250 && page.items.length <= page.total);
  return page.items.map(readNamespace);
}

export async function createBatesNamespace(session: string, prefix: string, suffix: string, padding: number, signal?: AbortSignal): Promise<BatesNamespace> {
  valid(prefix.length + suffix.length <= 128 && !/[\0\r\n]/.test(prefix + suffix), "Use a short single-line Bates prefix and suffix.");
  valid(safeInteger(padding, 1) && padding <= 10, "Bates padding must be between 1 and 10 digits.");
  return readNamespace(await sessionJSON("/api/v1/bates/namespaces", {
    session, signal, method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ prefix, suffix, padding }),
  }));
}

export async function previewBatesRange(session: string, snapshotID: string, namespaceID: string, startAt: number, signal?: AbortSignal): Promise<BatesPlan> {
  const request: BatesPlanRequest = { operation_id: crypto.randomUUID(), namespace_id: namespaceID, snapshot_id: snapshotID, recipe_sha256: "", start_at: startAt };
  const item = record(await sessionJSON("/api/v1/bates/preview", { session, signal, method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(request) }));
  const namespace = readNamespace(item.namespace);
  const labels = readLabels(item.labels);
  valid(namespace.namespace_id === namespaceID && item.stamped_nothing === true);
  valid(safeInteger(item.start_sequence, 1) && safeInteger(item.end_sequence, item.start_sequence));
  valid(labels.length === item.end_sequence - item.start_sequence + 1);
  return { ...item, namespace, labels } as unknown as BatesPlan;
}

export async function reserveBatesRange(session: string, snapshotID: string, plan: BatesPlan, recipeSHA256: string, operationID: string, signal?: AbortSignal): Promise<BatesAllocation> {
  valid(uuid.test(operationID) && hash.test(recipeSHA256));
  const request: BatesPlanRequest = { operation_id: operationID, namespace_id: plan.namespace.namespace_id, snapshot_id: snapshotID, recipe_sha256: recipeSHA256, start_at: plan.start_sequence };
  return readAllocation(await sessionJSON("/api/v1/bates/allocations", { session, signal, method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(request) }), request);
}

export function batesRecipe(namespace: BatesNamespace, startAt: number, position: BatesPosition = "bottom-right", marginPoints = 24): BatesRecipe {
  valid(safeInteger(startAt, 1), "The reviewed Bates range must start at a positive number.");
  valid(safeInteger(marginPoints) && marginPoints <= 144, "Bates margin must be between 0 and 144 points.");
  return {
    contract: "bates-stamp/v1", namespace_id: namespace.namespace_id, prefix: namespace.prefix, suffix: namespace.suffix,
    padding: namespace.padding, start_at: startAt, position, margin_points: marginPoints, font_name: "Helvetica",
    font_size_points: 9, color: "#000000", opacity: 1, units: "point", rotation_policy: "follow_page", restamp: false,
    engine_identity: { name: "pdfcpu", version: "v0.15.0", api: "AddWatermarksMap", options: ["onTop=true", "update=restamp"] },
  };
}

function canonical(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  if (value !== null && typeof value === "object") return `{${Object.entries(value).sort(([a], [b]) => a.localeCompare(b)).map(([key, item]) => `${JSON.stringify(key)}:${canonical(item)}`).join(",")}}`;
  return JSON.stringify(value);
}

export async function batesRecipeSHA256(recipe: BatesRecipe): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(canonical(recipe)));
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}

export async function startBatesExport(session: string, allocation: BatesAllocation, recipe: BatesRecipe, signal?: AbortSignal): Promise<BatesExport> {
  valid(recipe.namespace_id === allocation.namespace_id && recipe.start_at === allocation.start_sequence);
  valid(await batesRecipeSHA256(recipe) === allocation.recipe_sha256);
  const result = readExport(await sessionJSON("/api/v1/bates/exports", { session, signal, method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ allocation_id: allocation.allocation_id, recipe }) }));
  valid(result.allocation_id === allocation.allocation_id && result.recipe_sha256 === allocation.recipe_sha256);
  return result;
}

export async function readBatesExport(session: string, allocationID: string, signal?: AbortSignal): Promise<BatesExport> {
  valid(uuid.test(allocationID));
  const result = readExport(await sessionJSON(`/api/v1/bates/exports/${encodeURIComponent(allocationID)}`, { session, signal }));
  valid(result.allocation_id === allocationID);
  return result;
}

export async function listBatesExports(session: string, signal?: AbortSignal): Promise<BatesExportPage> {
  const page = record(await sessionJSON("/api/v1/bates/exports?limit=50", { session, signal }));
  valid(Array.isArray(page.items) && safeInteger(page.total) && page.items.length <= 50 && page.items.length <= page.total);
  return { ...page, items: page.items.map(readExport) } as unknown as BatesExportPage;
}

export async function prepareBatesDownload(session: string, value: BatesExport, signal?: AbortSignal): Promise<PreparedBatesDownload> {
  valid(value.state === "verified");
  const item = record(await sessionJSON(`/api/v1/bates/exports/${encodeURIComponent(value.allocation_id)}/download`, {
    session,
    signal,
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({}),
  }));
  valid(item.allocation_id === value.allocation_id && item.blob_sha256 === value.blob_sha256 && item.size === value.size);
  valid(typeof item.url === "string" && item.url.startsWith("/api/daemon/web-download/file?ticket=") && typeof item.name === "string" && item.name.endsWith(".pdf"));
  return item as unknown as PreparedBatesDownload;
}

export function offerBatesDownload(value: PreparedBatesDownload): void {
  const link = document.createElement("a");
  link.href = value.url;
  link.download = value.name;
  link.rel = "noopener";
  link.click();
}
