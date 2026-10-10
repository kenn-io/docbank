import { afterEach, expect, it, vi } from "vitest";
import { Photos, loadDensity } from "./photos.svelte.js";
import { photo } from "./photo-test-fixtures.js";

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); localStorage.clear(); });
const response = (items: ReturnType<typeof photo>[], cursor?: string) => new Response(JSON.stringify({ items, total: 3, next_cursor: cursor }));
const stubPhotoFetch = (fetcher: (url: string, init: RequestInit) => Promise<Response>, facets: unknown[] = []) => vi.stubGlobal("fetch", (url: string, init: RequestInit) => url.endsWith("/photos/assets/query") && JSON.parse(init.body as string).page_size === 1 ? Promise.resolve(new Response(JSON.stringify({ facets }))) : fetcher(url, init));

it.each(["hide", "unhide", "trash"])("reconciles other views after a lost %s reply and retains uncertain targets", async kind => {
  const fetcher = vi.fn().mockRejectedValueOnce(new TypeError("Reply lost after commit"))
    .mockRejectedValueOnce(new Error("Refresh unavailable"));
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn(), kind === "unhide");
  photos.items = [photo(1)]; photos.started = true; photos.selectLoaded();
  const changed = vi.fn();
  if (kind === "trash") expect(await photos.trashSelected(undefined, changed)).toBe(false);
  else await photos.setHidden("photo-1", undefined, changed);
  expect(changed).toHaveBeenCalledOnce();
  expect(photos.items).toEqual([photo(1)]);
  expect(photos.trashTargets).toEqual([photo(1)]);
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  photos.clearSelection();
  if (kind === "trash") await photos.trashSelected(undefined, changed);
  else await photos.setHidden("missing", undefined, changed);
  expect(changed).toHaveBeenCalledOnce();
  expect(fetcher).toHaveBeenCalledTimes(2);
  photos.dispose();
});

it.each([
  ["hidden_not_configured", 409, "Set a passcode in the Hidden view first."],
  ["hidden_locked", 403, "This photo is already hidden."],
])("explains a rejected hide with %s", async (code, status, message) => {
  const fetcher = vi.fn().mockResolvedValueOnce(Response.json({ code, detail: "Server failure" }, { status: Number(status) }))
    .mockResolvedValueOnce(response([photo(1)]));
  stubPhotoFetch(fetcher);
  const authFailure = vi.fn();
  const photos = new Photos("scoped", authFailure);
  photos.items = [photo(1)]; photos.started = true;
  await photos.setHidden("photo-1");
  expect(photos.actionError).toBe(`1 photo failed: ${message}`);
  expect(authFailure).not.toHaveBeenCalled();
  photos.dispose();
});

it("binds retries to captured or displayed revisions", async () => {
  const fetcher = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ id: "photo-1", revision: 2 })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Photo changed" }), { status: 412 }))
    .mockResolvedValueOnce(response([photo(3)]))
    .mockResolvedValueOnce(new Response(JSON.stringify({ id: "photo-2", revision: 2 })))
    .mockResolvedValueOnce(response([photo(3)]));
  stubPhotoFetch(fetcher);
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
  stubPhotoFetch(loadedFetcher);
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
  stubPhotoFetch(fetcher);
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
  stubPhotoFetch(fetcher);
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
  stubPhotoFetch(fetcher);
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
  stubPhotoFetch(fetcher);
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
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore();
  await photos.loadMore();
  await photos.refresh();
  expect(timers).toHaveLength(2);
  expect(photos.error).toBe("");
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-1", "photo-2"]);
  photos.dispose();
});

