import { afterEach, expect, it, vi } from "vitest";
import { APIError } from "./api-transport.js";
import * as download from "./download.js";
import { Photos, loadDensity } from "./photos.svelte.js";
import { photo } from "./photo-test-fixtures.js";
import * as snapshots from "./snapshots.js";

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); localStorage.clear(); });
const response = (items: ReturnType<typeof photo>[], cursor?: string) => new Response(JSON.stringify({ items, total: 3, next_cursor: cursor }));

it("conceals after a confirmed hide while finishing the captured batch and reporting failures", async () => {
  let finish!: (response: Response) => void;
  const fetcher = vi.fn().mockResolvedValueOnce(Response.json({ id: "photo-1", revision: 2 }))
    .mockImplementationOnce(() => new Promise<Response>(resolve => finish = resolve));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1), photo(2)]; photos.started = true; photos.selectLoaded();
  const report = vi.fn();
  const completed = vi.fn();
  const conceal = vi.fn(async () => photos.dispose());
  const write = photos.setHidden("photo-1", undefined, completed, report, conceal);
  await vi.waitFor(() => expect(finish).toBeDefined());
  expect(conceal).toHaveBeenCalledOnce();
  expect(completed).not.toHaveBeenCalled();
  finish(Response.json({ detail: "Stale revision" }, { status: 412 }));
  await write;
  expect(completed).toHaveBeenCalledOnce();
  expect(report).toHaveBeenLastCalledWith("1 photo failed: Stale revision");
});

it.each([{ kind: "hide", refused: false }, { kind: "trash", refused: false }, { kind: "trash", refused: true }])("reconciles uncertain $kind outcomes and keeps refused targets, refused=$refused", async ({ kind, refused }) => {
  const fetcher = vi.fn();
  if (refused) fetcher.mockResolvedValueOnce(Response.json({ detail: "Stale revision" }, { status: 412 })).mockResolvedValueOnce(response([photo(1)]));
  else fetcher.mockRejectedValueOnce(new TypeError("Reply lost after commit")).mockRejectedValueOnce(new Error("Refresh unavailable"));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1)]; photos.started = true; photos.selectLoaded();
  const changed = vi.fn();
  if (kind === "trash") expect(await photos.trashSelected(undefined, changed)).toBe(false);
  else await photos.setHidden("photo-1", undefined, changed);
  expect(changed).toHaveBeenCalledTimes(refused ? 0 : 1);
  expect(photos.items).toEqual([photo(1)]);
  expect(photos.trashTargets).toEqual([photo(1)]);
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
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
    .mockResolvedValueOnce(response([photo(2)]))
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
  expect(fetcher.mock.calls[4][0]).toBe("/api/v1/photos/assets/photo-2/trash");
  expect(new Headers(fetcher.mock.calls[4][1].headers).get("If-Match")).toBe("1");
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
  if (mode !== "checkbox") fetcher.mockResolvedValueOnce(response([photo(1)]));
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
  expect(fetcher.mock.calls.filter(([url]) => url.endsWith("/trash")).slice(1).map(call => call[0])).toEqual(expected.map(id => `/api/v1/photos/assets/${id}/trash`));
  expect(photos.trashTargets).toEqual([]);
  photos.dispose();
});

it("keeps base counts across day changes and rejects a pre-refresh calendar reply", async () => {
  const first = { facets: [{ dimension: "capture_day", available: true, total: 9000, missing: 4, other: 0, values: [] }] } as unknown as snapshots.PhotoTimelineCounts;
  const second = { facets: [{ dimension: "capture_day", available: true, total: 20, missing: 0, other: 0, values: [] }] } as unknown as snapshots.PhotoTimelineCounts;
  let finish!: (page: snapshots.PhotoTimelineCounts) => void;
  const create = vi.spyOn(snapshots, "fetchPhotoTimeline").mockResolvedValueOnce(first)
    .mockImplementationOnce(() => new Promise(resolve => finish = resolve)).mockResolvedValueOnce(second);
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response([photo(1)])));
  const photos = new Photos("scoped", vi.fn());
  await photos.loadTimeline();
  photos.setView("timeline");
  const pending = photos.loadTimeline();
  const oldSignal = create.mock.calls[1][2];
  photos.setView("grid");
  await photos.refresh();
  expect(photos.timeline).toBeUndefined();
  photos.setView("timeline");
  finish(first); await pending;
  expect(oldSignal.aborted).toBe(true);
  await vi.waitFor(() => expect(photos.timeline?.total).toBe(20));
  expect(create.mock.calls[2][1].sort.field).toBe("capture_time");
  expect(create.mock.calls[2][1].filters).toEqual({});
  await photos.selectDate("2024-02-29");
  expect(create).toHaveBeenCalledTimes(3);
  expect(photos.timeline?.total).toBe(20);
  photos.dispose();
});

