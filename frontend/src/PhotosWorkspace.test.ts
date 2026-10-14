import { afterEach, expect, it, vi } from "vitest";
import { tick } from "svelte";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import PhotosWorkspace from "./PhotosWorkspace.svelte";
import { APIError } from "./api-transport.js";
import { Photos, photoQuery, mapPhotoLimit, type MapView } from "./photos.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";
import { photo, mapPhoto } from "./photo-test-fixtures.js";
import { captureDateQuery } from "./photoTimeline.js";
import type { PhotoBrowseRow, PhotoMapRow } from "./generated/docbank.js";

// Answers background filter-count requests so tests that predate counts see only their own reads.
const stubPhotoFetch = (fetcher: (url: string, init: RequestInit) => unknown) => vi.stubGlobal("fetch", (url: string, init: RequestInit) => url.endsWith("/photos/assets/query") && JSON.parse(String(init?.body ?? "{}")).facets?.length ? Promise.resolve(Response.json({ items: [], total: 0, facets: [] })) : fetcher(url, init));

const map = vi.hoisted(() => ({onvisible:undefined as ((rows:PhotoMapRow[]) => void) | undefined, props:undefined as {view?:MapView; onview:(view:MapView) => void} | undefined}));
vi.mock("./PhotoMap.svelte", () => ({default:(_anchor:unknown, props:{onvisible:(rows:PhotoMapRow[]) => void; view?:MapView; onview:(view:MapView) => void}) => { map.onvisible = props.onvisible; map.props = props; return {}; }}));

const tiles = {tile_url:"",host:"",attribution:"",max_zoom:19};

afterEach(() => { cleanup(); map.onvisible = undefined; map.props = undefined; history.replaceState(null, "", "/"); vi.unstubAllGlobals(); vi.restoreAllMocks(); localStorage.clear(); new Photos("scoped", vi.fn()).setDensity("comfortable"); Reflect.deleteProperty(Element.prototype, "scrollIntoView"); });

it.each([false, true])("preserves subsequent scrolling when an interrupted refresh resumes after remount, map=%s", async mapMode => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
    const cell = this.closest<HTMLElement>(".cell");
    const scroll = this.closest<HTMLElement>(".photo-scroll");
    return cell && scroll ? new DOMRect(0, Number.parseFloat(cell.style.top) + 56 - scroll.scrollTop, Number.parseFloat(cell.style.width), Number.parseFloat(cell.style.height)) : new DOMRect(0, 0, 1000, 400);
  });
  const items = Array.from({ length: 60 }, (_, index) => mapMode ? mapPhoto(index + 1) : photo(index + 1));
  let finishRefresh!: (response: Response) => void;
  stubPhotoFetch(vi.fn().mockImplementationOnce(() => new Promise(() => {}))
    .mockImplementationOnce(() => new Promise<Response>(resolve => finishRefresh = resolve)));
  const photos = new Photos("scoped", vi.fn(), false, photoQuery, mapMode ? "map" : undefined);
  photos.items = items; photos.total = items.length; photos.started = true; photos.scrollTop = 500;
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  const props = { photos, cache, mapMode, tiles: { tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}", host: "tiles.example.org", attribution: "Example", max_zoom: 19 } };
  const view = render(PhotosWorkspace, props);
  if (mapMode) { await vi.dynamicImportSettled(); map.onvisible!(items as PhotoMapRow[]); await tick(); }
  await waitFor(() => expect(screen.getByTestId("photo-scroll").scrollTop).toBe(500));
  await fireEvent.click(screen.getByRole("button", { name: "Refresh previews" }));
  view.unmount();
  render(PhotosWorkspace, props);
  if (mapMode) { await vi.dynamicImportSettled(); map.onvisible!(items as PhotoMapRow[]); await tick(); }
  const scroll = screen.getByTestId("photo-scroll");
  await waitFor(() => expect(scroll.scrollTop).toBe(500));
  scroll.scrollTop = 1000;
  await fireEvent.scroll(scroll);
  const refreshed = items.map(item => ({ ...item, previews: { ...item.previews, grid: { state: "ready", generation_id: "refreshed" } } }));
  finishRefresh(new Response(JSON.stringify({ [mapMode ? "map_items" : "items"]: refreshed, total: items.length })));
  await waitFor(() => expect(photos.loading).toBe(false));
  await tick();
  expect(scroll.scrollTop).toBe(1000);
  expect(photos.scrollTop).toBe(1000);
  photos.dispose(); await cache.dispose();
});

it("disables search and filters until a delayed Hide finishes", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  let finish!: (response: Response) => void;
  vi.stubGlobal("fetch", vi.fn((url: string) => url.endsWith("/hide") ? new Promise<Response>(resolve => finish = resolve) : Promise.resolve(Response.json({ items: [], total: 0, facets: [] }))));
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1)]; photos.started = true;
  photos.baseQuery = photos.query = { ...photos.query, text: "Canon" };
  photos.facets = [{ dimension: "camera", available: true, reason: "", total: 1, missing: 0, other: 0, values: [{ key: "Canon", label: "Canon", count: 1, selected: false }] }];
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache });
  const controls = [screen.getByRole("searchbox", { name: "Search photos" }), screen.getByRole("button", { name: "Search" }), screen.getByRole("combobox", { name: "Sort photos: Capture date" }), screen.getByRole("button", { name: "Clear filters" }), screen.getByRole("button", { name: "Canon, 1 photos" })];
  expect(screen.getByRole("button", { name: "Clear filters" }).getAttribute("type")).toBe("button");
  const pending = photos.setHidden("photo-1");
  await waitFor(() => { for (const control of controls) expect(control.hasAttribute("disabled")).toBe(true); });
  finish(Response.json({ id: "photo-1", revision: 2 }));
  await pending;
  await waitFor(() => { for (const control of controls.slice(0, 4)) expect(control.hasAttribute("disabled")).toBe(false); });
  photos.dispose(); await cache.dispose();
});

it("restores applied search after a facet click and keeps empty-library guidance when clearing filters", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
  const fetcher = vi.fn().mockImplementation(() => Promise.resolve(Response.json({ items: [], total: 0, facets: [] })));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.started = true;
  photos.facets = [{ dimension: "camera", available: true, reason: "", total: 1, missing: 0, other: 0, values: [{ key: "Canon", label: "Canon", count: 1, selected: false }] }];
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache });
  await screen.findByText("Your photo library is empty");
  expect(await screen.findByText(/Import photos with docbank photos import/)).toBeTruthy();
  const search = screen.getByRole("searchbox", { name: "Search photos" }) as HTMLInputElement;
  expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
  await fireEvent.input(search, { target: { value: "Nikon" } });
  expect(search.value).toBe("Nikon");
  expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Canon, 1 photos" }));
  await waitFor(() => expect(search.value).toBe(""));
  expect(JSON.parse(fetcher.mock.calls[0][1].body).query.text).toBe("");
  await waitFor(() => expect(photos.loading).toBe(false));
  await fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
  await waitFor(() => expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull());
  await screen.findByText("Your photo library is empty");
  await fireEvent.click(screen.getByRole("combobox", { name: "Sort photos: Capture date" }));
  expect(screen.queryByRole("option", { name: "Relevance" })).toBeNull();
  await fireEvent.click(screen.getByRole("option", { name: "Capture date" }));
  await photos.setQuery({ ...photos.query, filters: { iso_min: 0 } });
  await screen.findByText("No matching photos");
  await fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
  await screen.findByText("Your photo library is empty");
  expect(screen.getByText(/Import photos with docbank photos import/)).toBeTruthy();
  photos.dispose(); await cache.dispose();
});

