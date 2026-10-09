import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import PhotoAlbumsIndex from "./PhotoAlbumsIndex.svelte";
import PhotosWorkspace from "./PhotosWorkspace.svelte";
import { Photos } from "./photos.svelte.js";
import { PhotoAlbums } from "./photoAlbums.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";
import { photo, photoAlbum } from "./photo-test-fixtures.js";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });
const album = photoAlbum({ included_count: 2, member_count: 2 });
function setup(scoped = false, onnavigate = vi.fn()) {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  HTMLElement.prototype.scrollIntoView = vi.fn();
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1, "2024-01-01"), photo(2, "2025-01-01")]; photos.started = true; photos.total = 10000;
  if (scoped) photos.query = { ...photos.query, filters: { set_ids: [album.id] }, sort: { field: "added_time", direction: "desc" } };
  const albums = new PhotoAlbums("scoped", vi.fn()); albums.items = [album];
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  const view = render(PhotosWorkspace, { photos, albums, cache, albumID: scoped ? album.id : "", onnavigate });
  return { photos, albums, cache, view };
}

it("selects loaded photos by ID when equally many selected photos are off-page", async () => {
  const { photos } = setup();
  photos.selection = { selectedIDs: new Set(["photo-3", "photo-4"]), anchorID: undefined };
  const button = await screen.findByRole("button", { name: "Select loaded photos" });
  expect((button as HTMLButtonElement).disabled).toBe(false);
  expect(screen.queryByRole("button", { name: "Select all 10,000 photos" })).toBeNull();
  await fireEvent.click(button);
  expect([...photos.selection.selectedIDs]).toEqual(photos.items.map(item => item.asset_id));
  await screen.findByRole("button", { name: "Select all 10,000 photos" });
});

it.each(["uuid", "prefixed"])("creates an album with a %s existing-album ID as its name", async kind => {
  const { photos, albums } = setup();
  const name = kind === "uuid" ? album.id : `album:${album.id}`;
  const created = { ...album, id: "created", name };
  const create = vi.spyOn(albums, "create").mockResolvedValue(created);
  const members = vi.spyOn(albums, "members").mockResolvedValue(created);
  photos.selectLoaded();
  await fireEvent.click(await screen.findByRole("button", { name: /Add to album/ }));
  await fireEvent.input(screen.getByRole("combobox", { name: "Find or create an album" }), { target: { value: name } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: `Create album "${name}"` }));
  await waitFor(() => expect(create).toHaveBeenCalledWith(name));
  expect(members).toHaveBeenCalledWith(created, photos.scope(), photos, false, expect.any(Function));
});

it.each(["picker add", "picker create", "index create", "delete", "duplicate"] as const)("keeps a delayed %s failure visible after navigation", async operation => {
  let finish!: (response: Response) => void;
  const fetcher = vi.fn().mockImplementationOnce(() => new Promise<Response>(resolve => finish = resolve)).mockResolvedValue(new Response(JSON.stringify([album])));
  vi.stubGlobal("fetch", fetcher);
  const { photos, albums, cache, view } = setup(operation === "delete" || operation === "duplicate");
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  let unmount = view.unmount;
  if (operation.startsWith("picker")) {
    await fireEvent.click(screen.getByRole("button", { name: /Add to album/ }));
    if (operation === "picker create") await fireEvent.input(screen.getByRole("combobox", { name: "Find or create an album" }), { target: { value: "Summer" } });
    await fireEvent.mouseDown(await screen.findByRole("option", { name: operation === "picker create" ? 'Create album "Summer"' : "Trip" }));
  } else if (operation === "index create") {
    view.unmount();
    unmount = render(PhotoAlbumsIndex, { albums, cache, onnavigate: vi.fn() }).unmount;
    await fireEvent.click(screen.getByRole("button", { name: "New album" }));
    await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), { target: { value: "Summer" } });
    await fireEvent.click(screen.getByRole("button", { name: "Create album" }));
  } else {
    await fireEvent.click(screen.getByRole("button", { name: "Album actions" }));
    await fireEvent.click(await screen.findByRole("menuitem", { name: operation === "delete" ? "Delete album…" : "Duplicate…" }));
    await fireEvent.click(screen.getByRole("button", { name: operation === "delete" ? "Delete album" : "Duplicate album" }));
  }
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));
  unmount();
  if (operation === "index create") render(PhotosWorkspace, { photos, albums, cache });
  else render(PhotoAlbumsIndex, { albums, cache, onnavigate: vi.fn() });
  finish(new Response(JSON.stringify({ detail: "Album change unavailable" }), { status: 503 }));
  await screen.findByText("Album change unavailable");
  await waitFor(() => expect(albums.busy).toBe(false));
  expect(screen.getAllByRole("alert")).toHaveLength(1);
  expect(albums.error).toBe("Album change unavailable");
  expect(photos.selection.selectedIDs.has("photo-1")).toBe(true);
});

