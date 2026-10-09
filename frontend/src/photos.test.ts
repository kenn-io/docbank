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

it("remembers density with safe defaults for unknown or unavailable storage", () => {
  const photos = new Photos("scoped", vi.fn());
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
  expect(photos.selection.selectedIDs.has("photo-64")).toBe(true); expect(photos.selection.selectedIDs.has("photo-2000")).toBe(true);
  expect(fetcher.mock.calls.slice(1).map(([, init]) => JSON.parse(init.body).query)).toEqual([
    { ...query, filters: { ...query.filters, asset_ids: query.filters.asset_ids.slice(0, 63) } },
    { ...query, filters: { ...query.filters, asset_ids: ["photo-64"] } },
  ]);
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

it("uses one deadline for all missing selection verification groups", async () => {
  const deadline = new AbortController();
  vi.spyOn(AbortSignal, "timeout").mockReturnValueOnce(new AbortController().signal).mockReturnValueOnce(deadline.signal).mockImplementation(() => new AbortController().signal);
  const old = Array.from({ length: 65 }, (_, index) => photo(index));
  const fetcher = vi.fn().mockResolvedValueOnce(response(Array.from({ length: 65 }, (_, index) => photo(index + 1000)), "next"))
    .mockResolvedValueOnce(response(old.slice(0, 64))).mockImplementationOnce(async () => { deadline.abort(new DOMException("Timed out", "TimeoutError")); return response([photo(64)]); });
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn()); photos.items = old; photos.selectLoaded(); await photos.refresh();
  expect(photos.items).toEqual(old); expect(photos.selection.selectedIDs.size).toBe(65); expect(photos.error).toContain("timed out");
});
