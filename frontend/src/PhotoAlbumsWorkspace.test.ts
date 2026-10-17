import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import PhotoAlbumsIndex from "./PhotoAlbumsIndex.svelte";
import PhotosWorkspace from "./PhotosWorkspace.svelte";
import { Photos } from "./photos.svelte.js";
import { PhotoAlbums } from "./photoAlbums.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";
import {
  photo,
  mapPhoto,
  photoAlbum,
  albumResponse,
  savedSmartSet,
} from "./photo-test-fixtures.js";
import { parseQuery, queryFingerprint } from "./query.js";
import { createSavedQuery, definitionsChanged } from "./savedQueries.js";

beforeEach(() =>
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  ),
);
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});
const album = photoAlbum({ included_count: 2, member_count: 2 });

it("requires acknowledgement before retrying an album creation after a network failure", async () => {
  const fetcher = vi
    .fn()
    .mockRejectedValueOnce(new TypeError("Failed to fetch"))
    .mockResolvedValue(albumResponse([album]));
  vi.stubGlobal("fetch", fetcher);
  const albums = new PhotoAlbums("scoped", vi.fn());
  const onnavigate = vi.fn();
  render(PhotoAlbumsIndex, { albums, cache: new PhotoPreviewCache("scoped", vi.fn()), onnavigate });
  await fireEvent.click(screen.getAllByRole("button", { name: "New album" })[0]);
  await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), {
    target: { value: "Trip" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Create album" }));
  await waitFor(() => expect(albums.busy).toBe(false));
  await screen.findByText(
    'An album named "Trip" may already exist. Another attempt can create an extra album.',
  );
  expect((screen.getByRole("button", { name: "Create album" }) as HTMLButtonElement).disabled).toBe(
    true,
  );
  await fireEvent.click(screen.getByRole("button", { name: "View Albums" }));
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(onnavigate).toHaveBeenCalledWith("/photos/albums");
  await fireEvent.click(screen.getByRole("button", { name: "Refresh albums" }));
  await waitFor(() => expect(albums.loading).toBe(false));
  await fireEvent.click(screen.getByRole("button", { name: "Allow another album" }));
  await fireEvent.click(screen.getByRole("button", { name: "New album" }));
  await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), {
    target: { value: "Trip" },
  });
  const before = fetcher.mock.calls.length;
  fetcher.mockResolvedValueOnce(albumResponse(album));
  await fireEvent.click(screen.getByRole("button", { name: "Create album" }));
  await waitFor(() => expect(onnavigate).toHaveBeenCalledWith(`/photos/albums/${album.id}`));
  expect(fetcher).toHaveBeenCalledTimes(before + 2);
});

function setup(scoped = false, onnavigate = vi.fn(), mapMode = false) {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  HTMLElement.prototype.scrollIntoView = vi.fn();
  const photos = new Photos("scoped", vi.fn(), false, undefined, {
    projection: mapMode ? "map" : undefined,
  });
  photos.items = [photo(1, "2024-01-01"), photo(2, "2025-01-01")];
  photos.started = true;
  photos.total = 10000;
  if (scoped)
    photos.query = {
      ...photos.query,
      filters: { set_ids: [album.id] },
      sort: { field: "added_time", direction: "desc" },
    };
  vi.spyOn(photos, "refresh").mockResolvedValue(true);
  const albums = new PhotoAlbums("scoped", vi.fn(), async (id) => {
    if (photos.started && id && photos.query.filters?.set_ids?.includes(id)) await photos.refresh();
  });
  albums.items = [album];
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  const view = render(PhotosWorkspace, {
    photos,
    albums,
    cache,
    mapMode,
    tiles: {
      tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}",
      host: "tiles.example.org",
      attribution: "Example",
      max_zoom: 19,
    },
    albumID: scoped ? album.id : "",
    onnavigate,
    photoID: "",
    onphotochange: vi.fn(),
  });
  return { photos, albums, cache, view };
}

