import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import PhotoAlbumsIndex from "./PhotoAlbumsIndex.svelte";
import PhotosWorkspace from "./PhotosWorkspace.svelte";
import { Photos } from "./photos.svelte.js";
import { PhotoAlbums } from "./photoAlbums.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";
import { photo, photoAlbum, albumResponse } from "./photo-test-fixtures.js";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });
const album = photoAlbum({ included_count: 2, member_count: 2 });

it.each(["network", "server", "rejection"])("requires acknowledgement before retrying an album creation with a %s failure", async failure => {
  const fetcher = vi.fn().mockImplementationOnce(async () => {
    if (failure === "network") throw new TypeError("Failed to fetch");
    return albumResponse({ detail: "Create unavailable" }, failure === "server" ? 503 : 400);
  }).mockResolvedValue(albumResponse([album]));
  vi.stubGlobal("fetch", fetcher);
  const albums = new PhotoAlbums("scoped", vi.fn());
  const onnavigate = vi.fn();
  render(PhotoAlbumsIndex, { albums, cache: new PhotoPreviewCache("scoped", vi.fn()), onnavigate });
  await fireEvent.click(screen.getAllByRole("button", { name: "New album" })[0]);
  await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), { target: { value: "Trip" } });
  await fireEvent.click(screen.getByRole("button", { name: "Create album" }));
  await waitFor(() => expect(albums.busy).toBe(false));
  if (failure !== "rejection") {
    await screen.findByText('An album named "Trip" may already exist. Another attempt can create an extra album.');
    expect((screen.getByRole("button", { name: "Create album" }) as HTMLButtonElement).disabled).toBe(true);
    await albums.create("Trip");
    await albums.duplicate(album, "Trip copy");
    expect(fetcher).toHaveBeenCalledTimes(2);
    await fireEvent.click(screen.getByRole("button", { name: "View Albums" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(onnavigate).toHaveBeenCalledWith("/photos/albums");
    await fireEvent.click(screen.getByRole("button", { name: "Refresh albums" }));
    await waitFor(() => expect(albums.loading).toBe(false));
    await albums.create("Trip");
    expect(fetcher).toHaveBeenCalledTimes(3);
    await fireEvent.click(screen.getByRole("button", { name: "Allow another album" }));
    await fireEvent.click(screen.getByRole("button", { name: "New album" }));
    await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), { target: { value: "Trip" } });
  } else {
    await screen.findByText("Create unavailable");
    expect(albums.unconfirmed).toBeUndefined();
  }
  const before = fetcher.mock.calls.length;
  fetcher.mockResolvedValueOnce(albumResponse(album));
  await fireEvent.click(screen.getByRole("button", { name: "Create album" }));
  await waitFor(() => expect(onnavigate).toHaveBeenCalledWith(`/photos/albums/${album.id}`));
  expect(fetcher).toHaveBeenCalledTimes(before + 2);
});

function setup(scoped = false, onnavigate = vi.fn()) {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  HTMLElement.prototype.scrollIntoView = vi.fn();
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1, "2024-01-01"), photo(2, "2025-01-01")]; photos.started = true; photos.total = 10000;
  if (scoped) photos.query = { ...photos.query, filters: { set_ids: [album.id] }, sort: { field: "added_time", direction: "desc" } };
  vi.spyOn(photos, "refresh").mockResolvedValue();
  const albums = new PhotoAlbums("scoped", vi.fn(), async id => { if (photos.started && photos.query.filters?.set_ids?.includes(id)) await photos.refresh(); }); albums.items = [album];
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  const view = render(PhotosWorkspace, { photos, albums, cache, albumID: scoped ? album.id : "", onnavigate });
  return { photos, albums, cache, view };
}

