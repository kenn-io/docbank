import { afterEach, expect, it, vi } from "vitest";
import { APIError } from "./api-transport.js";
import * as download from "./download.js";
import { Photos, loadDensity, photoQuery, type PhotoRow } from "./photos.svelte.js";
import { photo, mapPhoto } from "./photo-test-fixtures.js";
import * as snapshots from "./snapshots.js";

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); localStorage.clear(); });
const response = (items: PhotoRow[], cursor?: string, mapProjection = items.some(row => !("content_version_id" in row))) => new Response(JSON.stringify({ ...(mapProjection ? { items: [], map_items: items } : { items }), total: 3, next_cursor: cursor }));
const stubPhotoFetch = (fetcher: (url: string, init: RequestInit) => Promise<Response>, facets: unknown[] = []) => vi.stubGlobal("fetch", (url: string, init: RequestInit) => url.endsWith("/photos/assets/query") && JSON.parse(init.body as string).facets?.length ? Promise.resolve(new Response(JSON.stringify({ facets }))) : fetcher(url, init));

it("cancels the previous scope and carries album sort through paging and refresh", async () => {
  let finish!: (response: Response) => void;
  const fetcher = vi.fn().mockImplementationOnce(() => new Promise(resolve => finish = resolve)).mockResolvedValueOnce(response([photo(2)], "album-next")).mockResolvedValueOnce(response([photo(3)])).mockResolvedValueOnce(response([photo(2), photo(3)]));
  stubPhotoFetch(fetcher);
  const album = { ...photoQuery, filters: { set_ids: ["album"] }, sort: { field: "added_time" as const, direction: "desc" as const } };
  const photos = new Photos("scoped", vi.fn(), false, album);
  photos.items = [photo(1)]; photos.started = true; photos.cursor = "old-next";
  photos.scrollTop = 1200; photos.selectLoaded(); photos.allResults = true;
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  const pending = photos.loadMore();
  const query = { ...album, sort: { field: "import_time" as const, direction: "desc" as const } };
  await photos.setSort(query.sort);
  finish(response([photo(4)])); await pending;
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-2"]);
  expect(photos.selection.selectedIDs.size).toBe(0); expect(photos.allResults).toBe(false); expect(photos.scrollTop).toBe(0);
  await photos.loadMore(); await photos.refresh();
  expect(fetcher.mock.calls.slice(1).map(([, init]) => JSON.parse(init.body).query)).toEqual([query, query, query]);
  expect(fetcher.mock.calls[0][1].signal.aborted).toBe(true);
  photos.dispose();
});

it.each(["confirmed", "partial", "lost response", "rejected"])("retains trash progress and reports completion: %s", async kind => {
  const partial = kind === "partial", lost = kind === "lost response", rejected = kind === "rejected";
  const fetcher = lost ? vi.fn().mockRejectedValueOnce(new TypeError("Failed to fetch")) : vi.fn().mockResolvedValueOnce(rejected ? new Response(JSON.stringify({ detail: "Photo changed" }), { status: 412 }) : new Response(JSON.stringify({ id: "photo-1", revision: 2 })));
  if (partial || rejected) fetcher.mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Photo changed" }), { status: 412 }));
  fetcher.mockResolvedValueOnce(lost ? response([photo(1), photo(2)]) : new Response(JSON.stringify({ detail: "Refresh unavailable" }), { status: 503 }));
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1), photo(2)]; photos.total = 7; photos.started = true;
  if (partial || rejected) photos.selectLoaded();
  else photos.select("photo-1", new MouseEvent("click"), ["photo-1", "photo-2"]);
  const ontrashed = vi.fn();
  expect(await photos.trashSelected(undefined, ontrashed)).toBe(!partial && !lost && !rejected);
  expect(ontrashed).toHaveBeenCalledTimes(rejected ? 0 : 1);
  expect(photos.items.map(item => item.asset_id)).toEqual(lost || rejected ? ["photo-1", "photo-2"] : ["photo-2"]);
  expect(photos.total).toBe(lost ? 3 : rejected ? 7 : 6);
  expect([...photos.selection.selectedIDs]).toEqual(rejected ? ["photo-1", "photo-2"] : lost ? ["photo-1"] : partial ? ["photo-2"] : []);
  expect(photos.trashTargets.map(item => item.asset_id)).toEqual(rejected ? ["photo-1", "photo-2"] : lost ? ["photo-1"] : partial ? ["photo-2"] : []);
  expect(photos.error).toBe(lost ? "" : "Refresh unavailable");
  photos.dispose();
});

const rejectsPreview = (id = "photo-1", unchanged = 0) => ({ targets: [{ asset_id: id, revision: 1, member_revision: 1 }], photos: 1, files: 1, unchanged, mixed: [], mixed_count: 0, selection_limit: 64 });

it("ignores a closed rejects preview after reopening with a different selection", async () => {
  const preview = rejectsPreview("photo-2");
  let finishOld!: (response: Response) => void;
  let finishNew!: (response: Response) => void;
  const fetcher = vi.fn().mockImplementationOnce(() => new Promise(resolve => finishOld = resolve))
    .mockImplementationOnce(() => new Promise(resolve => finishNew = resolve));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.rejectsSelectionLimit = preview.selection_limit;
  photos.selection.selectedIDs = new Set(["photo-1"]);
  const old = photos.previewRejects(true);
  const signal = fetcher.mock.lastCall![1].signal as AbortSignal;
  photos.cancelRejects();
  expect(signal.aborted).toBe(true);
  photos.selection.selectedIDs = new Set(["photo-2"]);
  const current = photos.previewRejects(true);
  finishOld(Response.json({ ...preview, targets: [{ asset_id: "photo-1", revision: 1, member_revision: 1 }] }));
  await old;
  expect(photos.rejects).toBeUndefined();
  expect(photos.rejectsLoading).toBe(true);
  finishNew(Response.json(preview));
  await current;
  expect(photos.rejects).toEqual(preview);
  expect(photos.rejectsLoading).toBe(false);
  photos.dispose();
});

it.each(["Library", "selected", "over limit", "empty selection"])("bounds rejects requests while keeping Library available, scope=%s", async scope => {
  const preview = { ...rejectsPreview("photo-1", 4), selection_limit: 3 };
  const fetcher = vi.fn().mockResolvedValueOnce(Response.json(preview));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  await photos.previewRejects();
  expect(photos.rejectsSelectionLimit).toBe(3);
  fetcher.mockClear();
  fetcher.mockResolvedValueOnce(Response.json(preview));
  photos.selection.selectedIDs = new Set(Array.from({ length: 4 }, (_, i) => `photo-${i}`));
  if (scope === "selected") photos.selection.selectedIDs = new Set(["photo-2", "photo-1"]);
  if (scope === "empty selection") photos.clearSelection();
  await photos.previewRejects(scope !== "Library");
  if (scope === "over limit" || scope === "empty selection") {
    expect(photos.rejectsError).toContain("3 photos");
    expect(fetcher).not.toHaveBeenCalled();
  } else {
    expect(photos.rejectsSelected).toBe(scope === "selected");
    expect(JSON.parse(fetcher.mock.calls[0][1].body).query.filters?.asset_ids).toEqual(scope === "selected" ? ["photo-1", "photo-2"] : undefined);
    expect(photos.rejects).toEqual(preview);
  }
  photos.dispose();
});

it.each(["confirmed", "server", "unknown ID", "missing ID", "maintenance busy", "audit refusal"])("reconciles a rejects move after %s", async outcome => {
  const preview = rejectsPreview();
  const auditDetail = 'photo photo-1, file "reject.jpg": audit mutation unsupported';
  const refused = outcome === "maintenance busy" || outcome === "audit refusal";
  const fetcher = vi.fn().mockResolvedValueOnce(Response.json(preview));
  if (outcome === "confirmed") fetcher.mockResolvedValueOnce(Response.json({ moved: ["photo-1"] }));
  else if (outcome === "unknown ID") fetcher.mockResolvedValueOnce(Response.json({ moved: ["photo-2"] }));
  else if (outcome === "missing ID") fetcher.mockResolvedValueOnce(Response.json({ moved: [] }));
  else if (outcome === "server") fetcher.mockResolvedValueOnce(Response.json({ detail: "Internal error", code: "internal" }, { status: 500 }));
  else if (outcome === "maintenance busy") fetcher.mockResolvedValueOnce(Response.json({ detail: "Move refused", code: "maintenance_busy" }, { status: 503 }));
  else fetcher.mockResolvedValueOnce(Response.json({ detail: auditDetail, code: "audit_mutation_unsupported" }, { status: 409 }));
  fetcher.mockResolvedValueOnce(Response.json({ detail: "Refresh unavailable" }, { status: 503 }));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1), photo(2)]; photos.total = 7; photos.started = true;
  photos.selectLoaded();
  await photos.previewRejects(false);
  const changed = vi.fn();
  const restore = vi.fn(async () => {});
  const preserve = vi.fn(() => restore);
  expect(await photos.trashRejects(preserve, changed)).toBe(outcome === "confirmed");
  expect(JSON.parse(fetcher.mock.calls[1][1].body)).toEqual({ hidden: false, targets: preview.targets });
  expect(changed).toHaveBeenCalledTimes(refused ? 0 : 1);
  expect(photos.rejectsError).toBe(outcome === "confirmed" ? "" : outcome === "maintenance busy" ? "Move refused" : outcome === "audit refusal" ? `${auditDetail} The batch stayed in Docbank. Close this dialog, select up to 64 other photos, and choose Selected photos.` : "The move may have completed. Refresh Photos before trying again.");
  expect(photos.rejects).toBeUndefined();
  expect(photos.error).toBe("Refresh unavailable");
  if (outcome === "confirmed") {
    expect(photos.items).toEqual([photo(2)]);
    expect([...photos.selection.selectedIDs]).toEqual(["photo-2"]);
    expect(preserve).toHaveBeenCalledOnce();
    expect(restore).toHaveBeenCalledOnce();
  } else if (refused) {
    expect([...photos.selection.selectedIDs]).toEqual(["photo-1", "photo-2"]);
  }
  photos.dispose();
});

