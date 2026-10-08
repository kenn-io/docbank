import { afterEach, expect, it, vi } from "vitest";
import { Photos, loadDensity } from "./photos.svelte.js";
import { photo } from "./photo-test-fixtures.js";
import * as snapshots from "./snapshots.js";
import { APIError } from "./api-transport.js";

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); localStorage.clear(); });
const response = (items: ReturnType<typeof photo>[], cursor?: string) => new Response(JSON.stringify({ items, total: 3, next_cursor: cursor }));

it("invalidates cached calendar counts when the grid refreshes", async () => {
  const first = { facets: [{ dimension: "capture_day", available: true, total: 1, missing: 0, other: 0, values: [] }] } as unknown as snapshots.FacetCounts;
  const second = { facets: [{ dimension: "capture_day", available: true, total: 2, missing: 0, other: 0, values: [] }] } as unknown as snapshots.FacetCounts;
  const create = vi.spyOn(snapshots, "createFacetCounts").mockResolvedValueOnce(first).mockResolvedValueOnce(second);
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response([photo(1), photo(2)])));
  const photos = new Photos("scoped", vi.fn());
  await photos.loadTimeline(); photos.setView("timeline"); photos.setView("grid");
  await photos.refresh();
  expect(photos.timeline).toBeUndefined();
  expect(create).toHaveBeenCalledTimes(1);
  photos.setView("timeline");
  await vi.waitFor(() => expect(photos.timeline?.total).toBe(2));
  expect(create.mock.calls[1][1].filters).toEqual({});
  photos.dispose();
});

it("keeps base counts across day changes and rejects a pre-refresh calendar reply", async () => {
  const first = { facets: [{ dimension: "capture_day", available: true, total: 9000, missing: 4, other: 0, values: [] }] } as unknown as snapshots.FacetCounts;
  const second = { facets: [{ dimension: "capture_day", available: true, total: 20, missing: 0, other: 0, values: [] }] } as unknown as snapshots.FacetCounts;
  let finish!: (page: snapshots.FacetCounts) => void;
  const create = vi.spyOn(snapshots, "createFacetCounts").mockImplementationOnce(() => new Promise(resolve => finish = resolve)).mockResolvedValueOnce(second);
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response([photo(1)])));
  const photos = new Photos("scoped", vi.fn());
  photos.setView("timeline");
  const oldSignal = create.mock.calls[0][2];
  photos.setView("grid");
  await photos.refresh();
  photos.setView("timeline");
  finish(first); await Promise.resolve();
  expect(oldSignal.aborted).toBe(true);
  await vi.waitFor(() => expect(photos.timeline?.total).toBe(20));
  expect(create.mock.calls[1][1].sort.field).toBe("capture_time");
  await photos.selectDay("2024-02-29");
  expect(create).toHaveBeenCalledTimes(2);
  expect(photos.timeline?.total).toBe(20);
  photos.dispose();
});

it("maps bounded snapshot failures to an unavailable calendar", async () => {
  vi.spyOn(snapshots, "createFacetCounts").mockRejectedValue(new APIError("Too many photos", 413, "snapshot_too_large"));
  const photos = new Photos("scoped", vi.fn());
  await photos.loadTimeline();
  expect(photos.timeline?.available).toBe(false);
  expect(photos.timelineError).toBe("");
  photos.dispose();
});

it("retries a failed timeline and resumes an interrupted timeline request", async () => {
  const receipt = { facets: [{ dimension: "capture_day", available: false, reason: "member_budget_exceeded", values: [] }] } as unknown as snapshots.FacetCounts;
  const create = vi.spyOn(snapshots, "createFacetCounts").mockRejectedValueOnce(new Error("Scope unavailable"))
    .mockImplementationOnce(() => new Promise(() => {})).mockResolvedValueOnce(receipt);
  const photos = new Photos("scoped", vi.fn());
  await photos.loadTimeline();
  expect(photos.timelineError).toBe("Scope unavailable");
  photos.setView("timeline");
  expect(photos.timelineLoading).toBe(true);
  photos.cancelPending();
  photos.started = true;
  await photos.resume();
  expect(create).toHaveBeenCalledTimes(3);
  await vi.waitFor(() => expect(photos.timeline?.available).toBe(false));
  photos.dispose();
});

it("seeks an unloaded day, pages within it, and restores the full scope", async () => {
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(1)], "old"))
    .mockResolvedValueOnce(response([photo(2)], "day-page"))
    .mockResolvedValueOnce(response([photo(3)]))
    .mockResolvedValueOnce(response([photo(1)], "full"));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore();
  photos.selectLoaded(); photos.scrollTop = 500;
  await photos.selectDay("2024-02-29");
  expect(photos.selection.selectedIDs.size).toBe(0);
  expect(photos.scrollTop).toBe(0);
  const request = JSON.parse(fetcher.mock.calls[1][1].body);
  expect(request.cursor).toBeUndefined();
  expect(request.query.filters).toEqual({ capture_after: "2024-02-29", capture_before: "2024-03-01" });
  await photos.loadMore();
  expect(JSON.parse(fetcher.mock.calls[2][1].body).cursor).toBe("day-page");
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-2", "photo-3"]);
  await photos.selectDay();
  expect(JSON.parse(fetcher.mock.calls[3][1].body).query.filters).toEqual({});
  expect(photos.day).toBeUndefined();
  photos.dispose();
});

it("ignores an older day response after a newer day succeeds and retries failure", async () => {
  let finish!: (value: Response) => void;
  const fetcher = vi.fn().mockImplementationOnce(() => new Promise(resolve => finish = resolve))
    .mockResolvedValueOnce(response([photo(2)]))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Try again" }), { status: 503 }))
    .mockResolvedValueOnce(response([photo(3)]));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  const older = photos.selectDay("2024-01-01");
  await photos.selectDay("2024-02-29");
  finish(response([photo(1)])); await older;
  expect(photos.items[0].asset_id).toBe("photo-2");
  await photos.selectDay("2025-01-01");
  expect(photos.error).toBe("Try again");
  await photos.retry();
  expect(photos.items[0].asset_id).toBe("photo-3");
  expect(JSON.parse(fetcher.mock.calls[3][1].body).query.filters.capture_after).toBe("2025-01-01");
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
it("hands timeline authentication failure to the session owner once", async () => {
  vi.spyOn(snapshots, "createFacetCounts").mockRejectedValue(new APIError("Expired", 401, "unauthorized"));
  const failure = vi.fn(); const photos = new Photos("scoped", failure);
  await photos.loadTimeline();
  expect(failure).toHaveBeenCalledTimes(1); expect(photos.timelineError).toBe(""); expect(photos.timeline).toBeUndefined();
  photos.dispose();
});
