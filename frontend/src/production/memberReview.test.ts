import { afterEach, expect, it, vi } from "vitest";
import { loadCurrentMemberReviewBinding } from "./memberReview.js";
import { reviewProductionMember, type ProductionMember } from "./api.js";

const setID = "11111111-1111-4111-8111-111111111111";
const memberID = "22222222-2222-4222-8222-222222222222";
const operationID = "33333333-3333-4333-8333-333333333333";
const mapSHA = "a".repeat(64);
const binding = "b".repeat(64);
const member: ProductionMember = { id: memberID, ordinal: 1, node_id: 7,
  source_version_id: "44444444-4444-4444-8444-444444444444", pdf_sha256: "c".repeat(64), pdf_size: 100,
  map_sha256: mapSHA, mode: "redact_selected", reviewed: false };
const resolved = { set_id: setID, revision: 2, etag: 4, member_id: memberID, map_sha256: mapSHA,
  review_binding: binding, page: { number: 1, frame_sha256: "d".repeat(64), width: 10000, height: 10000 } };

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

it("loads a current member-wide review binding and records it with the same draft ETag", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url.endsWith("/resolve")) {
      expect(new Headers(init?.headers).get("If-Match")).toBe("4");
      expect(JSON.parse(String(init?.body))).toEqual({ member_id: memberID, page: 1, limit: 1 });
      return Response.json(resolved);
    }
    if (url.endsWith("/revisions/2")) return Response.json({ set_id: setID, revision: 2, etag: 4, state: "draft" });
    if (url.endsWith("/review")) {
      expect(new Headers(init?.headers).get("If-Match")).toBe("4");
      expect(JSON.parse(String(init?.body))).toEqual({ operation_id: operationID, binding, complete: true });
      return Response.json({ set_id: setID, revision: 2, etag: 5, operation_id: operationID });
    }
    throw new Error(`unexpected request ${url}`);
  });
  const signal = new AbortController().signal;
  const checked = await loadCurrentMemberReviewBinding("synthetic", setID, 2, 4, member, signal);
  expect(checked).toBe(binding);
  const receipt = await reviewProductionMember("synthetic", setID, 2, 4, memberID, checked, operationID, signal);
  expect(receipt.etag).toBe(5);
  expect(fetch.mock.calls).toHaveLength(3);
});

it("rejects a binding for another map or a changed draft before a review write", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(Response.json({ ...resolved, map_sha256: "e".repeat(64) }));
  await expect(loadCurrentMemberReviewBinding("synthetic", setID, 2, 4, member,
    new AbortController().signal)).rejects.toThrow(/member|map|binding/i);
  expect(fetch).toHaveBeenCalledTimes(1);
  vi.restoreAllMocks();
  vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(Response.json(resolved))
    .mockResolvedValueOnce(Response.json({ set_id: setID, revision: 2, etag: 5, state: "draft" }));
  await expect(loadCurrentMemberReviewBinding("synthetic", setID, 2, 4, member,
    new AbortController().signal)).rejects.toThrow(/changed/i);
});