it.each(["off-page", "narrow", "every page loaded"])(
  "selects loaded photos and the whole query: %s",
  async (kind) => {
    const { photos } = setup();
    const selectAllResults = vi.spyOn(photos, "selectAllResults");
    const selectLoaded = vi.spyOn(photos, "selectLoaded");
    if (kind === "every page loaded") {
      photos.total = 2;
      photos.cursor = undefined;
      photos.selectLoaded();
      await fireEvent.click(await screen.findByRole("button", { name: "Select all 2 photos" }));
      expect(selectAllResults).toHaveBeenCalledOnce();
      return;
    }
    photos.selection = {
      selectedIDs: new Set(
        kind === "narrow" ? ["photo-1", "photo-2", "photo-3"] : ["photo-3", "photo-4"],
      ),
      anchorID: undefined,
    };
    const button = await screen.findByRole("button", { name: "Select loaded photos" });
    if (kind === "narrow") {
      expect((button as HTMLButtonElement).disabled).toBe(false);
      await fireEvent.click(button);
      expect(selectLoaded).toHaveBeenCalledOnce();
      return;
    }
    expect(screen.queryByRole("button", { name: "Select all 10,000 photos" })).toBeNull();
    await fireEvent.click(button);
    expect(selectLoaded).toHaveBeenCalledOnce();
    await fireEvent.click(await screen.findByRole("button", { name: "Select all 10,000 photos" }));
    expect(selectAllResults).toHaveBeenCalledOnce();
    selectLoaded.mockClear();
    await fireEvent.click(button);
    expect(selectLoaded).toHaveBeenCalledOnce();
  },
);

it.each(["delete", "duplicate", "rename"] as const)(
  "reloads the inspected %s dialog after a conflict and retries explicitly",
  async (kind) => {
    const operation = kind;
    const current = { ...album, revision: 2, name: "Current trip", included_count: 3 };
    let finish!: (value: Response) => void;
    const fetcher = vi
      .fn()
      .mockImplementationOnce(() => new Promise<Response>((resolve) => (finish = resolve)))
      .mockResolvedValueOnce(albumResponse([current]));
    vi.stubGlobal("fetch", fetcher);
    const onnavigate = vi.fn();
    const { albums } = setup(true, onnavigate);
    albums.targetID = album.id;
    await fireEvent.click(screen.getByRole("button", { name: "Album actions" }));
    await fireEvent.click(
      await screen.findByRole("menuitem", {
        name:
          operation === "delete"
            ? "Delete album…"
            : operation === "rename"
              ? "Rename"
              : "Duplicate…",
      }),
    );
    if (operation === "duplicate")
      await fireEvent.input(screen.getByRole("textbox", { name: "Copy name" }), {
        target: { value: "My copy" },
      });
    if (operation === "rename")
      await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), {
        target: { value: "Holiday" },
      });
    albums.items = [{ ...album, revision: 2 }];
    const label =
      operation === "delete" ? "Delete album" : operation === "rename" ? "Save" : "Duplicate album";
    await fireEvent.click(screen.getByRole("button", { name: label }));
    expect(fetcher.mock.calls[0][1].headers.get("If-Match")).toBe('"1"');
    if (operation !== "rename") {
      expect((screen.getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled).toBe(
        true,
      );
      await fireEvent.keyDown(window, { key: "Escape" });
      expect(screen.getByRole("dialog")).toBeTruthy();
    }
    finish(albumResponse({ detail: "Album changed", code: "stale_revision" }, 412));
    await screen.findByText("Album changed");
    await screen.findByText(
      operation === "delete"
        ? 'Delete "Current trip"? Its 3 photos stay in your library.'
        : "Current trip · 3 photos",
    );
    if (operation !== "delete")
      expect(
        (
          screen.getByRole("textbox", {
            name: operation === "rename" ? "Album name" : "Copy name",
          }) as HTMLInputElement
        ).value,
      ).toBe(operation === "rename" ? "Holiday" : "My copy");
    await waitFor(() =>
      expect((screen.getByRole("button", { name: label }) as HTMLButtonElement).disabled).toBe(
        false,
      ),
    );
    expect(fetcher).toHaveBeenCalledTimes(2);
    const saved = { ...current, revision: 3, name: "Holiday" };
    fetcher
      .mockResolvedValueOnce(
        albumResponse(
          operation === "delete"
            ? { ...album, revision: 2, deleted_at: "2025-01-01" }
            : operation === "rename"
              ? saved
              : { ...album, id: "22222222-2222-4222-8222-000000000068" },
        ),
      )
      .mockResolvedValueOnce(
        albumResponse(operation === "delete" ? [] : operation === "rename" ? [saved] : [album]),
      );
    await fireEvent.click(screen.getByRole("button", { name: label }));
    if (operation === "rename") {
      await waitFor(() => expect(screen.queryByRole("textbox", { name: "Album name" })).toBeNull());
      await screen.findByRole("heading", { name: "Holiday" });
      expect(fetcher).toHaveBeenCalledTimes(4);
      expect(albums.items[0].revision).toBe(3);
      expect(JSON.parse(fetcher.mock.calls[2][1].body)).toEqual({ name: "Holiday" });
    } else
      await waitFor(() =>
        expect(onnavigate).toHaveBeenCalledWith(
          operation === "delete"
            ? "/photos/albums"
            : "/photos/albums/22222222-2222-4222-8222-000000000068",
        ),
      );
    expect(fetcher.mock.calls[2][1].headers.get("If-Match")).toBe('"2"');
    if (operation === "delete") {
      expect(albums.items).toEqual([]);
      expect(albums.targetID).toBe("");
    }
  },
);