it("keeps failed confirmation visible and closes after cancellation or success", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  let listings = 0;
  const fetcher = vi.fn(async (url: string, options: RequestInit) => {
    if (url.endsWith("/query")) {
      const ids = JSON.parse(String(options.body)).query.filters?.asset_ids;
      if (ids) return Response.json({ items: [photo(2)].filter(item => ids.includes(item.asset_id)), total: 1 });
      const first = ++listings === 1;
      return Response.json({ items: [photo(first ? 3 : 2)], total: first ? 3 : 1 });
    }
    if (url.endsWith("/photo-2/trash")) return Response.json({ detail: "Photo changed" }, { status: 412 });
    return Response.json({ id: url.split("/").at(-2), revision: 2 });
  });
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1), photo(2)]; photos.started = true; photos.selectLoaded();
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  const ontrashed = vi.fn();
  render(PhotosWorkspace, { photos, cache, ontrashed, photoID: "", onphotochange: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: "Move to trash" }));
  const dialog = screen.getByRole("dialog", { name: "Move selected photos to trash" });
  expect(within(dialog).getByText(/^Move 2 selected photos and/)).toBeTruthy();
  await fireEvent.click(within(dialog).getByRole("button", { name: "Move to trash" }));
  await within(dialog).findByText(/Photo changed/);
  expect(ontrashed).toHaveBeenCalledWith(photos);
  await fireEvent.click(await within(dialog).findByRole("button", { name: "Keep in Docbank" }));
  expect(screen.queryByRole("dialog", { name: "Move selected photos to trash" })).toBeNull();
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 3.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: "Move to trash" }));
  const single = screen.getByRole("dialog", { name: "Move selected photos to trash" });
  expect(within(single).getByText(/^Move 1 selected photo and/)).toBeTruthy();
  await fireEvent.click(within(single).getByRole("button", { name: "Move to trash" }));
  await waitFor(() => expect(screen.queryByRole("dialog", { name: "Move selected photos to trash" })).toBeNull());
  expect(ontrashed).toHaveBeenCalledTimes(2);
  photos.dispose(); await cache.dispose();
});

it("does not write the user's scroll position back to the grid", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  stubPhotoFetch(vi.fn());
  const photos = new Photos("scoped", vi.fn());
  photos.items = Array.from({ length: 60 }, (_, index) => photo(index + 1)); photos.total = 60; photos.started = true;
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache, photoID: "", onphotochange: vi.fn() });
  await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  const scroll = screen.getByTestId("photo-scroll");
  let top = 0;
  const writes: number[] = [];
  Object.defineProperty(scroll, "scrollTop", { configurable: true, get: () => top, set: (value: number) => { writes.push(value); top = value; } });
  top = 500; await fireEvent.scroll(scroll);
  await waitFor(() => expect(photos.scrollTop).toBe(500));
  expect(writes).toEqual([]);
  photos.dispose(); await cache.dispose();
});

it("starts no extra detail request after a successful reload", async () => {
  vi.stubGlobal("ResizeObserver", class {observe() {} disconnect() {}});
  const photos = new Photos("scoped", vi.fn()); photos.started = true;
  vi.spyOn(photos, "resume").mockResolvedValue();
  const lookup = vi.spyOn(photos, "lookup").mockResolvedValueOnce(photo(1)).mockResolvedValueOnce({...photo(1), name:"Reloaded photo.jpg"}).mockRejectedValue(new Error("Duplicate lookup failed"));
  vi.spyOn(photos, "preview").mockRejectedValueOnce(new APIError("Photo changed", 409, "photo_display_changed")).mockResolvedValue({state:"unsupported"});
  render(PhotosWorkspace, {photos, cache:{get:vi.fn()} as unknown as PhotoPreviewCache, photoID:"photo-1"});
  await screen.findByRole("dialog", {name:"Photo viewer, Photo 1.jpg"});
  await screen.findByText("Photo changed");
  await fireEvent.click(screen.getByRole("button", {name:"Reload photo"}));
  await screen.findByRole("dialog", {name:"Photo viewer, Reloaded photo.jpg"});
  await waitFor(() => expect(screen.queryByRole("button", {name:"Reload photo"})).toBeNull());
  await tick();
  expect(lookup).toHaveBeenCalledTimes(2);
  expect(screen.queryByText("Duplicate lookup failed")).toBeNull();
  photos.dispose();
});

it.each([
  ["2024", "2025-12-15", /^2024/, "2024-01-01", "2025-01-01", "in 2024"],
  ["2024-02", "2024-02-28", /^February 2024/, "2024-02-01", "2024-03-01", "in February 2024"],
  ["2024-02-29", "2024-02-28", /^2024-02-29 ·/, "2024-02-29", "2024-03-01", "on 2024-02-29"],
  ["2025-12", "2025-12-15", /^December 2025/, "2025-12-01", "2026-01-01", "in December 2025"],
  ["9999", "2025-12-15", /^9999/, "9999-01-01", undefined, "in 9999"],
])("stages timeline period %s, retains the grid on failure and restarts paging on Retry", async (date, previous, name, after, before, label) => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  const photos = new Photos("scoped", vi.fn());
  photos.started = true; photos.view = "timeline"; photos.date = previous; photos.query = captureDateQuery(photoQuery, previous);
  photos.items = Array.from({ length: 60 }, (_, index) => photo(index + 1)); photos.total = 100; photos.cursor = "old-page";
  photos.timeline = { dimension: "capture_day", available: true, total: 210, missing: 10, other: 0, values: [
    { key: "9999-12-31", label: "9999-12-31", count: 10, selected: false },
    { key: "2025-12-15", label: "2025-12-15", count: 100, selected: false },
    { key: "2024-02-29", label: "2024-02-29", count: 100, selected: false },
  ] };
  let finish!: (response: Response) => void;
  const replacement = Array.from({ length: 60 }, (_, index) => photo(index + 101, `${after}T12:00:00`));
  const fetcher = vi.fn().mockImplementationOnce(() => new Promise(resolve => finish = resolve))
    .mockResolvedValueOnce(new Response(JSON.stringify({ items: replacement, total: 100, next_cursor: "new-page" })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ items: [photo(200)], total: 100 })));
  stubPhotoFetch(fetcher);
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  const view = render(PhotosWorkspace, { photos, cache, photoID: "", onphotochange: vi.fn() });
  const cell = await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  await fireEvent.click(cell);
  const scroll = screen.getByTestId("photo-scroll");
  scroll.scrollTop = 500; await fireEvent.scroll(scroll);
  const heading = document.querySelector(".library-title span")!.textContent;
  const button = screen.getByRole(date.length === 4 ? "navigation" : "region", { name: date.length === 4 ? "Timeline years" : "Photo timeline" });
  await fireEvent.click([...button.querySelectorAll("button")].find(button => name.test(button.textContent!.trim()))!);
  expect(document.querySelector(".library-title span")!.textContent).toBe(heading);
  expect(photos.date).toBe(previous);
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  expect(scroll.scrollTop).toBe(500);
  const filters = { capture_after: after, ...(before ? { capture_before: before } : {}) };
  expect(JSON.parse(fetcher.mock.calls[0][1].body)).toMatchObject({ query: { filters } });
  expect(JSON.parse(fetcher.mock.calls[0][1].body).cursor).toBeUndefined();
  finish(new Response(JSON.stringify({ detail: "Date unavailable" }), { status: 503 }));
  await screen.findByText("Date unavailable");
  expect(document.querySelector(".library-title span")!.textContent).toBe(heading);
  expect(photos.items[0].asset_id).toBe("photo-1");
  expect(photos.cursor).toBe("old-page");
  expect(photos.date).toBe(previous);
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  expect(scroll.scrollTop).toBe(500);
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await screen.findByText(`100 photos ${label} · 60 loaded`);
  expect(photos.date).toBe(date);
  expect(photos.selection.selectedIDs.size).toBe(0);
  expect(scroll.scrollTop).toBe(0);
  expect(screen.getByText("210 photos in scope · 10 undated")).toBeTruthy();
  expect(fetcher).toHaveBeenCalledTimes(2);
  expect(JSON.parse(fetcher.mock.calls[1][1].body).cursor).toBeUndefined();
  await fireEvent.click(await screen.findByRole("button", { name: "Load more" }));
  await screen.findByText(`100 photos ${label} · 61 loaded`);
  expect(JSON.parse(fetcher.mock.calls[2][1].body)).toMatchObject({ cursor: "new-page", query: { filters } });
  view.unmount(); photos.dispose(); await cache.dispose();
});

