import { afterEach, expect, it, vi } from "vite-plus/test";
import { snapshotMemberHash } from "./snapshots.js";
import { PhotoCull } from "./photoCull.svelte.js";
import type { CullRun, CullStorage } from "./photoCullStorage.js";
import * as api from "./generated/docbank.js";
import { parseQuery, queryFingerprint } from "./query.js";
import { photo as fixturePhoto } from "./photo-test-fixtures.js";

const photo = (id: number) => ({
  ...fixturePhoto(id),
  asset_id: `00000000-0000-4000-8000-${String(id).padStart(12, "0")}`,
});

const query = parseQuery('{"sort":{"field":"name","direction":"desc"}}');
afterEach(() => vi.restoreAllMocks());
function storage() {
  let saved: CullRun | undefined;
  const store: CullStorage = {
    load: vi.fn(async () => structuredClone(saved)),
    replace: vi.fn(async (run) => {
      saved = structuredClone(run);
    }),
    checkpoint: vi.fn(async (checkpoint) => {
      if (saved) saved.checkpoint = structuredClone(checkpoint);
    }),
  };
  return store;
}
async function snapshot(ids: string[]) {
  return {
    query,
    dependencies: [],
    query_fingerprint: await queryFingerprint(query),
    member_hash: await snapshotMemberHash(
      ids.map((asset_id, index) => ({
        asset_id,
        node_id: index + 1,
        content_version_id: "11111111-1111-4111-8111-111111111111",
      })),
    ),
    snapshot_fingerprint: `sha256:${"b".repeat(64)}`,
    generation: { kind: "native" },
    coverage: { configuration: "unconfigured" },
    observed_at: "2026-10-01T00:00:00Z",
    created_at: "2026-10-01T00:00:00Z",
    expires_at: "2026-10-01T00:20:00Z",
    snapshot: true,
    snapshot_id: "c".repeat(32),
    total: ids.length,
    total_bytes: ids.length * 10,
    page_size: 250,
    facets: [],
    rows: ids.map((asset_id, index) => ({
      asset_id,
      node_id: index + 1,
      content_version_id: "11111111-1111-4111-8111-111111111111",
      blob_hash: "d".repeat(64),
      size: 10,
      revision: 1,
      name: `Photo ${index}.jpg`,
      path: `/Photo ${index}.jpg`,
      mime_type: "image/jpeg",
      media_family: "image",
      modified_at: "2026-10-01T00:00:00Z",
      sort_key: String(index),
      tags: [],
      collection_ids: [],
    })),
  };
}
it("captures the entire ordered photo snapshot and resumes without reading the snapshot again", async () => {
  const rows = [photo(3), photo(1), photo(2)];
  const capture = vi
    .spyOn(api, "createWorkspaceQuery")
    .mockResolvedValue(
      new Response(JSON.stringify(await snapshot(rows.map((row) => row.asset_id)))),
    );
  vi.spyOn(api, "listPhotoAssets").mockResolvedValue({
    items: rows,
    total: 3,
    facets: [],
    relevance_available: false,
  });
  const store = storage(),
    cull = new PhotoCull("session", store);
  expect(await cull.start(query), cull.error).toBe(true);
  expect(capture.mock.calls[0][0]).toMatchObject({
    photos: true,
    query: { sort: { field: "name", direction: "desc" } },
    page_size: 250,
  });
  expect(cull.run?.ids).toEqual([rows[0]!.asset_id, rows[1]!.asset_id, rows[2]!.asset_id]);
  cull.skip();
  cull.toggleAutoAdvance();
  await cull.flush();
  const resumed = new PhotoCull("new-session", store);
  await resumed.restore();
  expect(resumed.currentID).toBe(rows[1]!.asset_id);
  expect(resumed.run?.checkpoint).toMatchObject({
    skipped: [rows[0]!.asset_id],
    visited: [rows[0]!.asset_id, rows[1]!.asset_id],
    autoAdvance: false,
  });
  expect(capture).toHaveBeenCalledTimes(1);
});
it("keeps decisions in the pass and refreshes scope from display flags, regardless of rating", async () => {
  const rows = [photo(1), photo(2), photo(3)];
  rows[0]!.rating = 5;
  vi.spyOn(api, "createWorkspaceQuery").mockResolvedValue(
    new Response(JSON.stringify(await snapshot(rows.map((row) => row.asset_id)))),
  );
  const flags = vi.spyOn(api, "listPhotoAssets").mockImplementation(async () => ({
    items: rows,
    total: 3,
    facets: [],
    relevance_available: false,
  }));
  const cull = new PhotoCull("session", storage());
  await cull.start(query);
  rows[0]!.flag = "pick";
  rows[1]!.flag = "reject";
  expect(cull.run?.checkpoint.pass).toEqual(rows.map((row) => row.asset_id));
  cull.move(1);
  cull.skip();
  cull.move(1);
  expect(cull.complete).toBe(true);
  await cull.nextPass(true);
  expect(cull.run?.checkpoint.pass).toEqual([rows[2]!.asset_id]);
  await cull.nextPass();
  expect(cull.run?.checkpoint.scope).toBe("picks");
  expect(cull.run?.checkpoint.pass).toEqual([rows[0]!.asset_id]);
  expect(flags.mock.calls[0][0].query.filters?.asset_ids).toEqual(rows.map((row) => row.asset_id));
});
it("keeps the previous durable run after cancellation, capture failure or failed replacement", async () => {
  const row = photo(1),
    store = storage(),
    cull = new PhotoCull("session", store);
  const capture = vi
    .spyOn(api, "createWorkspaceQuery")
    .mockResolvedValue(new Response(JSON.stringify(await snapshot([row.asset_id]))));
  vi.spyOn(api, "listPhotoAssets").mockResolvedValue({
    items: [row],
    total: 1,
    facets: [],
    relevance_available: false,
  });
  await cull.start(query);
  await cull.flush();
  const original = structuredClone(cull.run);
  capture.mockImplementationOnce(async (_request, options) => {
    cull.cancel();
    options?.signal?.throwIfAborted();
    throw new Error("unreachable");
  });
  expect(await cull.start(query)).toBe(false);
  expect(cull.run).toEqual(original);
  capture.mockRejectedValueOnce(new Error("Snapshot expired"));
  expect(await cull.start(query)).toBe(false);
  expect(cull.error).toBe("Snapshot expired");
  capture.mockResolvedValueOnce(new Response(JSON.stringify(await snapshot([row.asset_id]))));
  vi.mocked(store.replace).mockRejectedValueOnce(new Error("Storage full"));
  expect(await cull.start(query)).toBe(false);
  expect(await store.load()).toEqual(original);
});
it("refreshes flags in bounded batches and accounts for unavailable captured members", async () => {
  const rows = Array.from({ length: 65 }, (_, i) => photo(i + 1));
  vi.spyOn(api, "createWorkspaceQuery").mockResolvedValue(
    new Response(JSON.stringify(await snapshot(rows.map((row) => row.asset_id)))),
  );
  const list = vi
    .spyOn(api, "listPhotoAssets")
    .mockResolvedValueOnce({
      items: rows.slice(0, 64),
      total: 64,
      facets: [],
      relevance_available: false,
    })
    .mockResolvedValueOnce({ items: [], total: 0, facets: [], relevance_available: false });
  const cull = new PhotoCull("session", storage());
  await cull.start(query);
  expect(list.mock.calls.map(([request]) => request.query.filters?.asset_ids?.length)).toEqual([
    64, 1,
  ]);
  expect(cull.run?.checkpoint.unavailable).toEqual([rows[64]!.asset_id]);
  expect(cull.run?.ids).toHaveLength(65);
  expect(cull.run?.checkpoint.pass).toHaveLength(64);
});
it("reports checkpoint failures and preserves navigation within the live pass", async () => {
  const rows = [photo(1), photo(2)],
    store = storage();
  vi.spyOn(api, "createWorkspaceQuery").mockResolvedValue(
    new Response(JSON.stringify(await snapshot(rows.map((row) => row.asset_id)))),
  );
  vi.spyOn(api, "listPhotoAssets").mockResolvedValue({
    items: rows,
    total: 2,
    facets: [],
    relevance_available: false,
  });
  const cull = new PhotoCull("session", store);
  await cull.start(query);
  await cull.flush();
  vi.mocked(store.checkpoint).mockRejectedValueOnce(new Error("Storage full"));
  cull.move(1);
  await cull.flush();
  expect(cull.currentID).toBe(rows[1]!.asset_id);
  expect(cull.error).toContain("Progress could not be saved");
});