it("B opens the picker without a target, ignores typing, then adds to the chosen target", async () => {
  const { photos, albums } = setup();
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: "Select loaded photos" }));
  await fireEvent.keyDown(window, { key: "b" });
  const input = await screen.findByRole("combobox", { name: "Find or create an album" });
  const members = vi.spyOn(albums, "members").mockImplementation(async () => {
    albums.error = "Another action failed";
    return album;
  });
  albums.targetID = album.id;
  await fireEvent.keyDown(input, { key: "b" });
  expect(members).not.toHaveBeenCalled();
  await fireEvent.keyDown(input, { key: "Escape" });
  await fireEvent.keyDown(window, { key: "b" });
  await waitFor(() =>
    expect(members).toHaveBeenCalledWith(album, { asset_ids: ["photo-1", "photo-2"] }),
  );
  await fireEvent.keyDown(window, { key: "b", repeat: true });
  await fireEvent.keyDown(window, { key: "b", ctrlKey: true });
  expect(members).toHaveBeenCalledTimes(1);
  expect(albums.error).toBe("Another action failed");
});

it("refreshes previews and summary counts inside an album", async () => {
  const { photos, albums } = setup(true);
  const refresh = vi.spyOn(photos, "refresh").mockResolvedValue(true);
  const load = vi.spyOn(albums, "load").mockResolvedValue(true);
  await fireEvent.click(screen.getByRole("button", { name: "Refresh previews" }));
  expect(refresh).toHaveBeenCalledTimes(1);
  expect(load).toHaveBeenCalledTimes(1);
});

