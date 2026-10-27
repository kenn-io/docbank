import * as actions from "./action-events.js";
import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { tick } from "svelte";
import { createShortcutManager } from "@kenn-io/kit-ui";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import PhotosWorkspace from "./PhotosWorkspace.svelte";
import HiddenSetupModal from "./HiddenSetupModal.svelte";
import { HiddenAccess } from "./hiddenAccess.svelte.js";
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
  photoTimelineCounts,
  stubPhotoTimeline,
  touchTap,
} from "./photo-test-fixtures.js";
import type { Query, QuerySort } from "./query.js";
import * as snapshots from "./snapshots.js";
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

vi.mock("./action-events.js", () => ({
  reportAction: vi.fn(),
  reportSearch: vi.fn(),
  reportFailure: vi.fn(),
}));

const tiles = { tile_url: "", host: "", attribution: "", max_zoom: 19 };

beforeEach(() => {
  localStorage.setItem("docbank.photoMetadata", "closed");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  stubPhotoTimeline();
});

afterEach(() => {
  cleanup();
  map.onvisible = undefined;
  map.props = undefined;
  history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
  vi.mocked(actions.reportAction).mockClear();
  vi.mocked(actions.reportSearch).mockClear();
  vi.mocked(actions.reportFailure).mockClear();
  vi.restoreAllMocks();
  localStorage.clear();
  new Photos("scoped", vi.fn()).setDensity("comfortable");
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

it.each([false, true])(
  "preserves subsequent scrolling when an interrupted refresh resumes after remount, map=%s",
  async (mapMode) => {
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
      this: HTMLElement,
    ) {
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
    });
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
    await fireEvent.click(screen.getByRole("button", { name: "More photo actions" }));
    await fireEvent.click(screen.getByRole("menuitem", { name: "Refresh previews" }));
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

it("disables filters until a delayed Hide finishes", async () => {
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
  photos.query = { ...photos.query, text: "Canon" };
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
  await fireEvent.click(screen.getByRole("button", { name: "Filters" }));
  const controls = [
    screen.getByRole("button", { name: "Clear filters" }),
    screen.getByRole("button", { name: "Canon, 1 photos" }),
  ];
  const pending = photos.setHidden("photo-1");
  await waitFor(() => {
    for (const control of controls) expect(control.hasAttribute("disabled")).toBe(true);
  });
  finish(Response.json({ id: "photo-1", revision: 2 }));
  await pending;
  await waitFor(() => expect(controls[0].hasAttribute("disabled")).toBe(false));
  photos.dispose();
  await cache.dispose();
});

it("keeps empty-library guidance when clearing filters", async () => {
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
  await fireEvent.click(screen.getByRole("button", { name: "Filters" }));
  await screen.findByText("Your photo library is empty");
  expect(
    await screen.findByText(
      /Choose a folder or files with Add photos\. Large camera cards on the daemon's own disk import faster with docbank photos import\./,
    ),
  ).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Canon, 1 photos" }));
  await waitFor(() => expect(photos.loading).toBe(false));
  await fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
  await waitFor(() => expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull());
  await screen.findByText("Your photo library is empty");

  await photos.setQuery({ ...photos.query, filters: { iso_min: 0 } });
  await screen.findByText("No matching photos");
  await fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
  await screen.findByText("Your photo library is empty");
  expect(photos.query.filters).toEqual({});
  expect(
    screen.getByText(
      /Choose a folder or files with Add photos\. Large camera cards on the daemon's own disk import faster with docbank photos import\./,
    ),
  ).toBeTruthy();
  photos.dispose();
  await cache.dispose();
});

it("keeps failed confirmation visible and closes after cancellation or success", async () => {
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
  // The reloaded list shows before the failed move finishes; a new move waits for that.
  await waitFor(() => expect(photos.trashing).toBe(false));
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

it("renders surviving photos beside collapsed stacks after a failed refresh", async () => {
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const photos = new Photos("scoped", vi.fn());
  const otherStack = { asset_ids: ["photo-3", "photo-4"], representative_id: "photo-3" };
  photos.items = [photo(2), ...[photo(3), photo(4)].map((row) => ({ ...row, stack: otherStack }))];
  photos.total = 3;
  photos.started = true;
  photos.error = "Refresh unavailable";
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache });
  await screen.findByRole("button", { name: "Select Photo 2.jpg" });
  expect(screen.getByRole("button", { name: "Select Photo 3.jpg" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Select Photo 4.jpg" })).toBeNull();
  photos.dispose();
  await cache.dispose();
});

it("does not write the user's scroll position back to the grid", async () => {
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

it("keeps loaded photos visible on paging failure and selects with touch checkboxes", async () => {
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
  expect(screen.getByRole("button", { name: "Select Photo 1.jpg" })).toBeTruthy();
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  expect(await screen.findByText("1 selected photo")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Select loaded photos" }));
  expect(screen.getByText("2 selected photos")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await screen.findByRole("button", { name: "Select Photo 3.jpg" });
  await waitFor(() => expect(photos.loading).toBe(false));
  fetcher.mockImplementationOnce(() => new Promise(() => {}));
  photos.cursor = "manual-page";
  await fireEvent.click(await screen.findByRole("button", { name: "Load more" }));
  view.unmount();
  expect(fetcher.mock.calls[3][1].signal.aborted).toBe(true);
  photos.dispose();
  await cache.dispose();
});

it("retains visible cells and pending previews across a prepend larger than the window", async () => {
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement,
  ) {
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
  });
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

it("clears selection with Escape from focused photo controls while honoring shortcut suppression", async () => {
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
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement,
  ) {
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
  });
  const photos = new Photos("scoped", vi.fn());
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  if (relevance)
    photos.query = {
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
    await screen.findByText("60 photos");
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
    "matchMedia",
    vi.fn(() => Object.assign(new EventTarget(), { matches: true })),
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
    vi.fn(() => Object.assign(new EventTarget(), { matches: false })),
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
      return true;
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
      return true;
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

it("shows a suggestion published after the inspector opened once previews refresh", async () => {
  const photos = workspaceWithPreparedPhoto();
  const prepared = vi
    .spyOn(api, "preparePhotoAuthored")
    .mockResolvedValueOnce(photoPreparation([preparedPhoto(1)]))
    .mockResolvedValueOnce({
      ...photoPreparation([preparedPhoto(1)]),
      suggestions: [
        {
          suggestion_id: "suggestion",
          content_version_id: "version",
          target: "file-1",
          asset_id: "photo-1",
          asset_revision: 1,
          source_revision: 1,
          node_revision: 1,
          value: { caption: "A harbor.", tags: [], visible_text: "" },
        },
      ],
    });
  stubPhotoFetch(async () => Response.json({ items: [photo(1)], total: 1 }));
  render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
  });
  photos.authored.stage({ rating: 3 });
  await fireEvent.click(await screen.findByRole("button", { name: "Metadata" }));
  await photos.authored.reload();
  expect(screen.queryByRole("region", { name: "Suggested photo metadata" })).toBeNull();

  await fireEvent.click(screen.getByRole("button", { name: "More photo actions" }));
  await fireEvent.click(screen.getByRole("menuitem", { name: "Refresh previews" }));
  await screen.findByRole("region", { name: "Suggested photo metadata" });
  expect(prepared).toHaveBeenCalledTimes(2);
  expect(photos.authored.patch).toEqual({ rating: 3 });
  photos.dispose();
});

function workspaceWithPreparedPhoto(target = preparedPhoto(1), projection?: "map") {
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const photos = new Photos("scoped", vi.fn(), false, photoQuery, { projection });
  photos.started = true;
  photos.items = [projection ? mapPhoto(1) : photo(1)];
  photos.authored.targets = [target];
  return photos;
}

it("preserves the caption draft when creating a keyword and applies both together", async () => {
  Element.prototype.scrollIntoView = vi.fn();
  const photos = workspaceWithPreparedPhoto();
  photos.selectLoaded();
  const tag = { id: "new-keyword", name: "Harbor", revision: 1, assignment_count: 0 };
  vi.spyOn(api, "createTag").mockResolvedValue(tag);
  const edit = vi
    .spyOn(api, "editPhotoAuthored")
    .mockResolvedValue({ receipt_id: "", before: [], after: [] });
  stubPhotoFetch(vi.fn());
  render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
  });
  await fireEvent.click(screen.getByRole("button", { name: "Metadata" }));
  const caption = await screen.findByRole("textbox", { name: "Caption" });
  await fireEvent.input(caption, { target: { value: "Unfinished caption" } });
  await fireEvent.input(screen.getByRole("combobox", { name: /Keyword/ }), {
    target: { value: "Harbor" },
  });
  await fireEvent.click(await screen.findByRole("option", { name: "Create “Harbor”" }));
  await screen.findByRole("button", { name: "Apply" });
  expect((caption as HTMLTextAreaElement).value).toBe("Unfinished caption");
  await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  await waitFor(() => expect(edit).toHaveBeenCalledOnce());
  expect(edit.mock.calls[0]![0]).toMatchObject({
    patch: { caption: "Unfinished caption" },
    keywords: [{ tag_id: tag.id, assign: true }],
  });
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
  const view = render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
  });
  photos.authored.stage({ rating: 0 });
  await fireEvent.click(await screen.findByRole("button", { name: "Metadata" }));
  await fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  const notify = edit.mock.calls[0]![3];
  await view.rerender({ photoID: "photo-1" });
  await waitFor(() => expect(lookup).toHaveBeenCalledOnce());
  await fireEvent.click(screen.getByRole("button", { name: "More photo actions" }));
  await fireEvent.click(screen.getByRole("menuitem", { name: "Refresh previews" }));
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
    // The viewer keeps its fit image while the reloaded photo prepares, so count that photo's prepares.
    await waitFor(() =>
      expect(preview.mock.calls.filter(([row]) => row.asset_id === "photo-1")).toHaveLength(
        preparations + 1,
      ),
    );
    await screen.findByText("Fit preview");
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

it("explains the relevance limit and hides it for complete results or capture dates", async () => {
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

it.each([false, true])("keeps RAW after a refused flag, reload fails=%s", async (reloadFails) => {
  Element.prototype.scrollIntoView = vi.fn();
  const photos = workspaceWithPreparedPhoto();
  const prepared = photoPreparation([
    preparedPhoto(1),
    preparedPhoto(2, { asset_id: "photo-1", file_id: "raw", file_name: "Photo 1.raw" }),
  ]);
  prepared.assets[0]!.files[1]!.role = "raw";
  const prepare = vi.spyOn(api, "preparePhotoAuthored").mockResolvedValueOnce(prepared);
  if (reloadFails) prepare.mockRejectedValueOnce(new Error("Metadata reload failed"));
  else prepare.mockResolvedValue(prepared);
  const row = photo(1);
  let finishLookup!: (value: PhotoBrowseRow) => void;
  vi.spyOn(photos, "lookup")
    .mockResolvedValueOnce(row)
    .mockImplementationOnce(() => new Promise((resolve) => (finishLookup = resolve)));
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  stubPhotoFetch(vi.fn());
  const edit = vi
    .spyOn(api, "editPhotoAuthored")
    .mockRejectedValueOnce(new APIError("Photo changed", 412, "stale_revision"))
    .mockResolvedValue({ receipt_id: "", before: [], after: [] });
  const shortcuts = createShortcutManager();
  render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    photoID: row.asset_id,
    shortcuts,
  });
  const loupe = await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
  await fireEvent.click(within(loupe).getByRole("button", { name: "Metadata" }));
  await within(loupe).findByRole("list", { name: "Assigned keywords" });
  await fireEvent.click(within(loupe).getByRole("combobox", { name: /Edit members/ }));
  await fireEvent.click(screen.getByRole("option", { name: "RAW · Photo 1.raw" }));
  await fireEvent.click(within(loupe).getByRole("button", { name: "Metadata" }));
  expect(within(loupe).queryByRole("list", { name: "Assigned keywords" })).toBeNull();
  shortcuts.handleKeydown(new KeyboardEvent("keydown", { key: "p", cancelable: true }));
  await within(loupe).findByText(reloadFails ? "Metadata reload failed" : "Photo changed");
  expect(edit).toHaveBeenCalledOnce();
  expect(Object.values(edit.mock.calls[0]![0].targets)).toMatchObject([
    { file_id: "raw", content_version_id: "version-1" },
  ]);
  expect(photos.authored.fileID).toBe("raw");
  if (!reloadFails) {
    await waitFor(() => expect(prepare).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(photos.authored.loading).toBe(false));
    expect(within(loupe).getByText("Photo changed")).toBeTruthy();
  }
  finishLookup({ ...row, content_version_id: "replacement-version" });
  await waitFor(() =>
    expect(photos.items[0]).toMatchObject({ content_version_id: "replacement-version" }),
  );
  if (!reloadFails) {
    shortcuts.handleKeydown(new KeyboardEvent("keydown", { key: "5", cancelable: true }));
    await waitFor(() => expect(edit).toHaveBeenCalledTimes(2));
    expect(Object.values(edit.mock.calls[1]![0].targets)).toMatchObject([
      { file_id: "raw", content_version_id: "replacement-version" },
    ]);
  }
  photos.dispose();
});

it("blocks a loupe flag while the photo is loading", async () => {
  const photos = workspaceWithPreparedPhoto();
  const lookup = vi.spyOn(photos, "lookup").mockImplementation(() => new Promise(() => {}));
  stubPhotoFetch(vi.fn());
  const edit = vi
    .spyOn(api, "editPhotoAuthored")
    .mockResolvedValue({ receipt_id: "", before: [], after: [] });
  const shortcuts = createShortcutManager();
  render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    photoID: "photo-1",
    shortcuts,
  });
  await waitFor(() => expect(lookup).toHaveBeenCalledOnce());
  shortcuts.handleKeydown(new KeyboardEvent("keydown", { key: "p", cancelable: true }));
  await screen.findByText("Photo is loading. Try again when it finishes.");
  expect(edit).not.toHaveBeenCalled();
  photos.dispose();
});

it("keeps the Metadata draft after a refused loupe edit until Reload metadata", async () => {
  const photos = workspaceWithPreparedPhoto();
  const row = photo(1);
  const lookup = vi.spyOn(photos, "lookup").mockResolvedValue(row);
  const prepare = vi
    .spyOn(api, "preparePhotoAuthored")
    .mockResolvedValue(photoPreparation([preparedPhoto(1)]));
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  stubPhotoFetch(vi.fn());
  const edit = vi
    .spyOn(api, "editPhotoAuthored")
    .mockRejectedValueOnce(new APIError("Photo changed", 412, "stale_revision"))
    .mockResolvedValue({ receipt_id: "", before: [], after: [] });
  const shortcuts = createShortcutManager();
  render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    photoID: row.asset_id,
    shortcuts,
  });
  const loupe = await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
  await fireEvent.click(within(loupe).getByRole("button", { name: "Metadata" }));
  await within(loupe).findByRole("list", { name: "Assigned keywords" });
  prepare.mockClear();
  photos.authored.stage({ caption: "Draft caption" });
  shortcuts.handleKeydown(new KeyboardEvent("keydown", { key: "p", cancelable: true }));
  await within(loupe).findByText("Photo changed");
  await waitFor(() => expect(lookup).toHaveBeenCalledTimes(2));
  expect(prepare).not.toHaveBeenCalled();
  shortcuts.handleKeydown(new KeyboardEvent("keydown", { key: "5", cancelable: true }));
  await within(loupe).findByText("Reload metadata before editing.");
  expect(edit).toHaveBeenCalledOnce();
  await fireEvent.click(within(loupe).getByRole("button", { name: "Reload metadata" }));
  await waitFor(() => expect(prepare).toHaveBeenCalledOnce());
  await waitFor(() => expect(photos.authored.conflict).toBe(false));
  shortcuts.handleKeydown(new KeyboardEvent("keydown", { key: "5", cancelable: true }));
  await waitFor(() => expect(edit).toHaveBeenCalledTimes(2));
  expect(edit.mock.calls[1]![0].patch).toEqual({ caption: "Draft caption", rating: 5 });
  photos.dispose();
});

it("guards loupe navigation through the metadata editor", async () => {
  const photos = new Photos("scoped", vi.fn());
  photos.started = true;
  photos.items = [photo(1), photo(2)];
  vi.spyOn(photos, "lookup").mockImplementation(async (id) =>
    photo(Number(id.slice("photo-".length))),
  );
  const onphotochange = vi.fn().mockReturnValue(false);
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
  photos.authored.stage({ caption: "Draft caption" });
  await fireEvent.keyDown(viewer, { key: "ArrowRight" });
  expect(onphotochange).not.toHaveBeenCalled();
  const filmstrip = within(viewer).getByRole("navigation", { name: "Photos in current result" });
  await fireEvent.click(within(filmstrip).getByRole("button", { name: "Show Photo 2.jpg" }));
  expect(onphotochange).not.toHaveBeenCalled();
  expect(actions.reportAction).not.toHaveBeenCalled();
  photos.authored.reset();
  await fireEvent.click(within(filmstrip).getByRole("button", { name: "Show Photo 2.jpg" }));
  expect(onphotochange).toHaveBeenCalledWith("photo-2", "step");
  expect(actions.reportAction).not.toHaveBeenCalled();
  onphotochange.mockReturnValue(true);
  onphotochange.mockClear();
  await fireEvent.keyDown(viewer, { key: "ArrowRight" });
  expect(onphotochange).toHaveBeenCalledOnce();
  expect(onphotochange).toHaveBeenCalledWith("photo-2", "step");
  expect(actions.reportAction).not.toHaveBeenCalled();
  view.unmount();
  photos.dispose();
});

it.each([
  { state: "metadata loading", stacked: false, collapsed: false },
  { state: "stack metadata loading", stacked: true, collapsed: false },
  { state: "details collapsed", stacked: false, collapsed: true },
])("opens the viewer with details on double-click with $state", async ({ stacked, collapsed }) => {
  if (collapsed) localStorage.setItem("docbank.photoMetadata", "closed");
  else localStorage.removeItem("docbank.photoMetadata");
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const prepare = vi
    .spyOn(api, "preparePhotoAuthored")
    .mockImplementation((scope) =>
      collapsed || (stacked && !scope.stack)
        ? Promise.resolve(photoPreparation([preparedPhoto(1), preparedPhoto(2)]))
        : new Promise(() => {}),
    );
  const photos = new Photos("scoped", vi.fn());
  photos.started = true;
  photos.items = stacked
    ? [
        {
          ...photo(1),
          stack: { asset_ids: ["photo-1", "photo-2"], representative_id: "photo-1" },
        },
        photo(2),
      ]
    : [photo(1)];
  photos.total = 1;
  const onphotochange = vi.fn().mockReturnValue(true);
  vi.spyOn(photos, "lookup").mockResolvedValue(photo(1));
  const view = render(PhotosWorkspace, {
    photos,
    onphotochange,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
  });
  const image = await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  if (collapsed) {
    expect(screen.queryByRole("complementary", { name: "Photo details" })).toBeNull();
  } else {
    await fireEvent.click(image);
    await waitFor(() => expect(prepare).toHaveBeenCalled());
    await waitFor(() =>
      expect(stacked ? photos.authored.stackLoading : photos.authored.loading).toBe(true),
    );
  }
  await fireEvent.doubleClick(image);
  expect(onphotochange).toHaveBeenCalledWith("photo-1", "open");
  await view.rerender({ photoID: "photo-1" });
  const viewer = await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
  expect(within(viewer).getByRole("complementary", { name: "Photo details" })).toBeTruthy();
  expect(photos.authored.patch).toEqual({});
  photos.dispose();
});

it("puts Add photos, the more menu, and the details toggle last, in that order", () => {
  const photos = workspaceWithPreparedPhoto();
  render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    onadd: vi.fn(),
  });
  const add = screen.getByRole("button", { name: "Add photos" });
  const more = screen.getByRole("button", { name: "More photo actions" });
  const details = screen.getByRole("button", { name: "Metadata" });
  const follows = (a: Element, b: Element) =>
    Boolean(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING);
  expect(follows(add, more)).toBe(true);
  expect(follows(more, details)).toBe(true);
  photos.dispose();
});

