import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/svelte";
import { tick } from "svelte";
import { mapPhoto } from "./photo-test-fixtures.js";
const mocks = vi.hoisted(() => {
  const frames = new Map<number, () => void>();
  let serial = 0;
  const util = {
    requestAnimFrame: vi.fn((callback: () => void) => {
      frames.set(++serial, callback);
      return serial;
    }),
    cancelAnimFrame: vi.fn((id: number) => frames.delete(id)),
  };
  const bounds = { latitude: 2 };
  const map = {
    fitBounds: vi.fn(),
    project: vi.fn((position: number[]) => ({ lat: position[0], lng: position[1] })),
    getPixelBounds: vi.fn(() => ({
      contains: (position: { lat: number }) => position.lat < bounds.latitude,
    })),
    getZoom: () => 0,
    getCenter: () => ({ lat: 10, lng: 20 }),
    getMaxZoom: () => 0,
    on: vi.fn(),
    invalidateSize: vi.fn(),
    remove: vi.fn(),
    setView: vi.fn(),
  };
  map.setView.mockReturnValue(map);
  const cluster = {
    addLayers: vi.fn(),
    clearLayers: vi.fn(),
    removeLayers: vi.fn(),
    getBounds: vi.fn(),
    on: vi.fn(),
    addTo: vi.fn(),
    getVisibleParent: vi.fn(),
  };
  const marker = vi.fn((coordinates: number[]) => ({
    getLatLng: () => ({
      lat: coordinates[0],
      lng: coordinates[1],
      equals: (value: number[]) => value[0] === coordinates[0] && value[1] === coordinates[1],
    }),
    setLatLng: vi.fn((value: number[]) => {
      coordinates = value;
    }),
    options: { title: "Photo " + coordinates[0] + ".jpg" },
    getElement: () => undefined,
    on: vi.fn(),
  }));
  const tile = { addTo: vi.fn(), on: vi.fn() };
  tile.on.mockReturnValue(tile);
  const layer: {
    createTile?: (
      this: { getTileUrl: (coords: unknown) => string },
      coords: unknown,
      done: (error: unknown, tile: HTMLElement) => void,
    ) => HTMLElement;
  } = {};
  const tileLayer = vi.fn(function () {
    return tile;
  });
  return {
    frames,
    util,
    bounds,
    map,
    cluster,
    marker,
    makeMap: vi.fn(() => map),
    tileLayer,
    tile,
    layer,
    extend: vi.fn((methods: typeof layer) => {
      Object.assign(layer, methods);
      return tileLayer;
    }),
  };
});
vi.mock("leaflet", () => ({
  default: {
    Util: mocks.util,
    map: mocks.makeMap,
    marker: mocks.marker,
    TileLayer: { extend: mocks.extend },
    markerClusterGroup: () => mocks.cluster,
    divIcon: vi.fn(),
  },
}));
vi.mock("leaflet.markercluster", () => ({}));
import type { PhotoMapRow } from "./generated/docbank.js";
import PhotoMap from "./PhotoMap.svelte";
import PhotosWorkspace from "./PhotosWorkspace.svelte";
import { Photos, photoQuery } from "./photos.svelte.js";
import type { PhotoPreviewCache } from "./photoPreviewCache.js";
afterEach(() => {
  cleanup();
  mocks.frames.clear();
  vi.clearAllMocks();
  vi.restoreAllMocks();
  mocks.bounds.latitude = 2;
  vi.unstubAllGlobals();
});
function frame() {
  const [id, callback] = mocks.frames.entries().next().value!;
  mocks.frames.delete(id);
  callback();
}

