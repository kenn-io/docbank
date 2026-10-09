import { afterEach, expect, it, vi } from "vitest";
import { PhotoAlbums, photoDragType } from "./photoAlbums.svelte.js";
import { Photos, photoQuery } from "./photos.svelte.js";
import { photoAlbum, photo, albumResponse } from "./photo-test-fixtures.js";

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });
const album = photoAlbum();
const response = albumResponse;
const selected = (count: number) => {
  const photos = new Photos("scoped", vi.fn());
  photos.selection = { selectedIDs: new Set(Array.from({ length: count }, (_, index) => `photo-${index}`)), anchorID: undefined };
  return photos;
};

it("adds 2,345 IDs in bounded batches with each returned revision", async () => {
  const fetcher = vi.fn().mockResolvedValueOnce(response({ ...album, revision: 2 }))
    .mockResolvedValueOnce(response({ ...album, revision: 3 }))
    .mockResolvedValueOnce(response({ ...album, revision: 4 }))
    .mockResolvedValueOnce(response([{ ...album, revision: 4, included_count: 2345 }]));
  vi.stubGlobal("fetch", fetcher);
  const albums = new PhotoAlbums("scoped", vi.fn());
  const photos = selected(2345);
  expect(await albums.members(album, photos.scope(), photos)).toBeTruthy();
  expect(fetcher.mock.calls.slice(0, 3).map(([, init]) => JSON.parse(init.body).asset_ids.length)).toEqual([1000, 1000, 345]);
  expect(fetcher.mock.calls.slice(0, 3).map(([, init]) => init.headers.get("If-Match"))).toEqual(['"1"', '"2"', '"3"']);
  expect(albums.notice).toBe("Added to Trip · now 2,345 photos");
});

it.each([412, 503, "malformed success"])("preserves unfinished IDs and reports progress when a later batch fails with %s", async status => {
  const revision = status === 412 ? 3 : 2;
  const fetcher = vi.fn().mockResolvedValueOnce(response({ ...album, revision: 2 }))
    .mockResolvedValueOnce(status === "malformed success" ? response({ ...album, revision: 4 }) : response(status === 412 ? { code: "stale_revision" } : { detail: "Members unavailable" }, status as number))
    .mockResolvedValueOnce(response([{ ...album, revision, included_count: 1000, effective_cover_asset_id: "new-cover" }]))
    .mockResolvedValueOnce(response({ ...album, revision: revision + 1 }))
    .mockResolvedValueOnce(response([{ ...album, revision: revision + 1, included_count: 1345 }]));
  vi.stubGlobal("fetch", fetcher);
  const albums = new PhotoAlbums("scoped", vi.fn()); const photos = selected(1345);
  expect(await albums.members(album, photos.scope(), photos)).toBeUndefined();
  expect(photos.selection.selectedIDs.size).toBe(345);
  expect(albums.items[0]).toMatchObject({ revision, included_count: 1000, effective_cover_asset_id: "new-cover" });
  expect(albums.error).toBe(`Added 1,000 of 1,345 photos. ${status === 412 ? "Trip changed. Try again." : status === "malformed success" ? "Invalid album response." : "Members unavailable"}`);
  expect(fetcher.mock.calls.filter(([url]) => url.endsWith("/members/add"))).toHaveLength(2);
  await albums.members(albums.items[0], photos.scope(), photos);
  expect(fetcher.mock.calls[3][1].headers.get("If-Match")).toBe(`"${revision}"`);
  expect(JSON.parse(fetcher.mock.calls[3][1].body).asset_ids).toHaveLength(345);
});

it("sends the complete live query once and retains all-results mode on a conflict", async () => {
  const fetcher = vi.fn().mockResolvedValueOnce(response({ code: "stale_revision" }, 412)).mockResolvedValueOnce(response([{ ...album, revision: 2 }]));
  vi.stubGlobal("fetch", fetcher);
  const albums = new PhotoAlbums("scoped", vi.fn());
  const photos = selected(250); photos.total = 10000; photos.allResults = true;
  await albums.members(album, photos.scope(), photos);
  expect(JSON.parse(fetcher.mock.calls[0][1].body)).toEqual({ query: photoQuery });
  expect(fetcher).toHaveBeenCalledTimes(2);
  expect(photos.allResults).toBe(true);
  expect(photos.selection.selectedIDs.size).toBe(250);
});

