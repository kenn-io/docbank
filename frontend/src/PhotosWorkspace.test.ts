import { afterEach, expect, it, vi } from "vite-plus/test";
import { tick } from "svelte";
import { createShortcutManager } from "@kenn-io/kit-ui";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import PhotosWorkspace from "./PhotosWorkspace.svelte";
import { snapshotEnvelope } from "./snapshot-test-fixtures.js";
import { PhotoCull } from "./photoCull.svelte.js";
import { photoCullStorage } from "./photoCullStorage.js";
import { IDBFactory, IDBKeyRange } from "fake-indexeddb";
import { idbRequest, transactionDone } from "./indexedDB.js";
import * as api from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { Photos, photoQuery, mapPhotoLimit, type MapView } from "./photos.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";
import {
  photoFace,
  photoMetadataResponse,
  photoPreparation,
  photoFacePage,
  preparedPhoto,
  mapPhoto,
  photo,
  touchTap,
} from "./photo-test-fixtures.js";
import { captureDateQuery } from "./photoTimeline.js";
import type {
  PhotoAsset,
  PhotoAuthoredReceipt,
  PhotoBrowseRow,
  PhotoMapRow,
} from "./generated/docbank.js";

// Answers background filter-count requests so tests that predate counts see only their own reads.
const stubPhotoFetch = (fetcher: (url: string, init: RequestInit) => unknown) =>
  vi.stubGlobal(
    "fetch",
    (url: string, init: RequestInit) =>
      photoMetadataResponse(url, init) ??
      (url.endsWith("/photos/assets/query") && JSON.parse(String(init?.body ?? "{}")).facets?.length
        ? Promise.resolve(Response.json({ items: [], total: 0, facets: [] }))
        : fetcher(url, init)),
  );

const map = vi.hoisted(() => ({
  onvisible: undefined as ((rows: PhotoMapRow[]) => void) | undefined,
  props: undefined as { view?: MapView; onview: (view: MapView) => void } | undefined,
}));
vi.mock("./PhotoMap.svelte", () => ({
  default: (
    _anchor: unknown,
    props: {
      onvisible: (rows: PhotoMapRow[]) => void;
      view?: MapView;
      onview: (view: MapView) => void;
    },
  ) => {
    map.onvisible = props.onvisible;
    map.props = props;
    return {};
  },
}));

const tiles = { tile_url: "", host: "", attribution: "", max_zoom: 19 };

afterEach(() => {
  cleanup();
  map.onvisible = undefined;
  map.props = undefined;
  history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  localStorage.clear();
  new Photos("scoped", vi.fn()).setDensity("comfortable");
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

it.each([false, true])(
  "preserves subsequent scrolling when an interrupted refresh resumes after remount, map=%s",
  async (mapMode) => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(
      function (this: HTMLElement) {
        const cell = this.closest<HTMLElement>(".cell");
        const scroll = this.closest<HTMLElement>(".photo-scroll");
        return cell && scroll
          ? new DOMRect(
              0,
              Number.parseFloat(cell.style.top) + 56 - scroll.scrollTop,
              Number.parseFloat(cell.style.width),
              Number.parseFloat(cell.style.height),
            )
          : new DOMRect(0, 0, 1000, 400);
      },
    );
    const items = Array.from({ length: 60 }, (_, index) =>
      mapMode ? mapPhoto(index + 1) : photo(index + 1),
    );
    let finishRefresh!: (response: Response) => void;
    stubPhotoFetch(
      vi
        .fn()
        .mockImplementationOnce(() => new Promise(() => {}))
        .mockImplementationOnce(
          () => new Promise<Response>((resolve) => (finishRefresh = resolve)),
        ),
    );
    const photos = new Photos("scoped", vi.fn(), false, photoQuery, {
      projection: mapMode ? "map" : undefined,
    });
    photos.items = items;
    photos.total = items.length;
    photos.started = true;
    photos.scrollTop = 500;
    const cache = new PhotoPreviewCache("scoped", vi.fn());
    const props = {
      photos,
      cache,
      mapMode,
      tiles: {
        tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}",
        host: "tiles.example.org",
        attribution: "Example",
        max_zoom: 19,
      },
    };
    const view = render(PhotosWorkspace, props);
    if (mapMode) {
      await vi.dynamicImportSettled();
      map.onvisible!(items as PhotoMapRow[]);
      await tick();
    }
    await waitFor(() => expect(screen.getByTestId("photo-scroll").scrollTop).toBe(500));
    await fireEvent.click(screen.getByRole("button", { name: "Refresh previews" }));
    view.unmount();
    render(PhotosWorkspace, props);
    if (mapMode) {
      await vi.dynamicImportSettled();
      map.onvisible!(items as PhotoMapRow[]);
      await tick();
    }
    const scroll = screen.getByTestId("photo-scroll");
    await waitFor(() => expect(scroll.scrollTop).toBe(500));
    scroll.scrollTop = 1000;
    await fireEvent.scroll(scroll);
    const refreshed = items.map((item) => ({
      ...item,
      previews: { ...item.previews, grid: { state: "ready", generation_id: "refreshed" } },
    }));
    finishRefresh(
      new Response(
        JSON.stringify({ [mapMode ? "map_items" : "items"]: refreshed, total: items.length }),
      ),
    );
    await waitFor(() => expect(photos.loading).toBe(false));
    await tick();
    expect(scroll.scrollTop).toBe(1000);
    expect(photos.scrollTop).toBe(1000);
    photos.dispose();
    await cache.dispose();
  },
);

it("disables search and filters until a delayed Hide finishes", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  let finish!: (response: Response) => void;
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) =>
      url.endsWith("/hide")
        ? new Promise<Response>((resolve) => (finish = resolve))
        : Promise.resolve(Response.json({ items: [], total: 0, facets: [] })),
    ),
  );
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1)];
  photos.started = true;
  photos.baseQuery = photos.query = { ...photos.query, text: "Canon" };
  photos.facets = [
    {
      dimension: "camera",
      available: true,
      reason: "",
      total: 1,
      missing: 0,
      other: 0,
      values: [{ key: "Canon", label: "Canon", count: 1, selected: false }],
    },
  ];
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache });
  const controls = [
    screen.getByRole("searchbox", { name: "Search photos" }),
    screen.getByRole("button", { name: "Search" }),
    screen.getByRole("combobox", { name: "Sort photos: Capture date" }),
    screen.getByRole("button", { name: "Clear filters" }),
    screen.getByRole("button", { name: "Canon, 1 photos" }),
  ];
  expect(screen.getByRole("button", { name: "Clear filters" }).getAttribute("type")).toBe("button");
  const pending = photos.setHidden("photo-1");
  await waitFor(() => {
    for (const control of controls) expect(control.hasAttribute("disabled")).toBe(true);
  });
  finish(Response.json({ id: "photo-1", revision: 2 }));
  await pending;
  await waitFor(() => {
    for (const control of controls.slice(0, 4))
      expect(control.hasAttribute("disabled")).toBe(false);
  });
  photos.dispose();
  await cache.dispose();
});

it("restores applied search after a facet click and keeps empty-library guidance when clearing filters", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
  const fetcher = vi
    .fn()
    .mockImplementation(() => Promise.resolve(Response.json({ items: [], total: 0, facets: [] })));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.started = true;
  photos.facets = [
    {
      dimension: "camera",
      available: true,
      reason: "",
      total: 1,
      missing: 0,
      other: 0,
      values: [{ key: "Canon", label: "Canon", count: 1, selected: false }],
    },
  ];
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache });
  await screen.findByText("Your photo library is empty");
  expect(
    await screen.findByText(
      /Choose a folder or files with Add photos\. Large camera cards on the daemon's own disk import faster with docbank photos import\./,
    ),
  ).toBeTruthy();
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
  await fireEvent.click(screen.getByRole("option", { name: "Capture date" }));
  await photos.setQuery({ ...photos.query, filters: { iso_min: 0 } });
  await screen.findByText("No matching photos");
  await fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
  await screen.findByText("Your photo library is empty");
  expect(
    screen.getByText(
      /Choose a folder or files with Add photos\. Large camera cards on the daemon's own disk import faster with docbank photos import\./,
    ),
  ).toBeTruthy();
  photos.dispose();
  await cache.dispose();
});

it("keeps failed confirmation visible and closes after cancellation or success", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  let listings = 0;
  const fetcher = vi.fn(async (url: string, options: RequestInit) => {
    if (url.endsWith("/query")) {
      const ids = JSON.parse(String(options.body)).query.filters?.asset_ids;
      if (ids)
        return Response.json({
          items: [photo(2)].filter((item) => ids.includes(item.asset_id)),
          total: 1,
        });
      const first = ++listings === 1;
      return Response.json({ items: [photo(first ? 3 : 2)], total: first ? 3 : 1 });
    }
    if (url.endsWith("/photo-2/trash"))
      return Response.json({ detail: "Photo changed" }, { status: 412 });
    return Response.json({ id: url.split("/").at(-2), revision: 2 });
  });
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1), photo(2)];
  photos.started = true;
  photos.selectLoaded();
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
  await waitFor(() =>
    expect(screen.queryByRole("dialog", { name: "Move selected photos to trash" })).toBeNull(),
  );
  expect(ontrashed).toHaveBeenCalledTimes(2);
  photos.dispose();
  await cache.dispose();
});

it("does not write the user's scroll position back to the grid", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  stubPhotoFetch(vi.fn());
  const photos = new Photos("scoped", vi.fn());
  photos.items = Array.from({ length: 60 }, (_, index) => photo(index + 1));
  photos.total = 60;
  photos.started = true;
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache, photoID: "", onphotochange: vi.fn() });
  await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  const scroll = screen.getByTestId("photo-scroll");
  let top = 0;
  const writes: number[] = [];
  Object.defineProperty(scroll, "scrollTop", {
    configurable: true,
    get: () => top,
    set: (value: number) => {
      writes.push(value);
      top = value;
    },
  });
  top = 500;
  await fireEvent.scroll(scroll);
  await waitFor(() => expect(photos.scrollTop).toBe(500));
  expect(writes).toEqual([]);
  photos.dispose();
  await cache.dispose();
});

it("starts no extra detail request after a successful reload", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const photos = new Photos("scoped", vi.fn());
  photos.started = true;
  vi.spyOn(photos, "resume").mockResolvedValue();
  const lookup = vi
    .spyOn(photos, "lookup")
    .mockResolvedValueOnce(photo(1))
    .mockResolvedValueOnce({ ...photo(1), name: "Reloaded photo.jpg" })
    .mockRejectedValue(new Error("Duplicate lookup failed"));
  vi.spyOn(photos, "preview")
    .mockRejectedValueOnce(new APIError("Photo changed", 409, "photo_display_changed"))
    .mockResolvedValue({ state: "unsupported" });
  render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    photoID: "photo-1",
  });
  await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
  await screen.findByText("Photo changed");
  await fireEvent.click(screen.getByRole("button", { name: "Reload photo" }));
  await screen.findByRole("dialog", { name: "Photo viewer, Reloaded photo.jpg" });
  await waitFor(() => expect(screen.queryByRole("button", { name: "Reload photo" })).toBeNull());
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
])(
  "stages timeline period %s, retains the grid on failure and restarts paging on Retry",
  async (date, previous, name, after, before, label) => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
    const photos = new Photos("scoped", vi.fn());
    photos.started = true;
    photos.view = "timeline";
    photos.date = previous;
    photos.query = captureDateQuery(photoQuery, previous);
    photos.items = Array.from({ length: 60 }, (_, index) => photo(index + 1));
    photos.total = 100;
    photos.cursor = "old-page";
    photos.timeline = {
      dimension: "capture_day",
      available: true,
      total: 210,
      missing: 10,
      other: 0,
      values: [
        { key: "9999-12-31", label: "9999-12-31", count: 10, selected: false },
        { key: "2025-12-15", label: "2025-12-15", count: 100, selected: false },
        { key: "2024-02-29", label: "2024-02-29", count: 100, selected: false },
      ],
    };
    let finish!: (response: Response) => void;
    const replacement = Array.from({ length: 60 }, (_, index) =>
      photo(index + 101, `${after}T12:00:00`),
    );
    const fetcher = vi
      .fn()
      .mockImplementationOnce(() => new Promise((resolve) => (finish = resolve)))
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ items: replacement, total: 100, next_cursor: "new-page" })),
      )
      .mockResolvedValueOnce(new Response(JSON.stringify({ items: [photo(200)], total: 100 })));
    stubPhotoFetch(fetcher);
    const cache = new PhotoPreviewCache("scoped", vi.fn());
    const view = render(PhotosWorkspace, { photos, cache, photoID: "", onphotochange: vi.fn() });
    const cell = await screen.findByRole("button", { name: "Select Photo 1.jpg" });
    await fireEvent.click(cell);
    const scroll = screen.getByTestId("photo-scroll");
    scroll.scrollTop = 500;
    await fireEvent.scroll(scroll);
    const heading = document.querySelector(".library-title span")!.textContent;
    const button = screen.getByRole(date.length === 4 ? "navigation" : "region", {
      name: date.length === 4 ? "Timeline years" : "Photo timeline",
    });
    await fireEvent.click(
      [...button.querySelectorAll("button")].find((button) =>
        name.test(button.textContent!.trim()),
      )!,
    );
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
    expect(JSON.parse(fetcher.mock.calls[2][1].body)).toMatchObject({
      cursor: "new-page",
      query: { filters },
    });
    view.unmount();
    photos.dispose();
    await cache.dispose();
  },
);

