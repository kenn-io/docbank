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
  mode: "redact_selected" | "keep_selected";
  reviewed: boolean;
}

export interface ProductionDecision {
  id: string;
  member_id: string;
  action: "keep" | "redact";
  uncertain: boolean;
  selector: {
    kind: string;
    pages?: number[];
    boxes?: { page: number }[];
    span?: { start: number; end: number };
  };
}

export interface ProductionPage<T> {
  items: T[];
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

export function createProductionSet(session: string, name: string, instructions: string,
  operationID: string, signal?: AbortSignal): Promise<ProductionSetCreated> {
  return sessionJSON<ProductionSetCreated>(setBase, {
    session, signal, method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ operation_id: operationID, name, instructions }),
  });
}