it("keeps server order and clears a session target after deletion", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response([{ ...album, name: "Zoo", starred: true }, { ...album, id: "22222222-2222-4222-8222-000000000069", name: "apple" }])));
  const albums = new PhotoAlbums("scoped", vi.fn()); albums.targetID = "deleted";
  await albums.load();
  expect(albums.items.map(item => item.name)).toEqual(["Zoo", "apple"]);
  expect(albums.targetID).toBe("");
});

it.each([{ partial: true, failedRead: false }, { partial: true, failedRead: true }, { partial: false, failedRead: false }, { partial: false, failedRead: true }])("refreshes removal after response loss: $partial partial, $failedRead failed reconciliation", async ({ partial, failedRead }) => {
  const remaining = partial ? Array.from({ length: 345 }, (_, index) => photo(index + 1000)) : [];
  const count = remaining.length, error = partial ? "Members unavailable" : "Failed to fetch";
  const fetcher = vi.fn();
  if (partial) fetcher.mockResolvedValueOnce(response({ ...album, revision: 2 }));
  fetcher.mockRejectedValueOnce(new TypeError(error))
    .mockResolvedValueOnce(failedRead ? response({ detail: "List unavailable" }, 503) : response([{ ...album, revision: 2, included_count: count, effective_cover_asset_id: partial ? "new-cover" : undefined }]))
    .mockResolvedValueOnce(response({ items: remaining.slice(0, 250), total: count, ...(partial ? { next_cursor: "tail" } : {}) }));
  if (partial) fetcher.mockResolvedValueOnce(response({ items: remaining.slice(250), total: count }));
  vi.stubGlobal("fetch", fetcher);
  const albums = new PhotoAlbums("scoped", vi.fn()); const photos = selected(partial ? 1345 : 1);
  albums.items = [{ ...album, included_count: photos.selection.selectedIDs.size, member_count: photos.selection.selectedIDs.size }];
  photos.query = { ...photoQuery, filters: { set_ids: [album.id] }, sort: { field: "added_time", direction: "desc" } };
  photos.items = Array.from({ length: photos.selection.selectedIDs.size }, (_, index) => photo(index)); photos.total = photos.items.length;
  await albums.members(album, photos.scope(), photos, true);
  expect([...photos.selection.selectedIDs]).toEqual(remaining.map(item => item.asset_id));
  expect(albums.error).toBe(partial ? `Removed 1,000 of 1,345 photos. ${error}` : error);
  expect(albums.loadError).toBe(failedRead ? "List unavailable" : "");
  expect(albums.items[0].revision).toBe(partial || !failedRead ? 2 : 1);
  expect(fetcher.mock.calls.filter(([url]) => url.endsWith("/photos/albums"))).toHaveLength(1);
  expect(fetcher.mock.calls[partial ? 3 : 2][0]).toContain("/photos/assets/query");
  expect(fetcher.mock.calls.filter(([url]) => url.endsWith("/members/remove"))).toHaveLength(partial ? 2 : 1);
  if (!failedRead) { expect(albums.items[0].included_count).toBe(count); expect(albums.items[0].effective_cover_asset_id).toBe(partial ? "new-cover" : undefined); }
  else {
    expect(albums.items[0]).toMatchObject({ included_count: undefined, member_count: undefined, cover_known: false });
    if (partial) fetcher.mockResolvedValueOnce(response({ ...album, revision: 3 }));
    fetcher.mockResolvedValueOnce(response([{ ...album, revision: partial ? 3 : 2, included_count: 0 }]));
    if (partial) {
      fetcher.mockResolvedValueOnce(response({ items: [], total: 0 }));
      await albums.members(albums.items[0], photos.scope(), photos, true);
      expect(fetcher.mock.calls[5][1].headers.get("If-Match")).toBe('"2"');
    } else { await albums.load(); expect(albums.error).toBe(error); }
    expect(albums.items[0]).toMatchObject({ included_count: 0, cover_known: true }); expect(photos.selection.selectedIDs.size).toBe(0);
  }
});