it("opens the details panel at the width saved for Photos", () => {
  localStorage.setItem("docbank.photoMetadata", "open");
  localStorage.setItem("docbank.panelWidth.photos", "400");
  const photos = workspaceWithPreparedPhoto();
  render(PhotosWorkspace, { photos, cache: { get: vi.fn() } as unknown as PhotoPreviewCache });
  const panel = screen.getByRole("complementary", { name: "Photo details" });
  expect(panel.style.getPropertyValue("--details-width")).toBe("min(400px, 50vw)");
  expect(screen.getByRole("separator", { name: "Resize photo details" })).toBeTruthy();
  photos.dispose();
});

it("starts metadata open and remembers the toggle across the library, viewer, and remounts", async () => {
  localStorage.removeItem("docbank.photoMetadata");
  const photos = workspaceWithPreparedPhoto();
  photos.selection = { selectedIDs: new Set(["photo-1"]), anchorID: "photo-1" };
  photos.authored.inspectedID = "";
  vi.spyOn(photos, "lookup").mockResolvedValue(photo(1));
  vi.spyOn(api, "preparePhotoAuthored").mockResolvedValue(photoPreparation([preparedPhoto(1)]));
  const props = { photos, cache: { get: vi.fn() } as unknown as PhotoPreviewCache, photoID: "" };
  const view = render(PhotosWorkspace, props);
  expect(screen.getByRole("complementary", { name: "Photo details" })).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Metadata" }));
  expect(screen.queryByRole("complementary", { name: "Photo details" })).toBeNull();
  await view.rerender({ ...props, photoID: "photo-1" });
  const viewer = await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
  expect(within(viewer).queryByRole("complementary", { name: "Photo details" })).toBeNull();
  await view.rerender(props);
  expect(screen.queryByRole("complementary", { name: "Photo details" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Metadata" }));
  expect(screen.getByRole("complementary", { name: "Photo details" })).toBeTruthy();
  await fireEvent.keyDown(window, { key: "i" });
  expect(screen.queryByRole("complementary", { name: "Photo details" })).toBeNull();
  view.unmount();
  render(PhotosWorkspace, props);
  expect(screen.queryByRole("complementary", { name: "Photo details" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Metadata" }));
  expect(screen.getByRole("complementary", { name: "Photo details" })).toBeTruthy();
  photos.dispose();
});

it.each(["library", "filters", "album"])(
  "shows details only while photos are selected in %s",
  async (view) => {
    localStorage.setItem("docbank.photoMetadata", "open");
    const photos = workspaceWithPreparedPhoto();
    photos.authored.reset();
    stubPhotoFetch(vi.fn());
    vi.spyOn(api, "preparePhotoAuthored").mockResolvedValue(photoPreparation([preparedPhoto(1)]));
    photos.filtersOpen = view === "filters";
    render(PhotosWorkspace, {
      photos,
      cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
      albumID: view === "album" ? "album-1" : "",
    });
    expect(screen.queryByRole("complementary", { name: "Photo details" })).toBeNull();
    expect(screen.queryByRole("separator", { name: "Resize photo details" })).toBeNull();
    await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
    expect(screen.getByRole("complementary", { name: "Photo details" })).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Clear selection" }));
    expect(screen.queryByRole("complementary", { name: "Photo details" })).toBeNull();
    expect(screen.queryByRole("separator", { name: "Resize photo details" })).toBeNull();
    photos.dispose();
  },
);

it("keeps the metadata toggle usable when browser storage is blocked", async () => {
  vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
    throw new Error("Blocked");
  });
  vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
    throw new Error("Blocked");
  });
  const photos = workspaceWithPreparedPhoto();
  render(PhotosWorkspace, { photos, cache: { get: vi.fn() } as unknown as PhotoPreviewCache });
  expect(screen.getByRole("complementary", { name: "Photo details" })).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Metadata" }));
  expect(screen.queryByRole("complementary", { name: "Photo details" })).toBeNull();
  photos.dispose();
});

it("keeps a Library reload error through a list refresh", async () => {
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
  const alert = screen.getByRole("alert");
  expect(alert.closest(".library-grid")).toBeTruthy();
  expect(
    document.querySelector(".photo-bar")!.compareDocumentPosition(alert) &
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
  const note = `Showing the newest ${mapPhotoLimit.toLocaleString()} of ${(mapPhotoLimit + 1000).toLocaleString()} located photos. Narrow your search to see the rest.`;
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

function openMapViewerSetup(mapMode = true, hidden = false) {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const photos = new Photos("scoped", vi.fn(), hidden, photoQuery, {
    projection: mapMode ? "map" : undefined,
  });
  photos.items = mapMode ? [mapPhoto(1), mapPhoto(2)] : [photo(1), photo(2)];
  photos.total = 2;
  photos.started = true;
  vi.spyOn(photos, "resume").mockResolvedValue();
  const lookup = vi.spyOn(photos, "lookup").mockResolvedValue(photo(1));
  const preview = vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  const props = {
    photos,
    mapMode,
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
it.each([true, false])(
  "reports an open denied access only outside Hidden, hidden=%s",
  async (hidden) => {
    const { photos, lookup, props, view } = openMapViewerSetup(true, hidden);
    lookup.mockRestore();
    const onauth = vi.spyOn(photos, "onauthfailure");
    stubPhotoFetch(
      vi
        .fn()
        .mockResolvedValue(
          Response.json(
            { code: hidden ? "hidden_locked" : "forbidden", detail: "Access denied" },
            { status: 403 },
          ),
        ),
    );
    props.onphotochange.mockReturnValue(true);
    await vi.dynamicImportSettled();
    map.onvisible!([mapPhoto(1)]);
    await fireEvent.click(await screen.findByRole("button", { name: "Open Photo 1.jpg" }));
    await view.rerender({ ...props, photoID: "photo-1" });
    await screen.findByText("Access denied");
    if (hidden) {
      expect(onauth).toHaveBeenCalledExactlyOnceWith(expect.objectContaining({ status: 403 }));
      expect(actions.reportFailure).not.toHaveBeenCalled();
    } else {
      expect(onauth).not.toHaveBeenCalled();
      expect(actions.reportFailure).toHaveBeenCalledExactlyOnceWith(
        "scoped",
        "open",
        expect.objectContaining({ status: 403 }),
      );
    }
    expect(actions.reportAction).not.toHaveBeenCalled();
    photos.dispose();
  },
);
it.each(
  [false, true].flatMap((mapMode) =>
    ["success", "failure", "closed", "stepped"].map((outcome) => ({ mapMode, outcome })),
  ),
)(
  "reports explicit photo opens once, map=$mapMode outcome=$outcome",
  async ({ mapMode, outcome }) => {
    const { photos, lookup, props, view } = openMapViewerSetup(mapMode);
    props.onphotochange.mockReturnValue(true);
    let resolve!: (row: PhotoBrowseRow) => void;
    let reject!: (cause: Error) => void;
    lookup.mockImplementation(
      () =>
        new Promise((done, fail) => {
          resolve = done;
          reject = fail;
        }),
    );
    await vi.dynamicImportSettled();
    if (mapMode) map.onvisible!([mapPhoto(1)]);
    await fireEvent.click(await screen.findByRole("button", { name: "Open Photo 1.jpg" }));
    // A grid refresh can remove the clicked row before the viewer receives its id.
    if (!mapMode) photos.items = [];
    await view.rerender({ ...props, photoID: "photo-1" });
    expect(lookup).toHaveBeenCalledExactlyOnceWith("photo-1", expect.any(AbortSignal));
    expect(actions.reportAction).not.toHaveBeenCalled();
    expect(actions.reportFailure).not.toHaveBeenCalled();
    if (outcome === "closed") {
      await fireEvent.click(await screen.findByRole("button", { name: "Close photo viewer" }));
      await view.rerender({ ...props, photoID: "" });
    } else if (outcome === "stepped") {
      photos.items = [photo(2)];
      await view.rerender({ ...props, photoID: "photo-2" });
    }
    await act(async () => {
      if (outcome === "success") resolve(photo(1));
      else reject(new APIError("This photo is no longer in this view.", 404, "not_found"));
    });
    if (outcome === "success")
      expect(actions.reportAction).toHaveBeenCalledExactlyOnceWith("scoped", "document_opened");
    else expect(actions.reportAction).not.toHaveBeenCalled();
    if (outcome === "failure")
      expect(actions.reportFailure).toHaveBeenCalledExactlyOnceWith(
        "scoped",
        "open",
        expect.objectContaining({ status: 404 }),
      );
    else expect(actions.reportFailure).not.toHaveBeenCalled();
    photos.dispose();
  },
);

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

it("reconciles a Map list refresh after its detail generation changes", async () => {
  Object.defineProperty(URL, "createObjectURL", {
    configurable: true,
    value: vi.fn(() => "blob:map-preview"),
  });
  Object.defineProperty(URL, "revokeObjectURL", { configurable: true, value: vi.fn() });
  Object.defineProperty(HTMLImageElement.prototype, "decode", {
    configurable: true,
    value: vi.fn(async () => {}),
  });
  const setup = openMapViewerSetup();
  const { photos, lookup, preview, props } = setup;
  photos.items = [mapPhoto(1)];
  const row = photo(1);
  row.previews.fit = { state: "ready", generation_id: "fit-1" };
  lookup.mockResolvedValue(row);
  preview.mockImplementation(async (row) => row.previews.fit);
  const get = vi.fn(async () => new Blob(["synthetic-photo"]));
  props.cache.get = get;
  await openMapViewer(setup);
  await screen.findByText("Fit preview");
  lookup.mockResolvedValue({
    ...row,
    previews: { ...row.previews, fit: { state: "ready", generation_id: "fit-2" } },
  });
  await act(() => {
    photos.items = [mapPhoto(1)];
  });
  await waitFor(() =>
    expect(get).toHaveBeenCalledWith("photo-1", "fit-2", expect.any(AbortSignal), false),
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
it("shows the Map count without Library controls", async () => {
  const { photos } = mapRecoverySetup();
  photos.total = 1;
  await vi.dynamicImportSettled();
  await fireEvent.click(screen.getByRole("button", { name: "More photo actions" }));
  const menu = screen.getByRole("menu");
  expect(within(menu).getByRole("menuitem", { name: "Refresh previews" })).toBeTruthy();
  expect(within(menu).queryByRole("menuitem", { name: "Trash rejected photos" })).toBeNull();
  expect(within(menu).queryByRole("menuitem", { name: /^Cull/ })).toBeNull();
  await fireEvent.keyDown(document, { key: "Escape" });
  expect(document.querySelector(".photo-stack-controls")).toBeNull();
  await screen.findByText("1 located photo");
  photos.dispose();
});

it("shows the Map viewport count as empty before loading, loading, then zero", async () => {
  const { photos } = mapRecoverySetup();
  photos.started = false;
  photos.error = "";
  await vi.dynamicImportSettled();
  await tick();
  expect(document.querySelector(".photo-count-value")?.textContent).toBe("");
  photos.loading = true;
  await tick();
  expect(document.querySelector(".photo-count-value")?.textContent).toBe("Loading…");
  photos.started = true;
  photos.loading = false;
  photos.total = 20;
  map.onvisible!([]);
  await tick();
  expect(document.querySelector(".photo-count-value")?.textContent).toBe("0 photos");
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
  await fireEvent.click(screen.getByRole("button", { name: "More photo actions" }));
  expect(
    screen
      .getByRole("menuitem", { name: "Export all (limit 16 photos)" })
      .getAttribute("aria-disabled"),
  ).toBe("true");
  await fireEvent.keyDown(document, { key: "Escape" });
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
  await fireEvent.click(screen.getByRole("button", { name: "More photo actions" }));
  await fireEvent.click(screen.getByRole("menuitem", { name: "Export all 3 photos" }));
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
  await fireEvent.click(screen.getByRole("button", { name: "More photo actions" }));
  const menu = screen.getByRole("menu");
  expect(within(menu).queryByRole("menuitem", { name: /^Export/ })).toBeNull();
  await fireEvent.keyDown(document, { key: "Escape" });
  await fireEvent.click(screen.getByRole("button", { name: "Export selection" }));
  expect(onexport).toHaveBeenCalledExactlyOnceWith({
    label: "Selected photos",
    total: 2,
    photos: { query: photoQuery, asset_ids: ["photo-1", "photo-2"], hidden: false },
  });
  photos.dispose();
});

it("exports the prepared metadata selection after the query grows beyond the export cap", async () => {
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
    vi.spyOn(api, "getPhotoSettings").mockResolvedValue({ revision: 1, burst_gap_seconds: 2 });
    vi.spyOn(api, "groupPhotoStacks").mockResolvedValue({
      groups: [],
      unavailable_ids: [],
      burst_gap_seconds: 2,
    });
    vi.stubGlobal("indexedDB", new IDBFactory());
    vi.stubGlobal("IDBKeyRange", IDBKeyRange);
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
    vi.spyOn(photos, "lookup").mockImplementation(async (id) =>
      rows.find((row) => row.asset_id === id)!,
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
    await fireEvent.click(within(cull).getByRole("button", { name: "Pick" }));
    await within(cull).findByText("Photo 2.jpg");
    expect([...photos.selectedIDs]).toEqual(ids);
    await fireEvent.click(within(cull).getByRole("button", { name: "Exit cull" }));
    await waitFor(() => expect([...photos.selectedIDs]).toEqual(oneUnavailable ? [ids[1]!] : ids));
    photos.dispose();
    await cache.dispose();
  },
);

it("reloads durable culls and retains their draft only for the same vault", async () => {
  const rows = [photo(1), photo(2)];
  const photos = new Photos("scoped", vi.fn());
  photos.items = rows;
  photos.started = true;
  vi.spyOn(photos, "lookup").mockImplementation(async (id) =>
    rows.find((row) => row.asset_id === id)!,
  );
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  let durableID = rows[0]!.asset_id;
  vi.spyOn(PhotoCull.prototype, "probe").mockImplementation(async function (this: PhotoCull) {
    this.saved = true;
  });
  const restore = vi.spyOn(PhotoCull.prototype, "restore").mockImplementation(async function (
    this: PhotoCull,
  ) {
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
  vi.spyOn(PhotoCull.prototype, "restore").mockResolvedValue(false);
  vi.spyOn(PhotoCull.prototype, "flush").mockResolvedValue();
  const rows = [photo(1), photo(2)];
  const ids = rows.map((row) => row.asset_id);
  const photos = new Photos("scoped", vi.fn());
  photos.items = rows;
  photos.total = rows.length;
  photos.started = true;
  vi.spyOn(photos, "lookup").mockImplementation(async (id) =>
    rows.find((row) => row.asset_id === id)!,
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
      await tick();
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

it.each(
  [1, 2].flatMap((formatVersion) =>
    ["Cancel", "Replace"].map((answer) => ({ formatVersion, answer })),
  ),
)(
  "$answer in the capture modal preserves or replaces saved format $formatVersion",
  async ({ answer, formatVersion }) => {
    vi.stubGlobal("indexedDB", new IDBFactory());
    vi.stubGlobal("IDBKeyRange", IDBKeyRange);
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
    const photos = new Photos("scoped", vi.fn());
    photos.items = [photo(1)];
    photos.total = 1;
    photos.started = true;
    const capture = vi
      .spyOn(api, "createWorkspaceQuery")
      .mockResolvedValue(Response.json(await snapshotEnvelope(photos.query, [])));
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
      expect(await photos.cull!.restore()).toBe(false);
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
      } else expect(await store.load()).toEqual(original);
    } else {
      await screen.findByRole("dialog", { name: "Cull photos" });
      expect(capture).toHaveBeenCalledOnce();
      await photos.cull!.flush();
      expect((await store.load())?.ids).toEqual([]);
    }
    photos.dispose();
  },
);

it.each(["resolution", "background import"])(
  "refreshes the workspace after an XMP %s",
  async (change) => {
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
    const before = preparedPhoto(1, { file_id: "file-1", xmp: { state: "pending" } });
    const photos = workspaceWithPreparedPhoto(before);
    photos.total = 1;
    photos.filtersOpen = true;
    photos.query = { ...photos.query, text: "Harbor" };
    vi.spyOn(api, "preparePhotoAuthored").mockResolvedValue(photoPreparation([before]));
    const lookup = vi.spyOn(photos, "lookup").mockResolvedValue(photo(1));
    vi.spyOn(photos, "preview").mockResolvedValue({ state: "ready", generation_id: "fit" });
    const receipt = { receipt_id: "xmp-import", before: [], after: [] } as PhotoAuthoredReceipt;
    const facet = {
      dimension: "camera",
      available: true,
      reason: "",
      total: 2,
      values: [{ key: "Canon", label: "Canon", count: 2, selected: false }],
    };
    vi.stubGlobal("fetch", async (url: string, init: RequestInit) => {
      if (url.endsWith("/photos/assets/query"))
        return Response.json(
          change === "resolution"
            ? {
                items: [{ ...photo(1), rating: 4, rotation: 90 }],
                total: 1,
                facets: [],
              }
            : {
                items: [{ ...photo(1), rating: 4, rotation: 90 }, photo(2)],
                total: 2,
                facets: [facet],
              },
        );
      const metadata = photoMetadataResponse(url, init);
      if (metadata) return metadata;
      throw new Error(`Unexpected request ${url}`);
    });
    const view = render(PhotosWorkspace, {
      photos,
      photoID: change === "background import" ? "photo-1" : undefined,
      cache: { get: vi.fn(async () => new Blob(["synthetic"])) } as unknown as PhotoPreviewCache,
    });
    const viewer = () => view.container.querySelector(".photo-viewport img");
    if (change === "resolution") {
      await tick();
      await photos.authored.onXMPChanged!(receipt);
      await waitFor(() => expect(photos.items[0]?.rotation).toBe(90));
      expect(photos.items).toEqual([{ ...photo(1), rating: 4, rotation: 90 }]);
      expect(photos.total).toBe(1);
      expect(photos.facets).toEqual([]);
    } else {
      await waitFor(() => expect(viewer()).toBeTruthy());
      expect(viewer()!.getAttribute("style") ?? "").not.toContain("rotate(90deg)");
      // The sync worker rotates the photo after the viewer loaded it.
      lookup.mockClear();
      lookup.mockResolvedValue({ ...photo(1), rating: 4, rotation: 90 });
      await photos.authored.onXMPChanged!();
      await waitFor(() => {
        expect(photos.items[0]?.rotation).toBe(90);
        expect(photos.items[0]).toMatchObject({ rating: 4 });
        expect(photos.total).toBe(2);
        expect(photos.facets).toEqual([facet]);
      });
      await waitFor(() => expect(lookup).toHaveBeenCalled());
      await waitFor(() => expect(viewer()!.getAttribute("style")).toContain("rotate(90deg)"));
    }
    photos.dispose();
  },
);

it("toggles the Filters panel", async () => {
  stubPhotoFetch(() => Promise.resolve(Response.json({ items: [photo(1)], total: 1, facets: [] })));
  const photos = new Photos("scoped", vi.fn());
  photos.started = true;
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache });
  const toggle = screen.getByRole("button", { name: "Filters" });
  expect(toggle.getAttribute("aria-expanded")).toBe("false");
  expect(screen.queryByRole("complementary", { name: "Photo facets" })).toBeNull();
  await fireEvent.click(toggle);
  expect(toggle.getAttribute("aria-expanded")).toBe("true");
  expect(screen.getByRole("complementary", { name: "Photo facets" })).toBeTruthy();
  const panelID = toggle.getAttribute("aria-controls");
  expect(panelID).toBeTruthy();
  expect(
    document
      .getElementById(panelID!)
      ?.contains(screen.getByRole("complementary", { name: "Photo facets" })),
  ).toBe(true);
  await fireEvent.click(toggle);
  expect(toggle.getAttribute("aria-expanded")).toBe("false");
  expect(screen.queryByRole("complementary", { name: "Photo facets" })).toBeNull();
  photos.dispose();
  await cache.dispose();
});

it("edits and clears a saved capture range in the Library Filters panel", async () => {
  vi.stubGlobal("fetch", async (url: string, init: RequestInit) => {
    const metadata = photoMetadataResponse(url, init);
    if (metadata) return metadata;
    const request = JSON.parse(String(init?.body));
    return Response.json({
      items: [photo(1, request.query.filters.capture_after)],
      total: 1,
      facets: request.facets?.includes("year")
        ? [
            {
              dimension: "year",
              available: true,
              total: 2,
              missing: 0,
              other: 0,
              values: [
                {
                  key: "2024",
                  label: "2024",
                  count: 1,
                  selected: request.query.filters.capture_after === "2024-01-01",
                },
                {
                  key: "2023",
                  label: "2023",
                  count: 1,
                  selected: request.query.filters.capture_after === "2023-01-01",
                },
              ],
            },
          ]
        : [],
    });
  });
  const photos = new Photos("scoped", vi.fn(), false, {
    ...photoQuery,
    filters: {
      capture_after: "2024-01-01",
      capture_before: "2025-01-01",
    },
  });
  await photos.loadMore();
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache });
  await fireEvent.click(
    screen.getByRole("button", {
      name: "Filters, 1 active filter",
    }),
  );
  // Year selection and clearing also cover FacetSidebar, which has no separate Year test.
  const year = await screen.findByRole("group", { name: "Year facet" });
  await fireEvent.click(within(year).getByRole("button", { name: "2023, 1 photos" }));
  expect(photos.query.filters.capture_after).toBe("2023-01-01");
  expect(photos.query.filters.capture_before).toBe("2024-01-01");
  await waitFor(() => expect(photos.loading).toBe(false));
  await fireEvent.click(await screen.findByRole("button", { name: "2023, 1 photos, selected" }));
  expect(photos.query.filters.capture_after).toBeUndefined();
  expect(photos.query.filters.capture_before).toBeUndefined();
  photos.dispose();
  await cache.dispose();
});

it("retries timeline counts with no loaded photos", async () => {
  const { photos, cache } = railLibrary();
  photos.items = [];
  photos.total = 0;
  photos.cursor = undefined;
  photos.timeline = undefined;
  photos.timelineError = "Timeline counts are unavailable.";
  vi.mocked(snapshots.fetchPhotoTimeline).mockResolvedValue(photoTimelineCounts([]));
  render(PhotosWorkspace, { photos, cache });
  const rail = await screen.findByRole("navigation", { name: "Timeline" });
  expect(within(rail).getByText("Timeline counts are unavailable.")).toBeTruthy();
  await fireEvent.click(within(rail).getByRole("button", { name: "Retry timeline" }));
  await waitFor(() => expect(photos.timelineError).toBe(""));
  await screen.findByText("Your photo library is empty");
  photos.dispose();
  await cache.dispose();
});

it.each(["counts", "error", "pending"])(
  "shows timeline %s when the initial photo listing fails",
  async (state) => {
    const { photos, cache } = railLibrary();
    photos.items = [];
    photos.started = false;
    photos.error = "Photo listing unavailable";
    if (state !== "counts") photos.timeline = undefined;
    if (state === "error") photos.timelineError = "Timeline counts are unavailable.";
    if (state === "pending") photos.timelineLoading = true;
    render(PhotosWorkspace, { photos, cache });
    const rail = await screen.findByRole("navigation", { name: "Timeline" });
    if (state === "counts")
      expect(within(rail).getByRole("button", { name: "2023, 60 photos" })).toBeTruthy();
    else if (state === "error")
      expect(within(rail).getByText("Timeline counts are unavailable.")).toBeTruthy();
    else expect(within(rail).getByText("Loading…")).toBeTruthy();
    expect(screen.getByText("Photo listing unavailable")).toBeTruthy();
    photos.dispose();
    await cache.dispose();
  },
);

it("renders the active filter badge", async () => {
  const photos = new Photos("scoped", vi.fn());
  photos.started = true;
  photos.query = { ...photos.query, filters: { rating_min: 1 } };
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache });
  expect(screen.getByRole("button", { name: "Filters, 1 active filter" }).textContent).toContain(
    "1",
  );
  photos.dispose();
  await cache.dispose();
});

it.each([false, true])(
  "moves Group, Density and Cull into the menu on compact screens, map=%s",
  async (mapMode) => {
    vi.stubGlobal("matchMedia", (media: string) =>
      Object.assign(new EventTarget(), { media, matches: media === "(max-width: 640px)" }),
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
    const photos = new Photos("scoped", vi.fn(), false, photoQuery, {
      projection: mapMode ? "map" : undefined,
    });
    photos.started = true;
    photos.items = [photo(1)];
    photos.total = 1;
    const cache = new PhotoPreviewCache("scoped", vi.fn());
    const view = render(PhotosWorkspace, {
      photos,
      cache,
      mapMode,
      tiles: {
        tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}",
        host: "tiles.example.org",
        attribution: "Example",
        max_zoom: 19,
      },
    });
    if (mapMode) {
      await vi.dynamicImportSettled();
      await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
    } else await screen.findByRole("button", { name: "Select Photo 1.jpg" });
    expect(screen.queryByRole("combobox", { name: /^Group photos/ })).toBeNull();
    expect(screen.queryByRole("combobox", { name: /^Grid density/ })).toBeNull();
    expect(screen.queryByRole("button", { name: "Cull" })).toBeNull();
    await fireEvent.click(screen.getByRole("button", { name: "More photo actions" }));
    if (!mapMode)
      expect(
        screen.getByRole("menuitem", { name: "Cull Vault identity unavailable. Reload to retry." }),
      ).toBeTruthy();
    await fireEvent.click(screen.getByRole("menuitemradio", { name: "Capture sessions" }));
    expect(photos.grouping).toBe("sessions");
    await fireEvent.click(screen.getByRole("menuitemradio", { name: "Large" }));
    expect(photos.density).toBe("large");
    await fireEvent.keyDown(document, { key: "Escape" });
    if (!mapMode) {
      vi.spyOn(PhotoCull.prototype, "probe").mockImplementation(async function (this: PhotoCull) {
        this.saved = true;
      });
      await view.rerender({ photos, cache, vaultID: "synthetic-vault" });
      await fireEvent.click(screen.getByRole("button", { name: "More photo actions" }));
      expect(await screen.findByRole("menuitem", { name: "Resume cull" })).toBeTruthy();
      expect(screen.getByRole("menuitem", { name: "Cull" })).toBeTruthy();
      await fireEvent.keyDown(document, { key: "Escape" });
    }
    photos.dispose();
    await cache.dispose();
  },
);

it("shows a loading pill while a page loads and Load more when it settles", async () => {
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  let finish!: (response: Response) => void;
  stubPhotoFetch(() => new Promise<Response>((resolve) => (finish = resolve)));
  const photos = new Photos("scoped", vi.fn());
  photos.started = true;
  photos.items = Array.from({ length: 60 }, (_, index) => photo(index + 1));
  photos.total = 100;
  photos.cursor = "next";
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotosWorkspace, { photos, cache });
  const status = document.querySelector(".photo-results > [role=status]");
  expect(status).toBeTruthy();
  expect(status!.textContent).toBe("");
  await fireEvent.click(screen.getByRole("button", { name: "Load more" }));
  expect(screen.getByText("Loading photos…")).toBeTruthy();
  expect(document.querySelector(".photo-results > [role=status]")).toBe(status);
  expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
  finish(Response.json({ items: [photo(61)], total: 100, next_cursor: "last" }));
  await waitFor(() => expect(screen.queryByText("Loading photos…")).toBeNull());
  expect(document.querySelector(".photo-results > [role=status]")).toBe(status);
  expect(screen.getByRole("button", { name: "Load more" })).toBeTruthy();
  photos.dispose();
  await cache.dispose();
});

it("explains paused grouping inside the selected photo's details", async () => {
  localStorage.setItem("docbank.photoMetadata", "open");
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1)];
  photos.total = 1;
  photos.started = true;
  photos.groupingIncomplete = true;
  photos.selection = { selectedIDs: new Set(["photo-1"]), anchorID: "photo-1" };
  render(PhotosWorkspace, { photos, cache: new PhotoPreviewCache("scoped", vi.fn()) });
  const panel = await screen.findByRole("complementary", { name: "Photo details" });
  await within(panel).findByText(/Some camera details are missing or outdated/);
  expect(screen.queryByRole("spinbutton", { name: /Burst gap/ })).toBeNull();
  photos.dispose();
});

function viewerWithCachedImage(photos: Photos, lookup: () => Promise<PhotoBrowseRow>) {
  let counter = 0;
  Object.defineProperty(URL, "createObjectURL", {
    configurable: true,
    value: vi.fn(() => `blob:synthetic-${++counter}`),
  });
  Object.defineProperty(URL, "revokeObjectURL", { configurable: true, value: vi.fn() });
  Object.defineProperty(HTMLImageElement.prototype, "decode", {
    configurable: true,
    value: vi.fn(async () => {}),
  });
  vi.spyOn(photos, "lookup").mockImplementation(lookup);
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "ready", generation_id: "fit-1" });
  stubPhotoFetch(vi.fn());
  return render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn(async () => new Blob(["synthetic-jpeg"])) } as unknown as PhotoPreviewCache,
    photoID: "photo-1",
  });
}

