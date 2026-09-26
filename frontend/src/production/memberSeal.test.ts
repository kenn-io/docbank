import { afterEach, expect, it, vi } from "vitest";
import { prepareProductionMemberSeal } from "./memberSeal.js";
import { sealProductionMembership, type ProductionDraft, type ProductionMember } from "./api.js";

const setID = "11111111-1111-4111-8111-111111111111";
const operationID = "22222222-2222-4222-8222-222222222222";
const memberHash = "a".repeat(64);
const draft: ProductionDraft = { set_id: setID, revision: 2, etag: 4, state: "draft",
  membership_sealed: false, member_hash: memberHash };
const member: ProductionMember = { id: "33333333-3333-4333-8333-333333333333", ordinal: 1, node_id: 7,
  source_version_id: "44444444-4444-4444-8444-444444444444", pdf_sha256: "b".repeat(64), pdf_size: 100,
  map_sha256: "c".repeat(64), mode: "redact_selected", reviewed: false };

afterEach(() => { vi.restoreAllMocks(); });

it("checks the complete list and current draft before sealing with an exact ETag", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url.endsWith("/revisions/2")) return Response.json(draft);
    if (url.endsWith("/seal")) {
      expect(new Headers(init?.headers).get("If-Match")).toBe("4");
      expect(JSON.parse(String(init?.body))).toEqual({ operation_id: operationID, total: 1, member_hash: memberHash });
      return Response.json({ operation_id: operationID, set_id: setID, revision: 2, etag: 5 });
    }
    throw new Error(`unexpected request ${url}`);
  });
  const evidence = await prepareProductionMemberSeal("synthetic", draft, [member], "", new AbortController().signal);
  expect(evidence).toEqual({ total: 1, memberHash });
  const receipt = await sealProductionMembership("synthetic", setID, 2, 4, evidence.total, evidence.memberHash, operationID);
  expect(receipt.etag).toBe(5);
  expect(fetch).toHaveBeenCalledTimes(2);
});

it("rejects incomplete, duplicate, or stale membership before writing", async () => {
  const signal = new AbortController().signal;
  const fetch = vi.spyOn(globalThis, "fetch");
  await expect(prepareProductionMemberSeal("synthetic", draft, [member], "next", signal)).rejects.toThrow(/all members/i);
  await expect(prepareProductionMemberSeal("synthetic", draft, [member, member], "", signal)).rejects.toThrow(/ordered|duplicate/i);
  expect(fetch).not.toHaveBeenCalled();
  fetch.mockResolvedValueOnce(Response.json({ ...draft, etag: 5 }));
  await expect(prepareProductionMemberSeal("synthetic", draft, [member], "", signal)).rejects.toThrow(/changed/i);
});
