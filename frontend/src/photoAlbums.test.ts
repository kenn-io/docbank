import { afterEach, expect, it, vi } from "vitest";
import { PhotoAlbums, photoDragType } from "./photoAlbums.svelte.js";
import { Photos, photoQuery } from "./photos.svelte.js";
import { photoAlbum, albumResponse as response } from "./photo-test-fixtures.js";

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });
const album = photoAlbum();

it.each(["create", "update", "cover", "duplicate", "delete", "add", "remove"] as const)("reloads after %s and holds busy until the list settles", async operation => {
  const result = { ...album, revision: 2 };
  const fetcher = vi.fn().mockResolvedValueOnce(response(result));
  vi.stubGlobal("fetch", fetcher);
  let finish!: () => void;
  fetcher.mockImplementationOnce(() => new Promise<Response>(resolve => finish = () => resolve(response([result]))));
  const albums = new PhotoAlbums("scoped", vi.fn());
  const pending = operation === "create" ? albums.create(" Trip ") : operation === "update" ? albums.update(album, { starred: true }) : operation === "cover" ? albums.cover(album, album.id) : operation === "duplicate" ? albums.duplicate(album, " Copy ") : operation === "delete" ? albums.delete(album) : albums.members(album, { asset_ids: [album.id] }, operation === "remove");
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
  expect(albums.busy).toBe(true);
  await albums.create("Blocked");
  expect(fetcher).toHaveBeenCalledTimes(2);
  finish();
  expect(await pending).toEqual(result);
  expect(albums.busy).toBe(false);
  expect(albums.error).toBe("");
  if (operation !== "create" && operation !== "duplicate") expect(fetcher.mock.calls[0][1].headers.get("If-Match")).toBe('"1"');
});

it.each(["create", "update", "cover", "duplicate", "delete", "add", "remove"] as const)("reloads after failed %s and shows the server error", async operation => {
  const current = { ...album, revision: 3 };
  const fetcher = vi.fn().mockResolvedValueOnce(response({ detail: "Album changed", code: "stale_revision" }, 412)).mockResolvedValueOnce(response([current]));
  vi.stubGlobal("fetch", fetcher);
  const albums = new PhotoAlbums("scoped", vi.fn());
  if (operation === "create") await albums.create("Trip");
  else if (operation === "update") await albums.update(album, { name: "Holiday" });
  else if (operation === "cover") await albums.cover(album, album.id);
  else if (operation === "duplicate") await albums.duplicate(album, "Copy");
  else if (operation === "delete") await albums.delete(album);
  else await albums.members(album, { asset_ids: [album.id] }, operation === "remove");
  expect(albums.items).toEqual([current]);
  expect(albums.error).toBe("Album changed");
  expect(albums.busy).toBe(false);
});

it("reloads after a lost create response so an existing album can be chosen", async () => {
  vi.stubGlobal("fetch", vi.fn().mockRejectedValueOnce(new TypeError("Failed to fetch")).mockResolvedValueOnce(response([album])));
  const albums = new PhotoAlbums("scoped", vi.fn());
  expect(await albums.create("Trip")).toBeUndefined();
  expect(albums.items).toEqual([album]);
  expect(albums.error).toBe("Failed to fetch");
});

it("keeps the loaded list when a reload fails and permits a later retry", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(response({ ...album, starred: true, revision: 2 })).mockResolvedValueOnce(response({ detail: "List unavailable" }, 503)).mockResolvedValueOnce(response([])));
  const albums = new PhotoAlbums("scoped", vi.fn()); albums.items = [album]; albums.targetID = album.id;
  await albums.update(album, { starred: true });
  expect(albums.items).toEqual([album]);
  expect(albums.loadError).toBe("List unavailable");
  await albums.load();
  expect(albums.items).toEqual([]);
  expect(albums.targetID).toBe("");
  expect(albums.loadError).toBe("");
});

