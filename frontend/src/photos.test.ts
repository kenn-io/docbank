import { afterEach, expect, it, vi } from "vitest";
import { Photos, loadDensity, photoQuery } from "./photos.svelte.js";
import { photo } from "./photo-test-fixtures.js";

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); localStorage.clear(); });
const response = (items: ReturnType<typeof photo>[], cursor?: string) => new Response(JSON.stringify({ items, total: 3, next_cursor: cursor }));

it("cancels the previous scope and carries album sort through paging and refresh", async () => {
  let finish!: (response: Response) => void;
  const fetcher = vi.fn().mockImplementationOnce(() => new Promise(resolve => finish = resolve)).mockResolvedValueOnce(response([photo(2)], "album-next")).mockResolvedValueOnce(response([photo(3)])).mockResolvedValueOnce(response([photo(2), photo(3)]));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  const pending = photos.loadMore();
  photos.scrollTop = 1200; photos.selectLoaded(); photos.allResults = true;
  const query = { ...photoQuery, filters: { set_ids: ["album"] }, sort: { field: "added_time" as const, direction: "desc" as const } };
  await photos.setQuery(query);
  finish(response([photo(1)])); await pending;
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-2"]);
  expect(photos.selection.selectedIDs.size).toBe(0); expect(photos.allResults).toBe(false); expect(photos.scrollTop).toBe(0);
  await photos.loadMore(); await photos.refresh();
  expect(fetcher.mock.calls.slice(1).map(([, init]) => JSON.parse(init.body).query)).toEqual([query, query, query]);
  expect(fetcher.mock.calls[0][1].signal.aborted).toBe(true);
  photos.dispose();
});

it.each([false, true])("removes confirmed trash successes when refresh fails, partial=%s", async partial => {
  const fetcher = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ id: "photo-1", revision: 2 })));
  if (partial) fetcher.mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Photo changed" }), { status: 412 }));
  fetcher.mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Refresh unavailable" }), { status: 503 }));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1), photo(2)]; photos.total = 7; photos.started = true;
  if (partial) photos.selectLoaded();
  else photos.select("photo-1", new MouseEvent("click"), ["photo-1", "photo-2"]);
  expect(await photos.trashSelected()).toBe(!partial);
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-2"]);
  expect(photos.total).toBe(6);
  expect([...photos.selection.selectedIDs]).toEqual(partial ? ["photo-2"] : []);
  expect(photos.trashTargets.map(item => item.asset_id)).toEqual(partial ? ["photo-2"] : []);
  expect(photos.error).toBe("Refresh unavailable");
  photos.dispose();
});

it("binds retries to captured or displayed revisions", async () => {
  const fetcher = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ id: "photo-1", revision: 2 })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Photo changed" }), { status: 412 }))
    .mockResolvedValueOnce(response([photo(3)]))
    .mockResolvedValueOnce(new Response(JSON.stringify({ id: "photo-2", revision: 2 })))
    .mockResolvedValueOnce(response([photo(3)]));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1), photo(2)];
  photos.started = true;
  photos.selectLoaded();
  const invalidateDocuments = vi.fn();
  expect(await photos.trashSelected(undefined, invalidateDocuments)).toBe(false);
  expect(invalidateDocuments).toHaveBeenCalledTimes(1);
  expect(fetcher.mock.calls[0][0]).toBe("/api/v1/photos/assets/photo-1/trash");
  expect(new Headers(fetcher.mock.calls[0][1].headers).get("If-Match")).toBe("1");
  expect([...photos.selection.selectedIDs]).toEqual(["photo-2"]);
  expect(photos.trashError).toBe("Photo changed");
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-3"]);
  expect(photos.trashTargets.map(item => item.asset_id)).toEqual(["photo-2"]);
  expect(await photos.trashSelected(undefined, invalidateDocuments)).toBe(true);
  expect(fetcher.mock.calls[3][0]).toBe("/api/v1/photos/assets/photo-2/trash");
  expect(new Headers(fetcher.mock.calls[3][1].headers).get("If-Match")).toBe("1");
  expect(photos.trashTargets).toEqual([]);
  expect(invalidateDocuments).toHaveBeenCalledTimes(2);
  photos.selection.selectedIDs.add("unloaded");
  expect(await photos.trashSelected()).toBe(false);
  photos.dispose();

  const updated = { ...photo(1), revision: 2 };
  const loadedFetcher = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Photo changed" }), { status: 412 }))
    .mockResolvedValueOnce(response([updated]))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Photo changed again" }), { status: 412 }))
    .mockResolvedValueOnce(response([{ ...updated, revision: 3 }]));
  vi.stubGlobal("fetch", loadedFetcher);
  const loaded = new Photos("scoped", vi.fn()); loaded.items = [photo(1)]; loaded.started = true; loaded.selectLoaded();
  expect(await loaded.trashSelected()).toBe(false);
  expect(await loaded.trashSelected()).toBe(false);
  expect(loadedFetcher.mock.calls[2][0]).toBe("/api/v1/photos/assets/photo-1/trash");
  expect(new Headers(loadedFetcher.mock.calls[2][1].headers).get("If-Match")).toBe("2");
  expect(loaded.selection.selectedIDs.has("photo-1")).toBe(true);
  loaded.dispose();
});

