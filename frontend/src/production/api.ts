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

const setBase = "/api/v1/productions/sets";

export function listProductionSets(session: string, cursor = "", signal?: AbortSignal): Promise<ProductionSetPage> {
  const params = new URLSearchParams({ limit: "50" });
  if (cursor) params.set("cursor", cursor);
  return sessionJSON<ProductionSetPage>(`${setBase}?${params}`, { session, signal });
}

export function getProductionDraft(session: string, set: ProductionSet, signal?: AbortSignal): Promise<ProductionDraft> {
  return sessionJSON<ProductionDraft>(`${setBase}/${encodeURIComponent(set.id)}/revisions/${set.head_revision}`, { session, signal });
}

export function createProductionSet(session: string, name: string, instructions: string,
  operationID: string, signal?: AbortSignal): Promise<ProductionSetCreated> {
  return sessionJSON<ProductionSetCreated>(setBase, {
    session, signal, method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ operation_id: operationID, name, instructions }),
  });
}
