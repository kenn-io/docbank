import { afterEach, expect, it, vi } from "vite-plus/test";
import { snapshotEnvelope } from "./snapshot-test-fixtures.js";
import { PhotoCull } from "./photoCull.svelte.js";
import type { CullRun, CullStorage, CullProgress } from "./photoCullStorage.js";
import * as api from "./generated/docbank.js";
import { parseQuery } from "./query.js";
import { photo as fixturePhoto } from "./photo-test-fixtures.js";

const photo = (id: number) => ({
  ...fixturePhoto(id),
  asset_id: `00000000-0000-4000-8000-${String(id).padStart(12, "0")}`,
});

const query = parseQuery('{"sort":{"field":"name","direction":"desc"}}');
afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
function storage() {
  let saved: CullRun | undefined;
  const store: CullStorage = {
    load: vi.fn(async () => structuredClone(saved)),
    replace: vi.fn(async (run) => {
      saved = structuredClone(run);
    }),
    checkpoint: vi.fn(async (progress: CullProgress) => {
      if (!saved) return;
      if (
        saved.checkpoint.runID !== progress.runID ||
        saved.checkpoint.passID !== progress.passID ||
        saved.checkpoint.revision !== progress.revision
      )
        throw new Error("Another cull run replaced this run. Close and resume it.");
      if (progress.pass) saved.checkpoint = structuredClone(progress.pass);
      else {
        if (progress.position !== undefined) saved.checkpoint.position = progress.position;
        if (progress.autoAdvance !== undefined) saved.checkpoint.autoAdvance = progress.autoAdvance;
        if (progress.marker)
          saved.checkpoint[progress.marker.kind][progress.marker.index] = progress.marker.id;
        saved.checkpoint.revision++;
      }
    }),
  };
  return store;
}
async function snapshot(ids: string[]) {
  return snapshotEnvelope(
    query,
    ids.map((asset_id, index) => ({
      asset_id,
      flag: "" as "" | "pick" | "reject",
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
  );
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
  await cull.nextPass(false, () => true);
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
  expect(await cull.start(query, () => true)).toBe(false);
  expect(cull.run).toEqual(original);
  capture.mockRejectedValueOnce(new Error("Snapshot expired"));
  expect(await cull.start(query, () => true)).toBe(false);
  expect(cull.error).toBe("Snapshot expired");
  capture.mockResolvedValueOnce(new Response(JSON.stringify(await snapshot([row.asset_id]))));
  vi.mocked(store.replace).mockRejectedValueOnce(new Error("Storage full"));
  expect(await cull.start(query, () => true)).toBe(false);
  expect(await store.load()).toEqual(original);
});
it("starts from frozen flags without another query and reports capture progress", async () => {
  const rows = [photo(1), photo(2)];
  const receipt = await snapshot(rows.map((row) => row.asset_id));
  receipt.rows[0]!.flag = "pick";
  vi.spyOn(api, "createWorkspaceQuery").mockResolvedValue(new Response(JSON.stringify(receipt)));
  const list = vi.spyOn(api, "listPhotoAssets");
  const cull = new PhotoCull("session", storage());
  expect(await cull.start(query), cull.error).toBe(true);
  expect(cull.run?.checkpoint.pass).toEqual([rows[1]!.asset_id]);
  expect(list).not.toHaveBeenCalled();
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
  cull.move(-1);
  expect(cull.currentID).toBe(rows[1]!.asset_id);
});

it("captures later snapshot pages before publishing a run", async () => {
  const rows = Array.from({ length: 251 }, (_, i) => photo(i + 1));
  const full = await snapshot(rows.map((row) => row.asset_id));
  full.rows[250]!.flag = "reject";
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
  expect(cull.run?.checkpoint.pass).toHaveLength(250);
  expect(cull.run?.checkpoint.pass).not.toContain(rows[250]!.asset_id);
  expect(next.mock.calls[0]?.slice(0, 2)).toEqual(["c".repeat(32), { cursor: "next-page" }]);
  expect(cull.run?.ids[250]).toBe("00000000-0000-4000-8000-000000000251");
  expect(store.replace).toHaveBeenCalledOnce();
});

it("finds unflagged photos outside a picks pass and preserves progress when scope or replacement is canceled", async () => {
  const rows = [photo(1), photo(2), photo(3)];
  vi.spyOn(api, "createWorkspaceQuery").mockResolvedValue(
    new Response(JSON.stringify(await snapshot(rows.map((row) => row.asset_id)))),
  );
  vi.spyOn(api, "listPhotoAssets").mockImplementation(async () => ({
    items: rows,
    total: 3,
    facets: [],
    relevance_available: false,
  }));
  const store = storage(),
    cull = new PhotoCull("session", store);
  await cull.start(query);
  await cull.flush();
  cull.move(1);
  await cull.flush();
  const before = structuredClone(cull.run);
  await cull.nextPass(false, () => false);
  expect(cull.run).toEqual(before);
  expect(await cull.start(query, () => false)).toBe(false);
  expect(await store.load()).toEqual(before);
  rows[0]!.flag = "pick";
  rows[1]!.flag = "reject";
  await cull.nextPass(false, () => true);
  expect(cull.run?.checkpoint.pass).toEqual([rows[0]!.asset_id]);
  cull.move(1);
  await cull.flush();
  await cull.checkRemainder();
  expect(cull.remainder).toBe(true);
  await cull.nextPass(true);
  expect(cull.run?.checkpoint.pass).toEqual([rows[2]!.asset_id]);
  rows[2]!.flag = "pick";
  cull.move(1);
  await cull.checkRemainder();
  expect(cull.remainder).toBe(false);
});
it("keeps automatic storage probes quiet and reports explicit failures", async () => {
  const store = storage();
  vi.mocked(store.load).mockRejectedValue(new Error("Storage unavailable"));
  const cull = new PhotoCull("session", store);
  expect(await cull.restore(true)).toBe(false);
  expect(cull.error).toBe("");
  expect(await cull.restore()).toBe(false);
  expect(cull.error).toBe("Storage unavailable");
});

it.each([false, true])(
  "cancels full-run flag reads without replacing a pass (completion: %s)",
  async (complete) => {
    const row = photo(1),
      store = storage(),
      cull = new PhotoCull("session", store);
    cull.run = {
      ids: [row.asset_id],
      checkpoint: {
        runID: "run",
        passID: "pass",
        revision: 0,
        pass: [row.asset_id],
        position: complete ? 1 : 0,
        visited: [],
        skipped: [],
        unavailable: [],
        scope: "unflagged",
        autoAdvance: true,
      },
    };
    const list = vi.spyOn(api, "listPhotoAssets").mockImplementation(
      (_request, options) =>
        new Promise((_resolve, reject) => {
          options!.signal!.addEventListener("abort", () => reject(options!.signal!.reason), {
            once: true,
          });
        }),
    );
    const before = cull.run;
    const read = complete ? cull.checkRemainder() : cull.nextPass(false, () => true);
    await vi.waitFor(() => expect(list).toHaveBeenCalledTimes(1));
    cull.cancel();
    await read;
    expect(list.mock.calls[0]![1]!.signal!.aborted).toBe(true);
    expect(cull.run).toBe(before);
    expect(cull.busy).toBe(false);
    expect(cull.error).toBe("");
    expect(store.checkpoint).not.toHaveBeenCalled();
  },
);