it.each(["replace", "checkbox", "loaded", "add"])("uses current selection after a failure: %s", async mode => {
  const fetcher = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Photo changed" }), { status: 412 }))
    .mockResolvedValueOnce(response(mode === "checkbox" ? [photo(1), photo(2)] : [photo(2)]));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1), photo(2)]; photos.started = true;
  photos.select("photo-1", new MouseEvent("click"), ["photo-1", "photo-2"]);
  expect(await photos.trashSelected()).toBe(false);
  if (mode === "replace") photos.select("photo-2", new MouseEvent("click"), ["photo-2"]);
  if (mode === "checkbox") { photos.check("photo-1", false, false, ["photo-1", "photo-2"]); photos.check("photo-2", true, false, ["photo-1", "photo-2"]); }
  if (mode === "loaded") photos.selectLoaded();
  if (mode === "add") photos.check("photo-2", true, false, ["photo-2"]);
  const expected = mode === "add" ? ["photo-1", "photo-2"] : ["photo-2"];
  for (const id of expected) fetcher.mockResolvedValueOnce(new Response(JSON.stringify({ id, revision: 2 })));
  fetcher.mockResolvedValueOnce(response([]));
  expect(await photos.trashSelected()).toBe(true);
  expect(fetcher.mock.calls.slice(2, -1).map(call => call[0])).toEqual(expected.map(id => `/api/v1/photos/assets/${id}/trash`));
  expect(photos.trashTargets).toEqual([]);
  photos.dispose();
});

it("keeps earlier pages on failure, waits for Retry, and reuses the failed cursor", async () => {
  const fetcher = vi.fn();
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  let finish!: (response: Response) => void;
  let release!: () => void;
  fetcher.mockImplementationOnce(() => new Promise(resolve => finish = resolve))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Temporary read failure" }), { status: 503 }))
    .mockResolvedValueOnce(response([photo(2), photo(3)]));
  const restore = vi.fn(() => new Promise<void>(resolve => release = resolve));
  const preserve = vi.fn(() => restore);
  const initial = photos.loadMore(preserve);
  expect(preserve).not.toHaveBeenCalled();
  finish(response([photo(1)], "next-page"));
  await vi.waitFor(() => expect(restore).toHaveBeenCalledTimes(1));
  await photos.loadMore();
  expect(fetcher).toHaveBeenCalledTimes(1);
  expect(photos.loading).toBe(true);
  release(); await initial;
  restore.mockImplementation(async () => {});
  await photos.loadMore(preserve);
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-1"]);
  expect(photos.error).toBe("Temporary read failure");
  await photos.loadMore();
  expect(fetcher).toHaveBeenCalledTimes(2);
  await photos.retry(preserve);
  expect(preserve).toHaveBeenCalledTimes(2);
  expect(restore).toHaveBeenCalledTimes(2);
  expect(photos.items).toHaveLength(3);
  expect(JSON.parse(fetcher.mock.calls[2][1].body).cursor).toBe("next-page");
  expect(fetcher.mock.calls[0][1].headers.get("X-Docbank-Web-Session")).toBe("scoped");
  await photos.loadMore();
  expect(fetcher).toHaveBeenCalledTimes(3);
  photos.dispose();
});