it.each([
  { kind: "hide", partial: true },
  { kind: "unhide", partial: true },
  { kind: "trash", partial: true },
  { kind: "trash", partial: false },
])("$kind writes keep failures, position and pending targets after failed refresh, partial=$partial", async ({ kind, partial }) => {
  let finish!: (response: Response) => void;
  let writeSignal: AbortSignal | undefined;
  let readSignal: AbortSignal | undefined;
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(1), photo(2), photo(3)]))
    .mockImplementationOnce((_url, options: RequestInit) => { readSignal = options.signal!; return new Promise(resolve => finish = resolve); })
    .mockImplementationOnce(async (_url, options: RequestInit) => { writeSignal = options.signal!; return Response.json({ id: "photo-1", revision: 2 }); });
  if (partial) fetcher.mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Photo changed" }), { status: 412 }));
  fetcher.mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Listing unavailable" }), { status: 503 }));
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn(), kind === "unhide");
  await photos.loadMore();
  photos.total = 7;
  photos.selection = { selectedIDs: new Set(partial ? ["photo-1", "photo-2"] : ["photo-1"]), anchorID: "photo-1" };
  photos.trashTargets = partial ? [photo(1), photo(2)] : [photo(1)];
  photos.scrollTop = 480;
  const oldRead = photos.refresh();
  const restore = vi.fn(async () => {});
  if (kind === "trash") expect(await photos.trashSelected(() => restore)).toBe(!partial);
  else await photos.setHidden("photo-1", () => restore);
  expect(readSignal?.aborted).toBe(true);
  expect(writeSignal).toBeDefined();
  expect(writeSignal?.aborted).toBe(false);
  finish(response([photo(1), photo(2), photo(3)])); await oldRead;
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-2", "photo-3"]);
  expect([...photos.selection.selectedIDs]).toEqual(partial ? ["photo-2"] : []);
  expect(photos.trashTargets.map(item => item.asset_id)).toEqual(partial ? ["photo-2"] : []);
  expect(photos.total).toBe(6);
  expect(photos.scrollTop).toBe(480);
  expect(restore).toHaveBeenCalledOnce();
  expect(kind === "trash" ? photos.trashError : photos.actionError).toBe(partial ? kind === "trash" ? "Photo changed" : "1 photo failed: Photo changed" : "");
  expect(photos.error).toBe("Listing unavailable");
  fetcher.mockResolvedValueOnce(response([photo(2), photo(3)]));
  await photos.retry();
  expect(kind === "trash" ? photos.trashError : photos.actionError).toBe(partial ? kind === "trash" ? "Photo changed" : "1 photo failed: Photo changed" : "");
  fetcher.mockImplementationOnce(() => new Promise(resolve => finish = resolve));
  const items = photos.items;
  const late = photos.refresh(); photos.dispose(); finish(response([photo(1)])); await late;
  expect(photos.items).toBe(items);
  photos.dispose();
});

it.each(["unhide", "trash"])("notifies after a pending %s finishes on a disposed Hidden store", async kind => {
  let finish!: (response: Response) => void;
  let signal: AbortSignal | undefined;
  const fetcher = vi.fn((_url: string, options: RequestInit) => {
    signal = options.signal!;
    return new Promise<Response>(resolve => finish = resolve);
  });
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn(), true);
  photos.items = [photo(1), photo(2)]; photos.started = true; photos.selectLoaded();
  const changed = vi.fn();
  const write = kind === "unhide" ? photos.setHidden("photo-1", undefined, changed) : photos.trashSelected(undefined, changed);
  photos.dispose();
  expect(signal?.aborted).toBe(false);
  finish(Response.json({ id: "photo-1", revision: 2 }));
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
  finish(Response.json({ id: "photo-2", revision: 2 }));
  await write;
  expect(changed).toHaveBeenCalledOnce();
  expect(fetcher.mock.calls.map(([url]) => url)).toEqual([`/api/v1/photos/assets/photo-1/${kind}`, `/api/v1/photos/assets/photo-2/${kind}`]);
});

it("bounds selection writes, rejects unconfirmed success and guards overlapping actions", async () => {
  const timers: AbortController[] = [];
  vi.spyOn(AbortSignal, "timeout").mockImplementation(() => { const timer = new AbortController(); timers.push(timer); return timer.signal; });
  let finish!: (response: Response) => void;
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(1)]))
    .mockImplementationOnce(() => new Promise(resolve => finish = resolve))
    .mockResolvedValueOnce(response([photo(1)]));
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore(); photos.selectLoaded();
  const write = photos.setHidden("photo-1");
  await photos.setQuery({ ...photos.query, text: "harbor" });
  expect(photos.query.text).toBe("");
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  await photos.setHidden("photo-1");
  expect(await photos.trashSelected()).toBe(false);
  expect(fetcher).toHaveBeenCalledTimes(2);
  expect(timers).toHaveLength(1);
  finish(Response.json({ id: "photo-2", revision: 2 })); await write;
  expect(photos.items).toHaveLength(1);
  expect(photos.actionError).toContain("did not confirm");
  expect(photos.hiding).toBe(false);
  photos.dispose();
});

it("reports unresolved visibility selections before sending requests", async () => {
  const fetcher = vi.fn(); stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1)]; photos.selectLoaded(); photos.selection.selectedIDs.add("unloaded");
  const report = vi.fn();
  await photos.setHidden("photo-1", undefined, undefined, report);
  expect(photos.actionError).toContain("Load and select");
  expect(report).toHaveBeenCalledWith(photos.actionError);
  expect(fetcher).not.toHaveBeenCalled();
  photos.dispose();
});