it.each(["uuid", "prefixed", "Unicode after deletion", "delayed create", "exact name"])(
  "chooses or creates albums from the picker: %s",
  async (kind) => {
    const created = { ...album, id: "22222222-2222-4222-8222-000000000070", name: "Summer" };
    let finish!: (response: Response) => void;
    const fetcher =
      kind === "delayed create"
        ? vi
            .fn()
            .mockImplementationOnce(() => new Promise<Response>((resolve) => (finish = resolve)))
            .mockResolvedValue(albumResponse([album, created]))
        : vi
            .fn()
            .mockResolvedValueOnce(albumResponse({ ...album, revision: 2 }))
            .mockResolvedValue(albumResponse([album]));
    if (kind === "delayed create" || kind === "exact name") vi.stubGlobal("fetch", fetcher);
    const { photos, albums } = setup();
    if (kind === "delayed create" || kind === "exact name") {
      await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
      await fireEvent.click(screen.getByRole("button", { name: /Add to album/ }));
      const input = screen.getByRole("combobox", { name: "Find or create an album" });
      await fireEvent.input(input, {
        target: { value: kind === "exact name" ? "Trip" : "Summer" },
      });
      if (kind === "exact name") {
        expect(screen.queryByRole("option", { name: 'Create album "Trip"' })).toBeNull();
        await fireEvent.keyDown(input, { key: "Enter" });
        await waitFor(() => expect(fetcher).toHaveBeenCalled());
        expect(fetcher.mock.calls[0][0]).toBe(`/api/v1/photos/albums/${album.id}/members/add`);
      } else {
        await fireEvent.mouseDown(
          await screen.findByRole("option", { name: 'Create album "Summer"' }),
        );
        photos.selection = {
          selectedIDs: new Set(Array.from({ length: 1001 }, (_, index) => `photo-${index + 1}`)),
          anchorID: undefined,
        };
        finish(albumResponse(created));
        await waitFor(() =>
          expect(
            fetcher.mock.calls.some(
              ([url]) => url === `/api/v1/photos/albums/${created.id}/members/add`,
            ),
          ).toBe(true),
        );
        const add = fetcher.mock.calls.find(
          ([url]) => url === `/api/v1/photos/albums/${created.id}/members/add`,
        )!;
        expect(JSON.parse(add[1].body).asset_ids).toEqual(["photo-1"]);
      }
      return;
    }
    const name =
      kind === "uuid" ? album.id : kind === "prefixed" ? `album:${album.id}` : "😀".repeat(129);
    const attempts = kind === "Unicode after deletion" ? 3 : 1;
    let nextID = 0;
    const create = vi.spyOn(albums, "create").mockImplementation(async (value) => {
      const item = { ...album, id: `created-${++nextID}`, name: value };
      albums.items = [...albums.items, item];
      return item;
    });
    const members = vi.spyOn(albums, "members").mockResolvedValue(album);
    await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
    await fireEvent.click(screen.getByRole("button", { name: "Select loaded photos" }));
    if (attempts === 1)
      await fireEvent.click(screen.getByRole("button", { name: "Select all 10,000 photos" }));
    for (let i = 0; i < attempts; i++) {
      if (i > 0)
        await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
      if (i > 0) albums.items = [album];
      await fireEvent.click(await screen.findByRole("button", { name: /Add to album/ }));
      const input = screen.getByRole("combobox", { name: "Find or create an album" });
      await fireEvent.input(input, { target: { value: name } });
      await fireEvent.mouseDown(
        await screen.findByRole("option", { name: `Create album "${name}"` }),
      );
      await waitFor(() => expect(members).toHaveBeenCalledTimes(i + 1));
      expect(members).toHaveBeenLastCalledWith(
        expect.objectContaining({ id: `created-${i + 1}` }),
        photos.scope(),
      );
    }
    expect(create).toHaveBeenCalledTimes(attempts);
    expect(create).toHaveBeenCalledWith(name);
    expect(albums.targetID).toBe(`created-${attempts}`);
    expect(members.mock.calls.map(([item]) => item.id)).toEqual(
      Array.from({ length: attempts }, (_, index) => `created-${index + 1}`),
    );
  },
);

it("waits for album initialization and delete completion before showing not found", async () => {
  const { albums } = setup(true);
  albums.items = [];
  await waitFor(() => expect(screen.queryByText("Album not found")).toBeNull());
  albums.initialized = true;
  await screen.findByText("Album not found");
  albums.busy = true;
  await waitFor(() => expect(screen.queryByText("Album not found")).toBeNull());
});