it("reads the open photo again on a live change and conceals its cached image once it is gone", async () => {
  const photos = workspaceWithPreparedPhoto();
  const row = photo(1);
  const lookup = vi.fn<() => Promise<PhotoBrowseRow>>().mockResolvedValue(row);
  viewerWithCachedImage(photos, lookup);
  await screen.findByText("Fit preview");
  lookup.mockResolvedValueOnce({ ...row, name: "Renamed.jpg" });
  await act(() => photos.changed({}));
  expect(lookup).toHaveBeenCalledTimes(2);
  await screen.findByRole("dialog", { name: "Photo viewer, Renamed.jpg" });
  expect(screen.getByText("Fit preview")).toBeTruthy();
  lookup.mockRejectedValueOnce(new APIError("This photo is no longer in this view.", 404, "x"));
  await act(() => photos.changed({}));
  await screen.findByText("This photo is no longer in this view.");
  expect(screen.queryByText("Fit preview")).toBeNull();
  expect(screen.getByText("No preview")).toBeTruthy();
  expect(photos.items).toEqual([]);
  photos.dispose();
});

it("keeps View original through an unrelated refresh of the same row", async () => {
  const photos = workspaceWithPreparedPhoto();
  photos.viewerReturn = {
    contextID: photos.contextID,
    assetID: "photo-1",
    control: "open",
    scrollTop: 0,
  };
  const row = photo(1);
  const lookup = vi.fn<() => Promise<PhotoBrowseRow>>().mockResolvedValue({ ...row });
  viewerWithCachedImage(photos, lookup);
  vi.spyOn(photos, "original").mockResolvedValue("blob:original");
  await screen.findByText("Fit preview");
  await fireEvent.click(screen.getByRole("button", { name: "View original" }));
  await screen.findByText("Original");
  await act(() => photos.changed({}));
  expect(lookup).toHaveBeenCalled();
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(screen.getByText("Original")).toBeTruthy();
  expect(screen.queryByText("Fit preview")).toBeNull();
  photos.dispose();
});