it("keeps loaded photos visible on paging failure and selects with touch checkboxes", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const fetcher = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ items: [photo(1), photo(2)], total: 3, next_cursor: "next" })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Page unavailable" }), { status: 503 }))
    .mockResolvedValueOnce(new Response(JSON.stringify({ items: [photo(3, "2024-01-01T12:00:00")], total: 3 })));
  vi.stubGlobal("fetch", (url: string, init: RequestInit) => JSON.parse(init.body as string).page_size === 1 ? Promise.resolve(new Response(JSON.stringify({ facets: [] }))) : fetcher(url, init));
  const photos = new Photos("scoped", vi.fn());
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  const view = render(PhotosWorkspace, { photos, cache, photoID: "", onphotochange: vi.fn() });
  await screen.findByText("Page unavailable");
  expect(screen.queryByRole("navigation", { name: "Photo years" })).toBeNull();
  expect(screen.getByRole("button", { name: "Select Photo 1.jpg" })).toBeTruthy();
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  expect(await screen.findByText("1 selected photo")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Select loaded photos" }));
  expect(screen.getByText("2 selected photos")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await screen.findByRole("button", { name: "Select Photo 3.jpg" });
  expect(screen.getByRole("navigation", { name: "Photo years" }).querySelectorAll("button")).toHaveLength(2);
  await waitFor(() => expect(photos.loading).toBe(false));
  fetcher.mockImplementationOnce(() => new Promise(() => {}));
  photos.cursor = "manual-page";
  await fireEvent.click(await screen.findByRole("button", { name: "Load more" }));
  expect(await screen.findByText("Loading photos…")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
  view.unmount();
  expect(fetcher.mock.calls[3][1].signal.aborted).toBe(true);
  photos.dispose();
  await cache.dispose();
});

it("retains visible cells and pending previews across a prepend larger than the window", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
    const cell = this.closest<HTMLElement>(".cell");
    const scroll = this.closest<HTMLElement>(".photo-scroll");
    return cell && scroll ? new DOMRect(0, Number.parseFloat(cell.style.top) + 56 - scroll.scrollTop, Number.parseFloat(cell.style.width), Number.parseFloat(cell.style.height)) : new DOMRect(0, 0, 1000, 400);
  });
  vi.stubGlobal("URL", class extends URL { static createObjectURL() { return "blob:synthetic"; } static revokeObjectURL() {} });
  const photos = new Photos("scoped", vi.fn());
  const initial = Array.from({ length: 6 }, (_, index) => photo(index + 1, `2025-06-01T${12 + index}:00:00`));
  for (const item of initial) item.previews.grid = { state: "ready", generation_id: "generation" };
  photos.items = initial; photos.started = true; photos.cursor = "older"; photos.grouping = "sessions";
  let finishPreview!: (blob: Blob) => void;
  const get = vi.fn((id: string, _generation: string, _signal: AbortSignal) => id === "photo-2" ? new Promise<Blob>(resolve => finishPreview = resolve) : Promise.resolve(new Blob(["synthetic"])));
  let finishPage!: (response: Response) => void;
  stubPhotoFetch(vi.fn(() => new Promise<Response>(resolve => finishPage = resolve)));
  render(PhotosWorkspace, { photos, cache: { get } as unknown as PhotoPreviewCache, photoID: "", onphotochange: vi.fn() });
  const image = await screen.findByRole("img", { name: "Photo 1.jpg" });
  await waitFor(() => expect(get).toHaveBeenCalledTimes(6));
  const pendingSignal = get.mock.calls.find(([id]) => id === "photo-2")![2];
  finishPage(new Response(JSON.stringify({ items: Array.from({ length: 60 }, (_, index) => photo(index + 7, "2025-06-01T11:00:00")), total: 66 })));
  await waitFor(() => expect(photos.loading).toBe(false));
  expect(image.isConnected).toBe(true);
  expect(screen.getByRole("img", { name: "Photo 1.jpg" })).toBe(image);
  expect(pendingSignal.aborted).toBe(false);
  expect(get.mock.calls.filter(([id]) => initial.some(item => item.asset_id === id))).toHaveLength(6);
  finishPreview(new Blob(["completed"]));
  await screen.findByRole("img", { name: "Photo 2.jpg" });
  photos.dispose();
});

it("shows both years of a New Year session and jumps to the newest containing group", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1, "2026-02-01T12:00:00"), photo(2, "2025-01-01T01:00:00"), photo(3, "2024-12-31T23:00:00"), photo(4, "0000-01-02")];
  photos.started = true; photos.grouping = "sessions";
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache, photoID: "", onphotochange: vi.fn() });
  await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  const rail = screen.getByRole("navigation", { name: "Photo years" });
  expect([...rail.querySelectorAll("button")].map(button => button.textContent?.trim())).toEqual(["2026", "2025", "2024"]);
  const scroll = screen.getByTestId("photo-scroll");
  const scrollTo = vi.fn();
  scroll.scrollTo = scrollTo;
  const newerGroup = scroll.querySelector<HTMLElement>('[data-month="photo-1"]')!;
  const session = scroll.querySelector<HTMLElement>('[data-month="photo-2"]')!;
  expect(session.getAttribute("aria-label")).toBe("2024-12-31 · Capture session");
  expect([...session.querySelectorAll("[data-asset]")].map(cell => cell.getAttribute("data-asset"))).toEqual(["photo-2", "photo-3"]);
  await fireEvent.click(screen.getByRole("button", { name: "2024" }));
  expect(scrollTo).toHaveBeenLastCalledWith({ top: Math.ceil(Number.parseFloat(newerGroup.style.height)) });
  await fireEvent.click(screen.getByRole("button", { name: "2025" }));
  expect(scrollTo).toHaveBeenLastCalledWith({ top: Math.ceil(Number.parseFloat(newerGroup.style.height)) });
  await fireEvent.click(screen.getByRole("button", { name: "2026" }));
  expect(scrollTo).toHaveBeenLastCalledWith({ top: 0 });
  photos.items = [...photos.items, photo(5, "2025-02-01T12:00:00")];
  await screen.findByRole("button", { name: "Select Photo 5.jpg" });
  await fireEvent.click(screen.getByRole("button", { name: "2025" }));
  expect(scrollTo).toHaveBeenLastCalledWith({ top: Math.ceil(Number.parseFloat(newerGroup.style.height)) });
  photos.dispose();
  await cache.dispose();
});

it("clears selection with Escape from focused photo controls while honoring shortcut suppression", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1)]; photos.started = true;
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache, photoID: "", onphotochange: vi.fn() });
  const cell = await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  await fireEvent.click(cell);
  cell.focus();
  expect(document.activeElement).toBe(cell);
  for (const name of [/^Group photos/, /^Grid density/]) {
    const dropdown = screen.getByRole("combobox", { name });
    await fireEvent.click(dropdown);
    await screen.findByRole("listbox");
    await fireEvent.keyDown(dropdown, { key: "Escape" });
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(screen.getByText("1 selected photo")).toBeTruthy();
  }
  cell.focus();
  await fireEvent.keyDown(document.activeElement!, { key: "Escape" });
  expect(screen.queryByText("1 selected photo")).toBeNull();
  const checkbox = screen.getByRole("checkbox", { name: "Select photo Photo 1.jpg" });
  await fireEvent.click(checkbox);
  checkbox.focus();
  expect(screen.getByText("1 selected photo")).toBeTruthy();
  await fireEvent.keyDown(document.activeElement!, { key: "Escape" });
  expect(screen.queryByText("1 selected photo")).toBeNull();
  photos.dispose();
  await cache.dispose();
});

it("keeps the retained scroll position when density changed in another view", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  const photos = new Photos("scoped", vi.fn());
  photos.items = Array.from({ length: 60 }, (_, index) => photo(index + 1)); photos.total = 60; photos.started = true; photos.scrollTop = 1000;
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  new Photos("scoped", vi.fn()).setDensity("compact");
  render(PhotosWorkspace, { photos, cache, photoID: "", onphotochange: vi.fn() });
  await waitFor(() => expect(screen.getByTestId("photo-scroll").scrollTop).toBe(1000));
  photos.dispose();
  await cache.dispose();
});

