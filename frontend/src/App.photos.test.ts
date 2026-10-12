import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { photo, photoAlbum, albumResponse, storage } from "./photo-test-fixtures.js";
import App from "./App.svelte";
import { Photos } from "./photos.svelte.js";

afterEach(() => { cleanup(); history.replaceState(null, "", "/"); vi.unstubAllGlobals(); vi.restoreAllMocks(); localStorage.clear(); new Photos("scoped", vi.fn()).setDensity("comfortable"); });

it.each(["server", "stale first page"])("refreshes an album opened again during a delayed membership write with a %s response", async outcome => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  Element.prototype.scrollIntoView = vi.fn();
  const album = photoAlbum();
  let included = false;
  let finish!: () => void;
  let releaseStale: (() => void) | undefined;
  let scopedReads = 0;
  vi.stubGlobal("fetch", vi.fn(async (url: string, init: RequestInit) => {
    if (url.endsWith("/photos/albums")) return albumResponse([{ ...album, included_count: included ? 1 : 0 }]);
    if (url.endsWith("/photos/assets/query")) {
      const scoped = JSON.parse(init.body as string).query.filters?.set_ids?.includes(album.id);
      if (outcome === "stale first page" && scoped && ++scopedReads === 1) { await new Promise<void>(resolve => releaseStale = resolve); return albumResponse({ items: [], total: 0 }); }
      return albumResponse({ items: scoped && !included ? [] : [photo(1)], total: scoped && !included ? 0 : 1 });
    }
    if (url.endsWith(`/photos/albums/${album.id}/members/add`)) {
      await new Promise<void>(resolve => finish = resolve);
      included = true;
      return outcome === "server" ? albumResponse({ detail: "Add unavailable" }, 503) : albumResponse({ ...album, revision: 2 });
    }
    if (url.includes("/nodes/1")) return albumResponse({ id: 1, kind: "dir", name: "", revision: 1, path: "/" });
    return albumResponse({ items: [], nodes: [], tags: [], profiles: [] });
  }));
  render(App);
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: /Add to album/ }));
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Trip" }));
  await waitFor(() => expect(finish).toBeTypeOf("function"));
  await fireEvent.click(screen.getByRole("button", { name: /^Trip/ }));
  if (outcome === "stale first page") {
    await waitFor(() => expect(releaseStale).toBeTypeOf("function"));
  } else {
    await screen.findByText("This album is empty");
    await fireEvent.click(screen.getByRole("button", { name: "Library" }));
    await screen.findByRole("button", { name: "Select Photo 1.jpg" });
    await fireEvent.click(screen.getByRole("button", { name: /^Trip/ }));
    await screen.findByText("This album is empty");
  }
  finish();
  if (outcome === "stale first page") {
    await waitFor(() => expect(scopedReads).toBe(2));
    releaseStale!();
  }
  if (outcome !== "stale first page") await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  await screen.findByText("1 photos · 1 loaded", undefined, { timeout: 5000 });
  expect(screen.queryByText("This album is empty")).toBeNull();
});

