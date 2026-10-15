import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { tick } from "svelte";
import PhotoSmartSet from "./PhotoSmartSet.svelte";
import { Photos } from "./photos.svelte.js";
import { PhotoAlbums } from "./photoAlbums.svelte.js";
import { updateSavedQuery } from "./savedQueries.js";
import { parseQuery, queryFingerprint } from "./query.js";
import { savedSmartSet as savedSmartSetFixture } from "./photo-test-fixtures.js";
import * as generated from "./generated/docbank.js";
import type { SavedQueryRunResult } from "./generated/docbank.js";

afterEach(() => {
  localStorage.clear();
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it("saves the applied photo scope, freezes it, and creates an album", async () => {
  const id = "00000000-0000-4000-8000-000000000001";
  const photos = new Photos("session", vi.fn());
  photos.query = parseQuery(
    '{"syntax":"advanced","text":"rating:5 pick label:red","sort":{"field":"relevance","direction":"desc"}}',
  );
  const albums = new PhotoAlbums("session", vi.fn());
  const calls: { url: string; body: Record<string, unknown> }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      if (url.endsWith("/photos/albums") && init?.method !== "POST") return Response.json([]);
      const body = JSON.parse(String(init.body));
      calls.push({ url, body });
      if (url.endsWith("/photos/albums"))
        return Response.json(
          {
            id,
            name: "Delivery",
            starred: false,
            revision: 1,
            created_at: "2026-01-01T00:00:00Z",
            updated_at: "2026-01-01T00:00:00Z",
          },
          { headers: { ETag: '"1"' } },
        );
      if (url.endsWith("/runs"))
        return Response.json({
          run: { total: 1250, expires_at: new Date(Date.now() + 60_000).toISOString() },
          snapshot: { snapshot_id: "a".repeat(32), member_hash: "b".repeat(64) },
        });
      const saved = savedSmartSetFixture(body.payload, await queryFingerprint(body.payload), {
        name: body.name,
      });
      return Response.json(saved, { headers: { ETag: '"1"' } });
    }),
  );
  const navigate = vi.fn();
  render(PhotoSmartSet, { photos, albums, onnavigate: navigate });
  await tick();
  expect(screen.queryByRole("button", { name: "Freeze results" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Save search" }));
  await fireEvent.input(screen.getByRole("textbox", { name: "Search name" }), {
    target: { value: "Picks" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(photos.smartSet?.name).toBe("Picks"));
  expect(screen.queryByRole("button", { name: "Save search" })).toBeNull();
  expect(screen.getByRole("button", { name: "Freeze results" })).toBeTruthy();
  expect(calls[0].body.payload).toEqual(photos.savedPhotoQuery);
  await fireEvent.click(screen.getByRole("button", { name: "Freeze results" }));
  await waitFor(() => expect(screen.getByText(/1,250 photos · Expires/)).toBeTruthy());
  expect(screen.queryByRole("button", { name: "Freeze results" })).toBeNull();
  expect(screen.getByRole("button", { name: "Discard" })).toBeTruthy();
  expect(calls[1].body).toEqual({ photos: true });
  await fireEvent.click(screen.getByRole("button", { name: "Save as album" }));
  await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), {
    target: { value: "Delivery" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(navigate).toHaveBeenCalledWith(`/photos/albums/${id}`));
  expect(photos.frozen).toBeUndefined();
  expect(screen.queryByRole("button", { name: "Save as album" })).toBeNull();
  expect(calls[2].body).toEqual({
    name: "Delivery",
    snapshot_id: "a".repeat(32),
    member_hash: "b".repeat(64),
  });
  photos.dispose();
  albums.dispose();
});

it("discards a frozen result and returns to the smart-set action", async () => {
  const photos = new Photos("session", vi.fn());
  const payload = photos.savedPhotoQuery;
  photos.smartSet = savedSmartSetFixture(payload, await queryFingerprint(payload), {
    name: "Picks",
  });
  photos.setFrozen({
    run: { total: 2, expires_at: new Date(Date.now() + 60_000).toISOString() },
    snapshot: { snapshot_id: "a".repeat(32), member_hash: "b".repeat(64) },
  } as SavedQueryRunResult);
  const albums = new PhotoAlbums("session", vi.fn());
  render(PhotoSmartSet, { photos, albums, onnavigate: vi.fn() });
  await tick();
  expect(screen.getByRole("button", { name: "Save as album" })).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Discard" }));
  expect(photos.frozen).toBeUndefined();
  expect(screen.queryByRole("button", { name: "Save as album" })).toBeNull();
  expect(screen.getByRole("button", { name: "Freeze results" })).toBeTruthy();
  photos.dispose();
  albums.dispose();
});

it("keeps the smart-set draft when initial recovery finishes", async () => {
  const original = new Photos("session", vi.fn());
  const payload = original.savedPhotoQuery;
  const saved = savedSmartSetFixture(payload, await queryFingerprint(payload));
  const createdID = "00000000-0000-4000-8000-000000000002";
  original.rememberSmartSet(saved);
  original.dispose();
  const photos = new Photos("session", vi.fn());
  let finish!: (response: Response) => void;
  const recovery = new Promise<Response>((resolve) => (finish = resolve));
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      if (url.endsWith(`/saved-queries/${saved.id}`)) return recovery;
      if (init?.method === "POST")
        return Response.json(
          { ...saved, id: createdID, name: JSON.parse(init.body as string).name },
          { headers: { ETag: '"1"' } },
        );
      return Response.json([]);
    }),
  );
  const albums = new PhotoAlbums("session", vi.fn());
  render(PhotoSmartSet, { photos, albums, onnavigate: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: "Save search" }));
  await fireEvent.input(screen.getByRole("textbox", { name: "Search name" }), {
    target: { value: "Delivery picks" },
  });
  finish(Response.json(saved, { headers: { ETag: '"1"' } }));
  await screen.findByText("Five stars");
  expect(screen.getByText("Smart set")).toBeTruthy();
  expect((screen.getByRole("textbox", { name: "Search name" }) as HTMLInputElement).value).toBe(
    "Delivery picks",
  );
  await fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await screen.findByText("Delivery picks");
  expect(photos.smartSet?.id).toBe(createdID);
  photos.dispose();
  albums.dispose();
});

