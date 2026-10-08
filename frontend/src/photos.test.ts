import { afterEach, expect, it, vi } from "vitest";
import { Photos, loadDensity } from "./photos.svelte.js";
import { photo } from "./photo-test-fixtures.js";

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); localStorage.clear(); });
const response = (items: ReturnType<typeof photo>[], cursor?: string) => new Response(JSON.stringify({ items, total: 3, next_cursor: cursor }));

it("trashes selected assets at displayed revisions and retains off-prefix failures for retry and invalidates Documents after partial success", async () => {
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
  photos.dispose();
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
  photos.selection.selectedIDs.add("unloaded");
  expect(await photos.trashSelected()).toBe(false);
  photos.dispose();
});

it("retries a loaded photo at its displayed revision without accepting unseen changes", async () => {
  const updated = { ...photo(1), revision: 2 };
  const fetcher = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Photo changed" }), { status: 412 }))
    .mockResolvedValueOnce(response([updated]))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Photo changed again" }), { status: 412 }))
    .mockResolvedValueOnce(response([{ ...updated, revision: 3 }]));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn()); photos.items = [photo(1)]; photos.started = true; photos.selectLoaded();
  expect(await photos.trashSelected()).toBe(false);
  expect(await photos.trashSelected()).toBe(false);
  expect(fetcher.mock.calls[2][0]).toBe("/api/v1/photos/assets/photo-1/trash");
  expect(new Headers(fetcher.mock.calls[2][1].headers).get("If-Match")).toBe("2");
  expect(photos.selection.selectedIDs.has("photo-1")).toBe(true);
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
    .mockResolvedValueOnce(response([photo(6), photo(7), photo(8), photo(9)], "bounded"));
  await photos.refresh();
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-1", "photo-6", "photo-7", "photo-8", "photo-9"]);
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  expect(photos.cursor).toBe("bounded");
  const boundedCalls = fetcher.mock.calls.length;
  fetcher.mockResolvedValueOnce(response([photo(9)], "shortcut"));
  await photos.refresh();
  expect(fetcher).toHaveBeenCalledTimes(boundedCalls + 1);
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