it("opens an album from the index, preserves its sort scope, and unstars without refetching", async () => {
  history.replaceState(null, "", "/photos/albums#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  Element.prototype.scrollIntoView = vi.fn();
  const album = photoAlbum({ starred: true, member_count: 1, included_count: 1 });
  const queries: unknown[] = [];
  const fetcher = vi.fn(async (url: string, init: RequestInit) => {
    if (url.endsWith("/photos/albums")) return new Response(JSON.stringify([album]));
    if (url.endsWith(`/photos/albums/${album.id}`) && init.method === "PUT") return albumResponse({ ...album, revision: 2, starred: false });
    if (url.endsWith("/photos/assets/query")) { queries.push(JSON.parse(init.body as string).query); return new Response(JSON.stringify({ items: [photo(1)], total: 1 })); }
    if (url.includes("/nodes/1")) return new Response(JSON.stringify({ id: 1, kind: "dir", name: "", revision: 1, path: "/" }));
    return new Response(JSON.stringify({ items: [], nodes: [], tags: [], profiles: [] }));
  });
  vi.stubGlobal("fetch", fetcher);
  render(App);
  await fireEvent.click(await screen.findByRole("link", { name: /Trip.*1 photo/ }));
  expect(location.pathname).toBe(`/photos/albums/${album.id}`);
  await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  expect(queries[0]).toMatchObject({ filters: { set_ids: [album.id] }, sort: { field: "added_time", direction: "desc" } });
  await fireEvent.click(screen.getByRole("combobox", { name: /Sort photos/ }));
  await fireEvent.click(await screen.findByRole("option", { name: "Imported" }));
  await waitFor(() => expect(queries.at(-1)).toMatchObject({ filters: { set_ids: [album.id] }, sort: { field: "import_time", direction: "desc" } }));
  expect(screen.getByRole("combobox", { name: "Sort photos: Imported" })).toBeTruthy();
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
  album.included_count = 7;
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  expect(location.pathname).toBe(`/photos/albums/${album.id}`);
  expect(await screen.findByRole("combobox", { name: "Sort photos: Imported" })).toBeTruthy();
  await screen.findByText("1 selected photo");
  await waitFor(() => expect(screen.getByRole("button", { name: /Trip/ }).textContent).toContain("7"));
  const browseCount = queries.length;
  await fireEvent.click(screen.getByRole("button", { name: "Unstar album" }));
  await waitFor(() => expect(fetcher.mock.calls.some(([url, init]) => url.endsWith(`/photos/albums/${album.id}`) && init.method === "PUT")).toBe(true));
  await waitFor(() => expect((screen.getByRole("button", { name: "Unstar album" }) as HTMLButtonElement).disabled).toBe(false));
  expect(queries).toHaveLength(browseCount);
});

it.each(["document trash", "delayed photo trash", "restore"])("refreshes albums and both started photo views after %s", async operation => {
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  Element.prototype.scrollIntoView = vi.fn();
  const album = photoAlbum({ member_count: 1, included_count: operation === "restore" ? 0 : 1 });
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  const node = { created_at: "2026-07-28T12:00:00Z", modified_at: "2026-07-28T12:00:00Z", revision: 1, size: 0 };
  const root = { ...node, id: 1, name: "", kind: "dir", path: "/" };
  const reports = { ...node, id: 2, parent_id: 1, name: "Reports", kind: "dir", path: "/Reports" };
  const file = { ...node, id: 3, parent_id: 2, name: "photo-1.jpg", kind: "file", current_version_id: "11111111-1111-4111-8111-111111111111", blob_hash: "a".repeat(64), size: 74, mime_type: "image/jpeg", path: "/Reports/photo-1.jpg" };
  const otherAlbum = photoAlbum({ id: "22222222-2222-4222-8222-000000000068", name: "Other" });
  let finishTrash!: () => void;
  let trashed = operation === "restore";
  const reads = { albums: 0, album: 0, library: 0 };
  vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => {
    const json = (value: unknown) => new Response(JSON.stringify(value), { headers: { "Content-Type": "application/json" } });
    if (url.endsWith("/photos/albums")) { reads.albums++; return json([{ ...album, included_count: trashed ? 0 : 1 }, otherAlbum]); }
    if (url.endsWith("/photos/assets/query")) { reads[JSON.parse(init!.body as string).query.filters?.set_ids ? "album" : "library"]++; return json({ items: trashed ? [] : [photo(1)], total: trashed ? 0 : 1 }); }
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1/children?limit=1000&offset=0") return json({ directory: root, items: [reports], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/nodes/2/children?limit=1000&offset=0") return json({ directory: reports, items: trashed ? [] : [file], total: trashed ? 0 : 1, limit: 1000, offset: 0 });
    if (url.endsWith("/trash") && init?.method === "POST") {
      if (operation === "delayed photo trash") await new Promise<void>(resolve => finishTrash = resolve);
      trashed = true;
      return json(url.includes("/photos/assets/") ? { id: "photo-1", revision: 2 } : { ...file, revision: 2, trashed_at: "2026-07-28T12:01:00Z" });
    }
    if (url.startsWith("/api/v1/trash?")) return json({ items: trashed ? [{ ...file, trashed_at: "2026-07-28T12:01:00Z" }] : [], total: trashed ? 1 : 0, limit: 1000, offset: 0 });
    if (url.endsWith("/nodes/3/restore")) { trashed = false; return json({ ...file, revision: 2 }); }
    if (url === "/api/v1/nodes/3") return json(file);
    if (url.startsWith("/api/v1/audit/status")) return json({ enabled: false, scopes: [] });
    return json({ items: [], total: 0, limit: 1000, offset: 0 });
  }));
  render(App);
  await screen.findByText(operation === "restore" ? "Your photo library is empty" : "1 photos · 1 loaded");
  await fireEvent.click(screen.getByRole("button", { name: /^Trip/ }));
  await screen.findByText(operation === "restore" ? "This album is empty" : "1 photos · 1 loaded");
  const before = { ...reads };
  if (operation === "document trash") {
    await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
    await fireEvent.dblClick(await screen.findByRole("cell", { name: "Reports" }));
    await screen.findByRole("cell", { name: "photo-1.jpg" });
    await fireEvent.click(screen.getByRole("button", { name: "Move to trash" }));
    await fireEvent.click(within(screen.getByRole("dialog", { name: "Move photo-1.jpg to trash" })).getByRole("button", { name: "Move to trash" }));
  } else if (operation === "restore") {
    await fireEvent.click(screen.getByRole("button", { name: "Recoverable trash" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Restore" }));
    await fireEvent.click(within(screen.getByRole("dialog", { name: "Restore photo-1.jpg from trash" })).getByRole("button", { name: "Restore" }));
  } else {
    await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
    await fireEvent.click(screen.getByRole("button", { name: "Move to trash" }));
    await fireEvent.click(within(screen.getByRole("dialog", { name: "Move selected photos to trash" })).getByRole("button", { name: "Move to trash" }));
  }
  if (operation === "delayed photo trash") {
    await waitFor(() => expect(finishTrash).toBeTypeOf("function"));
    await fireEvent.click(screen.getByRole("button", { name: /^Other/ }));
    await screen.findByRole("heading", { name: "Other" });
    await screen.findByText("1 photos · 1 loaded");
    Object.assign(before, reads);
    finishTrash();
  }
  await waitFor(() => { for (const key of ["albums", "album", "library"] as const) expect(reads[key]).toBeGreaterThan(before[key]); });
  if (operation === "document trash") await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  await screen.findByText(operation === "restore" ? "1 photos · 1 loaded" : "This album is empty");
});

it.each([false, true])("retains failed selection and browsing position through partial visibility changes, hidden=%s", async hidden => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
  const revoke = vi.fn();
  vi.stubGlobal("URL", class extends URL { static createObjectURL() { return "blob:synthetic"; } static revokeObjectURL = revoke; });
  const stored = storage();
  const writes = vi.spyOn(Photos.prototype, "setHidden");
  const polls = new Map<number, () => void>();
  let nextPoll = 0;
  const interval = globalThis.setInterval, clear = globalThis.clearInterval;
  vi.spyOn(globalThis, "setInterval").mockImplementation((callback, delay, ...args) => {
    if (delay !== 2000) return interval(callback, delay, ...args);
    const id = --nextPoll;
    polls.set(id, callback as () => void);
    return id as unknown as ReturnType<typeof setInterval>;
  });
  vi.spyOn(globalThis, "clearInterval").mockImplementation(id => { if (!polls.delete(Number(id))) clear(id); });
  const poll = async () => { for (const callback of polls.values()) callback(); await new Promise(resolve => setTimeout(resolve, 0)); };
  let changed = false;
  const reads = [0, 0];
  let finish!: (response: Response) => void;
  const items = Array.from({ length: 60 }, (_, index) => ({ ...photo(index + 1, new Date(Date.UTC(2025, 5, 1, 12, 60 - index)).toISOString()), previews: { grid: { state: "ready", generation_id: "synthetic" } } }));
  vi.stubGlobal("fetch", vi.fn(async (url: string, options?: RequestInit) => {
    if (url.endsWith("/photos/hidden")) return Response.json({ configured: true, expires_at: "2099-01-01T00:00:00Z", expires_in: 300, change_id: changed ? "second" : "first" });
    if (url.endsWith("/assets/query")) {
      const scope = !!JSON.parse(String(options?.body)).hidden;
      reads[Number(scope)]++;
      const rows = hidden && !scope ? changed ? [items[0]] : [] : changed ? items.slice(1) : items;
      return Response.json({ items: rows, total: rows.length });
    }
    if (url.includes("/previews/")) return new Response("synthetic-jpeg");
    if (url.endsWith(`/assets/photo-1/${hidden ? "unhide" : "hide"}`)) { changed = true; return Response.json({ id: "photo-1", revision: 2 }); }
    if (url.endsWith(`/assets/photo-2/${hidden ? "unhide" : "hide"}`)) return new Promise<Response>(resolve => finish = resolve);
    return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
  }));
  render(App);
  if (hidden) {
    await screen.findByText("Your photo library is empty");
    await fireEvent.click(screen.getByRole("button", { name: "Hidden" }));
  }
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select photo Photo 2.jpg" }));
  await screen.findByRole("img", { name: "Photo 1.jpg" });
  await fireEvent.click(screen.getByRole("combobox", { name: "Group photos: Months" }));
  await fireEvent.click(screen.getByRole("option", { name: "Capture sessions" }));
  await fireEvent.click(screen.getByRole("combobox", { name: "Grid density: Comfortable" }));
  await fireEvent.click(screen.getByRole("option", { name: "Compact" }));
  const scroll = screen.getByTestId("photo-scroll");
  scroll.scrollTop = 100;
  await fireEvent.scroll(scroll);
  const cacheName = stored.open.mock.calls[0][0];
  await fireEvent.click(screen.getByRole("button", { name: "Actions for Photo 1.jpg" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: hidden ? "Unhide" : "Hide" }));
  await waitFor(() => expect(finish).toBeDefined());
  expect(screen.queryByTestId("photo-scroll")).toBeNull();
  await waitFor(() => expect(stored.data.has(cacheName)).toBe(false));
  expect(revoke).toHaveBeenCalled();
  await poll();
  expect(reads[Number(hidden)]).toBe(1);
  expect(reads[Number(!hidden)]).toBe(hidden ? 1 : 0);
  expect(screen.queryByTestId("photo-scroll")).toBeNull();
  finish(Response.json({ detail: "Stale revision" }, { status: 412 }));
  await writes.mock.results[0].value;
  await poll();
  await screen.findByText("1 photo failed: Stale revision", {}, { timeout: 5000 });
  await waitFor(() => expect(screen.getByTestId("photo-scroll").scrollTop).toBe(100), { timeout: 5000 });
  expect(screen.getByRole("combobox", { name: "Group photos: Capture sessions" })).toBeTruthy();
  expect(screen.getByRole("combobox", { name: "Grid density: Compact" })).toBeTruthy();
  expect(screen.getByText("59 photos · 59 loaded")).toBeTruthy();
  expect((screen.getByRole("checkbox", { name: "Select photo Photo 2.jpg" }) as HTMLInputElement).checked).toBe(true);
  expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull();
  expect(screen.getByText("1 selected photo")).toBeTruthy();
  if (hidden) {
    expect(reads[0]).toBe(1);
    await fireEvent.click(screen.getByRole("button", { name: "Library" }));
    await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
    expect(screen.queryByRole("checkbox", { name: "Select photo Photo 2.jpg" })).toBeNull();
    expect(reads[0]).toBe(2);
  }
});