it("keeps loaded photos visible on paging failure and selects with touch checkboxes", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const fetcher = vi
    .fn()
    .mockResolvedValueOnce(
      new Response(JSON.stringify({ items: [photo(1), photo(2)], total: 3, next_cursor: "next" })),
    )
    .mockResolvedValueOnce(
      new Response(JSON.stringify({ detail: "Page unavailable" }), { status: 503 }),
    )
    .mockResolvedValueOnce(
      new Response(JSON.stringify({ items: [photo(3, "2024-01-01T12:00:00")], total: 3 })),
    );
  vi.stubGlobal("fetch", (url: string, init: RequestInit) =>
    JSON.parse(init.body as string).page_size === 1
      ? Promise.resolve(new Response(JSON.stringify({ facets: [] })))
      : fetcher(url, init),
  );
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
  expect(
    screen.getByRole("navigation", { name: "Photo years" }).querySelectorAll("button"),
  ).toHaveLength(2);
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
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(
    function (this: HTMLElement) {
      const cell = this.closest<HTMLElement>(".cell");
      const scroll = this.closest<HTMLElement>(".photo-scroll");
      return cell && scroll
        ? new DOMRect(
            0,
            Number.parseFloat(cell.style.top) + 56 - scroll.scrollTop,
            Number.parseFloat(cell.style.width),
            Number.parseFloat(cell.style.height),
          )
        : new DOMRect(0, 0, 1000, 400);
    },
  );
  vi.stubGlobal(
    "URL",
    class extends URL {
      static createObjectURL() {
        return "blob:synthetic";
      }
      static revokeObjectURL() {}
    },
  );
  const photos = new Photos("scoped", vi.fn());
  const initial = Array.from({ length: 6 }, (_, index) =>
    photo(index + 1, `2025-06-01T${12 + index}:00:00`),
  );
  for (const item of initial) item.previews.grid = { state: "ready", generation_id: "generation" };
  photos.items = initial;
  photos.started = true;
  photos.cursor = "older";
  photos.grouping = "sessions";
  let finishPreview!: (blob: Blob) => void;
  const get = vi.fn((id: string, _generation: string, _signal: AbortSignal) =>
    id === "photo-2"
      ? new Promise<Blob>((resolve) => (finishPreview = resolve))
      : Promise.resolve(new Blob(["synthetic"])),
  );
  let finishPage!: (response: Response) => void;
  stubPhotoFetch(vi.fn(() => new Promise<Response>((resolve) => (finishPage = resolve))));
  render(PhotosWorkspace, {
    photos,
    cache: { get } as unknown as PhotoPreviewCache,
    photoID: "",
    onphotochange: vi.fn(),
  });
  const image = await screen.findByRole("img", { name: "Photo 1.jpg" });
  await waitFor(() => expect(get).toHaveBeenCalledTimes(6));
  const pendingSignal = get.mock.calls.find(([id]) => id === "photo-2")![2];
  finishPage(
    new Response(
      JSON.stringify({
        items: Array.from({ length: 60 }, (_, index) => photo(index + 7, "2025-06-01T11:00:00")),
        total: 66,
      }),
    ),
  );
  await waitFor(() => expect(photos.loading).toBe(false));
  expect(image.isConnected).toBe(true);
  expect(screen.getByRole("img", { name: "Photo 1.jpg" })).toBe(image);
  expect(pendingSignal.aborted).toBe(false);
  expect(
    get.mock.calls.filter(([id]) => initial.some((item) => item.asset_id === id)),
  ).toHaveLength(6);
  finishPreview(new Blob(["completed"]));
  await screen.findByRole("img", { name: "Photo 2.jpg" });
  photos.dispose();
});

it("shows both years of a New Year session and jumps to the newest containing group", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [
    photo(1, "2026-02-01T12:00:00"),
    photo(2, "2025-01-01T01:00:00"),
    photo(3, "2024-12-31T23:00:00"),
    photo(4, "0000-01-02"),
  ];
  photos.started = true;
  photos.grouping = "sessions";
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache, photoID: "", onphotochange: vi.fn() });
  await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  const rail = screen.getByRole("navigation", { name: "Photo years" });
  expect([...rail.querySelectorAll("button")].map((button) => button.textContent?.trim())).toEqual([
    "2026",
    "2025",
    "2024",
  ]);
  const scroll = screen.getByTestId("photo-scroll");
  const scrollTo = vi.fn();
  scroll.scrollTo = scrollTo;
  const newerGroup = scroll.querySelector<HTMLElement>('[data-month="photo-1"]')!;
  const session = scroll.querySelector<HTMLElement>('[data-month="photo-2"]')!;
  expect(session.getAttribute("aria-label")).toBe("2024-12-31 · Capture session");
  expect(
    [...session.querySelectorAll("[data-asset]")].map((cell) => cell.getAttribute("data-asset")),
  ).toEqual(["photo-2", "photo-3"]);
  await fireEvent.click(screen.getByRole("button", { name: "2024" }));
  expect(scrollTo).toHaveBeenLastCalledWith({
    top: Math.ceil(Number.parseFloat(newerGroup.style.height)),
  });
  await fireEvent.click(screen.getByRole("button", { name: "2025" }));
  expect(scrollTo).toHaveBeenLastCalledWith({
    top: Math.ceil(Number.parseFloat(newerGroup.style.height)),
  });
  await fireEvent.click(screen.getByRole("button", { name: "2026" }));
  expect(scrollTo).toHaveBeenLastCalledWith({ top: 0 });
  photos.items = [...photos.items, photo(5, "2025-02-01T12:00:00")];
  await screen.findByRole("button", { name: "Select Photo 5.jpg" });
  await fireEvent.click(screen.getByRole("button", { name: "2025" }));
  expect(scrollTo).toHaveBeenLastCalledWith({
    top: Math.ceil(Number.parseFloat(newerGroup.style.height)),
  });
  photos.dispose();
  await cache.dispose();
});

it("clears selection with Escape from focused photo controls while honoring shortcut suppression", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1)];
  photos.started = true;
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
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  const photos = new Photos("scoped", vi.fn());
  photos.items = Array.from({ length: 60 }, (_, index) => photo(index + 1));
  photos.total = 60;
  photos.started = true;
  photos.scrollTop = 1000;
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  new Photos("scoped", vi.fn()).setDensity("compact");
  render(PhotosWorkspace, { photos, cache, photoID: "", onphotochange: vi.fn() });
  await waitFor(() => expect(screen.getByTestId("photo-scroll").scrollTop).toBe(1000));
  photos.dispose();
  await cache.dispose();
});

it.each([
  { kind: "density", title: "density changes while repeated-cursor pages add no rows" },
  { kind: "relevance density", title: "relevance density changes" },
  { kind: "album trash", title: "album trash" },
])("keeps the top photo after $title", async ({ kind }) => {
  const density = kind.endsWith("density");
  const relevance = kind === "relevance density";
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(
    function (this: HTMLElement) {
      const cell = this.closest<HTMLElement>(".cell");
      const scroll = this.closest<HTMLElement>(".photo-scroll");
      const contentOffset = cell?.closest(".photo-month")?.querySelector("h2") ? 56 : 12;
      return cell && scroll
        ? new DOMRect(
            0,
            Number.parseFloat(cell.style.top) + contentOffset - scroll.scrollTop,
            Number.parseFloat(cell.style.width),
            Number.parseFloat(cell.style.height),
          )
        : new DOMRect(0, 0, 1000, 400);
    },
  );
  const photos = new Photos("scoped", vi.fn());
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  if (relevance)
    photos.query = photos.baseQuery = {
      ...photos.query,
      text: "Canon",
      sort: { field: "relevance", direction: "desc" },
    };
  const items = Array.from({ length: 60 }, (_, index) => photo(index + 1));
  const page = (rows: typeof items, cursor?: string) =>
    new Response(JSON.stringify({ items: rows, total: 60, next_cursor: cursor }));
  const fetcher = vi.fn();
  if (kind === "album trash") {
    photos.query = {
      ...photos.query,
      filters: { set_ids: ["album"] },
      sort: { field: "added_time", direction: "desc" },
    };
    photos.items = items;
    photos.total = 60;
    photos.started = true;
    fetcher
      .mockResolvedValueOnce(new Response(JSON.stringify({ id: "photo-1", revision: 2 })))
      .mockResolvedValueOnce(new Response(JSON.stringify({ items: items.slice(1), total: 59 })));
  } else if (relevance) {
    fetcher.mockResolvedValueOnce(page(items));
  } else {
    fetcher
      .mockResolvedValueOnce(page(items.slice(0, 2), "repeat"))
      .mockResolvedValueOnce(page(items.slice(0, 2), "rest"))
      .mockResolvedValueOnce(page(items.slice(2)));
  }
  stubPhotoFetch(fetcher);
  render(PhotosWorkspace, {
    photos,
    cache,
    albumID: kind === "album trash" ? "album" : undefined,
    photoID: "",
    onphotochange: vi.fn(),
  });
  if (density) {
    await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
  } else {
    await screen.findByText("60 photos · 60 loaded");
  }
  if (kind === "density") {
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(3));
    expect(fetcher.mock.calls.map((call) => JSON.parse(call[1].body).cursor)).toEqual([
      undefined,
      "repeat",
      "rest",
    ]);
  }
  await waitFor(() => expect(photos.loading).toBe(false));
  const scroll = screen.getByTestId("photo-scroll");
  scroll.scrollTop = 1000;
  await fireEvent.scroll(scroll);
  const anchor = [...scroll.querySelectorAll<HTMLElement>("[data-asset]")].find(
    (cell) => cell.getBoundingClientRect().bottom > (relevance ? 12 : 56),
  )!;
  const offset = anchor.getBoundingClientRect().top;
  if (kind === "album trash") {
    photos.select(
      "photo-1",
      new MouseEvent("click"),
      photos.items.map((item) => item.asset_id),
    );
    await fireEvent.click(await screen.findByRole("button", { name: "Move to trash" }));
    await fireEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", { name: "Move to trash" }),
    );
    await waitFor(() => expect(photos.trashing).toBe(false));
  } else {
    Object.defineProperty(Element.prototype, "scrollIntoView", {
      configurable: true,
      value: vi.fn(),
    });
    await fireEvent.click(screen.getByRole("combobox", { name: "Grid density: Comfortable" }));
    await fireEvent.click(screen.getByRole("option", { name: "Compact" }));
    await waitFor(() => expect(scroll.scrollTop).not.toBe(1000));
  }
  const moved = scroll.querySelector<HTMLElement>(`[data-asset="${anchor.dataset.asset}"]`)!;
  expect(moved.getBoundingClientRect().top).toBeCloseTo(offset);
  photos.dispose();
  await cache.dispose();
});

it("opens on touch with empty selection and toggles membership on touch taps", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.stubGlobal(
    "matchMedia",
    vi.fn(() => ({ matches: true })),
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1), photo(2)];
  photos.started = true;
  const open = vi.fn();
  render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    photoID: "",
    onphotochange: open,
  });
  const cell = await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  await fireEvent.click(cell);
  expect(open).not.toHaveBeenCalled();
  expect(photos.selection.selectedIDs.has("photo-1")).toBe(true);
  photos.clearSelection();
  await touchTap(cell, 1000, 1);
  expect(open).toHaveBeenCalledWith("photo-1", "open");
  expect(photos.selection.selectedIDs.size).toBe(0);
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  vi.stubGlobal(
    "matchMedia",
    vi.fn(() => ({ matches: false })),
  );
  const second = screen.getByRole("button", { name: "Select Photo 2.jpg" });
  await touchTap(second, 1351, 1);
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1", "photo-2"]);
  await touchTap(second, 1702, 1);
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  await fireEvent.keyDown(cell, { key: "Enter" });
  expect(open).toHaveBeenCalledTimes(2);
});

