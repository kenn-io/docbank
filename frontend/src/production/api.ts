import { sessionJSON } from "../api-transport.js";

export interface ProductionSet {
  id: string;
  name: string;
  creator: string;
  created_at: string;
  head_revision: number;
}

export interface ProductionDraft {
  set_id: string;
  revision: number;
  etag: number;
  state: string;
  membership_sealed: boolean;
}

export interface ProductionSetPage {
  items: ProductionSet[];
  next_cursor: string;
}

export interface ProductionSetCreated {
  set: ProductionSet;
  draft: ProductionDraft;
}

export interface ProductionMember {
  id: string;
  ordinal: number;
  node_id: number;
  source_version_id: string;
  pdf_sha256: string;
  pdf_size: number;
  map_sha256: string;
  mode: "redact_selected" | "keep_selected";
  reviewed: boolean;
}

export interface ProductionDecision {
  id: string;
  member_id: string;
  action: "keep" | "redact";
  uncertain: boolean;
  reason: string;
  label: string;
  selector: {
    kind: string;
    map_sha256: string;
    unit_id?: string;
    pages?: number[];
    boxes?: { page: number; frame_sha256: string; x0: number; y0: number; x1: number; y1: number }[];
    span?: { start: number; end: number };
  };
}

export type ProductionChange =
  | { kind: "mode"; member_id: string; mode: ProductionMember["mode"] }
  | { kind: "decision"; decision: ProductionDecision };

export interface ProductionReceipt {
  operation_id: string;
  set_id: string;
  revision: number;
  etag: number;
}

export interface ProductionMapChunk {
  map_sha256: string;
  offset: number;
  total_bytes: number;
  data: string;
  chunk_sha256: string;
  next_cursor: string;
}

export interface ProductionPage<T> {
  items: T[];
  next_cursor: string;
}

export interface ProductionResolvedMaskPage {
  set_id: string;
  revision: number;
  etag: number;
  member_id: string;
  page: { number: number; frame_sha256: string; width: number; height: number; span: { start: number; end: number } };
  map_sha256: string;
  recipe_sha256: string;
  resolved_sha256: string;
  review_binding: string;
  total_boxes: number;
  items: { page: number; frame_sha256: string; x0: number; y0: number; x1: number; y1: number }[];
  next_cursor: string;
}

const setBase = "/api/v1/productions/sets";

export function listProductionSets(session: string, cursor = "", signal?: AbortSignal): Promise<ProductionSetPage> {
  const params = new URLSearchParams({ limit: "50" });
  if (cursor) params.set("cursor", cursor);
  return sessionJSON<ProductionSetPage>(`${setBase}?${params}`, { session, signal });
}

export function getProductionDraft(session: string, set: ProductionSet, signal?: AbortSignal): Promise<ProductionDraft> {
  return getProductionDraftAt(session, set.id, set.head_revision, signal);
}

export function getProductionDraftAt(session: string, setID: string, revision: number, signal?: AbortSignal): Promise<ProductionDraft> {
  return sessionJSON<ProductionDraft>(`${setBase}/${encodeURIComponent(setID)}/revisions/${revision}`, { session, signal });
}

export function getProductionSet(session: string, setID: string, signal?: AbortSignal): Promise<ProductionSet> {
  return sessionJSON<ProductionSet>(`${setBase}/${encodeURIComponent(setID)}`, { session, signal });
}

export function listProductionMembers(session: string, setID: string, revision: number,
  cursor = "", signal?: AbortSignal): Promise<ProductionPage<ProductionMember>> {
  const params = new URLSearchParams({ limit: "50" });
  if (cursor) params.set("cursor", cursor);
  return sessionJSON<ProductionPage<ProductionMember>>(`${setBase}/${encodeURIComponent(setID)}/revisions/${revision}/members?${params}`, { session, signal });
}

export function listUncertainDecisions(session: string, setID: string, revision: number,
  cursor = "", signal?: AbortSignal): Promise<ProductionPage<ProductionDecision>> {
  const params = new URLSearchParams({ limit: "50", uncertain: "true" });
  if (cursor) params.set("cursor", cursor);
  return sessionJSON<ProductionPage<ProductionDecision>>(`${setBase}/${encodeURIComponent(setID)}/revisions/${revision}/decisions?${params}`, { session, signal });
}

export function applyProductionChange(session: string, setID: string, revision: number,
  etag: number, operationID: string, change: ProductionChange, signal?: AbortSignal): Promise<ProductionReceipt> {
  return sessionJSON<ProductionReceipt>(`${setBase}/${encodeURIComponent(setID)}/revisions/${revision}/changes`, {
    session, signal, method: "POST", headers: { "Content-Type": "application/json", "If-Match": String(etag) },
    body: JSON.stringify({ operation_id: operationID, changes: [change] }),
  });
}

export function getProductionMapChunk(session: string, setID: string, revision: number, memberID: string,
  cursor = "", signal?: AbortSignal): Promise<ProductionMapChunk> {
  const params = new URLSearchParams({ limit: "65536" });
  if (cursor) params.set("cursor", cursor);
  return sessionJSON<ProductionMapChunk>(`${setBase}/${encodeURIComponent(setID)}/revisions/${revision}/maps/${encodeURIComponent(memberID)}?${params}`, { session, signal });
}

export function resolveProductionPage(session: string, setID: string, revision: number, etag: number,
  memberID: string, page: number, signal?: AbortSignal): Promise<ProductionResolvedMaskPage> {
  return sessionJSON<ProductionResolvedMaskPage>(`${setBase}/${encodeURIComponent(setID)}/revisions/${revision}/resolve`, {
    session, signal, method: "POST", headers: { "Content-Type": "application/json", "If-Match": String(etag) },
    body: JSON.stringify({ member_id: memberID, page, limit: 1 }),
  });
}

export function createProductionSet(session: string, name: string, instructions: string,
  operationID: string, signal?: AbortSignal): Promise<ProductionSetCreated> {
  return sessionJSON<ProductionSetCreated>(setBase, {
    session, signal, method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ operation_id: operationID, name, instructions }),
  });
}