function setup(
  items: PhotoMapRow[] = [{ ...mapPhoto(1), latitude: 1, longitude: 1 }],
  onauthfailure?: (cause: unknown) => void,
) {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const props = {
    items,
    session: "tab",
    tiles: {
      tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}",
      host: "tiles.example.org",
      attribution: "Example",
      max_zoom: 0,
    },
    onopen: vi.fn(),
    onvisible: vi.fn(),
    onauthfailure,
  };
  return { props, view: render(PhotoMap, props) };
}
it("renders located rows and cancels pending batches on replacement and disposal", async () => {
  const first = { ...mapPhoto(1), latitude: 1, longitude: 1 },
    second = { ...mapPhoto(2), latitude: 2, longitude: 2 };
  const { props, view } = setup([
    ...Array.from({ length: 201 }, (_, id) => ({
      ...mapPhoto(id),
      latitude: id % 90,
      longitude: id % 180,
    })),
    { ...mapPhoto(202), latitude: null, longitude: null },
  ]);
  frame();
  expect(mocks.marker).toHaveBeenCalledTimes(200);
  expect(mocks.frames.size).toBe(1);
  vi.clearAllMocks();
  await view.rerender({ ...props, items: [second] });
  expect(mocks.cluster.removeLayers).toHaveBeenLastCalledWith(
    expect.arrayContaining([expect.anything()]),
  );
  expect(mocks.marker).not.toHaveBeenCalled();
  expect(props.onvisible).toHaveBeenLastCalledWith([]);
  await view.rerender({
    ...props,
    items: [first, { ...mapPhoto(3), latitude: null, longitude: null }],
  });
  frame();
  expect(mocks.cluster.addLayers.mock.calls.at(-1)![0]).toHaveLength(1);
  await view.rerender({ ...props, items: [] });
  await view.rerender({
    ...props,
    items: Array.from({ length: 201 }, (_, id) => ({
      ...mapPhoto(id),
      latitude: id % 90,
      longitude: id % 180,
    })),
  });
  frame();
  vi.clearAllMocks();
  view.unmount();
  expect(mocks.frames.size).toBe(0);
  expect(mocks.util.cancelAnimFrame).toHaveBeenCalled();
  expect(mocks.map.remove).toHaveBeenCalledOnce();
});

it("recomputes visible rows when the map moves", () => {
  const second = { ...mapPhoto(2), latitude: 2, longitude: 2 };
  const { props } = setup([second]);
  mocks.bounds.latitude = 3;
  const moved = mocks.map.on.mock.calls.find(([name]) => name === "moveend")![1];
  moved();
  expect(props.onvisible).toHaveBeenLastCalledWith([second]);
});
it("restores and reports the map view", () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const onview = vi.fn();
  render(PhotoMap, {
    items: [{ ...mapPhoto(1), latitude: 1, longitude: 1 }],
    session: "tab",
    tiles: {
      tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}",
      host: "tiles.example.org",
      attribution: "Example",
      max_zoom: 0,
    },
    view: { lat: 5, lng: 6, zoom: 7 },
    onopen: vi.fn(),
    onvisible: vi.fn(),
    onview,
  });
  expect(mocks.map.setView).toHaveBeenCalledWith([5, 6], 7);
  const moved = mocks.map.on.mock.calls.find(([name]) => name === "moveend")![1];
  moved();
  expect(onview).toHaveBeenCalledWith({ lat: 10, lng: 20, zoom: 0 });
  onview.mockClear();
  cleanup();
  expect(() => moved()).not.toThrow();
  expect(onview).not.toHaveBeenCalled();
});
it("restores focus to a visible parent or the map without moving it", () => {
  const second = { ...mapPhoto(2), latitude: 2, longitude: 2 };
  const { view } = setup([second]);
  frame();
  const child = mocks.marker.mock.results[0]!.value;
  expect(mocks.marker).toHaveBeenCalledWith(
    [2, 2],
    expect.objectContaining({ autoPanOnFocus: false }),
  );
  const focus = vi.fn();
  mocks.cluster.getVisibleParent.mockReturnValue({ getElement: () => ({ focus }) });
  view.component.focus(second.asset_id);
  expect(mocks.cluster.getVisibleParent).toHaveBeenCalledWith(child);
  expect(focus).toHaveBeenCalledWith({ preventScroll: true });
  mocks.cluster.getVisibleParent.mockReturnValue(undefined);
  const container = view.container.querySelector<HTMLElement>(".photo-map")!;
  const fallback = vi.spyOn(container, "focus");
  view.component.focus(second.asset_id);
  expect(fallback).toHaveBeenCalledWith({ preventScroll: true });
  expect(mocks.map.setView).toHaveBeenCalledTimes(1);
  expect(mocks.map.fitBounds).not.toHaveBeenCalled();
});
it("bounds the map and tiles to one world", () => {
  const { props } = setup();
  expect(mocks.makeMap).toHaveBeenCalledWith(
    expect.anything(),
    expect.objectContaining({
      maxBounds: [
        [-90, -180],
        [90, 180],
      ],
      maxBoundsViscosity: 1,
    }),
  );
  expect(mocks.tileLayer).toHaveBeenCalledWith(
    props.tiles.tile_url,
    expect.objectContaining({ maxZoom: 0, noWrap: true }),
  );
});