it.each([false, true])("keeps concealed rows hidden when Trash restore interrupts revalidation, failed=%s", async failed => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  let poll!: () => void;
  const interval = globalThis.setInterval;
  vi.spyOn(globalThis, "setInterval").mockImplementation((callback, delay, ...args) => {
    if (delay === 2000) { poll = callback as () => void; return 0 as unknown as ReturnType<typeof setInterval>; }
    return interval(callback, delay, ...args);
  });
  const refresh = vi.spyOn(Photos.prototype, "refresh");
  let changed = false;
  let reads = 0;
  let finishConceal!: (response: Response) => void;
  let finishRestore!: (response: Response) => void;
  let concealSignal!: AbortSignal;
  const node = { id: 2, parent_id: 1, name: "Restored.jpg", kind: "file", revision: 1, size: 10, path: "/Restored.jpg", trashed_at: "2026-07-28T12:01:00Z" };
  vi.stubGlobal("fetch", vi.fn(async (url: string, options?: RequestInit) => {
    if (url.endsWith("/photos/hidden")) return Response.json({ configured: true, change_id: changed ? "second" : "first" });
    if (url.endsWith("/assets/query")) {
      reads++;
      if (reads === 2) { concealSignal = options!.signal!; return new Promise<Response>(resolve => finishConceal = resolve); }
      if (reads === 3) return new Promise<Response>(resolve => finishRestore = resolve);
      return Response.json({ items: changed ? [] : [photo(1)], total: changed ? 0 : 1 });
    }
    if (url.startsWith("/api/v1/trash?")) return Response.json({ items: [node], total: 1, limit: 1000, offset: 0 });
    if (url.endsWith("/nodes/2/restore")) return Response.json({ ...node, revision: 2, trashed_at: null });
    return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
  }));
  render(App);
  await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
  changed = true;
  poll();
  await waitFor(() => expect(finishConceal).toBeDefined());
  await fireEvent.click(screen.getByRole("button", { name: "Recoverable trash" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Restore" }));
  await fireEvent.click(within(screen.getByRole("dialog", { name: "Restore Restored.jpg from trash" })).getByRole("button", { name: "Restore" }));
  await waitFor(() => expect(finishRestore).toBeDefined());
  expect(concealSignal.aborted).toBe(true);
  finishConceal(Response.json({ items: [], total: 0 }));
  await refresh.mock.results[0].value;
  expect(screen.queryByTestId("photo-scroll")).toBeNull();
  expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull();
  finishRestore(failed ? Response.json({ detail: "Replacement failed" }, { status: 503 }) : Response.json({ items: [], total: 0 }));
  await refresh.mock.results[1].value;
  expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull();
  if (failed) expect(screen.queryByTestId("photo-scroll")).toBeNull();
  poll();
  await screen.findByText("Your photo library is empty");
});

async function failStatusReads(reads: () => number, count: number) {
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  for (const target = reads() + count; reads() < target;) {
    const before = reads();
    await fireEvent(document, new Event("visibilitychange"));
    await waitFor(() => expect(reads()).toBeGreaterThan(before), { timeout: 5000 });
    await new Promise(resolve => setTimeout(resolve, 0));
  }
}

it.each(["change", "failure", "background", "delayed page", "unrelated mutation"])("validates retained photo state after %s", async kind => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const revoke = vi.fn();
  vi.stubGlobal("URL", class extends URL { static createObjectURL() { return "blob:synthetic"; } static revokeObjectURL = revoke; });
  const stored = storage();
  let changed = false;
  let statusReads = 0;
  let delay = false;
  let accessFailed = true;
  let finish!: (response: Response) => void;
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    if (url.endsWith("/photos/hidden")) {
      statusReads++;
      if (changed && kind === "failure" && accessFailed) throw new Error("Status unavailable");
      return Response.json({ configured: false, change_id: changed && kind !== "unrelated mutation" ? "second" : "first" });
    }
    if (url.endsWith("/assets/query")) {
      if (delay) { delay = false; return new Promise<Response>(resolve => finish = resolve); }
      return Response.json({ items: changed && kind !== "failure" ? [] : [{ ...photo(1), previews: { grid: { state: "ready", generation_id: "synthetic" } } }], total: changed ? 0 : 1 });
    }
    if (url.includes("/previews/")) return new Response("synthetic-jpeg");
    return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
  }));
  render(App);
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  await screen.findByRole("img", { name: "Photo 1.jpg" });
  const cacheName = stored.open.mock.calls[0][0];
  await waitFor(() => expect(stored.data.get(cacheName)?.size).toBe(1));
  if (kind === "delayed page") {
    delay = true;
    await fireEvent.click(screen.getByRole("button", { name: "Refresh previews" }));
    await waitFor(() => expect(finish).toBeDefined());
  }
  changed = true;
  if (kind === "unrelated mutation") {
    const scroll = screen.getByTestId("photo-scroll");
    scroll.scrollTop = 100;
    await fireEvent.scroll(scroll);
    await waitFor(() => expect(statusReads).toBeGreaterThan(1), { timeout: 5000 });
    expect(screen.getByTestId("photo-scroll")).toBe(scroll);
    expect(scroll.scrollTop).toBe(100);
    expect(screen.getByText("1 selected photo")).toBeTruthy();
    expect(stored.data.has(cacheName)).toBe(true);
    expect(revoke).not.toHaveBeenCalled();
    return;
  }
  if (kind === "background") {
    vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
    await fireEvent(document, new Event("visibilitychange"));
  }
  if (kind === "failure") {
    const reads = statusReads;
    await waitFor(() => expect(statusReads).toBeGreaterThan(reads), { timeout: 5000 });
    await new Promise(resolve => setTimeout(resolve, 0));
    expect(screen.getByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeTruthy();
    expect(screen.queryByText("Status unavailable")).toBeNull();
    await failStatusReads(() => statusReads, 2);
  }
  await waitFor(() => expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull(), { timeout: 5000 });
  expect(screen.queryByText("1 selected photo")).toBeNull();
  await waitFor(() => expect(stored.data.has(cacheName)).toBe(false));
  expect(revoke).toHaveBeenCalledWith("blob:synthetic");
  if (kind === "failure") {
    await screen.findByText("Status unavailable");
    accessFailed = false;
    await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
    expect(screen.getByText("1 selected photo")).toBeTruthy();
  }
  if (kind === "delayed page") {
    finish(Response.json({ items: [photo(1)], total: 1 }));
    await screen.findByText("Your photo library is empty");
    expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull();
  }
});