it.each(["off-page", "narrow", "every page loaded"])("selects loaded photos and the whole query: %s", async kind => {
  const { photos } = setup();
  if (kind === "every page loaded") {
    photos.total = 2; photos.cursor = undefined;
    photos.selectLoaded();
    await fireEvent.click(await screen.findByRole("button", { name: "Select all 2 photos" }));
    expect(photos.scope()).toEqual({ query: photos.query });
    return;
  }
  photos.selection = { selectedIDs: new Set(kind === "narrow" ? ["photo-1", "photo-2", "photo-3"] : ["photo-3", "photo-4"]), anchorID: undefined };
  const button = await screen.findByRole("button", { name: "Select loaded photos" });
  if (kind === "narrow") {
    expect((button as HTMLButtonElement).disabled).toBe(false);
    await fireEvent.click(button);
    expect([...photos.selection.selectedIDs]).toEqual(["photo-1", "photo-2"]);
    expect((button as HTMLButtonElement).disabled).toBe(true);
    return;
  }
  expect(screen.queryByRole("button", { name: "Select all 10,000 photos" })).toBeNull();
  await fireEvent.click(button);
  expect([...photos.selection.selectedIDs]).toEqual(photos.items.map(item => item.asset_id));
  await fireEvent.click(await screen.findByRole("button", { name: "Select all 10,000 photos" }));
  expect(photos.scope()).toEqual({ query: photos.query });
  await fireEvent.click(button);
  expect(photos.allResults).toBe(false);
  expect(photos.scope()).toEqual({ asset_ids: photos.items.map(item => item.asset_id) });
});

it.each(["delete", "duplicate"] as const)("reloads the inspected %s dialog after a conflict and retries explicitly", async operation => {
  let finish!: (value: Response) => void;
  const fetcher = vi.fn().mockImplementationOnce(() => new Promise<Response>(resolve => finish = resolve)).mockResolvedValueOnce(albumResponse([{ ...album, revision: 2, name: "Current trip", included_count: 3 }]));
  vi.stubGlobal("fetch", fetcher);
  const onnavigate = vi.fn();
  const { albums } = setup(true, onnavigate);
  await fireEvent.click(screen.getByRole("button", { name: "Album actions" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: operation === "delete" ? "Delete album…" : "Duplicate…" }));
  if (operation === "duplicate") await fireEvent.input(screen.getByRole("textbox", { name: "Copy name" }), { target: { value: "My copy" } });
  albums.items = [{ ...album, revision: 2 }];
  const label = operation === "delete" ? "Delete album" : "Duplicate album";
  await fireEvent.click(screen.getByRole("button", { name: label }));
  expect(fetcher.mock.calls[0][1].headers.get("If-Match")).toBe('"1"');
  expect((screen.getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled).toBe(true);
  await fireEvent.keyDown(window, { key: "Escape" });
  expect(screen.getByRole("dialog")).toBeTruthy();
  finish(albumResponse({ detail: "Album changed" }, 412));
  await screen.findByText("Album changed");
  await screen.findByText(operation === "delete" ? 'Delete "Current trip"? Its 3 photos stay in your library.' : "Current trip · 3 photos");
  if (operation === "duplicate") expect((screen.getByRole("textbox", { name: "Copy name" }) as HTMLInputElement).value).toBe("My copy");
  await waitFor(() => expect((screen.getByRole("button", { name: label }) as HTMLButtonElement).disabled).toBe(false));
  fetcher.mockResolvedValueOnce(albumResponse(operation === "delete" ? { ...album, revision: 2, deleted_at: "2025-01-01" } : { ...album, id: "22222222-2222-4222-8222-000000000068" })).mockResolvedValueOnce(albumResponse([album]));
  await fireEvent.click(screen.getByRole("button", { name: label }));
  await waitFor(() => expect(onnavigate).toHaveBeenCalledWith(operation === "delete" ? "/photos/albums" : "/photos/albums/22222222-2222-4222-8222-000000000068"));
  expect(fetcher.mock.calls[2][1].headers.get("If-Match")).toBe('"2"');
});

it("B opens the picker without a target, ignores typing, then adds to the chosen target", async () => {
  const { photos, albums } = setup();
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: "Select loaded photos" }));
  await fireEvent.keyDown(window, { key: "b" });
  const input = await screen.findByRole("combobox", { name: "Find or create an album" });
  const members = vi.spyOn(albums, "members").mockImplementation(async () => { albums.error = "Another action failed"; return album; });
  albums.targetID = album.id;
  await fireEvent.keyDown(input, { key: "b" });
  expect(members).not.toHaveBeenCalled();
  await fireEvent.keyDown(input, { key: "Escape" });
  await fireEvent.keyDown(window, { key: "b" });
  await waitFor(() => expect(members).toHaveBeenCalledWith(album, { asset_ids: ["photo-1", "photo-2"] }));
  await fireEvent.keyDown(window, { key: "b", repeat: true });
  await fireEvent.keyDown(window, { key: "b", ctrlKey: true });
  expect(members).toHaveBeenCalledTimes(1);
  expect(albums.error).toBe("Another action failed");
});