it.each(["available", "read fails", "deleted"])("reconciles a missing-photo response when the album is %s", async mode => {
  const fetcher = vi.fn().mockResolvedValueOnce(response({ detail: "A selected photo is no longer available", code: "not_found" }, 404))
    .mockResolvedValueOnce(mode === "read fails" ? response({ detail: "List unavailable" }, 503) : response(mode === "deleted" ? [] : [album]))
    .mockResolvedValueOnce(mode === "read fails" ? response([album]) : response({ items: [photo(1)], total: 1 }));
  vi.stubGlobal("fetch", fetcher);
  const albums = new PhotoAlbums("scoped", vi.fn()); const photos = selected(2); photos.items = [photo(0), photo(1)];
  const restore = vi.fn().mockResolvedValue(undefined), preserve = vi.fn(() => restore);
  const refresh = vi.spyOn(photos, "refresh");
  await albums.members(album, photos.scope(), photos, false, preserve);
  if (mode === "read fails") {
    expect(albums.loadError).toBe("List unavailable");
    await albums.load(); expect(albums.loadError).toBe("");
  } else if (mode === "deleted") {
    expect(refresh).not.toHaveBeenCalled();
  } else {
    expect(preserve).toHaveBeenCalled(); expect(restore).toHaveBeenCalled();
    expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]); expect(photos.total).toBe(1);
  }
  expect(albums.error).toBe(mode === "deleted" ? "This album was deleted." : "A selected photo is no longer available");
});

it("clears all-results mode after removing every member and links the operated album", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(response({ ...album, revision: 2 })).mockResolvedValueOnce(response([album])).mockResolvedValueOnce(response({ items: [], total: 0 })));
  const albums = new PhotoAlbums("scoped", vi.fn()); albums.targetID = "another";
  const photos = selected(2); photos.allResults = true; photos.items = [photo(0), photo(1)];
  photos.query = { ...photoQuery, filters: { set_ids: [album.id] }, sort: { field: "added_time", direction: "desc" } };
  await albums.members(album, photos.scope(), photos, true);
  expect(photos.allResults).toBe(false);
  expect(photos.selection.selectedIDs.size).toBe(0);
  expect(albums.noticeID).toBe(album.id);
});

it.each(["create network", "duplicate server", "create invalid response", "create empty object", "create missing ETag", "create mismatched ETag", "create invalid ID", "create invalid revision", "create unsafe revision", "create invalid name", "create invalid starred", "create invalid timestamp", "create invalid cover", "create deleted", "duplicate source ID", "duplicate advanced revision"])("guards an unconfirmed %s through reads without blocking existing albums", async mode => {
  const fetcher = vi.fn();
  if (mode === "create network") fetcher.mockRejectedValueOnce(new TypeError("Failed to fetch"));
  else if (mode === "duplicate server") fetcher.mockResolvedValueOnce(response({ detail: "Unavailable" }, 503));
  else if (mode === "create invalid response") fetcher.mockResolvedValueOnce(new Response("invalid JSON"));
  else {
    const body = { ...album, ...(mode === "duplicate advanced revision" ? { id: "22222222-2222-4222-8222-000000000068", revision: 2 } : {}) };
    const changes: Record<string, object> = {
      "create invalid ID": { id: "invalid" }, "create invalid revision": { revision: 0 }, "create unsafe revision": { revision: Number.MAX_SAFE_INTEGER + 1 },
      "create invalid name": { name: null }, "create invalid starred": { starred: "false" }, "create invalid timestamp": { updated_at: "invalid" },
      "create invalid cover": { cover_asset_id: 5 }, "create deleted": { deleted_at: "2025-01-01T00:00:00Z" },
    };
    const reply = response(mode === "create empty object" ? {} : { ...body, ...changes[mode] });
    if (mode === "create missing ETag") reply.headers.delete("ETag");
    if (mode === "create mismatched ETag") reply.headers.set("ETag", '"2"');
    fetcher.mockResolvedValueOnce(reply);
  }
  fetcher.mockResolvedValueOnce(response({ detail: "List unavailable" }, 503)).mockResolvedValueOnce(response([album]))
    .mockResolvedValueOnce(response({ ...album, starred: true, revision: 2 })).mockResolvedValueOnce(response([album]));
  vi.stubGlobal("fetch", fetcher);
  const albums = new PhotoAlbums("scoped", vi.fn());
  if (mode.startsWith("duplicate")) await albums.duplicate(album, " Draft "); else await albums.create(" Draft ");
  expect(albums.unconfirmed).toEqual({ kind: mode.startsWith("duplicate") ? "duplicate" : "create", name: "Draft", ...(mode.startsWith("duplicate") ? { sourceID: album.id } : {}) });
  expect(albums.error).toBe(""); expect(albums.loadError).toBe("List unavailable");
  await albums.create("Changed draft"); await albums.duplicate(album, "Changed draft");
  expect(fetcher).toHaveBeenCalledTimes(2);
  await albums.load(); await albums.members(album, { asset_ids: ["photo-1"] }, selected(1));
  expect(albums.unconfirmed?.name).toBe("Draft"); expect(fetcher).toHaveBeenCalledTimes(5);
});