it.each(["loaded list", "failed list, name retry", "failed list, target retry"])(
  "reuses the acknowledged created album when retrying its failed membership: %s",
  async (kind) => {
    const created = { ...album, id: "22222222-2222-4222-8222-000000000067", name: "Summer" };
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(albumResponse(created))
      .mockResolvedValueOnce(
        kind === "loaded list"
          ? albumResponse([created])
          : albumResponse({ detail: "List unavailable" }, 503),
      )
      .mockResolvedValueOnce(albumResponse({ detail: "Add unavailable" }, 503))
      .mockResolvedValueOnce(
        kind === "loaded list"
          ? albumResponse([created])
          : albumResponse({ detail: "List unavailable" }, 503),
      )
      .mockResolvedValueOnce(albumResponse({ ...created, revision: 2 }))
      .mockResolvedValueOnce(albumResponse([created]));
    vi.stubGlobal("fetch", fetcher);
    const { albums } = setup();
    await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
    await fireEvent.click(screen.getByRole("button", { name: /Add to album/ }));
    await fireEvent.input(screen.getByRole("combobox", { name: "Find or create an album" }), {
      target: { value: "Summer" },
    });
    await fireEvent.mouseDown(await screen.findByRole("option", { name: 'Create album "Summer"' }));
    await screen.findByText("Add unavailable");
    await waitFor(() => expect(albums.busy).toBe(false));
    expect(albums.targetID).toBe(created.id);
    await screen.findByText("adds to Summer");
    const input = screen.getByRole("combobox", { name: "Find or create an album" });
    if (kind === "failed list, target retry") {
      await fireEvent.keyDown(input, { key: "Escape" });
      await fireEvent.keyDown(window, { key: "b" });
    } else {
      await fireEvent.input(input, { target: { value: "Summer" } });
      expect(screen.queryByRole("option", { name: 'Create album "Summer"' })).toBeNull();
      await fireEvent.mouseDown(await screen.findByRole("option", { name: "Summer" }));
    }
    await waitFor(() => expect(albums.notice).toBe("Added to Summer"));
    expect(
      fetcher.mock.calls.filter(
        ([url, init]) => url.endsWith("/photos/albums") && init.method === "POST",
      ),
    ).toHaveLength(1);
    expect(
      fetcher.mock.calls.filter(([url]) =>
        url.endsWith("/22222222-2222-4222-8222-000000000067/members/add"),
      ),
    ).toHaveLength(2);
  },
);

it("renders unavailable card observations and omits an unknown count from deletion", async () => {
  const { albums } = setup(true);
  albums.loadError = "Album list unavailable";
  await fireEvent.click(screen.getByRole("button", { name: "Album actions" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: "Delete album…" }));
  expect(screen.getByText('Delete "Trip"? Its photos stay in your library.')).toBeTruthy();
  cleanup();
  render(PhotoAlbumsIndex, {
    albums,
    cache: new PhotoPreviewCache("scoped", vi.fn()),
    onnavigate: vi.fn(),
  });
  expect(screen.getByText("Count unavailable")).toBeTruthy();
  expect(screen.getByText("Cover unavailable")).toBeTruthy();
  albums.loadError = "";
  albums.items = [{ ...album, included_count: 0 }];
  await screen.findByText("0 photos");
  await screen.findByText("No cover yet");
});

it("deduplicates shifted pages of smart sets", async () => {
  const payload = parseQuery('{"filters":{"kinds":["photo"]}}');
  const fingerprint = await queryFingerprint(payload);
  const saved = (index: number) =>
    savedSmartSet(payload, fingerprint, {
      id: `00000000-0000-4000-8000-${String(index).padStart(12, "0")}`,
      name: `Smart set ${index}`,
    });
  const offsets: number[] = [];
  const first = Array.from({ length: 100 }, (_, index) => saved(index));
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const query = new URL(String(input), "https://example.test").searchParams;
      const offset = query.get("offset");
      offsets.push(Number(offset));
      return offset === "0"
        ? Response.json({ items: first, total: 200, limit: 100, offset: 0 })
        : offset === "100"
          ? Response.json({
              items: [first[99], ...Array.from({ length: 99 }, (_, i) => saved(i + 100))],
              total: 201,
              limit: 100,
              offset: 100,
            })
          : Response.json({ items: [saved(199)], total: 201, limit: 100, offset: 200 });
    }),
  );
  const albums = new PhotoAlbums("scoped", vi.fn());
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotoAlbumsIndex, { albums, cache, onnavigate: vi.fn(), onopensmart: vi.fn() });
  await screen.findByRole("button", { name: "Smart set 99" });
  await fireEvent.click(screen.getByRole("button", { name: "Load more smart sets" }));
  await screen.findByRole("button", { name: "Smart set 100" });
  expect(screen.getAllByRole("button", { name: "Smart set 99" })).toHaveLength(1);
  await fireEvent.click(screen.getByRole("button", { name: "Load more smart sets" }));
  await screen.findByRole("button", { name: "Smart set 199" });
  expect(offsets).toEqual([0, 100, 200]);
  expect(screen.queryByRole("button", { name: "Load more smart sets" })).toBeNull();
  albums.dispose();
  await cache.dispose();
});