it.each(["create", "delete", "duplicate"] as const)("stops delayed %s success from navigating a destroyed view", async operation => {
  let finish!: (response: Response) => void;
  const fetcher = vi.fn().mockImplementationOnce(() => new Promise<Response>(resolve => finish = resolve)).mockResolvedValue(new Response(JSON.stringify([album])));
  vi.stubGlobal("fetch", fetcher);
  const onnavigate = vi.fn();
  const { albums, cache, view } = setup(operation !== "create", onnavigate);
  let unmount = view.unmount;
  if (operation === "create") {
    view.unmount();
    unmount = render(PhotoAlbumsIndex, { albums, cache, onnavigate }).unmount;
    await fireEvent.click(screen.getByRole("button", { name: "New album" }));
    await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), { target: { value: "Summer" } });
    await fireEvent.click(screen.getByRole("button", { name: "Create album" }));
  } else {
    await fireEvent.click(screen.getByRole("button", { name: "Album actions" }));
    await fireEvent.click(await screen.findByRole("menuitem", { name: operation === "delete" ? "Delete album…" : "Duplicate…" }));
    await fireEvent.click(screen.getByRole("button", { name: operation === "delete" ? "Delete album" : "Duplicate album" }));
  }
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));
  unmount();
  render(PhotoAlbumsIndex, { albums, cache, onnavigate });
  finish(new Response(JSON.stringify(album)));
  await waitFor(() => expect(albums.busy).toBe(false));
  expect(onnavigate).not.toHaveBeenCalled();
});

it.each(["create", "delete", "duplicate"] as const)("leaves delayed %s feedback in the workspace after its dialog is dismissed", async operation => {
  let finish!: () => void;
  const onnavigate = vi.fn();
  const { albums, cache, view } = setup(operation !== "create", onnavigate);
  vi.spyOn(albums, operation).mockImplementation(() => new Promise<typeof album | undefined>(resolve => finish = () => { albums.error = "Album change unavailable"; resolve(undefined); }));
  if (operation === "create") {
    view.unmount();
    render(PhotoAlbumsIndex, { albums, cache, onnavigate });
    await fireEvent.click(screen.getByRole("button", { name: "New album" }));
    await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), { target: { value: "Summer" } });
    await fireEvent.click(screen.getByRole("button", { name: "Create album" }));
  } else {
    await fireEvent.click(screen.getByRole("button", { name: "Album actions" }));
    await fireEvent.click(await screen.findByRole("menuitem", { name: operation === "delete" ? "Delete album…" : "Duplicate…" }));
    await fireEvent.click(screen.getByRole("button", { name: operation === "delete" ? "Delete album" : "Duplicate album" }));
  }
  await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  finish();
  await screen.findByText("Album change unavailable");
  expect(screen.getAllByRole("alert")).toHaveLength(1);
  expect(albums.error).toBe("Album change unavailable");
  expect(onnavigate).not.toHaveBeenCalled();
});

it("creates from the dock and adds the complete live query with only two loaded photos", async () => {
  const { photos, albums } = setup();
  const created = { ...album, id: "created", name: "Summer" };
  vi.spyOn(albums, "create").mockResolvedValue(created);
  const members = vi.spyOn(albums, "members").mockResolvedValue(created);
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: "Select loaded photos" }));
  await fireEvent.click(screen.getByRole("button", { name: "Select all 10,000 photos" }));
  await fireEvent.click(screen.getByRole("button", { name: /Add to album/ }));
  await fireEvent.input(screen.getByRole("combobox", { name: "Find or create an album" }), { target: { value: "Summer" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: 'Create album "Summer"' }));
  await waitFor(() => expect(members).toHaveBeenCalledWith(created, photos.scope(), photos, false, expect.any(Function)));
  expect(albums.targetID).toBe("created");
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
  await waitFor(() => expect(members).toHaveBeenCalledWith(album, { asset_ids: ["photo-1", "photo-2"] }, photos, false, expect.any(Function)));
  await fireEvent.keyDown(window, { key: "b", repeat: true });
  await fireEvent.keyDown(window, { key: "b", ctrlKey: true });
  expect(members).toHaveBeenCalledTimes(1);
  expect(albums.error).toBe("Another action failed");
});