it("refreshes a remounted Hidden grid after a delayed unhide", async () => {
  history.replaceState(null, "", "/photos/hidden#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  storage();
  let changed = false;
  let finish!: (response: Response) => void;
  vi.stubGlobal("fetch", vi.fn(async (url: string, options?: RequestInit) => {
    if (url.endsWith("/photos/hidden")) return Response.json({ configured: true, expires_at: "2099-01-01T00:00:00Z", expires_in: 300 });
    if (url.includes("/assets/query")) {
      const hidden = JSON.parse(String(options?.body)).hidden;
      const items = hidden && !changed ? [photo(1)] : [];
      return Response.json({ items, total: items.length });
    }
    if (url.endsWith("/assets/photo-1/unhide")) return new Promise<Response>(resolve => finish = resolve);
    return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
  }));
  render(App);
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: "Actions for Photo 1.jpg" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: "Unhide" }));
  await waitFor(() => expect(finish).toBeDefined());
  await fireEvent.click(screen.getByRole("button", { name: "Library" }));
  await waitFor(() => expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull());
  await fireEvent.click(screen.getByRole("button", { name: "Hidden" }));
  await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
  changed = true;
  finish(Response.json({ id: "photo-1", revision: 2 }));
  await screen.findByText("No hidden photos");
  expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull();
});

