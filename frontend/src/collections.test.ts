import { afterEach, expect, it, vi } from "vitest";
import { APIError } from "./api.js";
import { collectionByID, collectionLabel, collectionMembers, collections, setCollectionLabel } from "./collections.js";

const id = "11111111-1111-4111-8111-111111111111";
const otherID = "22222222-2222-4222-8222-222222222222";
const collection = {
  id, source_kind: "cli", source_description: "Synthetic proposals", started_at: "2026-01-01T00:00:00Z",
  file_count: 2, total_bytes: 12, label: "Proposals", label_revision: 3, label_updated_at: "2026-01-02T00:00:00Z",
};
const label = { ingest_id: id, label: "Proposals", revision: 3, updated_at: "2026-01-02T00:00:00Z" };
const node = {
  id: 2, parent_id: 1, name: "alpha.txt", kind: "file", path: "/alpha.txt", current_version_id: otherID,
  blob_hash: "a".repeat(64), size: 5, revision: 1, created_at: "2026-01-01T00:00:00Z", modified_at: "2026-01-01T00:00:00Z",
};
function respond(value: unknown, etag = '"3"') {
  return vi.spyOn(globalThis, "fetch").mockImplementation(async () => new Response(JSON.stringify(value), { headers: { ETag: etag } }));
}
afterEach(() => vi.restoreAllMocks());

it("lists a bounded page without replacing collection totals with page counts", async () => {
  const fetch = respond({ items: [collection], total: 3, limit: 1, offset: 1 });
  const page = await collections("session", 1, 1);
  expect(page.total).toBe(3);
  expect(page.items[0].file_count).toBe(2);
  expect(page.items[0].total_bytes).toBe(12);
  expect(String(fetch.mock.calls[0][0])).toBe("/api/v1/collections?limit=1&offset=1");
});

it.each([
  { file_count: -1 }, { total_bytes: Number.MAX_SAFE_INTEGER + 1 }, { label_revision: 0 }, { label: "" }, { id: otherID },
])("rejects invalid or mismatched collection authority %j", async (change) => {
  respond({ ...collection, ...change });
  await expect(collectionByID("session", id)).rejects.toThrow();
});

it("keeps retained empty collections distinct from missing ones", async () => {
  respond({ ...collection, file_count: 0, total_bytes: 0 });
  expect((await collectionByID("session", id)).file_count).toBe(0);
});

it("binds members to the collection snapshot and preserves exact content identity", async () => {
  respond({ collection, items: [node], total: 2, limit: 1, offset: 0 });
  const page = await collectionMembers("session", id, 0, 1);
  expect(page.items[0]).toEqual(node);
  expect(page.total).toBe(2);
  expect(page.collection.total_bytes).toBe(12);
});

it.each([
  { collection: { ...collection, id: otherID } },
  { total: 3 },
  { items: [{ ...node, path: undefined }] },
  { items: [{ ...node, kind: "dir" }] },
  { items: [{ ...node, blob_hash: "wrong" }] },
])("rejects inconsistent member receipts %j", async (change) => {
  respond({ collection, items: [node], total: 2, limit: 1, offset: 0, ...change });
  await expect(collectionMembers("session", id, 0, 1)).rejects.toThrow();
});

it("checks the dedicated label ETag", async () => {
  respond(label, '"4"');
  await expect(collectionLabel("session", id)).rejects.toThrow(/receipt/i);
});

it("fences normalized renames and explicit clearing with the inspected label revision", async () => {
  const fetch = respond({ ...label, label: "Café", revision: 4 }, '"4"');
  expect((await setCollectionLabel("session", label, "Cafe\u0301")).label).toBe("Café");
  const request = fetch.mock.calls[0][1];
  expect(request?.method).toBe("PUT");
  expect(new Headers(request?.headers).get("If-Match")).toBe('"3"');
  expect(JSON.parse(String(request?.body))).toEqual({ label: "Café" });
  fetch.mockResolvedValueOnce(new Response(JSON.stringify({ ...label, label: null, revision: 4 }), { headers: { ETag: '"4"' } }));
  expect((await setCollectionLabel("session", label, null)).label).toBeNull();
  expect(JSON.parse(String(fetch.mock.calls[1][1]?.body))).toEqual({ label: null });
});

it("accepts no-op revisions but rejects wrong returned label or revision", async () => {
  respond(label);
  expect((await setCollectionLabel("session", label, "Proposals")).revision).toBe(3);
  await expect(setCollectionLabel("session", label, "New label")).rejects.toThrow(/receipt/i);
});

it("rejects invalid labels and fences before sending", async () => {
  const fetch = respond(label);
  await expect(setCollectionLabel("session", { ...label, revision: 0 }, "Name")).rejects.toThrow();
  await expect(setCollectionLabel("session", label, "é".repeat(129))).rejects.toThrow();
  await expect(setCollectionLabel("session", label, "\ud800")).rejects.toThrow();
  expect(fetch).not.toHaveBeenCalled();
});

it("surfaces stale or audit-fenced changes without automatic retries", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ code: "stale_revision", detail: "Label changed elsewhere" }), { status: 412 }));
  await expect(setCollectionLabel("session", label, "New label")).rejects.toEqual(new APIError("Label changed elsewhere", 412, "stale_revision"));
  expect(fetch).toHaveBeenCalledTimes(1);
});