it("keeps B failures and selections while its source refresh is still running", async () => {
  const fetcher = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Add unavailable" }), { status: 503 })).mockResolvedValueOnce(new Response(JSON.stringify([album])));
  vi.stubGlobal("fetch", fetcher);
  const { photos, albums } = setup(true);
  let finish!: () => void;
  vi.spyOn(photos, "refresh").mockImplementation(() => new Promise<void>(resolve => finish = resolve));
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  albums.targetID = album.id;
  await fireEvent.keyDown(window, { key: "b" });
  await waitFor(() => expect(photos.refresh).toHaveBeenCalledTimes(1));
  expect(albums.busy).toBe(true);
  await fireEvent.keyDown(window, { key: "b" });
  expect(fetcher).toHaveBeenCalledTimes(2);
  expect(albums.error).toBe("Add unavailable");
  finish();
  await waitFor(() => expect(albums.busy).toBe(false));
  await screen.findByText("Add unavailable");
  expect(screen.getAllByRole("alert")).toHaveLength(1);
  expect(photos.selection.selectedIDs.has("photo-1")).toBe(true);
});

it("renders added order unchanged and saves or cancels inline rename", async () => {
  const { albums, view } = setup(true);
  await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  expect([...view.container.querySelectorAll("[data-asset]")].map(item => item.getAttribute("data-asset"))).toEqual(["photo-1", "photo-2"]);
  const update = vi.spyOn(albums, "update").mockResolvedValue(album);
  await fireEvent.click(screen.getByRole("button", { name: "Album actions" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: "Rename" }));
  await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), { target: { value: "Canceled" } });
  await fireEvent.keyDown(screen.getByRole("textbox", { name: "Album name" }), { key: "Escape" });
  expect(update).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole("button", { name: "Album actions" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: "Rename" }));
  const input = screen.getByRole("textbox", { name: "Album name" });
  await fireEvent.input(input, { target: { value: "Holiday" } });
  await fireEvent.submit(input.closest("form")!);
  expect(update).toHaveBeenCalledWith(album, { name: "Holiday" });
});

it.each(["delete", "duplicate"] as const)("retries %s with the refreshed revision without reopening the dialog", async operation => {
  const { albums } = setup(true);
  vi.spyOn(albums, operation).mockImplementationOnce(async () => { albums.error = "Trip changed elsewhere. Try again."; return undefined; }).mockResolvedValue(album);
  await fireEvent.click(screen.getByRole("button", { name: "Album actions" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: operation === "delete" ? "Delete album…" : "Duplicate…" }));
  const submit = screen.getByRole("button", { name: operation === "delete" ? "Delete album" : "Duplicate album" });
  await fireEvent.click(submit);
  await screen.findByText("Trip changed elsewhere. Try again.");
  expect(screen.getAllByRole("alert")).toHaveLength(1);
  expect(albums.error).toBe("");
  await fireEvent.click(submit);
  await waitFor(() => expect(screen.queryByRole("button", { name: operation === "delete" ? "Delete album" : "Duplicate album" })).toBeNull());
});

it("refreshes previews and summary counts inside an album", async () => {
  const { photos, albums } = setup(true);
  const refresh = vi.spyOn(photos, "refresh").mockResolvedValue();
  const load = vi.spyOn(albums, "load").mockResolvedValue(true);
  await fireEvent.click(screen.getByRole("button", { name: "Refresh previews" }));
  expect(refresh).toHaveBeenCalledTimes(1);
  expect(load).toHaveBeenCalledTimes(1);
});