it("keeps the original write rejection status through failed reads and clears it for later writes", async () => {
  const fetcher = vi.fn().mockResolvedValueOnce(response({ detail: "Name too long" }, 422))
    .mockResolvedValueOnce(response({ detail: "List unavailable" }, 503))
    .mockRejectedValueOnce(new TypeError("Failed to fetch"))
    .mockResolvedValueOnce(response([album]));
  vi.stubGlobal("fetch", fetcher);
  const albums = new PhotoAlbums("scoped", vi.fn());
  await albums.create("x".repeat(257));
  expect(albums.unconfirmed).toBeUndefined();
  expect(albums.errorStatus).toBe(422); expect(albums.loadError).toBe("List unavailable");
  await albums.update(album, { name: "Corrected name" });
  expect(albums.errorStatus).toBeUndefined(); expect(albums.error).toBe("Failed to fetch");
  albums.error = ""; albums.errorStatus = 422; albums.busy = true;
  await albums.create("Blocked");
  expect(albums.unconfirmed).toBeUndefined();
  expect(albums.errorStatus).toBeUndefined(); expect(fetcher).toHaveBeenCalledTimes(4);
  expect(albums.error).toBe("Another album change is still running. Try again.");
  albums.errorStatus = 422; albums.busy = false; albums.dispose();
  await albums.duplicate(album, "Blocked");
  expect(albums.unconfirmed).toBeUndefined();
  expect(albums.errorStatus).toBeUndefined(); expect(fetcher).toHaveBeenCalledTimes(4);
});

it.each(["wrong identity", "advanced revision", "missing deletion", "unchanged deletion", "invalid deletion"])("rejects an existing album response with %s before reporting success", async mode => {
  const body = { ...album, ...(mode === "wrong identity" ? { id: "22222222-2222-4222-8222-000000000068" } : mode === "advanced revision" ? { revision: 3 } : mode === "unchanged deletion" ? { deleted_at: "2025-01-01T00:00:00Z" } : mode === "invalid deletion" ? { revision: 2, deleted_at: 5 } : { revision: 2 }) };
  vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(response(body)).mockResolvedValueOnce(response([album])));
  const albums = new PhotoAlbums("scoped", vi.fn());
  const result = mode.endsWith("deletion") ? await albums.delete(album) : await albums.update(album, { starred: true });
  expect(result).toBeUndefined();
  expect(albums.error).toBe("Invalid album response.");
  expect(albums.notice).toBe("");
  expect(albums.items).toEqual([{ ...album, cover_known: true }]);
});

