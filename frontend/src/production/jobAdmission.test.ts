import { afterEach, expect, it, vi } from "vitest";
import { admitCurrentProductionJob } from "./jobAdmission.js";
import type { ProductionDraft, ProductionSet } from "./api.js";

const set: ProductionSet = { id: "11111111-1111-4111-8111-111111111111", name: "Synthetic finalized",
  creator: "synthetic", created_at: "2026-09-27T00:00:00Z", head_revision: 2 };
const draft: ProductionDraft = { set_id: set.id, revision: 2, etag: 5, state: "finalized",
  membership_sealed: true, member_hash: "a".repeat(64) };
const jobID = "22222222-2222-4222-8222-222222222222";
const operationID = "33333333-3333-4333-8333-333333333333";

afterEach(() => vi.restoreAllMocks());

it("admits one job from the exact finalized revision", async () => {
  const calls: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input); calls.push(url);
    if (url.endsWith(`/sets/${set.id}`)) return Response.json(set);
    if (url.endsWith(`/sets/${set.id}/revisions/2`)) return Response.json(draft);
    if (url.endsWith(`/sets/${set.id}/revisions/2/jobs`)) {
      expect(init?.method).toBe("POST");
      expect(new Headers(init?.headers).get("If-Match")).toBe("5");
      expect(JSON.parse(String(init?.body))).toEqual({ job_id: jobID, operation_id: operationID });
      return Response.json({ job_id: jobID, set_id: set.id, revision: 2,
        state: "queued", revision_sha256: "b".repeat(64) });
    }
    throw new Error(`unexpected ${url}`);
  });
  const status = await admitCurrentProductionJob("synthetic", set, draft, jobID, operationID,
    new AbortController().signal);
  expect(status.job_id).toBe(jobID);
  expect(calls).toHaveLength(3);
});

it("blocks an unfinalized or changed revision before a write", async () => {
  const signal = new AbortController().signal;
  const fetch = vi.spyOn(globalThis, "fetch");
  await expect(admitCurrentProductionJob("synthetic", set, { ...draft, state: "draft" },
    jobID, operationID, signal)).rejects.toThrow(/finalized/i);
  expect(fetch).not.toHaveBeenCalled();
  fetch.mockResolvedValueOnce(Response.json({ ...set, head_revision: 3 }));
  await expect(admitCurrentProductionJob("synthetic", set, draft, jobID, operationID, signal))
    .rejects.toThrow(/changed/i);
  expect(fetch).toHaveBeenCalledTimes(1);
});
