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
  await fireEvent.click(await screen.findByRole("button", { name: "Select all 10,000 photos" }));
  expect(photos.scope()).toEqual({ query: photos.query });
  expect((button as HTMLButtonElement).disabled).toBe(false);
  await fireEvent.click(button);
  expect(photos.allResults).toBe(false);
  expect(photos.scope()).toEqual({ asset_ids: photos.items.map(item => item.asset_id) });
  expect((button as HTMLButtonElement).disabled).toBe(true);
});

async function pendingAlbumWrite(operation: "picker add" | "picker create" | "index create" | "delete" | "duplicate") {
  let finish!: (response: Response) => void;
  const fetcher = vi.fn().mockImplementationOnce(() => new Promise<Response>(resolve => finish = resolve)).mockResolvedValue(new Response(JSON.stringify([album])));
  vi.stubGlobal("fetch", fetcher);
  const onnavigate = vi.fn();
  const context = setup(operation === "delete" || operation === "duplicate", onnavigate);
  const { albums, cache, view } = context;
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  let unmount = view.unmount;
  if (operation.startsWith("picker")) {
    await fireEvent.click(screen.getByRole("button", { name: /Add to album/ }));
    if (operation === "picker create") await fireEvent.input(screen.getByRole("combobox", { name: "Find or create an album" }), { target: { value: "Summer" } });
    await fireEvent.mouseDown(await screen.findByRole("option", { name: operation === "picker create" ? 'Create album "Summer"' : "Trip" }));
  } else if (operation === "index create") {
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
  await waitFor(() => expect(albums.busy).toBe(true));
  return { ...context, onnavigate, fetcher, finish, unmount };
}

it.each(["picker add", "picker create", "index create", "delete", "duplicate"] as const)("keeps a delayed %s failure visible after navigation", async operation => {
  const { photos, albums, cache, finish, unmount } = await pendingAlbumWrite(operation);
  unmount();
  if (operation === "index create") render(PhotosWorkspace, { photos, albums, cache });
  else render(PhotoAlbumsIndex, { albums, cache, onnavigate: vi.fn() });
  finish(new Response(JSON.stringify({ detail: "Album change unavailable" }), { status: 503 }));
  const uncertain = operation === "picker create" || operation === "index create" || operation === "duplicate";
  await screen.findByText(uncertain ? `An album named "${operation === "duplicate" ? "Trip copy" : "Summer"}" may already exist. Another attempt can create an extra album.` : "Album change unavailable");
  await waitFor(() => expect(albums.busy).toBe(false));
  expect(screen.getAllByRole("alert")).toHaveLength(1);
  expect(albums.error).toBe(uncertain ? "" : "Album change unavailable");
  expect(photos.selection.selectedIDs.has("photo-1")).toBe(true);
});

it.each(["Escape", "focusout"])("keeps a delayed picker failure visible after closing with %s", async dismissal => {
  const { albums, finish } = await pendingAlbumWrite("picker add");
  const input = screen.getByRole("combobox", { name: "Find or create an album" });
  if (dismissal === "Escape") await fireEvent.keyDown(input, { key: "Escape" });
  else await fireEvent.focusOut(input, { relatedTarget: screen.getByRole("button", { name: "Refresh previews" }) });
  await waitFor(() => expect(screen.queryByRole("combobox", { name: "Find or create an album" })).toBeNull());
  finish(new Response(JSON.stringify({ detail: "Add unavailable" }), { status: 503 }));
  await waitFor(() => expect(albums.busy).toBe(false));
  await waitFor(() => expect(albums.error).toBe(""));
  await screen.findByText("Add unavailable");
  expect(screen.getAllByRole("alert")).toHaveLength(1);
});

it.each(["index create", "delete", "duplicate"] as const)("stops delayed %s success from navigating a destroyed view", async operation => {
  const { albums, cache, onnavigate, finish, unmount } = await pendingAlbumWrite(operation);
  unmount();
  render(PhotoAlbumsIndex, { albums, cache, onnavigate });
  finish(new Response(JSON.stringify(album)));
  await waitFor(() => expect(albums.busy).toBe(false));
  expect(onnavigate).not.toHaveBeenCalled();
});

it.each(["index create", "delete", "duplicate"] as const)("keeps the %s dialog open while its write is pending and shows one failure", async operation => {
  const { albums, onnavigate, finish } = await pendingAlbumWrite(operation);
  expect((screen.getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled).toBe(true);
  await fireEvent.keyDown(window, { key: "Escape" });
  expect(screen.getByRole("dialog")).toBeTruthy();
  finish(new Response(JSON.stringify({ detail: "Album change unavailable" }), { status: 503 }));
  await screen.findByText(operation === "delete" ? "Album change unavailable" : `An album named "${operation === "duplicate" ? "Trip copy" : "Summer"}" may already exist. Another attempt can create an extra album.`);
  await waitFor(() => expect(albums.busy).toBe(false));
  expect(screen.getAllByRole("alert")).toHaveLength(1);
  expect(albums.error).toBe("");
  expect(onnavigate).not.toHaveBeenCalled();
});

it.each(["index create", "picker create", "duplicate"] as const)("acknowledges unconfirmed %s without replay before a separate confirmation", async operation => {
  const { photos, albums, cache, view } = setup(operation === "duplicate");
  const committed = { ...album, id: "committed", name: operation === "duplicate" ? "Trip copy" : "Summer" };
  const fetcher = vi.fn().mockRejectedValueOnce(new TypeError("Failed to fetch"))
    .mockResolvedValueOnce(new Response(JSON.stringify([album, committed])));
  vi.stubGlobal("fetch", fetcher);
  let current = view;
  if (operation === "index create") { view.unmount(); current = render(PhotoAlbumsIndex, { albums, cache, onnavigate: vi.fn() }); }
  const open = async () => {
    if (operation === "index create") await fireEvent.click(screen.getByRole("button", { name: "New album" }));
    else if (operation === "duplicate") { await fireEvent.click(screen.getByRole("button", { name: "Album actions" })); await fireEvent.click(await screen.findByRole("menuitem", { name: "Duplicate…" })); }
    else await fireEvent.click(await screen.findByRole("button", { name: /Add to album/ }));
  };
  if (operation === "picker create") photos.selectLoaded();
  await open();
  const inputName = operation === "index create" ? "Album name" : operation === "duplicate" ? "Copy name" : "Find or create an album";
  const input = screen.getByRole(operation === "picker create" ? "combobox" : "textbox", { name: inputName });
  await fireEvent.input(input, { target: { value: committed.name } });
  if (operation === "picker create") await fireEvent.mouseDown(await screen.findByRole("option", { name: `Create album "${committed.name}"` }));
  else await fireEvent.click(screen.getByRole("button", { name: operation === "index create" ? "Create album" : "Duplicate album" }));
  await screen.findByText(`An album named "${committed.name}" may already exist. Another attempt can create an extra album.`);
  await waitFor(() => expect(albums.busy).toBe(false));
  await fireEvent.input(input, { target: { value: "Edited draft" } });
  expect((input as HTMLInputElement).value).toBe("Edited draft");
  if (operation !== "picker create") await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  else await fireEvent.keyDown(input, { key: "Escape" });
  await open();
  await waitFor(() => expect((screen.getByRole(operation === "picker create" ? "combobox" : "textbox", { name: inputName }) as HTMLInputElement).value).toBe(committed.name));
  if (operation !== "picker create") await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  else await fireEvent.keyDown(screen.getByRole("combobox", { name: inputName }), { key: "Escape" });
  current.unmount(); render(PhotoAlbumsIndex, { albums, cache, onnavigate: vi.fn() });
  expect(screen.getByText(`An album named "${committed.name}" may already exist. Another attempt can create an extra album.`)).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Allow another album" }));
  expect(fetcher).toHaveBeenCalledTimes(2);
  cleanup();
  if (operation === "index create") render(PhotoAlbumsIndex, { albums, cache, onnavigate: vi.fn() });
  else render(PhotosWorkspace, { photos, albums, cache, albumID: operation === "duplicate" ? album.id : "", onnavigate: vi.fn() });
  await open();
  const retryInput = screen.getByRole(operation === "picker create" ? "combobox" : "textbox", { name: inputName });
  await fireEvent.input(retryInput, { target: { value: committed.name } });
  fetcher.mockResolvedValueOnce(new Response(JSON.stringify({ ...committed, id: "second" }))).mockResolvedValueOnce(new Response(JSON.stringify([album, committed])));
  if (operation === "picker create") {
    fetcher.mockResolvedValueOnce(new Response(JSON.stringify({ ...committed, id: "second", revision: 2 }))).mockResolvedValueOnce(new Response(JSON.stringify([album, committed])));
    await fireEvent.mouseDown(await screen.findByRole("option", { name: `Create album "${committed.name}"` }));
  } else await fireEvent.click(screen.getByRole("button", { name: operation === "index create" ? "Create album" : "Duplicate album" }));
  await waitFor(() => expect(fetcher.mock.calls.filter(([, init]) => init.method !== "GET")).toHaveLength(operation === "picker create" ? 3 : 2));
  if (operation === "duplicate") expect(fetcher.mock.calls[2][1].headers.get("If-Match")).toBe('"1"');
  expect(photos.selection.selectedIDs.size).toBe(operation === "picker create" ? 2 : 0);
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

it.each([false, true])("keeps B feedback after a repeated shortcut during source refresh, successful=%s", async successful => {
  const fetcher = vi.fn().mockResolvedValueOnce(successful ? new Response(JSON.stringify({ ...album, revision: 2 })) : new Response(JSON.stringify({ detail: "Add unavailable" }), { status: 503 })).mockResolvedValueOnce(new Response(JSON.stringify([album])));
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
  if (successful) expect(albums.error).toBe("Another album change is still running. Try again.");
  finish();
  await waitFor(() => expect(albums.busy).toBe(false));
  if (successful) {
    expect(albums.error).toBe("");
    expect(albums.notice).toContain("Added to Trip");
    expect(screen.queryByRole("alert")).toBeNull();
  } else {
    await screen.findByText("Add unavailable");
    expect(screen.getAllByRole("alert")).toHaveLength(1);
  }
});

it("renders added order unchanged and saves or cancels inline rename", async () => {
  const { albums, view } = setup(true);
  await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  expect([...view.container.querySelectorAll("[data-asset]")].map(item => item.getAttribute("data-asset"))).toEqual(["photo-1", "photo-2"]);
  const update = vi.spyOn(albums, "update").mockResolvedValue(album);
  await fireEvent.click(screen.getByRole("button", { name: "Album actions" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: "Rename" }));
  expect((screen.getByRole("button", { name: "Star album" }) as HTMLButtonElement).disabled).toBe(true);
  expect((screen.getByRole("button", { name: "Album actions" }) as HTMLButtonElement).disabled).toBe(true);
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

it.each(["rename", "duplicate"] as const)("corrects a rejected %s name with its original revision, then requires review for a conflict", async operation => {
  let finish!: (response: Response) => void;
  let reconcile!: (response: Response) => void;
  const current = { ...album, revision: 2 };
  const fetcher = vi.fn().mockImplementationOnce(() => new Promise<Response>(resolve => finish = resolve))
    .mockImplementationOnce(() => new Promise<Response>(resolve => reconcile = resolve))
    .mockResolvedValueOnce(new Response(JSON.stringify({ code: "stale_revision" }), { status: 412 }))
    .mockResolvedValueOnce(new Response(JSON.stringify([current])));
  vi.stubGlobal("fetch", fetcher);
  const { albums, photos } = setup(true);
  photos.selectLoaded();
  await fireEvent.click(screen.getByRole("button", { name: "Album actions" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: operation === "rename" ? "Rename" : "Duplicate…" }));
  const input = screen.getByRole("textbox", { name: operation === "rename" ? "Album name" : "Copy name" });
  const submit = screen.getByRole("button", { name: operation === "rename" ? "Save" : "Duplicate album" });
  await fireEvent.input(input, { target: { value: "x".repeat(257) } });
  await fireEvent.click(submit);
  await waitFor(() => expect(albums.busy).toBe(true));
  await fireEvent.input(input, { target: { value: "Corrected name" } });
  finish(new Response(JSON.stringify({ detail: "Name too long" }), { status: 422 }));
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
  if (operation === "rename") await fireEvent.keyDown(window, { key: "b" });
  reconcile(new Response(JSON.stringify({ detail: "List unavailable" }), { status: 503 }));
  await screen.findByText("Name too long");
  await waitFor(() => expect(albums.error).toBe(""));
  await waitFor(() => expect((submit as HTMLButtonElement).disabled).toBe(false));
  expect(albums.loadError).toBe("List unavailable");
  expect(screen.queryByRole("button", { name: "Review current album" })).toBeNull();
  expect((input as HTMLInputElement).value).toBe("Corrected name");
  await fireEvent.click(submit);
  await screen.findByRole("button", { name: "Review current album" });
  expect(fetcher.mock.calls.slice(0, 3).filter(([, init]) => init.method !== "GET").map(([, init]) => init.headers.get("If-Match"))).toEqual(['"1"', '"1"']);
  expect(JSON.parse(fetcher.mock.calls[2][1].body).name).toBe("Corrected name");
  expect((submit as HTMLButtonElement).disabled).toBe(true);
});

it.each(["rename", "delete", "duplicate"] as const)("keeps the inspected revision for %s until current-state review and a separate retry", async operation => {
  let finish!: (response: Response) => void;
  const current = { ...album, revision: 2, name: "Updated Trip", included_count: 3 };
  const result = { ...current, revision: 3, ...(operation === "delete" ? { deleted_at: "2025-01-01" } : { name: "My draft" }) };
  const fetcher = vi.fn().mockImplementationOnce(() => new Promise<Response>(resolve => finish = resolve))
    .mockResolvedValueOnce(new Response(JSON.stringify({ code: "stale_revision" }), { status: 412 }))
    .mockResolvedValueOnce(new Response(JSON.stringify([current])))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Review rejected" }), { status: 422 }))
    .mockResolvedValueOnce(new Response(JSON.stringify([current])))
    .mockResolvedValueOnce(new Response(JSON.stringify(result)))
    .mockResolvedValueOnce(new Response(JSON.stringify(operation === "delete" ? [] : [result])));
  vi.stubGlobal("fetch", fetcher);
  const { albums } = setup(true);
  await fireEvent.click(screen.getByRole("button", { name: "Album actions" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: operation === "rename" ? "Rename" : operation === "delete" ? "Delete album…" : "Duplicate…" }));
  if (operation !== "delete") await fireEvent.input(screen.getByRole("textbox", { name: operation === "rename" ? "Album name" : "Copy name" }), { target: { value: "My draft" } });
  const reading = albums.load();
  finish(new Response(JSON.stringify([current]))); await reading;
  const submit = screen.getByRole("button", { name: operation === "rename" ? "Save" : operation === "delete" ? "Delete album" : "Duplicate album" });
  await fireEvent.click(submit);
  await screen.findByText("Updated Trip changed. Try again.");
  expect(screen.getAllByRole("alert")).toHaveLength(1);
  expect(fetcher.mock.calls[1][1].headers.get("If-Match")).toBe('"1"');
  expect(albums.items[0].revision).toBe(2);
  expect((submit as HTMLButtonElement).disabled).toBe(true);
  await fireEvent.click(await screen.findByRole("button", { name: "Review current album" }));
  await screen.findByText("Review rejected");
  if (operation !== "delete") await fireEvent.input(screen.getByRole("textbox", { name: operation === "rename" ? "Album name" : "Copy name" }), { target: { value: "My draft" } });
  expect((submit as HTMLButtonElement).disabled).toBe(true);
  expect(fetcher.mock.calls.filter(([, init]) => init.method !== "GET")).toHaveLength(1);
  await fireEvent.click(screen.getByRole("button", { name: "Review current album" }));
  await waitFor(() => expect((submit as HTMLButtonElement).disabled).toBe(false));
  expect(fetcher).toHaveBeenCalledTimes(5);
  expect(fetcher.mock.calls[4][1].method).toBe("GET");
  if (operation === "delete") expect(screen.getByText('Delete "Updated Trip"? Its 3 photos stay in your library.')).toBeTruthy();
  else {
    expect(screen.getByText("Updated Trip · 3 photos")).toBeTruthy();
    expect((screen.getByRole("textbox", { name: operation === "rename" ? "Album name" : "Copy name" }) as HTMLInputElement).value).toBe("My draft");
  }
  await fireEvent.click(submit);
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(7));
  expect(fetcher.mock.calls[5][1].headers.get("If-Match")).toBe('"2"');
  await waitFor(() => expect(screen.queryByRole("button", { name: operation === "rename" ? "Save" : operation === "delete" ? "Delete album" : "Duplicate album" })).toBeNull());
});

it.each(["failed", "missing", "superseded", "dismissed", "changed after review"] as const)("keeps album recovery safe when its read is %s", async mode => {
  let finish!: (response: Response) => void;
  const current = { ...album, revision: 2, included_count: 3 }, newer = { ...current, revision: 3, included_count: 4 };
  const fetcher = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ code: "stale_revision", detail: "Trip changed. Try again." }), { status: 412 }))
    .mockResolvedValueOnce(new Response(JSON.stringify(mode === "dismissed" ? { detail: "List unavailable" } : [current]), { status: mode === "dismissed" ? 503 : 200 }))
    .mockImplementationOnce(() => new Promise<Response>(resolve => finish = resolve));
  vi.stubGlobal("fetch", fetcher);
  const { albums } = setup(true);
  const open = async () => {
    await fireEvent.click(screen.getByRole("button", { name: "Album actions" }));
    await fireEvent.click(await screen.findByRole("menuitem", { name: "Rename" }));
  };
  await open();
  await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), { target: { value: "My draft" } });
  await fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await screen.findByText("Trip changed. Try again.");
  await fireEvent.click(await screen.findByRole("button", { name: "Review current album" }));
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(3));
  if (mode === "superseded") {
    fetcher.mockResolvedValueOnce(new Response(JSON.stringify([newer]))); await albums.load();
  }
  if (mode === "dismissed") { await fireEvent.click(screen.getByRole("button", { name: "Cancel" })); await open(); }
  finish(new Response(JSON.stringify(mode === "failed" ? { detail: "Review unavailable" } : mode === "missing" ? [] : [mode === "dismissed" ? newer : current]), { status: mode === "failed" ? 503 : 200 }));
  if (mode === "failed" || mode === "missing" || mode === "superseded") {
    await screen.findByText(mode === "failed" ? "Review unavailable" : mode === "missing" ? "This album was deleted." : "Trip changed. Try again.");
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("textbox", { name: "Album name" }) as HTMLInputElement).value).toBe("My draft");
    expect(fetcher.mock.calls.filter(([, init]) => init.method !== "GET")).toHaveLength(1);
  } else if (mode === "dismissed") {
    await waitFor(() => expect(albums.items[0].revision).toBe(3));
    expect(screen.getByText("Trip · 2 photos")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
    fetcher.mockResolvedValueOnce(new Response(JSON.stringify(newer))).mockResolvedValueOnce(new Response(JSON.stringify([newer])));
    await fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(5));
    expect(fetcher.mock.calls[3][1].headers.get("If-Match")).toBe('"1"');
  } else {
    await waitFor(() => expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(false));
    fetcher.mockResolvedValueOnce(new Response(JSON.stringify([newer]))); await albums.load();
    fetcher.mockResolvedValueOnce(new Response(JSON.stringify({ code: "stale_revision" }), { status: 412 })).mockResolvedValueOnce(new Response(JSON.stringify([newer])));
    await fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByText("Trip changed. Try again.");
    expect(fetcher.mock.calls[4][1].headers.get("If-Match")).toBe('"2"');
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
  }
});

