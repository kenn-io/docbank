import { afterEach, expect, it, vi } from "vite-plus/test";
import { PhotoAlbums, photoDragType } from "./photoAlbums.svelte.js";
import { Photos, photoQuery } from "./photos.svelte.js";
import { photo, photoAlbum, albumResponse as response } from "./photo-test-fixtures.js";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});
const album = photoAlbum();

it("refreshes album dependents when frozen creation loses its response", async () => {
  const frozen = { snapshot_id: "a".repeat(32), member_hash: "b".repeat(64) };
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockRejectedValueOnce(new TypeError("Response lost"))
      .mockResolvedValueOnce(response([album])),
  );
  const membersChanged = vi.fn();
  const albums = new PhotoAlbums("session", vi.fn(), membersChanged);
  await albums.create("Delivery", frozen);
  expect(membersChanged).toHaveBeenCalledExactlyOnceWith(undefined);
  albums.dispose();
});

it.each(["create", "update", "cover", "duplicate", "delete", "add", "remove"] as const)(
  "applies %s before reload and retains it when reload fails",
  async (operation) => {
    const inserted = operation === "create" || operation === "duplicate";
    const result = {
      id: inserted ? "22222222-2222-4222-8222-000000000069" : album.id,
      name: "Holiday",
      revision: inserted ? 1 : 2,
      starred: true,
      created_at: album.created_at,
      updated_at: album.updated_at,
      ...(operation === "delete" ? { deleted_at: album.updated_at } : {}),
    };
    const acknowledged = operation === "delete" ? [] : inserted ? [album, result] : [result];
    const fetcher = vi.fn().mockResolvedValueOnce(response(result));
    vi.stubGlobal("fetch", fetcher);
    let finish!: () => void;
    fetcher.mockImplementationOnce(
      () =>
        new Promise<Response>(
          (resolve) => (finish = () => resolve(response({ detail: "List unavailable" }, 503))),
        ),
    );
    const onmemberschange = vi.fn();
    const albums = new PhotoAlbums("scoped", vi.fn(), onmemberschange);
    albums.items = [album];
    albums.targetID = album.id;
    const pending =
      operation === "create"
        ? albums.create(" Trip ")
        : operation === "update"
          ? albums.update(album, { starred: true })
          : operation === "cover"
            ? albums.cover(album, album.id)
            : operation === "duplicate"
              ? albums.duplicate(album, " Copy ")
              : operation === "delete"
                ? albums.delete(album)
                : albums.members(album, { asset_ids: [album.id] }, operation === "remove");
    await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
    expect(albums.busy).toBe(true);
    expect(albums.items).toEqual(acknowledged);
    expect(albums.targetID).toBe(operation === "delete" ? "" : album.id);
    await albums.create("Blocked");
    expect(fetcher).toHaveBeenCalledTimes(2);
    finish();
    expect(await pending).toEqual(result);
    expect(albums.busy).toBe(false);
    expect(albums.error).toBe("");
    expect(albums.items).toEqual(acknowledged);
    expect(albums.loadError).toBe("List unavailable");
    expect(onmemberschange.mock.calls).toEqual(
      operation === "add" || operation === "remove" ? [[album.id]] : [],
    );
    if (operation === "update") {
      fetcher.mockResolvedValueOnce(response([]));
      await albums.load();
      expect(albums.items).toEqual([]);
      expect(albums.targetID).toBe("");
      expect(albums.loadError).toBe("");
    }
    if (operation !== "create" && operation !== "duplicate")
      expect(fetcher.mock.calls[0][1].headers.get("If-Match")).toBe('"1"');
  },
);

it.each(["update", "cover", "duplicate", "delete", "add", "remove"] as const)(
  "reloads after failed %s and shows the server error",
  async (operation) => {
    const current = { ...album, revision: 3 };
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(
        response(
          { detail: "Album changed", code: "stale_revision" },
          operation === "update" ? 409 : 412,
        ),
      )
      .mockResolvedValueOnce(response([current]));
    vi.stubGlobal("fetch", fetcher);
    const onmemberschange = vi.fn();
    const albums = new PhotoAlbums("scoped", vi.fn(), onmemberschange);
    if (operation === "update") await albums.update(album, { name: "Holiday" });
    else if (operation === "cover") await albums.cover(album, album.id);
    else if (operation === "duplicate") await albums.duplicate(album, "Copy");
    else if (operation === "delete") await albums.delete(album);
    else await albums.members(album, { asset_ids: [album.id] }, operation === "remove");
    expect(albums.items).toEqual([current]);
    expect(albums.error).toBe("Album changed");
    expect(onmemberschange).not.toHaveBeenCalled();
    expect(albums.busy).toBe(false);
  },
);