it.each(["density", "album trash"])("keeps the top photo after %s and pages that add no rows", async kind => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
    const cell = this.closest<HTMLElement>(".cell");
    const scroll = this.closest<HTMLElement>(".photo-scroll");
    return cell && scroll ? new DOMRect(0, Number.parseFloat(cell.style.top) + 56 - scroll.scrollTop, Number.parseFloat(cell.style.width), Number.parseFloat(cell.style.height)) : new DOMRect(0, 0, 1000, 400);
  });
  const photos = new Photos("scoped", vi.fn());
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  const items = Array.from({ length: 60 }, (_, index) => photo(index + 1));
  const page = (rows: typeof items, cursor?: string) => new Response(JSON.stringify({ items: rows, total: 60, next_cursor: cursor }));
  const fetcher = vi.fn();
  if (kind === "album trash") {
    photos.query = { ...photos.query, filters: { set_ids: ["album"] }, sort: { field: "added_time", direction: "desc" } };
    photos.items = items; photos.total = 60; photos.started = true;
    fetcher.mockResolvedValueOnce(new Response(JSON.stringify({ id: "photo-1", revision: 2 }))).mockResolvedValueOnce(new Response(JSON.stringify({ items: items.slice(1), total: 59 })));
  } else {
    fetcher.mockResolvedValueOnce(page(items.slice(0, 2), "repeat")).mockResolvedValueOnce(page(items.slice(0, 2), "rest")).mockResolvedValueOnce(page(items.slice(2)));
  }
  stubPhotoFetch(fetcher);
  render(PhotosWorkspace, { photos, cache, albumID: kind === "album trash" ? "album" : undefined, photoID: "", onphotochange: vi.fn() });
  await screen.findByText("60 photos · 60 loaded");
  if (kind === "density") {
    expect(fetcher).toHaveBeenCalledTimes(3);
    expect(fetcher.mock.calls.map(call => JSON.parse(call[1].body).cursor)).toEqual([undefined, "repeat", "rest"]);
  }
  await waitFor(() => expect(photos.loading).toBe(false));
  const scroll = screen.getByTestId("photo-scroll");
  scroll.scrollTop = 1000;
  await fireEvent.scroll(scroll);
  const anchor = [...scroll.querySelectorAll<HTMLElement>("[data-asset]")].find(cell => cell.getBoundingClientRect().bottom > 56)!;
  const offset = anchor.getBoundingClientRect().top;
  if (kind === "album trash") {
    photos.select("photo-1", new MouseEvent("click"), photos.items.map(item => item.asset_id));
    await fireEvent.click(await screen.findByRole("button", { name: "Move to trash" }));
    await fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Move to trash" }));
    await waitFor(() => expect(photos.trashing).toBe(false));
  } else {
    Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
    await fireEvent.click(screen.getByRole("combobox", { name: "Grid density: Comfortable" }));
    await fireEvent.click(screen.getByRole("option", { name: "Compact" }));
    await waitFor(() => expect(scroll.scrollTop).not.toBe(1000));
  }
  const moved = scroll.querySelector<HTMLElement>(`[data-asset="${anchor.dataset.asset}"]`)!;
  expect(moved.getBoundingClientRect().top).toBeCloseTo(offset);
  photos.dispose();
  await cache.dispose();
});

it("opens on touch with empty selection and keeps selection taps after checking a photo", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.stubGlobal("matchMedia", vi.fn(() => ({ matches: true })));
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const photos = new Photos("scoped", vi.fn()); photos.items = [photo(1), photo(2)]; photos.started = true;
  const open = vi.fn();
  render(PhotosWorkspace, { photos, cache: { get: vi.fn() } as unknown as PhotoPreviewCache, photoID: "", onphotochange: open });
  const cell = await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  await fireEvent.click(cell); expect(open).not.toHaveBeenCalled(); expect(photos.selection.selectedIDs.has("photo-1")).toBe(true);
  photos.clearSelection();
  const touchEvent = new MouseEvent("click", { bubbles: true, button: 0 }); Object.defineProperty(touchEvent, "pointerType", { value: "touch" });
  await fireEvent(cell, touchEvent); expect(open).toHaveBeenCalledWith("photo-1", "open"); expect(photos.selection.selectedIDs.size).toBe(0);
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  vi.stubGlobal("matchMedia", vi.fn(() => ({ matches: false })));
  const second = screen.getByRole("button", { name: "Select Photo 2.jpg" });
  const tap = async () => { const event = new MouseEvent("click", { bubbles: true, button: 0 }); Object.defineProperty(event, "pointerType", { value: "touch" }); await fireEvent(second, event); };
  await tap(); expect([...photos.selection.selectedIDs]).toEqual(["photo-1", "photo-2"]);
  await tap(); expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  await fireEvent.doubleClick(second); expect(open).toHaveBeenCalledOnce();
  await fireEvent.keyDown(cell, { key: "Enter" }); expect(open).toHaveBeenCalledTimes(2);
});

it.each(["success", "grid", "removed", "pending"])("keeps query order and viewer membership after %s continuation", async scenario => {
  const expired = scenario === "removed";
  const removed = expired || scenario === "pending";
  const initial = [photo(1, "2024-04-30T23:30:00-02:00"), photo(2, "2024-05-01T00:00:00Z")], following = photo(3, "2024-05-01T00:30:00+02:00");
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const fetcher = vi.fn(async (url: string, init: RequestInit) => {
    if (url.endsWith("/preview")) return new Response(JSON.stringify({state:"unsupported"}));
    const body = JSON.parse(String(init.body));
    if (!failed) { failed = true; return new Response(JSON.stringify({detail:"Continuation failed",code:expired ? "cursor_expired" : "unavailable"}),{status:expired ? 422 : 503}); }
    if (expired && !body.cursor) return new Response(JSON.stringify({items:scenario === "removed" ? [initial[0]] : initial,total:3,next_cursor:"replacement"}));
    return new Response(JSON.stringify({items:[following],total:3}));
  });
  let failed = scenario === "success" || scenario === "grid"; stubPhotoFetch(fetcher);
  const photos = new Photos("scoped",vi.fn()); photos.items = initial; photos.total=3; photos.cursor="old"; photos.started=true; photos.loading=true;
  if (scenario === "grid") photos.error = "Continuation failed";
  let finishLookup!: (row:ReturnType<typeof photo>) => void;
  if (removed) vi.spyOn(photos, "lookup").mockImplementation(() => new Promise(resolve => finishLookup = resolve));
  let finishPage!: () => void;
  const loadMore = scenario === "pending" ? vi.spyOn(photos, "loadMore").mockImplementation(() => new Promise<void>(resolve => finishPage = resolve)) : undefined;
  const cache={get:vi.fn()} as unknown as PhotoPreviewCache;
  let view: ReturnType<typeof render>;
  const onphotochange=vi.fn((id: string) => { history.replaceState(null, "", `/photos#photo=${id}`); void view.rerender({photos,cache,photoID:id,onphotochange}); });
  view=render(PhotosWorkspace,{photos,cache,photoID:"",onphotochange});
  await fireEvent.keyDown(await screen.findByRole("button",{name:scenario === "grid" ? "Select Photo 1.jpg" : "Select Photo 2.jpg"}),{key:"Enter"});
  await screen.findByRole("navigation",{name:"Photos in current result"}); photos.loading=false;
  if (scenario !== "grid") await fireEvent.keyDown(screen.getByRole("dialog"),{key:"ArrowRight"});
  const returnRecord = photos.viewerReturn, currentURL = location.href;
  if (scenario === "pending") {
    await waitFor(() => expect(loadMore).toHaveBeenCalledOnce());
  } else if (scenario !== "success") {
  await screen.findByRole("button",{name:"Retry navigation"}); expect(within(screen.getByRole("dialog")).getByText("Continuation failed")).toBeTruthy();
  expect(screen.getAllByRole("button",{name:"Next photo"}).every(button => (button as HTMLButtonElement).disabled)).toBe(scenario !== "grid");
  await fireEvent.click(screen.getByRole("button",{name:"Retry navigation"}));
  await waitFor(() => expect(photos.error).toBe("")); expect(onphotochange).toHaveBeenCalledTimes(1);
  }
  if (removed) {
    if (scenario === "pending") photos.items = [initial[0]!, following];
    await screen.findByText("This photo is no longer in the refreshed result."); await waitFor(() => expect(finishLookup).toBeTypeOf("function"));
    finishLookup(initial[1]!); await screen.findByRole("dialog", {name:"Photo viewer, Photo 2.jpg"});
    expect(screen.getByText("This photo is no longer in the refreshed result.")).toBeTruthy(); expect(location.href).toBe(currentURL); expect(photos.viewerReturn).toBe(returnRecord);
    expect(screen.getAllByRole("button", {name:/^(Previous|Next) photo$/}).every(button => (button as HTMLButtonElement).disabled)).toBe(true);
    if (scenario === "pending") {
      finishPage(); await new Promise(resolve => setTimeout(resolve, 0));
      expect(onphotochange).toHaveBeenCalledTimes(1); expect(location.href).toBe(currentURL); expect(photos.viewerReturn).toBe(returnRecord);
      expect(screen.getByRole("dialog", {name:"Photo viewer, Photo 2.jpg"})).toBeTruthy();
      expect(screen.getAllByRole("button", {name:/^(Previous|Next) photo$/}).every(button => (button as HTMLButtonElement).disabled)).toBe(true); return;
    }
    await view.rerender({photos,cache,photoID:"photo-1",onphotochange}); await waitFor(() => expect(screen.queryByText("This photo is no longer in the refreshed result.")).toBeNull());
    await view.rerender({photos,cache,photoID:"photo-2",onphotochange}); await screen.findByText("This photo is no longer in the refreshed result.");
    photos.items = [...initial, following]; await waitFor(() => expect(screen.queryByText("This photo is no longer in the refreshed result.")).toBeNull());
  }
  if (scenario !== "success") {
  await waitFor(() => expect(screen.getAllByRole("button",{name:"Next photo"}).every(button => !(button as HTMLButtonElement).disabled)).toBe(true));
  await fireEvent.keyDown(screen.getByRole("dialog"),{key:"ArrowRight"});
  if (scenario === "grid") { await waitFor(() => expect(onphotochange).toHaveBeenLastCalledWith("photo-2", "step")); await fireEvent.keyDown(screen.getByRole("dialog"),{key:"ArrowRight"}); }
  }
  await waitFor(() => expect(onphotochange).toHaveBeenCalledWith("photo-3","step"));
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-1","photo-2","photo-3"]); expect(photos.error).toBe("");
  await fireEvent.keyDown(screen.getByRole("dialog"), {key:"ArrowLeft"}); await waitFor(() => expect(onphotochange).toHaveBeenLastCalledWith("photo-2", "step"));
  await fireEvent.keyDown(screen.getByRole("dialog"), {key:"ArrowLeft"}); await waitFor(() => expect(onphotochange).toHaveBeenLastCalledWith("photo-1", "step"));
});