it("clears previews on sidebar and history departure while retaining eligible selection", async () => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  vi.stubGlobal("URL", class extends URL { static createObjectURL() { return "blob:synthetic"; } static revokeObjectURL() {} });
  const stored = storage();
  let changed = false;
  const fetcher = vi.fn(async (url: string) => {
    if (url.endsWith("/photos/albums")) return new Response("[]");
    if (url.endsWith("/photos/hidden")) return Response.json({configured:true,change_id:changed ? "second" : "first"});
    if (url.includes("/photos/assets/query")) return new Response(JSON.stringify({ items: (changed ? [] : [photo(1)]).map(item => ({ ...item, previews: { ...item.previews, grid: { state: "ready", generation_id: "synthetic" } } })), total: changed ? 0 : 1 }));
    if (url.includes("/previews/")) return new Response("synthetic-jpeg");
    if (url.includes("/nodes/1")) return new Response(JSON.stringify({ id: 1, kind: "dir", name: "", revision: 1, path: "/" }));
    return new Response(JSON.stringify({ items: [], nodes: [], tags: [], profiles: [] }));
  });
  vi.stubGlobal("fetch", fetcher);
  render(App);
  await screen.findByRole("main", { name: "Photo library" });
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  const cacheName = stored.open.mock.calls[0][0];
  await waitFor(() => expect(stored.data.get(cacheName)?.size).toBe(1));
  const listings = () => fetcher.mock.calls.filter(([url]) => url.includes("/photos/assets/query")).length;
  const previews = () => fetcher.mock.calls.filter(([url]) => url.includes("/previews/")).length;
  expect(listings()).toBe(1);
  expect(previews()).toBe(1);
  const historyLength = history.length;
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  expect(history.length).toBe(historyLength);
  await fireEvent.click(screen.getByRole("button", { name: "Library" }));
  expect(history.length).toBe(historyLength);
  await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
  expect(location.pathname).toBe("/");
  expect(screen.queryByRole("main", { name: "Photo library" })).toBeNull();
  await waitFor(() => expect(stored.data.has(cacheName)).toBe(false));
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  expect(location.pathname).toBe("/photos");
  await screen.findByText("1 selected photo");
  await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
  expect(listings()).toBe(2);
  await waitFor(() => expect(previews()).toBe(2));
  const returnedCache = stored.open.mock.calls.at(-1)![0];
  history.replaceState(null, "", "/");
  await fireEvent(window, new PopStateEvent("popstate"));
  await waitFor(() => expect(screen.queryByRole("main", { name: "Photo library" })).toBeNull());
  await waitFor(() => expect(stored.data.has(returnedCache)).toBe(false));
  changed = true;
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  await screen.findByText("Your photo library is empty");
  expect(screen.queryByRole("checkbox", {name:"Select photo Photo 1.jpg"})).toBeNull();
});

