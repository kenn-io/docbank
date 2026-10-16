import { beforeEach, afterEach, expect, it, vi } from "vite-plus/test";
import { snapshotEnvelope } from "./snapshot-test-fixtures.js";
import { PhotoCull } from "./photoCull.svelte.js";
import * as photoQueries from "./photos.svelte.js";
import { photoCullStorage, type CullRun } from "./photoCullStorage.js";
import { IDBFactory, IDBKeyRange } from "fake-indexeddb";
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
beforeEach(() => {
  vi.stubGlobal("indexedDB", new IDBFactory());
  vi.stubGlobal("IDBKeyRange", IDBKeyRange);
});
function storage() {
  const store = photoCullStorage("synthetic-vault");
  vi.spyOn(store, "load");
  vi.spyOn(store, "saved");
  vi.spyOn(store, "replace");
  vi.spyOn(store, "checkpoint");
  return store;
}
function browsePage(rows: api.PhotoBrowseRow[], request: api.PhotoBrowseRequest) {
  const filters = request.query.filters;
  const items = rows.filter(
    (row) =>
      (!filters?.asset_ids || filters.asset_ids.includes(row.asset_id)) &&
      (!filters?.flags || filters.flags.includes(row.flag)),
  );
  return { items, total: items.length, facets: [], relevance_available: false };
}
it("probes saved headers and replaces a run without loading its membership", async () => {
  const store = storage(),
    original = new PhotoCull("session", store);
  vi.spyOn(api, "createWorkspaceQuery").mockImplementation(async () =>
    Response.json(await snapshot([photo(1).asset_id])),
  );
  await original.start(query);
  await original.flush();
  const resumed = new PhotoCull("session", store);
  await resumed.probe();
  expect(resumed.saved).toBe(true);
  expect(resumed.run).toBeUndefined();
  expect(await resumed.start(query, () => true)).toBe(true);
  await resumed.flush();
  expect(store.load).not.toHaveBeenCalled();
  expect(await resumed.restore()).toBe(true);
  expect(store.load).toHaveBeenCalledOnce();
});
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
it.each([
  { runID: "" },
  { runID: " " },
  { runID: 1 },
  { passID: "" },
  { passID: null },
  { revision: -1 },
  { revision: 0.5 },
  { revision: Number.MAX_SAFE_INTEGER + 1 },
  { revision: "0" },
  { autoAdvance: "true" },
  { autoAdvance: null },
  { scope: "invalid" },
  { position: 2 },
  { position: -1 },
  { position: 0.5 },
  { pass: null },
  { skipped: [1] },
  { pass: [photo(2).asset_id] },
])("refuses malformed saved cull progress: %j", async (invalid) => {
  const id = photo(1).asset_id;
  const saved: CullRun = {
    ids: [id],
    checkpoint: {
      runID: "11111111-1111-4111-8111-111111111111",
      passID: "22222222-2222-4222-8222-222222222222",
      revision: 0,
      pass: [id],
      position: 0,
      visited: [],
      skipped: [],
      unavailable: [],
      scope: "unflagged",
      autoAdvance: true,
    },
  };
  Object.assign(saved.checkpoint, invalid);
  const store = storage();
  vi.mocked(store.load).mockResolvedValue(saved);
  const cull = new PhotoCull("session", store);
  expect.soft(await cull.restore()).toBe(false);
  expect.soft(cull.run).toBeUndefined();
  expect(cull.error).toBe("Saved cull progress is invalid. Start a new cull.");
});
it("captures the entire ordered photo snapshot from its frozen flags and resumes without reading it again", async () => {
  const rows = [
    photo(3),
    photo(1),
    photo(2),
    ...Array.from({ length: 248 }, (_, i) => photo(i + 4)),
  ];
  const full = await snapshot(rows.map((row) => row.asset_id));
  full.rows[1]!.flag = "pick";
  full.rows[250]!.flag = "reject";
  const first = { ...full, rows: full.rows.slice(0, 250), next_cursor: "next-page" };
  const last = { ...full, rows: full.rows.slice(250), previous_cursor: "first-page" };
  const capture = vi
    .spyOn(api, "createWorkspaceQuery")
    .mockResolvedValue(new Response(JSON.stringify(first)));
  const next = vi
    .spyOn(api, "readWorkspaceQueryPage")
    .mockResolvedValue(new Response(JSON.stringify(last)));
  const list = vi.spyOn(api, "listPhotoAssets");
  const store = storage(),
    cull = new PhotoCull("session", store);
  expect(await cull.start(query), cull.error).toBe(true);
  expect(capture.mock.calls[0][0]).toMatchObject({
    photos: true,
    query: { sort: { field: "name", direction: "desc" } },
    page_size: 250,
  });
  expect(next.mock.calls[0]?.slice(0, 2)).toEqual(["c".repeat(32), { cursor: "next-page" }]);
  expect(cull.run?.ids).toEqual(rows.map((row) => row.asset_id));
  const flagged = new Set([rows[1]!.asset_id, rows[250]!.asset_id]);
  expect(cull.run?.checkpoint.pass).toEqual(
    rows.map((row) => row.asset_id).filter((id) => !flagged.has(id)),
  );
  expect(store.replace).toHaveBeenCalledOnce();
  expect(list).not.toHaveBeenCalled();
  cull.skip();
  cull.toggleAutoAdvance();
  await cull.flush();
  const resumed = new PhotoCull("new-session", store);
  await resumed.restore();
  expect(resumed.currentID).toBe(rows[2]!.asset_id);
  expect(resumed.run?.checkpoint).toMatchObject({
    skipped: [rows[0]!.asset_id],
    visited: [rows[0]!.asset_id, rows[2]!.asset_id],
    autoAdvance: false,
  });
  expect(capture).toHaveBeenCalledTimes(1);
});
it("filters captured IDs locally and reuses completion flags at the next pass boundary", async () => {
  const rows = Array.from({ length: 10 }, (_, index) => photo(index + 1));
  const full = await snapshot(rows.map((row) => row.asset_id));
  for (let index = 2; index < rows.length; index++) {
    rows[index]!.flag = "pick";
    full.rows[index]!.flag = "pick";
  }
  vi.spyOn(api, "createWorkspaceQuery").mockResolvedValue(Response.json(full));
  const list = vi
    .spyOn(api, "listPhotoAssets")
    .mockImplementation(async (request) => browsePage(rows, request));
  const cull = new PhotoCull("session", storage());
  expect(await cull.start(query), cull.error).toBe(true);
  expect(list).not.toHaveBeenCalled();
  const passIDs = rows.slice(0, 2).map((row) => row.asset_id);
  expect(cull.run?.checkpoint.pass).toEqual(passIDs);
  const before = structuredClone(cull.run);
  await cull.nextPass(false, () => false);
  expect(cull.run).toEqual(before);
  expect(list).not.toHaveBeenCalled();
  rows[0]!.flag = "reject";
  rows[1]!.flag = "reject";
  rows[2]!.flag = "";
  cull.choose(passIDs[1]!);
  cull.move(1);
  await cull.checkRemainder();
  expect(cull.error).toBe("");
  expect(cull.remainder).toBe(true);
  expect(list).toHaveBeenCalledTimes(1);
  expect(list.mock.calls[0]![0].query.filters).toEqual({
    asset_ids: rows.map((row) => row.asset_id),
  });
  list.mockClear();
  rows[2]!.flag = "pick";
  rows[3]!.flag = "reject";
  await cull.nextPass(false, () => true);
  expect(cull.error).toBe("");
  expect(cull.run?.checkpoint.scope).toBe("picks");
  expect(cull.run?.checkpoint.pass).toEqual([
    rows[3]!.asset_id,
    ...rows.slice(4).map((row) => row.asset_id),
  ]);
  expect(cull.run?.checkpoint.unavailable).toEqual([]);
  expect(list).not.toHaveBeenCalled();
  rows[2]!.flag = "";
  cull.choose(rows[9]!.asset_id);
  cull.move(1);
  await cull.nextPass(true);
  expect(cull.run?.checkpoint.scope).toBe("unflagged");
  expect(cull.run?.checkpoint.pass).toEqual([rows[2]!.asset_id]);
  rows[2]!.flag = "pick";
  cull.move(1);
  await cull.checkRemainder();
  expect(cull.remainder).toBe(false);
  expect(
    list.mock.calls.every(
      ([request]) =>
        request.query.filters?.asset_ids?.length && !request.cursor && !request.query.filters.flags,
    ),
  ).toBe(true);
});
it("loads restored flags once and removes missing current-pass rows from later scopes", async () => {
  const rows = [photo(1), photo(2), photo(3)],
    ids = rows.map((row) => row.asset_id),
    store = storage();
  vi.spyOn(api, "createWorkspaceQuery").mockResolvedValue(
    new Response(JSON.stringify(await snapshot(ids))),
  );
  const original = new PhotoCull("session", store);
  await original.start(query);
  await original.flush();
  rows[0]!.flag = "pick";
  rows[2]!.flag = "reject";
  const available = new Map([rows[0]!, rows[2]!].map((row) => [row.asset_id, row]));
  const list = vi.spyOn(api, "listPhotoAssets").mockImplementation(async (request) => ({
    ...browsePage([...available.values()], request),
  }));
  const resumed = new PhotoCull("new-session", store);
  await resumed.restore();
  await resumed.nextPass(false, () => true);
  expect(resumed.run?.checkpoint.pass).toEqual([ids[0]]);
  rows[0]!.flag = "reject";
  await resumed.nextPass(false, () => true);
  expect(resumed.run?.checkpoint.pass).toEqual([ids[0], ids[2]]);
  available.delete(ids[2]!);
  await resumed.nextPass(false, () => true);
  expect(resumed.run?.checkpoint.pass).toEqual([ids[0]]);
  expect(resumed.run?.checkpoint.unavailable).toEqual([ids[1], ids[2]]);
  expect(
    list.mock.calls
      .filter(([request]) => request.query.filters?.asset_ids)
      .map(([request]) => request.query.filters?.asset_ids),
  ).toEqual([ids, ids, ids]);
});
it("keeps the previous durable run after cancellation, capture failure or failed replacement", async () => {
  const row = photo(1),
    store = storage(),
    cull = new PhotoCull("session", store);
  const capture = vi
    .spyOn(api, "createWorkspaceQuery")
    .mockResolvedValue(new Response(JSON.stringify(await snapshot([row.asset_id]))));
  await cull.start(query);
  await cull.flush();
  const original = structuredClone(cull.run);
  expect(await cull.start(query, () => false)).toBe(false);
  expect(await store.load()).toEqual(original);
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
it("ignores auto-advance and unavailable updates while a scope change reads flags", async () => {
  const rows = [photo(1), photo(2)],
    store = storage();
  vi.spyOn(api, "createWorkspaceQuery").mockResolvedValue(
    new Response(JSON.stringify(await snapshot(rows.map((row) => row.asset_id)))),
  );
  const list = vi.spyOn(api, "listPhotoAssets");
  const cull = new PhotoCull("session", store);
  await cull.start(query);
  await cull.flush();
  let release!: () => void;
  list
    .mockImplementation(async (request) => browsePage(rows, request))
    .mockImplementationOnce(
      (request) =>
        new Promise((resolve) => {
          release = () => resolve(browsePage(rows, request));
        }),
    );
  const changing = cull.nextPass(false, () => true);
  await vi.waitFor(() => expect(cull.busy).toBe(true));
  await vi.waitFor(() => expect(list).toHaveBeenCalled());
  cull.toggleAutoAdvance();
  cull.unavailable(rows[1]!.asset_id);
  release();
  await changing;
  await cull.flush();
  expect(cull.error).toBe("");
  expect(cull.stale).toBe(false);
  expect(cull.run?.checkpoint.scope).toBe("picks");
  expect((await store.load())?.checkpoint).toEqual(cull.run?.checkpoint);
});
it("keeps per-photo marker work independent of how many photos were already marked", async () => {
  const rows = Array.from({ length: 8 }, (_, i) => photo(i + 1)),
    ids = rows.map((row) => row.asset_id),
    size = 2000;
  const seed = (kind: string) => Array.from({ length: size }, (_, i) => `${kind}-${i}`);
  let reads = 0;
  // Counts every indexed read of a marker list, however it is scanned or copied.
  const counted = (list: string[]) =>
    new Proxy(list, {
      get(target, key, receiver) {
        if (typeof key === "string" && /^\d+$/.test(key)) reads++;
        return Reflect.get(target, key, receiver);
      },
    });
  const store = storage();
  await store.replace({
    ids,
    checkpoint: {
      runID: "run",
      passID: "pass",
      revision: 0,
      pass: ids,
      position: 0,
      visited: seed("visited"),
      skipped: seed("skipped"),
      unavailable: seed("unavailable"),
      scope: "unflagged",
      autoAdvance: true,
    },
  });
  const load = store.load;
  store.load = vi.fn(async () => {
    const run = await load();
    const { visited, skipped, unavailable } = run!.checkpoint;
    run!.checkpoint = {
      ...run!.checkpoint,
      visited: counted(visited),
      skipped: counted(skipped),
      unavailable: counted(unavailable),
    };
    return run;
  });
  const cull = new PhotoCull("session", store);
  await cull.restore();
  // Restoring may read each list once. Marking afterwards must not read them again.
  reads = 0;
  cull.move(1);
  for (let step = 0; step < 3; step++) {
    cull.move(1);
    cull.skip();
    cull.unavailable(`missing-${step}`);
  }
  cull.move(-1);
  cull.move(1);
  cull.unavailable("missing-0");
  expect(reads).toBeLessThan(size / 10);
  await cull.flush();
  const checkpoint = cull.run!.checkpoint;
  expect([
    checkpoint.visited.length,
    checkpoint.skipped.length,
    checkpoint.unavailable.length,
  ]).toEqual([size + 7, size + 3, size + 3]);
  const resumed = new PhotoCull("new-session", store);
  await resumed.restore();
  expect(resumed.currentID).toBe(cull.currentID);
  expect(resumed.run?.checkpoint.visited.slice(size)).toEqual(ids.slice(1));
  expect(resumed.run?.checkpoint.skipped.slice(size)).toEqual([ids[2], ids[4], ids[6]]);
  expect(resumed.run?.checkpoint.unavailable.slice(size)).toEqual([
    "missing-0",
    "missing-1",
    "missing-2",
  ]);
});
it("reports checkpoint failures and freezes navigation after a failed save", async () => {
  const rows = [photo(1), photo(2)],
    store = storage();
  vi.spyOn(api, "createWorkspaceQuery").mockResolvedValue(
    new Response(JSON.stringify(await snapshot(rows.map((row) => row.asset_id)))),
  );
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

it("keeps automatic storage probes quiet and reports explicit failures", async () => {
  const store = storage();
  vi.mocked(store.load).mockRejectedValue(new Error("Storage unavailable"));
  const cull = new PhotoCull("session", store);
  expect(await cull.restore(true)).toBe(false);
  expect(cull.error).toBe("");
  expect(await cull.restore()).toBe(false);
  expect(cull.error).toBe("Storage unavailable");
});

it("keeps the newer remainder result when an older flag read finishes", async () => {
  const row = { ...photo(1), flag: "pick" as api.PhotoBrowseRowFlag };
  const cull = new PhotoCull("session", storage());
  cull.run = {
    ids: [row.asset_id],
    checkpoint: {
      runID: "run",
      passID: "pass",
      revision: 0,
      pass: [row.asset_id],
      position: 1,
      visited: [row.asset_id],
      skipped: [],
      unavailable: [],
      scope: "unflagged",
      autoAdvance: true,
    },
  };
  let release!: (page: api.PhotoBrowsePage) => void;
  const oldLookup = new Promise<api.PhotoBrowsePage>((resolve) => {
    release = resolve;
  });
  const stalePage = browsePage([{ ...row }], { query: photoQueries.photoQuery });
  const list = vi
    .spyOn(api, "listPhotoAssets")
    .mockImplementation(async (request) => browsePage([row], request))
    .mockImplementationOnce(() => oldLookup);
  const older = cull.checkRemainder();
  await vi.waitFor(() => expect(list).toHaveBeenCalledTimes(1));
  row.flag = "";
  await cull.checkRemainder();
  expect(cull.remainder).toBe(true);
  expect(list.mock.calls[0]![1]!.signal!.aborted).toBe(true);
  release(stalePage);
  await older;
  expect(cull.remainder).toBe(true);
});

it.each([false, true])(
  "cancels flag reads without replacing a pass: complete=%s",
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
    const list = vi.spyOn(api, "listPhotoAssets").mockImplementation((request, options) => {
      return new Promise((_resolve, reject) => {
        options!.signal!.addEventListener("abort", () => reject(options!.signal!.reason), {
          once: true,
        });
      });
    });
    const before = cull.run;
    const read = complete ? cull.checkRemainder() : cull.nextPass(false, () => true);
    await vi.waitFor(() => expect(list).toHaveBeenCalledTimes(1));
    cull.cancel();
    await read;
    expect(list.mock.calls.at(-1)![1]!.signal!.aborted).toBe(true);
    expect(cull.run).toBe(before);
    expect(cull.busy).toBe(false);
    expect(cull.error).toBe("");
    expect(store.checkpoint).not.toHaveBeenCalled();
  },
);

it("reports a failed flag lookup and clears its progress", async () => {
  const id = photo(1).asset_id;
  const cull = new PhotoCull("session", storage());
  cull.run = {
    ids: [id],
    checkpoint: {
      runID: "run",
      passID: "pass",
      revision: 0,
      pass: [id],
      position: 1,
      visited: [],
      skipped: [],
      unavailable: [],
      scope: "unflagged",
      autoAdvance: true,
    },
  };
  vi.spyOn(photoQueries, "lookupPhotoRows").mockRejectedValue(new Error("Lookup unavailable"));
  await cull.checkRemainder();
  expect(cull.error).toBe("Lookup unavailable");
  expect(cull.progress).toBe("");
  expect(cull.busy).toBe(false);
});