it("discards a freeze response after the applied query changes", async () => {
  const photos = new Photos("session", vi.fn());
  const payload = parseQuery(
    '{"text":"harbor","filters":{"kinds":["photo","video"]},"sort":{"field":"capture_time","direction":"desc"}}',
  );
  photos.query = payload;
  photos.smartSet = savedSmartSetFixture(payload, await queryFingerprint(payload), {
    name: "Harbor",
    revision: 3,
  });
  let finish!: (result: SavedQueryRunResult) => void;
  const run = vi
    .spyOn(generated, "runSavedQuery")
    .mockImplementation(() => new Promise((resolve) => (finish = resolve)));
  const albums = new PhotoAlbums("session", vi.fn());
  render(PhotoSmartSet, { photos, albums, onnavigate: vi.fn() });
  await tick();
  await fireEvent.click(screen.getByRole("button", { name: "Freeze results" }));
  expect(run).toHaveBeenCalledOnce();
  const completion: Promise<SavedQueryRunResult> = run.mock.results[0].value;
  photos.query = { ...payload, text: "river" };
  await tick();
  finish({
    run: { total: 1, expires_at: new Date(Date.now() + 60_000).toISOString() },
    snapshot: { snapshot_id: "a".repeat(32), member_hash: "b".repeat(64) },
  } as SavedQueryRunResult);
  await completion;
  await tick();
  expect(photos.frozen).toBeUndefined();
  photos.dispose();
  albums.dispose();
});