it.each(["close", "photo", "unmount", "live"])("keeps deferred continuation scoped to its %s photo visit", async interruption => {
  vi.stubGlobal("ResizeObserver", class {observe() {} disconnect() {}});
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const photos = new Photos("scoped", vi.fn()); photos.items = [photo(1), photo(2)]; photos.total = 3; photos.cursor = "next"; photos.started = true; photos.loading = true;
  vi.spyOn(photos, "preview").mockResolvedValue({state:"unsupported"});
  let finish!: () => void;
  const loadMore = vi.spyOn(photos, "loadMore").mockImplementation(() => { photos.loading = true; return new Promise<void>(resolve => finish = resolve); });
  const cache = {get:vi.fn()} as unknown as PhotoPreviewCache;
  let view: ReturnType<typeof render>;
  const onphotochange = vi.fn((id: string) => { void view.rerender({photos, cache, photoID:id, onphotochange}); });
  view = render(PhotosWorkspace, {photos, cache, photoID:"", onphotochange});
  await fireEvent.keyDown(await screen.findByRole("button", {name:"Select Photo 2.jpg"}), {key:"Enter"});
  await screen.findByRole("dialog", {name:"Photo viewer, Photo 2.jpg"}); photos.loading = false;
  await fireEvent.keyDown(screen.getByRole("dialog"), {key:"ArrowRight"});
  expect(loadMore).toHaveBeenCalledOnce();
  if (interruption === "close") {
    await fireEvent.keyDown(screen.getByRole("dialog"), {key:"Escape"});
    await waitFor(() => expect(photos.viewerReturn).toBeUndefined());
    await fireEvent.keyDown(screen.getByRole("button", {name:"Select Photo 2.jpg"}), {key:"Enter"});
  } else if (interruption === "photo") {
    await fireEvent.keyDown(screen.getByRole("dialog"), {key:"ArrowLeft"});
    await screen.findByRole("dialog", {name:"Photo viewer, Photo 1.jpg"});
    await fireEvent.keyDown(screen.getByRole("dialog"), {key:"ArrowRight"});
  } else if (interruption === "unmount") {
    view.unmount();
    view = render(PhotosWorkspace, {photos, cache, photoID:"photo-2", onphotochange});
  }
  await screen.findByRole("dialog", {name:"Photo viewer, Photo 2.jpg"});
  const changes = onphotochange.mock.calls.length;
  photos.items = [...photos.items, photo(3)]; photos.loading = false; finish();
  await new Promise(resolve => setTimeout(resolve, 0));
  expect(onphotochange).toHaveBeenCalledTimes(changes + (interruption === "live" ? 1 : 0));
  expect(screen.getByRole("dialog", {name:`Photo viewer, Photo ${interruption === "live" ? 3 : 2}.jpg`})).toBeTruthy();
});

it.each(["listing", "late", "navigation", "retry", "fresh"])("keeps standalone lookup authoritative across %s", async scenario => {
  vi.stubGlobal("ResizeObserver", class {observe() {} disconnect() {}});
  vi.stubGlobal("Image", class {src = ""; async decode() {}});
  vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:synthetic"); vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  const old = photo(1); old.previews.grid = {state:"ready", generation_id:"old-grid"};
  const fresh = {...photo(1), content_version_id:"fresh-version", previews:{...old.previews, grid:{state:"ready" as const, generation_id:"fresh-grid"}}};
  const photos = new Photos("scoped", vi.fn()); photos.started = true; photos.loading = true;
  if (["navigation", "retry", "fresh"].includes(scenario)) photos.items = [old];
  photos.cursor = "next"; photos.total = 999; photos.check("photo-1", true, false, ["photo-1"]);
  let finish!: (row:ReturnType<typeof photo>) => void;
  let reject!: (cause: Error) => void;
  const lookup = vi.spyOn(photos, "lookup").mockImplementation(() => new Promise((_, fail) => reject = fail));
  if (scenario === "fresh") lookup.mockImplementation(() => new Promise(resolve => finish = resolve));
  const preview = vi.spyOn(photos, "preview").mockResolvedValue(scenario === "fresh" ? {state:"ready", generation_id:"fresh-fit"} : {state:"unsupported"});
  const get = vi.fn(async () => new Blob(["cached-preview"]));
  const props = {photos, cache:{get} as unknown as PhotoPreviewCache, photoID:scenario === "navigation" ? "missing" : scenario === "retry" ? "photo-999" : "photo-1", onphotochange:vi.fn()};
  const view = render(PhotosWorkspace, props);
  if (scenario === "fresh") {
    await waitFor(() => expect(lookup).toHaveBeenCalledOnce());
    expect(view.container.querySelector(".photo-viewport img")).toBeNull(); expect(preview).not.toHaveBeenCalled();
    photos.items = [old, photo(2)]; await fireEvent.keyDown(screen.getByRole("dialog"), {key:"i"});
    expect(lookup).toHaveBeenCalledOnce(); expect(lookup.mock.calls[0]![1].aborted).toBe(false);
    finish(fresh); await screen.findByText("Fit preview");
    expect(preview).toHaveBeenCalledWith(expect.objectContaining({content_version_id:"fresh-version"}), "fit", expect.any(AbortSignal));
    expect(get).toHaveBeenCalledWith(old.asset_id, "fresh-grid", expect.any(AbortSignal), false);
    expect((photos.items[0] as PhotoBrowseRow).content_version_id).toBe("fresh-version");
    photos.items = [old]; await fireEvent.keyDown(screen.getByRole("dialog"), {key:"i"});
    expect(lookup).toHaveBeenCalledOnce(); expect(preview).toHaveBeenCalledTimes(1); return;
  }
  await waitFor(() => expect(reject).toBeTypeOf("function"));
  if (scenario !== "late") { reject(new Error("Lookup failed")); await screen.findByText("Lookup failed"); }
  if (scenario === "retry") {
    expect(screen.getByText("No preview")).toBeTruthy();
    lookup.mockRejectedValueOnce(new Error("Reload failed")).mockImplementationOnce(() => new Promise(resolve => finish = resolve));
    await fireEvent.click(screen.getByRole("button", {name:"Reload photo"}));
    await screen.findByText("Reload failed"); await fireEvent.click(screen.getByRole("button", {name:"Reload photo"}));
    await waitFor(() => expect((screen.getByRole("button", {name:"Reload photo"}) as HTMLButtonElement).disabled).toBe(true));
    expect(document.activeElement).toBe(screen.getByRole("dialog")); finish(photo(999));
    await screen.findByRole("dialog", {name:"Photo viewer, Photo 999.jpg"});
    expect(lookup).toHaveBeenCalledTimes(3); expect(photos.items.map(item => item.asset_id)).toEqual(["photo-1"]);
    expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]); expect(photos.cursor).toBe("next"); return;
  }
  lookup.mockResolvedValueOnce(photo(1));
  if (scenario === "navigation") {
    await view.rerender({...props, photoID:"photo-1"});
    await waitFor(() => expect(lookup.mock.calls[0]![1].aborted).toBe(true));
  } else {
    photos.items = [photo(1)]; await fireEvent.keyDown(screen.getByRole("dialog"), {key:"i"});
    expect(lookup.mock.calls[0]![1].aborted).toBe(false); expect(lookup).toHaveBeenCalledOnce();
    if (scenario === "late") reject(new Error("Lookup failed"));
    await screen.findByText("Lookup failed");
    expect(screen.queryByRole("dialog", {name:"Photo viewer, Photo 1.jpg"})).toBeNull();
    await fireEvent.click(screen.getByRole("button", {name:"Reload photo"}));
  }
  await screen.findByRole("dialog", {name:"Photo viewer, Photo 1.jpg"});
  await waitFor(() => expect(screen.queryByText("Lookup failed")).toBeNull());
});

