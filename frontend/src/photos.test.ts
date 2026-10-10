import { afterEach, expect, it, vi } from "vitest";
import { Photos, loadDensity } from "./photos.svelte.js";
import { photo } from "./photo-test-fixtures.js";

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); localStorage.clear(); });
const response = (items: ReturnType<typeof photo>[], cursor?: string) => new Response(JSON.stringify({ items, total: 3, next_cursor: cursor }));

it("freezes selected rejects scope and refuses a changed selection", async () => {
  const preview = { digest: "a".repeat(64), photos: 1, files: 1, unchanged: 0, mixed: [], mixed_count: 0 };
  const fetcher = vi.fn().mockResolvedValueOnce(Response.json(preview))
    .mockResolvedValueOnce(Response.json(preview)).mockResolvedValueOnce(response([]));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1)]; photos.selectLoaded();
  await photos.previewRejects(true);
  await photos.trashRejects();
  const first = JSON.parse(fetcher.mock.calls[0][1].body);
  expect(first.query.filters.asset_ids).toEqual(["photo-1"]);
  expect(JSON.parse(fetcher.mock.calls[1][1].body)).toEqual({ query: first.query, hidden: false, digest: preview.digest });
  photos.items = [photo(1)]; photos.selectLoaded();
  fetcher.mockResolvedValueOnce(Response.json(preview));
  await photos.previewRejects(true);
  photos.clearSelection();
  expect(await photos.trashRejects()).toBe(false);
  expect(photos.rejectsError).toContain("Selection changed");
  expect(fetcher).toHaveBeenCalledTimes(4);
  photos.dispose();
});

it.each([false, true])("bounds selected rejects scope while keeping Library available, selected=%s", async selected => {
  const fetcher = vi.fn().mockResolvedValue(Response.json({ digest: "a".repeat(64), photos: 1, files: 1, unchanged: 65, mixed: [], mixed_count: 0 }));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.selection.selectedIDs = new Set(Array.from({ length: 65 }, (_, i) => `photo-${i}`));
  await photos.previewRejects(selected);
  if (selected) {
    expect(photos.rejectsError).toContain("64 photos");
    expect(fetcher).not.toHaveBeenCalled();
  } else {
    expect(photos.rejectsSelected).toBe(false);
    expect(JSON.parse(fetcher.mock.calls[0][1].body).query.filters?.asset_ids).toBeUndefined();
    expect(photos.rejects?.photos).toBe(1);
  }
  photos.dispose();
});

it.each(["confirmed", "network", "server"])("recovers the loaded rejects range and surviving selection after a %s move and failed refresh", async outcome => {
  let finish!: (response: Response) => void;
  const preview = { digest: "a".repeat(64), photos: 1, files: 1, unchanged: 0, mixed: [], mixed_count: 0 };
  const fetcher = vi.fn().mockResolvedValueOnce(Response.json(preview));
  if (outcome === "confirmed") fetcher.mockResolvedValueOnce(Response.json(preview));
  else if (outcome === "server") fetcher.mockResolvedValueOnce(Response.json({ detail: "Internal error", code: "internal" }, { status: 500 }));
  else fetcher.mockRejectedValueOnce(new TypeError("Reply lost"));
  fetcher.mockResolvedValueOnce(Response.json({ detail: "Refresh unavailable" }, { status: 503 }))
    .mockImplementationOnce(() => new Promise<Response>(resolve => finish = resolve))
    .mockResolvedValueOnce(Response.json({ items: Array.from({ length: 250 }, (_, i) => photo(i + 2)), total: 749, next_cursor: "second" }))
    .mockResolvedValueOnce(Response.json({ items: Array.from({ length: 250 }, (_, i) => photo(i + 252)), total: 749, next_cursor: "remaining" }));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  const original = Array.from({ length: 500 }, (_, i) => photo(i + 1));
  photos.items = original; photos.total = 750; photos.cursor = "old"; photos.started = true; photos.scrollTop = 12_000;
  photos.selection = { selectedIDs: new Set(["photo-1", "photo-2", "photo-300", "photo-999"]), anchorID: "photo-300" };
  photos.trashTargets = [photo(999)];
  await photos.previewRejects();
  const changed = vi.fn(() => { expect(photos.items).toHaveLength(500); expect(photos.listingInvalid).toBe(true); });
  const restore = vi.fn(async () => expect(photos.scrollTop).toBe(12_000));
  const preserve = vi.fn(() => restore);
  expect(await photos.trashRejects(preserve, changed)).toBe(outcome === "confirmed");
  expect(changed).toHaveBeenCalledOnce();
  expect(photos.listingInvalid).toBe(true);
  expect(photos.items).toEqual(original);
  expect(photos.total).toBe(750);
  expect(photos.cursor).toBe("old");
  expect(photos.selection.selectedIDs.size).toBe(4);
  expect(photos.trashTargets).toEqual([]);
  expect(photos.error).toBe("Refresh unavailable");
  expect(photos.rejectsError).toBe(outcome === "confirmed" ? "" : "The move may have completed. Refresh Photos before trying again.");
  await photos.loadMore(); await photos.previewRejects(); await photos.trashSelected(); await photos.setHidden("photo-2");
  photos.select("photo-2", new MouseEvent("click"), ["photo-2"]); photos.check("photo-2", false, false, ["photo-2"]);
  photos.selectLoaded(); photos.clearSelection();
  expect(fetcher).toHaveBeenCalledTimes(3);
  expect(photos.selection.selectedIDs.size).toBe(4);
  expect(preserve).not.toHaveBeenCalled();
  const interrupted = photos.retry(preserve);
  photos.cancelPending();
  finish(response([photo(999)]));
  await interrupted;
  expect(photos.listingInvalid).toBe(true);
  expect(photos.items).toEqual(original);
  await photos.resume(preserve);
  expect(photos.error).toBe("");
  expect(photos.listingInvalid).toBe(false);
  expect(photos.items).toHaveLength(500);
  expect(photos.items[0].asset_id).toBe("photo-2");
  expect(photos.items.at(-1)?.asset_id).toBe("photo-501");
  expect([...photos.selection.selectedIDs]).toEqual(["photo-2", "photo-300"]);
  expect(photos.selection.anchorID).toBe("photo-300");
  expect(photos.cursor).toBe("remaining");
  expect(photos.total).toBe(749);
  expect(restore).toHaveBeenCalledOnce();
  photos.dispose();
});

