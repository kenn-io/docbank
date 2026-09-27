import { afterEach, expect, it, vi } from "vitest";
import { finalizeCurrentProductionDraft, type FinalizeAttempt } from "./finalizeDraft.js";
import type { ProductionDraft, ProductionSet } from "./api.js";

const set: ProductionSet = { id: "11111111-1111-4111-8111-111111111111", name: "Synthetic numbered",
  creator: "synthetic", created_at: "2026-09-27T00:00:00Z", head_revision: 2 };
const draft = { set_id: set.id, revision: 2, etag: 5, state: "draft", membership_sealed: true,
  member_hash: "a".repeat(64), numbering_recipe_id: "bates-sequential-v1" } as ProductionDraft;
const attempt: FinalizeAttempt = { set_id: set.id, revision: 2, etag: 5,
  namespace_id: "22222222-2222-4222-8222-222222222222",
  snapshot_id: "33333333-3333-4333-8333-333333333333",
  operation_id: "44444444-4444-4444-8444-444444444444" };
const finalized = { draft: { ...draft, state: "finalized" }, operation_id: attempt.operation_id,
  namespace_id: attempt.namespace_id, snapshot_id: attempt.snapshot_id,
  prepared_sha256: "b".repeat(64), receipt_sha256: "c".repeat(64) };

afterEach(() => vi.restoreAllMocks());

it("finalizes the exact current numbered draft with a fresh snapshot identity", async () => {
  const urls: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input); urls.push(url);
    if (url === `/api/v1/productions/sets/${set.id}`) return Response.json(set);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2`) return Response.json(draft);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2/finalize`) {
      expect(init?.method).toBe("POST");
      expect(new Headers(init?.headers).get("If-Match")).toBe("5");
      expect(JSON.parse(String(init?.body))).toEqual({ operation_id: attempt.operation_id,
        namespace_id: attempt.namespace_id, snapshot_id: attempt.snapshot_id, start_at: 0 });
      return Response.json(finalized);
    }
    throw new Error(`unexpected ${url}`);
  });
  const result = await finalizeCurrentProductionDraft("synthetic", set, draft, attempt,
    new AbortController().signal);
  expect(result.draft.state).toBe("finalized");
  expect(urls).toHaveLength(3);
});

it("rejects an unnumbered or changed draft before sending finalization", async () => {
  const signal = new AbortController().signal;
  const fetch = vi.spyOn(globalThis, "fetch");
  await expect(finalizeCurrentProductionDraft("synthetic", set,
    { ...draft, numbering_recipe_id: "" }, attempt, signal)).rejects.toThrow(/numbered/i);
  expect(fetch).not.toHaveBeenCalled();
  fetch.mockResolvedValueOnce(Response.json({ ...set, head_revision: 3 }));
  await expect(finalizeCurrentProductionDraft("synthetic", set, draft, attempt, signal))
    .rejects.toThrow(/changed/i);
  expect(fetch).toHaveBeenCalledTimes(1);
});

it("replays the frozen finalization after a lost response without a new preflight", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    expect(String(input)).toBe(`/api/v1/productions/sets/${set.id}/revisions/2/finalize`);
    expect(JSON.parse(String(init?.body)).operation_id).toBe(attempt.operation_id);
    return Response.json(finalized);
  });
  const result = await finalizeCurrentProductionDraft("synthetic", set, draft, attempt,
    new AbortController().signal, true);
  expect(result.snapshot_id).toBe(attempt.snapshot_id);
  expect(fetch).toHaveBeenCalledOnce();
});