it.each(["success", "grid", "removed", "pending"])(
  "keeps query order and viewer membership after %s continuation",
  async (scenario) => {
    const expired = scenario === "removed";
    const removed = expired || scenario === "pending";
    const initial = [photo(1, "2024-04-30T23:30:00-02:00"), photo(2, "2024-05-01T00:00:00Z")],
      following = photo(3, "2024-05-01T00:30:00+02:00");
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
    const fetcher = vi.fn(async (url: string, init: RequestInit) => {
      if (url.endsWith("/preview")) return new Response(JSON.stringify({ state: "unsupported" }));
      const body = JSON.parse(String(init.body));
      if (!failed) {
        failed = true;
        return new Response(
          JSON.stringify({
            detail: "Continuation failed",
            code: expired ? "cursor_expired" : "unavailable",
          }),
          { status: expired ? 422 : 503 },
        );
      }
      if (expired && !body.cursor)
        return new Response(
          JSON.stringify({
            items: scenario === "removed" ? [initial[0]] : initial,
            total: 3,
            next_cursor: "replacement",
          }),
        );
      return new Response(JSON.stringify({ items: [following], total: 3 }));
    });
    let failed = scenario === "success" || scenario === "grid";
    stubPhotoFetch(fetcher);
    const photos = new Photos("scoped", vi.fn());
    photos.items = initial;
    photos.total = 3;
    photos.cursor = "old";
    photos.started = true;
    photos.loading = true;
    if (scenario === "grid") photos.error = "Continuation failed";
    let finishLookup!: (row: ReturnType<typeof photo>) => void;
    if (removed)
      vi.spyOn(photos, "lookup").mockImplementation(
        () => new Promise((resolve) => (finishLookup = resolve)),
      );
    let finishPage!: () => void;
    const loadMore =
      scenario === "pending"
        ? vi
            .spyOn(photos, "loadMore")
            .mockImplementation(() => new Promise<void>((resolve) => (finishPage = resolve)))
        : undefined;
    const cache = { get: vi.fn() } as unknown as PhotoPreviewCache;
    let view: ReturnType<typeof render>;
    const onphotochange = vi.fn((id: string) => {
      history.replaceState(null, "", `/photos#photo=${id}`);
      void view.rerender({ photos, cache, photoID: id, onphotochange });
    });
    view = render(PhotosWorkspace, { photos, cache, photoID: "", onphotochange });
    await fireEvent.keyDown(
      await screen.findByRole("button", {
        name: scenario === "grid" ? "Select Photo 1.jpg" : "Select Photo 2.jpg",
      }),
      { key: "Enter" },
    );
    await screen.findByRole("navigation", { name: "Photos in current result" });
    photos.loading = false;
    if (scenario !== "grid")
      await fireEvent.keyDown(screen.getByRole("dialog"), { key: "ArrowRight" });
    const returnRecord = photos.viewerReturn,
      currentURL = location.href;
    if (scenario === "pending") {
      await waitFor(() => expect(loadMore).toHaveBeenCalledOnce());
    } else if (scenario !== "success") {
      await screen.findByRole("button", { name: "Retry navigation" });
      expect(within(screen.getByRole("dialog")).getByText("Continuation failed")).toBeTruthy();
      expect(
        screen
          .getAllByRole("button", { name: "Next photo" })
          .every((button) => (button as HTMLButtonElement).disabled),
      ).toBe(scenario !== "grid");
      await fireEvent.click(screen.getByRole("button", { name: "Retry navigation" }));
      await waitFor(() => expect(photos.error).toBe(""));
      expect(onphotochange).toHaveBeenCalledTimes(1);
    }
    if (removed) {
      if (scenario === "pending") photos.items = [initial[0]!, following];
      await screen.findByText("This photo is no longer in the refreshed result.");
      await waitFor(() => expect(finishLookup).toBeTypeOf("function"));
      finishLookup(initial[1]!);
      await screen.findByRole("dialog", { name: "Photo viewer, Photo 2.jpg" });
      expect(screen.getByText("This photo is no longer in the refreshed result.")).toBeTruthy();
      expect(location.href).toBe(currentURL);
      expect(photos.viewerReturn).toBe(returnRecord);
      expect(
        screen
          .getAllByRole("button", { name: /^(Previous|Next) photo$/ })
          .every((button) => (button as HTMLButtonElement).disabled),
      ).toBe(true);
      if (scenario === "pending") {
        finishPage();
        await new Promise((resolve) => setTimeout(resolve, 0));
        expect(onphotochange).toHaveBeenCalledTimes(1);
        expect(location.href).toBe(currentURL);
        expect(photos.viewerReturn).toBe(returnRecord);
        expect(screen.getByRole("dialog", { name: "Photo viewer, Photo 2.jpg" })).toBeTruthy();
        expect(
          screen
            .getAllByRole("button", { name: /^(Previous|Next) photo$/ })
            .every((button) => (button as HTMLButtonElement).disabled),
        ).toBe(true);
        return;
      }
      await view.rerender({ photos, cache, photoID: "photo-1", onphotochange });
      await waitFor(() =>
        expect(screen.queryByText("This photo is no longer in the refreshed result.")).toBeNull(),
      );
      await view.rerender({ photos, cache, photoID: "photo-2", onphotochange });
      await screen.findByText("This photo is no longer in the refreshed result.");
      photos.items = [...initial, following];
      await waitFor(() =>
        expect(screen.queryByText("This photo is no longer in the refreshed result.")).toBeNull(),
      );
    }
    if (scenario !== "success") {
      await waitFor(() =>
        expect(
          screen
            .getAllByRole("button", { name: "Next photo" })
            .every((button) => !(button as HTMLButtonElement).disabled),
        ).toBe(true),
      );
      await fireEvent.keyDown(screen.getByRole("dialog"), { key: "ArrowRight" });
      if (scenario === "grid") {
        await waitFor(() => expect(onphotochange).toHaveBeenLastCalledWith("photo-2", "step"));
        await fireEvent.keyDown(screen.getByRole("dialog"), { key: "ArrowRight" });
      }
    }
    await waitFor(() => expect(onphotochange).toHaveBeenCalledWith("photo-3", "step"));
    expect(photos.items.map((item) => item.asset_id)).toEqual(["photo-1", "photo-2", "photo-3"]);
    expect(photos.error).toBe("");
    await fireEvent.keyDown(screen.getByRole("dialog"), { key: "ArrowLeft" });
    await waitFor(() => expect(onphotochange).toHaveBeenLastCalledWith("photo-2", "step"));
    await fireEvent.keyDown(screen.getByRole("dialog"), { key: "ArrowLeft" });
    await waitFor(() => expect(onphotochange).toHaveBeenLastCalledWith("photo-1", "step"));
  },
);

it.each(["close", "photo", "unmount", "live"])(
  "keeps deferred continuation scoped to its %s photo visit",
  async (interruption) => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
    const photos = new Photos("scoped", vi.fn());
    photos.items = [photo(1), photo(2)];
    photos.total = 3;
    photos.cursor = "next";
    photos.started = true;
    photos.loading = true;
    vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
    let finish!: () => void;
    const loadMore = vi.spyOn(photos, "loadMore").mockImplementation(() => {
      photos.loading = true;
      return new Promise<void>((resolve) => (finish = resolve));
    });
    const cache = { get: vi.fn() } as unknown as PhotoPreviewCache;
    let view: ReturnType<typeof render>;
    const onphotochange = vi.fn((id: string) => {
      void view.rerender({ photos, cache, photoID: id, onphotochange });
    });
    view = render(PhotosWorkspace, { photos, cache, photoID: "", onphotochange });
    await fireEvent.keyDown(await screen.findByRole("button", { name: "Select Photo 2.jpg" }), {
      key: "Enter",
    });
    await screen.findByRole("dialog", { name: "Photo viewer, Photo 2.jpg" });
    photos.loading = false;
    await fireEvent.keyDown(screen.getByRole("dialog"), { key: "ArrowRight" });
    expect(loadMore).toHaveBeenCalledOnce();
    if (interruption === "close") {
      await fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
      await waitFor(() => expect(photos.viewerReturn).toBeUndefined());
      await fireEvent.keyDown(screen.getByRole("button", { name: "Select Photo 2.jpg" }), {
        key: "Enter",
      });
    } else if (interruption === "photo") {
      await fireEvent.keyDown(screen.getByRole("dialog"), { key: "ArrowLeft" });
      await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
      await fireEvent.keyDown(screen.getByRole("dialog"), { key: "ArrowRight" });
    } else if (interruption === "unmount") {
      view.unmount();
      view = render(PhotosWorkspace, { photos, cache, photoID: "photo-2", onphotochange });
    }
    await screen.findByRole("dialog", { name: "Photo viewer, Photo 2.jpg" });
    const changes = onphotochange.mock.calls.length;
    photos.items = [...photos.items, photo(3)];
    photos.loading = false;
    finish();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(onphotochange).toHaveBeenCalledTimes(changes + (interruption === "live" ? 1 : 0));
    expect(
      screen.getByRole("dialog", {
        name: `Photo viewer, Photo ${interruption === "live" ? 3 : 2}.jpg`,
      }),
    ).toBeTruthy();
  },
);

it.each(["listing", "late", "navigation", "retry", "fresh"])(
  "keeps standalone lookup authoritative across %s",
  async (scenario) => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.stubGlobal(
      "Image",
      class {
        src = "";
        async decode() {}
      },
    );
    vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:synthetic");
    vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
    const old = photo(1);
    old.previews.grid = { state: "ready", generation_id: "old-grid" };
    const fresh = {
      ...photo(1),
      content_version_id: "fresh-version",
      previews: { ...old.previews, grid: { state: "ready" as const, generation_id: "fresh-grid" } },
    };
    const photos = new Photos("scoped", vi.fn());
    photos.started = true;
    photos.loading = true;
    if (["navigation", "retry", "fresh"].includes(scenario)) photos.items = [old];
    photos.cursor = "next";
    photos.total = 999;
    photos.check("photo-1", true, false, ["photo-1"]);
    let finish!: (row: ReturnType<typeof photo>) => void;
    let reject!: (cause: Error) => void;
    const lookup = vi
      .spyOn(photos, "lookup")
      .mockImplementation(() => new Promise((_, fail) => (reject = fail)));
    if (scenario === "fresh")
      lookup.mockImplementation(() => new Promise((resolve) => (finish = resolve)));
    const preview = vi
      .spyOn(photos, "preview")
      .mockResolvedValue(
        scenario === "fresh"
          ? { state: "ready", generation_id: "fresh-fit" }
          : { state: "unsupported" },
      );
    const get = vi.fn(async () => new Blob(["cached-preview"]));
    const props = {
      photos,
      cache: { get } as unknown as PhotoPreviewCache,
      photoID:
        scenario === "navigation" ? "missing" : scenario === "retry" ? "photo-999" : "photo-1",
      onphotochange: vi.fn(),
    };
    const view = render(PhotosWorkspace, props);
    if (scenario === "fresh") {
      await waitFor(() => expect(lookup).toHaveBeenCalledOnce());
      expect(view.container.querySelector(".photo-viewport img")).toBeNull();
      expect(preview).not.toHaveBeenCalled();
      photos.items = [old, photo(2)];
      await tick();
      expect(lookup).toHaveBeenCalledOnce();
      expect(lookup.mock.calls[0]![1].aborted).toBe(false);
      finish(fresh);
      await screen.findByText("Fit preview");
      expect(preview).toHaveBeenCalledWith(
        expect.objectContaining({ content_version_id: "fresh-version" }),
        "fit",
        expect.any(AbortSignal),
      );
      expect(get).toHaveBeenCalledWith(old.asset_id, "fresh-grid", expect.any(AbortSignal), false);
      expect((photos.items[0] as PhotoBrowseRow).content_version_id).toBe("fresh-version");
      photos.items = [old];
      await tick();
      expect(lookup).toHaveBeenCalledOnce();
      expect(preview).toHaveBeenCalledTimes(1);
      return;
    }
    await waitFor(() => expect(reject).toBeTypeOf("function"));
    if (scenario !== "late") {
      reject(new Error("Lookup failed"));
      await screen.findByText("Lookup failed");
    }
    if (scenario === "retry") {
      expect(screen.getByText("No preview")).toBeTruthy();
      lookup
        .mockRejectedValueOnce(new Error("Reload failed"))
        .mockImplementationOnce(() => new Promise((resolve) => (finish = resolve)));
      await fireEvent.click(screen.getByRole("button", { name: "Reload photo" }));
      await screen.findByText("Reload failed");
      await fireEvent.click(screen.getByRole("button", { name: "Reload photo" }));
      await waitFor(() =>
        expect(
          (screen.getByRole("button", { name: "Reload photo" }) as HTMLButtonElement).disabled,
        ).toBe(true),
      );
      expect(document.activeElement).toBe(screen.getByRole("dialog"));
      finish(photo(999));
      await screen.findByRole("dialog", { name: "Photo viewer, Photo 999.jpg" });
      expect(lookup).toHaveBeenCalledTimes(3);
      expect(photos.items.map((item) => item.asset_id)).toEqual(["photo-1"]);
      expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
      expect(photos.cursor).toBe("next");
      return;
    }
    lookup.mockResolvedValueOnce(photo(1));
    if (scenario === "navigation") {
      await view.rerender({ ...props, photoID: "photo-1" });
      await waitFor(() => expect(lookup.mock.calls[0]![1].aborted).toBe(true));
    } else {
      photos.items = [photo(1)];
      await tick();
      expect(lookup.mock.calls[0]![1].aborted).toBe(false);
      expect(lookup).toHaveBeenCalledOnce();
      if (scenario === "late") reject(new Error("Lookup failed"));
      await screen.findByText("Lookup failed");
      expect(screen.queryByRole("dialog", { name: "Photo viewer, Photo 1.jpg" })).toBeNull();
      await fireEvent.click(screen.getByRole("button", { name: "Reload photo" }));
    }
    await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
    await waitFor(() => expect(screen.queryByText("Lookup failed")).toBeNull());
  },
);

function workspaceWithPreparedPhoto(target = preparedPhoto(1), projection?: "map") {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const photos = new Photos("scoped", vi.fn(), false, photoQuery, { projection });
  photos.started = true;
  photos.items = [projection ? mapPhoto(1) : photo(1)];
  photos.authored.targets = [target];
  return photos;
}

it("refreshes a filtered view and restarts pending reads after a metadata edit", async () => {
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
  const old = photo(1);
  const before = preparedPhoto(1, { file_id: "file-1" });
  const photos = workspaceWithPreparedPhoto(before);
  photos.items = [old];
  photos.query = { ...photos.query, filters: { flags: ["pick"] } };
  photos.authored.inspectedID = "photo-1";
  photos.authored.asset = {
    id: "photo-1",
    display_file_id: "file-1",
    files: [{ id: "file-1", role: "image" }],
  } as PhotoAsset;
  const lookup = vi.spyOn(photos, "lookup").mockImplementation(() => new Promise(() => {}));
  const edit = vi.spyOn(photos.authored, "edit").mockResolvedValue(undefined);
  const fetcher = vi
    .fn()
    .mockImplementationOnce(
      (_url: string, init: RequestInit) =>
        new Promise<Response>((_resolve, reject) => {
          init.signal!.addEventListener("abort", () => reject(init.signal!.reason), { once: true });
        }),
    )
    .mockResolvedValueOnce(Response.json({ items: [{ ...old, rotation: 90 }], total: 1 }));
  stubPhotoFetch(fetcher);
  const receipt = {
    receipt_id: "rotated",
    before: [before],
    after: [
      { ...before, revision: 2, values: { ...before.values, flag: "pick", rotation: 90 as const } },
    ],
  } as PhotoAuthoredReceipt;
  render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    photoID: "photo-1",
  });
  await waitFor(() => expect(lookup).toHaveBeenCalledOnce());
  photos.authored.stage({ rating: 0 });
  await fireEvent.click(screen.getByRole("button", { name: "Metadata" }));
  await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  const notify = edit.mock.calls[0]![3];
  await fireEvent.click(screen.getByRole("button", { name: "Refresh previews" }));
  const oldSignal = fetcher.mock.calls[0]![1].signal as AbortSignal;
  const oldLookupSignal = lookup.mock.calls[0]![1];
  await notify(receipt, []);
  await waitFor(() => expect(lookup).toHaveBeenCalledTimes(2));
  expect(oldSignal.aborted).toBe(true);
  expect(oldLookupSignal.aborted).toBe(true);
  expect(JSON.parse(fetcher.mock.calls[1]![1].body as string).query.filters).toEqual({
    flags: ["pick"],
  });
  photos.dispose();
});

