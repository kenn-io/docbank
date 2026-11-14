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

it("checks readable documents in one preview", async () => {
  const previews: unknown[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url.endsWith(`/tags/${request.tag_id}`))
      return Response.json({ id: request.tag_id, name: "Review" });
    if (url.endsWith("/batch/tags/preview")) {
      const { nodes } = JSON.parse(String(init?.body));
      previews.push(nodes);
      return Response.json({
        tag_id: request.tag_id,
        tag_revision: 2,
        nodes: nodes.map((node: object) => ({ ...node, assigned: true })),
      });
    }
    const id = Number(url.match(/nodes\/(\d+)/)?.[1]);
    return Response.json({ id, name: `File ${id}`, revision: id + 1 });
  });
  const { rows } = await checkTagResults("session", request, () => {});
  expect(rows.map((row) => row.status)).toEqual(["matched", "matched", "matched"]);
  expect(previews).toEqual([
    [
      { node_id: 1, revision: 2 },
      { node_id: 2, revision: 3 },
      { node_id: 3, revision: 4 },
    ],
  ]);
});

it("keeps matched and unavailable documents", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
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
    if (url.endsWith("/batch/tags/preview")) {
      const { nodes } = JSON.parse(String(init?.body));
      if (nodes.some((node: { node_id: number }) => node.node_id === 2))
        return Response.json({ code: "not_found" }, { status: 404 });
      return Response.json({
        tag_id: request.tag_id,
        tag_revision: 2,
        nodes: nodes.map((node: object) => ({ ...node, assigned: true })),
      });
    }
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
      : new Promise((resolve, reject) => {
          init?.signal?.addEventListener("abort", () => {
            if (String(input).endsWith("/nodes/1"))
              resolve(Response.json({ id: 1, name: "File 1", revision: 1 }));
            else reject(init.signal?.reason);
          });
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
  let previews = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url.endsWith(`/tags/${request.tag_id}`))
      return Response.json({ id: request.tag_id, name: "Review" });
    if (url.endsWith("/batch/tags/preview")) {
      previews++;
      const { nodes } = JSON.parse(String(init?.body));
      return Response.json({
        tag_id: request.tag_id,
        tag_revision: 2,
        nodes: nodes.map((node: { node_id: number }) => ({
          ...node,
          assigned: node.node_id === 1,
        })),
      });
    }
    const id = Number(url.match(/nodes\/(\d+)/)?.[1]);
    return Response.json({ id, name: `File ${id}`, revision: id + 1 });
  });
  const { rows } = await checkTagResults("session", { ...request, assign: false }, () => {});
  expect(rows.map((row) => [row.node_id, row.status, row.revision])).toEqual([
    [1, "remaining", 2],
    [2, "matched", 3],
    [3, "matched", 4],
  ]);
  expect(previews).toBe(1);
});

it("falls back to per-document previews so one stale document keeps the other results", async () => {
  const previews: number[][] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url.endsWith(`/tags/${request.tag_id}`))
      return Response.json({ id: request.tag_id, name: "Review" });
    if (url.endsWith("/batch/tags/preview")) {
      const { nodes } = JSON.parse(String(init?.body));
      previews.push(nodes.map((node: { node_id: number }) => node.node_id));
      if (nodes.length > 1 || nodes[0].node_id === 2)
        return Response.json({ code: "stale_revision" }, { status: 409 });
      return Response.json({
        tag_id: request.tag_id,
        tag_revision: 2,
        nodes: [{ ...nodes[0], assigned: true }],
      });
    }
    const id = Number(url.match(/nodes\/(\d+)/)?.[1]);
    return Response.json({ id, name: `File ${id}`, revision: id + 1 });
  });
  const { rows } = await checkTagResults("session", request, () => {});
  expect(rows.map((row) => [row.node_id, row.status])).toEqual([
    [1, "matched"],
    [2, "unavailable"],
    [3, "matched"],
  ]);
  expect(previews).toEqual([[1, 2, 3], [1], [2], [3]]);
});