it("stages multiple replacement pages and preserves accepted rows on a failed refresh", async () => {
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(1), photo(2)], "next"))
    .mockResolvedValueOnce(response([photo(3)], "old-tail"))
    .mockResolvedValueOnce(response([photo(4), photo(1)], "replacement"))
    .mockResolvedValueOnce(new Response("{}", { status: 503 }))
    .mockResolvedValueOnce(response([photo(4), photo(1)], "replacement"))
    .mockResolvedValueOnce(response([photo(2), photo(3)], "new-tail"));
  vi.stubGlobal("fetch", fetcher);
  const restore = vi.fn(async () => {});
  const preserve = vi.fn(() => restore);
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore(); await photos.loadMore();
  photos.select("photo-2", new MouseEvent("click"), ["photo-1", "photo-2", "photo-3"]);
  await photos.refresh(preserve);
  expect(preserve).not.toHaveBeenCalled();
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-1", "photo-2", "photo-3"]);
  expect(photos.cursor).toBe("old-tail");
  expect([...photos.selection.selectedIDs]).toEqual(["photo-2"]);
  expect(restore).not.toHaveBeenCalled();
  photos.cancelPending(); await photos.resume(preserve);
  expect(fetcher).toHaveBeenCalledTimes(4);
  await photos.retry(preserve);
  expect(preserve).toHaveBeenCalledTimes(1);
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-4", "photo-1", "photo-2", "photo-3"]);
  expect(photos.cursor).toBe("new-tail");
  expect(restore).toHaveBeenCalledTimes(1);
  fetcher.mockResolvedValueOnce(new Response(JSON.stringify({ code: "cursor_expired" }), { status: 422 }))
    .mockResolvedValueOnce(response([photo(4), photo(1), photo(2), photo(3)], "renewed"))
    .mockResolvedValueOnce(response([photo(5)], "newer-tail"));
  await photos.loadMore();
  await photos.retry();
  expect(JSON.parse(fetcher.mock.calls[7][1].body).cursor).toBeUndefined();
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-4", "photo-1", "photo-2", "photo-3", "photo-5"]);
  expect([...photos.selection.selectedIDs]).toEqual(["photo-2"]);
  photos.selectLoaded();
  fetcher.mockResolvedValueOnce(response([photo(1)], "more"))
    .mockResolvedValueOnce(response([photo(6), photo(7), photo(8), photo(9)], "bounded")).mockResolvedValueOnce(response([]));
  await photos.refresh();
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-1", "photo-6", "photo-7", "photo-8", "photo-9"]);
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  expect(photos.cursor).toBe("bounded");
  const boundedCalls = fetcher.mock.calls.length;
  fetcher.mockResolvedValueOnce(response([photo(9)], "shortcut")).mockResolvedValueOnce(response([]));
  await photos.refresh();
  expect(fetcher).toHaveBeenCalledTimes(boundedCalls + 2);
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-9"]);
  expect(photos.cursor).toBe("shortcut");
  let finishReplacement!: (response: Response) => void;
  fetcher.mockImplementationOnce(() => new Promise(resolve => finishReplacement = resolve))
    .mockResolvedValueOnce(response([photo(10), photo(11)], "resumed-refresh"));
  const interruptedRefresh = photos.refresh();
  photos.cancelPending();
  expect(photos.loading).toBe(false);
  const refreshStart = fetcher.mock.calls.length;
  await photos.resume(preserve);
  expect(JSON.parse(fetcher.mock.calls[refreshStart][1].body).cursor).toBeUndefined();
  finishReplacement(response([photo(99)])); await interruptedRefresh;
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-10", "photo-11"]);
  fetcher.mockResolvedValueOnce(new Response(JSON.stringify({ code: "cursor_expired" }), { status: 422 }))
    .mockImplementationOnce(() => new Promise(resolve => finishReplacement = resolve))
    .mockResolvedValueOnce(response([photo(10), photo(11)], "resumed-expiry"))
    .mockResolvedValueOnce(response([photo(12)], "forward"));
  await photos.loadMore();
  const interruptedExpiry = photos.retry();
  photos.cancelPending();
  const expiryStart = fetcher.mock.calls.length;
  await photos.resume(preserve);
  expect(JSON.parse(fetcher.mock.calls[expiryStart][1].body).cursor).toBeUndefined();
  finishReplacement(response([photo(99)])); await interruptedExpiry;
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-10", "photo-11", "photo-12"]);
  const completedCalls = fetcher.mock.calls.length;
  await photos.resume();
  expect(fetcher).toHaveBeenCalledTimes(completedCalls);
  const timer = new AbortController();
  vi.spyOn(AbortSignal, "timeout").mockReturnValue(timer.signal);
  fetcher.mockImplementationOnce((_url, init: RequestInit) => new Promise((_resolve, reject) => init.signal!.addEventListener("abort", () => reject(init.signal!.reason))));
  const refresh = photos.refresh();
  timer.abort(new DOMException("Timed out", "TimeoutError"));
  await refresh;
  expect(photos.error).toContain("timed out");
  photos.dispose();
});