it.each([
  [new APIError("Too many photos", 413, "snapshot_too_large"), "", 0, false],
  [new APIError("Expired", 401, "unauthorized"), "", 1, undefined],
  [new Error("Scope unavailable"), "Scope unavailable", 0, undefined],
])("maps timeline failure %s", async (error, message, failures, available) => {
  vi.spyOn(snapshots, "fetchPhotoTimeline").mockRejectedValue(error);
  const failure = vi.fn();
  const photos = new Photos("scoped", failure);
  await photos.loadTimeline();
  expect(photos.timeline?.available).toBe(available);
  expect(photos.timelineError).toBe(message);
  expect(failure).toHaveBeenCalledTimes(failures as number);
  photos.dispose();
});

it("resumes an interrupted timeline request", async () => {
  const receipt = { facets: [{ dimension: "capture_day", available: false, reason: "member_budget_exceeded", values: [] }] } as unknown as snapshots.PhotoTimelineCounts;
  const create = vi.spyOn(snapshots, "fetchPhotoTimeline").mockImplementationOnce(() => new Promise(() => {})).mockResolvedValueOnce(receipt);
  const photos = new Photos("scoped", vi.fn());
  photos.setView("timeline");
  expect(photos.timelineLoading).toBe(true);
  photos.cancelPending();
  photos.started = true;
  await photos.resume();
  expect(create).toHaveBeenCalledTimes(2);
  await vi.waitFor(() => expect(photos.timeline?.available).toBe(false));
  photos.dispose();
});

it("seeks an unloaded day, pages within it, and restores the full scope", async () => {
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(1)], "old"))
    .mockResolvedValueOnce(response([photo(4)], "year-page"))
    .mockResolvedValueOnce(response([photo(5)], "month-page"))
    .mockResolvedValueOnce(response([photo(2)], "day-page"))
    .mockResolvedValueOnce(response([photo(3)]))
    .mockResolvedValueOnce(response([photo(1)], "full"));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore();
  photos.selectLoaded(); photos.scrollTop = 500;
  await photos.selectDate("2025");
  expect(photos.items[0].asset_id).toBe("photo-4");
  expect(photos.date).toBe("2025");
  expect(photos.selection.selectedIDs.size).toBe(0);
  expect(JSON.parse(fetcher.mock.calls[1][1].body).cursor).toBeUndefined();
  await photos.selectDate("2025-12");
  expect(photos.items[0].asset_id).toBe("photo-5");
  expect(photos.date).toBe("2025-12");
  await photos.selectDate("2024-02-29");
  expect(photos.selection.selectedIDs.size).toBe(0);
  expect(photos.scrollTop).toBe(0);
  const request = JSON.parse(fetcher.mock.calls[3][1].body);
  expect(request.cursor).toBeUndefined();
  expect(request.query.filters.capture_after).toBe("2024-02-29");
  await photos.loadMore();
  expect(JSON.parse(fetcher.mock.calls[4][1].body).cursor).toBe("day-page");
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-2", "photo-3"]);
  await photos.selectDate();
  expect(photos.date).toBeUndefined();
  let finish!: (value: Response) => void;
  fetcher.mockImplementationOnce(() => new Promise(resolve => finish = resolve))
    .mockResolvedValueOnce(response([photo(2)]))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Try again" }), { status: 503 }))
    .mockResolvedValueOnce(response([photo(3)]));
  const older = photos.selectDate("2024-01-01");
  await photos.selectDate("2024-02-29");
  finish(response([photo(1)])); await older;
  expect(photos.items[0].asset_id).toBe("photo-2");
  await photos.selectDate("2025-01-01");
  expect(photos.error).toBe("Try again");
  await photos.retry();
  expect(photos.items[0].asset_id).toBe("photo-3");
  photos.dispose();
});