it.each([undefined, "map"] as const)(
  "reuses a refreshed full row only outside projections, projection=%s",
  async (projection) => {
    const photos = workspaceWithPreparedPhoto(preparedPhoto(1), projection);
    const lookup = vi.fn<() => Promise<PhotoBrowseRow>>().mockResolvedValue(photo(1));
    viewerWithCachedImage(photos, lookup);
    await screen.findByText("Fit preview");
    const stack = { asset_ids: ["photo-1", "photo-2"], representative_id: "photo-1" };
    const refreshed = projection
      ? { ...mapPhoto(1), name: "Refreshed.jpg" }
      : { ...photo(1), name: "Refreshed.jpg", stack };
    lookup.mockResolvedValue({ ...photo(1), name: "Looked up.jpg" });
    await act(async () => {
      photos.items = [refreshed];
      await photos.changed({ listRefreshed: true });
    });
    await screen.findByRole("dialog", {
      name: `Photo viewer, ${projection ? "Looked up.jpg" : "Refreshed.jpg"}`,
    });
    expect(lookup).toHaveBeenCalledTimes(projection ? 2 : 1);
    expect(photos.items[0]).toEqual(refreshed);
    expect(screen.getByText("Fit preview")).toBeTruthy();
    photos.dispose();
  },
);

