import { afterEach, expect, it, vi } from "vite-plus/test";
import { tick } from "svelte";
import { createShortcutManager } from "@kenn-io/kit-ui";
import * as photoLoupe from "./PhotoLoupe.svelte";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import PhotosWorkspace from "./PhotosWorkspace.svelte";
import * as api from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { Photos, photoQuery, mapPhotoLimit, type MapView } from "./photos.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";
import {
  photoMetadataResponse,
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
      // Navigation continues from the removed photo's former position.
      expect(
        screen
          .getAllByRole("button", { name: "Previous photo" })
          .every((button) => !(button as HTMLButtonElement).disabled),
      ).toBe(true);
      if (scenario === "removed") {
        await fireEvent.keyDown(screen.getByRole("dialog"), { key: "ArrowLeft" });
        await waitFor(() => expect(onphotochange).toHaveBeenLastCalledWith("photo-1", "step"));
      }
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
            .every((button) => !(button as HTMLButtonElement).disabled),
        ).toBe(true);
        expect(
          within(screen.getByRole("navigation", { name: "Photos in current result" }))
            .getAllByRole("button")
            .map((button) => button.getAttribute("aria-label")),
        ).toEqual(["Show Photo 1.jpg", "Show Photo 3.jpg"]);
        await fireEvent.keyDown(screen.getByRole("dialog"), { key: "ArrowRight" });
        await waitFor(() => expect(onphotochange).toHaveBeenLastCalledWith("photo-3", "step"));
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