it("conceals after a confirmed hide while finishing the captured batch and reporting failures", async () => {
  let finish!: (response: Response) => void;
  const fetcher = vi.fn().mockResolvedValueOnce(Response.json({ id: "photo-1", revision: 2 }))
    .mockImplementationOnce(() => new Promise<Response>(resolve => finish = resolve));
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1), photo(2)]; photos.started = true; photos.selectLoaded();
  const report = vi.fn();
  const completed = vi.fn();
  const conceal = vi.fn(async () => photos.dispose());
  const write = photos.setHidden("photo-1", undefined, completed, report, conceal);
  await vi.waitFor(() => expect(finish).toBeDefined());
  expect(conceal).toHaveBeenCalledOnce();
  expect(completed).not.toHaveBeenCalled();
  finish(Response.json({ detail: "Stale revision" }, { status: 412 }));
  await write;
  expect(completed).toHaveBeenCalledOnce();
  expect(report).toHaveBeenLastCalledWith("1 photo failed: Stale revision");
});

it.each([{ kind: "hide", refused: false }, { kind: "trash", refused: false }, { kind: "trash", refused: true }])("reconciles uncertain $kind outcomes and keeps refused targets, refused=$refused", async ({ kind, refused }) => {
  const fetcher = vi.fn();
  if (refused) fetcher.mockResolvedValueOnce(Response.json({ detail: "Stale revision" }, { status: 412 })).mockResolvedValueOnce(response([photo(1)]));
  else fetcher.mockRejectedValueOnce(new TypeError("Reply lost after commit")).mockRejectedValueOnce(new Error("Refresh unavailable"));
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1)]; photos.started = true; photos.selectLoaded();
  const changed = vi.fn();
  if (kind === "trash") expect(await photos.trashSelected(undefined, changed)).toBe(false);
  else await photos.setHidden("photo-1", undefined, changed);
  expect(changed).toHaveBeenCalledTimes(refused ? 0 : 1);
  expect(photos.items).toEqual([photo(1)]);
  expect(photos.trashTargets).toEqual([photo(1)]);
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  expect(fetcher).toHaveBeenCalledTimes(2);
  photos.dispose();
});

it.each([
  ["hidden_not_configured", 409, "Set a passcode in the Hidden view first."],
  ["hidden_locked", 403, "This photo is already hidden."],
])("explains a rejected hide with %s", async (code, status, message) => {
  const fetcher = vi.fn().mockResolvedValueOnce(Response.json({ code, detail: "Server failure" }, { status: Number(status) }))
    .mockResolvedValueOnce(response([photo(1)]));
  stubPhotoFetch(fetcher);
  const authFailure = vi.fn();
  const photos = new Photos("scoped", authFailure);
  photos.items = [photo(1)]; photos.started = true;
  await photos.setHidden("photo-1");
  expect(photos.actionError).toBe(`1 photo failed: ${message}`);
  expect(authFailure).not.toHaveBeenCalled();
  photos.dispose();
});

it("binds retries to captured or displayed revisions", async () => {
  const fetcher = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ id: "photo-1", revision: 2 })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Photo changed" }), { status: 412 }))
    .mockResolvedValueOnce(response([photo(3)]))
    .mockResolvedValueOnce(response([photo(2)]))
    .mockResolvedValueOnce(new Response(JSON.stringify({ id: "photo-2", revision: 2 })))
    .mockResolvedValueOnce(response([photo(3)]));
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1), photo(2)];
  photos.started = true;
  photos.selectLoaded();
  const invalidateDocuments = vi.fn();
  expect(await photos.trashSelected(undefined, invalidateDocuments)).toBe(false);
  expect(invalidateDocuments).toHaveBeenCalledTimes(1);
  expect(fetcher.mock.calls[0][0]).toBe("/api/v1/photos/assets/photo-1/trash");
  expect(new Headers(fetcher.mock.calls[0][1].headers).get("If-Match")).toBe("1");
  expect([...photos.selection.selectedIDs]).toEqual(["photo-2"]);
  expect(photos.trashError).toBe("Photo changed");
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-3"]);
  expect(photos.trashTargets.map(item => item.asset_id)).toEqual(["photo-2"]);
  expect(await photos.trashSelected(undefined, invalidateDocuments)).toBe(true);
  expect(fetcher.mock.calls[4][0]).toBe("/api/v1/photos/assets/photo-2/trash");
  expect(new Headers(fetcher.mock.calls[4][1].headers).get("If-Match")).toBe("1");
  expect(photos.trashTargets).toEqual([]);
  expect(invalidateDocuments).toHaveBeenCalledTimes(2);
  photos.selection.selectedIDs.add("unloaded");
  expect(await photos.trashSelected()).toBe(false);
  photos.dispose();

  const updated = { ...photo(1), revision: 2 };
  const loadedFetcher = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Photo changed" }), { status: 412 }))
    .mockResolvedValueOnce(response([updated]))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Photo changed again" }), { status: 412 }))
    .mockResolvedValueOnce(response([{ ...updated, revision: 3 }]));
  stubPhotoFetch(loadedFetcher);
  const loaded = new Photos("scoped", vi.fn()); loaded.items = [photo(1)]; loaded.started = true; loaded.selectLoaded();
  expect(await loaded.trashSelected()).toBe(false);
  expect(await loaded.trashSelected()).toBe(false);
  expect(loadedFetcher.mock.calls[2][0]).toBe("/api/v1/photos/assets/photo-1/trash");
  expect(new Headers(loadedFetcher.mock.calls[2][1].headers).get("If-Match")).toBe("2");
  expect(loaded.selection.selectedIDs.has("photo-1")).toBe(true);
  loaded.dispose();
});

it.each(["replace", "checkbox", "loaded", "add"])("uses current selection after a failure: %s", async mode => {
  const fetcher = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Photo changed" }), { status: 412 }))
    .mockResolvedValueOnce(response(mode === "checkbox" ? [photo(1), photo(2)] : [photo(2)]));
  if (mode !== "checkbox") fetcher.mockResolvedValueOnce(response([photo(1)]));
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1), photo(2)]; photos.started = true;
  photos.select("photo-1", new MouseEvent("click"), ["photo-1", "photo-2"]);
  expect(await photos.trashSelected()).toBe(false);
  if (mode === "replace") photos.select("photo-2", new MouseEvent("click"), ["photo-2"]);
  if (mode === "checkbox") { photos.check("photo-1", false, false, ["photo-1", "photo-2"]); photos.check("photo-2", true, false, ["photo-1", "photo-2"]); }
  if (mode === "loaded") photos.selectLoaded();
  if (mode === "add") photos.check("photo-2", true, false, ["photo-2"]);
  const expected = mode === "add" ? ["photo-1", "photo-2"] : ["photo-2"];
  for (const id of expected) fetcher.mockResolvedValueOnce(new Response(JSON.stringify({ id, revision: 2 })));
  fetcher.mockResolvedValueOnce(response([]));
  expect(await photos.trashSelected()).toBe(true);
  expect(fetcher.mock.calls.filter(([url]) => url.endsWith("/trash")).slice(1).map(call => call[0])).toEqual(expected.map(id => `/api/v1/photos/assets/${id}/trash`));
  expect(photos.trashTargets).toEqual([]);
  photos.dispose();
});

it("keeps base counts across day changes and rejects a pre-refresh calendar reply", async () => {
  const first = { facets: [{ dimension: "capture_day", available: true, total: 9000, missing: 4, other: 0, values: [] }] } as unknown as snapshots.PhotoTimelineCounts;
  const second = { facets: [{ dimension: "capture_day", available: true, total: 20, missing: 0, other: 0, values: [] }] } as unknown as snapshots.PhotoTimelineCounts;
  let finish!: (page: snapshots.PhotoTimelineCounts) => void;
  const create = vi.spyOn(snapshots, "fetchPhotoTimeline").mockResolvedValueOnce(first)
    .mockImplementationOnce(() => new Promise(resolve => finish = resolve)).mockResolvedValueOnce(second);
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response([photo(1)])));
  const photos = new Photos("scoped", vi.fn());
  await photos.loadTimeline();
  photos.setView("timeline");
  const pending = photos.loadTimeline();
  const oldSignal = create.mock.calls[1][2];
  photos.setView("grid");
  await photos.refresh();
  expect(photos.timeline).toBeUndefined();
  photos.setView("timeline");
  finish(first); await pending;
  expect(oldSignal.aborted).toBe(true);
  await vi.waitFor(() => expect(photos.timeline?.total).toBe(20));
  expect(create.mock.calls[2][1].sort.field).toBe("capture_time");
  expect(create.mock.calls[2][1].filters).toEqual({});
  await photos.selectDate("2024-02-29");
  expect(create).toHaveBeenCalledTimes(3);
  expect(photos.timeline?.total).toBe(20);
  photos.dispose();
});

it.each([
  [new APIError("Too many photos", 413, "snapshot_too_large"), "", 0, false],
  [new APIError("Expired", 401, "unauthorized"), "", 1, undefined],
  [new Error("Scope unavailable"), "Scope unavailable", 0, undefined],
])("maps timeline failure %s", async (error, message, failures, available) => {
  vi.spyOn(snapshots, "fetchPhotoTimeline").mockRejectedValue(error);
  const failure = vi.fn();
  const photos = new Photos("scoped", failure);
  await photos.loadTimeline();
  expect(photos.timeline?.available).toBe(available);
  expect(photos.timelineError).toBe(message);
  expect(failure).toHaveBeenCalledTimes(failures as number);
  photos.dispose();
});

