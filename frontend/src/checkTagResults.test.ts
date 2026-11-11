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

it("previews live documents and retains missing and trashed rows", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url.includes("preview")) {
      expect(JSON.parse(String(init?.body)).nodes).toEqual([{ node_id: 1, revision: 4 }]);
      return Response.json({
        tag_id: request.tag_id,
        tag_revision: 2,
        nodes: [{ node_id: 1, revision: 4, assigned: true }],
      });
    }
    if (url.includes("/nodes/2"))
      return Response.json({ id: 2, name: "Trash", revision: 2, trashed_at: "2026-01-01" });
    if (url.includes("/nodes/3")) return Response.json({ detail: "Missing" }, { status: 404 });
    return Response.json({ id: 1, name: "Live", revision: 4 });
  });
  expect(await checkTagResults("session", request, () => {})).toEqual([
    { node_id: 1, name: "Live", revision: 4, status: "matched" },
    { node_id: 2, name: "Trash", status: "trash" },
    { node_id: 3, name: "Document 3", status: "unavailable" },
  ]);
  expect(fetch).toHaveBeenCalledTimes(4);
});

it("keeps candidates unchecked when the preview rejects changed revisions", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    if (String(input).includes("preview"))
      return Response.json({ code: "stale_revision" }, { status: 409 });
    const id = Number(String(input).match(/nodes\/(\d+)/)?.[1]);
    return Response.json({ id, name: `Document ${id}`, revision: 4 });
  });
  const rows = await checkTagResults("session", request, () => {});
  expect(rows).toEqual(
    request.nodes.map(({ node_id }) => ({
      node_id,
      name: `Document ${node_id}`,
      status: "unavailable",
    })),
  );
});

it("skips the preview when no documents can be read", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockRejectedValue(new TypeError("offline"));
  const rows = await checkTagResults("session", request, () => {});
  expect(rows.every((row) => row.status === "unavailable")).toBe(true);
  expect(fetch).toHaveBeenCalledTimes(3);
});

it.each(["node", "preview"])("propagates authentication failure from %s", async (source) => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    if (source === "node" || String(input).includes("preview"))
      return Response.json({ detail: "Unauthorized" }, { status: 401 });
    const id = Number(String(input).match(/nodes\/(\d+)/)?.[1]);
    return Response.json({ id, name: `Document ${id}`, revision: 4 });
  });
  await expect(checkTagResults("session", request, () => {})).rejects.toMatchObject({
    status: 401,
  });
});