it("ignores a live detail response after leaving and returning to Photos", async () => {
  const photos = workspaceWithPreparedPhoto();
  const lookup = vi.fn<() => Promise<PhotoBrowseRow>>().mockResolvedValue(photo(1));
  const view = viewerWithCachedImage(photos, lookup);
  await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
  let finish!: (row: PhotoBrowseRow) => void;
  lookup.mockImplementationOnce(() => new Promise((resolve) => (finish = resolve)));
  const changing = photos.changed({});
  await waitFor(() => expect(finish).toBeDefined());
  view.unmount();
  stubPhotoFetch(async () =>
    Response.json({ items: [{ ...photo(1), name: "Fresh.jpg" }], total: 1 }),
  );
  await act(() => photos.refresh());
  render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    photoID: "",
  });
  await screen.findByRole("button", { name: "Open Fresh.jpg" });
  await act(async () => {
    finish({ ...photo(1), name: "Older.jpg" });
    await changing;
  });
  expect(screen.getByRole("button", { name: "Open Fresh.jpg" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Open Older.jpg" })).toBeNull();
  photos.dispose();
});

it.each([
  { older: "foreground", outcome: "answers", newer: "live read" },
  { older: "foreground", outcome: "fails with 404", newer: "live read" },
  { older: "live", outcome: "fails with 404", newer: "navigation" },
])(
  "keeps the newest detail after $newer when an older $older read $outcome",
  async ({ older, outcome, newer }) => {
    const photos = workspaceWithPreparedPhoto();
    const row = photo(1);
    let settle!: () => void;
    const pending = () =>
      new Promise<PhotoBrowseRow>((resolve, reject) => {
        settle = () =>
          outcome === "answers"
            ? resolve({ ...row, name: "Older.jpg" })
            : reject(new APIError("This photo is no longer in this view.", 404, "x"));
      });
    const lookup = vi.fn<() => Promise<PhotoBrowseRow>>();
    if (older === "foreground") lookup.mockImplementationOnce(pending);
    else lookup.mockResolvedValueOnce(row).mockImplementationOnce(pending);
    const { rerender } = viewerWithCachedImage(photos, lookup);
    await waitFor(() => expect(lookup).toHaveBeenCalledTimes(1));
    if (older === "live") {
      await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
      void photos.changed({});
      await waitFor(() => expect(lookup).toHaveBeenCalledTimes(2));
    }
    const name = newer === "navigation" ? "Photo 2.jpg" : "Newer.jpg";
    lookup.mockResolvedValueOnce(newer === "navigation" ? photo(2) : { ...row, name });
    if (newer === "navigation") await rerender({ photoID: "photo-2" });
    else await act(() => photos.changed({}));
    await screen.findByRole("dialog", { name: `Photo viewer, ${name}` });
    settle();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(screen.queryByText("This photo is no longer in this view.")).toBeNull();
    expect(screen.getByRole("dialog", { name: `Photo viewer, ${name}` })).toBeTruthy();
    photos.dispose();
  },
);

it("forwards live changes to the metadata editor", async () => {
  const photos = workspaceWithPreparedPhoto();
  photos.selectLoaded();
  const invalidate = vi.spyOn(photos.authored, "invalidate").mockResolvedValue();
  stubPhotoFetch(vi.fn());
  render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    photoID: "",
  });
  await act(() => photos.changed({}));
  expect(invalidate).toHaveBeenCalledOnce();
  photos.dispose();
});