it("resumes an interrupted timeline request", async () => {
  const receipt = { facets: [{ dimension: "capture_day", available: false, reason: "member_budget_exceeded", values: [] }] } as unknown as snapshots.PhotoTimelineCounts;
  const create = vi.spyOn(snapshots, "fetchPhotoTimeline").mockImplementationOnce(() => new Promise(() => {})).mockResolvedValueOnce(receipt);
  const photos = new Photos("scoped", vi.fn());
  photos.setView("timeline");
  expect(photos.timelineLoading).toBe(true);
  photos.cancelPending();
  photos.started = true;
  await photos.resume();
  expect(create).toHaveBeenCalledTimes(2);
  await vi.waitFor(() => expect(photos.timeline?.available).toBe(false));
  photos.dispose();
});

it("seeks an unloaded day, pages within it, and restores the full scope", async () => {
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(1)], "old"))
    .mockResolvedValueOnce(response([photo(4)], "year-page"))
    .mockResolvedValueOnce(response([photo(5)], "month-page"))
    .mockResolvedValueOnce(response([photo(2)], "day-page"))
    .mockResolvedValueOnce(response([photo(3)]))
    .mockResolvedValueOnce(response([photo(1)], "full"));
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore();
  photos.selectLoaded(); photos.scrollTop = 500;
  await photos.selectDate("2025");
  expect(photos.items[0].asset_id).toBe("photo-4");
  expect(photos.date).toBe("2025");
  expect(photos.selection.selectedIDs.size).toBe(0);
  expect(JSON.parse(fetcher.mock.calls[1][1].body).cursor).toBeUndefined();
  await photos.selectDate("2025-12");
  expect(photos.items[0].asset_id).toBe("photo-5");
  expect(photos.date).toBe("2025-12");
  await photos.selectDate("2024-02-29");
  expect(photos.selection.selectedIDs.size).toBe(0);
  expect(photos.scrollTop).toBe(0);
  const request = JSON.parse(fetcher.mock.calls[3][1].body);
  expect(request.cursor).toBeUndefined();
  expect(request.query.filters.capture_after).toBe("2024-02-29");
  await photos.loadMore();
  expect(JSON.parse(fetcher.mock.calls[4][1].body).cursor).toBe("day-page");
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-2", "photo-3"]);
  await photos.selectDate();
  expect(photos.date).toBeUndefined();
  let finish!: (value: Response) => void;
  fetcher.mockImplementationOnce(() => new Promise(resolve => finish = resolve))
    .mockResolvedValueOnce(response([photo(2)]))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Try again" }), { status: 503 }))
    .mockResolvedValueOnce(response([photo(3)]));
  const older = photos.selectDate("2024-01-01");
  await photos.selectDate("2024-02-29");
  finish(response([photo(1)])); await older;
  expect(photos.items[0].asset_id).toBe("photo-2");
  await photos.selectDate("2025-01-01");
  expect(photos.error).toBe("Try again");
  await photos.retry();
  expect(photos.items[0].asset_id).toBe("photo-3");
  photos.dispose();
});

it("keeps a pending date change when the library refreshes", async () => {
  let finish!: (value: Response) => void;
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(1)], "old"))
    .mockImplementationOnce(() => new Promise(resolve => finish = resolve))
    .mockResolvedValueOnce(response([photo(2)]));
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore();
  const pending = photos.selectDate("2024-02-29");
  await photos.refresh();
  finish(response([photo(3)])); await pending;
  expect(photos.date).toBe("2024-02-29");
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-2"]);
  expect(JSON.parse(fetcher.mock.calls[2][1].body).query.filters).toEqual({ capture_after: "2024-02-29", capture_before: "2024-03-01" });
  photos.dispose();
});

it("keeps earlier pages on failure, waits for Retry, and reuses the failed cursor", async () => {
  const fetcher = vi.fn();
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  let finish!: (response: Response) => void;
  let release!: () => void;
  fetcher.mockImplementationOnce(() => new Promise(resolve => finish = resolve))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Temporary read failure" }), { status: 503 }))
    .mockResolvedValueOnce(response([photo(2), photo(3)]));
  const restore = vi.fn(() => new Promise<void>(resolve => release = resolve));
  const preserve = vi.fn(() => restore);
  const initial = photos.loadMore(preserve);
  expect(preserve).not.toHaveBeenCalled();
  finish(response([photo(1)], "next-page"));
  await vi.waitFor(() => expect(restore).toHaveBeenCalledTimes(1));
  await photos.loadMore();
  expect(fetcher).toHaveBeenCalledTimes(1);
  expect(photos.loading).toBe(true);
  release(); await initial;
  restore.mockImplementation(async () => {});
  await photos.loadMore(preserve);
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-1"]);
  expect(photos.error).toBe("Temporary read failure");
  await photos.loadMore();
  expect(fetcher).toHaveBeenCalledTimes(2);
  await photos.retry(preserve);
  expect(preserve).toHaveBeenCalledTimes(2);
  expect(restore).toHaveBeenCalledTimes(2);
  expect(photos.items).toHaveLength(3);
  expect(JSON.parse(fetcher.mock.calls[2][1].body).cursor).toBe("next-page");
  expect(fetcher.mock.calls[0][1].headers.get("X-Docbank-Web-Session")).toBe("scoped");
  await photos.loadMore();
  expect(fetcher).toHaveBeenCalledTimes(3);
  photos.dispose();
});

it.each(["2024", undefined])("resumes interrupted date navigation to %s through load more and keeps the accepted date on failure", async date => {
  let finish!: (value: Response) => void;
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(1)], "old"))
    .mockImplementationOnce(() => new Promise(resolve => finish = resolve))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Try again" }), { status: 503 }))
    .mockResolvedValueOnce(response([photo(2)], "new"));
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  await photos.selectDate("2025-01-01");
  photos.selectLoaded(); photos.scrollTop = 500;
  const interrupted = photos.selectDate(date);
  photos.cancelPending();
  await photos.loadMore();
  finish(response([photo(99)])); await interrupted;
  expect(photos.error).toBe("Try again");
  expect(photos.date).toBe("2025-01-01");
  expect(photos.items[0].asset_id).toBe("photo-1");
  expect(photos.cursor).toBe("old");
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  expect(photos.scrollTop).toBe(500);
  await photos.retry();
  expect(photos.date).toBe(date);
  expect(photos.items[0].asset_id).toBe("photo-2");
  expect(photos.cursor).toBe("new");
  expect(photos.selection.selectedIDs.size).toBe(0);
  expect(photos.scrollTop).toBe(0);
  expect(JSON.parse(fetcher.mock.calls[2][1].body)).toEqual(JSON.parse(fetcher.mock.calls[3][1].body));
  photos.dispose();
});

it("stages multiple replacement pages and preserves accepted rows on a failed refresh", async () => {
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(1), photo(2)], "next"))
    .mockResolvedValueOnce(response([photo(3)], "old-tail"))
    .mockResolvedValueOnce(response([photo(4), photo(1)], "replacement"))
    .mockResolvedValueOnce(new Response("{}", { status: 503 }))
    .mockResolvedValueOnce(response([photo(4), photo(1)], "replacement"))
    .mockResolvedValueOnce(response([photo(2), photo(3)], "new-tail"));
  stubPhotoFetch(fetcher);
  const restore = vi.fn(async () => {});
  const preserve = vi.fn(() => restore);
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore(); await photos.loadMore();
  photos.select("photo-2", new MouseEvent("click"), ["photo-1", "photo-2", "photo-3"]);
  await photos.refresh(preserve);
  expect(preserve).not.toHaveBeenCalled();
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-1", "photo-2", "photo-3"]);
  expect(photos.cursor).toBe("old-tail");
  expect([...photos.selection.selectedIDs]).toEqual(["photo-2"]);
  expect(restore).not.toHaveBeenCalled();
  await photos.resume(preserve);
  expect(fetcher).toHaveBeenCalledTimes(4);
  await photos.retry(preserve);
  expect(JSON.parse(fetcher.mock.calls[4][1].body).cursor).toBeUndefined();
  expect(preserve).toHaveBeenCalledTimes(1);
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-4", "photo-1", "photo-2", "photo-3"]);
  expect(photos.cursor).toBe("new-tail");
  expect(restore).toHaveBeenCalledTimes(1);
  fetcher.mockResolvedValueOnce(new Response(JSON.stringify({ code: "cursor_expired" }), { status: 422 }))
    .mockResolvedValueOnce(response([photo(4), photo(1), photo(2), photo(3)], "renewed"))
    .mockResolvedValueOnce(response([photo(5)], "newer-tail"));
  await photos.loadMore();
  await photos.retry();
  expect(JSON.parse(fetcher.mock.calls[7][1].body).cursor).toBeUndefined();
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-4", "photo-1", "photo-2", "photo-3", "photo-5"]);
  expect([...photos.selection.selectedIDs]).toEqual(["photo-2"]);
  photos.selectLoaded();
  fetcher.mockResolvedValueOnce(response([photo(1)], "more"))
    .mockResolvedValueOnce(response([photo(6), photo(7), photo(8), photo(9)], "bounded"));
  await photos.refresh();
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-1", "photo-6", "photo-7", "photo-8", "photo-9"]);
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  expect(photos.cursor).toBe("bounded");
  const boundedCalls = fetcher.mock.calls.length;
  fetcher.mockResolvedValueOnce(response([photo(9)], "shortcut"));
  await photos.refresh();
  expect(fetcher).toHaveBeenCalledTimes(boundedCalls + 1);
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-9"]);
  expect(photos.cursor).toBe("shortcut");
  let finishReplacement!: (response: Response) => void;
  fetcher.mockImplementationOnce(() => new Promise(resolve => finishReplacement = resolve))
    .mockResolvedValueOnce(response([{ ...photo(10), previews: { ...photo(10).previews, grid: { state: "ready", generation_id: "refreshed" } } }, photo(11)], "resumed-refresh"));
  const interruptedRefresh = photos.refresh();
  photos.cancelPending();
  expect(photos.loading).toBe(false);
  const refreshStart = fetcher.mock.calls.length;
  await photos.loadMore(preserve);
  expect(JSON.parse(fetcher.mock.calls[refreshStart][1].body).cursor).toBeUndefined();
  finishReplacement(response([photo(99)])); await interruptedRefresh;
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-10", "photo-11"]);
  expect(photos.items[0].previews.grid).toEqual({ state: "ready", generation_id: "refreshed" });
  fetcher.mockResolvedValueOnce(new Response(JSON.stringify({ code: "cursor_expired" }), { status: 422 }))
    .mockImplementationOnce(() => new Promise(resolve => finishReplacement = resolve))
    .mockResolvedValueOnce(response([photo(10), photo(11)], "resumed-expiry"))
    .mockResolvedValueOnce(response([photo(12)], "forward"));
  await photos.loadMore();
  const interruptedExpiry = photos.retry();
  photos.cancelPending();
  const expiryStart = fetcher.mock.calls.length;
  await photos.resume(preserve);
  expect(JSON.parse(fetcher.mock.calls[expiryStart][1].body).cursor).toBeUndefined();
  finishReplacement(response([photo(99)])); await interruptedExpiry;
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-10", "photo-11", "photo-12"]);
  const completedCalls = fetcher.mock.calls.length;
  await photos.resume();
  expect(fetcher).toHaveBeenCalledTimes(completedCalls);
  const timer = new AbortController();
  vi.spyOn(AbortSignal, "timeout").mockReturnValue(timer.signal);
  fetcher.mockImplementationOnce((_url, init: RequestInit) => new Promise((_resolve, reject) => init.signal!.addEventListener("abort", () => reject(init.signal!.reason))));
  const refresh = photos.refresh();
  timer.abort(new DOMException("Timed out", "TimeoutError"));
  await refresh;
  expect(photos.error).toContain("timed out");
  photos.dispose();
});