it.each([false, true])(
  "returns membership feedback before photo refresh and preserves newer feedback, uncertain=%s",
  async (uncertain) => {
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(
        uncertain
          ? response({ detail: "Add unavailable" }, 503)
          : response({ ...album, revision: 2 }),
      )
      .mockResolvedValueOnce(response([{ ...album, included_count: 12000 }]));
    vi.stubGlobal("fetch", fetcher);
    let finishRefresh!: () => void;
    const onmemberschange = vi.fn(() => new Promise<void>((resolve) => (finishRefresh = resolve)));
    const albums = new PhotoAlbums("scoped", vi.fn(), onmemberschange);
    const first = albums.members(album, { query: photoQuery });
    await vi.waitFor(() => expect(onmemberschange).toHaveBeenCalledWith(album.id));
    expect(JSON.parse(fetcher.mock.calls[0][1].body)).toEqual({ query: photoQuery });
    expect(fetcher).toHaveBeenCalledTimes(2);
    expect(await first).toEqual(uncertain ? undefined : { ...album, revision: 2 });
    expect(albums.notice).toBe(uncertain ? "" : "Added to Trip");
    expect(albums.noticeID).toBe(uncertain ? "" : album.id);
    expect(albums.error).toBe(uncertain ? "Add unavailable" : "");
    expect(albums.busy).toBe(false);
    let finishList!: () => void;
    fetcher
      .mockResolvedValueOnce(response({ detail: "Album changed" }, 412))
      .mockImplementationOnce(
        () => new Promise<Response>((resolve) => (finishList = () => resolve(response([album])))),
      );
    const newer = albums.members({ ...album, revision: 2 }, { asset_ids: [album.id] });
    await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(4));
    finishRefresh();
    await Promise.resolve();
    expect(albums.busy).toBe(true);
    expect(albums.error).toBe("Album changed");
    expect(albums.notice).toBe("");
    finishList();
    expect(await newer).toBeUndefined();
    expect(albums.busy).toBe(false);
    expect(albums.error).toBe("Album changed");
    expect(albums.notice).toBe("");
  },
);

it.each(["over-limit", "map over-limit", "whole-query", "external payload", "unselected photo"])(
  "uses the selection scope for a drag: %s",
  async (kind) => {
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(response({ ...album, revision: 2 }))
      .mockResolvedValueOnce(response([album]));
    vi.stubGlobal("fetch", fetcher);
    const albums = new PhotoAlbums("scoped", vi.fn());
    const photos = new Photos("scoped", vi.fn(), false, photoQuery, {
      projection: kind === "map over-limit" ? "map" : undefined,
    });
    photos.total = 12000;
    photos.selection.selectedIDs = new Set(Array.from({ length: 1001 }, (_, id) => String(id)));
    photos.allResults =
      kind !== "over-limit" && kind !== "map over-limit" && kind !== "unselected photo";
    if (kind === "unselected photo") {
      photos.items = [photo(1), photo(2)];
      photos.select("photo-1", new MouseEvent("click"), ["photo-1", "photo-2"]);
    }
    const transfer = { setData: vi.fn(), types: [photoDragType], effectAllowed: "" };
    const event = { dataTransfer: transfer, preventDefault: vi.fn() } as unknown as DragEvent;
    const members = vi.spyOn(albums, "members");
    if (kind === "whole-query") {
      photos.allResults = false;
      albums.startDrag("0", event, photos);
      await albums.drop(album, event);
      photos.allResults = true;
    }
    albums.startDrag(kind === "unselected photo" ? "photo-2" : "0", event, photos);
    if (kind === "over-limit" || kind === "map over-limit") {
      expect(event.preventDefault).toHaveBeenCalledOnce();
      expect(transfer.setData).not.toHaveBeenCalled();
      await albums.drop(album, event);
      expect(fetcher).not.toHaveBeenCalled();
      expect(albums.error).toBe(
        kind === "map over-limit"
          ? "Select 1,000 photos or fewer to add to an album."
          : "Select all 12,000 photos to add more than 1,000 at once.",
      );
    } else if (kind === "unselected photo") {
      expect(photos.allResults).toBe(false);
      expect(albums.drag).toEqual({ asset_ids: ["photo-2"] });
    } else {
      expect(albums.drag).toEqual({ query: photoQuery });
      await albums.drop(album, event);
      expect(members).toHaveBeenCalledWith(album, { query: photoQuery });
      if (kind === "external payload") {
        await albums.drop(album, event);
        expect(members).toHaveBeenCalledOnce();
      } else expect(albums.error).toBe("");
    }
    photos.dispose();
  },
);