it("refreshes the smart-set subscription after creating a definition", async () => {
  const scope = parseQuery('{"filters":{"kinds":["photo"]}}');
  const initial = savedSmartSet(scope, await queryFingerprint(scope), { name: "Review" });
  const newest = { ...initial, id: "00000000-0000-4000-8000-000000000002", name: "New smart set" };
  let added = false;
  const offsets: number[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init: RequestInit) => {
      if (init?.method === "POST") {
        added = true;
        return Response.json(newest, { headers: { ETag: '"1"' } });
      }
      offsets.push(
        Number(new URL(String(input), "https://example.test").searchParams.get("offset")),
      );
      return Response.json({
        items: added ? [initial, newest] : [initial],
        total: added ? 2 : 1,
        limit: 100,
        offset: 0,
      });
    }),
  );
  const albums = new PhotoAlbums("scoped", vi.fn());
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotoAlbumsIndex, { albums, cache, onnavigate: vi.fn(), onopensmart: vi.fn() });
  await screen.findByRole("button", { name: initial.name });
  await createSavedQuery("scoped", {
    name: newest.name,
    description: initial.description,
    kind: "query",
    payload: scope,
  });
  expect(await screen.findByRole("button", { name: "New smart set" })).toBeTruthy();
  expect(screen.getByRole("button", { name: initial.name })).toBeTruthy();
  expect(offsets).toEqual([0, 0]);
  cleanup();
  albums.dispose();
  await cache.dispose();
});

it("disables smart-set opening during a Library mutation", async () => {
  const payload = parseQuery('{"filters":{"kinds":["photo","video"]}}');
  const entry = savedSmartSet(payload, await queryFingerprint(payload));
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValue(
        Response.json(
          { items: [entry], total: 1, limit: 100, offset: 0 },
          { headers: { ETag: '"1"' } },
        ),
      ),
  );
  const albums = new PhotoAlbums("scoped", vi.fn());
  render(PhotoAlbumsIndex, {
    albums,
    cache: new PhotoPreviewCache("scoped", vi.fn()),
    onnavigate: vi.fn(),
    onopensmart: vi.fn(),
    smartDisabled: true,
  });
  const button = await screen.findByRole("button", { name: entry.name });
  expect((button as HTMLButtonElement).disabled).toBe(true);
  albums.dispose();
});

it("resets the empty index create dialog after cancelled attempts", async () => {
  const albums = new PhotoAlbums("scoped", vi.fn());
  render(PhotoAlbumsIndex, {
    albums,
    cache: new PhotoPreviewCache("scoped", vi.fn()),
    onnavigate: vi.fn(),
  });
  for (let attempt = 0; attempt < 2; attempt++) {
    await fireEvent.click(screen.getAllByRole("button", { name: "New album" })[1]);
    expect((screen.getByRole("textbox", { name: "Album name" }) as HTMLInputElement).value).toBe(
      "",
    );
    await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), {
      target: { value: "Cancelled" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  }
});

it.each([false, true])(
  "disables explicit album changes over 1,000 photos until whole-query selection, map=%s",
  async (mapMode) => {
    const { photos, albums } = setup(!mapMode, vi.fn(), mapMode);
    photos.items = Array.from({ length: 1001 }, (_, id) => (mapMode ? mapPhoto(id) : photo(id)));
    photos.total = 1001;
    photos.selectLoaded();
    const members = vi.spyOn(albums, "members").mockResolvedValue(album);
    const picker = await screen.findByRole("button", { name: /Add to album/ });
    expect((picker as HTMLButtonElement).disabled).toBe(true);
    if (mapMode) {
      await screen.findByText("Select 1,000 photos or fewer to add to an album.");
      expect(screen.queryByRole("button", { name: "Select all 1,001 photos" })).toBeNull();
      photos.check(
        "photo-0",
        false,
        false,
        photos.items.map((item) => item.asset_id),
      );
      await waitFor(() => expect((picker as HTMLButtonElement).disabled).toBe(false));
      expect(screen.queryByText("Select 1,000 photos or fewer to add to an album.")).toBeNull();
      return;
    }
    expect(
      (screen.getByRole("button", { name: "Remove from album" }) as HTMLButtonElement).disabled,
    ).toBe(true);
    await screen.findByText("Select all 1,001 photos to add more than 1,000 at once.");
    await fireEvent.click(screen.getByRole("button", { name: "Select all 1,001 photos" }));
    expect((picker as HTMLButtonElement).disabled).toBe(false);
    await fireEvent.click(screen.getByRole("button", { name: "Remove from album" }));
    expect(members).toHaveBeenCalledWith(album, { query: photos.query }, true);
  },
);

it("renders added order and cancels rename without a write", async () => {
  const { albums, view } = setup(true);
  await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  expect(
    [...view.container.querySelectorAll("[data-asset]")].map((item) =>
      item.getAttribute("data-asset"),
    ),
  ).toEqual(["photo-1", "photo-2"]);
  const update = vi.spyOn(albums, "update").mockResolvedValue(album);
  await fireEvent.click(screen.getByRole("button", { name: "Album actions" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: "Rename" }));
  expect((screen.getByRole("button", { name: "Star album" }) as HTMLButtonElement).disabled).toBe(
    true,
  );
  expect(
    (screen.getByRole("button", { name: "Album actions" }) as HTMLButtonElement).disabled,
  ).toBe(true);
  await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), {
    target: { value: "Canceled" },
  });
  await fireEvent.keyDown(screen.getByRole("textbox", { name: "Album name" }), { key: "Escape" });
  expect(update).not.toHaveBeenCalled();
});