it("restarts Library expiry recovery after its second page fails", async () => {
  const first = Array.from({ length: 250 }, (_, index) => photo(index + 1));
  const second = Array.from({ length: 250 }, (_, index) => photo(index + 251));
  const fetcher = vi.fn().mockResolvedValueOnce(response(first, "expired"))
    .mockResolvedValueOnce(Response.json({ code: "cursor_expired" }, { status: 422 }))
    .mockResolvedValueOnce(response(first, "recovery-second"))
    .mockResolvedValueOnce(Response.json({ detail: "Page unavailable" }, { status: 503 }))
    .mockResolvedValueOnce(response(first, "recovery-second"))
    .mockResolvedValueOnce(response(second, "recovery-third"));
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore(); await photos.loadMore(); await photos.retry();
  expect(photos.items).toEqual(first);
  expect(photos.error).toBe("Page unavailable");
  await photos.retry();
  expect(fetcher.mock.calls.map(([, init]) => JSON.parse(init.body).cursor)).toEqual([undefined, "expired", undefined, "recovery-second", undefined, "recovery-second"]);
  expect(photos.items).toEqual([...first, ...second]);
  expect(photos.cursor).toBe("recovery-third");
  photos.dispose();
});

it.each(["unavailable", "cursor_expired", "invalid_photo_cursor"])("retries a staged Map refresh after %s", async failure => {
  const rejected = failure !== "unavailable";
  const first = mapPhoto(1), second = mapPhoto(2);
  const fetcher = vi.fn().mockResolvedValueOnce(response([first], "second"))
    .mockResolvedValueOnce(Response.json({ code: failure, detail: "Page unavailable" }, { status: rejected ? 422 : 503 }));
  if (rejected) fetcher.mockResolvedValueOnce(response([first], "renewed-second"));
  fetcher.mockResolvedValueOnce(response([second]));
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn(), false, photoQuery, "map");
  photos.items = [mapPhoto(3)]; photos.started = true;
  expect(await photos.refresh()).toBe(false);
  expect(photos.items.map(row => row.asset_id)).toEqual(["photo-3"]);
  await photos.retry();
  expect(fetcher.mock.calls.map(([, init]) => JSON.parse(init.body).cursor)).toEqual(rejected ? [undefined, "second", undefined, "renewed-second"] : [undefined, "second", "second"]);
  expect(photos.items).toEqual([first, second]);
  expect(photos.staged).toBeUndefined();
  photos.dispose();
});

it("ignores canceled reads while a later request continues", async () => {
  let finish!: (value: Response) => void;
  let finishNew!: (value: Response) => void;
  const fetcher = vi.fn().mockImplementationOnce(() => new Promise(resolve => finish = resolve))
    .mockResolvedValueOnce(response([photo(2)], "next"));
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  const initial = photos.resume();
  photos.cancelPending();
  await photos.resume();
  finish(response([photo(1)])); await initial;
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-2"]);
  fetcher.mockImplementationOnce(() => new Promise(resolve => finish = resolve))
    .mockImplementationOnce(() => new Promise(resolve => finishNew = resolve));
  const old = photos.loadMore();
  photos.cancelPending();
  const current = photos.loadMore();
  finish(response([photo(3)])); await old;
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-2"]);
  expect(photos.loading).toBe(true);
  finishNew(response([photo(4)])); await current;
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-2", "photo-4"]);
  fetcher.mockResolvedValueOnce(new Response("{}", { status: 503 }))
    .mockImplementationOnce(() => new Promise(resolve => finish = resolve));
  await photos.refresh();
  const late = photos.retry();
  photos.dispose(); finish(response([photo(3)]));
  await late; await photos.resume();
  expect(photos.items[0].asset_id).toBe("photo-2");
});

it("maps single, modifier and checkbox clicks to selection", () => {
  const photos = new Photos("scoped", vi.fn());
  const ids = ["photo-0", "photo-1", "photo-2"];
  photos.select(ids[0], new MouseEvent("click"), ids);
  photos.select(ids[2], new MouseEvent("click", { shiftKey: true }), ids);
  expect(photos.selection.selectedIDs.size).toBe(3);
  photos.select(ids[1], new MouseEvent("click", { metaKey: true }), ids);
  expect(photos.selection.selectedIDs.has(ids[1])).toBe(false);
  photos.select(ids[2], new MouseEvent("click", { ctrlKey: true }), ids);
  expect(photos.selection.selectedIDs.has(ids[2])).toBe(false);
  photos.check(ids[1], true, false, ids);
  expect(photos.selection.selectedIDs.has(ids[1])).toBe(true);
  photos.select(ids[0], new MouseEvent("click"), ids);
  expect([...photos.selection.selectedIDs]).toEqual([ids[0]]);
  photos.dispose();
});

it("remembers density with safe defaults for unknown or unavailable storage", async () => {
  const photos = new Photos("scoped", vi.fn());
  new Photos("scoped", vi.fn()).setDensity("large");
  expect(photos.density).toBe("large");
  photos.setDensity("compact");
  expect(new Photos("scoped", vi.fn()).density).toBe("compact");
  localStorage.setItem("docbank.photos.density", "__proto__");
  expect(loadDensity()).toBe("comfortable");
  vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => { throw new Error("disabled"); });
  vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => { throw new Error("disabled"); });
  expect(loadDensity()).toBe("comfortable");
  expect(() => photos.setDensity("large")).not.toThrow();
  expect(photos.density).toBe("large");
  photos.dispose();
});

it("times each replacement page separately", async () => {
  const timers: AbortController[] = [];
  vi.spyOn(AbortSignal, "timeout").mockImplementation(() => { const timer = new AbortController(); timers.push(timer); return timer.signal; });
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(1)], "second")).mockResolvedValueOnce(response([photo(2)], "third"))
    .mockResolvedValueOnce(response([photo(1)], "refresh-second"))
    .mockImplementationOnce(async () => { timers[2].abort(new DOMException("Timed out", "TimeoutError")); return response([photo(2)], "third"); });
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore();
  await photos.loadMore();
  await photos.refresh();
  expect(timers).toHaveLength(4);
  expect(photos.error).toBe("");
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-1", "photo-2"]);
  photos.dispose();
});

it("accepts a refresh when the completed listing timeout expires during eligibility lookup", async () => {
  const timer = new AbortController();
  vi.spyOn(AbortSignal, "timeout").mockReturnValueOnce(timer.signal);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1)]; photos.started = true;
  photos.trashTargets = [photo(2)]; photos.selectLoaded(["photo-2"]);
  const fetcher = vi.fn().mockResolvedValueOnce(response([{...photo(1),revision:2}]))
    .mockImplementationOnce(async () => { timer.abort(new DOMException("Timed out", "TimeoutError")); return response([photo(2)]); });
  stubPhotoFetch(fetcher);
  expect(await photos.refresh()).toBe(true);
  expect(photos.items[0].revision).toBe(2);
  expect([...photos.selection.selectedIDs]).toEqual(["photo-2"]);
  expect(photos.error).toBe("");
  photos.dispose();
});