it.each([
  ["preview", "changed"], ["original", "changed"], ["preview", "same"], ["original", "same"],
] as const)("reloads a %s failure with a %s version and keeps the viewer context", async (operation, version) => {
  vi.stubGlobal("ResizeObserver", class {observe() {} disconnect() {}});
  vi.stubGlobal("Image", class {src = ""; async decode() {}});
  vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:synthetic"); vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  const old = photo(1), fresh = {...photo(1), content_version_id:version === "changed" ? "fresh-version" : old.content_version_id};
  const photos = new Photos("scoped", vi.fn()); photos.items = [old, photo(2)]; photos.started = true; photos.cursor = "next";
  photos.check("photo-2", true, false, ["photo-1", "photo-2"]);
  const record = {contextID:photos.contextID, assetID:old.asset_id, control:"open" as const, scrollTop:400}; photos.viewerReturn = record;
  let finish!: (response:Response) => void;
  const fetcher = vi.fn().mockImplementation(() => new Promise<Response>(resolve => finish = resolve));
  if (version === "same") fetcher.mockRejectedValueOnce(new Error("Reload failed"));
  stubPhotoFetch(fetcher);
  const changed = new APIError("Photo changed", 409, "photo_display_changed");
  const preview = vi.spyOn(photos, "preview").mockResolvedValue({state:"ready", generation_id:"fresh-fit"});
  if (operation === "preview") preview.mockRejectedValueOnce(changed);
  else vi.spyOn(photos, "original").mockRejectedValueOnce(changed);
  render(PhotosWorkspace, {photos, cache:{get:vi.fn(async () => new Blob(["synthetic"]))} as unknown as PhotoPreviewCache, photoID:old.asset_id, onphotochange:vi.fn()});
  if (operation === "original") { await screen.findByText("Fit preview"); await fireEvent.click(screen.getByRole("button", {name:"View original"})); }
  await screen.findByText("Photo changed"); expect(screen.queryByRole("button", {name:"Retry preview"})).toBeNull();
  const preparations = preview.mock.calls.length;
  await fireEvent.click(screen.getByRole("button", {name:"Reload photo"}));
  if (version === "same") {
    await screen.findByText("Reload failed"); expect(preview).toHaveBeenCalledTimes(preparations);
    await fireEvent.click(screen.getByRole("button", {name:"Reload photo"}));
  }
  await waitFor(() => expect(finish).toBeTypeOf("function"));
  expect(preview).toHaveBeenCalledTimes(preparations);
  finish(new Response(JSON.stringify({items:[fresh], total:2})));
  await waitFor(() => expect((photos.items[0] as PhotoBrowseRow).content_version_id).toBe(fresh.content_version_id)); await screen.findByText("Fit preview");
  await waitFor(() => expect(preview).toHaveBeenCalledTimes(preparations + 1));
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-1", "photo-2"]); expect(photos.cursor).toBe("next");
  expect([...photos.selection.selectedIDs]).toEqual(["photo-2"]); expect(photos.viewerReturn).toBe(record);
  expect(screen.queryByText("Photo changed")).toBeNull(); expect(document.activeElement).toBe(screen.getByRole("dialog"));
});

it.each(["success", "failure", "interrupted", "late"] as const)("consumes a %s reload before revisiting the listed photo", async outcome => {
  vi.stubGlobal("ResizeObserver", class {observe() {} disconnect() {}});
  const photos = new Photos("scoped", vi.fn()); photos.items = [photo(1), photo(2)]; photos.started = true;
  photos.viewerReturn = {contextID:photos.contextID, assetID:"photo-1", control:"open", scrollTop:0};
  let finish!: (row:ReturnType<typeof photo>) => void;
  const lookup = vi.spyOn(photos, "lookup");
  if (outcome === "success") lookup.mockResolvedValue(photo(1));
  else if (outcome === "failure") lookup.mockRejectedValue(new Error("Reload failed"));
  else if (outcome === "late") lookup.mockImplementation(() => new Promise(resolve => finish = resolve));
  else lookup.mockImplementation(() => new Promise(() => {}));
  vi.spyOn(photos, "preview").mockRejectedValueOnce(new APIError("Changed", 409, "photo_display_changed")).mockResolvedValue({state:"unsupported"});
  const props = {photos, cache:{get:vi.fn()} as unknown as PhotoPreviewCache, photoID:"photo-1", onphotochange:vi.fn()};
  const view = render(PhotosWorkspace, props);
  await fireEvent.click(await screen.findByRole("button", {name:"Reload photo"}));
  await waitFor(() => expect(lookup).toHaveBeenCalledOnce());
  if (outcome === "failure") await screen.findByText("Reload failed");
  if (outcome === "success") await waitFor(() => expect(screen.queryByText("Changed")).toBeNull());
  await view.rerender({...props, photoID:"photo-2"});
  await waitFor(() => expect(lookup.mock.calls[0]![1].aborted).toBe(true));
  if (outcome === "late") {
    const version = (photos.items[0] as PhotoBrowseRow).content_version_id;
    finish({...photo(1), content_version_id:"late-version"}); await new Promise(resolve => setTimeout(resolve, 0));
    expect((photos.items[0] as PhotoBrowseRow).content_version_id).toBe(version); expect(screen.getByRole("dialog", {name:"Photo viewer, Photo 2.jpg"})).toBeTruthy();
  }
  await view.rerender(props);
  await screen.findByRole("dialog", {name:"Photo viewer, Photo 1.jpg"});
  expect(lookup).toHaveBeenCalledOnce();
});

it("switches Grid and Timeline, seeks an empty day and clears its date", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const photos = new Photos("scoped", vi.fn());
  photos.started = true; photos.items = [photo(1)]; photos.total = 1;
  photos.timeline = { dimension: "capture_day", available: true, total: 1, missing: 0, other: 0, values: [{ key: "2024-02-29", label: "2024-02-29", count: 1, selected: false }] };
  let finish!: (response: Response) => void;
  const fetcher = vi.fn().mockImplementationOnce(() => new Promise(resolve => finish = resolve))
    .mockResolvedValueOnce(new Response(JSON.stringify({ items: [], total: 0 })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ items: [], total: 0 })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ items: [], total: 0 })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ items: [photo(1)], total: 1 })));
  stubPhotoFetch(fetcher);
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  const view = render(PhotosWorkspace, { photos, cache, photoID: "", onphotochange: vi.fn() });
  expect(screen.getByRole("button", { name: "Grid", pressed: true })).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Timeline" }));
  expect(screen.getByRole("button", { name: "Timeline", pressed: true })).toBeTruthy();
  await fireEvent.click(screen.getByRole("navigation", { name: "Timeline years" }).querySelector("button")!);
  expect(screen.getByText("1 photos · 1 loaded")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Select Photo 1.jpg" })).toBeTruthy();
  expect(screen.queryByText("0 photos in 2024 · 0 loaded")).toBeNull();
  finish(new Response(JSON.stringify({ detail: "Try again" }), { status: 503 }));
  await screen.findByText("Try again");
  expect(screen.getByText("1 photos · 1 loaded")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Select Photo 1.jpg" })).toBeTruthy();
  expect(screen.queryByText("0 photos in 2024 · 0 loaded")).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: /^Retry$/ }));
  expect(await screen.findByText("No photos in this year")).toBeTruthy();
  expect(screen.getByText("0 photos in 2024 · 0 loaded")).toBeTruthy();
  await photos.selectDate("2024-02");
  expect(await screen.findByText("No photos in this month")).toBeTruthy();
  expect(screen.getByText("0 photos in February 2024 · 0 loaded")).toBeTruthy();
  await photos.selectDate("2024-02-29");
  expect(await screen.findByText("No photos on this day")).toBeTruthy();
  expect(screen.getByText("0 photos on 2024-02-29 · 0 loaded")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Clear date" }));
  await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  expect(screen.queryByRole("button", { name: "Clear date" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Grid" }));
  expect(screen.queryByRole("region", { name: "Photo timeline" })).toBeNull();
  view.unmount(); photos.dispose(); await cache.dispose();
});

it("explains the relevance limit and hides it for complete results or capture dates", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  const photos = new Photos("scoped", vi.fn());
  photos.started = true;
  photos.items = [photo(1)];
  photos.total = 5000;
  photos.query = { ...photos.query, text: "Canon", sort: { field: "relevance", direction: "desc" } };
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache });
  const note = "Showing the best 250 of 5,000 matches. Refine the search or sort by capture date to see all.";
  await screen.findByText(note);
  expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
  photos.total = 1;
  await waitFor(() => expect(screen.queryByText(note)).toBeNull());
  photos.total = 5000;
  photos.query = { ...photos.query, sort: { field: "capture_time", direction: "desc" } };
  await waitFor(() => expect(screen.queryByText(note)).toBeNull());
  photos.dispose(); await cache.dispose();
});