it("refreshes previews and summary counts inside an album", async () => {
  const { photos, albums } = setup(true);
  const refresh = vi.spyOn(photos, "refresh").mockResolvedValue();
  const load = vi.spyOn(albums, "load").mockResolvedValue(true);
  await fireEvent.click(screen.getByRole("button", { name: "Refresh previews" }));
  expect(refresh).toHaveBeenCalledTimes(1);
  expect(load).toHaveBeenCalledTimes(1);
});

it.each(["uuid", "prefixed", "Unicode after deletion", "delayed create", "exact name"])("chooses or creates albums from the picker: %s", async kind => {
  const created = { ...album, id: "22222222-2222-4222-8222-000000000070", name: "Summer" };
  let finish!: (response: Response) => void;
  const fetcher = kind === "delayed create"
    ? vi.fn().mockImplementationOnce(() => new Promise<Response>(resolve => finish = resolve)).mockResolvedValue(albumResponse([album, created]))
    : vi.fn().mockResolvedValueOnce(albumResponse({ ...album, revision: 2 })).mockResolvedValue(albumResponse([album]));
  if (kind === "delayed create" || kind === "exact name") vi.stubGlobal("fetch", fetcher);
  const { photos, albums } = setup();
  if (kind === "delayed create" || kind === "exact name") {
    await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
    await fireEvent.click(screen.getByRole("button", { name: /Add to album/ }));
    const input = screen.getByRole("combobox", { name: "Find or create an album" });
    await fireEvent.input(input, { target: { value: kind === "exact name" ? "Trip" : "Summer" } });
    if (kind === "exact name") {
      expect(screen.queryByRole("option", { name: 'Create album "Trip"' })).toBeNull();
      await fireEvent.keyDown(input, { key: "Enter" });
      await waitFor(() => expect(fetcher).toHaveBeenCalled());
      expect(fetcher.mock.calls[0][0]).toBe(`/api/v1/photos/albums/${album.id}/members/add`);
    } else {
      await fireEvent.mouseDown(await screen.findByRole("option", { name: 'Create album "Summer"' }));
      photos.selection = { selectedIDs: new Set(Array.from({ length: 1001 }, (_, index) => `photo-${index + 1}`)), anchorID: undefined };
      finish(albumResponse(created));
      await waitFor(() => expect(fetcher.mock.calls.some(([url]) => url === `/api/v1/photos/albums/${created.id}/members/add`)).toBe(true));
      const add = fetcher.mock.calls.find(([url]) => url === `/api/v1/photos/albums/${created.id}/members/add`)!;
      expect(JSON.parse(add[1].body).asset_ids).toEqual(["photo-1"]);
    }
    return;
  }
  const name = kind === "uuid" ? album.id : kind === "prefixed" ? `album:${album.id}` : "😀".repeat(129);
  const attempts = kind === "Unicode after deletion" ? 3 : 1;
  let nextID = 0;
  const create = vi.spyOn(albums, "create").mockImplementation(async value => {
    const item = { ...album, id: `created-${++nextID}`, name: value };
    albums.items = [...albums.items, item]; return item;
  });
  const members = vi.spyOn(albums, "members").mockResolvedValue(album);
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: "Select loaded photos" }));
  if (attempts === 1) await fireEvent.click(screen.getByRole("button", { name: "Select all 10,000 photos" }));
  for (let i = 0; i < attempts; i++) {
    if (i > 0) await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
    if (i > 0) albums.items = [album];
    await fireEvent.click(await screen.findByRole("button", { name: /Add to album/ }));
    const input = screen.getByRole("combobox", { name: "Find or create an album" });
    expect(input.hasAttribute("maxlength")).toBe(false);
    await fireEvent.input(input, { target: { value: name } });
    await fireEvent.mouseDown(await screen.findByRole("option", { name: `Create album "${name}"` }));
    await waitFor(() => expect(members).toHaveBeenCalledTimes(i + 1));
    expect(members).toHaveBeenLastCalledWith(expect.objectContaining({ id: `created-${i + 1}` }), photos.scope());
  }
  expect(create).toHaveBeenCalledTimes(attempts);
  expect(create).toHaveBeenCalledWith(name);
  expect(albums.targetID).toBe(`created-${attempts}`);
  expect(members.mock.calls.map(([item]) => item.id)).toEqual(Array.from({ length: attempts }, (_, index) => `created-${index + 1}`));
});

