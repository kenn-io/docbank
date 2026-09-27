import { afterEach, expect, it, vi } from "vitest";
import { loadCurrentMemberAppend } from "./memberAppend.js";
import { appendProductionMember, type ProductionDraft, type ProductionPreparedMember } from "./api.js";

const setID = "11111111-1111-4111-8111-111111111111";
const sourceID = "22222222-2222-4222-8222-222222222222";
const nextID = "33333333-3333-4333-8333-333333333333";
const operationID = "44444444-4444-4444-8444-444444444444";
const versionID = "55555555-5555-4555-8555-555555555555";
const draft: ProductionDraft = { set_id: setID, revision: 2, etag: 4, state: "draft", membership_sealed: false,
  member_hash: "a".repeat(64) };
const source: ProductionPreparedMember = { id: sourceID, vault_id: "66666666-6666-4666-8666-666666666666",
  source_version_id: versionID, source_sha256: "b".repeat(64), source_size: 14,
  pdf_sha256: "c".repeat(64), pdf_size: 28, node_id: 7, ordinal: 1,
  family: { kind: "standalone", root_version_id: versionID }, mode: "redact_selected",
  map_sha256: "d".repeat(64), page_inventory_sha256: "e".repeat(64),
  reviewed: true, review_binding: "f".repeat(64) };

afterEach(() => { vi.restoreAllMocks(); });

it("adds a new occurrence with exact retained source authority and no inherited review", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url.endsWith("/revisions/2")) return Response.json(draft);
    if (url.endsWith("/members")) {
      expect(new Headers(init?.headers).get("If-Match")).toBe("4");
      const body = JSON.parse(String(init?.body));
      expect(body).toEqual({ operation_id: operationID, members: [{ ...source, id: nextID, ordinal: 2,
        reviewed: false, review_binding: "" }] });
      return Response.json({ operation_id: operationID, set_id: setID, revision: 2, etag: 5 });
    }
    throw new Error(`unexpected request ${url}`);
  });
  const member = await loadCurrentMemberAppend("synthetic", draft, [source], "", source, nextID,
    new AbortController().signal);
  expect(member.id).toBe(nextID);
  expect(member.source_version_id).toBe(versionID);
  expect(member.reviewed).toBe(false);
  const receipt = await appendProductionMember("synthetic", setID, 2, 4, member, operationID);
  expect(receipt.etag).toBe(5);
  expect(fetch).toHaveBeenCalledTimes(2);
});

it("rejects incomplete or stale target membership before appending", async () => {
  const signal = new AbortController().signal;
  const fetch = vi.spyOn(globalThis, "fetch");
  await expect(loadCurrentMemberAppend("synthetic", draft, [source], "next", source, nextID, signal))
    .rejects.toThrow(/all.*members/i);
  await expect(loadCurrentMemberAppend("synthetic", draft, [source, source], "", source, nextID, signal))
    .rejects.toThrow(/ordered|duplicate/i);
  await expect(loadCurrentMemberAppend("synthetic", draft, [], "", { ...source, map_sha256: "bad" }, nextID, signal))
    .rejects.toThrow(/source/i);
  expect(fetch).not.toHaveBeenCalled();
  fetch.mockResolvedValueOnce(Response.json({ ...draft, etag: 5 }));
  await expect(loadCurrentMemberAppend("synthetic", draft, [source], "", source, nextID, signal))
    .rejects.toThrow(/changed/i);
});
