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
    .mockResolvedValueOnce(response([photo(2)]));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore();
  await photos.loadMore();
  await photos.retry();
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-2"]);
  expect(JSON.parse(fetcher.mock.calls[2][1].body).cursor).toBeUndefined();
  photos.dispose();
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