function tileFlow(fetcher: ReturnType<typeof vi.fn>) {
  const onauthfailure = vi.fn();
  setup(undefined, onauthfailure);
  vi.stubGlobal("fetch", fetcher);
  const created = vi.fn((_blob: Blob) => "blob:tile");
  vi.stubGlobal("URL", Object.assign(URL, { createObjectURL: created, revokeObjectURL: vi.fn() }));
  const layer = { getTileUrl: () => "/api/daemon/web-map-tiles/2/1/1" };
  const make = (done = vi.fn()) => ({
    done,
    tile: mocks.layer.createTile!.call(layer, { z: 2, x: 1, y: 1 }, done) as HTMLImageElement,
  });
  const event = (name: string, tile: HTMLElement) =>
    mocks.tile.on.mock.calls
      .filter(([n]) => n === name)
      .at(-1)![1]({ tile, coords: { z: 2, x: 1, y: 1 } });
  return { onauthfailure, make, event, created };
}
const png = () =>
  Promise.resolve(new Response("png", { headers: { "Content-Type": "image/png" } }));
it("loads each tile through the session and releases its blob URL", async () => {
  const fetcher = vi.fn((_url: string, _init: RequestInit) => png());
  const { make, created } = tileFlow(fetcher);
  const { done, tile } = make();
  await waitFor(() => expect(tile.src).toBe("blob:tile"));
  const [url, init] = fetcher.mock.calls[0]!;
  expect(url).toBe("/api/daemon/web-map-tiles/2/1/1");
  const headers = new Headers(init.headers);
  expect(headers.get("X-Docbank-Web-Session")).toBe("tab");
  expect(headers.get("Accept")).toBe("image/*");
  tile.dispatchEvent(new Event("load"));
  expect(done).toHaveBeenCalledWith(undefined, tile);
  expect(URL.revokeObjectURL).toHaveBeenCalledWith("blob:tile");
  expect(created).toHaveBeenCalledOnce();
});
it("aborts a pending fetch and ignores a late response once Leaflet drops the tile", async () => {
  let finish!: (response: Response) => void;
  const fetcher = vi.fn(
    (_url: string, _init: RequestInit) =>
      new Promise<Response>((resolve) => {
        finish = resolve;
      }),
  );
  const { make, event, created } = tileFlow(fetcher);
  const { done, tile } = make();
  event("tileabort", tile);
  expect(fetcher.mock.calls[0]![1].signal!.aborted).toBe(true);
  finish(new Response("png"));
  await new Promise((resolve) => setTimeout(resolve));
  expect(created).not.toHaveBeenCalled();
  expect(done).not.toHaveBeenCalled();
});
it("revokes a loaded blob when the tile unloads after Leaflet replaces its callbacks", async () => {
  const { make, event, created } = tileFlow(vi.fn(() => png()));
  const { done, tile } = make();
  await waitFor(() => expect(created).toHaveBeenCalledOnce());
  tile.onload = tile.onerror = () => false;
  event("tileunload", tile);
  expect(URL.revokeObjectURL).toHaveBeenCalledWith("blob:tile");
  tile.dispatchEvent(new Event("load"));
  expect(done).not.toHaveBeenCalled();
});
it("reports provider failures as tile errors and authentication failures to the session owner", async () => {
  const fetcher = vi.fn(() => Promise.resolve(Response.json({ detail: "down" }, { status: 502 })));
  const { make, onauthfailure } = tileFlow(fetcher);
  const failed = make();
  await waitFor(() => expect(failed.done).toHaveBeenCalledWith(expect.any(Error), failed.tile));
  expect(onauthfailure).not.toHaveBeenCalled();
  fetcher.mockResolvedValueOnce(Response.json({ detail: "expired" }, { status: 401 }));
  const expired = make();
  await waitFor(() => expect(onauthfailure).toHaveBeenCalledOnce());
  expect(expired.done).not.toHaveBeenCalled();
});