it("keeps a Library reload error through a list refresh", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  const photos = new Photos("scoped", vi.fn()); photos.items = [photo(1)]; photos.started = true;
  photos.viewerReturn = { contextID: photos.contextID, assetID: "photo-1", control: "open", scrollTop: 0 };
  const lookup = vi.spyOn(photos, "lookup").mockRejectedValue(new Error("Reload failed"));
  vi.spyOn(photos, "preview").mockRejectedValue(new APIError("Photo changed", 409, "photo_display_changed"));
  render(PhotosWorkspace, { photos, cache: { get: vi.fn() } as unknown as PhotoPreviewCache, photoID: "photo-1" });
  await fireEvent.click(await screen.findByRole("button", { name: "Reload photo" }));
  await screen.findByText("Reload failed");
  photos.items = [{ ...photo(1), revision: 2 }]; await tick();
  expect(screen.getByText("Reload failed")).toBeTruthy();
  expect(lookup).toHaveBeenCalledOnce();
  photos.dispose();
});

it("keeps Library recovery below its toolbar and inside the grid", async () => {
  const photos = new Photos("scoped", vi.fn()); photos.started = true; photos.error = "Page unavailable";
  const retry = vi.spyOn(photos, "retry").mockResolvedValue();
  render(PhotosWorkspace, { photos, cache: { get: vi.fn() } as unknown as PhotoPreviewCache });
  expect(screen.getByRole("button", { name: "Move rejects" })).toBeTruthy();
  const alert = screen.getByRole("alert");
  expect(alert.closest(".library-grid")).toBeTruthy();
  expect(document.querySelector(".photo-toolbar")!.compareDocumentPosition(alert) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  await fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
  expect(retry).toHaveBeenCalledOnce();
  photos.dispose();
});

it("renders disabled maps without fetching and keeps the Library selection on Escape", async () => {
 const fetcher=vi.fn();vi.stubGlobal("fetch",fetcher);
 const photos=new Photos("scoped",vi.fn());photos.items=[photo(1)];photos.selectLoaded();
 render(PhotosWorkspace,{tiles,photos,mapMode:true,cache:{get:vi.fn()} as unknown as PhotoPreviewCache,photoID:"",onphotochange:vi.fn()});
 await screen.findByText("Maps are disabled");await fireEvent.keyDown(window,{key:"Escape"});
 expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);expect(fetcher).not.toHaveBeenCalled();photos.dispose();
});