it.each(["asset", "query", "album", "refetch", "removed", "failure", "burst", "followup", "early"])(
  "revalidates pending initial metadata after a live %s change",
  async (scenario) => {
    const photos = workspaceWithPreparedPhoto();
    photos.authored.targets = [];
    const queryScope = ["query", "album", "early"].includes(scenario);
    const album = "11111111-1111-4111-8111-111111111111";
    if (scenario === "album") photos.query.filters = { set_ids: [album] };
    if (queryScope) photos.selectAllResults();
    else photos.selectLoaded();
    const before = preparedPhoto(1, { values: { ...preparedPhoto(1).values, caption: "Before" } });
    const current = preparedPhoto(1, {
      revision: 7,
      values: { ...before.values, caption: "After" },
    });
    const unloaded = preparedPhoto(2);
    const replies: ((value: Awaited<ReturnType<typeof api.preparePhotoAuthored>>) => void)[] = [];
    const prepare = vi
      .spyOn(api, "preparePhotoAuthored")
      .mockImplementation(() => new Promise((resolve) => replies.push(resolve)));
    const edit = vi.spyOn(api, "editPhotoAuthored").mockResolvedValue({
      receipt_id: "",
      before: [],
      after: [],
    });
    let failList = scenario === "failure";
    let finishList!: () => void;
    stubPhotoFetch(async (url) => {
      if (!url.endsWith("/photos/assets/query")) throw new Error(`Unexpected request: ${url}`);
      if (failList) throw new Error("Listing failed");
      if (scenario === "early" && !finishList)
        await new Promise<void>((resolve) => (finishList = resolve));
      return Response.json({
        items: scenario === "removed" ? [] : [photo(1)],
        total: queryScope ? 2 : scenario === "removed" ? 0 : 1,
        facets: [],
      });
    });
    const view = render(PhotosWorkspace, {
      photos,
      cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    });
    await fireEvent.click(screen.getByRole("button", { name: "Metadata" }));
    await waitFor(() => expect(prepare).toHaveBeenCalledOnce());
    const changes = {
      assets: new Map(scenario === "album" || scenario === "refetch" ? [] : [["photo-1", 2]]),
      albums: new Map(scenario === "album" ? [[album, 2]] : []),
      refetch: scenario === "refetch",
    };
    const refreshing = view.component.refresh(changes);
    await tick();
    expect(
      screen.queryByText("No photos are available to review. Clear edits to discard this draft."),
    ).toBeNull();
    if (scenario === "failure") {
      await expect(refreshing).rejects.toThrow("Listing failed");
      replies[0]!({ assets: [], targets: [before] });
      await waitFor(() => expect(photos.authored.loading).toBe(false));
      expect(photos.authored.conflict).toBe(true);
      expect(
        screen.getByRole<HTMLButtonElement>("button", { name: "Reload metadata" }).disabled,
      ).toBe(false);
      expect(prepare).toHaveBeenCalledOnce();
      failList = false;
    } else if (scenario === "early") {
      await waitFor(() => expect(finishList).toBeTypeOf("function"));
      replies[0]!({ assets: [], targets: [before, unloaded] });
      await waitFor(() => expect(photos.authored.loading).toBe(false));
      finishList();
    } else {
      await waitFor(() => expect(photos.loading).toBe(false));
      expect(photos.authored.loading).toBe(true);
      expect(prepare).toHaveBeenCalledOnce();
    }
    const retry =
      scenario === "failure" || scenario === "burst" ? view.component.refresh(changes) : undefined;
    if (!["failure", "early"].includes(scenario)) replies[0]!({ assets: [], targets: [before] });
    await waitFor(() => expect(prepare).toHaveBeenCalledTimes(2));
    expect(screen.getByRole<HTMLButtonElement>("button", { name: "Apply" }).disabled).toBe(true);
    const scope = prepare.mock.calls[1]![0];
    if (queryScope) {
      expect(scope.query).toEqual(photos.query);
      expect(scope.asset_ids).toBeUndefined();
    } else expect(scope.asset_ids).toEqual(scenario === "removed" ? [] : ["photo-1"]);
    if (scenario === "followup") await view.component.refresh(changes);
    replies[1]!({
      assets: [],
      targets:
        scenario === "removed"
          ? []
          : scenario === "followup"
            ? [before]
            : queryScope
              ? [current, unloaded]
              : [current],
    });
    if (scenario !== "failure") await refreshing;
    await retry;
    await tick();
    expect(prepare).toHaveBeenCalledTimes(2);
    expect(photos.authored.loading).toBe(false);
    if (scenario === "followup") {
      expect(photos.authored.conflict).toBe(true);
      expect(
        screen.getByRole<HTMLButtonElement>("button", { name: "Reload metadata" }).disabled,
      ).toBe(false);
    } else if (scenario === "removed") expect(photos.authored.targets).toEqual([]);
    else {
      expect(photos.authored.value("caption")).toBe(queryScope ? undefined : "After");
      await photos.authored.edit({}, { rating: 5 }, false, vi.fn());
      expect(edit).toHaveBeenCalledWith(
        expect.objectContaining({
          targets: expect.arrayContaining([
            expect.objectContaining({ file_id: current.file_id, revision: 7 }),
          ]),
        }),
        expect.anything(),
      );
      if (queryScope) expect(edit.mock.calls[0]![0].targets).toHaveLength(2);
    }
    view.unmount();
    photos.dispose();
  },
);

