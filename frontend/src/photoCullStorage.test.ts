import { beforeEach, afterEach, expect, it, vi } from "vite-plus/test";
import { IDBFactory, IDBKeyRange, IDBDatabase } from "fake-indexeddb";
import { PhotoCull } from "./photoCull.svelte.js";
import { idbRequest, transactionDone } from "./indexedDB.js";
import { photoCullStorage, type CullRun } from "./photoCullStorage.js";

beforeEach(() => {
  vi.stubGlobal("indexedDB", new IDBFactory());
  vi.stubGlobal("IDBKeyRange", IDBKeyRange);
});
afterEach(() => vi.unstubAllGlobals());

function run(runID = "run"): CullRun {
  const ids = Array.from({ length: 14 }, (_, i) => `photo-${i}`);
  return {
    ids,
    checkpoint: {
      runID,
      revision: 0,
      pass: ids,
      position: 0,
      visited: ids,
      skipped: ids.slice(1),
      unavailable: ids.slice(2),
      scope: "unflagged",
      autoAdvance: true,
    },
  };
}

it("round trips membership and markers past index ten in numeric order", async () => {
  const store = photoCullStorage("vault-a");
  const captured = run();
  await store.replace(captured);
  expect(await photoCullStorage("vault-a").load()).toEqual(captured);
});

it("checkpoints position, markers and a replacement pass", async () => {
  const store = photoCullStorage("vault-a"),
    captured = run();
  await store.replace(captured);
  await store.checkpoint({
    runID: "run",
    revision: 0,
    position: 3,
    autoAdvance: false,
    marker: { kind: "skipped", ids: ["photo-0"], index: 13 },
  });
  expect((await store.load())?.checkpoint).toMatchObject({
    position: 3,
    revision: 1,
    autoAdvance: false,
    skipped: [...captured.checkpoint.skipped, "photo-0"],
  });
  const pass = {
    ...captured.checkpoint,
    pass: ["photo-4"],
    position: 0,
    visited: [],
    skipped: [],
    unavailable: [],
  };
  await store.checkpoint({ runID: "run", revision: 1, pass });
  expect(await store.load()).toEqual({ ids: captured.ids, checkpoint: { ...pass, revision: 2 } });
});

it("refuses writes from a handle whose run was replaced", async () => {
  const old = photoCullStorage("vault-a"),
    newer = photoCullStorage("vault-a");
  const captured = run();
  await old.replace(captured);
  const replacement = run("new-run");
  await newer.replace(replacement, undefined, captured.checkpoint);
  await expect(old.checkpoint({ runID: "run", revision: 0, position: 2 })).rejects.toThrow(
    "changed in another tab",
  );
  await expect(old.replace(captured, undefined, captured.checkpoint)).rejects.toThrow(
    "changed in another tab",
  );
  expect(await newer.load()).toEqual(replacement);
});

it("keeps separate vaults and reads only the saved header for the resume offer", async () => {
  const a = photoCullStorage("vault-a"),
    b = photoCullStorage("vault-b");
  expect(await a.saved()).toBeUndefined();
  await a.replace(run());
  await b.replace(run("other-run"));
  const transaction = vi.spyOn(IDBDatabase.prototype, "transaction");
  expect(await a.saved()).toEqual({
    formatVersion: 1,
    runID: "run",
    revision: 0,
    position: 0,
    scope: "unflagged",
    autoAdvance: true,
  });
  expect(transaction).toHaveBeenCalledExactlyOnceWith("checkpoints", "readonly");
  expect((await b.load())?.checkpoint.runID).toBe("other-run");
  expect((await a.load())?.checkpoint.runID).toBe("run");
});

it("refuses a newer saved format without replacing or checkpointing it", async () => {
  const store = photoCullStorage("vault-a");
  const captured = run();
  await store.replace(captured);
  const db = await idbRequest(indexedDB.open("docbank-photo-cull-v1", 1));
  try {
    const tx = db.transaction("checkpoints", "readwrite");
    const done = transactionDone(tx);
    const saved = await idbRequest<Record<string, unknown>>(
      tx.objectStore("checkpoints").get("vault-a"),
    );
    tx.objectStore("checkpoints").put({ ...saved, formatVersion: 2 }, "vault-a");
    await done;
    const cull = new PhotoCull("session", store);
    await cull.probe();
    expect(cull.saved).toBe(false);
    expect(await cull.restore()).toBe(false);
    expect(cull.run).toBeUndefined();
    expect(cull.error).toContain("unsupported format");
    await expect(store.replace(run("replacement"))).rejects.toThrow("changed in another tab");
    await expect(store.replace(run("replacement"), undefined, captured.checkpoint)).rejects.toThrow(
      "changed in another tab",
    );
    await expect(store.checkpoint({ runID: "run", revision: 0, position: 1 })).rejects.toThrow(
      "changed in another tab",
    );
    const read = db.transaction(["checkpoints", "members"], "readonly");
    const readDone = transactionDone(read);
    expect(await idbRequest(read.objectStore("checkpoints").get("vault-a"))).toEqual({
      ...saved,
      formatVersion: 2,
    });
    expect(await idbRequest(read.objectStore("members").get("vault-a"))).toEqual({
      ids: captured.ids,
      pass: captured.checkpoint.pass,
    });
    await readDone;
  } finally {
    db.close();
  }
});