it("refreshes previews and summary counts inside an album", async () => {
  const { photos, albums } = setup(true);
  const refresh = vi.spyOn(photos, "refresh").mockResolvedValue();
  const load = vi.spyOn(albums, "load").mockResolvedValue(true);
  await fireEvent.click(screen.getByRole("button", { name: "Refresh previews" }));
  expect(refresh).toHaveBeenCalledTimes(1);
  expect(load).toHaveBeenCalledTimes(1);
});

it.each(["uuid", "prefixed", "Unicode after deletion"])("creates albums for explicit custom choices: %s", async kind => {
  const { photos, albums } = setup();
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
    if (i === 2) albums.items = [album];
    await fireEvent.click(await screen.findByRole("button", { name: /Add to album/ }));
    const input = screen.getByRole("combobox", { name: "Find or create an album" });
    expect(input.hasAttribute("maxlength")).toBe(false);
    await fireEvent.input(input, { target: { value: name } });
    await fireEvent.mouseDown(await screen.findByRole("option", { name: `Create album "${name}"` }));
    await waitFor(() => expect(members).toHaveBeenCalledTimes(i + 1));
    expect(members).toHaveBeenLastCalledWith(expect.objectContaining({ id: `created-${i + 1}` }), photos.scope(), photos, false, expect.any(Function));
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
  const created = { ...album, id: "created", name: "Summer" };
  const fetcher = vi.fn()
    .mockResolvedValueOnce(new Response(JSON.stringify(created)))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Album list unavailable" }), { status: 503 }))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Add unavailable" }), { status: 503 }))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Album list unavailable" }), { status: 503 }))
    .mockResolvedValueOnce(new Response(JSON.stringify({ ...created, revision: 2 })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Album list unavailable" }), { status: 503 }));
  vi.stubGlobal("fetch", fetcher);
  const { albums } = setup();
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: /Add to album/ }));
  await fireEvent.input(screen.getByRole("combobox", { name: "Find or create an album" }), { target: { value: "Summer" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: 'Create album "Summer"' }));
  await screen.findByText("Add unavailable");
  await fireEvent.input(screen.getByRole("combobox", { name: "Find or create an album" }), { target: { value: "" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Summer" }));
  await waitFor(() => expect(albums.notice).toBe("Added to Summer"));
  expect(fetcher.mock.calls.filter(([url, init]) => url.endsWith("/photos/albums") && init.method === "POST")).toHaveLength(1);
  expect(fetcher.mock.calls.filter(([url]) => url.endsWith("/created/members/add"))).toHaveLength(2);
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