it("waits for album initialization and delete completion before showing not found", async () => {
  const { albums } = setup(true);
  albums.items = [];
  await waitFor(() => expect(screen.queryByText("Album not found")).toBeNull());
  albums.initialized = true;
  await screen.findByText("Album not found");
  albums.busy = true;
  await waitFor(() => expect(screen.queryByText("Album not found")).toBeNull());
});

it("reuses the acknowledged created album when retrying its failed membership", async () => {
  const created = { ...album, id: "22222222-2222-4222-8222-000000000067", name: "Summer" };
  const fetcher = vi.fn()
    .mockResolvedValueOnce(albumResponse(created))
    .mockResolvedValueOnce(albumResponse([created]))
    .mockResolvedValueOnce(albumResponse({ detail: "Add unavailable" }, 503))
    .mockResolvedValueOnce(albumResponse([created]))
    .mockResolvedValueOnce(albumResponse({ ...created, revision: 2 }))
    .mockResolvedValueOnce(albumResponse([created]));
  vi.stubGlobal("fetch", fetcher);
  const { albums } = setup();
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: /Add to album/ }));
  await fireEvent.input(screen.getByRole("combobox", { name: "Find or create an album" }), { target: { value: "Summer" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: 'Create album "Summer"' }));
  await screen.findByText("Add unavailable");
  await fireEvent.input(screen.getByRole("combobox", { name: "Find or create an album" }), { target: { value: "" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Summer" }));
  await waitFor(() => expect(albums.notice).toBe("Added to Summer · now 2 photos"));
  expect(fetcher.mock.calls.filter(([url, init]) => url.endsWith("/photos/albums") && init.method === "POST")).toHaveLength(1);
  expect(fetcher.mock.calls.filter(([url]) => url.endsWith("/22222222-2222-4222-8222-000000000067/members/add"))).toHaveLength(2);
});

it("renders unavailable card observations and omits an unknown count from deletion", async () => {
  const { albums } = setup(true); albums.loadError = "Album list unavailable";
  await fireEvent.click(screen.getByRole("button", { name: "Album actions" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: "Delete album…" }));
  expect(screen.getByText('Delete "Trip"? Its photos stay in your library.')).toBeTruthy();
  cleanup();
  render(PhotoAlbumsIndex, { albums, cache: new PhotoPreviewCache("scoped", vi.fn()), onnavigate: vi.fn() });
  expect(screen.getByText("Count unavailable")).toBeTruthy(); expect(screen.getByText("Cover unavailable")).toBeTruthy();
  albums.loadError = ""; albums.items = [{ ...album, included_count: 0 }];
  await screen.findByText("0 photos"); await screen.findByText("No cover yet");
});

it("resets the empty index create dialog after cancelled attempts", async () => {
  const albums = new PhotoAlbums("scoped", vi.fn());
  render(PhotoAlbumsIndex, { albums, cache: new PhotoPreviewCache("scoped", vi.fn()), onnavigate: vi.fn() });
  for (let attempt = 0; attempt < 2; attempt++) {
    await fireEvent.click(screen.getAllByRole("button", { name: "New album" })[1]);
    expect((screen.getByRole("textbox", { name: "Album name" }) as HTMLInputElement).value).toBe("");
    await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), { target: { value: "Cancelled" } });
    await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  }
});

it("disables explicit album changes over 1,000 photos until whole-query selection", async () => {
  const { photos, albums } = setup(true);
  photos.items = Array.from({ length: 1001 }, (_, id) => photo(id)); photos.total = 1001; photos.selectLoaded();
  const members = vi.spyOn(albums, "members").mockResolvedValue(album);
  const picker = await screen.findByRole("button", { name: /Add to album/ });
  expect((picker as HTMLButtonElement).disabled).toBe(true);
  expect((screen.getByRole("button", { name: "Remove from album" }) as HTMLButtonElement).disabled).toBe(true);
  await screen.findByText("Select all 1,001 photos to add more than 1,000 at once.");
  await fireEvent.click(screen.getByRole("button", { name: "Select all 1,001 photos" }));
  expect((picker as HTMLButtonElement).disabled).toBe(false);
  await fireEvent.click(screen.getByRole("button", { name: "Remove from album" }));
  expect(members).toHaveBeenCalledWith(album, { query: photos.query }, true);
});

