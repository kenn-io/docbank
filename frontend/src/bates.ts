import { offerPreparedDownload } from "./download.js";
import * as generated from "./generated/docbank.js";
import type {
  BatesAllocation,
  BatesDownloadTicket,
  BatesExport,
  BatesExportPage,
  BatesNamespace,
  BatesPageLabel,
  BatesPlan,
  Recipe as BatesRecipe,
} from "./generated/docbank.js";

export interface BatesSource {
  package_id: string;
  package_name: string;
  snapshot_id: string;
  page_count: number;
}

export type {
  BatesAllocation,
  BatesDownloadTicket,
  BatesExport,
  BatesExportPage,
  BatesNamespace,
  BatesPageLabel,
  BatesPlan,
  BatesRecipe,
};

export type BatesPosition =
  | "top-left" | "top-center" | "top-right"
  | "middle-left" | "middle-center" | "middle-right"
  | "bottom-left" | "bottom-center" | "bottom-right";

/** A reservation the operator reviewed but has not yet published. */
export interface PendingBatesReservation {
  operation_id: string;
  snapshot_id: string;
  recipe: BatesRecipe;
  plan: BatesPlan;
  allocation_id?: string;
}

type BatesPlanRequest = Parameters<typeof generated.reserveBatesRange>[0];

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const hash = /^[0-9a-f]{64}$/;
export const maxBatesPages = 250;
const pendingKey = "docbank.bates.pendingReservation";

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
  valid(Array.isArray(value) && value.length > 0 && value.length <= maxBatesPages);
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
  valid(["reserved", "committed"].includes(String(item.state)));
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
  valid(Array.isArray(item.pages) && item.pages.length === item.page_count && item.pages.length <= maxBatesPages);
  const pages = item.pages.map((raw, index) => {
    const page = record(raw);
    valid(page.ordinal === index + 1 && typeof page.occurrence_id === "string" && page.occurrence_id.length > 0);
    valid(typeof page.source_blob_sha256 === "string" && hash.test(page.source_blob_sha256));
    valid(safeInteger(page.source_page, 1) && safeInteger(page.output_page, 1) && typeof page.label === "string" && page.label.length > 0);
    return page as unknown as BatesExport["pages"][number];
  });
  return { ...item, pages } as unknown as BatesExport;
}

export async function listBatesSources(session: string, signal?: AbortSignal): Promise<BatesSource[]> {
  const sources: BatesSource[] = [];
  let after: string | undefined;
  do {
    const page = await generated.listPackages(after ? { limit: 250, after } : { limit: 250 }, { session, signal });
    valid(page.next_after === undefined || page.next_after !== after, "The package list did not advance.");
    for (const item of page.items) {
      const sealed = (item.direction === "received" && ["complete", "partial"].includes(item.state)) ||
        (item.direction === "produced" && item.state === "sealed");
      if (sealed && typeof item.snapshot_id === "string" && uuid.test(item.snapshot_id) && safeInteger(item.page_count, 1)) {
        sources.push({ package_id: item.package_id, package_name: item.package_name, snapshot_id: item.snapshot_id, page_count: item.page_count });
      }
    }
    after = page.next_after;
  } while (after);
  return sources;
}

export async function listBatesNamespaces(session: string, signal?: AbortSignal): Promise<BatesNamespace[]> {
  const page = record(await generated.listBatesNamespaces({ limit: 250 }, { session, signal }));
  valid(Array.isArray(page.items) && safeInteger(page.total) && page.items.length <= 250 && page.items.length <= page.total);
  return page.items.map(readNamespace);
}

export async function createBatesNamespace(session: string, prefix: string, suffix: string, padding: number, signal?: AbortSignal): Promise<BatesNamespace> {
  valid(prefix.length + suffix.length <= 128 && !/[\0\r\n]/.test(prefix + suffix), "Use a short single-line Bates prefix and suffix.");
  valid(safeInteger(padding, 1) && padding <= 10, "Bates padding must be between 1 and 10 digits.");
  return readNamespace(await generated.createBatesNamespace({ prefix, suffix, padding }, { session, signal }));
}

export async function previewBatesRange(session: string, snapshotID: string, namespaceID: string, startAt: number, signal?: AbortSignal): Promise<BatesPlan> {
  const request: BatesPlanRequest = { operation_id: crypto.randomUUID(), namespace_id: namespaceID, snapshot_id: snapshotID, recipe_sha256: "", start_at: startAt };
  return readPlan(await generated.planBatesStamp(request, { session, signal }), namespaceID);
}

