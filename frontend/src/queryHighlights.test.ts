import { createHash } from "node:crypto";
import { afterEach, expect, it, vi } from "vitest";
import { canonicalQuery, parseQuery } from "./query.js";
import { readQueryHighlights } from "./queryHighlights.js";
import type { SnapshotPage } from "./snapshots.js";

afterEach(() => vi.restoreAllMocks());

const query = parseQuery(`{"syntax":"advanced","text":"alpha AND saved:review"}`);
const fingerprint = `sha256:${createHash("sha256").update(canonicalQuery(query)).digest("hex")}`;
const dependency = {
  kind: "saved" as const,
  id: "11111111-1111-4111-8111-111111111111",
  revision: 4,
};
const snapshot = {
  query,
  query_fingerprint: fingerprint,
  dependencies: [dependency],
} as SnapshotPage;

function receipt(overrides: Record<string, unknown> = {}): Response {
  return new Response(JSON.stringify({
    $schema: "https://example.test/schemas/QueryHighlightReceipt.json",
    query_fingerprint: fingerprint,
    dependencies: [dependency],
    terms: ["alpha", "nested phrase"],
    ...overrides,
  }), { headers: { "Content-Type": "application/json" } });
}

it("accepts compiler terms only when fingerprint and saved revisions match the snapshot", async () => {
  const controller = new AbortController();
  const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(receipt());

  await expect(readQueryHighlights("session", snapshot, controller.signal)).resolves.toEqual([
    "alpha", "nested phrase",
  ]);
  const [path, request] = fetchMock.mock.calls[0] ?? [];
  expect(path).toBe("/api/v1/queries/highlights");
  expect(request?.method).toBe("POST");
  expect(request?.body).toBe(canonicalQuery(query));
  expect(request?.signal).toBe(controller.signal);
});

it.each([
  ["changed dependency revision", { dependencies: [{ ...dependency, revision: 5 }] }],
  ["changed fingerprint", { query_fingerprint: `sha256:${"0".repeat(64)}` }],
  ["unknown receipt field", { future: true }],
  ["invalid schema link", { $schema: 7 }],
  ["duplicate term", { terms: ["alpha", "alpha"] }],
  ["more than 64 terms", { terms: Array.from({ length: 65 }, (_, index) => `t${index}`) }],
  ["term longer than 256 scalars", { terms: ["😀".repeat(257)] }],
])("rejects %s instead of applying unbound highlights", async (_name, overrides) => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(receipt(overrides));
  await expect(readQueryHighlights("session", snapshot, new AbortController().signal))
    .rejects.toThrow(/highlight receipt/i);
});
