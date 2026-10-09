import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import PhotosWorkspace from "./PhotosWorkspace.svelte";
import { Photos, photoQuery } from "./photos.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";
import { photo } from "./photo-test-fixtures.js";
import { captureDateQuery } from "./photoTimeline.js";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); localStorage.clear(); Reflect.deleteProperty(Element.prototype, "scrollIntoView"); });

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
  vi.stubGlobal("fetch", fetcher);
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  const view = render(PhotosWorkspace, { photos, cache });
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
  vi.stubGlobal("fetch", fetcher);
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
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache });
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
  vi.stubGlobal("fetch", fetcher);
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  const view = render(PhotosWorkspace, { photos, cache });
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