it("shows one provider notice when tiles fail repeatedly", async () => {
  setup();
  const failed = mocks.tile.on.mock.calls.find(([name]) => name === "tileerror")![1];
  const tile = { coords: { z: 2, x: 1, y: 1 } },
    other = { coords: { z: 2, x: 2, y: 1 } };
  failed(tile);
  failed(tile);
  await tick();
  const notices = screen.getAllByRole("alert");
  expect(notices).toHaveLength(1);
  expect(notices[0].textContent).toBe(
    "Map tiles from tiles.example.org couldn't load. Check [web.map_tiles], then restart Docbank.",
  );
  const loaded = mocks.tile.on.mock.calls.find(([name]) => name === "tileload")![1];
  loaded(other);
  await tick();
  expect(screen.getAllByRole("alert")).toHaveLength(1);
  failed(other);
  loaded(tile);
  await tick();
  expect(screen.getAllByRole("alert")).toHaveLength(1);
  loaded(other);
  await tick();
  expect(screen.queryByRole("alert")).toBeNull();
  failed(tile);
  failed(other);
  await tick();
  const unloaded = mocks.tile.on.mock.calls.find(([name]) => name === "tileunload")![1];
  unloaded(tile);
  await tick();
  expect(screen.getAllByRole("alert")).toHaveLength(1);
  unloaded(other);
  await tick();
  expect(screen.queryByRole("alert")).toBeNull();
});

it("scans only appended rows and recomputes replacements", async () => {
  const first = mapPhoto(1),
    second = mapPhoto(2),
    third = mapPhoto(3);
  const { props, view } = setup([first]);
  frame();
  const marker = mocks.marker.mock.results[0]!.value;
  const position = vi.spyOn(marker, "getLatLng");
  mocks.map.project.mockClear();
  const appended = [first, second];
  await view.rerender({ items: appended, appendedFrom: props.items });
  expect(mocks.map.project).toHaveBeenCalledTimes(1);
  expect(position).not.toHaveBeenCalled();
  frame();
  expect(mocks.marker).toHaveBeenCalledTimes(2);
  expect(props.onvisible).toHaveBeenLastCalledWith([first, second]);
  mocks.map.project.mockClear();
  const replacement = { ...first, latitude: 3 };
  await view.rerender({ items: [replacement, third, second], appendedFrom: undefined });
  expect(mocks.map.project).toHaveBeenCalledTimes(3);
  expect(position).toHaveBeenCalledOnce();
  expect(props.onvisible).toHaveBeenLastCalledWith([third, second]);
});

it("keeps unfinished marker batches when another page appends", async () => {
  const items = Array.from({ length: 250 }, (_, index) => mapPhoto(index + 1));
  const { view } = setup(items);
  frame();
  expect(mocks.marker).toHaveBeenCalledTimes(200);
  await view.rerender({ items: [...items, mapPhoto(251)], appendedFrom: items });
  frame();
  expect(mocks.marker).toHaveBeenCalledTimes(251);
  expect(mocks.frames.size).toBe(0);
});

