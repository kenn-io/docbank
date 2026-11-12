// @vitest-environment node
import { afterEach, expect, it, vi } from "vite-plus/test";
import { checkTagResults } from "./checkTagResults.js";
import type { BatchTagRequest } from "./batch-tags.js";
import type { SelectionTarget } from "./selection.js";

const request: BatchTagRequest & { nodes: SelectionTarget[] } = {
  operation_id: "11111111-1111-4111-8111-111111111111",
  tag_id: "22222222-2222-4222-8222-222222222222",
  assign: true,
  nodes: [
    { node_id: 1, revision: 1, name: "Live" },
    { node_id: 2, revision: 1, name: "Trash" },
    { node_id: 3, revision: 1, name: "Missing" },
  ],
};
afterEach(() => vi.restoreAllMocks());

it("keeps matched and unavailable documents", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.endsWith(`/tags/${request.tag_id}`))
      return Response.json({ id: request.tag_id, name: "Review" });
    if (url.includes("/nodes/2"))
      return Response.json({
        id: 2,
        name: "Trash",
        path: "/Trash",
        revision: 2,
        trashed_at: "2026-01-01",
      });
    if (url.includes("/nodes/3")) return Response.json({ detail: "Missing" }, { status: 404 });
    if (url.includes("/tags?"))
      return Response.json({ items: [{ id: request.tag_id }], total: 1, limit: 1000, offset: 0 });
    return Response.json({ id: 1, name: "Live", path: "/Live", revision: 4 });
  });
  const progress: number[] = [];
  expect((await checkTagResults("session", request, (count) => progress.push(count))).rows).toEqual(
    [
      { node_id: 1, name: "/Live", revision: 4, status: "matched" },
      { node_id: 2, name: "Trash", status: "unavailable" },
      { node_id: 3, name: "Missing", status: "unavailable" },
    ],
  );
  expect(progress).toEqual([1, 2, 3]);

  vi.restoreAllMocks();
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    if (String(input).endsWith(`/tags/${request.tag_id}`))
      return Response.json({ id: request.tag_id, name: "Review" });
    throw new TypeError("offline");
  });
  expect((await checkTagResults("session", request, () => {})).rows).toEqual([
    { node_id: 1, name: "Live", status: "unavailable" },
    { node_id: 2, name: "Trash", status: "unavailable" },
    { node_id: 3, name: "Missing", status: "unavailable" },
  ]);
});

it("propagates authentication failure", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) =>
    String(input).endsWith(`/tags/${request.tag_id}`)
      ? Response.json({ id: request.tag_id, name: "Review" })
      : Response.json({ detail: "expired" }, { status: 401 }),
  );
  await expect(checkTagResults("session", request, () => {})).rejects.toMatchObject({
    status: 401,
  });
});

it("stops pending reads and schedules no more documents when canceled", async () => {
  const controller = new AbortController();
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation((input, init) =>
    String(input).endsWith(`/tags/${request.tag_id}`)
      ? Promise.resolve(Response.json({ id: request.tag_id, name: "Review" }))
      : new Promise((_resolve, reject) => {
          init?.signal?.addEventListener("abort", () => reject(init.signal?.reason));
        }),
  );
  const many = {
    ...request,
    nodes: Array.from({ length: 20 }, (_, i) => ({
      node_id: i + 1,
      revision: 1,
      name: `File ${i + 1}`,
    })),
  };
  const checking = checkTagResults("session", many, () => {}, controller.signal);
  await vi.waitFor(() => expect(fetch).toHaveBeenCalledTimes(9));
  controller.abort();
  await expect(checking).rejects.toMatchObject({ name: "AbortError" });
  expect(fetch).toHaveBeenCalledTimes(9);
});

it("classifies removal results", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.endsWith(`/tags/${request.tag_id}`))
      return Response.json({ id: request.tag_id, name: "Review" });
    const id = Number(url.match(/nodes\/(\d+)/)?.[1]);
    if (url.includes("/tags?")) {
      const items = id === 1 ? [{ id: request.tag_id }] : [];
      return Response.json({ items, total: items.length, limit: 1000, offset: 0 });
    }
    return Response.json({ id, name: `File ${id}`, revision: id + 1 });
  });
  const { rows } = await checkTagResults("session", { ...request, assign: false }, () => {});
  expect(rows.map((row) => [row.node_id, row.status, row.revision])).toEqual([
    [1, "remaining", 2],
    [2, "matched", 3],
    [3, "matched", 4],
  ]);
});