it.each([false, true])("Hidden trash invalidates Documents and Trash restore refreshes only an unlocked grid, locked=%s", async locked => {
  history.replaceState(null, "", "/#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const root = { id: 1, name: "", kind: "dir", size: 0, revision: 1, path: "/", created_at: "2026-07-28T12:00:00Z", modified_at: "2026-07-28T12:00:00Z" };
  const node = { ...root, id: 2, parent_id: 1, name: "Photo 1.jpg", kind: "file", mime_type: "image/jpeg", current_version_id: "version-1", blob_hash: "a".repeat(64), photo_asset_id: "photo-1", photo_file_count: 1 };
  let trashed = false;
  let unlocked = true;
  let rootReads = 0;
  let hiddenReads = 0;
  const fetcher = vi.fn(async (url: string, options?: RequestInit) => {
    if (url.endsWith("/photos/hidden/lock")) { unlocked = false; return Response.json({}); }
    if (url.endsWith("/photos/hidden")) return Response.json({ configured: true, ...(unlocked ? { expires_at: "2099-01-01T00:00:00Z", expires_in: 300 } : {}) });
    if (url.includes("/assets/query")) {
      const hidden = JSON.parse(String(options?.body)).hidden;
      if (hidden) hiddenReads++;
      return Response.json({ items: hidden && !trashed ? [photo(1)] : [], total: hidden && !trashed ? 1 : 0 });
    }
    if (url.endsWith("/assets/photo-1/trash")) { trashed = true; return Response.json({ id: "photo-1", revision: 2 }); }
    if (url.endsWith("/nodes/2/restore")) { trashed = false; return Response.json({ ...node, revision: 3, path: "/Photo 1.jpg" }); }
    if (url.startsWith("/api/v1/trash?")) return Response.json({ items: trashed ? [{ ...node, revision: 2, trashed_at: "2026-07-28T12:01:00Z" }] : [], total: trashed ? 1 : 0, limit: 1000, offset: 0 });
    if (url.startsWith("/api/v1/path?")) return Response.json(root);
    if (url === "/api/v1/nodes/1") return Response.json(root);
    if (url.startsWith("/api/v1/nodes/1/children")) { rootReads++; return Response.json({ directory: root, items: trashed ? [] : [node], total: trashed ? 0 : 1, limit: 1000, offset: 0 }); }
    return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
  });
  vi.stubGlobal("fetch", fetcher);
  render(App);
  await screen.findByRole("cell", { name: "Photo 1.jpg" });
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  await fireEvent.click(screen.getByRole("button", { name: "Hidden" }));
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: "Move to trash" }));
  await fireEvent.click(within(screen.getByRole("dialog", { name: "Move selected photos to trash" })).getByRole("button", { name: "Move to trash" }));
  await waitFor(() => expect(rootReads).toBe(2));
  await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
  await waitFor(() => expect(screen.queryByRole("cell", { name: "Photo 1.jpg" })).toBeNull());
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  await fireEvent.click(screen.getByRole("button", { name: "Hidden" }));
  await screen.findByText("No hidden photos");
  if (locked) { await fireEvent.click(screen.getByRole("button", { name: "Lock" })); await screen.findByRole("button", { name: "Unlock" }); }
  const before = hiddenReads;
  await fireEvent.click(screen.getByRole("button", { name: "Recoverable trash" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Restore" }));
  await fireEvent.click(within(screen.getByRole("dialog", { name: "Restore Photo 1.jpg from trash" })).getByRole("button", { name: "Restore" }));
  await screen.findByText("Restored Photo 1.jpg");
  if (locked) {
    expect(screen.getByRole("button", { name: "Unlock" })).toBeTruthy();
    expect(hiddenReads).toBe(before);
  } else {
    await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
    expect(hiddenReads).toBe(before + 1);
  }
  await fireEvent.click(screen.getByRole("button", { name: "Close recoverable trash" }));
  await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
  await screen.findByRole("cell", { name: "Photo 1.jpg" });
});

it.each([false, true])("keeps direct links and history scoped and conceals a mounted viewer when access changes, hidden=%s", async hidden => {
  const path = hidden ? "/photos/hidden" : "/photos";
  history.replaceState(null, "", `${path}#web_session=synthetic&web_upload_secret=proof&photo=photo-1`);
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  const revoke = vi.fn();
  vi.stubGlobal("URL", class extends URL { static createObjectURL() { return "blob:hidden-viewer"; } static revokeObjectURL = revoke; });
  Object.defineProperty(HTMLImageElement.prototype, "decode", { configurable: true, value: vi.fn(async () => {}) });
  storage();
  let unlocked = true;
  let changeID = "first";
  let unavailable = false;
  let statusReads = 0;
  let listed = true;
  let holdRefresh = false;
  let finishRefresh!: (response: Response) => void;
  const item = photo(1); item.previews.grid = { state: "ready", generation_id: "grid" }; item.previews.fit = { state: "ready", generation_id: "fit" };
  vi.stubGlobal("fetch", vi.fn(async (url: string, options?: RequestInit) => {
    if (url.endsWith("/photos/hidden")) { statusReads++; if (unavailable) throw new Error("Status unavailable"); return Response.json({ configured: true, change_id: changeID, ...(unlocked ? { expires_at: "2099-01-01T00:00:00Z", expires_in: 300 } : {}) }); }
    if (url.endsWith("/hidden/lock")) { unlocked = false; return Response.json({ configured: true }); }
    if (url.endsWith("/assets/query")) {
      expect(JSON.parse(String(options?.body)).hidden).toBe(hidden);
      if (holdRefresh) { holdRefresh = false; return new Promise<Response>(resolve => finishRefresh = resolve); }
      return Response.json(listed ? { items: [item], total: 1 } : { items: [], total: 0 });
    }
    if (url.includes("/previews/")) return new Response("synthetic-jpeg");
    return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
  }));
  render(App);
  await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
  await screen.findByText("Fit preview");
  history.pushState(null, "", path);
  await fireEvent(window, new PopStateEvent("popstate"));
  await waitFor(() => expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull());
  await fireEvent.click(screen.getByRole("button", { name: "Open Photo 1.jpg" }));
  expect(location.pathname).toBe(path);
  await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
  history.back();
  await waitFor(() => expect(location.hash).toBe(""));
  await waitFor(() => expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull());
  history.forward();
  await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
  const before = revoke.mock.calls.length;
  if (hidden) {
    await fireEvent.click(screen.getByRole("button", { name: "Lock" }));
    await screen.findByRole("button", { name: "Unlock" });
  } else {
    unavailable = true;
    await failStatusReads(() => statusReads, 3);
    await screen.findByText("Status unavailable");
    expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull();
    expect(location.hash).toBe("#photo=photo-1");
    unavailable = false;
    holdRefresh = true;
    await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(finishRefresh).toBeDefined());
    expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull();
    finishRefresh(Response.json({ items: [item], total: 1 }));
    await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
    const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
    await fireEvent(document, new Event("visibilitychange"));
    expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull();
    expect(location.hash).toBe("#photo=photo-1");
    visibility.mockReturnValue("visible");
    await fireEvent(document, new Event("visibilitychange"));
    await screen.findByRole("dialog", { name: "Photo viewer, Photo 1.jpg" });
    const queries = () => vi.mocked(fetch).mock.calls.filter(([url]) => String(url).endsWith("/assets/query")).length;
    let refreshed = queries();
    changeID = "second";
    await fireEvent(document, new Event("visibilitychange"));
    await waitFor(() => expect(queries()).toBeGreaterThan(refreshed), { timeout: 5000 });
    await screen.findByText("Fit preview");
    expect(screen.getByRole("dialog", { name: "Photo viewer, Photo 1.jpg" })).toBeTruthy();
    expect(location.hash).toBe("#photo=photo-1");
    const shown = revoke.mock.calls.length;
    refreshed = queries();
    listed = false;
    changeID = "third";
    await fireEvent(document, new Event("visibilitychange"));
    await waitFor(() => expect(queries()).toBeGreaterThan(refreshed), { timeout: 5000 });
    await screen.findByText("This photo is no longer in the refreshed result.");
    expect(screen.queryByText("Fit preview")).toBeNull();
    expect(revoke.mock.calls.length).toBeGreaterThan(shown);
    return;
  }
  expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull();
  await waitFor(() => expect(location.hash).toBe(""));
  expect(history.state?.photoReturn).toBeUndefined();
  expect(revoke.mock.calls.length).toBeGreaterThan(before);
});

