import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, render, waitFor } from "@testing-library/svelte";
import { tick } from "svelte";
import PhotoGrid from "./PhotoGrid.svelte";
import { createPhotoChunks, createPhotoGrouper, groupPhotos } from "./photoGrid.js";
import { photo } from "./photo-test-fixtures.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";

beforeEach(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it("keeps mounted sections bounded while appending to 50,000 capture sessions", async () => {
  const rows = Array.from({ length: 100_000 }, (_, id) =>
    photo(id, new Date(Date.UTC(2025, 0, 1) - Math.floor(id / 2) * 5 * 3600_000).toISOString()),
  );
  const grouper = createPhotoGrouper(),
    expanded = new Set<string>();
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  const source = rows.slice();
  const initial = grouper.sync(source, "sessions", expanded);
  const props = {
    ...initial,
    loadedItems: source,
    cache,
    selectedIDs: new Set<string>(),
    targetRowHeight: 200,
    loading: false,
    onselect: vi.fn(),
    oncheck: vi.fn(),
    onopen: vi.fn(),
    onloadmore: vi.fn(),
  };
  const view = render(PhotoGrid, props);
  await tick();
  expect(document.querySelectorAll("[data-month]").length).toBeLessThan(12);
  rows.push(...Array.from({ length: 250 }, (_, i) => photo(100_000 + i, "1990-01-01T00:00:00Z")));
  const current = rows.slice();
  await view.rerender({
    ...grouper.sync(current, "sessions", expanded, source),
    appendedFrom: source,
    loadedItems: current,
  });
  expect(document.querySelectorAll("[data-month]").length).toBeLessThan(12);
  await waitFor(() => expect(document.querySelector('[data-asset="photo-0"]')).toBeTruthy());
  await view.rerender({ scrollTop: 12_800_000 });
  expect(document.querySelectorAll("[data-month]").length).toBeLessThan(12);
  expect(document.querySelector('[data-asset="photo-100000"]')).toBeTruthy();
  await cache.dispose();
});

it("keeps loading pages while a date jump is pending", async () => {
  const rows = Array.from({ length: 20_000 }, (_, id) => photo(id, "2025-06-01T12:00:00Z"));
  const grouper = createPhotoGrouper(),
    expanded = new Set<string>();
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  const load = vi.fn();
  const source = rows.slice();
  const view = render(PhotoGrid, {
    ...grouper.sync(source, "months", expanded),
    loadedItems: source,
    cache,
    selectedIDs: new Set<string>(),
    targetRowHeight: 200,
    loading: false,
    more: true,
    seek: "2024-01",
    onselect: vi.fn(),
    oncheck: vi.fn(),
    onopen: vi.fn(),
    onloadmore: load,
  });
  await tick();
  await waitFor(() => expect(load).toHaveBeenCalled());
  load.mockClear();
  rows.push(...Array.from({ length: 250 }, (_, i) => photo(20_000 + i, "2025-05-01T12:00:00Z")));
  const current = rows.slice();
  await view.rerender({
    ...grouper.sync(current, "months", expanded, source),
    appendedFrom: source,
    loadedItems: current,
  });
  await waitFor(() => expect(load).toHaveBeenCalled());
  await cache.dispose();
});

it("restores an anchor moved into another group while its original group survives", async () => {
  let laidOut: ReturnType<ReturnType<typeof createPhotoChunks>> = [];
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement,
  ) {
    const cell = this.closest<HTMLElement>(".cell"),
      section = this.closest<HTMLElement>("[data-month]");
    const scroll = this.closest<HTMLElement>(".photo-scroll");
    return cell && scroll
      ? new DOMRect(
          0,
          (laidOut.find((chunk) => chunk.group.key === section?.dataset.month)?.offset ?? 0) +
            Number.parseFloat(cell.style.top) +
            56 -
            scroll.scrollTop,
          100,
          Number.parseFloat(cell.style.height),
        )
      : new DOMRect(0, 0, 1000, 400);
  });
  const rows = Array.from({ length: 30 }, (_, id) => photo(id, "2025-06-01T12:00:00Z"));
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  const view = render(PhotoGrid, {
    groups: groupPhotos(rows, "months"),
    loadedItems: rows,
    cache,
    selectedIDs: new Set<string>(),
    targetRowHeight: 200,
    loading: false,
    onselect: vi.fn(),
    oncheck: vi.fn(),
    onopen: vi.fn(),
    onloadmore: vi.fn(),
  });
  await tick();
  await waitFor(() => expect(document.querySelector('[data-asset="photo-0"]')).toBeTruthy());
  laidOut = createPhotoChunks()(groupPhotos(rows, "months"), 1000, 200);
  const restore = view.component.preservePosition();
  const changed = rows.map((row, i) =>
    i === 0 ? { ...row, capture_time: "2025-05-01T12:00:00Z" } : row,
  );
  laidOut = createPhotoChunks()(groupPhotos(changed, "months"), 1000, 200);
  await view.rerender({ groups: groupPhotos(changed, "months"), loadedItems: changed });
  await restore();
  expect(view.getByTestId("photo-scroll").scrollTop).toBeGreaterThan(0);
  const anchor = document.querySelector<HTMLElement>('[data-asset="photo-0"]')!;
  expect(anchor.getBoundingClientRect().top).toBe(56);
  await cache.dispose();
});