it.each([false, true].flatMap(hidden => ["success", "failure", "cancel"].map(outcome => ({ hidden, outcome }))))("revalidates off-prefix captured selections in bounded queries, hidden=$hidden outcome=$outcome", async ({ hidden, outcome }) => {
  const prefix = Array.from({ length: 250 }, (_, index) => photo(index + 1));
  const previous = prefix.map(item => ({ ...item, name: `Earlier ${item.name}` }));
  const captured = Array.from({ length: 65 }, (_, index) => photo(index + 1001));
  const photos = new Photos("scoped", vi.fn(), hidden);
  photos.items = previous; photos.started = true; photos.trashTargets = captured;
  photos.selection = { selectedIDs: new Set(captured.map(item => item.asset_id)), anchorID: captured[0].asset_id };
  let groups = 0;
  const fetcher = vi.fn(async (url: string, options: RequestInit) => {
    if (!url.endsWith("/query")) return Response.json({ detail:"Stale revision" },{ status:412 });
    const body = JSON.parse(String(options.body));
    expect(body.hidden).toBe(hidden);
    const ids: string[] | undefined = body.query.filters?.asset_ids;
    if (!ids) return response(prefix,"more");
    expect(ids.length).toBeLessThanOrEqual(64);
    groups++;
    if (groups === 2 && outcome === "failure") return Response.json({ detail:"Eligibility unavailable" },{status:503});
    if (groups === 2 && outcome === "cancel") photos.cancelPending();
    return response(captured.filter(item => ids.includes(item.asset_id) && item !== captured[64]).map(item => ({ ...item, revision:7 })));
  });
  vi.stubGlobal("fetch",fetcher);
  expect(await photos.refresh()).toBe(outcome === "success");
  expect(groups).toBe(2);
  expect(photos.items).toEqual(outcome === "success" ? prefix : previous);
  if (outcome === "success") {
    expect([...photos.selection.selectedIDs]).toEqual(captured.slice(0,64).map(item => item.asset_id));
    expect(photos.trashTargets).toEqual(captured.slice(0,64));
    await photos.setHidden(captured[0].asset_id);
    const writes = fetcher.mock.calls.filter(([url]) => !url.endsWith("/query"));
    expect(writes).toHaveLength(64);
    expect(writes[0][0]).toBe(`/api/v1/photos/assets/${captured[0].asset_id}/${hidden ? "unhide" : "hide"}`);
    expect(writes.every(([,options]) => new Headers(options.headers).get("If-Match") === "1")).toBe(true);
  } else {
    expect(photos.trashTargets).toEqual(captured);
    expect(photos.selection.selectedIDs.size).toBe(65);
    expect(photos.error).toBe(outcome === "failure" ? "Eligibility unavailable" : "");
  }
  photos.dispose();
});

it.each([
  { kind: "hide", partial: true },
  { kind: "trash", partial: true },
  { kind: "trash", partial: false },
])("$kind writes keep failures, position and pending targets after failed refresh, partial=$partial", async ({ kind, partial }) => {
  let finish!: (response: Response) => void;
  let writeSignal: AbortSignal | undefined;
  let readSignal: AbortSignal | undefined;
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(1), photo(2), photo(3)]))
    .mockImplementationOnce((_url, options: RequestInit) => { readSignal = options.signal!; return new Promise(resolve => finish = resolve); })
    .mockImplementationOnce(async (_url, options: RequestInit) => { writeSignal = options.signal!; return Response.json({ id: "photo-1", revision: 2 }); });
  if (partial) fetcher.mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Photo changed" }), { status: 412 }));
  fetcher.mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Listing unavailable" }), { status: 503 }));
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore();
  photos.total = 7;
  photos.selection = { selectedIDs: new Set(partial ? ["photo-1", "photo-2"] : ["photo-1"]), anchorID: "photo-1" };
  photos.trashTargets = partial ? [photo(1), photo(2)] : [photo(1)];
  photos.scrollTop = 480;
  const oldRead = photos.refresh();
  const restore = vi.fn(async () => {});
  const write = kind === "trash" ? photos.trashSelected(() => restore) : photos.setHidden("photo-1", () => restore);
  if (kind === "trash") expect(await write).toBe(!partial);
  else await write;
  expect(readSignal?.aborted).toBe(true);
  expect(writeSignal).toBeDefined();
  expect(writeSignal?.aborted).toBe(false);
  finish(response([photo(1), photo(2), photo(3)])); await oldRead;
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-2", "photo-3"]);
  expect([...photos.selection.selectedIDs]).toEqual(partial ? ["photo-2"] : []);
  expect(photos.trashTargets.map(item => item.asset_id)).toEqual(partial ? ["photo-2"] : []);
  expect(photos.total).toBe(6);
  expect(photos.scrollTop).toBe(480);
  expect(restore).toHaveBeenCalledOnce();
  expect(kind === "trash" ? photos.trashError : photos.actionError).toBe(partial ? kind === "trash" ? "Photo changed" : "1 photo failed: Photo changed" : "");
  expect(photos.error).toBe("Listing unavailable");
  fetcher.mockResolvedValueOnce(response([photo(2), photo(3)]));
  await photos.retry();
  expect(kind === "trash" ? photos.trashError : photos.actionError).toBe(partial ? kind === "trash" ? "Photo changed" : "1 photo failed: Photo changed" : "");
  photos.dispose();
});

it.each(["unhide", "trash"])("notifies after a pending %s finishes on a disposed Hidden store", async kind => {
  let finish!: (response: Response) => void;
  let signal: AbortSignal | undefined;
  const fetcher = vi.fn((_url: string, options: RequestInit) => {
    signal = options.signal!;
    return new Promise<Response>(resolve => finish = resolve);
  });
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn(), true);
  photos.items = [photo(1), photo(2)]; photos.started = true; photos.selectLoaded();
  const changed = vi.fn();
  const write = kind === "unhide" ? photos.setHidden("photo-1", undefined, changed) : photos.trashSelected(undefined, changed);
  photos.dispose();
  expect(signal?.aborted).toBe(false);
  finish(Response.json({ id: "photo-1", revision: 2 }));
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
  finish(Response.json({ id: "photo-2", revision: 2 }));
  await write;
  expect(changed).toHaveBeenCalledOnce();
  expect(fetcher.mock.calls.map(([url]) => url)).toEqual([`/api/v1/photos/assets/photo-1/${kind}`, `/api/v1/photos/assets/photo-2/${kind}`]);
});

it("bounds selection writes, rejects unconfirmed success and guards overlapping actions", async () => {
  const timers: AbortController[] = [];
  vi.spyOn(AbortSignal, "timeout").mockImplementation(() => { const timer = new AbortController(); timers.push(timer); return timer.signal; });
  let finish!: (response: Response) => void;
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(1)]))
    .mockImplementationOnce(() => new Promise(resolve => finish = resolve))
    .mockResolvedValueOnce(response([photo(1)]));
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  await photos.loadMore(); photos.selectLoaded();
  const write = photos.setHidden("photo-1");
  await photos.setQuery({ ...photos.query, text: "harbor" });
  expect(photos.query.text).toBe("");
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  await photos.setHidden("photo-1");
  expect(await photos.trashSelected()).toBe(false);
  expect(fetcher).toHaveBeenCalledTimes(2);
  timers[1].abort(new DOMException("Timed out", "TimeoutError"));
  finish(Response.json({ id: "photo-1", revision: 2 })); await write;
  expect(photos.actionError).toContain("Timed out");
  expect(photos.hiding).toBe(false);
  fetcher.mockResolvedValueOnce(Response.json({ id: "photo-2", revision: 2 })).mockResolvedValueOnce(response([photo(1)]));
  await photos.setHidden("photo-1");
  expect(photos.items).toHaveLength(1);
  expect(photos.actionError).toContain("did not confirm");
  expect(photos.hiding).toBe(false);
  photos.dispose();
});

it("reports unresolved visibility selections before sending requests", async () => {
  const fetcher = vi.fn(); stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1)]; photos.selectLoaded(); photos.selection.selectedIDs.add("unloaded");
  const report = vi.fn();
  await photos.setHidden("photo-1", undefined, undefined, report);
  expect(photos.actionError).toContain("Load and select");
  expect(report).toHaveBeenCalledWith(photos.actionError);
  expect(fetcher).not.toHaveBeenCalled();
  photos.dispose();
});

it.each([false, true])("looks up direct photos without replacing loaded results, hidden=%s", async hidden => {
 const item = photo(1); const fetcher = vi.fn().mockResolvedValueOnce(response([photo(2)])); vi.stubGlobal("fetch",fetcher);
 const photos = new Photos("scoped",vi.fn(), hidden); photos.items=[item];
 expect((await photos.lookup("photo-2",new AbortController().signal)).asset_id).toBe("photo-2");
 expect(photos.items).toEqual([item]);
 const fresh = {...item,content_version_id:"new-version"}; fetcher.mockResolvedValueOnce(response([fresh]));
 expect((await photos.lookup(item.asset_id,new AbortController().signal)).content_version_id).toBe("new-version"); expect(photos.items[0]).toEqual(item);
 expect(JSON.parse(fetcher.mock.calls[0][1].body).query.filters.asset_ids).toEqual(["photo-2"]);
 expect(JSON.parse(fetcher.mock.calls[0][1].body).hidden).toBe(hidden);
 expect(fetcher).toHaveBeenCalledTimes(2); photos.dispose();
});

it.each([{ hidden: false, status: 401 }, { hidden: true, status: 403 }])("retains prepared preview slots and applies access cleanup for lookup, preparation and original reads, hidden=$hidden", async ({ hidden, status }) => {
  const fetcher = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ state: "ready", generation_id: "fit-generation" })));
  stubPhotoFetch(fetcher); const onauth = vi.fn(); const photos = new Photos("scoped", onauth, hidden); const item = photo(1); const signal = new AbortController().signal;
  const slot = await photos.preview(item,"fit",signal); expect(item.previews.fit).toEqual(slot);
  expect(JSON.parse(fetcher.mock.calls[0][1].body).hidden).toBe(hidden);
  expect(await photos.preview(item,"fit",signal)).toEqual(slot); expect(fetcher).toHaveBeenCalledOnce();
  for (const operation of [() => photos.lookup(item.asset_id,signal), () => photos.preview(item,"large",signal)]) {
    fetcher.mockResolvedValueOnce(new Response(JSON.stringify({detail:"Session expired"}),{status}));
    await expect(operation()).rejects.toMatchObject({status});
  }
  fetcher.mockClear().mockImplementation(async (url: string) => url === "/api/v1/photos/assets/query"
    ? Response.json({code:"hidden_locked",detail:"Session expired"},{status})
    : Response.json({id:item.node_id,kind:"file",current_version_id:item.content_version_id,blob_hash:"a".repeat(64),size:10,mime_type:"image/jpeg",revision:1,name:item.name}));
  const read = vi.spyOn(download,"readVerifiedPreview").mockResolvedValue({kind:"image",url:"blob:original",mediaType:"image/jpeg"});
  await expect(photos.original(item,signal)).rejects.toMatchObject({status,code:"hidden_locked"});
  expect(fetcher.mock.calls.map(([url]) => url)).toEqual(["/api/v1/photos/assets/query"]);
  expect(read).not.toHaveBeenCalled();
  expect(onauth).toHaveBeenCalledTimes(3); photos.dispose();
});