it("publishes and pages rows before optional counts, and isolates count retry, cancellation and stale scopes", async () => {
  const counts: { request: any; signal: AbortSignal; finish: (value: Response) => void }[] = [];
  const rows = vi.fn().mockResolvedValueOnce(response([photo(1)], "next")).mockResolvedValueOnce(response([photo(2)], "later"));
  vi.stubGlobal("fetch", (url: string, init: RequestInit) => {
    const request = JSON.parse(init.body as string);
    if (request.page_size !== 1) return rows(url, init);
    return new Promise<Response>(finish => counts.push({ request, signal: init.signal!, finish }));
  });
  const photos = new Photos("scope", vi.fn());
  await photos.loadMore();
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-1"]);
  expect(photos.loading).toBe(false);
  expect(photos.facetsLoading).toBe(true);
  await photos.loadMore();
  expect(photos.items).toHaveLength(2);
  expect(photos.cursor).toBe("later");
  expect(rows.mock.calls.map(call => JSON.parse(call[1].body).facets)).toEqual([[], []]);
  photos.cancelPending();
  expect(counts[0].signal.aborted).toBe(true);
  const resumed = photos.resume();
  expect(counts).toHaveLength(2);
  const facet = { dimension: "camera", available: true, total: 3, values: [], missing: 3, other: 0 };
  counts[0].finish(new Response(JSON.stringify({ facets: [{ ...facet, total: 99 }] })));
  counts[1].finish(new Response(JSON.stringify({ items: [photo(99)], total: 99, next_cursor: "wrong", facets: [facet] })));
  await resumed;
  expect(photos.facets).toEqual([facet]);
  expect(photos.total).toBe(3);
  expect(photos.cursor).toBe("later");
  expect(photos.items).toHaveLength(2);
  rows.mockResolvedValueOnce(response([photo(3), photo(6)], "refresh-tail"));
  await photos.refresh();
  expect(photos.items[0].asset_id).toBe("photo-3");
  counts[2].finish(new Response(JSON.stringify({ detail: "Counts unavailable" }), { status: 503 }));
  await vi.waitFor(() => expect(photos.facetsError).toBe("Counts unavailable"));
  expect(photos.error).toBe("");
  rows.mockResolvedValueOnce(response([photo(4)], "stale-tail"));
  await photos.loadMore();
  expect(photos.items).toHaveLength(3);
  const retry = photos.retryFacets();
  let finishStale!: (response: Response) => void;
  rows.mockImplementationOnce(() => new Promise(resolve => finishStale = resolve));
  const staleRows = photos.loadMore();
  rows.mockResolvedValueOnce(response([photo(5)], "ranked"));
  await photos.setQuery({ ...photos.query, text: "Canon", sort: { field: "relevance", direction: "desc" } });
  expect(rows.mock.calls[4][1].signal.aborted).toBe(true);
  finishStale(response([photo(88)])); await staleRows;
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-5"]);
  expect(counts[3].signal.aborted).toBe(true);
  counts[3].finish(new Response(JSON.stringify({ facets: [facet] })));
  await retry;
  expect(photos.facets).toEqual([]);
  expect(counts[4].request.query.text).toBe("Canon");
  expect(counts[4].request.query.sort).toEqual({ field: "capture_time", direction: "desc" });
  expect(counts[4].request.facets).toEqual(["camera", "lens", "year", "location", "set"]);
  expect(counts[4].request.cursor).toBeUndefined();
  rows.mockResolvedValueOnce(response([photo(6)]));
  await photos.loadMore();
  const request = JSON.parse(rows.mock.calls[6][1].body);
  expect(request.query.text).toBe("Canon"); expect(request.cursor).toBe("ranked");
  expect(new Photos("other", vi.fn()).query.text).toBe("");
  for (const reason of ["member_budget_exceeded", "byte_budget_exceeded", "time_budget_exceeded"]) {
    const pending = counts.at(-1)!;
    pending.finish(new Response(JSON.stringify({ facets: [{ dimension: "camera", available: false, reason }] })));
    await vi.waitFor(() => expect(photos.facetsError).not.toBe(""));
    expect(photos.facetsRetryable).toBe(reason === "time_budget_exceeded");
    expect(photos.facetsError).toBe(reason === "time_budget_exceeded" ? "Some photo counts couldn't be loaded." : "Photo counts exceed the library's size limit. Narrow your search or filters.");
    void photos.retryFacets();
  }
  photos.dispose();
  expect(counts.at(-1)!.signal.aborted).toBe(true);
  counts.at(-1)!.finish(new Response(JSON.stringify({ facets: [facet] })));
  await vi.waitFor(() => expect(photos.items.map(item => item.asset_id)).toEqual(["photo-5", "photo-6"]));
});