it("keeps metadata focus and selection through an unrelated live list and comparison", async () => {
  const target = preparedPhoto(1);
  const photos = workspaceWithPreparedPhoto(target);
  photos.selectLoaded();
  photos.authored.targets = [target];
  const selected = [...photos.selectedIDs];
  let finishList!: () => void;
  let finishPrepare!: () => void;
  const prepare = vi.spyOn(api, "preparePhotoAuthored").mockImplementation(async () => {
    await new Promise<void>((resolve) => (finishPrepare = resolve));
    return { assets: [], targets: [target] };
  });
  const edit = vi.spyOn(api, "editPhotoAuthored");
  const undo = vi.spyOn(api, "undoPhotoAuthored");
  stubPhotoFetch(async (url) => {
    if (!url.endsWith("/photos/assets/query")) throw new Error(`Unexpected request: ${url}`);
    await new Promise<void>((resolve) => (finishList = resolve));
    return Response.json({ items: [photo(1)], total: 1, facets: [] });
  });
  const view = render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
  });
  await fireEvent.click(screen.getByRole("button", { name: "Metadata" }));
  const caption = screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Caption" });
  await fireEvent.input(caption, { target: { value: "Local draft" } });
  caption.focus();
  caption.setSelectionRange(1, 6);
  photos.authored.receiptID = "prior";
  const refreshing = view.component.refresh({
    assets: new Map(),
    albums: new Map(),
    refetch: true,
  });
  await waitFor(() => expect(finishList).toBeTypeOf("function"));
  expect(caption.disabled).toBe(false);
  expect(document.activeElement).toBe(caption);
  expect(screen.getByRole<HTMLButtonElement>("button", { name: "Apply" }).disabled).toBe(true);
  await photos.authored.edit({}, { rating: 5 }, false, vi.fn());
  await photos.authored.undo(vi.fn());
  expect(edit).not.toHaveBeenCalled();
  expect(undo).not.toHaveBeenCalled();
  finishList();
  await waitFor(() => expect(prepare).toHaveBeenCalledOnce());
  expect(caption.disabled).toBe(false);
  expect(document.activeElement).toBe(caption);
  expect(screen.getByRole("textbox", { name: "Caption" })).toBe(caption);
  expect([caption.selectionStart, caption.selectionEnd]).toEqual([1, 6]);
  expect([...photos.selectedIDs]).toEqual(selected);
  expect(screen.getByRole<HTMLButtonElement>("button", { name: "Apply" }).disabled).toBe(true);
  finishPrepare();
  await refreshing;
  await tick();
  expect(document.activeElement).toBe(caption);
  expect([caption.selectionStart, caption.selectionEnd]).toEqual([1, 6]);
  expect(caption.value).toBe("Local draft");
  expect(screen.getByRole<HTMLButtonElement>("button", { name: "Apply" }).disabled).toBe(false);
  view.unmount();
  photos.dispose();
});

it.each(["listing", "comparison"])(
  "blocks retained metadata after a live %s failure",
  async (phase) => {
    const photos = workspaceWithPreparedPhoto();
    const prepare = vi
      .spyOn(api, "preparePhotoAuthored")
      .mockRejectedValue(new APIError("Comparison failed", 503, "unavailable"));
    stubPhotoFetch(async () => {
      if (phase === "listing") throw new Error("Listing failed");
      return Response.json({ items: [photo(1)], total: 1, facets: [] });
    });
    const view = render(PhotosWorkspace, {
      photos,
      cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    });
    await fireEvent.click(screen.getByRole("button", { name: "Metadata" }));
    await fireEvent.input(screen.getByRole("textbox", { name: "Caption" }), {
      target: { value: "Local draft" },
    });
    const refreshing = view.component.refresh({
      assets: new Map(),
      albums: new Map(),
      refetch: true,
    });
    if (phase === "listing") await expect(refreshing).rejects.toThrow("Listing failed");
    else await refreshing;
    await tick();
    expect(photos.authored.patch.caption).toBe("Local draft");
    expect(screen.getByRole<HTMLButtonElement>("button", { name: "Apply" }).disabled).toBe(true);
    expect(
      screen.getByRole<HTMLButtonElement>("button", { name: "Reload metadata" }).disabled,
    ).toBe(false);
    if (phase === "comparison") expect(prepare).toHaveBeenCalledOnce();
    view.unmount();
    photos.dispose();
  },
);

it("shows a warning when a deleted keyword's staged edit is removed", async () => {
  const photos = workspaceWithPreparedPhoto();
  vi.spyOn(api, "getTag").mockRejectedValue(new APIError("Deleted", 404, "not_found"));
  const view = render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
  });
  await fireEvent.click(screen.getByRole("button", { name: "Metadata" }));
  const caption = screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Caption" });
  await fireEvent.input(caption, { target: { value: "Local draft" } });
  photos.authored.tags = [{ id: "deleted", name: "Travel", revision: 1, assignment_count: 0 }];
  photos.authored.stage({}, [{ tag_id: "deleted", assign: true }]);
  await photos.authored.refreshAssignedTags();
  expect(
    await screen.findByText("“Travel” was deleted. Its staged edit was removed."),
  ).toBeTruthy();
  expect(caption.value).toBe("Local draft");
  expect(screen.getByRole<HTMLButtonElement>("button", { name: "Apply" }).disabled).toBe(false);
  view.unmount();
  photos.dispose();
});