it.each(["display", "excluded", "version", "download-display", "download-node", "download-version", "download-excluded", "download-error", "download-auth", "download-abort", "download-success"])("checks original photo binding for %s without adopting stale results", async scenario => {
  const old = photo(1), current = {...old, display_file_id:"other-file", node_id:2, content_version_id:"other-version"};
  const onauth = vi.fn(), photos = new Photos("scoped", onauth); photos.items = [old];
  const lookup = vi.spyOn(photos, "lookup"); if (scenario === "display") lookup.mockResolvedValue(current); else if (scenario === "version") lookup.mockResolvedValue(old); else lookup.mockRejectedValue(new APIError("This photo is no longer in Library.", 404, "not_found"));
  const fetcher = vi.fn(async () => new Response(JSON.stringify({id:1, kind:"file", current_version_id:old.content_version_id, blob_hash:"a".repeat(64), size:10, mime_type:"image/jpeg", revision:1, name:old.name})));
  stubPhotoFetch(fetcher); const read = vi.spyOn(download, "readVerifiedPreview").mockResolvedValue({kind:"image", url:"blob:original", mediaType:"image/jpeg"});
  if (scenario.startsWith("download-")) {
    Object.defineProperty(URL, "revokeObjectURL", {configurable:true, value:vi.fn()});
    lookup.mockResolvedValue(old);
    let finish!: (result: {kind:"image"; url:string; mediaType:string}) => void;
    read.mockImplementationOnce(() => new Promise(resolve => finish = resolve));
    const controller = new AbortController(), original = photos.original(old, controller.signal);
    await vi.waitFor(() => expect(read).toHaveBeenCalledOnce());
    if (scenario === "download-display") lookup.mockResolvedValue({...old, display_file_id:"other-file"});
    else if (scenario === "download-node") lookup.mockResolvedValue({...old, node_id:2});
    else if (scenario === "download-version") lookup.mockResolvedValue({...old, content_version_id:"other-version"});
    else if (scenario === "download-excluded") lookup.mockRejectedValue(new APIError("Excluded", 404, "not_found"));
    else if (scenario === "download-error") lookup.mockRejectedValue(new Error("Lookup failed"));
    else if (scenario === "download-auth") { lookup.mockRestore(); fetcher.mockResolvedValue(new Response(JSON.stringify({detail:"Session expired"}), {status:401})); }
    else if (scenario === "download-abort") controller.abort();
    const settled = scenario === "download-success" ? expect(original).resolves.toBe("blob:original")
      : scenario === "download-error" ? expect(original).rejects.toThrow("Lookup failed")
      : scenario === "download-auth" ? expect(original).rejects.toMatchObject({status:401})
      : scenario === "download-abort" ? expect(original).rejects.toMatchObject({name:"AbortError"})
      : expect(original).rejects.toMatchObject({status:409, code:"photo_display_changed"});
    finish({kind:"image", url:"blob:original", mediaType:"image/jpeg"}); await settled;
    if (scenario === "download-success") expect(URL.revokeObjectURL).not.toHaveBeenCalled();
    else expect(URL.revokeObjectURL).toHaveBeenCalledExactlyOnceWith("blob:original");
    expect(onauth).toHaveBeenCalledTimes(scenario === "download-auth" ? 1 : 0);
    expect(photos.items).toEqual([old]); return;
  }
  if (scenario === "version") fetcher.mockResolvedValue(new Response(JSON.stringify({current_version_id:"changed"})));
  await expect(photos.original(old, new AbortController().signal)).rejects.toMatchObject({status:409, code:"photo_display_changed"});
  expect(photos.items).toEqual([old]); expect(fetcher).toHaveBeenCalledTimes(scenario === "version" ? 1 : 0); expect(read).not.toHaveBeenCalled();
  if (scenario === "display") {
    fetcher.mockResolvedValue(new Response(JSON.stringify({id:2, kind:"file", current_version_id:current.content_version_id, blob_hash:"b".repeat(64), size:10, mime_type:"image/jpeg", revision:1, name:current.name})));
    expect(await photos.original(current, new AbortController().signal)).toBe("blob:original"); expect(read.mock.calls[0]![1].nodeID).toBe(2);
  }
});

it.each(["success", "failure", "expired", "invalid_photo_cursor", "cancel"])("pages one GPS collection and resumes its accepted cursor after %s", async outcome => {
 let finish!:(value:Response)=>void;
 const located=mapPhoto;
 const query={...photoQuery,filters:{capture_after:"2024-01-01",gps_bounds:{south:"-90",west:"-180",north:"90",east:"180"}}};
 const fetcher=vi.fn().mockResolvedValueOnce(response([located(2)],"second")).mockImplementationOnce(()=>new Promise(resolve=>finish=resolve));vi.stubGlobal("fetch",fetcher);
 const photos=new Photos("scoped",vi.fn(),false,query,"map");const pending=photos.resume();
 await vi.waitFor(()=>expect(photos.items.map(row=>row.asset_id)).toEqual(["photo-2"]));
 expect(photos.loading).toBe(true);photos.selectLoaded();
 if(outcome === "cancel") photos.cancelPending();
 const rejected=outcome === "expired" || outcome === "invalid_photo_cursor";
 finish(outcome === "failure"?new Response("{}",{status:503}):rejected?Response.json({code:outcome === "expired"?"cursor_expired":outcome},{status:422}):response([located(1),located(3)]));await pending;
 if(outcome !== "success") {
  if(rejected) fetcher.mockResolvedValueOnce(response([located(2)],"second"));
  fetcher.mockResolvedValueOnce(response([located(1),located(3)]));await photos.retry();
 }
 expect(photos.items.map(row=>row.asset_id)).toEqual(["photo-2","photo-1","photo-3"]);expect(photos.total).toBe(3);expect(photos.error).toBe("");
 expect(JSON.parse(fetcher.mock.calls[0]![1].body)).toMatchObject({query,projection:"map"});
 if(outcome === "failure" || outcome === "cancel") expect(JSON.parse(fetcher.mock.calls[2]![1].body).cursor).toBe("second");
 if(rejected) expect(fetcher.mock.calls.map(([,init])=>JSON.parse(init.body).cursor)).toEqual([undefined,"second",undefined,"second"]);
 photos.dispose();
});

it("marks appended pages and clears the marker on other item assignments", async () => {
 const photos=new Photos("scoped",vi.fn(),false,photoQuery,"map");
 const previous=[mapPhoto(1)];photos.items=previous;photos.started=true;photos.cursor="next";
 vi.stubGlobal("fetch",vi.fn().mockResolvedValue(response([mapPhoto(2)])));
 await photos.loadMore();
 expect(photos.appendedFrom).toBe(previous);expect(photos.items).toHaveLength(2);
 photos.items=photos.items.map(row=>({...row,latitude:3}));
 expect(photos.appendedFrom).toBeUndefined();
 photos.dispose();
});

it("reuses seen IDs across pages and resets them when items are replaced", async () => {
 const photos=new Photos("scoped",vi.fn(),false,photoQuery,"map");
 const previous=[mapPhoto(1)];photos.items=previous;photos.started=true;photos.cursor="next";
 const scans=vi.fn(()=>"photo-1");Object.defineProperty(previous[0],"asset_id",{get:scans});
 const fetcher=vi.fn().mockResolvedValueOnce(response([mapPhoto(1),mapPhoto(2)],"last")).mockResolvedValueOnce(response([mapPhoto(2),mapPhoto(3),mapPhoto(3)]));vi.stubGlobal("fetch",fetcher);
 await photos.loadMore();
 expect(scans).toHaveBeenCalledTimes(1);expect(photos.items.map(row=>row.asset_id)).toEqual(["photo-1","photo-2","photo-3"]);
 photos.items=[mapPhoto(2),mapPhoto(4)];photos.cursor="replacement-tail";
 fetcher.mockResolvedValueOnce(response([mapPhoto(1),mapPhoto(2),mapPhoto(3),mapPhoto(4),mapPhoto(5),mapPhoto(5)]));
 await photos.loadMore();
 expect(photos.items.map(row=>row.asset_id)).toEqual(["photo-2","photo-4","photo-1","photo-3","photo-5"]);
 photos.dispose();
});

it.each(["map", "library"])("routes %s Reload authentication failures through the store handler", async projection => {
 const onauth=vi.fn();
 const photos=new Photos("expired",onauth,false,photoQuery,projection === "map"?"map":undefined);
 photos.items=[projection === "map"?mapPhoto(1):photo(1)];photos.started=true;
 const fetcher=vi.fn().mockResolvedValue(Response.json({code:"unauthorized"},{status:401}));vi.stubGlobal("fetch",fetcher);
 await expect(photos.reload("photo-1",new AbortController().signal)).rejects.toMatchObject({status:401});
 expect(onauth).toHaveBeenCalledTimes(1);expect(onauth).toHaveBeenCalledWith(expect.objectContaining({status:401}));
 expect(JSON.parse(fetcher.mock.calls[0]![1].body).projection).toBe(projection === "map"?"map":undefined);
 photos.dispose();
});