it("shares selection scope for an in-app drag and rejects an external payload", async () => {
  const albums = new PhotoAlbums("scoped", vi.fn());
  const photos = selected(2); photos.allResults = true;
  const transfer = { setData: vi.fn(), setDragImage: vi.fn(), types: [photoDragType], effectAllowed: "" };
  albums.startDrag("photo-0", { dataTransfer: transfer } as unknown as DragEvent, photos);
  expect(albums.drag?.scope).toEqual({ query: photoQuery });
  const members = vi.spyOn(albums, "members").mockResolvedValue(album);
  await albums.drop(album, { dataTransfer: transfer, preventDefault: vi.fn() } as unknown as DragEvent);
  expect(members).toHaveBeenCalledWith(album, { query: photoQuery }, photos, false, undefined);
  await albums.drop(album, { dataTransfer: transfer, preventDefault: vi.fn() } as unknown as DragEvent);
  expect(members).toHaveBeenCalledTimes(1);
  albums.startDrag("unselected", { dataTransfer: transfer } as unknown as DragEvent, photos);
  expect(albums.drag?.scope).toEqual({ asset_ids: ["unselected"] });
  members.mockRestore();
  const fetcher = vi.fn().mockResolvedValueOnce(response({ ...album, revision: 2 })).mockResolvedValueOnce(response([album]));
  vi.stubGlobal("fetch", fetcher);
  photos.query = { ...photoQuery, filters: { set_ids: [album.id] } };
  let finish!: () => void;
  vi.spyOn(photos, "refresh").mockImplementation(() => new Promise<void>(resolve => finish = resolve));
  const first = albums.drop(album, { dataTransfer: transfer, preventDefault: vi.fn() } as unknown as DragEvent);
  await vi.waitFor(() => expect(photos.refresh).toHaveBeenCalledOnce());
  albums.startDrag("photo-0", { dataTransfer: transfer } as unknown as DragEvent, photos);
  await albums.drop(album, { dataTransfer: transfer, preventDefault: vi.fn() } as unknown as DragEvent);
  expect(albums.error).toBe("Another album change is still running. Try again.");
  expect(fetcher).toHaveBeenCalledTimes(2);
  finish(); await first;
  expect(albums.error).toBe("");
  expect(albums.notice).toContain("Added to Trip");
});

it("orders acknowledged properties by star, Unicode code point name and ID while retaining observations", async () => {
  const items = [photoAlbum({ id: "22222222-2222-4222-8222-000000000001", name: "apple" }), photoAlbum({ id: "22222222-2222-4222-8222-000000000002", name: "Zoo" }), photoAlbum({ id: "22222222-2222-4222-8222-000000000004", name: "Same" }), photoAlbum({ id: "22222222-2222-4222-8222-000000000003", name: "Same" }), photoAlbum({ id: "22222222-2222-4222-8222-000000000005", name: "\ue000" }), photoAlbum({ id: "22222222-2222-4222-8222-000000000006", name: "\u{10000}" })];
  const fetcher = vi.fn().mockResolvedValueOnce(response(items))
    .mockResolvedValueOnce(response({ ...items[0], starred: true, revision: 2 })).mockResolvedValueOnce(response({ detail: "List unavailable" }, 503))
    .mockResolvedValueOnce(response({ ...items[0], starred: true, name: "A", revision: 3 })).mockResolvedValueOnce(response({ detail: "List unavailable" }, 503))
    .mockResolvedValueOnce(response({ ...items[1], name: "B", revision: 2 })).mockResolvedValueOnce(response({ detail: "List unavailable" }, 503));
  vi.stubGlobal("fetch", fetcher);
  const albums = new PhotoAlbums("scoped", vi.fn()); await albums.load();
  await albums.update(albums.items[0], { starred: true });
  expect(albums.items.map(item => item.id)).toEqual(["22222222-2222-4222-8222-000000000001", "22222222-2222-4222-8222-000000000003", "22222222-2222-4222-8222-000000000004", "22222222-2222-4222-8222-000000000002", "22222222-2222-4222-8222-000000000005", "22222222-2222-4222-8222-000000000006"]);
  await albums.update(albums.items[0], { name: "A" });
  expect(albums.items[0]).toMatchObject({ name: "A", starred: true, included_count: 0, cover_known: true });
  await albums.update(albums.items.find(item => item.id === "22222222-2222-4222-8222-000000000002")!, { name: "B" });
  expect(albums.items.map(item => item.id)).toEqual(["22222222-2222-4222-8222-000000000001", "22222222-2222-4222-8222-000000000002", "22222222-2222-4222-8222-000000000003", "22222222-2222-4222-8222-000000000004", "22222222-2222-4222-8222-000000000005", "22222222-2222-4222-8222-000000000006"]);
});