it("creates a new album for every explicit custom choice, including Unicode names after deletion", async () => {
  const { photos, albums } = setup();
  const name = "😀".repeat(129);
  let nextID = 0;
  const create = vi.spyOn(albums, "create").mockImplementation(async value => {
    const item = { ...album, id: `created-${++nextID}`, name: value };
    albums.items = [...albums.items, item]; return item;
  });
  const members = vi.spyOn(albums, "members").mockResolvedValue(album);
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: "Select loaded photos" }));
  for (let i = 0; i < 3; i++) {
    if (i > 0) await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
    if (i === 2) albums.items = [album];
    await fireEvent.click(await screen.findByRole("button", { name: /Add to album/ }));
    const input = screen.getByRole("combobox", { name: "Find or create an album" });
    expect(input.hasAttribute("maxlength")).toBe(false);
    await fireEvent.input(input, { target: { value: name } });
    await fireEvent.mouseDown(await screen.findByRole("option", { name: `Create album "${name}"` }));
    await waitFor(() => expect(members).toHaveBeenCalledTimes(i + 1));
  }
  expect(create).toHaveBeenCalledTimes(3);
  expect(members.mock.calls.map(([item]) => item.id)).toEqual(["created-1", "created-2", "created-3"]);
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

it("adds to an acknowledged creation after list failure and retries its membership without another create", async () => {
  const created = { ...album, id: "created", name: "Summer" };
  const fetcher = vi.fn()
    .mockResolvedValueOnce(new Response(JSON.stringify(created)))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Album list unavailable" }), { status: 503 }))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Add unavailable" }), { status: 503 }))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Album list unavailable" }), { status: 503 }))
    .mockResolvedValueOnce(new Response(JSON.stringify({ ...created, revision: 2 })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Album list unavailable" }), { status: 503 }));
  vi.stubGlobal("fetch", fetcher);
  const { albums, photos } = setup();
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: /Add to album/ }));
  await fireEvent.input(screen.getByRole("combobox", { name: "Find or create an album" }), { target: { value: "Summer" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: 'Create album "Summer"' }));
  await screen.findByText("Add unavailable");
  await waitFor(() => expect(albums.error).toBe(""));
  expect(screen.getAllByRole("alert")).toHaveLength(1);
  expect(albums.error).toBe("");
  expect(albums.loadError).toBe("Album list unavailable");
  expect(photos.selection.selectedIDs.has("photo-1")).toBe(true);
  await fireEvent.input(screen.getByRole("combobox", { name: "Find or create an album" }), { target: { value: "" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Summer" }));
  await waitFor(() => expect(albums.notice).toBe("Added to Summer"));
  expect(fetcher.mock.calls.filter(([url, init]) => url.endsWith("/photos/albums") && init.method === "POST")).toHaveLength(1);
  expect(fetcher.mock.calls.filter(([url]) => url.endsWith("/created/members/add"))).toHaveLength(2);
  expect(albums.error).toBe("");
});

it("renders unavailable card observations and omits an unknown count from deletion", async () => {
  const { albums } = setup(true); albums.items = [{ ...album, included_count: undefined, member_count: undefined, cover_known: false }];
  await fireEvent.click(screen.getByRole("button", { name: "Album actions" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: "Delete album…" }));
  expect(screen.getByText('Delete "Trip"? Its photos stay in your library.')).toBeTruthy();
  cleanup();
  render(PhotoAlbumsIndex, { albums, cache: new PhotoPreviewCache("scoped", vi.fn()), onnavigate: vi.fn() });
  expect(screen.getByText("Count unavailable")).toBeTruthy(); expect(screen.getByText("Cover unavailable")).toBeTruthy();
  albums.items = [{ ...album, included_count: 0, cover_known: true }];
  await screen.findByText("0 photos"); await screen.findByText("No cover yet");
});

it("resets the empty index create dialog after failed and cancelled attempts", async () => {
  const albums = new PhotoAlbums("scoped", vi.fn());
  vi.spyOn(albums, "create").mockImplementation(async () => { albums.error = "Create unavailable"; return undefined; });
  render(PhotoAlbumsIndex, { albums, cache: new PhotoPreviewCache("scoped", vi.fn()), onnavigate: vi.fn() });
  await fireEvent.click(screen.getAllByRole("button", { name: "New album" })[0]);
  await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), { target: { value: "Trip" } });
  await fireEvent.click(screen.getByRole("button", { name: "Create album" }));
  await screen.findByText("Create unavailable");
  expect(screen.getAllByRole("alert")).toHaveLength(1);
  expect(albums.error).toBe("");
  await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  for (let attempt = 0; attempt < 2; attempt++) {
    await fireEvent.click(screen.getAllByRole("button", { name: "New album" })[1]);
    expect((screen.getByRole("textbox", { name: "Album name" }) as HTMLInputElement).value).toBe("");
    expect(screen.queryByText("Create unavailable")).toBeNull();
    await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), { target: { value: "Cancelled" } });
    await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  }
});