it.each(["edit", "Undo"] as const)(
  "reloads an unfiltered Map only after a rotation %s",
  async (action) => {
    Object.defineProperty(Element.prototype, "scrollIntoView", {
      configurable: true,
      value: vi.fn(),
    });
    const before = preparedPhoto(1, { file_id: "file-1" });
    const mapped = workspaceWithPreparedPhoto(before, "map");
    vi.spyOn(mapped, "lookup").mockResolvedValue(photo(1));
    const fetcher = vi.fn(async () =>
      Response.json({ map_items: [{ ...mapPhoto(1), rotation: 90 }], total: 1 }),
    );
    stubPhotoFetch(fetcher);
    const edit = vi.spyOn(mapped.authored, "edit").mockResolvedValue(undefined);
    const undo = vi.spyOn(mapped.authored, "undo").mockResolvedValue(undefined);
    render(PhotosWorkspace, {
      photos: mapped,
      cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
      mapMode: true,
      tiles: {
        tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}",
        host: "tiles.example.org",
        attribution: "Example",
        max_zoom: 19,
      },
    });
    let notify: (receipt: PhotoAuthoredReceipt, tags: api.Tag[]) => Promise<void>;
    if (action === "edit") {
      mapped.authored.stage({ rotation: 90 });
      await fireEvent.click(await screen.findByRole("button", { name: "Metadata" }));
      await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
      notify = edit.mock.calls[0]![3];
    } else {
      mapped.authored.receiptID = "rotated";
      mapped.authored.undoState.summary = "1 file updated: rotation";
      await fireEvent.click(await screen.findByRole("button", { name: "Undo last photo edit" }));
      notify = undo.mock.calls[0]![0];
    }
    const rated = {
      receipt_id: "rated",
      before: [before],
      after: [{ ...before, revision: 2, values: { ...before.values, rating: 3 } }],
    } as PhotoAuthoredReceipt;
    await notify(rated, []);
    expect(fetcher).not.toHaveBeenCalled();
    await notify(
      {
        receipt_id: "rotated",
        before: [before],
        after: [{ ...before, revision: 3, values: { ...before.values, rotation: 90 as const } }],
      } as PhotoAuthoredReceipt,
      [],
    );
    expect(fetcher).toHaveBeenCalled();
    expect((mapped.items[0] as PhotoMapRow).rotation).toBe(90);
    mapped.dispose();
  },
);

it("restarts a pending viewer lookup when Undo changes a rotation before the display file is known", async () => {
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
  const before = preparedPhoto(1, {
    file_id: "file-1",
    values: { ...preparedPhoto(1).values, rotation: 90 },
  });
  const photos = workspaceWithPreparedPhoto(before);
  photos.items = [];
  photos.authored.receiptID = "rotated";
  photos.authored.undoState.summary = "1 file updated: rotation";
  const lookup = vi.spyOn(photos, "lookup").mockImplementation(() => new Promise(() => {}));
  const undo = vi.spyOn(photos.authored, "undo").mockResolvedValue(undefined);
  stubPhotoFetch(vi.fn());
  render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    photoID: "photo-1",
  });
  await waitFor(() => expect(lookup).toHaveBeenCalledOnce());
  await fireEvent.click(await screen.findByRole("button", { name: "Undo last photo edit" }));
  const stale = lookup.mock.calls[0]![1];
  await undo.mock.calls[0]![0](
    {
      receipt_id: "undone",
      before: [before],
      after: [{ ...before, revision: 3, values: { ...before.values, rotation: 0 } }],
    } as PhotoAuthoredReceipt,
    [],
  );
  await waitFor(() => expect(lookup).toHaveBeenCalledTimes(2));
  expect(stale.aborted).toBe(true);
  photos.dispose();
});

it("patches an unfiltered view in place after a metadata edit", async () => {
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
  const before = preparedPhoto(1, { file_id: "file-1" });
  const photos = workspaceWithPreparedPhoto(before);
  photos.items = [{ ...photo(1), display_file_id: "file-1" }];
  const edit = vi.spyOn(photos.authored, "edit").mockResolvedValue(undefined);
  const fetcher = vi.fn();
  stubPhotoFetch(fetcher);
  const receipt = {
    receipt_id: "rated",
    before: [before],
    after: [
      { ...before, revision: 2, values: { ...before.values, rating: 3, rotation: 90 as const } },
    ],
  } as PhotoAuthoredReceipt;
  render(PhotosWorkspace, { photos, cache: { get: vi.fn() } as unknown as PhotoPreviewCache });
  photos.authored.stage({ rating: 3 });
  await fireEvent.click(screen.getByRole("button", { name: "Metadata" }));
  await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  await edit.mock.calls[0]![3](receipt, []);
  expect(fetcher).not.toHaveBeenCalled();
  expect(photos.loading).toBe(false);
  expect(photos.authored.concealed).toBe(false);
  expect((photos.items[0] as PhotoBrowseRow).rotation).toBe(90);
  photos.dispose();
});

it.each([
  ["preview", "changed"],
  ["original", "changed"],
  ["preview", "same"],
  ["original", "same"],
] as const)(
  "reloads a %s failure with a %s version and keeps the viewer context",
  async (operation, version) => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.stubGlobal(
      "Image",
      class {
        src = "";
        async decode() {}
      },
    );
    vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:synthetic");
    vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
    const old = photo(1),
      fresh = {
        ...photo(1),
        content_version_id: version === "changed" ? "fresh-version" : old.content_version_id,
      };
    const photos = new Photos("scoped", vi.fn());
    photos.items = [old, photo(2)];
    photos.started = true;
    photos.cursor = "next";
    photos.check("photo-2", true, false, ["photo-1", "photo-2"]);
    const record = {
      contextID: photos.contextID,
      assetID: old.asset_id,
      control: "open" as const,
      scrollTop: 400,
    };
    photos.viewerReturn = record;
    let finish!: (response: Response) => void;
    const fetcher = vi
      .fn()
      .mockImplementation(() => new Promise<Response>((resolve) => (finish = resolve)));
    if (version === "same") fetcher.mockRejectedValueOnce(new Error("Reload failed"));
    stubPhotoFetch(fetcher);
    const changed = new APIError("Photo changed", 409, "photo_display_changed");
    const preview = vi
      .spyOn(photos, "preview")
      .mockResolvedValue({ state: "ready", generation_id: "fresh-fit" });
    if (operation === "preview") preview.mockRejectedValueOnce(changed);
    else vi.spyOn(photos, "original").mockRejectedValueOnce(changed);
    render(PhotosWorkspace, {
      photos,
      cache: { get: vi.fn(async () => new Blob(["synthetic"])) } as unknown as PhotoPreviewCache,
      photoID: old.asset_id,
      onphotochange: vi.fn(),
    });
    if (operation === "original") {
      await screen.findByText("Fit preview");
      await fireEvent.click(screen.getByRole("button", { name: "View original" }));
    }
    await screen.findByText("Photo changed");
    expect(screen.queryByRole("button", { name: "Retry preview" })).toBeNull();
    const preparations = preview.mock.calls.length;
    await fireEvent.click(screen.getByRole("button", { name: "Reload photo" }));
    if (version === "same") {
      await screen.findByText("Reload failed");
      expect(preview).toHaveBeenCalledTimes(preparations);
      await fireEvent.click(screen.getByRole("button", { name: "Reload photo" }));
    }
    await waitFor(() => expect(finish).toBeTypeOf("function"));
    expect(preview).toHaveBeenCalledTimes(preparations);
    finish(new Response(JSON.stringify({ items: [fresh], total: 2 })));
    await waitFor(() =>
      expect((photos.items[0] as PhotoBrowseRow).content_version_id).toBe(fresh.content_version_id),
    );
    await screen.findByText("Fit preview");
    await waitFor(() => expect(preview).toHaveBeenCalledTimes(preparations + 1));
    expect(photos.items.map((item) => item.asset_id)).toEqual(["photo-1", "photo-2"]);
    expect(photos.cursor).toBe("next");
    expect([...photos.selection.selectedIDs]).toEqual(["photo-2"]);
    expect(photos.viewerReturn).toBe(record);
    expect(screen.queryByText("Photo changed")).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole("dialog"));
  },
);

it.each(["success", "failure", "interrupted", "late"] as const)(
  "consumes a %s reload before revisiting the listed photo",
  async (outcome) => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    const photos = new Photos("scoped", vi.fn());
    photos.items = [photo(1), photo(2)];
    photos.started = true;
    photos.viewerReturn = {
      contextID: photos.contextID,
      assetID: "photo-1",
      control: "open",
      scrollTop: 0,
    };
    let finish!: (row: ReturnType<typeof photo>) => void;
    const lookup = vi.spyOn(photos, "lookup");
    if (outcome === "success") lookup.mockResolvedValue(photo(1));
    else if (outcome === "failure") lookup.mockRejectedValue(new Error("Reload failed"));
    else if (outcome === "late")
      lookup.mockImplementation(() => new Promise((resolve) => (finish = resolve)));
    else lookup.mockImplementation(() => new Promise(() => {}));
    vi.spyOn(photos, "preview")
      .mockRejectedValueOnce(new APIError("Changed", 409, "photo_display_changed"))
      .mockResolvedValue({ state: "unsupported" });
    const props = {
      photos,
      cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
      photoID: "photo-1",
      onphotochange: vi.fn(),
    };
    const view = render(PhotosWorkspace, props);
    await fireEvent.click(await screen.findByRole("button", { name: "Reload photo" }));
    await waitFor(() => expect(lookup).toHaveBeenCalledOnce());
    if (outcome === "failure") await screen.findByText("Reload failed");
    if (outcome === "success")
      await waitFor(() => expect(screen.queryByText("Changed")).toBeNull());
    await view.rerender({ ...props, photoID: "photo-2" });
    await waitFor(() => expect(lookup.mock.calls[0]![1].aborted).toBe(true));
    if (outcome === "late") {
      const version = (photos.items[0] as PhotoBrowseRow).content_version_id;
      finish({ ...photo(1), content_version_id: "late-version" });
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect((photos.items[0] as PhotoBrowseRow).content_version_id).toBe(version);
      expect(screen.getByRole("dialog", { name: "Photo viewer, Photo 2.jpg" })).toBeTruthy();
    }
    await view.rerender(props);
    await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
    expect(lookup).toHaveBeenCalledOnce();
  },
);

it("switches Grid and Timeline, seeks an empty day and clears its date", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const photos = new Photos("scoped", vi.fn());
  photos.started = true;
  photos.items = [photo(1)];
  photos.total = 1;
  photos.timeline = {
    dimension: "capture_day",
    available: true,
    total: 1,
    missing: 0,
    other: 0,
    values: [{ key: "2024-02-29", label: "2024-02-29", count: 1, selected: false }],
  };
  let finish!: (response: Response) => void;
  const fetcher = vi
    .fn()
    .mockImplementationOnce(() => new Promise((resolve) => (finish = resolve)))
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
  await fireEvent.click(
    screen.getByRole("navigation", { name: "Timeline years" }).querySelector("button")!,
  );
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
  view.unmount();
  photos.dispose();
  await cache.dispose();
});

it("explains the relevance limit and hides it for complete results or capture dates", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const photos = new Photos("scoped", vi.fn());
  photos.started = true;
  photos.items = [photo(1)];
  photos.total = 5000;
  photos.query = {
    ...photos.query,
    text: "Canon",
    sort: { field: "relevance", direction: "desc" },
  };
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache });
  const note =
    "Showing the best 250 of 5,000 matches. Refine the search or sort by capture date to see all.";
  await screen.findByText(note);
  photos.total = 1;
  await waitFor(() => expect(screen.queryByText(note)).toBeNull());
  photos.total = 5000;
  photos.query = { ...photos.query, sort: { field: "capture_time", direction: "desc" } };
  await waitFor(() => expect(screen.queryByText(note)).toBeNull());
  photos.dispose();
  await cache.dispose();
});

it("applies the selected Relevance sort", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  Element.prototype.scrollIntoView = vi.fn();
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      Response.json({ items: [], total: 0, facets: [], relevance_available: false }),
    ),
  );
  const photos = new Photos("scoped", vi.fn());
  const setSort = vi.spyOn(photos, "setSort");
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache });
  await screen.findByText("Your photo library is empty");
  await fireEvent.click(screen.getByRole("combobox", { name: "Sort photos: Capture date" }));
  expect(screen.queryByRole("option", { name: "Relevance" })).toBeNull();
  await fireEvent.keyDown(screen.getByRole("listbox"), { key: "Escape" });
  photos.relevanceAvailable = true;
  await fireEvent.click(screen.getByRole("combobox", { name: "Sort photos: Capture date" }));
  await fireEvent.click(await screen.findByRole("option", { name: "Relevance" }));
  expect(setSort).toHaveBeenCalledWith({ field: "relevance", direction: "desc" });
  photos.dispose();
  await cache.dispose();
});