it("ignores canceled reads while a later request continues", async () => {
  let finish!: (value: Response) => void;
  let finishNew!: (value: Response) => void;
  const fetcher = vi.fn().mockImplementationOnce(() => new Promise(resolve => finish = resolve))
    .mockResolvedValueOnce(response([photo(2)], "next"));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  const initial = photos.resume();
  photos.cancelPending();
  await photos.resume();
  finish(response([photo(1)])); await initial;
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-2"]);
  fetcher.mockImplementationOnce(() => new Promise(resolve => finish = resolve))
    .mockImplementationOnce(() => new Promise(resolve => finishNew = resolve));
  const old = photos.loadMore();
  photos.cancelPending();
  const current = photos.loadMore();
  finish(response([photo(3)])); await old;
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-2"]);
  expect(photos.loading).toBe(true);
  finishNew(response([photo(4)])); await current;
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-2", "photo-4"]);
  fetcher.mockResolvedValueOnce(new Response("{}", { status: 503 }))
    .mockImplementationOnce(() => new Promise(resolve => finish = resolve));
  await photos.refresh();
  const late = photos.retry();
  photos.dispose(); finish(response([photo(3)]));
  await late; await photos.resume();
  expect(photos.items[0].asset_id).toBe("photo-2");
});

it("maps single, modifier and checkbox clicks to selection", () => {
  const photos = new Photos("scoped", vi.fn());
  const ids = ["photo-0", "photo-1", "photo-2"];
  photos.select(ids[0], new MouseEvent("click"), ids);
  photos.select(ids[2], new MouseEvent("click", { shiftKey: true }), ids);
  expect(photos.selection.selectedIDs.size).toBe(3);
  photos.select(ids[1], new MouseEvent("click", { metaKey: true }), ids);
  expect(photos.selection.selectedIDs.has(ids[1])).toBe(false);
  photos.select(ids[2], new MouseEvent("click", { ctrlKey: true }), ids);
  expect(photos.selection.selectedIDs.has(ids[2])).toBe(false);
  photos.check(ids[1], true, false, ids);
  expect(photos.selection.selectedIDs.has(ids[1])).toBe(true);
  photos.select(ids[0], new MouseEvent("click"), ids);
  expect([...photos.selection.selectedIDs]).toEqual([ids[0]]);
  photos.dispose();
});