it("keeps a pending date change when the library refreshes", async () => {
  let finish!: (value: Response) => void;
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(1)], "old"))
    .mockImplementationOnce(() => new Promise(resolve => finish = resolve))
    .mockResolvedValueOnce(response([photo(2)]));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore();
  const pending = photos.selectDate("2024-02-29");
  await photos.refresh();
  finish(response([photo(3)])); await pending;
  expect(photos.date).toBe("2024-02-29");
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-2"]);
  expect(JSON.parse(fetcher.mock.calls[2][1].body).query.filters).toEqual({ capture_after: "2024-02-29", capture_before: "2024-03-01" });
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

it.each(["2024", undefined])("resumes interrupted date navigation to %s and keeps the accepted date on failure", async date => {
  let finish!: (value: Response) => void;
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(1)], "old"))
    .mockImplementationOnce(() => new Promise(resolve => finish = resolve))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Try again" }), { status: 503 }))
    .mockResolvedValueOnce(response([photo(2)], "new"));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  await photos.selectDate("2025-01-01");
  photos.selectLoaded(); photos.scrollTop = 500;
  const interrupted = photos.selectDate(date);
  photos.cancelPending();
  await photos.resume();
  finish(response([photo(99)])); await interrupted;
  expect(photos.error).toBe("Try again");
  expect(photos.date).toBe("2025-01-01");
  expect(photos.items[0].asset_id).toBe("photo-1");
  expect(photos.cursor).toBe("old");
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  expect(photos.scrollTop).toBe(500);
  await photos.retry();
  expect(photos.date).toBe(date);
  expect(photos.items[0].asset_id).toBe("photo-2");
  expect(photos.cursor).toBe("new");
  expect(photos.selection.selectedIDs.size).toBe(0);
  expect(photos.scrollTop).toBe(0);
  expect(JSON.parse(fetcher.mock.calls[2][1].body)).toEqual(JSON.parse(fetcher.mock.calls[3][1].body));
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

it.each([false, true].flatMap(hidden => ["success", "failure", "cancel"].map(outcome => ({ hidden, outcome }))))("revalidates off-prefix captured selections in bounded queries, hidden=$hidden outcome=$outcome", async ({ hidden, outcome }) => {
  const prefix = Array.from({ length: 250 }, (_, index) => photo(index + 1));
  const previous = prefix.map(item => ({ ...item, name: `Earlier ${item.name}` }));
  const captured = Array.from({ length: 65 }, (_, index) => photo(index + 1001));
  const photos = new Photos("scoped", vi.fn(), hidden);
  photos.items = previous; photos.started = true; photos.trashTargets = captured;
  photos.selection = { selectedIDs: new Set(captured.map(item => item.asset_id)), anchorID: captured[0].asset_id };
  let groups = 0;
  const fetcher = vi.fn(async (url: string, options: RequestInit) => {
    if (!url.endsWith("/query")) return Response.json({ detail:"Stale revision" },{ status:412 });
    const body = JSON.parse(String(options.body));
    expect(body.hidden).toBe(hidden);
    const ids: string[] | undefined = body.query.filters?.asset_ids;
    if (!ids) return response(prefix,"more");
    expect(ids.length).toBeLessThanOrEqual(64);
    groups++;
    if (groups === 2 && outcome === "failure") return Response.json({ detail:"Eligibility unavailable" },{status:503});
    if (groups === 2 && outcome === "cancel") photos.cancelPending();
    return response(captured.filter(item => ids.includes(item.asset_id) && item !== captured[64]).map(item => ({ ...item, revision:7 })));
  });
  vi.stubGlobal("fetch",fetcher);
  expect(await photos.refresh()).toBe(outcome === "success");
  expect(groups).toBe(2);
  expect(photos.items).toEqual(outcome === "success" ? prefix : previous);
  if (outcome === "success") {
    expect([...photos.selection.selectedIDs]).toEqual(captured.slice(0,64).map(item => item.asset_id));
    expect(photos.trashTargets).toEqual(captured.slice(0,64));
    await photos.setHidden(captured[0].asset_id);
    const writes = fetcher.mock.calls.filter(([url]) => !url.endsWith("/query"));
    expect(writes).toHaveLength(64);
    expect(writes[0][0]).toBe(`/api/v1/photos/assets/${captured[0].asset_id}/${hidden ? "unhide" : "hide"}`);
    expect(writes.every(([,options]) => new Headers(options.headers).get("If-Match") === "1")).toBe(true);
  } else {
    expect(photos.trashTargets).toEqual(captured);
    expect(photos.selection.selectedIDs.size).toBe(65);
    expect(photos.error).toBe(outcome === "failure" ? "Eligibility unavailable" : "");
  }
  photos.dispose();
});