it.each([
  ["4", { rating: 4 }],
  ["6", { label: "red" }],
  ["7", { label: "yellow" }],
  ["8", { label: "green" }],
  ["9", { label: "blue" }],
  ["v", { label: "purple" }],
])("applies the registered %s photo shortcut", async (key, patch) => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1)];
  photos.started = true;
  photos.selectLoaded();
  const edit = vi.spyOn(photos.authored, "edit").mockResolvedValue();
  const shortcuts = createShortcutManager();
  render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    shortcuts,
  });
  shortcuts.handleKeydown(new KeyboardEvent("keydown", { key, cancelable: true }));
  expect(edit).toHaveBeenCalledWith(
    { asset_ids: ["photo-1"] },
    patch,
    false,
    expect.any(Function),
    { keywords: undefined, rotationDelta: undefined },
  );
  photos.dispose();
});

it.each([
  ["no target", undefined],
  ["loading", "loading"],
  ["hiding", "hiding"],
  ["trashing", "trashing"],
  ["busy", "busy"],
] as const)("gates metadata shortcuts for %s", async (_scenario, pending) => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const photos = new Photos("scoped", vi.fn());
  photos.started = true;
  photos.items = [photo(1)];
  if (pending) {
    photos.selectLoaded();
    photos.authored.targets = [preparedPhoto(1)];
    photos.authored.receiptID = "last-edit";
    photos.authored.stage({ caption: "Draft" });
  }
  const edit = vi.spyOn(photos.authored, "edit").mockResolvedValue(undefined),
    undo = vi.spyOn(photos.authored, "undo").mockResolvedValue(undefined);
  const shortcuts = createShortcutManager();
  render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    shortcuts,
  });
  if (pending) {
    if (pending === "busy") photos.authored.busy = true;
    else photos[pending] = true;
    await tick();
  }
  for (const key of ["4", "z"]) {
    const event = new KeyboardEvent("keydown", {
      key,
      ctrlKey: key === "z",
      bubbles: true,
      cancelable: true,
    });
    shortcuts.handleKeydown(event);
    expect(event.defaultPrevented).toBe(true);
  }
  expect(edit).not.toHaveBeenCalled();
  expect(undo).not.toHaveBeenCalled();
  photos.dispose();
});

it("guards loupe navigation through the metadata editor", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const photos = new Photos("scoped", vi.fn());
  photos.started = true;
  photos.items = [photo(1), photo(2)];
  vi.spyOn(photos, "lookup").mockImplementation(async (id) =>
    photo(Number(id.slice("photo-".length))),
  );
  const onphotochange = vi.fn();
  const canChangeTarget = vi.spyOn(photos.authored, "canChangeTarget").mockReturnValue(false);
  photos.viewerReturn = {
    contextID: photos.contextID,
    assetID: "photo-1",
    control: "open",
    scrollTop: 0,
  };
  const view = render(PhotosWorkspace, {
    photos,
    onphotochange,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    shortcuts: createShortcutManager(),
    photoID: "photo-1",
  });
  const viewer = await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
  await fireEvent.keyDown(viewer, { key: "ArrowRight" });
  expect(onphotochange).not.toHaveBeenCalled();
  canChangeTarget.mockReturnValue(true);
  await fireEvent.keyDown(viewer, { key: "ArrowRight" });
  expect(onphotochange).toHaveBeenCalledWith("photo-2", "step");
  view.unmount();
  photos.dispose();
});

it("keeps a Library reload error through a list refresh", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1)];
  photos.started = true;
  photos.viewerReturn = {
    contextID: photos.contextID,
    assetID: "photo-1",
    control: "open",
    scrollTop: 0,
  };
  const lookup = vi.spyOn(photos, "lookup").mockRejectedValue(new Error("Reload failed"));
  vi.spyOn(photos, "preview").mockRejectedValue(
    new APIError("Photo changed", 409, "photo_display_changed"),
  );
  render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    photoID: "photo-1",
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Reload photo" }));
  await screen.findByText("Reload failed");
  photos.items = [{ ...photo(1), revision: 2 }];
  await tick();
  expect(screen.getByText("Reload failed")).toBeTruthy();
  expect(lookup).toHaveBeenCalledOnce();
  photos.dispose();
});

it("keeps Library recovery below its toolbar and inside the grid", async () => {
  const photos = new Photos("scoped", vi.fn());
  photos.started = true;
  photos.error = "Page unavailable";
  const retry = vi.spyOn(photos, "retry").mockResolvedValue();
  render(PhotosWorkspace, { photos, cache: { get: vi.fn() } as unknown as PhotoPreviewCache });
  expect(screen.getByRole("button", { name: "Move rejects" })).toBeTruthy();
  const alert = screen.getByRole("alert");
  expect(alert.closest(".library-grid")).toBeTruthy();
  expect(
    document.querySelector(".photo-toolbar")!.compareDocumentPosition(alert) &
      Node.DOCUMENT_POSITION_FOLLOWING,
  ).toBeTruthy();
  await fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
  expect(retry).toHaveBeenCalledOnce();
  photos.dispose();
});

it("renders disabled maps without fetching and keeps the Library selection on Escape", async () => {
  const fetcher = vi.fn();
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1)];
  photos.selectLoaded();
  render(PhotosWorkspace, {
    tiles,
    photos,
    mapMode: true,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    photoID: "",
    onphotochange: vi.fn(),
  });
  await screen.findByText("Maps are disabled");
  await fireEvent.keyDown(window, { key: "Escape" });
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  expect(fetcher).not.toHaveBeenCalled();
  photos.dispose();
});

it("shows the Map limit notice only for capped results and bounds loading progress", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const photos = new Photos("scoped", vi.fn(), false, photoQuery, { projection: "map" });
  photos.items = [mapPhoto(1), mapPhoto(2)];
  photos.started = true;
  photos.total = 3000;
  photos.cursor = "next";
  photos.loading = true;
  vi.spyOn(photos, "resume").mockResolvedValue();
  render(PhotosWorkspace, {
    photos,
    mapMode: true,
    tiles: {
      tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}",
      host: "tiles.example.org",
      attribution: "Example",
      max_zoom: 19,
    },
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
  });
  const note = `Showing the newest ${mapPhotoLimit.toLocaleString()} of ${(mapPhotoLimit + 1000).toLocaleString()} located photos. Narrow your search or date to see the rest.`;
  expect(screen.queryByText(/^Showing the newest /)).toBeNull();
  photos.total = mapPhotoLimit + 1000;
  await screen.findByText(`Loading located photos: 2 of ${mapPhotoLimit.toLocaleString()}`);
  photos.items = Array.from({ length: mapPhotoLimit }, (_, index) => mapPhoto(index + 1));
  photos.loading = false;
  const notice = await screen.findByText(note);
  expect(notice.getAttribute("role")).toBe("status");
  expect(notice.classList.contains("photo-search-limit")).toBe(true);
  photos.cursor = undefined;
  await tick();
  expect(screen.getByText(note)).toBe(notice);
  photos.total = photos.items.length;
  await waitFor(() => expect(screen.queryByText(/^Showing the newest /)).toBeNull());
  photos.items = photos.items.slice(0, -1);
  photos.total = mapPhotoLimit + 1000;
  photos.cursor = "next";
  await waitFor(() => expect(screen.queryByText(/^Showing the newest /)).toBeNull());
  photos.cursor = undefined;
  await tick();
  expect(screen.queryByText(/^Showing the newest /)).toBeNull();
  photos.dispose();
});

it("reopens the map where it was after a remount", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const photos = new Photos("scoped", vi.fn());
  photos.items = [mapPhoto(1)];
  photos.total = 1;
  photos.started = true;
  vi.spyOn(photos, "resume").mockResolvedValue();
  const props = {
    photos,
    mapMode: true,
    tiles: {
      tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}",
      host: "tiles.example.org",
      attribution: "Example",
      max_zoom: 19,
    },
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    photoID: "",
    onphotochange: vi.fn(),
  };
  const first = render(PhotosWorkspace, props);
  await vi.dynamicImportSettled();
  expect(map.props!.view).toBeUndefined();
  map.props!.onview({ lat: 48.8, lng: 2.3, zoom: 12 });
  first.unmount();
  render(PhotosWorkspace, props);
  await vi.dynamicImportSettled();
  expect(map.props!.view).toEqual({ lat: 48.8, lng: 2.3, zoom: 12 });
  photos.dispose();
});

function openMapViewerSetup() {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const photos = new Photos("scoped", vi.fn(), false, photoQuery, { projection: "map" });
  photos.items = [mapPhoto(1), mapPhoto(2)];
  photos.total = 2;
  photos.started = true;
  vi.spyOn(photos, "resume").mockResolvedValue();
  const lookup = vi.spyOn(photos, "lookup").mockResolvedValue(photo(1));
  const preview = vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  const props = {
    photos,
    mapMode: true,
    tiles: {
      tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}",
      host: "tiles.example.org",
      attribution: "Example",
      max_zoom: 19,
    },
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    photoID: "",
    onphotochange: vi.fn(),
  };
  return { photos, lookup, preview, props, view: render(PhotosWorkspace, props) };
}
async function openMapViewer({ props, view }: ReturnType<typeof openMapViewerSetup>) {
  await vi.dynamicImportSettled();
  map.onvisible!([mapPhoto(1)]);
  await fireEvent.click(await screen.findByRole("button", { name: "Open Photo 1.jpg" }));
  await view.rerender({ ...props, photoID: "photo-1" });
  await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
}
it("disables viewer continuation at the Map ceiling while retaining its cursor", async () => {
  const setup = openMapViewerSetup();
  const { photos, lookup, props, view } = setup;
  photos.items = Array.from({ length: mapPhotoLimit }, (_, index) => mapPhoto(index + 1));
  photos.total = mapPhotoLimit + 1000;
  photos.cursor = "remaining";
  lookup.mockResolvedValue(photo(mapPhotoLimit));
  const loadMore = vi.spyOn(photos, "loadMore");
  await vi.dynamicImportSettled();
  map.onvisible!([mapPhoto(mapPhotoLimit)]);
  await fireEvent.click(
    await screen.findByRole("button", { name: `Open Photo ${mapPhotoLimit}.jpg` }),
  );
  await view.rerender({ ...props, photoID: `photo-${mapPhotoLimit}` });
  await screen.findByRole("dialog", { name: `Photo viewer, Photo ${mapPhotoLimit}.jpg` });
  expect(
    screen
      .getAllByRole("button", { name: "Next photo" })
      .every((button) => (button as HTMLButtonElement).disabled),
  ).toBe(true);
  await fireEvent.keyDown(window, { key: "ArrowRight" });
  expect(loadMore).not.toHaveBeenCalled();
  expect(props.onphotochange).toHaveBeenCalledOnce();
  expect(photos.cursor).toBe("remaining");
  photos.items = photos.items.slice(1);
  await tick();
  expect(
    screen
      .getAllByRole("button", { name: "Next photo" })
      .every((button) => (button as HTMLButtonElement).disabled),
  ).toBe(false);
  photos.dispose();
});
it("revalidates the open Map photo after a reload and a list refresh", async () => {
  const setup = openMapViewerSetup();
  const { photos, lookup, preview } = setup;
  stubPhotoFetch(
    vi.fn(async () =>
      Response.json({ items: [], map_items: [{ ...mapPhoto(1), revision: 2 }], total: 1 }),
    ),
  );
  preview.mockImplementation(async (row) => {
    if (row.revision === 1) throw new APIError("Photo changed", 409, "photo_display_changed");
    return { state: "unsupported" };
  });
  await openMapViewer(setup);
  await screen.findByText("Photo changed");
  lookup.mockResolvedValue({ ...photo(1), revision: 2 });
  await fireEvent.click(screen.getByRole("button", { name: "Reload photo" }));
  await waitFor(() =>
    expect(preview).toHaveBeenCalledWith(
      expect.objectContaining({ revision: 2 }),
      "fit",
      expect.any(AbortSignal),
    ),
  );
  expect(lookup).toHaveBeenCalledTimes(2);
  photos.items = [...photos.items, mapPhoto(3)];
  await tick();
  expect(lookup).toHaveBeenCalledTimes(2);
  let finish!: (row: ReturnType<typeof photo>) => void;
  lookup.mockImplementation(() => new Promise((resolve) => (finish = resolve)));
  await photos.refresh();
  await waitFor(() => expect(finish).toBeTypeOf("function"));
  expect(screen.getByRole("dialog", { name: "Photo viewer, Photo 1.jpg" })).toBeTruthy();
  finish({
    ...photo(1),
    revision: 3,
    name: "Refreshed photo.jpg",
    content_version_id: "refreshed-version",
  });
  await screen.findByRole("dialog", { name: "Photo viewer, Refreshed photo.jpg" });
  await waitFor(() =>
    expect(preview).toHaveBeenCalledWith(
      expect.objectContaining({ content_version_id: "refreshed-version" }),
      "fit",
      expect.any(AbortSignal),
    ),
  );
  photos.dispose();
});

it("keeps an open Map photo's detail request through appended pages", async () => {
  const setup = openMapViewerSetup();
  const { photos, lookup } = setup;
  let finish!: (row: ReturnType<typeof photo>) => void;
  lookup.mockImplementation(() => new Promise((resolve) => (finish = resolve)));
  await vi.dynamicImportSettled();
  map.onvisible!([photos.items[0] as PhotoMapRow]);
  await fireEvent.click(await screen.findByRole("button", { name: "Open Photo 1.jpg" }));
  await setup.view.rerender({ ...setup.props, photoID: "photo-1" });
  await waitFor(() => expect(lookup).toHaveBeenCalledOnce());
  const signal = lookup.mock.calls[0]![1];
  photos.items = [...photos.items, mapPhoto(3)];
  await tick();
  expect(lookup).toHaveBeenCalledOnce();
  expect(signal.aborted).toBe(false);
  finish(photo(1));
  await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
  photos.items = [...photos.items, mapPhoto(4)];
  await tick();
  expect(lookup).toHaveBeenCalledOnce();
  photos.dispose();
});