it.each([
  { ...album, id: "22222222-2222-4222-8222-000000000068" },
  { ...album, updated_at: "invalid" },
])("rejects malformed or mismatched records and refreshes", async (body) => {
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(response(body))
      .mockResolvedValueOnce(response([album])),
  );
  const albums = new PhotoAlbums("scoped", vi.fn());
  expect(await albums.update(album, { starred: true })).toBeUndefined();
  expect(albums.error).toBe("Invalid album response.");
  expect(albums.items).toEqual([album]);
});

it.each([undefined, '"2"', "network", "invalid ID", "server", "rejection"])(
  "classifies failed creation responses and controls retry: %s",
  async (etag) => {
    const reply =
      etag === "server" || etag === "rejection"
        ? response({ detail: "Create unavailable" }, etag === "server" ? 503 : 400)
        : response(etag === "invalid ID" ? { ...album, id: "invalid" } : album);
    if (etag === undefined) reply.headers.delete("ETag");
    else if (etag !== "invalid ID") reply.headers.set("ETag", etag);
    const fetcher =
      etag === "network"
        ? vi.fn().mockRejectedValueOnce(new TypeError("Failed to fetch"))
        : vi.fn().mockResolvedValueOnce(reply);
    fetcher.mockResolvedValueOnce(response([album]));
    vi.stubGlobal("fetch", fetcher);
    const albums = new PhotoAlbums("scoped", vi.fn());
    expect(await albums.create("Trip")).toBeUndefined();
    expect(albums.unconfirmed).toEqual(etag === "rejection" ? undefined : { name: "Trip" });
    expect(albums.items).toEqual([album]);
    if (etag === "rejection") {
      expect(albums.error).toBe("Create unavailable");
      fetcher.mockResolvedValueOnce(response(album)).mockResolvedValueOnce(response([album]));
      expect(await albums.create("Trip")).toEqual(album);
      expect(fetcher).toHaveBeenCalledTimes(4);
    } else {
      expect(await albums.create("Trip")).toBeUndefined();
      expect(await albums.duplicate(album, "Trip copy")).toBeUndefined();
      expect(fetcher).toHaveBeenCalledTimes(2);
    }
  },
);

it.each([false, true])(
  "keeps only the latest album list response, older failure=%s",
  async (fails) => {
    const finishes: ((value: Response) => void)[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(() => new Promise<Response>((resolve) => finishes.push(resolve))),
    );
    const albums = new PhotoAlbums("scoped", vi.fn());
    albums.targetID = album.id;
    const old = albums.load();
    const latest = albums.load();
    finishes[0](fails ? response({ detail: "Old error" }, 503) : response([]));
    await old;
    expect(albums.loading).toBe(true);
    expect(albums.initialized).toBe(false);
    expect(albums.targetID).toBe(album.id);
    expect(albums.loadError).toBe("");
    finishes[1](response([album]));
    await latest;
    const late = albums.load();
    await Promise.resolve();
    const newest = albums.load();
    finishes[3](response([{ ...album, revision: 3 }]));
    await newest;
    finishes[2](fails ? response({ detail: "Old error" }, 503) : response([]));
    await late;
    expect(albums.items).toEqual([{ ...album, revision: 3 }]);
    expect(albums.targetID).toBe(album.id);
    expect(albums.loadError).toBe("");
    expect(albums.loading).toBe(false);
  },
);

it("recovers a timed-out album refresh without canceling later writes", async () => {
  const deadline = new AbortController();
  vi.spyOn(AbortSignal, "timeout").mockReturnValue(deadline.signal);
  const fetcher = vi
    .fn()
    .mockImplementationOnce(
      (_url: string, init: RequestInit) =>
        new Promise<Response>((_, reject) =>
          init.signal!.addEventListener("abort", () => reject(init.signal!.reason), { once: true }),
        ),
    )
    .mockResolvedValueOnce(response({ ...album, revision: 2, starred: true }))
    .mockResolvedValueOnce(response([{ ...album, revision: 2, starred: true }]));
  vi.stubGlobal("fetch", fetcher);
  const albums = new PhotoAlbums("scoped", vi.fn());
  albums.items = [album];
  const pending = albums.load();
  deadline.abort(new Error("Album list timed out"));
  expect(await pending).toBe(false);
  expect(albums.loadError).toBe("Album list timed out");
  expect(albums.loading).toBe(false);
  expect(albums.items).toEqual([album]);
  vi.mocked(AbortSignal.timeout).mockReturnValue(new AbortController().signal);
  expect(await albums.update(album, { starred: true })).toMatchObject({
    revision: 2,
    starred: true,
  });
  expect(fetcher.mock.calls[1]![1].signal.aborted).toBe(false);
  expect(albums.loadError).toBe("");
  albums.dispose();
});