it.each([[412, "stale_revision"], [503, "maintenance_busy"]] as const)("requires another rejects preview after a definite %s refusal without reconciling other views", async (status, code) => {
  const preview = { digest: "a".repeat(64), photos: 1, files: 1, unchanged: 0, mixed: [], mixed_count: 0 };
  const fetcher = vi.fn().mockResolvedValueOnce(Response.json(preview))
    .mockResolvedValueOnce(Response.json({ detail: "Move refused", code }, { status }))
    .mockResolvedValueOnce(response([photo(1)]));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  await photos.previewRejects(false);
  const changed = vi.fn();
  expect(await photos.trashRejects(undefined, changed)).toBe(false);
  expect(changed).not.toHaveBeenCalled();
  expect(photos.rejectsError).toBe("Move refused");
  expect(photos.rejects).toBeUndefined();
  expect(await photos.trashRejects()).toBe(false);
  expect(fetcher).toHaveBeenCalledTimes(3);
  photos.dispose();
});

it.each(["hide", "unhide", "trash"])("reconciles other views after a lost %s reply and retains uncertain targets", async kind => {
  const fetcher = vi.fn().mockRejectedValueOnce(new TypeError("Reply lost after commit"))
    .mockRejectedValueOnce(new Error("Refresh unavailable"));
  vi.stubGlobal("fetch", fetcher);
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
  vi.stubGlobal("fetch", fetcher);
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
  vi.stubGlobal("fetch", fetcher);
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
  vi.stubGlobal("fetch", fetcher);
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
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore(); photos.selectLoaded();
  const write = photos.setHidden("photo-1");
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
  const fetcher = vi.fn(); vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1)]; photos.selectLoaded(); photos.selection.selectedIDs.add("unloaded");
  const report = vi.fn();
  await photos.setHidden("photo-1", undefined, undefined, report);
  expect(photos.actionError).toContain("Load and select");
  expect(report).toHaveBeenCalledWith(photos.actionError);
  expect(fetcher).not.toHaveBeenCalled();
  photos.dispose();
});

it("previews oversized rejects without authorizing a move", async () => {
 const preview = { digest: "a".repeat(64), photos: 1001, files: 1001, unchanged: 0, mixed: [], mixed_count: 0 };
 const fetcher = vi.fn().mockResolvedValue(Response.json(preview));
 vi.stubGlobal("fetch", fetcher);
 const photos = new Photos("scoped", vi.fn());
 await photos.previewRejects(false);
 expect(photos.rejects).toEqual(preview);
 expect(await photos.trashRejects()).toBe(false);
 expect(fetcher).toHaveBeenCalledTimes(1);
 photos.dispose();
});