it("sends whole-query membership in one atomic write and links its feedback", async () => {
  const fetcher = vi.fn().mockResolvedValueOnce(response({ ...album, revision: 2 })).mockResolvedValueOnce(response([{ ...album, included_count: 12000 }]));
  vi.stubGlobal("fetch", fetcher);
  const albums = new PhotoAlbums("scoped", vi.fn());
  await albums.members(album, { query: photoQuery });
  expect(JSON.parse(fetcher.mock.calls[0][1].body)).toEqual({ query: photoQuery });
  expect(fetcher).toHaveBeenCalledTimes(2);
  expect(albums.notice).toBe("Added to Trip · now 12,000 photos");
  expect(albums.noticeID).toBe(album.id);
});

it("shows the server error when 1,001 IDs are dropped", async () => {
  const fetcher = vi.fn().mockResolvedValueOnce(response({ detail: "Too many photos", code: "invalid_photo_album" }, 422)).mockResolvedValueOnce(response([album]));
  vi.stubGlobal("fetch", fetcher);
  const albums = new PhotoAlbums("scoped", vi.fn());
  albums.drag = { asset_ids: Array.from({ length: 1001 }, (_, id) => String(id)) };
  await albums.drop(album, { dataTransfer: { types: [photoDragType] }, preventDefault: vi.fn() } as unknown as DragEvent);
  expect(JSON.parse(fetcher.mock.calls[0][1].body).asset_ids).toHaveLength(1001);
  expect(albums.error).toBe("Too many photos");
});

it.each([{ ...album, id: "invalid" }, { ...album, id: "22222222-2222-4222-8222-000000000068" }, { ...album, updated_at: "invalid" }])("rejects malformed or mismatched records and refreshes", async body => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(response(body)).mockResolvedValueOnce(response([album])));
  const albums = new PhotoAlbums("scoped", vi.fn());
  expect(await albums.update(album, { starred: true })).toBeUndefined();
  expect(albums.error).toBe("Invalid album response.");
  expect(albums.items).toEqual([album]);
});

it("uses the selection scope for an in-app drag and rejects external payloads", async () => {
  const albums = new PhotoAlbums("scoped", vi.fn());
  const photos = new Photos("scoped", vi.fn()); photos.selection.selectedIDs.add("photo-1"); photos.allResults = true;
  const transfer = { setData: vi.fn(), types: [photoDragType], effectAllowed: "" };
  albums.startDrag("photo-1", { dataTransfer: transfer } as unknown as DragEvent, photos);
  expect(albums.drag).toEqual({ query: photoQuery });
  const members = vi.spyOn(albums, "members").mockResolvedValue(album);
  await albums.drop(album, { dataTransfer: transfer, preventDefault: vi.fn() } as unknown as DragEvent);
  expect(members).toHaveBeenCalledWith(album, { query: photoQuery });
  await albums.drop(album, { dataTransfer: transfer, preventDefault: vi.fn() } as unknown as DragEvent);
  expect(members).toHaveBeenCalledOnce();
  albums.startDrag("unselected", { dataTransfer: transfer } as unknown as DragEvent, photos);
  expect(albums.drag).toEqual({ asset_ids: ["unselected"] });
  photos.dispose();
});

it.each([false, true])("keeps only the latest album list response, older failure=%s", async fails => {
  const finishes: ((value: Response) => void)[] = [];
  vi.stubGlobal("fetch", vi.fn(() => new Promise<Response>(resolve => finishes.push(resolve))));
  const albums = new PhotoAlbums("scoped", vi.fn()); albums.targetID = album.id;
  const old = albums.load();
  const latest = albums.load();
  finishes[0](fails ? response({ detail: "Old error" }, 503) : response([]));
  await old;
  expect(albums.loading).toBe(true);
  expect(albums.initialized).toBe(false);
  expect(albums.targetID).toBe(album.id);
  expect(albums.loadError).toBe("");
  finishes[1](response([album])); await latest;
  const late = albums.load();
  await Promise.resolve();
  const newest = albums.load();
  finishes[3](response([{ ...album, revision: 3 }])); await newest;
  finishes[2](fails ? response({ detail: "Old error" }, 503) : response([])); await late;
  expect(albums.items).toEqual([{ ...album, revision: 3 }]);
  expect(albums.targetID).toBe(album.id);
  expect(albums.loadError).toBe("");
  expect(albums.loading).toBe(false);
});
