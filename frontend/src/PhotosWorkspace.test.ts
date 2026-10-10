import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import PhotosWorkspace from "./PhotosWorkspace.svelte";
import { Photos } from "./photos.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";
import { photo } from "./photo-test-fixtures.js";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); localStorage.clear(); Reflect.deleteProperty(Element.prototype, "scrollIntoView"); });

it("disables search and filters until a delayed Hide finishes", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  let finish!: (response: Response) => void;
  vi.stubGlobal("fetch", vi.fn((url: string) => url.endsWith("/hide") ? new Promise<Response>(resolve => finish = resolve) : Promise.resolve(Response.json({ items: [], total: 0, facets: [] }))));
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1)]; photos.started = true;
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

it("keeps failed confirmation visible and closes after cancellation or success", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const fetcher = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ id: "photo-1", revision: 2 })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Photo changed" }), { status: 412 }))
    .mockResolvedValueOnce(new Response(JSON.stringify({ items: [photo(3)], total: 3 })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ id: "photo-3", revision: 2 })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ items: [photo(2)], total: 1 })));
  vi.stubGlobal("fetch", (url: string, init: RequestInit) => url.endsWith("/photos/assets/query") && JSON.parse(init.body as string).page_size === 1 ? Promise.resolve(Response.json({ facets: [] })) : fetcher(url, init));
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1), photo(2)]; photos.started = true; photos.selectLoaded();
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  const ontrashed = vi.fn();
  render(PhotosWorkspace, { photos, cache, ontrashed });
  await fireEvent.click(screen.getByRole("button", { name: "Move to trash" }));
  const dialog = screen.getByRole("dialog", { name: "Move selected photos to trash" });
  expect(within(dialog).getByText(/^Move 2 selected photos and/)).toBeTruthy();
  await fireEvent.click(within(dialog).getByRole("button", { name: "Move to trash" }));
  await within(dialog).findByText(/Photo changed/);
  expect(ontrashed).toHaveBeenCalledTimes(1);
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
  const view = render(PhotosWorkspace, { photos, cache });
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
  vi.stubGlobal("fetch", vi.fn(() => new Promise<Response>(resolve => finishPage = resolve)));
  render(PhotosWorkspace, { photos, cache: { get } as unknown as PhotoPreviewCache });
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
  render(PhotosWorkspace, { photos, cache });
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
  render(PhotosWorkspace, { photos, cache });
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

it("keeps loading pages that add no rows and keeps the top photo across density changes", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
    const cell = this.closest<HTMLElement>(".cell");
    const scroll = this.closest<HTMLElement>(".photo-scroll");
    return cell && scroll ? new DOMRect(0, Number.parseFloat(cell.style.top) + 56 - scroll.scrollTop, Number.parseFloat(cell.style.width), Number.parseFloat(cell.style.height)) : new DOMRect(0, 0, 1000, 400);
  });
  const items = Array.from({ length: 60 }, (_, index) => photo(index + 1));
  const page = (rows: typeof items, cursor?: string) => new Response(JSON.stringify({ items: rows, total: 60, next_cursor: cursor }));
  const fetcher = vi.fn().mockResolvedValueOnce(page(items.slice(0, 2), "repeat")).mockResolvedValueOnce(page(items.slice(0, 2), "rest"))
    .mockResolvedValueOnce(page(items.slice(2)));
  vi.stubGlobal("fetch", (url: string, init: RequestInit) => JSON.parse(init.body as string).facets.length ? Promise.resolve(new Response(JSON.stringify({ facets: [] }))) : fetcher(url, init));
  const photos = new Photos("scoped", vi.fn());
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache });
  await screen.findByText("60 photos · 60 loaded");
  expect(fetcher).toHaveBeenCalledTimes(3);
  expect(fetcher.mock.calls.map(call => JSON.parse(call[1].body).cursor)).toEqual([undefined, "repeat", "rest"]);
  await waitFor(() => expect(photos.loading).toBe(false));
  const scroll = screen.getByTestId("photo-scroll");
  scroll.scrollTop = 1000;
  await fireEvent.scroll(scroll);
  const anchor = [...scroll.querySelectorAll<HTMLElement>("[data-asset]")].find(cell => cell.getBoundingClientRect().bottom > 56)!;
  const offset = anchor.getBoundingClientRect().top;
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
  await fireEvent.click(screen.getByRole("combobox", { name: "Grid density: Comfortable" }));
  await fireEvent.click(screen.getByRole("option", { name: "Compact" }));
  await waitFor(() => expect(scroll.scrollTop).not.toBe(1000));
  const moved = scroll.querySelector<HTMLElement>(`[data-asset="${anchor.dataset.asset}"]`)!;
  expect(moved.getBoundingClientRect().top).toBeCloseTo(offset);
  photos.dispose();
  await cache.dispose();
});

it("keeps import guidance after clearing an empty search and recognizes numeric zero filters", async () => {
 vi.stubGlobal("ResizeObserver",class { observe() {} disconnect() {} });
 Object.defineProperty(Element.prototype,"scrollIntoView",{configurable:true,value:vi.fn()});
 const photos = new Photos("scoped",vi.fn()); photos.started = true;
 const cache = new PhotoPreviewCache("scoped",vi.fn());
 vi.stubGlobal("fetch",vi.fn().mockImplementation(() => Promise.resolve(new Response(JSON.stringify({items:[],total:0})))));
 render(PhotosWorkspace,{photos,cache});
 await screen.findByText("Your photo library is empty");
 await fireEvent.click(screen.getByRole("combobox",{name:"Sort photos: Capture date"}));
 expect(screen.queryByRole("option",{name:"Relevance"})).toBeNull();
 await fireEvent.click(screen.getByRole("option",{name:"Capture date"}));
 expect(await screen.findByText(/Import photos with docbank photos import/)).toBeTruthy();
 await photos.setQuery({...photos.query,filters:{iso_min:0}});
 await screen.findByText("No matching photos");
 await fireEvent.click(screen.getByRole("button",{name:"Clear filters"}));
 await screen.findByText("Your photo library is empty");
 photos.dispose(); await cache.dispose();
});
it("explains fixed count limits without offering retry", async () => {
  const reason = "byte_budget_exceeded";
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.stubGlobal("fetch", vi.fn(async (_url: string, init: RequestInit) => new Response(JSON.stringify(JSON.parse(init.body as string).facets.length ? { facets: [{ dimension: "camera", available: false, reason }] } : { items: [], total: 0 }))));
  const photos = new Photos("scoped", vi.fn());
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache });
  await screen.findByText("Photo counts exceed the query's limits. Narrow your search or filters.");
  expect(screen.queryByRole("button", { name: "Retry counts" })).toBeNull();
  photos.dispose(); await cache.dispose();
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
