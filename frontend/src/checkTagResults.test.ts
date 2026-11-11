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

it("keeps matched, trashed, and missing documents", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
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
  expect(await checkTagResults("session", request, (count) => progress.push(count))).toEqual([
    { node_id: 1, name: "/Live", revision: 4, status: "matched" },
    { node_id: 2, name: "/Trash", status: "trash" },
    { node_id: 3, name: "Document 3", status: "unavailable" },
  ]);
  expect(progress).toEqual([1, 2, 3]);
});

it("retains all documents when reads fail", async () => {
  vi.spyOn(globalThis, "fetch").mockRejectedValue(new TypeError("offline"));
  expect(await checkTagResults("session", request, () => {})).toEqual(
    request.nodes.map(({ node_id }) => ({
      node_id,
      name: `Document ${node_id}`,
      status: "unavailable",
    })),
  );
});

it.each(["inspection", "metadata"])("propagates authentication failure from %s", async (source) => {
  let reads = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(async () => {
    reads++;
    return Response.json(
      { detail: "failure" },
      { status: source === "inspection" || reads > 3 ? 401 : 404 },
    );
  });
  await expect(checkTagResults("session", request, () => {})).rejects.toMatchObject({
    status: 401,
  });
});

it("stops in-flight reads and queued documents when cancelled", async () => {
  const controller = new AbortController();
  const many = {
    ...request,
    nodes: Array.from({ length: 16 }, (_, i) => ({ node_id: i + 1, revision: 1 })),
  };
  const signals: AbortSignal[] = [];
  const fetcher = vi.spyOn(globalThis, "fetch").mockImplementation((_input, init) => {
    signals.push(init!.signal as AbortSignal);
    return new Promise<Response>((_resolve, reject) =>
      init!.signal!.addEventListener("abort", () => reject(init!.signal!.reason)),
    );
  });
  const result = checkTagResults("session", many, () => {}, controller.signal);
  controller.abort();
  await expect(result).rejects.toBeDefined();
  expect(fetcher).toHaveBeenCalledTimes(8);
  expect(signals.every((signal) => signal.aborted)).toBe(true);
});

it("cancels other reads and queued documents on authentication failure", async () => {
  const many = {
    ...request,
    nodes: Array.from({ length: 16 }, (_, i) => ({ node_id: i + 1, revision: 1 })),
  };
  const signals: AbortSignal[] = [];
  const fetcher = vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    if (String(input).endsWith("/nodes/1"))
      return Promise.resolve(Response.json({ detail: "expired" }, { status: 401 }));
    signals.push(init!.signal as AbortSignal);
    return new Promise<Response>((_resolve, reject) =>
      init!.signal!.addEventListener("abort", () => reject(init!.signal!.reason)),
    );
  });
  await expect(checkTagResults("session", many, () => {})).rejects.toMatchObject({ status: 401 });
  expect(fetcher).toHaveBeenCalledTimes(8);
  expect(signals).toHaveLength(7);
  expect(signals.every((signal) => signal.aborted)).toBe(true);
});
