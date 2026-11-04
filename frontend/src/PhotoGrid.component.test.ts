import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, render } from "@testing-library/svelte";
import { tick } from "svelte";
import PhotoGrid from "./PhotoGrid.svelte";
import { createPhotoGrouper } from "./photoGrid.js";
import { arrayView } from "./photoRows.js";
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
  const props = {
    groups: grouper.sync(arrayView(rows), "sessions", expanded),
    loadedItems: arrayView(rows),
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
  let reads = 0;
  for (const group of props.groups) {
    const key = group.key;
    Object.defineProperty(group, "key", {
      get: () => {
        reads++;
        return key;
      },
    });
  }
  rows.push(...Array.from({ length: 250 }, (_, i) => photo(100_000 + i, "1990-01-01T00:00:00Z")));
  await view.rerender({
    groups: grouper.sync(arrayView(rows), "sessions", expanded),
    loadedItems: arrayView(rows),
  });
  expect(reads).toBeLessThan(100);
  expect(document.querySelectorAll("[data-month]").length).toBeLessThan(12);
  expect(document.querySelector('[data-asset="photo-0"]')).toBeTruthy();
  await view.rerender({ scrollTop: 12_800_000 });
  expect(document.querySelectorAll("[data-month]").length).toBeLessThan(12);
  expect(document.querySelector('[data-asset="photo-100000"]')).toBeTruthy();
  await cache.dispose();
});