it("captures later snapshot pages before publishing a run", async () => {
  const rows = Array.from({ length: 251 }, (_, i) => photo(i + 1));
  const full = await snapshot(rows.map((row) => row.asset_id));
  const first = { ...full, rows: full.rows.slice(0, 250), next_cursor: "next-page" };
  const last = { ...full, rows: full.rows.slice(250), previous_cursor: "first-page" };
  vi.spyOn(api, "createWorkspaceQuery").mockResolvedValue(new Response(JSON.stringify(first)));
  const next = vi
    .spyOn(api, "readWorkspaceQueryPage")
    .mockResolvedValue(new Response(JSON.stringify(last)));
  vi.spyOn(api, "listPhotoAssets").mockImplementation(async (request) => ({
    items: rows.filter((row) => request.query.filters?.asset_ids?.includes(row.asset_id)),
    total: 0,
    facets: [],
    relevance_available: false,
  }));
  const store = storage(),
    cull = new PhotoCull("session", store);
  expect(await cull.start(query), cull.error).toBe(true);
  expect(next.mock.calls[0]?.slice(0, 2)).toEqual(["c".repeat(32), { cursor: "next-page" }]);
  expect(cull.run?.ids[250]).toBe("00000000-0000-4000-8000-000000000251");
  expect(store.replace).toHaveBeenCalledOnce();
});