it("keeps shortcuts and stack decisions usable during refresh", async () => {
  const photos = workspaceWithPreparedPhoto();
  photos.authored.targets = [];
  const stack = { asset_ids: ["photo-1", "photo-2"], representative_id: "photo-1" };
  const row = { ...photo(1), stack };
  photos.items = [row];
  photos.selectLoaded();
  vi.spyOn(api, "preparePhotoAuthored").mockResolvedValue(
    photoPreparation([preparedPhoto(1), preparedPhoto(2)]),
  );
  let finish!: (response: Response) => void;
  stubPhotoFetch(vi.fn(() => new Promise<Response>((resolve) => (finish = resolve))));
  const edit = vi.spyOn(photos.authored, "edit").mockResolvedValue();
  const shortcuts = createShortcutManager();
  render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    shortcuts,
  });
  await waitFor(() => expect(photos.authored.stackTargets).toHaveLength(2));
  const refreshing = photos.refresh(undefined, true);
  await waitFor(() => expect(photos.loading).toBe(true));
  expect(photos.authored.loading).toBe(false);
  shortcuts.handleKeydown(new KeyboardEvent("keydown", { key: "4", cancelable: true }));
  expect(edit).toHaveBeenCalledOnce();
  expect(screen.queryByText(/Try again when they finish/)).toBeNull();
  for (const name of ["Keep all", "Keep selected", "Choose representative"]) {
    expect((screen.getByRole("button", { name }) as HTMLButtonElement).disabled).toBe(false);
  }
  // A stack conflict mid-refresh still leaves Reload stack usable.
  await act(() => {
    photos.authored.conflict = true;
  });
  expect((screen.getByRole("button", { name: "Reload stack" }) as HTMLButtonElement).disabled).toBe(
    false,
  );
  await act(async () => {
    finish(Response.json({ items: [row], total: 1, stacks: [stack] }));
    expect(await refreshing).toBe(true);
  });
  photos.dispose();
});