it.each([
  { kind: "hide", partial: true },
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
  const photos = new Photos("scoped", vi.fn());
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
  timers[0].abort(new DOMException("Timed out", "TimeoutError"));
  finish(Response.json({ id: "photo-1", revision: 2 })); await write;
  expect(photos.actionError).toContain("Timed out");
  expect(photos.hiding).toBe(false);
  fetcher.mockResolvedValueOnce(Response.json({ id: "photo-2", revision: 2 })).mockResolvedValueOnce(response([photo(1)]));
  await photos.setHidden("photo-1");
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

it.each([false, true])("looks up direct photos without replacing loaded results, hidden=%s", async hidden => {
 const item = photo(1); const fetcher = vi.fn().mockResolvedValueOnce(response([photo(2)])); vi.stubGlobal("fetch",fetcher);
 const photos = new Photos("scoped",vi.fn(), hidden); photos.items=[item];
 expect((await photos.lookup("photo-2",new AbortController().signal)).asset_id).toBe("photo-2");
 expect(photos.items).toEqual([item]);
 const fresh = {...item,content_version_id:"new-version"}; fetcher.mockResolvedValueOnce(response([fresh]));
 expect((await photos.lookup(item.asset_id,new AbortController().signal)).content_version_id).toBe("new-version"); expect(photos.items[0]).toEqual(item);
 expect(JSON.parse(fetcher.mock.calls[0][1].body).query.filters.asset_ids).toEqual(["photo-2"]);
 expect(JSON.parse(fetcher.mock.calls[0][1].body).hidden).toBe(hidden);
 expect(fetcher).toHaveBeenCalledTimes(2); photos.dispose();
});

it.each([{ hidden: false, status: 401 }, { hidden: true, status: 403 }])("retains prepared preview slots and applies access cleanup for lookup, preparation and original reads, hidden=$hidden", async ({ hidden, status }) => {
  const fetcher = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ state: "ready", generation_id: "fit-generation" })));
  vi.stubGlobal("fetch", fetcher); const onauth = vi.fn(); const photos = new Photos("scoped", onauth, hidden); const item = photo(1); const signal = new AbortController().signal;
  const slot = await photos.preview(item,"fit",signal); expect(item.previews.fit).toEqual(slot);
  expect(JSON.parse(fetcher.mock.calls[0][1].body).hidden).toBe(hidden);
  expect(await photos.preview(item,"fit",signal)).toEqual(slot); expect(fetcher).toHaveBeenCalledOnce();
  for (const operation of [() => photos.lookup(item.asset_id,signal), () => photos.preview(item,"large",signal)]) {
    fetcher.mockResolvedValueOnce(new Response(JSON.stringify({detail:"Session expired"}),{status}));
    await expect(operation()).rejects.toMatchObject({status});
  }
  fetcher.mockClear().mockImplementation(async (url: string) => url === "/api/v1/photos/assets/query"
    ? Response.json({code:"hidden_locked",detail:"Session expired"},{status})
    : Response.json({id:item.node_id,kind:"file",current_version_id:item.content_version_id,blob_hash:"a".repeat(64),size:10,mime_type:"image/jpeg",revision:1,name:item.name}));
  const read = vi.spyOn(download,"readVerifiedPreview").mockResolvedValue({kind:"image",url:"blob:original",mediaType:"image/jpeg"});
  await expect(photos.original(item,signal)).rejects.toMatchObject({status,code:"hidden_locked"});
  expect(fetcher.mock.calls.map(([url]) => url)).toEqual(["/api/v1/photos/assets/query"]);
  expect(read).not.toHaveBeenCalled();
  expect(onauth).toHaveBeenCalledTimes(3); photos.dispose();
});