it("remembers density with safe defaults for unknown or unavailable storage", async () => {
  const photos = new Photos("scoped", vi.fn());
  photos.started = true;
  photos.scrollTop = 750;
  photos.items = [photo(1)];
  new Photos("scoped", vi.fn()).setDensity("large");
  photos.syncDensity();
  expect(photos.density).toBe("large");
  expect(photos.scrollTop).toBe(750);
  expect(photos.items).toEqual([photo(1)]);
  photos.setDensity("compact");
  expect(new Photos("scoped", vi.fn()).density).toBe("compact");
  localStorage.setItem("docbank.photos.density", "__proto__");
  expect(loadDensity()).toBe("comfortable");
  vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => { throw new Error("disabled"); });
  vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => { throw new Error("disabled"); });
  expect(loadDensity()).toBe("comfortable");
  expect(() => photos.setDensity("large")).not.toThrow();
  expect(photos.density).toBe("large");
  photos.dispose();
});

it("times each replacement page separately", async () => {
  const timers: AbortController[] = [];
  vi.spyOn(AbortSignal, "timeout").mockImplementation(() => { const timer = new AbortController(); timers.push(timer); return timer.signal; });
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(1)], "second")).mockResolvedValueOnce(response([photo(2)], "third"))
    .mockResolvedValueOnce(response([photo(1)], "refresh-second"))
    .mockImplementationOnce(async () => { timers[0].abort(new DOMException("Timed out", "TimeoutError")); return response([photo(2)], "third"); });
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore();
  await photos.loadMore();
  await photos.refresh();
  expect(timers).toHaveLength(2);
  expect(photos.error).toBe("");
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-1", "photo-2"]);
  photos.dispose();
});

it("verifies missing selections in bounded groups, intersects the asset filter, and retains newer choices", async () => {
  let release!: (value: Response) => void;
  const old = Array.from({ length: 66 }, (_, index) => photo(index));
  const prefix = Array.from({ length: 66 }, (_, index) => photo(index + 1000));
  const fetcher = vi.fn().mockResolvedValueOnce(response(prefix, "next"))
    .mockResolvedValueOnce(response(old.slice(1, 64))).mockImplementationOnce(() => new Promise(resolve => release = resolve));
  vi.stubGlobal("fetch", fetcher);
  const query = { ...photoQuery, filters: { set_ids: ["album"], asset_ids: old.slice(1, 65).map(item => item.asset_id) } };
  const photos = new Photos("scoped", vi.fn(), query); photos.items = old; photos.selectLoaded();
  const refresh = photos.refresh(); await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(3));
  photos.selection.selectedIDs.delete("photo-1"); photos.selection.selectedIDs.add("photo-2000");
  release(response([photo(64)])); await refresh;
  expect(photos.items).toEqual(prefix); expect(photos.cursor).toBe("next");
  expect(photos.selection.selectedIDs.has("photo-1")).toBe(false); expect(photos.selection.selectedIDs.has("photo-65")).toBe(false);
  expect(photos.selection.selectedIDs.has("photo-64")).toBe(true); expect(photos.selection.selectedIDs.has("photo-2000")).toBe(false);
  const verification = fetcher.mock.calls.slice(1).map(([, init]) => JSON.parse(init.body).query);
  for (const value of verification) expect(value).toMatchObject({ ...query, filters: { ...query.filters, asset_ids: expect.any(Array) } });
  expect(verification.flatMap(value => value.filters.asset_ids)).toEqual(query.filters.asset_ids);
});

it("retains rows and selection on verification failure, retries, and cancels verification on scope change", async () => {
  let release!: (value: Response) => void;
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(2)], "next"))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Verification unavailable" }), { status: 503 }))
    .mockResolvedValueOnce(response([photo(2)], "next")).mockResolvedValueOnce(response([photo(1)]));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn()); photos.items = [photo(1)]; photos.selectLoaded();
  await photos.refresh(); expect(photos.items).toEqual([photo(1)]); expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  expect(photos.error).toBe("Verification unavailable"); await photos.retry(); expect(photos.items).toEqual([photo(2)]);
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  fetcher.mockResolvedValueOnce(response([photo(3)], "next")).mockImplementationOnce(() => new Promise(resolve => release = resolve)).mockResolvedValueOnce(response([photo(4)]));
  const old = photos.refresh(); await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(6));
  await photos.setQuery({ ...photoQuery, filters: { set_ids: ["other"] } }); release(response([photo(1)])); await old;
  expect(photos.items).toEqual([photo(4)]); expect(photos.selection.selectedIDs.size).toBe(0);
});

