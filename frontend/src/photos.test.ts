import { afterEach, expect, it, vi } from "vitest";
import { Photos, loadDensity } from "./photos.svelte.js";
import { photo } from "./photo-test-fixtures.js";

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); localStorage.clear(); });
const response = (items: ReturnType<typeof photo>[], cursor?: string) => new Response(JSON.stringify({ items, total: 3, next_cursor: cursor }));

it("keeps earlier pages on failure, waits for Retry, and reuses the failed cursor", async () => {
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(1)], "next-page"))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Temporary read failure" }), { status: 503 }))
    .mockResolvedValueOnce(response([photo(2), photo(3)]));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore();
  await photos.loadMore();
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-1"]);
  expect(photos.error).toBe("Temporary read failure");
  await photos.loadMore();
  expect(fetcher).toHaveBeenCalledTimes(2);
  await photos.retry();
  expect(photos.items).toHaveLength(3);
  expect(JSON.parse(fetcher.mock.calls[2][1].body).cursor).toBe("next-page");
  expect(fetcher.mock.calls[0][1].headers.get("X-Docbank-Web-Session")).toBe("scoped");
  await photos.loadMore();
  expect(fetcher).toHaveBeenCalledTimes(3);
  photos.dispose();
});

it("restarts paging when the previous cursor expires", async () => {
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(1)], "expired"))
    .mockResolvedValueOnce(new Response(JSON.stringify({ code: "cursor_expired" }), { status: 422 }))
    .mockResolvedValueOnce(response([photo(1)], "renewed"))
    .mockResolvedValueOnce(response([photo(2)]));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore();
  photos.select("photo-1", new MouseEvent("click"), ["photo-1"]);
  await photos.loadMore();
  await photos.retry();
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-1", "photo-2"]);
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  expect(JSON.parse(fetcher.mock.calls[2][1].body).cursor).toBeUndefined();
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
  await photos.retry(preserve);
  expect(preserve).toHaveBeenCalledTimes(1);
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-4", "photo-1", "photo-2", "photo-3"]);
  expect(photos.cursor).toBe("new-tail");
  expect(restore).toHaveBeenCalledTimes(1);
  photos.dispose();
});

it("finishes a replacement when the former tail disappeared and drops only removed selections", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(response([photo(1), photo(2)]))
    .mockResolvedValueOnce(response([photo(1)], "more"))
    .mockResolvedValueOnce(response([photo(1), photo(3)])));
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore(); photos.selectLoaded();
  await photos.refresh();
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-1", "photo-3"]);
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  expect(photos.cursor).toBeUndefined();
  photos.dispose();
});

it("retains accepted state when recovery times out and ignores a disposed replacement", async () => {
  const timer = new AbortController();
  vi.spyOn(AbortSignal, "timeout").mockReturnValue(timer.signal);
  let finish!: (value: Response) => void;
  vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(response([photo(1)]))
    .mockImplementationOnce((_url, init: RequestInit) => new Promise((_resolve, reject) => init.signal!.addEventListener("abort", () => reject(init.signal!.reason))))
    .mockImplementationOnce(() => new Promise(resolve => finish = resolve)));
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore(); photos.selectLoaded();
  const refresh = photos.refresh();
  timer.abort(new DOMException("Timed out", "TimeoutError"));
  await refresh;
  expect(photos.error).toContain("timed out");
  expect(photos.items).toHaveLength(1);
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  vi.spyOn(AbortSignal, "timeout").mockReturnValue(new AbortController().signal);
  const late = photos.retry();
  photos.dispose(); finish(response([photo(2)]));
  await late;
  expect(photos.items[0].asset_id).toBe("photo-1");
});

it("ignores a replaced request even if the transport completes after abort", async () => {
  let finish!: (value: Response) => void;
  vi.stubGlobal("fetch", vi.fn().mockImplementationOnce(() => new Promise(resolve => finish = resolve)).mockResolvedValueOnce(response([photo(2)])));
  const photos = new Photos("scoped", vi.fn());
  const old = photos.loadMore();
  await photos.refresh();
  finish(response([photo(1)]));
  await old;
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-2"]);
  photos.dispose();
});

it("keeps range selection outside mounted cells and supports single and modifier clicks", () => {
  const photos = new Photos("scoped", vi.fn());
  const ids = Array.from({ length: 1000 }, (_, index) => `photo-${index}`);
  photos.select(ids[0], new MouseEvent("click"), ids);
  photos.select(ids[999], new MouseEvent("click", { shiftKey: true }), ids);
  expect(photos.selection.selectedIDs.size).toBe(1000);
  photos.select(ids[400], new MouseEvent("click", { metaKey: true }), ids);
  expect(photos.selection.selectedIDs.has(ids[400])).toBe(false);
  photos.select(ids[401], new MouseEvent("click", { ctrlKey: true }), ids);
  expect(photos.selection.selectedIDs.has(ids[401])).toBe(false);
  photos.check(ids[400], true, false, ids);
  expect(photos.selection.selectedIDs.has(ids[400])).toBe(true);
  photos.select(ids[5], new MouseEvent("click"), ids);
  expect([...photos.selection.selectedIDs]).toEqual([ids[5]]);
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


it("captures navigation only when the final replacement page succeeds", async () => {
  let finish!: (response: Response) => void;
  vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(response([photo(1), photo(2)]))
    .mockResolvedValueOnce(response([photo(1)], "last"))
    .mockImplementationOnce(() => new Promise(resolve => finish = resolve)));
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore();
  const restore = vi.fn(async () => {});
  const preserve = vi.fn(() => { expect(photos.scrollTop).toBe(9000); return restore; });
  const refresh = photos.refresh(preserve);
  await vi.waitFor(() => expect(finish).toBeTypeOf("function"));
  expect(preserve).not.toHaveBeenCalled();
  photos.scrollTop = 9000;
  finish(response([photo(2)]));
  await refresh;
  expect(preserve).toHaveBeenCalledTimes(1);
  expect(restore).toHaveBeenCalledTimes(1);
  photos.dispose();
});
