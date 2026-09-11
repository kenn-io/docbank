import { afterEach, expect, it, vi } from "vitest";
import { readDuplicateContext } from "./duplicates.js";
import type { SelectedSource } from "./selectedSource.js";

afterEach(() => vi.restoreAllMocks());

const source: SelectedSource = { kind: "snapshot", key: "snapshot:7", nodeID: 7,
  mutationRevision: 2, versionID: "11111111-1111-4111-8111-111111111111",
  blobHash: "a".repeat(64), size: 12, name: "frozen.txt", path: "/frozen.txt",
  mimeType: "text/plain", modifiedAt: "2026-09-11T00:00:00Z", observedAt: "2026-09-11T00:00:00Z",
  originalTags: [], collectionIDs: [] };

const reference = (id: number) => ({ node_id: id, revision: id, version_id:
  `00000000-0000-4000-8000-${String(id).padStart(12, "0")}`, sha256: source.blobHash,
  size: source.size, name: `copy-${id}.txt`, path: `/outside/copy-${id}.txt`,
  media_type: "text/plain", modified_at: "2026-09-11T00:00:00Z" });

it("loads only the exact selected current duplicate group", async () => {
  const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({
    $schema: "https://example.test/schemas/DuplicateByHashReceipt.json",
    sha256: source.blobHash, size: source.size, reference_count: 2,
    references: [reference(7), reference(9)], references_truncated: false,
  }), { headers: { "Content-Type": "application/json" } }));
  const group = await readDuplicateContext("session", source, new AbortController().signal);
  expect(group.references.map((item) => item.nodeID)).toEqual([7, 9]);
  expect(String(fetchMock.mock.calls[0]?.[0])).toContain(`sha256=${source.blobHash}&size=12`);
});

it.each([
  ["wrong hash", { sha256: "b".repeat(64) }],
  ["widened receipt", { extra: true }],
  ["invalid schema link", { $schema: 7 }],
  ["wrong member hash", { references: [reference(7), { ...reference(9), sha256: "b".repeat(64) }] }],
])("rejects %s", async (_name, patch) => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({
    sha256: source.blobHash, size: source.size, reference_count: 2,
    references: [reference(7), reference(9)], references_truncated: false, ...patch,
  }), { headers: { "Content-Type": "application/json" } }));
  await expect(readDuplicateContext("session", source, new AbortController().signal)).rejects.toThrow(/Malformed duplicate context/);
});