it("refreshes the complete GPS query when its accepted population grows", async () => {
 const rows=Array.from({length:250},(_,index)=>mapPhoto(index+1));
 const photos=new Photos("scoped",vi.fn(),false,photoQuery,"map");photos.items=rows;photos.started=true;photos.total=250;
 const fetcher=vi.fn();
 fetcher.mockResolvedValueOnce(Response.json({items:[],map_items:rows,total:251,next_cursor:"last"})).mockResolvedValueOnce(Response.json({items:[],map_items:[mapPhoto(251)],total:251}));vi.stubGlobal("fetch",fetcher);
 await photos.refresh();
 expect(photos.items).toHaveLength(251);expect(photos.total).toBe(251);expect(photos.cursor).toBeUndefined();expect(photos.loading).toBe(false);photos.dispose();
});

it.each(["complete", "partial", "rejected", "uncertain"])("reconciles Map trash without restarting confirmed pages, %s", async outcome => {
 let finish!:(value:Response)=>void;
 const photos=new Photos("scoped",vi.fn(),false,photoQuery,"map");photos.items=[mapPhoto(1),mapPhoto(2)];photos.started=true;photos.total=outcome === "partial"?3:2;photos.selectLoaded(["photo-1"]);
 const fetcher=vi.fn();
 if(outcome === "partial") {photos.cursor="remaining";fetcher.mockImplementationOnce(()=>new Promise(resolve=>finish=resolve));}
 fetcher.mockResolvedValueOnce(outcome === "rejected"?Response.json({detail:"Photo changed"},{status:412}):outcome === "uncertain"?Response.json({detail:"Write unavailable"},{status:503}):Response.json({id:"photo-1",revision:2}));
 if(outcome === "partial") fetcher.mockResolvedValueOnce(response([mapPhoto(3)]));
 if(outcome === "uncertain") fetcher.mockResolvedValueOnce(Response.json({items:[],map_items:[mapPhoto(2)],total:1})).mockResolvedValueOnce(response([],undefined,true));
 if(outcome === "rejected") fetcher.mockResolvedValueOnce(Response.json({items:[],map_items:[mapPhoto(1),mapPhoto(2)],total:2}));
 vi.stubGlobal("fetch",fetcher);const pending=outcome === "partial"?photos.loadMore():undefined;
 await photos.trashSelected();
 if(outcome === "partial") {finish(response([mapPhoto(1)]));await pending;await vi.waitFor(()=>expect(photos.loading).toBe(false));expect(JSON.parse(fetcher.mock.calls[2]![1].body).cursor).toBe("remaining");}
 expect(photos.items.map(row=>row.asset_id)).toEqual(outcome === "rejected"?["photo-1","photo-2"]:outcome === "partial"?["photo-2","photo-3"]:["photo-2"]);
 expect(photos.total).toBe(outcome === "partial" || outcome === "rejected"?2:1);expect([...photos.selection.selectedIDs]).toEqual(outcome === "rejected"?["photo-1"]:[]);
 expect(fetcher).toHaveBeenCalledTimes(outcome === "partial" || outcome === "uncertain"?3:outcome === "rejected"?2:1);photos.dispose();
});

it.each(["library", "map"])("resumes an interrupted %s page after Reload", async projection => {
 let finish!:(value:Response)=>void;
 const mapMode=projection === "map";
 const query={...photoQuery,filters:{gps_bounds:{south:"-90",west:"-180",north:"90",east:"180"}}};
 const fetcher=vi.fn();
 if(mapMode) fetcher.mockResolvedValueOnce(response([mapPhoto(1)],"second"));
 fetcher.mockImplementationOnce(()=>new Promise(resolve=>finish=resolve))
  .mockResolvedValueOnce(response([{...(mapMode?mapPhoto(1):photo(1)),revision:2}]))
  .mockResolvedValueOnce(response(mapMode?[mapPhoto(2)]:[photo(1),photo(2)]));
 if(mapMode) fetcher.mockResolvedValueOnce(response([photo(1)]));
 stubPhotoFetch(fetcher);
 const photos=new Photos("scoped",vi.fn(),false,mapMode?query:photoQuery,mapMode?"map":undefined);
 const pending=mapMode?photos.resume():photos.loadMore();await vi.waitFor(()=>expect(finish).toBeTypeOf("function"));
 expect(photos.started).toBe(mapMode);
 await photos.reload("photo-1",new AbortController().signal);finish(response([mapMode?mapPhoto(3):photo(3)]));await pending;
 await vi.waitFor(()=>expect(mapMode?photos.loading:!photos.started).toBe(false));
 expect(photos.items.map(row=>row.asset_id)).toEqual(["photo-1","photo-2"]);
 if(mapMode) expect(photos.cursor).toBeUndefined();
 expect(JSON.parse(fetcher.mock.calls[mapMode?3:2]![1].body).cursor).toBe(mapMode?"second":undefined);
 photos.dispose();
});

it.each(["fresh", "unlocated", "missing", "aborted"])("reconciles a compact Reload with current membership, %s", async outcome => {
 const member=outcome === "fresh";
 const fresh={...photo(1),name:"Fresh photo.jpg",content_version_id:"fresh-version"};
 const summary={asset_id:fresh.asset_id,revision:2,name:fresh.name,capture_time:fresh.capture_time,width_px:600,height_px:400,latitude:3,longitude:12,previews:{grid:{state:"missing" as const}}};
 let finish!:(value:Response)=>void;
 const fetcher=vi.fn();
 if(outcome === "aborted") fetcher.mockImplementationOnce(()=>new Promise(resolve=>finish=resolve));
 else fetcher.mockResolvedValueOnce(Response.json({items:[],map_items:member?[summary]:[],total:member?1:0}));
 if(outcome !== "missing" && outcome !== "aborted") fetcher.mockResolvedValueOnce(Response.json({items:[fresh],total:1}));
 else if(outcome === "missing") fetcher.mockResolvedValueOnce(Response.json({items:[],total:0}));
 vi.stubGlobal("fetch",fetcher);
 const query={...photoQuery,filters:{gps_bounds:{south:"-90",west:"-180",north:"90",east:"180"}}};
 const photos=new Photos("scoped",vi.fn(),false,query,"map");photos.items=[{...summary,name:"Original photo.jpg",latitude:0},{...summary,asset_id:"photo-2"}];photos.started=true;photos.total=2;photos.selectLoaded();
 const original=photos.items[0];const controller=new AbortController();const reload=photos.reload("photo-1",controller.signal);
 if(outcome === "aborted") {
  controller.abort();finish(Response.json({items:[],map_items:[summary],total:1}));await expect(reload).rejects.toMatchObject({name:"AbortError"});expect(photos.items[0]).toEqual(original);photos.dispose();return;
 }
 if(outcome === "missing") await expect(reload).rejects.toMatchObject({status:404,code:"not_found"});else expect(await reload).toEqual(fresh);
 expect(photos.items.map(row=>row.asset_id)).toEqual(member?["photo-1","photo-2"]:["photo-2"]);expect(photos.total).toBe(member?2:1);expect([...photos.selection.selectedIDs]).toEqual(member?["photo-1","photo-2"]:["photo-2"]);
 if(member) expect(photos.items[0]).toEqual(summary);
 expect(fetcher).toHaveBeenCalledTimes(2);photos.dispose();
});

it("keeps a fresh Reload row when an older replacement finishes late", async () => {
 let finish!:(value:Response)=>void;
 const fresh={...photo(1),content_version_id:"fresh-version"};
 const fetcher=vi.fn().mockImplementationOnce(()=>new Promise(resolve=>finish=resolve)).mockResolvedValueOnce(Response.json({items:[fresh],total:1})).mockResolvedValueOnce(response([fresh,photo(2)]));vi.stubGlobal("fetch",fetcher);
 const photos=new Photos("scoped",vi.fn());photos.items=[photo(1),photo(2)];photos.started=true;photos.total=2;photos.cursor="next";photos.selectLoaded(["photo-2"]);
 const refresh=photos.refresh();expect(await photos.reload("photo-1",new AbortController().signal)).toEqual(fresh);
 finish(Response.json({items:[photo(1)],total:1}));await refresh;
 expect(photos.items[0]).toEqual(fresh);expect(photos.items.map(row=>row.asset_id)).toEqual(["photo-1","photo-2"]);expect(photos.error).toBe("Photo list changed. Retry to finish loading.");expect(photos.cursor).toBe("next");expect([...photos.selection.selectedIDs]).toEqual(["photo-2"]);
 await photos.retry();expect(photos.items[0]).toEqual(fresh);expect(photos.error).toBe("");photos.dispose();
});

it.each(["deselected anchor", "displaced selection"])("reconciles selection across refresh: %s", async kind => {
  if (kind === "deselected anchor") {
    const items = [photo(1), photo(2), photo(3)];
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response(items)));
    const photos = new Photos("scoped", vi.fn()); photos.items = items;
    const ids = items.map(item => item.asset_id);
    photos.select(ids[0], new MouseEvent("click"), ids);
    photos.select(ids[1], new MouseEvent("click", { ctrlKey: true }), ids);
    photos.select(ids[1], new MouseEvent("click", { ctrlKey: true }), ids);
    await photos.refresh();
    expect(photos.selection.anchorID).toBe(ids[1]);
    photos.select(ids[2], new MouseEvent("click", { shiftKey: true }), ids);
    expect([...photos.selection.selectedIDs]).toEqual(ids);
    return;
  }
  const fetcher = vi.fn().mockResolvedValueOnce(response([photo(2)], "next"));
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn()); photos.items = [photo(1)];
  photos.selection = { selectedIDs: new Set(["photo-1", "off-page"]), anchorID: undefined };
  photos.selectLoaded();
  expect([...photos.selection.selectedIDs]).toEqual(["photo-1"]);
  await photos.refresh();
  expect(photos.selection.selectedIDs.size).toBe(0);
  expect(fetcher).toHaveBeenCalledOnce();
  photos.selectAllResults();
  fetcher.mockResolvedValueOnce(response([photo(3)], "next"));
  await photos.refresh();
  expect(photos.scope()).toEqual({ query: photoQuery });
  expect([...photos.selection.selectedIDs]).toEqual(["photo-3"]);
  photos.selectLoaded();
  expect(photos.allResults).toBe(false);
  expect(photos.scope()).toEqual({ asset_ids: ["photo-3"] });
});