it.each(["star", "cover"])("applies the selected album's %s action", async (action) => {
  const { albums } = setup(true);
  const update = vi.spyOn(albums, "update").mockResolvedValue(album);
  const cover = vi.spyOn(albums, "cover").mockResolvedValue(album);
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  await fireEvent.click(
    screen.getByRole("button", { name: action === "star" ? "Star album" : "Use as cover" }),
  );
  if (action === "star") expect(update).toHaveBeenCalledWith(album, { starred: true });
  else expect(cover).toHaveBeenCalledWith(album, "photo-1");
});

it("retains one error and selection when a failed membership completes after remounting the index", async () => {
  let finish!: (value: Response) => void;
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockImplementationOnce(() => new Promise<Response>((resolve) => (finish = resolve)))
      .mockResolvedValueOnce(albumResponse([album])),
  );
  const { photos, albums, cache, view } = setup();
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  photos.selectLoaded();
  albums.targetID = album.id;
  await screen.findByText("adds to Trip");
  await fireEvent.keyDown(window, { key: "b" });
  await waitFor(() => expect(albums.busy).toBe(true));
  view.unmount();
  render(PhotoAlbumsIndex, { albums, cache, onnavigate: vi.fn() });
  finish(albumResponse({ detail: "Add unavailable" }, 503));
  await screen.findByText("Add unavailable");
  expect(screen.getAllByRole("alert")).toHaveLength(1);
  expect(photos.selection.selectedIDs.size).toBe(2);
});

it("shows a failed smart-set notification and reloads the rendered list on recovery", async () => {
  const scope = parseQuery('{"filters":{"kinds":["photo"]}}');
  const saved = savedSmartSet(scope, await queryFingerprint(scope));
  let failing = false;
  let name = "Original smart set";
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      failing
        ? Response.json({ detail: "Definitions unavailable" }, { status: 503 })
        : Response.json({ items: [{ ...saved, name }], total: 1, limit: 100, offset: 0 }),
    ),
  );
  const albums = new PhotoAlbums("scoped", vi.fn());
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  render(PhotoAlbumsIndex, { albums, cache, onnavigate: vi.fn(), onopensmart: vi.fn() });
  await screen.findByRole("button", { name });
  failing = true;
  await definitionsChanged("scoped").catch(() => {});
  expect(screen.getByRole("button", { name: "Retry smart sets" })).toBeTruthy();
  failing = false;
  name = "Renamed smart set";
  await definitionsChanged("scoped");
  await screen.findByRole("button", { name });
  expect(screen.queryByText("Definitions unavailable")).toBeNull();
  albums.dispose();
  await cache.dispose();
});