it("opens and closes the loupe inside an album without leaving it", async () => {
  const album = photoAlbum({ id: "22222222-2222-4222-8222-0000000000aa", included_count: 1, member_count: 1 });
  history.replaceState(null, "", `/photos/albums/${album.id}#web_session=synthetic&web_upload_secret=proof`);
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  Element.prototype.scrollIntoView = vi.fn();
  const queries: { filters?: { set_ids?: string[] } }[] = [];
  vi.stubGlobal("fetch", vi.fn(async (url: string, init: RequestInit) => {
    if (url.endsWith("/photos/albums")) return new Response(JSON.stringify([album]));
    if (url.endsWith("/photos/assets/query")) { queries.push(JSON.parse(init.body as string).query); return new Response(JSON.stringify({ items: [photo(1)], total: 1 })); }
    if (url.includes("/nodes/1")) return new Response(JSON.stringify({ id: 1, kind: "dir", name: "", revision: 1, path: "/" }));
    return new Response(JSON.stringify({ items: [], nodes: [], tags: [], profiles: [] }));
  }));
  render(App);
  await fireEvent.keyDown(await screen.findByRole("button", { name: "Select Photo 1.jpg" }), { key: "Enter" });
  await screen.findByRole("dialog", { name: /Photo viewer/ });
  expect(location.pathname).toBe(`/photos/albums/${album.id}`);
  expect(new URLSearchParams(location.hash.slice(1)).get("photo")).toBe("photo-1");
  expect(screen.getByRole("main", { name: "Photo album" })).toBeTruthy();
  expect(queries[0]?.filters?.set_ids).toEqual([album.id]);
  await fireEvent.click(screen.getByRole("button", { name: "Close photo viewer" }));
  await waitFor(() => expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull());
  await waitFor(() => expect(location.hash).not.toContain("photo="));
  expect(location.pathname).toBe(`/photos/albums/${album.id}`);
  await fireEvent.keyDown(await screen.findByRole("button", { name: "Select Photo 1.jpg" }), { key: "Enter" });
  await screen.findByRole("dialog", { name: /Photo viewer/ });
  await fireEvent.click(screen.getByRole("button", { name: "Library" }));
  await waitFor(() => expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull());
  expect(location.pathname).toBe("/photos");
  expect(location.hash).not.toContain("photo=");
});