it("keeps an album scope through timeline counts, date filters, sort changes, and whole-query selection", async () => {
  const counts = vi.spyOn(snapshots, "fetchPhotoTimeline").mockResolvedValue({ facets: [{ dimension: "capture_day", available: true, total: 1, missing: 0, other: 0, values: [] }] } as unknown as snapshots.PhotoTimelineCounts);
  const fetcher = vi.fn(async () => response([photo(1)]));
  stubPhotoFetch(fetcher);
  const album = { ...photoQuery, filters: { set_ids: ["album"] }, sort: { field: "added_time" as const, direction: "desc" as const } };
  const photos = new Photos("scoped", vi.fn(), false, album);
  const sent = () => JSON.parse((fetcher.mock.calls.at(-1) as unknown as [string, RequestInit])[1].body as string).query;
  await photos.loadTimeline();
  expect(counts.mock.calls[0]![1].filters).toEqual({ set_ids: ["album"] });
  await photos.selectDate("2024-02");
  expect(sent()).toMatchObject({ filters: { set_ids: ["album"], capture_after: "2024-02-01", capture_before: "2024-03-01" }, sort: { field: "added_time" } });
  await photos.setSort({ field: "import_time", direction: "desc" });
  expect(sent()).toMatchObject({ filters: { set_ids: ["album"], capture_after: "2024-02-01" }, sort: { field: "import_time" } });
  photos.selectAllResults();
  expect(photos.scope().query?.filters).toMatchObject({ set_ids: ["album"], capture_after: "2024-02-01" });
  await photos.selectDate();
  expect(sent().filters).toEqual({ set_ids: ["album"] });
  photos.dispose();
});

it("keeps a pending timeline request when an album sort changes", async () => {
  let finish!: (page: snapshots.PhotoTimelineCounts) => void;
  const counts = vi.spyOn(snapshots, "fetchPhotoTimeline").mockImplementation(() => new Promise(resolve => finish = resolve));
  vi.stubGlobal("fetch", vi.fn(async () => response([photo(1)])));
  const photos = new Photos("scoped", vi.fn(), false, { ...photoQuery, filters: { set_ids: ["album"] }, sort: { field: "added_time", direction: "desc" } });
  photos.setView("timeline");
  await vi.waitFor(() => expect(counts).toHaveBeenCalledOnce());
  await photos.setSort({ field: "import_time", direction: "desc" });
  expect(counts.mock.calls[0]![2].aborted).toBe(false);
  expect(photos.timelineLoading).toBe(true);
  finish({ facets: [{ dimension: "capture_day", available: true, total: 1, missing: 0, other: 0, values: [] }] } as unknown as snapshots.PhotoTimelineCounts);
  await vi.waitFor(() => expect(photos.timeline?.total).toBe(1));
  expect(photos.timelineLoading).toBe(false);
  expect(counts).toHaveBeenCalledOnce();
  photos.dispose();
});

it("refuses to hide when the whole query is selected", async () => {
  const fetcher = vi.fn();
  stubPhotoFetch(fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.items = [photo(1), photo(2)]; photos.started = true; photos.total = 500;
  photos.selectAllResults();
  const report = vi.fn();
  await photos.setHidden("photo-1", undefined, undefined, report);
  expect(report).toHaveBeenCalledWith("Hide changes loaded photos only. Choose Select loaded photos first.");
  expect(fetcher).not.toHaveBeenCalled();
  expect(photos.allResults).toBe(true);
  photos.dispose();
});

it("publishes and pages rows before optional counts, and isolates count retry, cancellation and stale scopes", async () => {
  const counts: { request: any; signal: AbortSignal; finish: (value: Response) => void }[] = [];
  const rows = vi.fn().mockResolvedValueOnce(response([photo(1)], "next")).mockResolvedValueOnce(response([photo(2)], "later"));
  vi.stubGlobal("fetch", (url: string, init: RequestInit) => {
    const request = JSON.parse(init.body as string);
    if (request.page_size !== 1) return rows(url, init);
    return new Promise<Response>(finish => counts.push({ request, signal: init.signal!, finish }));
  });
  const photos = new Photos("scope", vi.fn());
  await photos.loadMore();
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-1"]);
  expect(photos.loading).toBe(false);
  expect(photos.facetsLoading).toBe(true);
  await photos.loadMore();
  expect(photos.items).toHaveLength(2);
  expect(photos.cursor).toBe("later");
  expect(rows.mock.calls.map(call => JSON.parse(call[1].body).facets)).toEqual([[], []]);
  photos.cancelPending();
  expect(counts[0].signal.aborted).toBe(true);
  photos.resume();
  expect(counts).toHaveLength(2);
  const facet = { dimension: "camera", available: true, total: 3, values: [], missing: 3, other: 0 };
  counts[0].finish(new Response(JSON.stringify({ facets: [{ ...facet, total: 99 }] })));
  counts[1].finish(new Response(JSON.stringify({ items: [photo(99)], total: 99, next_cursor: "wrong", facets: [facet] })));
  await vi.waitFor(() => expect(photos.facets).toEqual([facet]));
  expect(photos.total).toBe(3);
  expect(photos.cursor).toBe("later");
  expect(photos.items).toHaveLength(2);
  rows.mockResolvedValueOnce(response([photo(3), photo(6)], "refresh-tail"));
  await photos.refresh();
  expect(photos.items[0].asset_id).toBe("photo-3");
  counts[2].finish(new Response(JSON.stringify({ detail: "Counts unavailable" }), { status: 503 }));
  await vi.waitFor(() => expect(photos.facetsError).toBe("Counts unavailable"));
  expect(photos.error).toBe("");
  rows.mockResolvedValueOnce(response([photo(4)], "stale-tail"));
  await photos.loadMore();
  expect(photos.items).toHaveLength(3);
  const retry = photos.retryFacets();
  let finishStale!: (response: Response) => void;
  rows.mockImplementationOnce(() => new Promise(resolve => finishStale = resolve));
  const staleRows = photos.loadMore();
  rows.mockResolvedValueOnce(response([photo(5)]));
  await photos.setQuery({ ...photos.query, text: "Canon", sort: { field: "relevance", direction: "desc" } });
  expect(rows.mock.calls[4][1].signal.aborted).toBe(true);
  finishStale(response([photo(88)])); await staleRows;
  expect(photos.items.map(item => item.asset_id)).toEqual(["photo-5"]);
  expect(counts[3].signal.aborted).toBe(true);
  counts[3].finish(new Response(JSON.stringify({ facets: [facet] })));
  await retry;
  expect(photos.facets).toEqual([]);
  expect(counts[4].request.query.text).toBe("Canon");
  expect(counts[4].request.query.sort).toEqual({ field: "capture_time", direction: "desc" });
  expect(counts[4].request.facets).toEqual(["camera", "lens", "year", "location", "set"]);
  expect(counts[4].request.cursor).toBeUndefined();
  const rowRequests = rows.mock.calls.length;
  await photos.loadMore();
  expect(rows).toHaveBeenCalledTimes(rowRequests);
  expect(photos.cursor).toBeUndefined();
  expect(new Photos("other", vi.fn()).query.text).toBe("");
  photos.dispose();
  expect(counts.at(-1)!.signal.aborted).toBe(true);
  counts.at(-1)!.finish(new Response(JSON.stringify({ facets: [facet] })));
  await vi.waitFor(() => expect(photos.items.map(item => item.asset_id)).toEqual(["photo-5"]));
});


it.each([
  { reason: "time_budget_exceeded", message: "Some photo counts couldn't be loaded.", retryable: true },
  { reason: "member_budget_exceeded", message: "Photo counts exceed the query's limits. Narrow your search or filters.", retryable: false },
  { reason: "byte_budget_exceeded", message: "Photo counts exceed the query's limits. Narrow your search or filters.", retryable: false },
])("handles unavailable $reason counts", async ({ reason, message, retryable }) => {
  const facet = { dimension: "camera", available: true, total: 3, values: [], missing: 3, other: 0 };
  const counts = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ facets: [{ dimension: "camera", available: false, reason }] })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ facets: [facet] })));
  const rows = vi.fn(async () => response([photo(1)]));
  vi.stubGlobal("fetch", (url: string, init: RequestInit) => JSON.parse(init.body as string).page_size === 1 ? counts() : rows());
  const photos = new Photos("scope", vi.fn());
  await photos.loadMore();
  await vi.waitFor(() => expect(photos.facetsError).toBe(message));
  expect(photos.facetsRetryable).toBe(retryable);
  if (retryable) {
    await photos.retryFacets();
    expect(counts).toHaveBeenCalledTimes(2);
    expect(rows).toHaveBeenCalledOnce();
    expect(photos.facetsError).toBe("");
    expect(photos.facets).toEqual([facet]);
  }
  photos.dispose();
});

it("requests filter counts for the library but not for an album", async () => {
  for (const album of [false, true]) {
    const fetcher = vi.fn(async (_url: string, _init: RequestInit) => response([photo(1)]));
    vi.stubGlobal("fetch", fetcher);
    const scope = album ? { ...photoQuery, filters: { set_ids: ["00000000-0000-4000-8000-000000000001"] } } : photoQuery;
    const photos = new Photos("scoped", vi.fn(), false, scope);
    await photos.loadMore();
    await vi.waitFor(() => expect(photos.facetsLoading).toBe(false));
    const counted = fetcher.mock.calls.filter(([, init]) => JSON.parse(String(init.body)).facets?.length).length;
    expect(counted).toBe(album ? 0 : 1);
    photos.dispose();
  }
});