it.each([
  { dirty: false, refetch: true },
  { dirty: true, refetch: false },
  { dirty: true, refetch: true },
])(
  "reconciles open metadata after live changes, dirty=$dirty, refetch=$refetch",
  async ({ dirty, refetch }) => {
    const before = preparedPhoto(1);
    before.values.caption = "Before";
    const photos = workspaceWithPreparedPhoto(before);
    const unchanged = dirty && refetch;
    const updated = unchanged
      ? before
      : preparedPhoto(1, {
          asset_revision: 2,
          revision: 7,
          node_revision: 9,
          values: { ...before.values, caption: "Remote caption", rating: 4 },
        });
    let finishPrepare: (() => void) | undefined;
    const fetcher = vi.fn(async (url: string, init: RequestInit) => {
      if (url.endsWith("/photos/assets/query"))
        return Response.json({ items: [{ ...photo(1), revision: 2 }], total: 1, facets: [] });
      if (url.endsWith("/photos/authored/prepare")) {
        if (unchanged) await new Promise<void>((resolve) => (finishPrepare = resolve));
        return Response.json({ assets: [], targets: [updated] });
      }
      if (url.endsWith("/photos/authored/edit")) {
        const input = JSON.parse(String(init.body));
        return Response.json({
          receipt_id: "saved",
          before: [updated],
          after: [{ ...updated, values: { ...updated.values, ...input.patch } }],
        });
      }
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetcher);
    const view = render(PhotosWorkspace, {
      photos,
      cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    });
    await fireEvent.click(screen.getByRole("button", { name: "Metadata" }));
    const caption = () => screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Caption" });
    expect(caption().value).toBe("Before");
    if (dirty) await fireEvent.input(caption(), { target: { value: "Local draft" } });
    await view.component.refresh({
      assets: new Map([["photo-2", 2]]),
      albums: new Map(),
      refetch: false,
    });
    expect(caption().value).toBe(dirty ? "Local draft" : "Before");
    expect(fetcher.mock.calls.some(([url]) => url.endsWith("/photos/authored/prepare"))).toBe(
      false,
    );

    const refreshing = view.component.refresh({
      assets: new Map(refetch ? [] : [["photo-1", 2]]),
      albums: new Map(),
      refetch,
    });
    if (unchanged) {
      await waitFor(() => expect(finishPrepare).toBeTypeOf("function"));
      expect(
        screen.queryByText("Review the current photos before applying these edits."),
      ).toBeNull();
      finishPrepare!();
    }
    await refreshing;
    await tick();
    if (dirty && !unchanged) {
      expect(caption().value).toBe("Local draft");
      expect(screen.getByRole<HTMLButtonElement>("button", { name: "Apply" }).disabled).toBe(true);
      await fireEvent.click(screen.getByRole("button", { name: "Reload metadata" }));
      await waitFor(() => expect(caption().disabled).toBe(false));
      expect(caption().value).toBe("Local draft");
    } else {
      expect(caption().value).toBe(unchanged ? "Local draft" : "Remote caption");
      await fireEvent.input(caption(), { target: { value: "Local draft" } });
    }
    if (!unchanged) expect(screen.getByRole("combobox", { name: "Rating: 4 stars" })).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(photos.authored.busy).toBe(false));
    const saved = fetcher.mock.calls.find(([url]) => url.endsWith("/photos/authored/edit"));
    expect(JSON.parse(String(saved?.[1].body))).toMatchObject({
      targets: [
        {
          asset_id: "photo-1",
          asset_revision: updated.asset_revision,
          file_id: "file-1",
          revision: updated.revision,
          node_revision: updated.node_revision,
        },
      ],
      patch: { caption: "Local draft" },
    });
    view.unmount();
    photos.dispose();
  },
);

