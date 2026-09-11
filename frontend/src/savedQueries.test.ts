import { createHash } from "node:crypto";
import { afterEach, describe, expect, it, vi } from "vitest";
import { APIError } from "./api.js";
import { createSavedQuery, deleteSavedQuery, getSavedQuery, listSavedQueries, updateSavedQuery } from "./savedQueries.js";

const id = "00000000-0000-4000-8000-000000000001";
const canonical = '{"filters":{"extensions":["pdf"],"no_tags":true},"mode":"hybrid","sort":{"direction":"desc","field":"size"},"syntax":"advanced","text":"alpha OR β","v":1}';
const payload = JSON.parse(canonical);
const record = {
  id, name: "Review", description: "Synthetic definition", kind: "query" as const,
  payload, fingerprint: `sha256:${createHash("sha256").update(canonical).digest("hex")}`,
  revision: 3, created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-02T00:00:00Z",
};
function respond(value: unknown, etag = '"3"') {
  return vi.spyOn(globalThis, "fetch").mockImplementation(async () => new Response(JSON.stringify(value), {
    headers: { ETag: etag, "Content-Type": "application/json" },
  }));
}
afterEach(() => vi.restoreAllMocks());

describe("saved definition receipts", () => {
  it("loads the complete expression, unsupported facets, mode and sort", async () => {
    respond(record);
    const loaded = await getSavedQuery("session", id);
    expect(loaded.payload).toEqual(payload);
    expect(loaded.revision).toBe(3);
  });

  it.each([
    ["identity", { id: "00000000-0000-4000-8000-000000000002" }],
    ["fingerprint", { fingerprint: `sha256:${"0".repeat(64)}` }],
    ["revision", { revision: 0 }],
    ["kind", { kind: "highlight_set" }],
    ["unknown query field", { payload: { ...payload, unknown: true } }],
  ])("rejects a wrong %s", async (_name, change) => {
    respond({ ...record, ...change });
    await expect(getSavedQuery("session", id)).rejects.toThrow();
  });

  it("binds the ETag to the payload revision", async () => {
    respond(record, '"4"');
    await expect(getSavedQuery("session", id)).rejects.toThrow(/receipt/i);
  });

  it("validates list selectors, totals and distinct identities", async () => {
    const fetch = respond({ items: [record], total: 1, limit: 100, offset: 0 });
    expect((await listSavedQueries("session", "query")).items[0].payload).toEqual(payload);
    expect(String(fetch.mock.calls[0][0])).toContain("kind=query");
    fetch.mockResolvedValueOnce(new Response(JSON.stringify({ items: [record, record], total: 2, limit: 100, offset: 0 })));
    await expect(listSavedQueries("session", "query")).rejects.toThrow(/receipt/i);
    fetch.mockResolvedValueOnce(new Response(JSON.stringify({ items: [record], total: 1, limit: 100, offset: 0 })));
    await expect(listSavedQueries("session", "highlight_set")).rejects.toThrow(/receipt/i);
  });

  it("creates without changing the full definition and checks the returned content", async () => {
    const fetch = respond({ ...record, revision: 1 }, '"1"');
    await createSavedQuery("session", { name: record.name, description: record.description, kind: "query", payload });
    expect(JSON.parse(String(fetch.mock.calls[0][1]?.body))).toEqual({ name: record.name, description: record.description, kind: "query", payload });
    fetch.mockResolvedValueOnce(new Response(JSON.stringify({ ...record, name: "Wrong", revision: 1 }), { headers: { ETag: '"1"' } }));
    await expect(createSavedQuery("session", { name: record.name, description: record.description, kind: "query", payload })).rejects.toThrow(/receipt/i);
  });

  it("fences a rename and preserves omitted fields", async () => {
    const fetch = respond({ ...record, name: "Renamed", revision: 4 }, '"4"');
    const updated = await updateSavedQuery("session", record, { name: "Renamed" });
    expect(updated.payload).toEqual(payload);
    expect(new Headers(fetch.mock.calls[0][1]?.headers).get("If-Match")).toBe('"3"');
    expect(JSON.parse(String(fetch.mock.calls[0][1]?.body))).toEqual({ name: "Renamed" });
  });

  it("accepts an unchanged revision only for a no-op", async () => {
    respond(record);
    expect((await updateSavedQuery("session", record, { name: "Review" })).revision).toBe(3);
    await expect(updateSavedQuery("session", record, { name: "Renamed" })).rejects.toThrow(/receipt/i);
  });

  it("rejects null patches and unsafe fences before sending", async () => {
    const fetch = respond(record);
    await expect(updateSavedQuery("session", record, { description: null } as never)).rejects.toThrow();
    await expect(deleteSavedQuery("session", { ...record, revision: 0 })).rejects.toThrow();
    expect(fetch).not.toHaveBeenCalled();
  });

  it("does not retry a stale mutation or claim deletion", async () => {
    const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ code: "stale_revision", detail: "Changed elsewhere" }), { status: 412 }));
    await expect(deleteSavedQuery("session", record)).rejects.toEqual(new APIError("Changed elsewhere", 412, "stale_revision"));
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  it("binds the deletion receipt to the entire observed definition", async () => {
    const fetch = respond(record);
    expect((await deleteSavedQuery("session", record)).id).toBe(id);
    expect(new Headers(fetch.mock.calls[0][1]?.headers).get("If-Match")).toBe('"3"');
    fetch.mockResolvedValueOnce(new Response(JSON.stringify({ ...record, name: "Someone else" }), { headers: { ETag: '"3"' } }));
    await expect(deleteSavedQuery("session", record)).rejects.toThrow(/receipt/i);
  });

  it("saves bounded literal highlights separately from executable queries", async () => {
    const highlight = { v: 1, terms: [{ text: "alpha", color: "#ffcc00" }] };
    const fingerprint = `sha256:${createHash("sha256").update('{"terms":[{"color":"#ffcc00","text":"alpha"}],"v":1}').digest("hex")}`;
    const fetch = respond({ ...record, kind: "highlight_set", payload: highlight, fingerprint, revision: 1 }, '"1"');
    expect((await createSavedQuery("session", { name: record.name, description: record.description, kind: "highlight_set", payload: highlight })).kind).toBe("highlight_set");
    fetch.mockClear();
    await expect(createSavedQuery("session", { name: "Bad", kind: "highlight_set", payload: { v: 1, terms: [{ text: "x", color: "red" }] } })).rejects.toThrow();
    expect(fetch).not.toHaveBeenCalled();
  });
});