it("keeps newer Library values across an older live read", async () => {
  const before = preparedPhoto(1);
  const photos = workspaceWithPreparedPhoto(before);
  const shortcuts = createShortcutManager();
  let finish!: (receipt: PhotoAuthoredReceipt) => void;
  const receipt = new Promise<PhotoAuthoredReceipt>((resolve) => (finish = resolve));
  const edit = vi.spyOn(api, "editPhotoAuthored").mockReturnValue(receipt);
  const lookup = vi.spyOn(photos, "lookup").mockResolvedValue(photo(1));
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  stubPhotoFetch(async () => Response.json({ items: [{ ...photo(1), rotation: 90 }], total: 1 }));
  render(PhotosWorkspace, {
    photos,
    shortcuts,
    photoID: "photo-1",
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
  });
  await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
  let finishRead!: (row: PhotoBrowseRow) => void;
  lookup.mockImplementationOnce(() => new Promise((resolve) => (finishRead = resolve)));
  const reading = photos.changed({});
  await waitFor(() => expect(finishRead).toBeDefined());
  shortcuts.handleKeydown(new KeyboardEvent("keydown", { key: "r", cancelable: true }));
  await waitFor(() => expect(edit).toHaveBeenCalledOnce());
  expect(edit.mock.calls[0]![0].rotation_delta).toBe(90);
  await act(() =>
    finish({
      receipt_id: "local",
      before: [before],
      after: [{ ...before, revision: 2, values: { ...before.values, rotation: 90 } }],
    }),
  );
  await waitFor(() => expect(photos.authored.busy).toBe(false));
  await waitFor(() => expect(photos.items[0]!.rotation).toBe(90));
  finishRead(photo(1));
  await reading;
  expect(photos.items[0]!.rotation).toBe(90);
  photos.dispose();
});

// A Library of three months, 60 photos each; only the newest is loaded, and each later page
// holds the next older month.
function railLibrary(
  sort: QuerySort = { field: "capture_time", direction: "desc" },
  filters: Query["filters"] = {},
  hiddenAccess?: HiddenAccess,
) {
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  const month = (offset: number, date: string) =>
    Array.from({ length: 60 }, (_, i) => photo(offset + i + 1, date));
  const pages: Record<string, () => Promise<Response>> = {
    first: async () =>
      Response.json({ items: month(0, "2025-06-01"), total: 180, next_cursor: "p2" }),
    p2: async () =>
      Response.json({ items: month(60, "2024-06-01"), total: 180, next_cursor: "p3" }),
    p3: async () => Response.json({ items: month(120, "2023-06-01"), total: 180 }),
  };
  const bodies: { cursor?: string; query: unknown }[] = [];
  stubPhotoFetch(async (_url, init) => {
    const body = JSON.parse(String(init.body));
    bodies.push(body);
    return pages[body.cursor ?? "first"]!();
  });
  const photos = new Photos(
    "scoped",
    vi.fn(),
    false,
    { ...photoQuery, sort, filters },
    { hiddenAccess },
  );
  photos.started = true;
  photos.items = month(0, "2025-06-01");
  photos.total = 180;
  photos.cursor = "p2";
  photos.timeline = photoTimelineCounts([
    ["2025-06-01", 60],
    ["2024-06-01", 60],
    ["2023-06-01", 60],
  ]).facets[0];
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  return { photos, cache, pages, bodies };
}

// Sections stack in the grid, so a month starts below the sections before it.
function sectionTop(month: string) {
  let top = 0;
  for (const section of document.querySelectorAll<HTMLElement>("[data-month]")) {
    if (section.dataset.month === month) return top;
    top += Number.parseFloat(section.style.height);
  }
  throw new Error(`no section for ${month}`);
}

it("pages a chosen year in until it arrives, with the query, count, and selection unchanged", async () => {
  const { photos, cache, bodies } = railLibrary();
  render(PhotosWorkspace, { photos, cache });
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  const query = JSON.stringify(photos.query);
  const rail = screen.getByRole("navigation", { name: "Timeline" });
  await fireEvent.click(within(rail).getByRole("button", { name: "2023, 60 photos" }));
  const scroll = screen.getByTestId("photo-scroll");
  await waitFor(() => expect(photos.seek).toBe(""));
  expect(scroll.scrollTop).toBe(Math.ceil(sectionTop("2023-06")));
  expect(bodies.map((body) => body.cursor)).toEqual(["p2", "p3"]);
  expect(bodies.every((body) => JSON.stringify(body.query) === query)).toBe(true);
  expect(screen.getByText("180 photos")).toBeTruthy();
  expect([...photos.selectedIDs]).toEqual(["photo-1"]);
  photos.dispose();
  await cache.dispose();
});

it.each(["", "river"])("disables the date rail outside capture order, text=%s", async (text) => {
  const albumID = text ? "" : "00000000-0000-4000-8000-000000000001";
  const { photos, cache } = railLibrary(
    { field: text ? "relevance" : "added_time", direction: "desc" },
    albumID ? { set_ids: [albumID] } : {},
  );
  photos.query = { ...photos.query, text };
  render(PhotosWorkspace, { photos, cache, albumID });
  const rail = await screen.findByRole("navigation", { name: "Timeline" });
  expect(
    within(rail).getByText(`Sort by ${text ? "Capture date" : "Captured"}, newest first.`),
  ).toBeTruthy();
  const year = within(rail).getByRole("button", { name: "2023, 60 photos" });
  expect((year as HTMLButtonElement).disabled).toBe(true);
  photos.dispose();
  await cache.dispose();
});

it("passes loaded stack members to the grid for a date jump without paging", async () => {
  const { photos, cache, bodies } = railLibrary();
  // A burst crosses midnight. January's representative hides its December members.
  const stack = {
    asset_ids: ["photo-1", "photo-2"],
    representative_id: "photo-1",
  };
  photos.items = [
    { ...photo(1, "2025-01-01T00:00:00"), stack },
    { ...photo(2, "2024-12-31T23:59:59"), stack },
    ...Array.from({ length: 60 }, (_, index) => photo(index + 3, "2024-11-30T12:00:00")),
  ];
  photos.total = 122;
  photos.timeline = photoTimelineCounts([
    ["2025-01-01", 1],
    ["2024-12-31", 1],
    ["2024-11-30", 60],
    ["2024-10-01", 60],
  ]).facets[0];
  const view = render(PhotosWorkspace, { photos, cache });
  try {
    await screen.findByRole("button", { name: "Select Photo 1.jpg" });
    expect(screen.queryByRole("button", { name: "Select Photo 2.jpg" })).toBeNull();
    const rail = screen.getByRole("navigation", { name: "Timeline" });
    await fireEvent.click(within(rail).getByRole("button", { name: "2024, 121 photos" }));
    expect(photos.seek).toBe("");
    expect(bodies).toEqual([]);
  } finally {
    view.unmount();
    photos.dispose();
    await cache.dispose();
  }
});

it("keeps a jump pending through a paging failure and finishes it on Retry", async () => {
  const { photos, cache, pages } = railLibrary();
  const page = pages.p2!;
  pages.p2 = async () => Response.json({ detail: "Page unavailable" }, { status: 503 });
  render(PhotosWorkspace, { photos, cache });
  const rail = await screen.findByRole("navigation", { name: "Timeline" });
  await fireEvent.click(within(rail).getByRole("button", { name: "2023, 60 photos" }));
  await screen.findByText("Page unavailable");
  expect(photos.seek).toBe("2023-06");
  pages.p2 = page;
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(photos.seek).toBe(""));
  expect(screen.getByTestId("photo-scroll").scrollTop).toBe(Math.ceil(sectionTop("2023-06")));
  photos.dispose();
  await cache.dispose();
});