it.each(["server expiry", "a member leaves Library", "member hash mismatch"])(
  "keeps the album name and refreezes after %s",
  async (mode) => {
    const photos = new Photos("session", vi.fn());
    const payload = photos.savedPhotoQuery;
    const saved = savedSmartSetFixture(payload, await queryFingerprint(payload));
    photos.smartSet = saved;
    const result = () => ({
      run: { total: 2, expires_at: new Date(Date.now() + 60_000).toISOString() },
      snapshot: { snapshot_id: "a".repeat(32), member_hash: "b".repeat(64) },
    });
    const replacement = {
      run: { total: 2, expires_at: new Date(Date.now() + 60_000).toISOString() },
      snapshot: { snapshot_id: "c".repeat(32), member_hash: "d".repeat(64) },
    };
    const code =
      mode === "a member leaves Library"
        ? "photo_snapshot_member_not_in_library"
        : mode === "member hash mismatch"
          ? "snapshot_member_hash"
          : "snapshot_gone";
    const status =
      code === "snapshot_gone" ? 410 : code === "photo_snapshot_member_not_in_library" ? 412 : 400;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init: RequestInit) => {
        if (url.endsWith("/runs")) return Response.json(replacement);
        if (init?.method === "POST")
          return Response.json(
            { status, code, detail: "query snapshot is missing, expired, or revoked" },
            { status },
          );
        if (url.includes("/saved-queries/"))
          return Response.json(saved, { headers: { ETag: '"1"' } });
        return Response.json([]);
      }),
    );
    const albums = new PhotoAlbums("session", vi.fn());
    render(PhotoSmartSet, { photos, albums, onnavigate: vi.fn() });
    await tick();
    photos.setFrozen(result() as Parameters<Photos["setFrozen"]>[0]);
    await tick();
    await fireEvent.click(screen.getByRole("button", { name: "Save as album" }));
    await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), {
      target: { value: "Delivery" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await vi.waitFor(() => expect(albums.errorCode).toBe(code));
    await tick();
    const recovery =
      mode === "a member leaves Library"
        ? "A frozen photo left Library. Freeze again to use current matches."
        : mode === "member hash mismatch"
          ? "Frozen results could not be verified. Freeze again to save this album."
          : "Frozen results expired. Freeze again to save this album.";
    expect(screen.getByText(recovery)).toBeTruthy();
    if (mode === "server expiry")
      expect(screen.getByText("query snapshot is missing, expired, or revoked")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Save" }).hasAttribute("disabled")).toBe(true);
    expect((screen.getByRole("textbox", { name: "Album name" }) as HTMLInputElement).value).toBe(
      "Delivery",
    );
    await fireEvent.click(screen.getByRole("button", { name: "Freeze again" }));
    await vi.waitFor(() =>
      expect(photos.frozen?.snapshot.snapshot_id).toBe(replacement.snapshot.snapshot_id),
    );
    await tick();
    expect(screen.queryByText(recovery)).toBeNull();
    expect((screen.getByRole("textbox", { name: "Album name" }) as HTMLInputElement).value).toBe(
      "Delivery",
    );
    expect(screen.getByRole("button", { name: "Save" }).hasAttribute("disabled")).toBe(false);
    photos.dispose();
    albums.dispose();
  },
);

it("refreshes a renamed definition through the subscription", async () => {
  const id = "00000000-0000-4000-8000-000000000001";
  const payload = parseQuery(
    '{"text":"harbor","filters":{"kinds":["photo","video"]},"sort":{"field":"name","direction":"asc"}}',
  );
  const saved = savedSmartSetFixture(payload, await queryFingerprint(payload), {
    id,
    name: "Picks",
  });
  let current = saved;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      if (init?.method === "PATCH") current = { ...current, name: "Renamed", revision: 2 };
      return Response.json(current, { headers: { ETag: `"${current.revision}"` }, status: 200 });
    }),
  );
  const photos = new Photos("session", vi.fn());
  photos.query = payload;
  photos.smartSet = saved;
  const albums = new PhotoAlbums("session", vi.fn());
  render(PhotoSmartSet, { photos, albums, onnavigate: vi.fn() });
  await updateSavedQuery("session", saved, { name: "Renamed" });
  expect(await screen.findByText("Renamed")).toBeTruthy();
  photos.dispose();
  albums.dispose();
});