it("viewer follows the Map query while the viewport filters its grid", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [
    { ...mapPhoto(1), longitude: 120 },
    { ...mapPhoto(2), latitude: -85 },
  ];
  photos.total = 2;
  photos.started = true;
  vi.spyOn(photos, "resume").mockResolvedValue();
  vi.spyOn(photos, "lookup").mockResolvedValue(photo(1));
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  const onphotochange = vi.fn();
  const props = {
    photos,
    mapMode: true,
    tiles: {
      tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}",
      host: "tiles.example.org",
      attribution: "Example",
      max_zoom: 19,
    },
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    photoID: "",
    onphotochange,
  };
  const view = render(PhotosWorkspace, props);
  await vi.dynamicImportSettled();
  map.onvisible!([{ ...mapPhoto(1), longitude: 120 }]);
  await fireEvent.click(await screen.findByRole("button", { name: "Open Photo 1.jpg" }));
  expect(screen.queryByRole("button", { name: "Open Photo 2.jpg" })).toBeNull();
  await view.rerender({ ...props, photoID: "photo-1" });
  await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
  expect(screen.getByText("1 of 2")).toBeTruthy();
  await fireEvent.click(screen.getAllByRole("button", { name: "Next photo" })[0]!);
  expect(onphotochange).toHaveBeenLastCalledWith("photo-2", "step");
  map.onvisible!([]);
  await waitFor(() =>
    expect(document.querySelector('.photo-layout [data-asset="photo-1"]')).toBeNull(),
  );
  expect(screen.getByRole("dialog", { name: "Photo viewer, Photo 1.jpg" })).toBeTruthy();
  photos.dispose();
});

function mapRecoverySetup() {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(400);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [{ ...photo(1), latitude: 0, longitude: 120 }];
  photos.started = true;
  photos.error = "Page unavailable";
  photos.selectLoaded();
  const retry = vi.spyOn(photos, "retry").mockResolvedValue();
  render(PhotosWorkspace, {
    photos,
    mapMode: true,
    tiles: {
      tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}",
      host: "tiles.example.org",
      attribution: "Example",
      max_zoom: 19,
    },
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    photoID: "",
    onphotochange: vi.fn(),
  });
  return { photos, retry };
}
it("keeps Map recovery outside mobile tabs", async () => {
  const { photos, retry } = mapRecoverySetup();
  await vi.dynamicImportSettled();
  map.onvisible!([]);
  const alert = screen.getByRole("alert");
  expect(alert.closest(".photo-layout")).toBeNull();
  await fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
  expect(retry).toHaveBeenCalledOnce();
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  expect(screen.getByRole("alert")).toBe(alert);
  photos.dispose();
});
it("discloses offscreen selection and replaces it with the visible photos", async () => {
  const { photos } = mapRecoverySetup();
  photos.items = [mapPhoto(1), mapPhoto(2)];
  photos.selectLoaded();
  await vi.dynamicImportSettled();
  map.onvisible!([photos.items[0] as PhotoMapRow]);
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  photos.loading = true;
  await tick();
  await screen.findByText("1 outside this view");
  await fireEvent.click(screen.getByRole("button", { name: "Move to trash" }));
  const dialog = screen.getByRole("dialog", { name: "Move selected photos to trash" });
  expect(within(dialog).getByText("1 selected photo is outside this view.")).toBeTruthy();
  await fireEvent.click(within(dialog).getByRole("button", { name: "Keep in Docbank" }));
  const select = screen.getByRole("button", { name: "Select visible photos" });
  await fireEvent.click(select);
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  photos.dispose();
});
it("distinguishes an empty Map area from no located photos", async () => {
  const { photos } = mapRecoverySetup();
  await vi.dynamicImportSettled();
  map.onvisible!([]);
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  photos.error = "";
  await screen.findByText("No photos in this area");
  photos.items = [];
  await screen.findByText("No located photos");
  photos.dispose();
});
it("shows the Map date without Library controls", async () => {
  const { photos } = mapRecoverySetup();
  photos.date = "2024";
  photos.total = 1;
  await vi.dynamicImportSettled();
  expect(screen.queryByRole("button", { name: "Move rejects" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Timeline" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Clear date" })).toBeNull();
  await screen.findByText("1 located photo in 2024");
  photos.dispose();
});

it.each(["library", "map", "hidden", "album"])(
  "offers Add photos only in Library, scope=%s",
  async (scope) => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(Response.json({ items: [], total: 0, facets: [] })),
    );
    const photos = new Photos("scoped", vi.fn(), scope === "hidden");
    photos.started = true;
    const cache = new PhotoPreviewCache("scoped", vi.fn());
    const add = vi.fn();
    render(PhotosWorkspace, {
      photos,
      cache,
      mapMode: scope === "map",
      tiles: {
        tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}",
        host: "tiles.example.org",
        attribution: "Example",
        max_zoom: 19,
      },
      albumID: scope === "album" ? "album-1" : "",
      onadd: add,
    });
    if (scope === "library") {
      const buttons = screen.getAllByRole("button", { name: "Add photos" });
      expect(buttons).toHaveLength(2);
      await fireEvent.click(buttons[0]);
      await fireEvent.click(buttons[1]);
      expect(add).toHaveBeenCalledTimes(2);
    } else {
      if (scope === "map") await screen.findByText("Photos in view");
      expect(screen.queryByRole("button", { name: "Add photos" })).toBeNull();
    }
    photos.dispose();
    await cache.dispose();
  },
);

it("explains why Add photos is unavailable", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(Response.json({ items: [], total: 0, facets: [] })),
  );
  const photos = new Photos("scoped", vi.fn());
  photos.started = true;
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache, uploadChannelError: "The upload channel ended." });
  for (const button of screen.getAllByRole("button", { name: "Add photos" })) {
    expect((button as HTMLButtonElement).disabled).toBe(true);
    expect(button.title).toBe("The upload channel ended.");
  }
  photos.dispose();
  await cache.dispose();
});

it("explains the photo export cap before offering scope or selected export", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const photos = new Photos("scoped", vi.fn());
  photos.items = Array.from({ length: 17 }, (_, i) => photo(i + 1));
  photos.total = 17;
  photos.started = true;
  photos.selectLoaded();
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(Response.json({ items: photos.items, total: 17 })),
  );
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  const onexport = vi.fn();
  render(PhotosWorkspace, { photos, cache, onexport });
  expect(
    (screen.getByRole("button", { name: "Export photos" }) as HTMLButtonElement).disabled,
  ).toBe(true);
  expect(
    (screen.getByRole("button", { name: "Export selection" }) as HTMLButtonElement).disabled,
  ).toBe(true);
  expect(screen.getByText(/Clear the selection to choose fewer photos/)).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Clear selection" }));
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: "Export selection" }));
  expect(onexport).toHaveBeenCalledWith(expect.objectContaining({ total: 1 }));
  photos.dispose();
  await cache.dispose();
});
it("exports the visible scope, including album filters and all-results selections", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const albumQuery = {
    ...photoQuery,
    filters: { set_ids: ["11111111-1111-4111-8111-111111111111"] },
  };
  const photos = new Photos("scoped", vi.fn(), false, albumQuery);
  photos.items = [photo(1), photo(2)];
  photos.total = 3;
  photos.started = true;
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(Response.json({ items: photos.items, total: 3 })),
  );
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  const onexport = vi.fn();
  render(PhotosWorkspace, { photos, cache, onexport });
  await fireEvent.click(screen.getByRole("button", { name: "Export photos" }));
  expect(onexport).toHaveBeenCalledExactlyOnceWith(
    expect.objectContaining({
      label: "Library",
      total: 3,
      photos: { query: albumQuery, asset_ids: [], hidden: false },
    }),
  );
  onexport.mockClear();
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  photos.allResults = true;
  await tick();
  await fireEvent.click(screen.getByRole("button", { name: "Export selection" }));
  expect(onexport).toHaveBeenCalledExactlyOnceWith(
    expect.objectContaining({
      label: "Selected photos",
      total: 3,
      photos: { query: albumQuery, asset_ids: [], hidden: false },
    }),
  );
  photos.dispose();
  await cache.dispose();
});

it("exports explicit Map selections without offering the broader Library query", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const photos = new Photos("scoped", vi.fn(), false, photoQuery, { projection: "map" });
  photos.items = [mapPhoto(1), mapPhoto(2)];
  photos.total = 2;
  photos.started = true;
  photos.selectLoaded();
  vi.spyOn(photos, "resume").mockResolvedValue();
  const onexport = vi.fn();
  render(PhotosWorkspace, {
    photos,
    mapMode: true,
    tiles: {
      tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}",
      host: "tiles.example.org",
      attribution: "Example",
      max_zoom: 19,
    },
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    onexport,
  });
  await vi.dynamicImportSettled();
  map.onvisible!([mapPhoto(1)]);
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  expect(screen.queryByRole("button", { name: "Export photos" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Export selection" }));
  expect(onexport).toHaveBeenCalledExactlyOnceWith({
    label: "Selected photos",
    total: 2,
    photos: { query: photoQuery, asset_ids: ["photo-1", "photo-2"], hidden: false },
  });
  photos.dispose();
});

it("exports the prepared metadata selection after the query grows beyond the export cap", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1)];
  photos.total = 3;
  photos.started = true;
  photos.selectAllResults();
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      expect(url).toBe("/api/v1/photos/authored/prepare");
      expect(JSON.parse(String(init.body))).toEqual({ query: photoQuery });
      return Response.json({
        assets: [],
        targets: [preparedPhoto(1), preparedPhoto(2), preparedPhoto(3)],
      });
    }),
  );
  await photos.authored.prepare(photos.scope());
  photos.total = 17;
  const onexport = vi.fn();
  render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    onexport,
  });
  expect(screen.getByText("All 3 photos selected")).toBeTruthy();
  const button = screen.getByRole("button", { name: "Export selection" }) as HTMLButtonElement;
  expect(button.disabled).toBe(false);
  await fireEvent.click(button);
  expect(onexport).toHaveBeenCalledExactlyOnceWith({
    label: "Selected photos",
    total: 3,
    photos: { query: photoQuery, asset_ids: ["photo-1", "photo-2", "photo-3"], hidden: false },
  });
  photos.dispose();
});

it("disables selected export when a metadata draft keeps an empty selection open", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1)];
  photos.total = 1;
  photos.started = true;
  photos.authored.stage({ caption: "Retained draft" });
  const onexport = vi.fn();
  render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    onexport,
  });
  const button = screen.getByRole("button", { name: "Export selection" }) as HTMLButtonElement;
  expect(button.disabled).toBe(true);
  button.click();
  expect(onexport).not.toHaveBeenCalled();
  photos.dispose();
});

it.each([false, true])(
  "reconciles grid selection after cull (one unavailable: %s)",
  async (oneUnavailable) => {
    vi.spyOn(api, "getPhotoSettings").mockResolvedValue({
      revision: 1,
      preference: null,
      burst_gap_seconds: 2,
    });
    vi.spyOn(api, "groupPhotoStacks").mockResolvedValue({
      groups: [],
      unavailable_ids: [],
      burst_gap_seconds: 2,
    });
    vi.stubGlobal("indexedDB", new IDBFactory());
    vi.stubGlobal("IDBKeyRange", IDBKeyRange);
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(PhotoCull.prototype, "restore").mockResolvedValue(false);
    vi.spyOn(PhotoCull.prototype, "flush").mockResolvedValue();
    const rows = [photo(1), photo(2)];
    const photos = new Photos("scoped", vi.fn());
    photos.items = rows;
    photos.total = rows.length;
    photos.started = true;
    const ids = rows.map((row) => row.asset_id);
    photos.select(ids[0]!, new MouseEvent("click"), ids);
    photos.select(ids[1]!, new MouseEvent("click", { ctrlKey: true }), ids);
    vi.spyOn(PhotoCull.prototype, "start").mockImplementation(async function (this: PhotoCull) {
      this.run = {
        ids,
        checkpoint: {
          runID: "run",
          revision: 0,
          pass: ids,
          position: 0,
          visited: [ids[0]!],
          skipped: [],
          unavailable: [],
          scope: "unflagged",
          autoAdvance: true,
        },
      };
      await photoCullStorage("vault").replace(this.run);
      return true;
    });
    vi.spyOn(photos, "lookup").mockImplementation(
      async (id) => rows.find((row) => row.asset_id === id)!,
    );
    vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
    const before = preparedPhoto(1);
    const receipt = {
      receipt_id: "picked",
      before: [before],
      after: [{ ...before, revision: 2, values: { ...before.values, flag: "pick" } }],
    } as PhotoAuthoredReceipt;
    stubPhotoFetch(async (url, init) => {
      if (url.endsWith("/photos/authored/edit")) return Response.json(receipt);
      if (url.endsWith("/photos/assets/query")) {
        const requested = JSON.parse(String(init.body)).query.filters?.asset_ids;
        const eligible = oneUnavailable ? [rows[1]!] : rows;
        return Response.json({
          items: requested ? eligible.filter((row) => requested.includes(row.asset_id)) : eligible,
          total: eligible.length,
        });
      }
      throw new Error(`Unexpected request ${url}`);
    });
    const cache = new PhotoPreviewCache("scoped", vi.fn());
    render(PhotosWorkspace, { photos, cache, vaultID: "vault" });
    await fireEvent.click(screen.getByRole("button", { name: "Cull" }));
    const cull = await screen.findByRole("dialog", { name: "Cull photos" });
    await within(cull).findByText("Photo 1.jpg");
    await fireEvent.click(within(cull).getByRole("button", { name: "Pick (P)" }));
    await within(cull).findByText("Photo 2.jpg");
    expect([...photos.selectedIDs]).toEqual(ids);
    await fireEvent.click(within(cull).getByRole("button", { name: "Exit cull" }));
    await waitFor(() => expect([...photos.selectedIDs]).toEqual(oneUnavailable ? [ids[1]!] : ids));
    photos.dispose();
    await cache.dispose();
  },
);

