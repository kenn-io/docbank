import { afterEach, expect, it, vi } from "vitest";
import { forkCurrentProductionDraft, type ForkAttempt } from "./forkDraft.js";
import type { ProductionDraft, ProductionSet } from "./api.js";

const set: ProductionSet = { id: "11111111-1111-4111-8111-111111111111", name: "Synthetic final",
  creator: "synthetic", created_at: "2026-09-27T00:00:00Z", head_revision: 2 };
const draft: ProductionDraft = { set_id: set.id, revision: 2, etag: 5, state: "finalized",
  membership_sealed: true, member_hash: "a".repeat(64), numbering_recipe_id: "bates-sequential-v1" };
const attempt: ForkAttempt = { set_id: set.id, revision: 2, etag: 5,
  operation_id: "22222222-2222-4222-8222-222222222222" };
const forked: ProductionDraft = { ...draft, revision: 3, etag: 1, state: "draft", membership_sealed: false };

afterEach(() => vi.restoreAllMocks());

it("forks only the exact current finalized revision", async () => {
  const requests: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input); requests.push(url);
    if (url === `/api/v1/productions/sets/${set.id}`) return Response.json(set);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2`) return Response.json(draft);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2/fork`) {
      expect(init?.method).toBe("POST");
      expect(JSON.parse(String(init?.body))).toEqual({ operation_id: attempt.operation_id });
      return Response.json(forked, { status: 201 });
    }
    throw new Error(`unexpected ${url}`);
  });
  await expect(forkCurrentProductionDraft("synthetic", set, draft, attempt,
    new AbortController().signal)).resolves.toEqual(forked);
  expect(requests).toHaveLength(3);
});

it("rejects a stale or unfinalized source before the fork write", async () => {
  const fetch = vi.spyOn(globalThis, "fetch");
  const signal = new AbortController().signal;
  await expect(forkCurrentProductionDraft("synthetic", set, { ...draft, state: "draft" }, attempt, signal))
    .rejects.toThrow(/finalized/i);
  expect(fetch).not.toHaveBeenCalled();
  fetch.mockResolvedValueOnce(Response.json({ ...set, head_revision: 3 }));
  await expect(forkCurrentProductionDraft("synthetic", set, draft, attempt, signal))
    .rejects.toThrow(/changed/i);
  expect(fetch).toHaveBeenCalledOnce();
});

it("replays the frozen fork after a lost response even when the head changed", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    expect(String(input)).toBe(`/api/v1/productions/sets/${set.id}/revisions/2/fork`);
    expect(JSON.parse(String(init?.body))).toEqual({ operation_id: attempt.operation_id });
    return Response.json(forked, { status: 201 });
  });
  const result = await forkCurrentProductionDraft("synthetic", set, draft, attempt,
    new AbortController().signal, true);
  expect(result.revision).toBe(3);
  expect(fetch).toHaveBeenCalledOnce();
});