it.each(["display", "excluded", "version", "download-display", "download-node", "download-version", "download-excluded", "download-error", "download-auth", "download-abort", "download-success"])("checks original photo binding for %s without adopting stale results", async scenario => {
  const old = photo(1), current = {...old, display_file_id:"other-file", node_id:2, content_version_id:"other-version"};
  const onauth = vi.fn(), photos = new Photos("scoped", onauth); photos.items = [old];
  const lookup = vi.spyOn(photos, "lookup"); if (scenario === "display") lookup.mockResolvedValue(current); else if (scenario === "version") lookup.mockResolvedValue(old); else lookup.mockRejectedValue(new APIError("This photo is no longer in Library.", 404, "not_found"));
  const fetcher = vi.fn(async () => new Response(JSON.stringify({id:1, kind:"file", current_version_id:old.content_version_id, blob_hash:"a".repeat(64), size:10, mime_type:"image/jpeg", revision:1, name:old.name})));
  vi.stubGlobal("fetch", fetcher); const read = vi.spyOn(download, "readVerifiedPreview").mockResolvedValue({kind:"image", url:"blob:original", mediaType:"image/jpeg"});
  if (scenario.startsWith("download-")) {
    Object.defineProperty(URL, "revokeObjectURL", {configurable:true, value:vi.fn()});
    lookup.mockResolvedValue(old);
    let finish!: (result: {kind:"image"; url:string; mediaType:string}) => void;
    read.mockImplementationOnce(() => new Promise(resolve => finish = resolve));
    const controller = new AbortController(), original = photos.original(old, controller.signal);
    await vi.waitFor(() => expect(read).toHaveBeenCalledOnce());
    if (scenario === "download-display") lookup.mockResolvedValue({...old, display_file_id:"other-file"});
    else if (scenario === "download-node") lookup.mockResolvedValue({...old, node_id:2});
    else if (scenario === "download-version") lookup.mockResolvedValue({...old, content_version_id:"other-version"});
    else if (scenario === "download-excluded") lookup.mockRejectedValue(new APIError("Excluded", 404, "not_found"));
    else if (scenario === "download-error") lookup.mockRejectedValue(new Error("Lookup failed"));
    else if (scenario === "download-auth") { lookup.mockRestore(); fetcher.mockResolvedValue(new Response(JSON.stringify({detail:"Session expired"}), {status:401})); }
    else if (scenario === "download-abort") controller.abort();
    const settled = scenario === "download-success" ? expect(original).resolves.toBe("blob:original")
      : scenario === "download-error" ? expect(original).rejects.toThrow("Lookup failed")
      : scenario === "download-auth" ? expect(original).rejects.toMatchObject({status:401})
      : scenario === "download-abort" ? expect(original).rejects.toMatchObject({name:"AbortError"})
      : expect(original).rejects.toMatchObject({status:409, code:"photo_display_changed"});
    finish({kind:"image", url:"blob:original", mediaType:"image/jpeg"}); await settled;
    if (scenario === "download-success") expect(URL.revokeObjectURL).not.toHaveBeenCalled();
    else expect(URL.revokeObjectURL).toHaveBeenCalledExactlyOnceWith("blob:original");
    expect(onauth).toHaveBeenCalledTimes(scenario === "download-auth" ? 1 : 0);
    expect(photos.items).toEqual([old]); return;
  }
  if (scenario === "version") fetcher.mockResolvedValue(new Response(JSON.stringify({current_version_id:"changed"})));
  await expect(photos.original(old, new AbortController().signal)).rejects.toMatchObject({status:409, code:"photo_display_changed"});
  expect(photos.items).toEqual([old]); expect(fetcher).toHaveBeenCalledTimes(scenario === "version" ? 1 : 0); expect(read).not.toHaveBeenCalled();
  if (scenario === "display") {
    fetcher.mockResolvedValue(new Response(JSON.stringify({id:2, kind:"file", current_version_id:current.content_version_id, blob_hash:"b".repeat(64), size:10, mime_type:"image/jpeg", revision:1, name:current.name})));
    expect(await photos.original(current, new AbortController().signal)).toBe("blob:original"); expect(read.mock.calls[0]![1].nodeID).toBe(2);
  }
});