it("reports a saved edit as saved when its follow-up refresh fails", async () => {
  const before = preparedPhoto(1);
  const photos = workspaceWithPreparedPhoto(before);
  photos.query = { ...photos.query, text: "beach" };
  const saved = preparedPhoto(1, { revision: 7, values: { ...before.values, caption: "Saved" } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      if (url.endsWith("/photos/assets/query"))
        return Response.json({ detail: "Listing unavailable" }, { status: 503 });
      if (url.endsWith("/photos/authored/edit"))
        return Response.json({ receipt_id: "saved", before: [before], after: [saved] });
      throw new Error(`Unexpected request: ${url}`);
    }),
  );
  const view = render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
  });
  await fireEvent.click(screen.getByRole("button", { name: "Metadata" }));
  const caption = screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Caption" });
  await fireEvent.input(caption, { target: { value: "Saved" } });
  await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  await waitFor(() => expect(photos.error).toBe("Listing unavailable"));
  await waitFor(() => expect(photos.authored.busy).toBe(false));
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(photos.authored.error).toBe("");
  expect(photos.authored.conflict).toBe(false);
  expect(photos.authored.receiptID).toBe("saved");
  view.unmount();
  photos.dispose();
});

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
  const lookup = vi
    .spyOn(photos, "lookup")
    .mockImplementationOnce(() => new Promise(() => {}))
    .mockResolvedValue({ ...old, rotation: 90 });
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
  expect(oldSignal.aborted).toBe(true);
  expect(oldLookupSignal.aborted).toBe(true);
  expect(lookup.mock.calls.at(-1)![1].aborted).toBe(false);
  expect((photos.items[0] as PhotoBrowseRow).rotation).toBe(90);
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
    const fixture = viewerLookupFixture({
      projection: "map",
      rows: [mapPhoto(1), mapPhoto(2)],
      fromGrid: true,
      resolved: true,
      renderLoupe: true,
    });
    const { photos, responses, view, props } = fixture;
    responses.mockImplementation(async (id) => photo(id === "photo-1" ? 1 : 2));
    let finish!: (row: ReturnType<typeof photo>) => void;
    const reload = vi.spyOn(photos, "reload");
    if (outcome === "success") reload.mockResolvedValue({ ...photo(1), revision: 2 });
    else if (outcome === "failure") reload.mockRejectedValue(new Error("Reload failed"));
    else if (outcome === "late")
      reload.mockImplementation(() => new Promise((resolve) => (finish = resolve)));
    else reload.mockImplementation(() => new Promise(() => {}));
    vi.spyOn(photos, "preview")
      .mockRejectedValueOnce(new APIError("Changed", 409, "photo_display_changed"))
      .mockResolvedValue({ state: "unsupported" });
    await fireEvent.click(await screen.findByRole("button", { name: "Reload photo" }));
    expectActiveViewerLookups(fixture);
    await waitFor(() => expect(reload).toHaveBeenCalledOnce());
    if (outcome === "failure") await screen.findByText("Reload failed");
    expect(reload).toHaveBeenCalledWith("photo-1", expect.any(AbortSignal));
    if (outcome === "success") await waitFor(() => expect(fixture.loupe.photo?.revision).toBe(2));
    await view.rerender({ ...props, photoID: "photo-2" });
    await waitFor(() => expect(reload.mock.calls[0]![1].aborted).toBe(true));
    if (outcome === "late") {
      finish({ ...photo(1), content_version_id: "late-version" });
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(screen.getByRole("dialog", { name: "Photo viewer, Photo 2.jpg" })).toBeTruthy();
    }
    await view.rerender(props);
    await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
    expect(reload).toHaveBeenCalledOnce();
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
    undefined,
    undefined,
  );
  photos.dispose();
});

