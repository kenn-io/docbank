import { afterEach, expect, it, vi } from "vitest";
import { loadSelectionFrame } from "./selectionFrame.js";

const setID = "11111111-1111-4111-8111-111111111111";
const memberID = "22222222-2222-4222-8222-222222222222";
const mapSHA = "a".repeat(64);
const frameSHA = "b".repeat(64);
const draft = { set_id: setID, revision: 2, etag: 4, state: "draft", membership_sealed: false };
const resolved = { set_id: setID, revision: 2, etag: 4, member_id: memberID,
  page: { number: 1, frame_sha256: frameSHA, width: 20000, height: 10000, span: { start: 0, end: 1 } },
  map_sha256: mapSHA, recipe_sha256: "c".repeat(64), resolved_sha256: "d".repeat(64),
  review_binding: "e".repeat(64), total_boxes: 0, items: [], next_cursor: "" };

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

it("reads one exact retained page frame under the current draft ETag", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url.endsWith("/resolve")) {
      expect(init?.method).toBe("POST");
      expect(new Headers(init?.headers).get("If-Match")).toBe("4");
      expect(JSON.parse(String(init?.body))).toEqual({ member_id: memberID, page: 1, limit: 1 });
      return Response.json(resolved);
    }
    if (url.endsWith("/revisions/2")) return Response.json(draft);
    throw new Error(`unexpected request ${url}`);
  });
  expect(await loadSelectionFrame("synthetic", setID, 2, 4, memberID, mapSHA, 1,
    new AbortController().signal)).toEqual({ page: 1, sha256: frameSHA, width: 20000, height: 10000 });
});

it("rejects a frame from another map, page or draft revision", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async input => {
    const url = String(input);
    if (url.endsWith("/resolve")) return Response.json({ ...resolved, map_sha256: "f".repeat(64) });
    if (url.endsWith("/revisions/2")) return Response.json(draft);
    throw new Error(`unexpected request ${url}`);
  });
  await expect(loadSelectionFrame("synthetic", setID, 2, 4, memberID, mapSHA, 1,
    new AbortController().signal)).rejects.toThrow(/frame|map/i);
  vi.restoreAllMocks();
  vi.spyOn(globalThis, "fetch").mockImplementation(async input => {
    const url = String(input);
    if (url.endsWith("/resolve")) return Response.json({ ...resolved, page: { ...resolved.page, number: 2 } });
    if (url.endsWith("/revisions/2")) return Response.json(draft);
    throw new Error(`unexpected request ${url}`);
  });
  await expect(loadSelectionFrame("synthetic", setID, 2, 4, memberID, mapSHA, 1,
    new AbortController().signal)).rejects.toThrow(/frame|page/i);
  vi.restoreAllMocks();
  vi.spyOn(globalThis, "fetch").mockImplementation(async input => {
    const url = String(input);
    if (url.endsWith("/resolve")) return Response.json(resolved);
    if (url.endsWith("/revisions/2")) return Response.json({ ...draft, etag: 5 });
    throw new Error(`unexpected request ${url}`);
  });
  await expect(loadSelectionFrame("synthetic", setID, 2, 4, memberID, mapSHA, 1,
    new AbortController().signal)).rejects.toThrow(/changed/i);
});
