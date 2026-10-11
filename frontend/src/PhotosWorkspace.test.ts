import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import PhotosWorkspace from "./PhotosWorkspace.svelte";
import { APIError } from "./api-transport.js";
import { Photos } from "./photos.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";
import { photo } from "./photo-test-fixtures.js";

afterEach(() => { cleanup(); history.replaceState(null, "", "/"); vi.unstubAllGlobals(); vi.restoreAllMocks(); localStorage.clear(); Reflect.deleteProperty(Element.prototype, "scrollIntoView"); });

it("keeps failed confirmation visible and closes after cancellation or success", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const fetcher = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ id: "photo-1", revision: 2 })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Photo changed" }), { status: 412 }))
    .mockResolvedValueOnce(new Response(JSON.stringify({ items: [photo(3)], total: 3 })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ id: "photo-3", revision: 2 })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ items: [photo(2)], total: 1 })));
  vi.stubGlobal("fetch", fetcher);
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
  vi.stubGlobal("fetch", fetcher);
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
  vi.stubGlobal("fetch", vi.fn(() => new Promise<Response>(resolve => finishPage = resolve)));
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
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache, photoID: "", onphotochange: vi.fn() });
  await screen.findByText("60 photos · 60 loaded");
  expect(fetcher).toHaveBeenCalledTimes(3);
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
  let failed = scenario === "success" || scenario === "grid"; vi.stubGlobal("fetch",fetcher);
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
    expect(photos.items[0]!.content_version_id).toBe("fresh-version");
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
  vi.stubGlobal("fetch", fetcher);
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
  await waitFor(() => expect(photos.items[0]!.content_version_id).toBe(fresh.content_version_id)); await screen.findByText("Fit preview");
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
    const version = photos.items[0]!.content_version_id;
    finish({...photo(1), content_version_id:"late-version"}); await new Promise(resolve => setTimeout(resolve, 0));
    expect(photos.items[0]!.content_version_id).toBe(version); expect(screen.getByRole("dialog", {name:"Photo viewer, Photo 2.jpg"})).toBeTruthy();
  }
  await view.rerender(props);
  await screen.findByRole("dialog", {name:"Photo viewer, Photo 1.jpg"});
  expect(lookup).toHaveBeenCalledOnce();
});
