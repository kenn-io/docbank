// @vitest-environment node
import { afterEach, expect, it, vi } from "vite-plus/test";
import { checkTagResults } from "./checkTagResults.js";
import type { BatchTagRequest } from "./batch-tags.js";

const request: BatchTagRequest = {
  operation_id: "11111111-1111-4111-8111-111111111111",
  tag_id: "22222222-2222-4222-8222-222222222222",
  assign: true,
  nodes: [
    { node_id: 1, revision: 1 },
    { node_id: 2, revision: 1 },
    { node_id: 3, revision: 1 },
  ],
};
afterEach(() => vi.restoreAllMocks());

it("reads all tag pages and excludes missing and trashed documents", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.includes("/nodes/2"))
      return Response.json({ id: 2, name: "Trash", revision: 2, trashed_at: "2026-01-01" });
    if (url.includes("/nodes/3")) return Response.json({ detail: "Missing" }, { status: 404 });
    if (url.includes("/tags")) {
      const offset = Number(new URL(url, "http://example.test").searchParams.get("offset"));
      const items =
        offset === 0
          ? Array.from({ length: 1000 }, (_, i) => ({ id: `tag-${i}` }))
          : [{ id: request.tag_id }];
      return Response.json({ items, total: 1001, limit: 1000, offset });
    }
    return Response.json({ id: 1, name: "Live", revision: 4 });
  });
  const result = await checkTagResults("session", request, () => {});
  expect(result).toEqual({
    complete: true,
    rows: [
      { node_id: 1, name: "Live", revision: 4, status: "matched" },
      { node_id: 2, name: "Trash", status: "trash" },
      { node_id: 3, name: "Document 3", status: "unavailable" },
    ],
  });
  expect(fetch.mock.calls.some(([url]) => String(url).includes("offset=1000"))).toBe(true);
});

it("blocks retry when a read fails or changes while tags load", async () => {
  let reads = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.includes("/nodes/2")) throw new TypeError("Network error");
    if (url.includes("/nodes/3")) return Response.json({ detail: "Missing" }, { status: 404 });
    if (url.includes("/tags"))
      return Response.json({ items: [], total: 0, limit: 1000, offset: 0 });
    return Response.json({ id: 1, name: "Live", revision: ++reads < 3 ? 4 : 5 });
  });
  const result = await checkTagResults("session", request, () => {});
  expect(result.complete).toBe(false);
  expect(result.rows.map((row) => row.status)).toEqual([
    "unavailable",
    "unavailable",
    "unavailable",
  ]);
});
