// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  batchTagRequestDigest, changeBatchTags, previewBatchTags,
  validateBatchTagReceipt, type BatchTagRequest, type BatchTagReceipt,
} from "./batch-tags.js";

const request: BatchTagRequest = {
  operation_id: "11111111-1111-4111-8111-111111111111",
  tag_id: "22222222-2222-4222-8222-222222222222",
  assign: true,
  nodes: [{ node_id: 9, revision: 4 }, { node_id: 7, revision: 3 }],
};
const receipt: BatchTagReceipt = {
  version: 1, operation_id: request.operation_id, tag_id: request.tag_id, assign: true,
  request_digest: "9d29429ce1a103db51351b4d136306a00fb8dc77ae392b506ba09e625073c725",
  tag_revision: 3, assignment_count: 2, completed_at: "2026-09-11T00:00:00.000000000Z",
  nodes: [
    { node_id: 7, expected_revision: 3, revision: 4, changed: true },
    { node_id: 9, expected_revision: 4, revision: 4, changed: false },
  ],
};

afterEach(() => vi.restoreAllMocks());

describe("batch tag request and receipt identity", () => {
  it("uses the cross-language digest grammar independent of display order", async () => {
    expect(await batchTagRequestDigest(request)).toBe(receipt.request_digest);
    expect(await batchTagRequestDigest({ ...request, nodes: [...request.nodes].reverse() })).toBe(receipt.request_digest);
    expect(await batchTagRequestDigest({ ...request, assign: false })).not.toBe(receipt.request_digest);
    expect(request.nodes[0].node_id).toBe(9);
  });

  it("accepts complete changed and no-op results", async () => {
    await expect(validateBatchTagReceipt(request, receipt)).resolves.toEqual(receipt);
  });

  it.each([
    { ...receipt, nodes: [] },
    { ...receipt, nodes: [receipt.nodes[0], receipt.nodes[0]] },
    { ...receipt, nodes: [{ ...receipt.nodes[0], node_id: 8 }, receipt.nodes[1]] },
    { ...receipt, nodes: [{ ...receipt.nodes[0], revision: 3 }, receipt.nodes[1]] },
    { ...receipt, nodes: [receipt.nodes[0], { ...receipt.nodes[1], revision: 5 }] },
    { ...receipt, assign: false },
    { ...receipt, request_digest: "0".repeat(64) },
    { ...receipt, operation_id: request.tag_id },
    { ...receipt, tag_id: request.operation_id },
    { ...receipt, tag_revision: Number.MAX_SAFE_INTEGER + 1 },
    { ...receipt, assignment_count: -1 },
    { ...receipt, completed_at: "2026-02-30T00:00:00.000000000Z" },
    { ...receipt, version: 2 },
    null,
  ])("rejects incomplete or substituted receipts %#", async (invalid) => {
    await expect(validateBatchTagReceipt(request, invalid)).rejects.toThrow();
  });

  it("rejects malformed requests before egress", async () => {
    const fetch = vi.spyOn(globalThis, "fetch");
    for (const nodes of [[], [request.nodes[0], request.nodes[0]], [{ node_id: 7, revision: 0 }], [{ node_id: Number.MAX_SAFE_INTEGER + 1, revision: 1 }]]) {
      await expect(changeBatchTags("session", { ...request, nodes })).rejects.toThrow();
    }
    expect(fetch).not.toHaveBeenCalled();
  });

  it("keeps the exact request available after response loss", async () => {
    const fetch = vi.spyOn(globalThis, "fetch")
      .mockRejectedValueOnce(new TypeError("Synthetic response loss"))
      .mockResolvedValueOnce(new Response(JSON.stringify(receipt), { status: 200 }));
    await expect(changeBatchTags("session", request)).rejects.toThrow("Synthetic response loss");
    await expect(changeBatchTags("session", request)).resolves.toEqual(receipt);
    expect(fetch.mock.calls[0][1]?.body).toBe(fetch.mock.calls[1][1]?.body);
    expect(new Headers(fetch.mock.calls[1][1]?.headers).get("Content-Type")).toBe("application/json");
    expect(JSON.parse(String(fetch.mock.calls[1][1]?.body)).operation_id).toBe(request.operation_id);
  });

  it("refuses partial preview membership", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({
      tag_id: request.tag_id, tag_revision: 3,
      nodes: [{ node_id: 7, revision: 3, assigned: true }],
    }), { status: 200 }));
    await expect(previewBatchTags("session", request.tag_id, request.nodes)).rejects.toThrow();
  });

  it("returns exact mixed preview identities", async () => {
    const preview = { tag_id: request.tag_id, tag_revision: 3, nodes: [
      { node_id: 7, revision: 3, assigned: true },
      { node_id: 9, revision: 4, assigned: false },
    ] };
    const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify(preview), { status: 200 }));
    await expect(previewBatchTags("session", request.tag_id, request.nodes)).resolves.toEqual(preview);
    expect(new Headers(fetch.mock.calls[0][1]?.headers).get("Content-Type")).toBe("application/json");
  });
});