it.each(["append", "hide", "trash"])(
  "shows page one before page two resolves and recomputes after %s",
  async (action) => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
    const first = mapPhoto(1),
      second = mapPhoto(2),
      third = mapPhoto(3);
    let finish!: (response: Response) => void;
    const fetcher = vi.fn((url: string, init: RequestInit) => {
      if (url.endsWith("/hide") || url.endsWith("/trash"))
        return Promise.resolve(Response.json({ id: first.asset_id, revision: 2 }));
      const body = JSON.parse(String(init.body));
      if (body.facets?.length)
        return Promise.resolve(Response.json({ items: [], total: 0, facets: [] }));
      return body.cursor
        ? new Promise<Response>((resolve) => (finish = resolve))
        : Promise.resolve(
            Response.json({ map_items: [first, second], total: 3, next_cursor: "page-two" }),
          );
    });
    vi.stubGlobal("fetch", fetcher);
    const photos = new Photos("scoped", vi.fn(), false, photoQuery, { projection: "map" });
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
    const loading = photos.loadMore();
    await vi.dynamicImportSettled();
    await screen.findByRole("button", { name: "Open Photo 1.jpg" });
    expect(screen.getByRole("button", { name: "Open Photo 2.jpg" })).toBeTruthy();
    expect(screen.getByText("2 photos", { exact: true })).toBeTruthy();
    expect(screen.getByText("Loading located photos: 2 of 3")).toBeTruthy();
    expect(photos.loading).toBe(true);
    expect(finish).toBeTypeOf("function");
    mocks.map.project.mockClear();
    if (action === "append") {
      finish(Response.json({ map_items: [third], total: 3 }));
      await loading;
      await screen.findByRole("button", { name: "Open Photo 3.jpg" });
      expect(screen.getByText("3 photos", { exact: true })).toBeTruthy();
      expect(mocks.map.project).toHaveBeenCalledTimes(1);
    } else {
      if (action === "hide") await photos.setHidden(first.asset_id);
      else {
        photos.selectLoaded([first.asset_id]);
        await photos.trashSelected();
      }
      await waitFor(() =>
        expect(screen.queryByRole("button", { name: "Open Photo 1.jpg" })).toBeNull(),
      );
      expect(screen.getByRole("button", { name: "Open Photo 2.jpg" })).toBeTruthy();
      expect(screen.getByText("1 photos", { exact: true })).toBeTruthy();
      expect(mocks.map.project).toHaveBeenCalledTimes(1);
      finish(Response.json({ map_items: [third], total: 3 }));
      await loading;
      expect(photos.items.map((row) => row.asset_id)).toEqual([second.asset_id]);
    }
    photos.dispose();
  },
);

it("replaces an interior marker when its coordinates or name change", async () => {
  const first = { ...mapPhoto(1), latitude: 1, longitude: 1 },
    second = { ...mapPhoto(2), latitude: 2, longitude: 2 };
  const { view } = setup([first, second]);
  frame();
  await view.rerender({ items: [{ ...first, latitude: 4, name: "Moved photo.jpg" }, second] });
  frame();
  expect(mocks.marker).toHaveBeenLastCalledWith(
    [4, 1],
    expect.objectContaining({ title: "Moved photo.jpg" }),
  );
  expect(mocks.cluster.removeLayers.mock.calls.at(-1)![0]).toHaveLength(1);
});

it("updates visible rows when a reloaded interior row moves out of and into view", async () => {
  const first = { ...mapPhoto(1), latitude: 1 },
    second = { ...mapPhoto(2), latitude: 3 };
  const { props, view } = setup([first, second]);
  expect(props.onvisible).toHaveBeenLastCalledWith([first]);
  const outside = { ...first, latitude: 4 };
  await view.rerender({ items: [outside, second] });
  expect(props.onvisible).toHaveBeenLastCalledWith([]);
  const inside = { ...outside, latitude: 0 };
  await view.rerender({ items: [inside, second] });
  expect(props.onvisible).toHaveBeenLastCalledWith([inside]);
});