it("reloads durable culls and retains their draft only for the same vault", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const rows = [photo(1), photo(2)];
  const photos = new Photos("scoped", vi.fn());
  photos.items = rows;
  photos.started = true;
  vi.spyOn(photos, "lookup").mockImplementation(
    async (id) => rows.find((row) => row.asset_id === id)!,
  );
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  let durableID = rows[0]!.asset_id;
  vi.spyOn(PhotoCull.prototype, "probe").mockImplementation(async function (this: PhotoCull) {
    this.saved = true;
  });
  const restore = vi
    .spyOn(PhotoCull.prototype, "restore")
    .mockImplementation(async function (this: PhotoCull) {
      this.run = {
        ids: [durableID],
        checkpoint: {
          runID: durableID,
          revision: 0,
          pass: [durableID],
          position: 0,
          visited: [],
          skipped: [],
          unavailable: [],
          scope: "all",
          autoAdvance: true,
        },
      };
      return true;
    });
  const props = {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    vaultID: "vault",
  };
  const view = render(PhotosWorkspace, props);
  const resume = await screen.findByRole("button", { name: "Resume cull" });
  durableID = rows[1]!.asset_id;
  await fireEvent.click(resume);
  const dialog = await screen.findByRole("dialog", { name: "Cull photos" });
  await within(dialog).findByText("Photo 2.jpg");
  expect(restore).toHaveBeenCalledTimes(1);
  const retained = photos.cullEditor!;
  const dispose = vi.spyOn(retained, "dispose");
  retained.stage({ caption: "Unsaved cull decision" });
  await view.rerender({ ...props, title: "Same vault" });
  expect(screen.getByRole("dialog", { name: "Cull photos" })).toBe(dialog);
  expect(within(dialog).getByRole("button", { name: "Discard edit" })).toBeTruthy();
  expect(restore).toHaveBeenCalledTimes(1);
  expect(dispose).not.toHaveBeenCalled();
  await view.rerender({ ...props, vaultID: "22222222-2222-4222-8222-222222222222" });
  expect(screen.queryByRole("dialog", { name: "Cull photos" })).toBeNull();
  expect(dispose).toHaveBeenCalledTimes(1);
  await fireEvent.click(screen.getByRole("button", { name: "Resume cull" }));
  const replacement = await screen.findByRole("dialog", { name: "Cull photos" });
  expect(within(replacement).queryByRole("button", { name: "Discard edit" })).toBeNull();
  photos.dispose();
});

async function captureWorkspace() {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(PhotoCull.prototype, "restore").mockResolvedValue(false);
  vi.spyOn(PhotoCull.prototype, "flush").mockResolvedValue();
  const rows = [photo(1), photo(2)];
  const ids = rows.map((row) => row.asset_id);
  const photos = new Photos("scoped", vi.fn());
  photos.items = rows;
  photos.total = rows.length;
  photos.started = true;
  vi.spyOn(photos, "lookup").mockImplementation(
    async (id) => rows.find((row) => row.asset_id === id)!,
  );
  vi.spyOn(photos, "lookupMany").mockResolvedValue(rows);
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  let finish!: () => void;
  vi.spyOn(PhotoCull.prototype, "start").mockImplementation(function (this: PhotoCull) {
    return new Promise<boolean>((resolve) => {
      finish = () => {
        this.run = {
          ids,
          checkpoint: {
            runID: "run",
            revision: 0,
            pass: ids,
            position: 0,
            visited: [],
            skipped: [],
            unavailable: [],
            scope: "all",
            autoAdvance: true,
          },
        };
        resolve(true);
      };
    });
  });
  render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    vaultID: "vault",
  });
  await fireEvent.click(screen.getByRole("button", { name: "Cull" }));
  await waitFor(() => expect(finish).toBeDefined());
  return { photos, ids, finish: () => finish() };
}

it("compares eight selected photos and asks to narrow a larger selection", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const rows = Array.from({ length: 9 }, (_, index) => photo(index + 1));
  const ids = rows.map((row) => row.asset_id);
  const photos = new Photos("scoped", vi.fn());
  photos.items = rows;
  photos.total = rows.length;
  photos.started = true;
  vi.spyOn(photos, "lookupMany").mockImplementation(async (members) =>
    members.map((id) => rows.find((row) => row.asset_id === id)!),
  );
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  for (const id of ids) photos.select(id, new MouseEvent("click", { ctrlKey: true }), ids);
  render(PhotosWorkspace, { photos, cache: { get: vi.fn() } as unknown as PhotoPreviewCache });
  expect((screen.getByRole("button", { name: "Compare" }) as HTMLButtonElement).disabled).toBe(
    true,
  );
  expect(screen.getByText("Select 2–8 photos to compare.")).toBeTruthy();
  await act(() => photos.select(ids[8]!, new MouseEvent("click", { ctrlKey: true }), ids));
  const compare = screen.getByRole("button", { name: "Compare" });
  expect((compare as HTMLButtonElement).disabled).toBe(false);
  expect(compare.title).toBe("Select 2–8 photos to compare");
  await fireEvent.click(compare);
  const dialog = await screen.findByRole("dialog", { name: "Compare photos" });
  await waitFor(() =>
    expect(
      within(dialog)
        .getAllByRole("heading")
        .map((heading) => heading.textContent),
    ).toEqual([
      "Photo 1.jpg",
      "Photo 2.jpg",
      "Photo 3.jpg",
      "Photo 4.jpg",
      "Photo 5.jpg",
      "Photo 6.jpg",
      "Photo 7.jpg",
      "Photo 8.jpg",
    ]),
  );
  photos.dispose();
});

it.each(["compare", "draft"])(
  "handles a competing %s action while cull capture finishes",
  async (action) => {
    const { photos, ids, finish } = await captureWorkspace();
    if (action === "compare") {
      photos.select(ids[0]!, new MouseEvent("click"), ids);
      photos.select(ids[1]!, new MouseEvent("click", { ctrlKey: true }), ids);
      await fireEvent.click(screen.getByRole("button", { name: "Compare" }));
      expect(screen.queryByRole("dialog", { name: "Compare photos" })).toBeNull();
      expect(screen.queryByRole("dialog", { name: "Cull photos" })).toBeNull();
    } else photos.authored.stage({ caption: "Draft written while capturing" });
    await act(async () => finish());
    if (action === "compare") {
      expect(screen.queryByRole("dialog", { name: "Compare photos" })).toBeNull();
      expect(screen.getByRole("dialog", { name: "Cull photos" })).toBeTruthy();
    } else {
      expect(screen.queryByRole("dialog", { name: "Cull photos" })).toBeNull();
      expect(photos.authored.dirty).toBe(true);
    }
    photos.dispose();
  },
);

it("uses the resolved vault identity when Cull mounted before audit status arrived", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  // Reaching IndexedDB distinguishes a valid binding from the empty-identity guard.
  const open = vi.fn(() => {
    throw new Error("Synthetic storage reached");
  });
  vi.stubGlobal("indexedDB", { open });
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1)];
  photos.total = 1;
  photos.started = true;
  const cache = { get: vi.fn() } as unknown as PhotoPreviewCache;
  const view = render(PhotosWorkspace, { photos, cache, vaultID: "" });
  expect((screen.getByRole("button", { name: "Cull" }) as HTMLButtonElement).disabled).toBe(true);
  expect(screen.getByRole("button", { name: "Cull" }).title).toBe(
    "Vault identity unavailable. Reload to retry.",
  );
  await view.rerender({ photos, cache, vaultID: "11111111-1111-4111-8111-111111111111" });
  const cull = screen.getByRole("button", { name: "Cull" }) as HTMLButtonElement;
  expect(cull.disabled).toBe(false);
  expect(cull.title).toBe("");
  await fireEvent.click(cull);
  await waitFor(() =>
    expect(screen.getByRole("alert").textContent).toBe("Synthetic storage reached"),
  );
  expect(open).toHaveBeenCalledWith("docbank-photo-cull-v1", 1);
  photos.dispose();
});

it("resets an invalid burst gap input to 2", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1)];
  photos.total = 1;
  photos.started = true;
  render(PhotosWorkspace, { photos, cache: { get: vi.fn() } as unknown as PhotoPreviewCache });
  const input = screen.getByRole("spinbutton", { name: "Burst gap (seconds)" }) as HTMLInputElement;
  await fireEvent.change(input, { target: { value: "1.5" } });
  await waitFor(() => expect(input.value).toBe("2"));
  photos.dispose();
});

it("keeps a reloaded stage photo in its collapsed stack when returning to the grid", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  stubPhotoFetch(async () => Response.json({ items: [], total: 0, facets: [] }));
  const photos = new Photos("scoped", vi.fn());
  const stack = { asset_ids: ["photo-1", "photo-2"], representative_id: "photo-1" };
  photos.items = [photo(1), photo(2)].map((row) => ({ ...row, stack }));
  photos.total = 2;
  photos.started = true;
  photos.viewerReturn = {
    contextID: photos.contextID,
    assetID: "photo-1",
    control: "open",
    scrollTop: 0,
  };
  vi.spyOn(photos, "resume").mockResolvedValue();
  vi.spyOn(photos, "lookup").mockResolvedValue({ ...photo(1), name: "Reloaded photo.jpg" });
  vi.spyOn(photos, "preview")
    .mockRejectedValueOnce(new APIError("Photo changed", 409, "photo_display_changed"))
    .mockResolvedValue({ state: "unsupported" });
  const view = render(PhotosWorkspace, {
    photos,
    photoID: "photo-1",
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Reload photo" }));
  await screen.findByRole("dialog", { name: "Photo viewer, Reloaded photo.jpg" });
  await view.rerender({ photoID: "" });
  await screen.findByRole("button", { name: "Select Reloaded photo.jpg" });
  expect(screen.queryByRole("button", { name: "Select Photo 2.jpg" })).toBeNull();
  expect(screen.getByRole("button", { name: "Expand stack of 2 photos" })).toBeTruthy();
  photos.dispose();
});

it.each(["Keep all", "Keep selected", "Choose representative"])(
  "preserves selected frames after %s followed by Keep selected",
  async (firstAction) => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
    const rows = [photo(1), photo(2), photo(3)];
    const stack = {
      asset_ids: rows.map((row) => row.asset_id),
      representative_id: "photo-1",
    };
    const targets = rows.map((row) => preparedPhoto(Number(row.asset_id.slice(6))));
    vi.spyOn(api, "preparePhotoAuthored").mockImplementation(async (scope) =>
      photoPreparation(
        targets
          .filter((target) => scope.asset_ids?.includes(target.asset_id))
          .map((target) => ({ ...target, values: { ...target.values } })),
      ),
    );
    stubPhotoFetch(async () =>
      Response.json({
        items: rows,
        stacks: [stack],
        total: 3,
        facets: [],
        relevance_available: false,
      }),
    );
    const decide = vi.spyOn(api, "decidePhotoStack").mockImplementation(async (request) => {
      if (request.decision === "representative") {
        const row = rows.find((row) => row.asset_id === request.current_id)!;
        const before = row.revision;
        row.revision++;
        targets.find((target) => target.asset_id === row.asset_id)!.asset_revision++;
        return {
          receipt_id: "representative",
          before: [],
          after: [],
          representatives: [
            {
              asset_id: row.asset_id,
              before_revision: before,
              after_revision: row.revision,
              after: 1,
            },
          ],
        };
      }
      const before: api.PhotoAuthoredSnapshot[] = [],
        after: api.PhotoAuthoredSnapshot[] = [];
      for (const target of targets) {
        before.push({ ...target, values: { ...target.values } });
        target.revision++;
        target.node_revision++;
        target.values.flag =
          request.decision === "all" || request.selected_ids?.includes(target.asset_id)
            ? "pick"
            : target.values.flag || "reject";
        rows.find((row) => row.asset_id === target.asset_id)!.flag = target.values.flag;
        after.push({ ...target, values: { ...target.values } });
      }
      return { receipt_id: after.length ? "stack" : "", before, after };
    });
    const photos = new Photos("scoped", vi.fn());
    photos.items = rows.map((row) => ({ ...row, stack }));
    photos.total = 3;
    photos.started = true;
    photos.expandedStacks = new Set(["photo-1"]);
    photos.selection = { selectedIDs: new Set(["photo-2"]), anchorID: "photo-2" };
    vi.spyOn(photos, "resume").mockResolvedValue();
    const refresh = vi.spyOn(photos, "refresh");
    render(PhotosWorkspace, { photos, cache: { get: vi.fn() } as unknown as PhotoPreviewCache });
    for (let i = 1; i <= 2; i++) {
      const button = await screen.findByRole("button", {
        name: i === 1 ? firstAction : "Keep selected",
      });
      await waitFor(() => expect(button.matches(":disabled")).toBe(false));
      await fireEvent.click(button);
      await waitFor(() => expect(decide).toHaveBeenCalledTimes(i));
      await waitFor(() => expect(photos.authored.busy || photos.loading).toBe(false));
      await Promise.all(refresh.mock.results.map((result) => result.value));
      expect([...photos.selectedIDs]).toEqual(["photo-2"]);
      expect(photos.scope()).toEqual({ asset_ids: ["photo-2"] });
    }
    expect(decide.mock.calls.map(([request]) => request.selected_ids)).toEqual([
      ["photo-2"],
      ["photo-2"],
    ]);
    expect(decide.mock.calls.map(([request]) => request.current_id)).toEqual([
      "photo-2",
      "photo-2",
    ]);
    expect(decide.mock.calls.map(([request]) => request.decision)).toEqual([
      firstAction === "Keep all"
        ? "all"
        : firstAction === "Keep selected"
          ? "selected"
          : "representative",
      "selected",
    ]);
    photos.dispose();
  },
);

