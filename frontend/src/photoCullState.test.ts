import { beforeEach, afterEach, expect, it, vi } from "vite-plus/test";
import { snapshotEnvelope } from "./snapshot-test-fixtures.js";
import { PhotoCull } from "./photoCull.svelte.js";
import { APIError } from "./api-transport.js";
import { CullFormatError, photoCullStorage, type CullRun } from "./photoCullStorage.js";
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
function flagPage(rows: api.PhotoBrowseRow[], request: Parameters<typeof api.lookupPhotoFlags>[0]) {
  return {
    items: rows
      .filter((row) => Object.values(request.asset_ids).includes(row.asset_id))
      .map(({ asset_id, flag }) => ({ asset_id, flag })),
  };
}

it("uses bulk flags to select picks for the next pass", async () => {
  const rows = [photo(1), { ...photo(2), flag: "pick" as const }];
  vi.spyOn(api, "createWorkspaceQuery").mockImplementation(async () =>
    Response.json(await snapshot(rows.map((row) => row.asset_id))),
  );
  const lookup = vi
    .spyOn(api, "lookupPhotoFlags")
    .mockImplementation(async (request) => flagPage(rows, request));
  const cull = new PhotoCull("session", storage());
  await cull.start(query);
  await cull.nextPass(false, () => true);
  expect(cull.run?.checkpoint.pass).toEqual([rows[1]!.asset_id]);
  expect(lookup).toHaveBeenCalledOnce();
  expect(lookup.mock.calls[0]![0]).toEqual({ asset_ids: rows.map((row) => row.asset_id) });
});
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
  const list = vi.spyOn(api, "lookupPhotoFlags");
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
it("filters captured IDs locally with fresh flags at each pass boundary", async () => {
  const rows = Array.from({ length: 10 }, (_, index) => photo(index + 1));
  const full = await snapshot(rows.map((row) => row.asset_id));
  for (let index = 2; index < rows.length; index++) {
    rows[index]!.flag = "pick";
    full.rows[index]!.flag = "pick";
  }
  vi.spyOn(api, "createWorkspaceQuery").mockResolvedValue(Response.json(full));
  const list = vi
    .spyOn(api, "lookupPhotoFlags")
    .mockImplementation(async (request) => flagPage(rows, request));
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
  expect(list.mock.calls[0]![0]).toEqual({
    asset_ids: rows.map((row) => row.asset_id),
  });
  list.mockClear();
  rows[2]!.flag = "pick";
  rows[3]!.flag = "reject";
  await cull.nextPass(false, () => true);
  expect(cull.error).toBe("");
  expect(cull.run?.checkpoint.scope).toBe("picks");
  expect(cull.run?.checkpoint.pass).toEqual([
    rows[2]!.asset_id,
    ...rows.slice(4).map((row) => row.asset_id),
  ]);
  expect(cull.run?.checkpoint.unavailable).toEqual([]);
  expect(list).toHaveBeenCalledTimes(1);
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
  expect(list.mock.calls.every(([request]) => Object.values(request.asset_ids).length > 0)).toBe(
    true,
  );
});
it("persists missing remainder photos in one checkpoint and keeps existing markers", async () => {
  const rows = [photo(1), photo(2), photo(3), photo(4)],
    ids = rows.map((row) => row.asset_id),
    store = storage();
  vi.spyOn(api, "createWorkspaceQuery").mockResolvedValue(Response.json(await snapshot(ids)));
  vi.spyOn(api, "lookupPhotoFlags").mockImplementation(async (request) =>
    flagPage([rows[3]!], request),
  );
  const cull = new PhotoCull("session", store);
  await cull.start(query);
  cull.unavailable(ids[0]!);
  cull.choose(ids[3]!);
  cull.move(1);
  await cull.flush();
  vi.mocked(store.checkpoint).mockClear();
  await cull.checkRemainder();
  await cull.flush();
  expect(store.checkpoint).toHaveBeenCalledOnce();
  expect(cull.remainder).toBe(true);
  expect((await store.load())!.checkpoint.unavailable).toEqual([ids[0], ids[1], ids[2]]);
  await cull.checkRemainder();
  await cull.flush();
  expect(store.checkpoint).toHaveBeenCalledOnce();
  vi.mocked(api.lookupPhotoFlags).mockImplementation(async (request) => flagPage([], request));
  await cull.nextPass(true);
  await cull.flush();
  expect(cull.complete).toBe(true);
  expect(cull.remainder).toBe(false);
  expect((await store.load())!.checkpoint.unavailable).toEqual(ids);
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
  const list = vi.spyOn(api, "lookupPhotoFlags").mockImplementation(async (request) => ({
    ...flagPage([...available.values()], request),
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
  expect(list.mock.calls.map(([request]) => request.asset_ids)).toEqual([ids, ids, ids]);
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
  const replacement = new Response(JSON.stringify(await snapshot([photo(2).asset_id])));
  let release!: () => void;
  capture.mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        release = () => resolve(replacement);
      }),
  );
  const replacing = cull.start(query, () => true);
  await vi.waitFor(() => expect(release).toBeTypeOf("function"));
  cull.cancel();
  release();
  expect(await replacing).toBe(false);
  await cull.flush();
  expect(cull.run).toEqual(original);
  expect(await store.load()).toEqual(original);
  expect(cull.error).toBe("");
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
  const list = vi.spyOn(api, "lookupPhotoFlags");
  const cull = new PhotoCull("session", store);
  await cull.start(query);
  await cull.flush();
  let release!: () => void;
  list
    .mockImplementation(async (request) => flagPage(rows, request))
    .mockImplementationOnce(
      (request) =>
        new Promise((resolve) => {
          release = () => resolve(flagPage(rows, request));
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
  vi.mocked(store.saved).mockRejectedValue(new Error("Storage unavailable"));
  vi.mocked(store.load).mockRejectedValue(new Error("Storage unavailable"));
  const cull = new PhotoCull("session", store);
  cull.saved = true;
  await expect(cull.probe()).resolves.toBeUndefined();
  expect(cull.saved).toBe(false);
  expect(cull.error).toBe("");
  expect(await cull.restore()).toBe(false);
  expect(cull.error).toBe("Storage unavailable");
  await cull.probe();
  expect(cull.error).toBe("Storage unavailable");
  vi.mocked(store.saved).mockResolvedValue(undefined);
  await cull.probe();
  expect(cull.error).toBe("Storage unavailable");
  vi.mocked(store.saved).mockRejectedValue(
    new CullFormatError({
      runID: "saved-run",
      revision: 0,
      position: 0,
      scope: "all",
      autoAdvance: true,
      formatVersion: 2,
    }),
  );
  await cull.probe();
  expect(cull.saved).toBe(false);
  expect(cull.error).toBe(
    "Saved cull progress uses an unsupported format. Update Docbank to resume it.",
  );
});

it("keeps the newer remainder result when an older flag read finishes", async () => {
  const row = { ...photo(1), flag: "pick" as api.PhotoBrowseRowFlag };
  const cull = new PhotoCull("session", storage());
  cull.run = {
    ids: [row.asset_id],
    checkpoint: {
      runID: "run",
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
  let release!: (page: api.PhotoFlagsResponse) => void;
  const oldLookup = new Promise<api.PhotoFlagsResponse>((resolve) => {
    release = resolve;
  });
  const stalePage = flagPage([{ ...row }], { asset_ids: [row.asset_id] });
  const list = vi
    .spyOn(api, "lookupPhotoFlags")
    .mockImplementation(async (request) => flagPage([row], request))
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
    const list = vi.spyOn(api, "lookupPhotoFlags").mockImplementation((request, options) => {
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
  vi.spyOn(api, "lookupPhotoFlags").mockRejectedValue(new Error("Lookup unavailable"));
  const check = cull.checkRemainder();
  expect(cull.busy).toBe(true);
  await check;
  expect(cull.error).toBe("Lookup unavailable");
  expect(cull.progress).toBe("");
  expect(cull.busy).toBe(false);
});

it.each(["capture", "next pass", "remainder"])(
  "recovers the session after a 401 during %s",
  async (site) => {
    const store = storage();
    const onauthfailure = vi.fn();
    const cull = new PhotoCull("session", store, onauthfailure);
    const cause = new APIError("Session expired", 401, "unauthorized");
    const capture = vi
      .spyOn(api, "createWorkspaceQuery")
      .mockImplementation(async () => Response.json(await snapshot([photo(1).asset_id])));
    if (site === "capture") {
      capture.mockRejectedValueOnce(cause);
      await cull.start(query);
    } else {
      await cull.start(query);
      await cull.flush();
      cull.move(1);
      await cull.flush();
      vi.spyOn(api, "lookupPhotoFlags").mockRejectedValueOnce(cause);
      if (site === "next pass") await cull.nextPass();
      else await cull.checkRemainder();
    }
    expect(onauthfailure).toHaveBeenCalledExactlyOnceWith(cause);
    expect(cull.error).toBe("Session expired");
  },
);

it.each(["left at first", "current thumbnail", "right after completion", "next pass"])(
  "preserves another tab's checkpoint after %s",
  async (navigation) => {
    const store = storage();
    vi.spyOn(api, "createWorkspaceQuery").mockResolvedValue(
      Response.json(await snapshot([photo(1).asset_id, photo(2).asset_id])),
    );
    const first = new PhotoCull("session", store);
    await first.start(query);
    if (navigation === "right after completion") {
      first.move(1);
      first.move(1);
    }
    await first.flush();
    const other = new PhotoCull("session", photoCullStorage("synthetic-vault"));
    await other.restore();
    const saved = await store.saved();
    vi.mocked(store.checkpoint).mockClear();
    if (navigation === "current thumbnail" || navigation === "next pass")
      first.choose(first.currentID!);
    else first.move(navigation === "left at first" ? -1 : 1);
    await first.flush();
    expect(store.checkpoint).not.toHaveBeenCalled();
    expect(await store.saved()).toEqual(saved);
    expect(await other.verifyDecision()).toBe(true);
    if (navigation === "next pass") {
      vi.spyOn(api, "listPhotoAssets").mockImplementation(async (request) =>
        flagPage([photo(1), photo(2)], request),
      );
      await first.nextPass(false, () => true);
    } else first.move(navigation === "right after completion" ? -1 : 1);
    await first.flush();
    expect(await other.verifyDecision()).toBe(false);
    expect(other.error).toContain("Close and resume it.");
  },
);

it("reads a pass remainder once until a flag change and lets Previous cancel the read", async () => {
  const rows = [photo(1), photo(2)],
    ids = rows.map((row) => row.asset_id);
  vi.spyOn(api, "createWorkspaceQuery").mockResolvedValue(Response.json(await snapshot(ids)));
  const list = vi
    .spyOn(api, "listPhotoAssets")
    .mockImplementation(async (request) => browsePage(rows, request));
  const cull = new PhotoCull("session", storage());
  await cull.start(query);
  cull.move(1);
  cull.move(1);
  await cull.checkRemainder();
  await cull.checkRemainder();
  expect(list).toHaveBeenCalledOnce();
  expect(cull.remainder).toBe(true);
  rows[0]!.flag = "pick";
  rows[1]!.flag = "pick";
  cull.flagsChanged();
  await cull.checkRemainder();
  expect(list).toHaveBeenCalledTimes(2);
  expect(cull.remainder).toBe(false);

  cull.flagsChanged();
  list.mockImplementation(
    (_request, options) =>
      new Promise((_resolve, reject) =>
        options!.signal!.addEventListener("abort", () => reject(options!.signal!.reason)),
      ),
  );
  const pending = cull.checkRemainder();
  await vi.waitFor(() => expect(list).toHaveBeenCalledTimes(3));
  expect(cull.checkingRemainder).toBe(true);
  cull.move(-1);
  await pending;
  expect(list.mock.calls[2]![1]!.signal!.aborted).toBe(true);
  expect(cull.currentID).toBe(ids[1]);
  expect(cull.busy).toBe(false);
  expect(cull.error).toBe("");
});

it("verifies a decision against progress queued before the check", async () => {
  const ids = [photo(1).asset_id, photo(2).asset_id],
    store = storage();
  vi.spyOn(api, "createWorkspaceQuery").mockResolvedValue(Response.json(await snapshot(ids)));
  const cull = new PhotoCull("session", store);
  await cull.start(query);
  await cull.flush();
  const read = photoCullStorage("synthetic-vault").saved;
  let release!: () => void;
  const gate = new Promise<void>((resolve) => (release = resolve));
  vi.mocked(store.saved).mockImplementationOnce(async () => {
    const saved = await read();
    await gate;
    return saved;
  });
  const verified = cull.verifyDecision();
  await vi.waitFor(() => expect(store.saved).toHaveBeenCalled());
  cull.unavailable(ids[1]!);
  release();
  expect(await verified).toBe(true);
  await cull.flush();
  expect(cull.stale).toBe(false);
  expect((await store.load())!.checkpoint.unavailable).toEqual([ids[1]]);
});

it("refreshes 20,001 flags sequentially and records absent IDs as unavailable", async () => {
  const ids = Array.from({ length: 20_001 }, (_, index) => photo(index + 1).asset_id);
  const store = storage();
  const cull = new PhotoCull("session", store);
  cull.run = {
    ids,
    checkpoint: {
      runID: "run",
      passID: "pass",
      revision: 0,
      pass: ids,
      position: ids.length,
      visited: [],
      skipped: [],
      unavailable: [],
      scope: "unflagged",
      autoAdvance: true,
    },
  };
  await store.replace(cull.run);
  let release!: (value: api.PhotoFlagsResponse) => void;
  const lookup = vi
    .spyOn(api, "lookupPhotoFlags")
    .mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          release = resolve;
        }),
    )
    .mockImplementationOnce(async () => {
      expect(cull.progress).toBe("Checking 20000 of 20001 photos…");
      return { items: [] };
    });
  const checking = cull.checkRemainder();
  await vi.waitFor(() => expect(lookup).toHaveBeenCalledOnce());
  expect(cull.progress).toBe("Checking 0 of 20001 photos…");
  expect(lookup.mock.calls[0]![0]).toEqual({ asset_ids: ids.slice(0, 20_000) });
  expect(lookup.mock.calls[0]![1]).toMatchObject({
    session: "session",
    signal: expect.any(AbortSignal),
  });
  release({ items: ids.slice(0, 20_000).map((asset_id) => ({ asset_id, flag: "pick" })) });
  await checking;
  await cull.flush();
  expect(cull.progress).toBe("");
  expect(lookup).toHaveBeenCalledTimes(2);
  expect(lookup.mock.calls[1]![0]).toEqual({ asset_ids: [ids[20_000]] });
  expect(cull.run.checkpoint.unavailable).toEqual([ids[20_000]]);
  expect(cull.error).toBe("");
  expect(cull.stale).toBe(false);
  expect((await store.load())?.checkpoint.unavailable).toEqual([ids[20_000]]);
  expect(cull.remainder).toBe(false);
});

it("skips the flag request for an empty run", async () => {
  const cull = new PhotoCull("session", storage());
  cull.run = {
    ids: [],
    checkpoint: {
      runID: "run",
      passID: "pass",
      revision: 0,
      pass: [],
      position: 0,
      visited: [],
      skipped: [],
      unavailable: [],
      scope: "unflagged",
      autoAdvance: true,
    },
  };
  const lookup = vi.spyOn(api, "lookupPhotoFlags");
  await cull.checkRemainder();
  expect(lookup).not.toHaveBeenCalled();
  expect(cull.remainder).toBe(false);
});