it("preserves a deselected range anchor through refresh", async () => {
  const items = [photo(1), photo(2), photo(3)];
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response(items)));
  const photos = new Photos("scoped", vi.fn()); photos.items = items;
  const ids = items.map(item => item.asset_id);
  photos.select(ids[0], new MouseEvent("click"), ids);
  photos.select(ids[1], new MouseEvent("click", { ctrlKey: true }), ids);
  photos.select(ids[1], new MouseEvent("click", { ctrlKey: true }), ids);
  await photos.refresh();
  expect(photos.selection.anchorID).toBe(ids[1]);
  photos.select(ids[2], new MouseEvent("click", { shiftKey: true }), ids);
  expect([...photos.selection.selectedIDs]).toEqual(ids);
});

it.each([true, false])("verifies a displaced deselected anchor, included=%s", async included => {
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(3)], "next"))
    .mockResolvedValueOnce(response(included ? [photo(2)] : []));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn()); photos.items = [photo(2)];
  photos.selection = { selectedIDs: new Set(), anchorID: "photo-2" };
  await photos.refresh();
  expect(photos.selection.anchorID).toBe(included ? "photo-2" : undefined);
  expect(JSON.parse(fetcher.mock.calls[1][1].body).query.filters.asset_ids).toEqual(["photo-2"]);
});

it.each([true, false])("verifies photos selected while refresh waits, included=%s", async included => {
  let release!: (value: Response) => void;
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(3), photo(4)], "next"))
    .mockImplementationOnce(() => new Promise(resolve => release = resolve))
    .mockResolvedValueOnce(response(included ? [photo(2)] : []));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn()); photos.items = [photo(1), photo(2)];
  const ids = photos.items.map(item => item.asset_id);
  photos.select(ids[0], new MouseEvent("click"), ids);
  const refresh = photos.refresh();
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
  photos.select(ids[1], new MouseEvent("click", { ctrlKey: true }), ids);
  release(response([photo(1)])); await refresh;
  expect(photos.scope()).toEqual({ asset_ids: included ? ids : [ids[0]] });
  expect(photos.selection.anchorID).toBe(included ? ids[1] : undefined);
  expect(JSON.parse(fetcher.mock.calls[2][1].body).query.filters.asset_ids).toEqual([ids[1]]);
});

it("verifies 10,000 displaced selections with four concurrent checks and separate deadlines", async () => {
  const timers: AbortController[] = [];
  vi.spyOn(AbortSignal, "timeout").mockImplementation(() => { const timer = new AbortController(); timers.push(timer); return timer.signal; });
  const old = Array.from({ length: 10000 }, (_, index) => photo(index));
  let active = 0, maximum = 0;
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(20000)], "next"))
    .mockImplementation(async (_url, init) => {
      active++; maximum = Math.max(maximum, active);
      const ids: string[] = JSON.parse(init.body).query.filters.asset_ids;
      expect(ids.length).toBeLessThanOrEqual(64);
      if (Number(ids[0].slice(6)) >= 256) timers[1].abort(new DOMException("Timed out", "TimeoutError"));
      await Promise.resolve(); active--;
      return response(ids.map(id => photo(Number(id.slice(6)))));
    });
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn()); photos.items = [old[0]];
  photos.selection = { selectedIDs: new Set(old.map(item => item.asset_id)), anchorID: undefined };
  await photos.refresh();
  expect(photos.error).toBe(""); expect(photos.items).toEqual([photo(20000)]);
  expect(photos.selection.selectedIDs.size).toBe(10000);
  expect(maximum).toBe(4); expect(active).toBe(0);
  expect(fetcher).toHaveBeenCalledTimes(158); expect(timers).toHaveLength(158);
});