it("reloads a stack conflict and keeps the selection", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const rows = [photo(1), photo(2)];
  const stack = { asset_ids: ["photo-1", "photo-2"], representative_id: "photo-1" };
  stubPhotoFetch(async () =>
    Response.json({
      items: rows,
      stacks: [stack],
      total: 2,
      facets: [],
      relevance_available: false,
    }),
  );
  const prepare = vi
    .spyOn(api, "preparePhotoAuthored")
    .mockRejectedValueOnce(new APIError("Stack changed", 409, "photo_stack_changed"))
    .mockResolvedValue(photoPreparation([preparedPhoto(1), preparedPhoto(2)]));
  const photos = new Photos("scoped", vi.fn());
  photos.items = rows.map((row) => ({ ...row, stack }));
  photos.total = 2;
  photos.started = true;
  photos.selection = { selectedIDs: new Set(["photo-1"]), anchorID: "photo-1" };
  vi.spyOn(photos, "resume").mockResolvedValue();
  render(PhotosWorkspace, { photos, cache: { get: vi.fn() } as unknown as PhotoPreviewCache });
  await fireEvent.click(await screen.findByRole("button", { name: "Reload stack" }));
  await waitFor(() => expect(prepare).toHaveBeenCalledTimes(2));
  await waitFor(() => expect(photos.authored.stackLoading || photos.authored.conflict).toBe(false));
  expect([...photos.selectedIDs]).toEqual(["photo-1"]);
  expect(screen.getByRole("button", { name: "Keep all" }).matches(":disabled")).toBe(false);
  photos.dispose();
});

it("range selects in rendered month order across offset dates and a collapsed stack", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  stubPhotoFetch(async () => Response.json({ items: [], total: 0, facets: [] }));
  const photos = new Photos("scoped", vi.fn());
  const stack = { asset_ids: ["photo-2", "photo-3"], representative_id: "photo-2" };
  photos.items = [
    photo(1, "2025-02-28T23:59:59-12:00"),
    { ...photo(2, "2025-03-01T00:00:02+14:00"), stack },
    { ...photo(3, "2025-03-01T00:00:01+14:00"), stack },
    photo(4, "2025-03-01T00:00:00+14:00"),
  ];
  photos.total = 4;
  photos.started = true;
  vi.spyOn(photos, "resume").mockResolvedValue();
  render(PhotosWorkspace, { photos, cache: { get: vi.fn() } as unknown as PhotoPreviewCache });
  const first = await screen.findByRole("button", { name: "Select Photo 2.jpg" });
  expect(
    screen
      .getAllByRole("button", { name: /^Select Photo/ })
      .map((button) => button.getAttribute("aria-label")),
  ).toEqual(["Select Photo 2.jpg", "Select Photo 4.jpg", "Select Photo 1.jpg"]);
  await fireEvent.click(first);
  await waitFor(() => expect(photos.authored.stackLoading).toBe(false));
  await fireEvent.click(screen.getByRole("button", { name: "Select Photo 1.jpg" }), {
    shiftKey: true,
  });
  expect([...photos.selectedIDs].sort()).toEqual(["photo-1", "photo-2", "photo-3", "photo-4"]);
  photos.dispose();
});

it.each(
  [1, 2].flatMap((formatVersion) =>
    ["Cancel", "Replace"].map((answer) => ({ formatVersion, answer })),
  ),
)(
  "$answer in the capture modal preserves or replaces saved format $formatVersion",
  async ({ answer, formatVersion }) => {
    vi.stubGlobal("indexedDB", new IDBFactory());
    vi.stubGlobal("IDBKeyRange", IDBKeyRange);
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    const store = photoCullStorage("synthetic-vault");
    const original = {
      ids: ["photo-1"],
      checkpoint: {
        runID: "saved-run",
        revision: 0,
        pass: ["photo-1"],
        position: 0,
        visited: [],
        skipped: [],
        unavailable: [],
        scope: "all" as const,
        autoAdvance: true,
      },
    };
    await store.replace(original);
    if (formatVersion === 2) {
      const saved = await store.saved();
      const db = await idbRequest(indexedDB.open("docbank-photo-cull-v1", 1));
      try {
        const tx = db.transaction("checkpoints", "readwrite");
        const done = transactionDone(tx);
        tx.objectStore("checkpoints").put({ ...saved, formatVersion }, "synthetic-vault");
        await done;
      } finally {
        db.close();
      }
    }
    const durable = formatVersion === 1 ? await store.load() : undefined;
    const photos = new Photos("scoped", vi.fn());
    photos.items = [photo(1)];
    photos.total = 1;
    photos.started = true;
    const capture = vi
      .spyOn(api, "createWorkspaceQuery")
      .mockResolvedValue(Response.json(await snapshotEnvelope(photos.query, [])));
    vi.spyOn(api, "getPhotoSettings").mockResolvedValue({
      revision: 1,
      preference: null,
      burst_gap_seconds: 2,
    });
    render(PhotosWorkspace, {
      photos,
      vaultID: "synthetic-vault",
      cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    });
    if (formatVersion === 2) {
      await screen.findByText(
        "Saved cull progress uses an unsupported format. Update Docbank to resume it.",
      );
      expect(screen.queryByRole("button", { name: "Resume cull" })).toBeNull();
    } else await screen.findByRole("button", { name: "Resume cull" });
    await fireEvent.click(screen.getByRole("button", { name: "Cull" }));
    await screen.findByRole("dialog", { name: "Replace saved cull progress?" });
    expect(capture).not.toHaveBeenCalled();
    await fireEvent.click(screen.getByRole("button", { name: answer }));
    if (answer === "Cancel") {
      await waitFor(() => expect(photos.cull!.capturing).toBe(false));
      expect(screen.queryByRole("dialog", { name: "Cull photos" })).toBeNull();
      expect(capture).not.toHaveBeenCalled();
      if (formatVersion === 2) {
        await expect(store.load()).rejects.toThrow("unsupported format");
        await screen.findByText(
          "Saved cull progress uses an unsupported format. Update Docbank to resume it.",
        );
      } else expect(await store.load()).toEqual(durable);
    } else {
      await screen.findByRole("dialog", { name: "Cull photos" });
      expect(capture).toHaveBeenCalledOnce();
      await photos.cull!.flush();
      expect((await store.load())?.ids).toEqual([]);
    }
    photos.dispose();
  },
);
it("shows the person filter chip and clears its query filter", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  stubPhotoFetch(async () => Response.json({ items: [], total: 0 }));
  const photos = new Photos("scoped", vi.fn());
  photos.personFilter = { id: "10000000-0000-4000-8000-000000000001", name: "Ada" };
  await photos.setQuery({
    ...photoQuery,
    filters: { person_ids: ["10000000-0000-4000-8000-000000000001"] },
  });
  photos.started = true;
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache });
  expect(await screen.findByText("Person: Ada")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
  await waitFor(() => expect(screen.queryByText("Person: Ada")).toBeNull());
  expect(photos.query.filters.person_ids).toBeUndefined();
  photos.dispose();
  await cache.dispose();
});

async function facesWorkspaceSetup() {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  Object.defineProperty(URL, "createObjectURL", {
    configurable: true,
    value: vi.fn(() => "blob:synthetic"),
  });
  Object.defineProperty(URL, "revokeObjectURL", { configurable: true, value: vi.fn() });
  Object.defineProperty(HTMLImageElement.prototype, "decode", {
    configurable: true,
    value: vi.fn(async () => {}),
  });
  const read = vi.spyOn(api, "listPhotoFaces").mockResolvedValue(photoFacePage());
  stubPhotoFetch(async () => Response.json({ items: [photo(1)], total: 1, facets: [] }));
  const photos = new Photos("session", vi.fn());
  photos.items = [photo(1)];
  photos.selectedIDs = new Set(["photo-1"]);
  photos.total = 1;
  photos.started = true;
  vi.spyOn(photos, "preview").mockResolvedValue({
    state: "ready",
    generation_id: "fit",
    width: 1000,
    height: 800,
  });
  const onphotochange = vi.fn();
  const props = {
    onphotochange,
    photos,
    cache: { get: vi.fn(async () => new Blob(["synthetic"])) } as unknown as PhotoPreviewCache,
  };
  const component = render(PhotosWorkspace, props);
  await fireEvent.click(await screen.findByRole("button", { name: "Metadata" }));
  await screen.findByRole("button", { name: "Find or create a person: Ada" });
  return { component, props, onphotochange, photos, read };
}

it("keeps loaded faces when opening Loupe", async () => {
  const { component, props, onphotochange, read } = await facesWorkspaceSetup();
  await fireEvent.click(screen.getByRole("button", { name: "Open Photo 1.jpg" }));
  expect(onphotochange).toHaveBeenCalledWith("photo-1", "open");
  await component.rerender({ ...props, photoID: "photo-1" });
  const loupe = await screen.findByRole("dialog", { name: /^Photo viewer,/ });
  const image = await within(loupe).findByRole("img", { name: "Photo 1.jpg" });
  Object.defineProperty(image, "naturalWidth", { value: 1000 });
  Object.defineProperty(image, "naturalHeight", { value: 800 });
  await fireEvent.load(image);
  await fireEvent.click(within(loupe).getByRole("button", { name: "Metadata" }));
  await within(loupe).findByRole("button", { name: "Find or create a person: Ada" });
  expect(read).toHaveBeenCalledOnce();
});

it("clears loaded faces before failed Map navigation", async () => {
  const { component, props, photos, read } = await facesWorkspaceSetup();
  vi.spyOn(photos, "lookup").mockRejectedValue(new Error("Photo unavailable"));
  await component.rerender({ ...props, mapMode: true, photoID: "photo-2" });
  await screen.findByText("Photo unavailable");
  expect(screen.queryByRole("button", { name: "Find or create a person: Ada" })).toBeNull();
  expect(photos.faceEditor?.version).toBe("");
  expect(read).toHaveBeenCalledOnce();
});

it.each([false, true])(
  "revalidates same-version People after refresh, readable=%s",
  async (readable) => {
    const { component, props, photos, read } = await facesWorkspaceSetup();
    await fireEvent.click(screen.getByRole("button", { name: "Open Photo 1.jpg" }));
    await component.rerender({ ...props, photoID: "photo-1" });
    await fireEvent.click(
      within(await screen.findByRole("dialog", { name: /^Photo viewer,/ })).getByRole("button", {
        name: "Metadata",
      }),
    );
    read.mockResolvedValue(
      photoFacePage({
        people: [{ person_id: "person-a", display_name: "Grace", state: "curated" }],
      }),
    );
    let finish!: (page: Response) => void;
    stubPhotoFetch(
      () =>
        new Promise<Response>((resolve) => {
          finish = resolve;
        }),
    );
    vi.spyOn(photos, "lookup").mockImplementation(async () => {
      if (!readable) throw new Error("Photo unavailable");
      return photo(1);
    });
    let refreshing!: Promise<boolean>;
    await act(() => {
      refreshing = photos.refresh();
    });
    expect(screen.queryByRole("button", { name: "Find or create a person: Ada" })).toBeNull();
    await act(async () => {
      finish(Response.json({ items: readable ? [photo(1)] : [], total: readable ? 1 : 0 }));
      await refreshing;
    });
    if (readable)
      await within(screen.getByRole("dialog", { name: /^Photo viewer,/ })).findByRole("button", {
        name: "Find or create a person: Grace",
      });
    else {
      await waitFor(() => expect(photos.lookup).toHaveBeenCalled());
      expect(screen.queryByRole("region", { name: "People" })).toBeNull();
    }
  },
);

it("reports an uncertain clear after remount", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  const row = photo(1);
  let committed = false;
  let finish!: () => void;
  vi.spyOn(api, "listPhotoFaces").mockImplementation(async () =>
    photoFacePage(
      committed
        ? { faces: [photoFace({ assertion_id: undefined })], assertions: [], people: [] }
        : {},
    ),
  );
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [] });
  vi.spyOn(api, "decidePhotoFace").mockImplementation(async (version, decision, options) => {
    expect(version).toBe(row.content_version_id);
    expect(decision).toMatchObject({ action: "retract", face_id: "face-a", revision: 7 });
    await new Promise<void>((resolve) => (finish = resolve));
    expect(options!.signal!.aborted).toBe(false);
    committed = true;
    throw new Error("Response lost");
  });
  stubPhotoFetch(async (url, init) => {
    if (!url.endsWith("/photos/assets/query")) return Response.json({ state: "unsupported" });
    const query = JSON.parse(String(init.body ?? "{}")).query;
    const dependent = !!query.filters.person_ids?.length || !!query.text;
    const rows = committed && dependent && !query?.filters?.asset_ids ? [] : [row];
    return Response.json({ items: rows, total: rows.length, facets: [] });
  });
  const photos = new Photos("session", vi.fn(), false, { ...photoQuery, text: "person:Ada" });
  photos.items = [row];
  photos.total = 1;
  photos.started = true;
  let cache = new PhotoPreviewCache("session", vi.fn());
  const props = {
    photos,
    cache,
    photoID: row.asset_id,
    onphotochange: vi.fn(),
  };
  let view = render(PhotosWorkspace, props);
  const loupe = await screen.findByRole("dialog", { name: /^Photo viewer,/ });
  await fireEvent.click(within(loupe).getByRole("button", { name: "Metadata" }));
  await fireEvent.click(
    await within(loupe).findByRole("button", { name: "Find or create a person: Ada" }),
  );
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Clear name" }));
  await waitFor(() => expect(finish).toBeTypeOf("function"));
  view.unmount();
  await cache.dispose();
  cache = new PhotoPreviewCache("session", vi.fn());
  finish();
  await waitFor(() => expect(photos.total).toBe(0));
  view = render(PhotosWorkspace, { ...props, cache, photoID: "" });
  expect((await screen.findAllByText("Response lost")).length).toBeGreaterThan(0);
  await screen.findByText("No matching photos");
  photos.dispose();
  await cache.dispose();
});