const freezeRecoveryCases = [
  {
    name: "snapshot gone",
    kind: "error",
    code: "snapshot_gone",
    status: 410,
    message: "Frozen results are unavailable. Try freezing again.",
  },
  {
    name: "snapshot capacity",
    kind: "error",
    code: "snapshot_capacity",
    status: 429,
    message: "Photo searches are busy. Try freezing again in a moment.",
  },
  {
    name: "snapshot builders busy",
    kind: "error",
    code: "snapshot_busy",
    status: 429,
    message: "Photo searches are busy. Try freezing again in a moment.",
  },
  {
    name: "photo leaving Library",
    kind: "error",
    code: "photo_snapshot_member_not_in_library",
    status: 412,
    message: "A photo left Library while freezing. Try freezing again.",
  },
  {
    name: "deleted smart set",
    kind: "error",
    code: "not_found",
    status: 404,
    message: "This smart set was deleted. Save the search as a new smart set.",
  },
  { name: "changed revision", kind: "changed", change: "revision" },
  { name: "changed query", kind: "changed", change: "query" },
] as const;

it.each(freezeRecoveryCases)("handles $name during freeze", async (scenario) => {
  const photos = new Photos("session", vi.fn());
  const payload = photos.savedPhotoQuery;
  const saved = savedSmartSetFixture(payload, await queryFingerprint(payload));
  const refreshedPayload =
    scenario.kind === "changed" && scenario.change === "query"
      ? { ...payload, text: "changed" }
      : payload;
  photos.smartSet = saved;
  if (scenario.kind === "error")
    photos.setFrozen({
      run: { total: 1, expires_at: new Date(Date.now() + 60_000).toISOString() },
      snapshot: { snapshot_id: "b".repeat(32), member_hash: "c".repeat(64) },
    } as SavedQueryRunResult);
  let runs = 0;
  const revisions: (string | null)[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      if (!url.endsWith("/runs")) {
        if (scenario.kind === "error")
          return Response.json({ code: "not_found", detail: "gone" }, { status: 404 });
        const refreshed = savedSmartSetFixture(
          refreshedPayload,
          await queryFingerprint(refreshedPayload),
          { revision: 2 },
        );
        return Response.json(refreshed, { headers: { ETag: '"2"' } });
      }
      if (scenario.kind === "error")
        expect(JSON.parse(init.body as string).replaces_snapshot_id).toBe("b".repeat(32));
      revisions.push(new Headers(init.headers).get("If-Match"));
      if (scenario.kind === "error")
        return Response.json(
          { status: scenario.status, code: scenario.code, detail: "freeze refused" },
          { status: scenario.status },
        );
      if (!runs++)
        return Response.json({ code: "stale_revision", detail: "changed" }, { status: 412 });
      return Response.json({
        run: { total: 1, expires_at: new Date(Date.now() + 60_000).toISOString() },
        snapshot: { snapshot_id: "a".repeat(32), member_hash: "b".repeat(64) },
      });
    }),
  );
  const albums = new PhotoAlbums("session", vi.fn());
  render(PhotoSmartSet, { photos, albums, onnavigate: vi.fn() });
  await fireEvent.click(
    screen.getByRole("button", {
      name: scenario.kind === "error" ? "Freeze again" : "Freeze results",
    }),
  );
  if (scenario.kind === "error") {
    expect(await screen.findByRole("alert")).toHaveProperty("textContent", scenario.message);
    if (
      scenario.code === "snapshot_capacity" ||
      scenario.code === "snapshot_busy" ||
      scenario.code === "snapshot_gone" ||
      scenario.code === "photo_snapshot_member_not_in_library"
    )
      expect(photos.frozen?.snapshot.snapshot_id).toBe("b".repeat(32));
  } else {
    expect(await screen.findByRole("alert")).toHaveProperty(
      "textContent",
      "This smart set changed. Reopen it from Albums to use the latest version.",
    );
    if (scenario.change === "revision") {
      await waitFor(() => expect(photos.matchingSmartSet?.revision).toBe(2));
      await fireEvent.click(screen.getByRole("button", { name: "Freeze results" }));
      expect(await screen.findByText(/1 photo · Expires/)).toBeTruthy();
      expect(revisions).toEqual(['"1"', '"2"']);
    } else {
      expect(photos.matchingSmartSet).toBeUndefined();
      expect(screen.queryByRole("button", { name: "Freeze results" })).toBeNull();
      expect(screen.getByRole("button", { name: "Save as new search" })).toBeTruthy();
      expect(revisions).toEqual(['"1"']);
    }
  }
  photos.dispose();
  albums.dispose();
});