it.each(["create", "duplicate", "delete"])("keeps acknowledged %s results after a failed read with only justified observations", async operation => {
  const created = operation === "delete" ? { ...album, revision: 2, deleted_at: "2025-01-01" } : { ...album, id: "22222222-2222-4222-8222-000000000068", name: "A" };
  vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(response(created)).mockResolvedValueOnce(response({ detail: "List unavailable" }, 503)));
  const albums = new PhotoAlbums("scoped", vi.fn()); albums.targetID = album.id; albums.items = [{ ...album, included_count: 10, cover_known: true }];
  if (operation === "create") await albums.create("A"); else if (operation === "duplicate") await albums.duplicate(album, "A"); else expect(await albums.delete(album)).toMatchObject({ deleted_at: "2025-01-01" });
  expect(albums.unconfirmed).toBeUndefined();
  expect(albums.error).toBe(""); expect(albums.loadError).toBe("List unavailable");
  if (operation === "delete") { expect(albums.items).toEqual([]); expect(albums.targetID).toBe(""); return; }
  expect(albums.items.map(item => item.name)).toEqual(["A", "Trip"]);
  expect(albums.items[0]).toMatchObject({ included_count: operation === "create" ? 0 : undefined, cover_known: operation === "create" });
  expect(albums.items[0].effective_cover_asset_id).toBe(operation === "create" ? null : undefined);
});

it.each(["members", "cover"])("invalidates %s observations before an uncertain write and rejects an older list", async operation => {
  let release!: (value: Response) => void;
  let lose!: () => void;
  let reconcile!: (value: Response) => void;
  const fetcher = vi.fn().mockResolvedValueOnce(response([{ ...album, included_count: 10, effective_cover_asset_id: "old", cover_generation_id: "old" }]))
    .mockImplementationOnce(() => new Promise(resolve => release = resolve)).mockImplementationOnce(() => new Promise((_, reject) => lose = () => reject(new TypeError("Failed to fetch"))))
    .mockImplementationOnce(() => new Promise(resolve => reconcile = resolve)).mockResolvedValueOnce(response([album]));
  vi.stubGlobal("fetch", fetcher);
  const albums = new PhotoAlbums("scoped", vi.fn()); await albums.load(); const earlier = albums.load();
  const pending = operation === "members" ? albums.members(album, { asset_ids: ["photo-1"] }, selected(1)) : albums.cover(album, "photo-1");
  await albums.load(); expect(fetcher).toHaveBeenCalledTimes(3);
  release(response([{ ...album, included_count: 10, effective_cover_asset_id: "old" }])); await earlier;
  expect(albums.items[0]).toMatchObject({ included_count: operation === "members" ? undefined : 10, cover_known: false, effective_cover_asset_id: undefined });
  lose(); await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(4));
  await albums.load(); expect(fetcher).toHaveBeenCalledTimes(4); expect(albums.busy).toBe(true);
  reconcile(response({ detail: "List unavailable" }, 503)); await pending;
  expect(albums.error).toBe("Failed to fetch");
  await albums.load(); expect(albums.items[0]).toMatchObject({ included_count: 0, cover_known: true });
});

it.each([false, true])("verifies an off-page selection after an uncertain removal, actually removed=%s", async removed => {
  const prefix = Array.from({ length: 250 }, (_, index) => photo(index + 1000));
  const fetcher = vi.fn();
  if (removed) fetcher.mockRejectedValueOnce(new TypeError("Failed to fetch")); else fetcher.mockResolvedValueOnce(response({ code: "stale_revision" }, 412));
  fetcher.mockResolvedValueOnce(response([{ ...album, revision: 2 }]))
    .mockResolvedValueOnce(response({ items: prefix, total: 400, next_cursor: "next" }))
    .mockResolvedValueOnce(response({ items: removed ? [] : [photo(1)], total: removed ? 0 : 1 }));
  vi.stubGlobal("fetch", fetcher);
  const albums = new PhotoAlbums("scoped", vi.fn()); const photos = selected(0);
  photos.items = [photo(1)]; photos.selection.selectedIDs.add("photo-1");
  photos.query = { ...photoQuery, filters: { set_ids: [album.id] } };
  await albums.members(album, photos.scope(), photos, true);
  expect([...photos.selection.selectedIDs]).toEqual(removed ? [] : ["photo-1"]);
  if (!removed) {
    fetcher.mockResolvedValueOnce(response({ ...album, revision: 3 })).mockResolvedValueOnce(response([{ ...album, revision: 3 }])).mockResolvedValueOnce(response({ items: prefix, total: 400, next_cursor: "next" }));
    await albums.members(albums.items[0], photos.scope(), photos, true);
    expect(JSON.parse(fetcher.mock.calls[4][1].body).asset_ids).toEqual(["photo-1"]); expect(fetcher.mock.calls[4][1].headers.get("If-Match")).toBe('"2"'); expect(photos.selection.selectedIDs.size).toBe(0);
  }
});