it("renders added order, cancels rename, and saves the retained draft after a conflict", async () => {
  const current = { ...album, revision: 2, name: "Current trip", included_count: 3 };
  const fetcher = vi.fn().mockResolvedValueOnce(albumResponse({ detail: "Album changed", code: "stale_revision" }, 412)).mockResolvedValueOnce(albumResponse([current])).mockResolvedValueOnce(albumResponse({ ...current, revision: 3, name: "Holiday" })).mockResolvedValueOnce(albumResponse([{ ...current, revision: 3, name: "Holiday" }]));
  vi.stubGlobal("fetch", fetcher);
  const { albums, view } = setup(true);
  await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  expect([...view.container.querySelectorAll("[data-asset]")].map(item => item.getAttribute("data-asset"))).toEqual(["photo-1", "photo-2"]);
  const update = vi.spyOn(albums, "update");
  await fireEvent.click(screen.getByRole("button", { name: "Album actions" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: "Rename" }));
  expect((screen.getByRole("button", { name: "Star album" }) as HTMLButtonElement).disabled).toBe(true);
  expect((screen.getByRole("button", { name: "Album actions" }) as HTMLButtonElement).disabled).toBe(true);
  await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), { target: { value: "Canceled" } });
  await fireEvent.keyDown(screen.getByRole("textbox", { name: "Album name" }), { key: "Escape" });
  expect(update).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole("button", { name: "Album actions" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: "Rename" }));
  await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), { target: { value: "Holiday" } });
  await fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await screen.findByText("Album changed");
  await waitFor(() => expect(albums.busy).toBe(false));
  expect(fetcher).toHaveBeenCalledTimes(2);
  await screen.findByText("Current trip · 3 photos");
  expect((screen.getByRole("textbox", { name: "Album name" }) as HTMLInputElement).value).toBe("Holiday");
  await fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(4));
  expect(fetcher.mock.calls[2][1].headers.get("If-Match")).toBe('"2"');
  expect(JSON.parse(fetcher.mock.calls[2][1].body)).toEqual({ name: "Holiday" });
  await waitFor(() => expect(screen.queryByRole("textbox", { name: "Album name" })).toBeNull());
});

it.each(["star", "cover", "remove"])("applies the selected album's %s action", async action => {
  const { photos, albums } = setup(true);
  const update = vi.spyOn(albums, "update").mockResolvedValue(album);
  const cover = vi.spyOn(albums, "cover").mockResolvedValue(album);
  const members = vi.spyOn(albums, "members").mockResolvedValue(album);
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: action === "star" ? "Star album" : action === "cover" ? "Use as cover" : "Remove from album" }));
  if (action === "star") expect(update).toHaveBeenCalledWith(album, { starred: true });
  else if (action === "cover") expect(cover).toHaveBeenCalledWith(album, "photo-1");
  else expect(members).toHaveBeenCalledWith(album, photos.scope(), true);
});

it.each(["add", "remove", "add failure"])("refreshes the affected album model after membership %s completes while unmounted", async action => {
  let finish!: (value: Response) => void;
  vi.stubGlobal("fetch", vi.fn().mockImplementationOnce(() => new Promise<Response>(resolve => finish = resolve)).mockResolvedValueOnce(albumResponse([album])));
  const { photos, albums, cache, view } = setup(action !== "add failure");
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  photos.selectLoaded(); albums.targetID = album.id;
  await screen.findByText("adds to Trip");
  if (action !== "remove") await fireEvent.keyDown(window, { key: "b" });
  else await fireEvent.click(await screen.findByRole("button", { name: "Remove from album" }));
  await waitFor(() => expect(albums.busy).toBe(true));
  view.unmount();
  if (action === "add failure") render(PhotoAlbumsIndex, { albums, cache, onnavigate: vi.fn() });
  finish(action === "add failure" ? albumResponse({ detail: "Add unavailable" }, 503) : albumResponse({ ...album, revision: 2 }));
  if (action === "add failure") {
    await screen.findByText("Add unavailable");
    expect(screen.getAllByRole("alert")).toHaveLength(1);
    expect(photos.selection.selectedIDs.size).toBe(2);
    return;
  }
  await waitFor(() => expect(photos.refresh).toHaveBeenCalledOnce());
  expect(photos.refresh).toHaveBeenCalledWith();
});