it("warns without navigating when a frozen album creation loses its response", async () => {
  const id = "00000000-0000-4000-8000-000000000002";
  const photos = new Photos("session", vi.fn());
  const payload = photos.savedPhotoQuery;
  photos.smartSet = savedSmartSetFixture(payload, await queryFingerprint(payload));
  const frozen = {
    run: { total: 2, expires_at: new Date(Date.now() + 60_000).toISOString() },
    snapshot: { snapshot_id: "a".repeat(32), member_hash: "b".repeat(64) },
  } as SavedQueryRunResult;
  photos.setFrozen(frozen);
  let committed = false;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      if (url.endsWith("/photos/albums") && init?.method === "POST") {
        committed = true;
        throw new TypeError("Response lost");
      }
      if (url.endsWith("/photos/albums"))
        return Response.json(
          committed
            ? [
                {
                  id,
                  name: "Delivery",
                  revision: 1,
                  starred: false,
                  created_at: "2026-01-01T00:00:00Z",
                  updated_at: "2026-01-01T00:00:00Z",
                },
              ]
            : [],
        );
      throw new Error(`Unexpected request: ${url}`);
    }),
  );
  const albums = new PhotoAlbums("session", vi.fn());
  const navigate = vi.fn();
  render(PhotoSmartSet, { photos, albums, onnavigate: navigate });
  await fireEvent.click(screen.getByRole("button", { name: "Save as album" }));
  await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), {
    target: { value: "Delivery" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await screen.findByRole("alert");
  expect(navigate).not.toHaveBeenCalled();
  expect(photos.frozen).toBe(frozen);
  photos.dispose();
  albums.dispose();
});

it("keeps a frozen result when album saving waits for another album change", async () => {
  const photos = new Photos("session", vi.fn());
  const payload = photos.savedPhotoQuery;
  photos.smartSet = savedSmartSetFixture(payload, await queryFingerprint(payload));
  const frozen = {
    run: { total: 2, expires_at: new Date(Date.now() + 60_000).toISOString() },
    snapshot: { snapshot_id: "a".repeat(32), member_hash: "b".repeat(64) },
  } as SavedQueryRunResult;
  photos.setFrozen(frozen);
  const fetcher = vi.fn(async () => Response.json([]));
  vi.stubGlobal("fetch", fetcher);
  const albums = new PhotoAlbums("session", vi.fn());
  render(PhotoSmartSet, { photos, albums, onnavigate: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: "Save as album" }));
  await fireEvent.input(screen.getByRole("textbox", { name: "Album name" }), {
    target: { value: "Delivery" },
  });
  albums.busy = true;
  albums.errorCode = "snapshot_gone";
  await tick();
  expect(screen.getByRole("button", { name: "Save" })).toHaveProperty("disabled", true);
  await fireEvent.submit(screen.getByRole("textbox", { name: "Album name" }).closest("form")!);
  expect(await screen.findByRole("alert")).toHaveProperty(
    "textContent",
    "Another album change is still running. Try again.",
  );
  expect(photos.frozen).toBe(frozen);
  expect(photos.frozenExpired).toBe(false);
  expect(fetcher).not.toHaveBeenCalled();
  photos.dispose();
  albums.dispose();
});