it.each([undefined, "first"])("conceals an open album when hidden photos change, then rereads its photos and album counts, initial change=%s", async initial => {
  const album = photoAlbum({ id: "22222222-2222-4222-8222-0000000000ab", included_count: 1, member_count: 1 });
  history.replaceState(null, "", `/photos/albums/${album.id}#web_session=synthetic&web_upload_secret=proof`);
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  storage();
  let changed = false;
  let albumReads = 0;
  vi.stubGlobal("fetch", vi.fn(async (url: string, init: RequestInit) => {
    if (url.endsWith("/photos/hidden")) return Response.json({ configured: true, change_id: changed ? "second" : initial });
    if (url.endsWith("/photos/albums")) { albumReads++; return Response.json([{ ...album, included_count: changed ? 0 : 1 }]); }
    if (url.endsWith("/photos/assets/query")) {
      expect(JSON.parse(init.body as string).query.filters.set_ids).toEqual([album.id]);
      return Response.json({ items: changed ? [] : [photo(1)], total: changed ? 0 : 1 });
    }
    return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
  }));
  render(App);
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  expect(screen.getByRole("main", { name: "Photo album" })).toBeTruthy();
  const reads = albumReads;
  changed = true;
  await waitFor(() => expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull(), { timeout: 5000 });
  await screen.findByText("This album is empty", undefined, { timeout: 5000 });
  expect(albumReads).toBeGreaterThan(reads);
  await waitFor(() => expect(screen.getByRole("button", { name: /^Trip/ }).textContent).toContain("0"));
  expect(location.pathname).toBe(`/photos/albums/${album.id}`);
});