it("shows the Map limit notice only for capped results and bounds loading progress", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  const photos = new Photos("scoped", vi.fn(), false, photoQuery, "map");
  photos.items = [mapPhoto(1), mapPhoto(2)]; photos.started = true; photos.total = 3000; photos.cursor = "next"; photos.loading = true;
  vi.spyOn(photos, "resume").mockResolvedValue();
  render(PhotosWorkspace, { photos, mapMode: true, tiles: { tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}", host: "tiles.example.org", attribution: "Example", max_zoom: 19 }, cache: { get: vi.fn() } as unknown as PhotoPreviewCache });
  const note = `Showing the newest ${mapPhotoLimit.toLocaleString()} of ${(mapPhotoLimit + 1000).toLocaleString()} located photos. Narrow your search or date to see the rest.`;
  expect(screen.queryByText(/^Showing the newest /)).toBeNull();
  photos.total = mapPhotoLimit + 1000;
  await screen.findByText(`Loading located photos: 2 of ${mapPhotoLimit.toLocaleString()}`);
  photos.items = Array.from({ length: mapPhotoLimit }, (_, index) => mapPhoto(index + 1)); photos.loading = false;
  const notice = await screen.findByText(note);
  expect(notice.getAttribute("role")).toBe("status");
  expect(notice.classList.contains("photo-search-limit")).toBe(true);
  photos.cursor = undefined;
  await tick();
  expect(screen.getByText(note)).toBe(notice);
  photos.total = photos.items.length;
  await waitFor(() => expect(screen.queryByText(/^Showing the newest /)).toBeNull());
  photos.items = photos.items.slice(0, -1); photos.total = mapPhotoLimit + 1000; photos.cursor = "next";
  await waitFor(() => expect(screen.queryByText(/^Showing the newest /)).toBeNull());
  photos.cursor = undefined;
  await tick();
  expect(screen.queryByText(/^Showing the newest /)).toBeNull();
  photos.dispose();
});

it("reopens the map where it was after a remount", async () => {
 vi.stubGlobal("ResizeObserver",class {observe() {} disconnect() {}});
 const photos=new Photos("scoped",vi.fn());photos.items=[mapPhoto(1)];photos.total=1;photos.started=true;vi.spyOn(photos,"resume").mockResolvedValue();
 const props={photos,mapMode:true,tiles:{tile_url:"/api/daemon/web-map-tiles/{z}/{x}/{y}",host: "tiles.example.org",attribution:"Example",max_zoom:19},cache:{get:vi.fn()} as unknown as PhotoPreviewCache,photoID:"",onphotochange:vi.fn()};
 const first=render(PhotosWorkspace,props);await vi.dynamicImportSettled();expect(map.props!.view).toBeUndefined();
 map.props!.onview({lat:48.8,lng:2.3,zoom:12});first.unmount();
 render(PhotosWorkspace,props);await vi.dynamicImportSettled();
 expect(map.props!.view).toEqual({lat:48.8,lng:2.3,zoom:12});photos.dispose();
});

function openMapViewerSetup() {
 vi.stubGlobal("ResizeObserver",class {observe() {} disconnect() {}});
 vi.spyOn(HTMLElement.prototype,"clientWidth","get").mockReturnValue(1000);vi.spyOn(HTMLElement.prototype,"clientHeight","get").mockReturnValue(800);
 const photos=new Photos("scoped",vi.fn(),false,photoQuery,"map");photos.items=[mapPhoto(1),mapPhoto(2)];photos.total=2;photos.started=true;vi.spyOn(photos,"resume").mockResolvedValue();
 const lookup=vi.spyOn(photos,"lookup").mockResolvedValue(photo(1));const preview=vi.spyOn(photos,"preview").mockResolvedValue({state:"unsupported"});
 const props={photos,mapMode:true,tiles:{tile_url:"/api/daemon/web-map-tiles/{z}/{x}/{y}",host: "tiles.example.org",attribution:"Example",max_zoom:19},cache:{get:vi.fn()} as unknown as PhotoPreviewCache,photoID:"",onphotochange:vi.fn()};
 return {photos,lookup,preview,props,view:render(PhotosWorkspace,props)};
}
async function openMapViewer({props,view}: ReturnType<typeof openMapViewerSetup>) {
 await vi.dynamicImportSettled();map.onvisible!([mapPhoto(1)]);
 await fireEvent.click(await screen.findByRole("button",{name:"Open Photo 1.jpg"}));
 await view.rerender({...props,photoID:"photo-1"});await screen.findByRole("dialog",{name:"Photo viewer, Photo 1.jpg"});
}
it("disables viewer continuation at the Map ceiling while retaining its cursor", async () => {
 const setup=openMapViewerSetup();const {photos,lookup,props,view}=setup;
 photos.items=Array.from({length:mapPhotoLimit},(_,index)=>mapPhoto(index+1));photos.total=mapPhotoLimit+1000;photos.cursor="remaining";
 lookup.mockResolvedValue(photo(mapPhotoLimit));
 const loadMore=vi.spyOn(photos,"loadMore");
 await vi.dynamicImportSettled();map.onvisible!([mapPhoto(mapPhotoLimit)]);
 await fireEvent.click(await screen.findByRole("button",{name:`Open Photo ${mapPhotoLimit}.jpg`}));
 await view.rerender({...props,photoID:`photo-${mapPhotoLimit}`});await screen.findByRole("dialog",{name:`Photo viewer, Photo ${mapPhotoLimit}.jpg`});
 expect(screen.getAllByRole("button",{name:"Next photo"}).every(button=>(button as HTMLButtonElement).disabled)).toBe(true);
 await fireEvent.keyDown(window,{key:"ArrowRight"});expect(loadMore).not.toHaveBeenCalled();expect(props.onphotochange).toHaveBeenCalledOnce();
 expect(photos.cursor).toBe("remaining");
 photos.items=photos.items.slice(1);await tick();
 expect(screen.getAllByRole("button",{name:"Next photo"}).every(button=>(button as HTMLButtonElement).disabled)).toBe(false);
 photos.dispose();
});
it("revalidates the open Map photo after a reload and a list refresh", async () => {
 const setup=openMapViewerSetup();const {photos,lookup,preview}=setup;
 stubPhotoFetch(vi.fn(async()=>Response.json({items:[],map_items:[{...mapPhoto(1),revision:2}],total:1})));
 preview.mockImplementation(async row=>{if(row.revision===1)throw new APIError("Photo changed",409,"photo_display_changed");return {state:"unsupported"};});
 await openMapViewer(setup);await screen.findByText("Photo changed");lookup.mockResolvedValue({...photo(1),revision:2});
 await fireEvent.click(screen.getByRole("button",{name:"Reload photo"}));
 await waitFor(()=>expect(preview).toHaveBeenCalledWith(expect.objectContaining({revision:2}),"fit",expect.any(AbortSignal)));
 expect(lookup).toHaveBeenCalledTimes(2);
 photos.items=[...photos.items,mapPhoto(3)];await tick();
 expect(lookup).toHaveBeenCalledTimes(2);
 let finish!:(row:ReturnType<typeof photo>)=>void;
 lookup.mockImplementation(()=>new Promise(resolve=>finish=resolve));
 await photos.refresh();
 await waitFor(()=>expect(finish).toBeTypeOf("function"));
 expect(screen.getByRole("dialog",{name:"Photo viewer, Photo 1.jpg"})).toBeTruthy();
 finish({...photo(1),revision:3,name:"Refreshed photo.jpg",content_version_id:"refreshed-version"});
 await screen.findByRole("dialog",{name:"Photo viewer, Refreshed photo.jpg"});
 await waitFor(()=>expect(preview).toHaveBeenCalledWith(expect.objectContaining({content_version_id:"refreshed-version"}),"fit",expect.any(AbortSignal)));
 photos.dispose();
});

it("keeps an open Map photo's detail request through appended pages", async () => {
 const setup=openMapViewerSetup();const {photos,lookup}=setup;
 let finish!:(row:ReturnType<typeof photo>)=>void;
 lookup.mockImplementation(()=>new Promise(resolve=>finish=resolve));
 await vi.dynamicImportSettled();map.onvisible!([photos.items[0] as PhotoMapRow]);
 await fireEvent.click(await screen.findByRole("button",{name:"Open Photo 1.jpg"}));
 await setup.view.rerender({...setup.props,photoID:"photo-1"});
 await waitFor(()=>expect(lookup).toHaveBeenCalledOnce());
 const signal=lookup.mock.calls[0]![1];
 photos.items=[...photos.items,mapPhoto(3)];await tick();
 expect(lookup).toHaveBeenCalledOnce();expect(signal.aborted).toBe(false);
 finish(photo(1));await screen.findByRole("dialog",{name:"Photo viewer, Photo 1.jpg"});
 photos.items=[...photos.items,mapPhoto(4)];await tick();
 expect(lookup).toHaveBeenCalledOnce();
 photos.dispose();
});

it("viewer follows the Map query while the viewport filters its grid", async () => {
 vi.stubGlobal("ResizeObserver",class {observe() {} disconnect() {}});
 vi.spyOn(HTMLElement.prototype,"clientWidth","get").mockReturnValue(1000);vi.spyOn(HTMLElement.prototype,"clientHeight","get").mockReturnValue(800);
 const photos=new Photos("scoped",vi.fn());photos.items=[{...mapPhoto(1),longitude:120},{...mapPhoto(2),latitude:-85}];photos.total=2;photos.started=true;vi.spyOn(photos,"resume").mockResolvedValue();vi.spyOn(photos,"lookup").mockResolvedValue(photo(1));
 vi.spyOn(photos,"preview").mockResolvedValue({state:"unsupported"});const onphotochange=vi.fn();
 const props={photos,mapMode:true,tiles:{tile_url:"/api/daemon/web-map-tiles/{z}/{x}/{y}",host: "tiles.example.org",attribution:"Example",max_zoom:19},cache:{get:vi.fn()} as unknown as PhotoPreviewCache,photoID:"",onphotochange};
 const view=render(PhotosWorkspace,props);await vi.dynamicImportSettled();map.onvisible!([{...mapPhoto(1),longitude:120}]);
 await fireEvent.click(await screen.findByRole("button",{name:"Open Photo 1.jpg"}));expect(screen.queryByRole("button",{name:"Open Photo 2.jpg"})).toBeNull();
 await view.rerender({...props,photoID:"photo-1"});await screen.findByRole("dialog",{name:"Photo viewer, Photo 1.jpg"});
 expect(screen.getByText("1 of 2")).toBeTruthy();await fireEvent.click(screen.getAllByRole("button",{name:"Next photo"})[0]!);expect(onphotochange).toHaveBeenLastCalledWith("photo-2","step");
 map.onvisible!([]);await waitFor(()=>expect(document.querySelector('.photo-layout [data-asset="photo-1"]')).toBeNull());
 expect(screen.getByRole("dialog",{name:"Photo viewer, Photo 1.jpg"})).toBeTruthy();photos.dispose();
});

function mapRecoverySetup() {
 vi.stubGlobal("ResizeObserver",class {observe() {} disconnect() {}});
 vi.spyOn(HTMLElement.prototype,"clientWidth","get").mockReturnValue(400);vi.spyOn(HTMLElement.prototype,"clientHeight","get").mockReturnValue(800);
 const photos=new Photos("scoped",vi.fn());photos.items=[{...photo(1),latitude:0,longitude:120}];photos.started=true;photos.error="Page unavailable";
 photos.selectLoaded();const retry=vi.spyOn(photos,"retry").mockResolvedValue();
 render(PhotosWorkspace,{photos,mapMode:true,tiles:{tile_url:"/api/daemon/web-map-tiles/{z}/{x}/{y}",host: "tiles.example.org",attribution:"Example",max_zoom:19},cache:{get:vi.fn()} as unknown as PhotoPreviewCache,photoID:"",onphotochange:vi.fn()});
 return {photos,retry};
}
it("keeps Map recovery outside mobile tabs", async () => {
 const {photos,retry}=mapRecoverySetup();await vi.dynamicImportSettled();map.onvisible!([]);
 const alert=screen.getByRole("alert");expect(alert.closest(".photo-layout")).toBeNull();await fireEvent.click(within(alert).getByRole("button",{name:"Retry"}));expect(retry).toHaveBeenCalledOnce();
 await fireEvent.click(screen.getByRole("button",{name:"Photos"}));expect(screen.getByRole("alert")).toBe(alert);photos.dispose();
});
it("discloses offscreen selection and replaces it with the visible photos", async () => {
 const {photos}=mapRecoverySetup();photos.items=[mapPhoto(1),mapPhoto(2)];photos.selectLoaded();await vi.dynamicImportSettled();map.onvisible!([photos.items[0] as PhotoMapRow]);
 await fireEvent.click(screen.getByRole("button",{name:"Photos"}));photos.loading=true;await tick();
 await screen.findByText("1 outside this view");await fireEvent.click(screen.getByRole("button",{name:"Move to trash"}));
 const dialog=screen.getByRole("dialog",{name:"Move selected photos to trash"});expect(within(dialog).getByText("1 selected photo is outside this view.")).toBeTruthy();
 await fireEvent.click(within(dialog).getByRole("button",{name:"Keep in Docbank"}));
 const select=screen.getByRole("button",{name:"Select visible photos"});await fireEvent.click(select);expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);photos.dispose();
});
it("distinguishes an empty Map area from no located photos", async () => {
 const {photos}=mapRecoverySetup();await vi.dynamicImportSettled();map.onvisible!([]);
 await fireEvent.click(screen.getByRole("button",{name:"Photos"}));photos.error="";
 await screen.findByText("No photos in this area");photos.items=[];await screen.findByText("No located photos");photos.dispose();
});
it("shows the Map date without Library controls", async () => {
 const {photos}=mapRecoverySetup();photos.date="2024";photos.total=1;await vi.dynamicImportSettled();
 expect(screen.queryByRole("button",{name:"Move rejects"})).toBeNull();
 expect(screen.queryByRole("button",{name:"Timeline"})).toBeNull();expect(screen.queryByRole("button",{name:"Clear date"})).toBeNull();await screen.findByText("1 located photo in 2024");photos.dispose();
});
