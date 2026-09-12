import { afterEach, expect, it, vi } from "vitest";
import { appendRelationPage, readRelatedSource, readRelationPage, validatePublication, validateRelationPage, type AttachmentIdentity } from "./attachments.js";

export const parent: AttachmentIdentity = { node_id: 1, version_id: "11111111-1111-4111-8111-111111111111", sha256: "a".repeat(64), size: 12 };
export const child: AttachmentIdentity = { node_id: 2, version_id: "22222222-2222-4222-8222-222222222222", sha256: "b".repeat(64), size: 4 };
export const relation = (order = 1) => ({ operation_id: "publication-1", order, parent, generation_id: "c".repeat(64), attachment_id: "d".repeat(64), part_path: `1.${order + 1}`, sibling_order: order, filename: "same.txt", outcome: "decoded", child });
const item = (order = 1) => ({ relation: relation(order), state: "pending", reason: "text_extraction" });
const page = (items = [item()]) => ({ items, total: items.length, next_operation_id: "", next_order: 0 });
afterEach(() => vi.restoreAllMocks());

it("preserves equal payloads and names as ordered occurrences across explicit pages", () => {
  const first = validateRelationPage({ ...page([item(1), item(2)]), total: 3, next_operation_id: "publication-1", next_order: 2 }, parent, "outgoing", undefined, 2);
  const next = validateRelationPage({ ...page([item(3)]), total: 3 }, parent, "outgoing", first.next, 2);
  expect(appendRelationPage(first, next).items.map((entry) => entry.relation.order)).toEqual([1, 2, 3]);
  expect(first.items).toHaveLength(2);
});

it("rejects wrong selected identities, nonmonotonic pages and changing totals", () => {
  expect(() => validateRelationPage(page(), child, "outgoing")).toThrow(/identity/i);
  expect(() => validateRelationPage(page([item(2), item(1)]), parent, "outgoing")).toThrow(/order/i);
  expect(() => validateRelationPage(page(), parent, "outgoing", { operationID: "publication-1", order: 1 })).toThrow(/order/i);
  const first = validateRelationPage({ ...page(), total: 2, next_operation_id: "publication-1", next_order: 1 }, parent, "outgoing", undefined, 1);
  const second = validateRelationPage({ ...page([item(2)]), total: 3, next_operation_id: "publication-1", next_order: 2 }, parent, "outgoing", first.next, 1);
  expect(() => appendRelationPage(first, second)).toThrow(/changed/i);
  expect(() => validateRelationPage({ ...page(), next_operation_id: "publication-2", next_order: 1 }, parent, "outgoing")).toThrow();
});

it("keeps partial inventory independent of live processing and checks receipt agreement", () => {
  const selected = validateRelationPage(page(), child, "incoming");
  const receipt = { operation_id: "publication-1", request_digest: "e".repeat(64), created_at: "2026-09-12T00:00:00Z", inventory_state: "partial", relations: [relation()] };
  expect(validatePublication(receipt, selected.items[0].relation).inventory_state).toBe("partial");
  expect(() => validatePublication({ ...receipt, relations: [{ ...relation(), child: { ...child, sha256: "f".repeat(64) } }] }, selected.items[0].relation)).toThrow(/agree/i);
  expect(() => validatePublication({ ...receipt, inventory_state: "pending" }, selected.items[0].relation)).toThrow();
});

it("uses exact version metadata and current authorization without adopting the newer head", async () => {
  const requests: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const route = String(input); requests.push(route);
    if (route === `/api/v1/versions/${child.version_id}`) return Response.json({ id: child.version_id, node_id: child.node_id, blob_hash: child.sha256, size: child.size, mime_type: "text/plain", recorded_at: "2026-09-12T00:00:00Z" });
    return Response.json({ id: child.node_id, kind: "file", revision: 9, name: "renamed.bin", path: "/renamed.bin", mime_type: "application/octet-stream", current_version_id: "33333333-3333-4333-8333-333333333333", blob_hash: "f".repeat(64), size: 100, modified_at: "2026-09-12T01:00:00Z" });
  });
  const selected = await readRelatedSource("session", child, new AbortController().signal);
  expect(selected).toMatchObject({ kind: "related", nodeID: 2, versionID: child.version_id, blobHash: child.sha256, size: 4, mimeType: "text/plain", mutationRevision: 9 });
  expect(requests).toEqual([`/api/v1/versions/${child.version_id}`, "/api/v1/nodes/2"]);
});

it("rejects missing and denied targets, mismatched versions, and oversized responses", async () => {
  const fetch = vi.spyOn(globalThis, "fetch");
  fetch.mockResolvedValueOnce(Response.json({ detail: "gone" }, { status: 404 }));
  await expect(readRelatedSource("session", child, new AbortController().signal)).rejects.toMatchObject({ status: 404 });
  fetch.mockResolvedValueOnce(Response.json({ detail: "denied" }, { status: 403 }));
  await expect(readRelatedSource("session", child, new AbortController().signal)).rejects.toMatchObject({ status: 403 });
  fetch.mockResolvedValueOnce(Response.json({ id: parent.version_id, node_id: 2, blob_hash: child.sha256, size: 4 }));
  await expect(readRelatedSource("session", child, new AbortController().signal)).rejects.toThrow(/identity/i);
  fetch.mockResolvedValueOnce(new Response(" ".repeat(2 * 1024 * 1024 + 1)));
  await expect(readRelationPage("session", parent, "outgoing", new AbortController().signal)).rejects.toThrow(/limit/i);
});