function readPlan(value: unknown, namespaceID: string): BatesPlan {
  const item = record(value);
  const namespace = readNamespace(item.namespace);
  const labels = readLabels(item.labels);
  valid(namespace.namespace_id === namespaceID && item.stamped_nothing === true);
  valid(safeInteger(item.start_sequence, 1) && safeInteger(item.end_sequence, item.start_sequence));
  valid(labels.length === item.end_sequence - item.start_sequence + 1);
  return { ...item, namespace, labels } as unknown as BatesPlan;
}

export async function reserveBatesRange(session: string, pending: PendingBatesReservation, signal?: AbortSignal): Promise<BatesAllocation> {
  const recipeSHA256 = await batesRecipeSHA256(pending.recipe);
  valid(uuid.test(pending.operation_id));
  const request: BatesPlanRequest = {
    operation_id: pending.operation_id, namespace_id: pending.plan.namespace.namespace_id, snapshot_id: pending.snapshot_id,
    recipe_sha256: recipeSHA256, start_at: pending.plan.start_sequence,
  };
  return readAllocation(await generated.reserveBatesRange(request, { session, signal }), request);
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
  if (value !== null && typeof value === "object") {
    const entries = Object.entries(value).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
    return `{${entries.map(([key, item]) => `${JSON.stringify(key)}:${canonical(item)}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

export async function batesRecipeSHA256(recipe: BatesRecipe): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(canonical(recipe)));
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}

export async function startBatesExport(session: string, allocation: BatesAllocation, recipe: BatesRecipe, signal?: AbortSignal): Promise<BatesExport> {
  valid(recipe.namespace_id === allocation.namespace_id && recipe.start_at === allocation.start_sequence);
  valid(await batesRecipeSHA256(recipe) === allocation.recipe_sha256);
  const result = readExport(await generated.publishBatesExport({ allocation_id: allocation.allocation_id, recipe }, { session, signal }));
  valid(result.allocation_id === allocation.allocation_id && result.recipe_sha256 === allocation.recipe_sha256);
  return result;
}

export async function readBatesExport(session: string, allocationID: string, signal?: AbortSignal): Promise<BatesExport> {
  valid(uuid.test(allocationID));
  const result = readExport(await generated.readBatesExport(allocationID, { session, signal }));
  valid(result.allocation_id === allocationID);
  return result;
}

export async function listBatesExports(session: string, signal?: AbortSignal): Promise<BatesExportPage> {
  const page = record(await generated.listBatesExports({ limit: 50 }, { session, signal }));
  valid(Array.isArray(page.items) && safeInteger(page.total) && page.items.length <= 50 && page.items.length <= page.total);
  return { ...page, items: page.items.map(readExport) } as unknown as BatesExportPage;
}

export async function downloadBatesExport(session: string, value: BatesExport, signal?: AbortSignal): Promise<void> {
  valid(value.state === "verified");
  const item: BatesDownloadTicket = await generated.downloadBatesExport(value.allocation_id, {}, { session, signal });
  valid(item.allocation_id === value.allocation_id && item.blob_sha256 === value.blob_sha256 && item.size === value.size);
  valid(typeof item.url === "string" && item.url.startsWith("/api/daemon/web-download/file?ticket=") && typeof item.name === "string" && item.name.endsWith(".pdf"));
  offerPreparedDownload(item);
}

/** Returns the reservation this browser tab still owes an export, if any. */
export function loadPendingBatesReservation(): PendingBatesReservation | null {
  let raw: string | null;
  try {
    raw = sessionStorage.getItem(pendingKey);
  } catch {
    return null;
  }
  if (raw === null) return null;
  try {
    const item = record(JSON.parse(raw));
    valid(typeof item.operation_id === "string" && uuid.test(item.operation_id));
    valid(typeof item.snapshot_id === "string" && uuid.test(item.snapshot_id));
    valid(item.allocation_id === undefined || (typeof item.allocation_id === "string" && uuid.test(item.allocation_id)));
    const plan = readPlan(item.plan, readNamespace(record(item.plan).namespace).namespace_id);
    const stored = record(item.recipe);
    const recipe = batesRecipe(plan.namespace, plan.start_sequence, stored.position as BatesPosition, Number(stored.margin_points));
    valid(canonical(recipe) === canonical(stored));
    return { ...item, plan, recipe } as unknown as PendingBatesReservation;
  } catch {
    clearPendingBatesReservation();
    return null;
  }
}

/** Remembers a pending reservation for this tab; returns false when the browser refuses storage. */
export function savePendingBatesReservation(pending: PendingBatesReservation): boolean {
  try {
    sessionStorage.setItem(pendingKey, JSON.stringify(pending));
    return true;
  } catch {
    return false;
  }
}

export function clearPendingBatesReservation(): void {
  try {
    sessionStorage.removeItem(pendingKey);
  } catch {
    // Storage that cannot be read cannot resurrect the reservation either.
  }
}