it("resumes a date jump after cancelling Hidden setup when its page finished during setup", async () => {
  const access = new HiddenAccess("scoped", vi.fn());
  access.configured = false;
  const { photos, cache, pages, bodies } = railLibrary(undefined, undefined, access);
  let finish!: (reply: Response) => void;
  const held = pages.p2!;
  pages.p2 = () => new Promise<Response>((resolve) => (finish = resolve));
  render(PhotosWorkspace, { photos, cache });
  const rail = await screen.findByRole("navigation", { name: "Timeline" });
  await fireEvent.click(within(rail).getByRole("button", { name: "2023, 60 photos" }));
  expect(bodies.map((body) => body.cursor)).toEqual(["p2"]);
  const hide = photos.setHidden("photo-1");
  await waitFor(() => expect(access.setupPrompt).toBeDefined());
  const modal = render(HiddenSetupModal, { access });
  finish(await held());
  await waitFor(() => expect(photos.loading).toBe(false));
  expect(photos.items).toHaveLength(120);
  expect(photos.seek).toBe("2023-06");
  expect(bodies.map((body) => body.cursor)).toEqual(["p2"]);
  await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  await hide;
  modal.unmount();
  await waitFor(() => expect(photos.seek).toBe(""));
  expect(bodies.map((body) => body.cursor)).toEqual(["p2", "p3"]);
  expect(screen.getByTestId("photo-scroll").scrollTop).toBe(Math.ceil(sectionTop("2023-06")));
  photos.dispose();
  await cache.dispose();
});

it.each(["scrolling", "opening a photo", "unmounting"] as const)(
  "clears a pending jump when %s",
  async (action) => {
    const { photos, cache, pages, bodies } = railLibrary();
    let finish!: (reply: Response) => void;
    const held = pages.p2!;
    pages.p2 = () => new Promise<Response>((resolve) => (finish = resolve));
    const onphotochange = vi.fn();
    const { unmount } = render(PhotosWorkspace, { photos, cache, onphotochange });
    const rail = await screen.findByRole("navigation", { name: "Timeline" });
    await fireEvent.click(within(rail).getByRole("button", { name: "2023, 60 photos" }));
    expect(photos.seek).toBe("2023-06");
    if (action === "scrolling") {
      expect(within(rail).getByRole("button", { name: "Jun 2023, 60 photos" })).toBeTruthy();
      const scroll = screen.getByTestId("photo-scroll");
      scroll.scrollTop = 100;
      await fireEvent.scroll(scroll);
      expect(within(rail).queryByRole("button", { name: /^Jun 2023/ })).toBeNull();
    } else if (action === "opening a photo") {
      await fireEvent.keyDown(screen.getByRole("button", { name: "Select Photo 1.jpg" }), {
        key: "Enter",
      });
      expect(onphotochange).toHaveBeenCalledWith("photo-1", "open");
    } else unmount();
    expect(photos.seek).toBe("");
    finish(await held());
    await waitFor(() => expect(photos.loading).toBe(false));
    expect(photos.seek).toBe("");
    if (action === "scrolling") {
      expect(screen.getByTestId("photo-scroll").scrollTop).toBe(100);
      expect(photos.items.map((item) => item.asset_id)).toEqual(
        Array.from({ length: 120 }, (_, index) => `photo-${index + 1}`),
      );
      expect(bodies.map((body) => body.cursor)).toEqual(["p2"]);
    }
    photos.dispose();
    await cache.dispose();
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

function photoFacePageFor(
  row: ReturnType<typeof photo>,
  overrides: Partial<ReturnType<typeof photoFacePage>> = {},
) {
  return photoFacePage({
    faces: [photoFace({ content_version_id: row.content_version_id })],
    ...overrides,
  });
}

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
  const read = vi.spyOn(api, "listPhotoFaces").mockResolvedValue(photoFacePageFor(photo(1)));
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
  localStorage.setItem("docbank.photoMetadata", "open");
  const component = render(PhotosWorkspace, props);
  await screen.findByRole("button", { name: "Find or create a person: Ada" });
  return { component, props, onphotochange, photos, read };
}

it("clears People after failed Map navigation", async () => {
  const { component, props, photos, read } = await facesWorkspaceSetup();
  vi.spyOn(photos, "lookup").mockRejectedValue(new Error("Photo unavailable"));
  await component.rerender({ ...props, mapMode: true, photoID: "photo-2" });
  await screen.findByText("Photo unavailable");
  expect(screen.queryByRole("button", { name: "Find or create a person: Ada" })).toBeNull();
  expect(photos.faceEditor?.version).toBe("");
  expect(read).toHaveBeenCalledOnce();
});

it.each([false, true])("revalidates People after refresh, readable=%s", async (readable) => {
  const { component, props, photos, read } = await facesWorkspaceSetup();
  await fireEvent.click(screen.getByRole("button", { name: "Open Photo 1.jpg" }));
  await component.rerender({ ...props, photoID: "photo-1" });
  await within(await screen.findByRole("dialog", { name: /^Photo viewer,/ })).findByRole("button", {
    name: "Find or create a person: Ada",
  });
  expect(read).toHaveBeenCalledOnce();
  read.mockResolvedValue(
    photoFacePageFor(photo(1), {
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
});

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
    photoFacePageFor(
      row,
      committed
        ? {
            faces: [
              photoFace({ content_version_id: row.content_version_id, assertion_id: undefined }),
            ],
            assertions: [],
            people: [],
          }
        : {},
    ),
  );
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [] });
  vi.spyOn(api, "decidePhotoFace").mockImplementation(async () => {
    await new Promise<void>((resolve) => (finish = resolve));
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
  view = render(PhotosWorkspace, { ...props, cache, photoID: "" });
  expect((await screen.findAllByText("Response lost")).length).toBeGreaterThan(0);
  await screen.findByText("No matching photos");
  photos.dispose();
  await cache.dispose();
});

it.each([false, true])(
  "restores Cull faces after a filtered rating refresh, leaves filter=%s",
  async (leavesFilter) => {
    vi.stubGlobal("indexedDB", new IDBFactory());
    vi.stubGlobal("IDBKeyRange", IDBKeyRange);
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
    const { component, props, photos, read } = await facesWorkspaceSetup();
    await fireEvent.click(screen.getByRole("button", { name: "Metadata" }));
    photos.query = { ...photos.query, text: "Photo" };
    const rows = [photo(1), photo(2)];
    const review = new PhotoCull(photos.session, photoCullStorage("vault"), vi.fn());
    review.run = {
      ids: rows.map((row) => row.asset_id),
      checkpoint: {
        runID: "run",
        revision: 0,
        pass: rows.map((row) => row.asset_id),
        position: 0,
        visited: ["photo-1"],
        skipped: [],
        unavailable: [],
        scope: "all",
        autoAdvance: false,
      },
    };
    await photoCullStorage("vault").replace(review.run);
    vi.spyOn(api, "preparePhotoAuthored").mockResolvedValue(photoPreparation([preparedPhoto(1)]));
    photos.cull = review;
    photos.cullVaultID = "vault";
    photos.culling = true;
    await component.rerender({ ...props, vaultID: "vault" });
    const dialog = await screen.findByRole("dialog", { name: "Cull photos" });
    const closeups = await within(dialog).findByRole("region", { name: "Face closeups" });
    await within(closeups).findByText("Face 1 · Ada");
    let finishRefresh!: () => void;
    stubPhotoFetch(async (url, init) => {
      if (!url.endsWith("/photos/assets/query")) throw new Error(url);
      const ids = JSON.parse(String(init.body)).query.filters?.asset_ids;
      if (ids)
        return Response.json({
          items: rows.filter((row) => ids.includes(row.asset_id)),
          total: ids.length,
          facets: [],
        });
      await new Promise<void>((resolve) => {
        finishRefresh = resolve;
      });
      return Response.json({
        items: leavesFilter ? [] : [photo(1)],
        total: leavesFilter ? 0 : 1,
        facets: [],
      });
    });
    const before = preparedPhoto(1);
    vi.spyOn(api, "editPhotoAuthored").mockResolvedValue({
      receipt_id: "rated",
      before: [before],
      after: [{ ...before, revision: 2, values: { ...before.values, rating: 5 } }],
    });
    const readsBefore = read.mock.calls.length;
    await fireEvent.keyDown(dialog.querySelector(".photo-cull")!, { key: "5" });
    await waitFor(() => expect(photos.loading).toBe(true));
    expect(within(dialog).queryByRole("region", { name: "Face closeups" })).toBeNull();
    await act(() => finishRefresh());
    await waitFor(() => expect(photos.loading).toBe(false));
    await tick();
    await within(await within(dialog).findByRole("region", { name: "Face closeups" })).findByText(
      "Face 1 · Ada",
    );
    expect(review.currentID).toBe("photo-1");
    expect(read).toHaveBeenCalledTimes(readsBefore + 1);
    expect(read).toHaveBeenLastCalledWith("version-1", expect.anything());
  },
);

it("names the photo being edited in the metadata panel header", async () => {
  localStorage.setItem("docbank.photoMetadata", "open");
  const photos = workspaceWithPreparedPhoto();
  photos.selectLoaded();
  render(PhotosWorkspace, {
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
  });
  const panel = await screen.findByRole("complementary", { name: "Photo details" });
  expect(within(panel).getByRole("heading", { level: 2, name: photo(1).name })).toBeTruthy();
  expect(within(panel).queryByRole("heading", { name: "Metadata" })).toBeNull();
  photos.dispose();
});