it.each([
  ["no target", undefined],
  ["loading", "loading"],
  ["hiding", "hiding"],
  ["trashing", "trashing"],
  ["busy", "busy"],
  ["validating", "validating"],
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
    if (pending === "busy" || pending === "validating") photos.authored[pending] = true;
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

function viewerLookupFixture({
  old = photo(1),
  rows = [photo(2)],
  projection,
  fromGrid = false,
  resolved = false,
  renderLoupe = false,
}: {
  old?: ReturnType<typeof photo>;
  rows?: Array<PhotoBrowseRow | PhotoMapRow>;
  projection?: "map";
  fromGrid?: boolean;
  resolved?: boolean;
  renderLoupe?: boolean;
} = {}) {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const photos = new Photos("scoped", vi.fn(), false, photoQuery, { projection });
  photos.started = true;
  photos.items = rows;
  if (fromGrid)
    photos.viewerReturn = {
      contextID: photos.contextID,
      assetID: "photo-1",
      control: "open",
      scrollTop: 0,
    };
  let finish!: (row: ReturnType<typeof photo>) => void;
  let reject!: (cause: Error) => void;
  const responses = vi
    .fn<(id: string, signal: AbortSignal) => Promise<ReturnType<typeof photo>>>()
    .mockImplementation(
      () =>
        new Promise((resolve, fail) => {
          finish = resolve;
          reject = fail;
        }),
    );
  const requests: { id: string; signal: AbortSignal; abortedAtCall: boolean | undefined }[] = [];
  const lookup = vi.spyOn(photos, "lookup").mockImplementation((id, signal) => {
    requests.push({ id, signal, abortedAtCall: signal?.aborted });
    return responses(id, signal);
  });
  if (resolved) responses.mockResolvedValueOnce(old);
  const Loupe = photoLoupe.default;
  let loupe!: Parameters<typeof photoLoupe.default>[1];
  vi.spyOn(photoLoupe, "default").mockImplementation((_anchor, inputs) => {
    loupe = inputs;
    return renderLoupe ? Loupe(_anchor, inputs) : {};
  });
  const props = {
    photos,
    cache: { get: vi.fn(async () => new Blob(["synthetic"])) } as unknown as PhotoPreviewCache,
    photoID: "photo-1",
    onphotochange: vi.fn(),
  };
  const view = render(PhotosWorkspace, props);
  const refreshRows = vi.spyOn(photos, "refresh").mockImplementation(async () => {
    photos.items = [...rows];
    return true;
  });
  return {
    old,
    photos,
    responses,
    lookup,
    requests,
    props,
    view,
    refreshRows,
    get rows() {
      return rows;
    },
    set rows(value: typeof rows) {
      rows = value;
    },
    get loupe() {
      return loupe;
    },
    get finish() {
      return finish;
    },
    get reject() {
      return reject;
    },
    set reject(value: typeof reject) {
      reject = value;
    },
  };
}

function expectActiveViewerLookups(fixture: ReturnType<typeof viewerLookupFixture>) {
  for (const request of fixture.requests)
    expect(request).toEqual({
      id: "photo-1",
      signal: expect.any(AbortSignal),
      abortedAtCall: false,
    });
}

async function pendingViewerRefresh(
  fixture: ReturnType<typeof viewerLookupFixture>,
  unrelated = false,
  noBatch = false,
) {
  const { responses, lookup, view } = fixture;
  let resolve!: (row: ReturnType<typeof photo>) => void;
  responses.mockImplementationOnce(
    () =>
      new Promise((done, fail) => {
        resolve = done;
        fixture.reject = fail;
      }),
  );
  await waitFor(() => expect(lookup).toHaveBeenCalledOnce());
  const refresh = view.component.refresh(
    noBatch
      ? undefined
      : {
          assets: new Map([[unrelated ? "photo-2" : "photo-1", unrelated ? 2 : 1]]),
          albums: new Map(),
          refetch: false,
        },
  );
  let settled = false;
  void refresh.then(
    () => (settled = true),
    () => (settled = true),
  );
  await waitFor(() => expect(lookup).toHaveBeenCalledTimes(2));
  await tick();
  if (!unrelated) expect(settled).toBe(false);
  expect(lookup).toHaveBeenCalledTimes(2);
  return { refresh, resolve };
}

it.each([
  "event revision stale",
  "event revision new",
  "event refresh false",
  "event listing incomplete",
  "event listing refetch",
  "event listing count failure",
  "event listing failure",
])("reconciles refreshed Library rows with the open viewer: %s", async (scenario) => {
  const old = photo(1);
  if (scenario.startsWith("event listing ")) old.revision = 2;
  const fixture = viewerLookupFixture({ old, rows: [old, photo(2)] });
  const { photos, responses, lookup, view, refreshRows } = fixture;
  await waitFor(() => expect(lookup).toHaveBeenCalledOnce());
  fixture.finish(old);
  await waitFor(() => expect(fixture.loupe.photo).toEqual(old));
  await waitFor(() => expect(fixture.loupe.reloading).toBe(false));
  responses.mockResolvedValue(old);
  lookup.mockClear();
  if (scenario === "event refresh false") {
    refreshRows.mockImplementation(async () => {
      photos.error = "Listing unavailable";
      return false;
    });
    await expect(
      view.component.refresh({ assets: new Map(), albums: new Map(), refetch: true }),
    ).rejects.toThrow("Listing unavailable");
  } else if (scenario === "event revision stale") {
    await view.component.refresh({
      assets: new Map([["photo-2", 2]]),
      albums: new Map(),
      refetch: false,
    });
    await tick();
    expect(lookup).not.toHaveBeenCalled();
    await view.component.refresh({
      assets: new Map([["photo-1", 1]]),
      albums: new Map(),
      refetch: false,
    });
    await tick();
    expect(lookup).not.toHaveBeenCalled();
  } else if (scenario === "event revision new") {
    responses.mockResolvedValue({ ...photo(1), revision: 2, name: "Event update.jpg" });
    await view.component.refresh({
      assets: new Map([["photo-1", 2]]),
      albums: new Map(),
      refetch: false,
    });
    await waitFor(() => expect(fixture.loupe.photo?.name).toBe("Event update.jpg"));
    expect(lookup).toHaveBeenCalledTimes(1);
  } else if (scenario === "event listing incomplete") {
    refreshRows.mockResolvedValueOnce(false);
    await view.component.refresh({
      assets: new Map([["photo-1", 2]]),
      albums: new Map(),
      refetch: false,
    });
    await waitFor(() => expect(lookup).toHaveBeenCalledTimes(1));
  } else if (scenario === "event listing refetch") {
    await view.component.refresh({ assets: new Map(), albums: new Map(), refetch: true });
    await waitFor(() => expect(lookup).toHaveBeenCalledTimes(1));
  } else if (scenario === "event listing count failure") {
    const cause = new Error("Counts unavailable");
    fixture.rows = [
      { ...photo(1), revision: 3, name: "Updated despite count failure.jpg" },
      photo(2),
    ];
    refreshRows.mockImplementationOnce(async () => {
      photos.items = [...fixture.rows];
      throw cause;
    });
    await expect(
      view.component.refresh({
        assets: new Map([["photo-1", 3]]),
        albums: new Map(),
        refetch: false,
      }),
    ).rejects.toBe(cause);
    await waitFor(() =>
      expect(fixture.loupe.photo?.name).toBe("Updated despite count failure.jpg"),
    );
    expect(lookup).not.toHaveBeenCalled();
  } else {
    const previous = fixture.loupe.photo;
    refreshRows.mockRejectedValueOnce(new Error("Listing unavailable"));
    await expect(
      view.component.refresh({
        assets: new Map([["photo-1", 4]]),
        albums: new Map(),
        refetch: false,
      }),
    ).rejects.toThrow("Listing unavailable");
    expect(lookup).not.toHaveBeenCalled();
    expect(fixture.loupe.photo).toBe(previous);
  }
  expect(
    refreshRows.mock.calls.every(
      ([preserve, live]) => typeof preserve === "function" && live === true,
    ),
  ).toBe(true);
  expectActiveViewerLookups(fixture);
  view.unmount();
});

it.each([
  "replaced read",
  "lookup failure",
  "removed then restored",
  "manual refresh",
  "lookup timeout",
])("reconciles pending viewer details after %s", async (scenario) => {
  const map = scenario === "replaced read" || scenario === "manual refresh";
  const ready = scenario === "removed then restored" || scenario === "lookup timeout";
  if (scenario === "manual refresh")
    vi.spyOn(Photos.prototype, "preview").mockResolvedValue({ state: "unsupported" });
  const fixture = viewerLookupFixture({
    projection: map ? "map" : undefined,
    rows: map ? [mapPhoto(1), mapPhoto(2)] : [photo(2)],
    resolved: ready,
    fromGrid: scenario === "replaced read",
    renderLoupe: scenario === "manual refresh",
  });
  const { old, lookup, view } = fixture;
  await waitFor(() => expect(lookup).toHaveBeenCalledOnce());
  if (ready) await waitFor(() => expect(fixture.loupe.photo?.asset_id).toBe(old.asset_id));
  const finishInitial = fixture.finish;
  const rejectInitial = fixture.reject;
  const refreshed = { ...photo(1), revision: 2, name: "Recovered photo.jpg" };
  let dialog: HTMLElement | undefined;
  if (scenario === "manual refresh") {
    dialog = await screen.findByRole("dialog", { name: "Photo viewer" });
    fixture.rows = [{ ...mapPhoto(1), revision: 2, name: refreshed.name }];
  }
  if (scenario === "replaced read") fixture.rows = [mapPhoto(1), mapPhoto(2)];
  const deadline = new AbortController();
  if (scenario === "lookup timeout") {
    await waitFor(() => expect(fixture.loupe.reloading).toBe(false));
    vi.spyOn(AbortSignal, "timeout").mockReturnValue(deadline.signal);
  }
  const { refresh, resolve } = await pendingViewerRefresh(
    fixture,
    scenario === "replaced read",
    scenario === "manual refresh",
  );
  if (!ready) expect(lookup.mock.calls[0]![1].aborted).toBe(true);
  if (scenario === "replaced read") {
    expect(lookup.mock.calls[1]![1].aborted).toBe(false);
    rejectInitial(new DOMException("The operation was aborted", "AbortError"));
    await tick();
    expect(fixture.loupe.error).toBe("");
    resolve(refreshed);
    await refresh;
    await waitFor(() => expect(fixture.loupe.photo).toEqual(refreshed));
    expect(lookup).toHaveBeenCalledTimes(2);
  } else if (scenario === "lookup failure") {
    const cause = new Error("Temporary lookup failure");
    fixture.reject(cause);
    await expect(refresh).rejects.toBe(cause);
  } else if (scenario === "removed then restored") {
    fixture.reject(new APIError("No longer available", 404, "not_found"));
    await refresh;
    await waitFor(() => expect(fixture.loupe.error).toBe("No longer available"));
    expect(fixture.loupe.photo).toBeUndefined();
    fixture.rows = [{ ...photo(1), revision: 3, name: "Restored photo.jpg" }];
    lookup.mockClear();
    await view.component.refresh({
      assets: new Map([["photo-1", 3]]),
      albums: new Map(),
      refetch: false,
    });
    expect(fixture.loupe.photo).toEqual(fixture.rows[0]);
    expect(fixture.loupe.error).toBe("");
    expect(lookup).not.toHaveBeenCalled();
  } else if (scenario === "manual refresh") {
    expect(fixture.refreshRows).toHaveBeenCalledExactlyOnceWith(expect.any(Function), false);
    resolve(refreshed);
    await refresh;
    expect(await screen.findByRole("dialog", { name: `Photo viewer, ${refreshed.name}` })).toBe(
      dialog,
    );
    finishInitial(old);
    await tick();
    expect(screen.getByRole("dialog", { name: `Photo viewer, ${refreshed.name}` })).toBe(dialog);
    expect(fixture.loupe.photo).toEqual(refreshed);
  } else {
    const cause = new DOMException("Photo details timed out", "TimeoutError");
    const failed = expect(refresh).rejects.toBe(cause);
    deadline.abort(cause);
    resolve(refreshed);
    await failed;
    expect(fixture.loupe.error).toBe("Photo details timed out.");
    expect(fixture.loupe.reloading).toBe(false);
    vi.mocked(AbortSignal.timeout).mockReturnValue(new AbortController().signal);
    fixture.responses.mockResolvedValue(refreshed);
    await view.component.refresh({
      assets: new Map([["photo-1", 2]]),
      albums: new Map(),
      refetch: false,
    });
    await waitFor(() => expect(fixture.loupe.photo).toEqual(refreshed));
    expect(fixture.loupe.error).toBe("");
  }
  expectActiveViewerLookups(fixture);
  view.unmount();
});
