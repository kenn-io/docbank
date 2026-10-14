import { afterEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { photo, mapPhoto, photoAlbum, albumResponse, storage } from "./photo-test-fixtures.js";
import App from "./App.svelte";
import { Photos } from "./photos.svelte.js";
import type { Query } from "./query.js";

// Answers background filter-count requests so tests that predate counts see only their own reads.
const stubPhotoFetch = (fetcher: (url: string, init: RequestInit) => unknown) => vi.stubGlobal("fetch", (url: string, init: RequestInit) => url.endsWith("/photos/assets/query") && JSON.parse(String(init?.body ?? "{}")).facets?.length ? Promise.resolve(Response.json({ items: [], total: 0, facets: [] })) : fetcher(url, init));

const map = vi.hoisted(() => ({props:undefined as {items:ReturnType<typeof mapPhoto>[]} | undefined}));
vi.mock("./PhotoMap.svelte",()=>({default:(_anchor:unknown,props:NonNullable<typeof map.props>)=>{map.props=props;return {};}}));

afterEach(() => { cleanup(); map.props=undefined; document.querySelectorAll('meta[name="docbank-map-tiles"]').forEach(meta=>meta.remove()); history.replaceState(null, "", "/"); vi.unstubAllGlobals(); vi.restoreAllMocks(); localStorage.clear(); new Photos("scoped", vi.fn()).setDensity("comfortable"); });

it("removes a delayed direct-link photo fragment on tab switch without history.back", async () => {
  history.replaceState(null, "", "/photos/hidden#web_session=synthetic&web_upload_secret=proof&photo=photo-1");
  storage();
  let finish!: (response: Response) => void;
  stubPhotoFetch(vi.fn(async (url: string) => {
    if (url.endsWith("/photos/hidden")) return new Promise<Response>(resolve => finish = resolve);
    return Response.json({ items: [], total: 0 });
  }));
  const back = vi.spyOn(history, "back");
  render(App);
  await waitFor(() => expect(finish).toBeTypeOf("function"));
  expect(history.state?.photoReturn).toBeUndefined();
  expect(location.hash).toBe("#photo=photo-1");
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
  await fireEvent(document, new Event("visibilitychange"));
  expect(back).not.toHaveBeenCalled();
  expect(location.hash).toBe("");
  finish(Response.json({ configured: true }));
});

it("locks once when sign-in completes after the Photos tab was hidden", async () => {
  history.replaceState(null, "", "/photos/hidden");
  storage();
  let finish!: (response: Response) => void;
  let locks = 0;
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    if (url.endsWith("/web-auth")) return Response.json({ enabled: true });
    if (url.endsWith("/login")) return new Promise<Response>(resolve => finish = resolve);
    if (url.endsWith("/photos/hidden/lock")) locks++;
    return Response.json({ configured: true, items: [], total: 0 });
  }));
  render(App);
  await fireEvent.input(await screen.findByLabelText("API key"), { target: { value: "synthetic-key" } });
  await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  await waitFor(() => expect(finish).toBeTypeOf("function"));
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
  await fireEvent(document, new Event("visibilitychange"));
  finish(Response.json({ enabled: true, session: { token: "synthetic", upload_secret: "proof" } }));
  await screen.findByRole("button", { name: "Sign out" });
  await waitFor(() => expect(locks).toBe(1));
  await fireEvent(window, new Event("pagehide"));
  await waitFor(() => expect(locks).toBe(1));
});

it.each(["setup", "unlock", "change", "disable"])("preserves submitted %s passcodes when navigating to Library behind a held leave lock", async kind => {
  history.replaceState(null, "", "/photos/hidden#web_session=synthetic&web_upload_secret=proof");
  storage();
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  let finish!: () => void;
  const requests: { kind: string; body: unknown }[] = [];
  stubPhotoFetch(vi.fn(async (url: string, init: RequestInit) => {
    if (url.endsWith("/photos/hidden/lock")) { await new Promise<void>(resolve => finish = resolve); return Response.json({}); }
    if (init?.method === "POST" && /\/(setup|unlock|change|disable)$/.test(url)) requests.push({ kind: url.split("/").at(-1)!, body: JSON.parse(String(init.body)) });
    return Response.json({ configured: kind !== "setup", items: [], total: 0 });
  }));
  render(App);
  await screen.findByRole("button", { name: kind === "setup" ? "Set passcode" : "Unlock" });
  const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
  await fireEvent(document, new Event("visibilitychange"));
  await waitFor(() => expect(finish).toBeTypeOf("function"));
  visibility.mockReturnValue("visible");
  await fireEvent(document, new Event("visibilitychange"));
  if (kind === "change" || kind === "disable") {
    await fireEvent.click(screen.getByText("Manage passcode"));
    await fireEvent.input(screen.getByLabelText("Current passcode", { exact: true }), { target: { value: "submitted-current" } });
    if (kind === "change") await fireEvent.input(screen.getByLabelText("New passcode", { exact: true }), { target: { value: "submitted-new" } });
  } else await fireEvent.input(screen.getByLabelText("Passcode", { exact: true }), { target: { value: "submitted-passcode" } });
  await fireEvent.click(screen.getByRole("button", { name: kind === "setup" ? "Set passcode" : kind === "unlock" ? "Unlock" : kind === "change" ? "Change passcode" : "Disable Hidden" }));
  await fireEvent.click(screen.getByRole("button", { name: "Library" }));
  expect(location.pathname).toBe("/photos");
  expect(requests).toEqual([]);
  finish();
  await waitFor(() => expect(requests).toHaveLength(kind === "setup" ? 2 : 1));
  expect(requests).toEqual(kind === "setup" ? [{ kind: "setup", body: { passcode: "submitted-passcode" } }, { kind: "unlock", body: { passcode: "submitted-passcode" } }] : [{ kind, body: kind === "change" ? { passcode: "submitted-current", new_passcode: "submitted-new" } : { passcode: kind === "disable" ? "submitted-current" : "submitted-passcode" } }]);
});

it("renders no hidden photo buttons after a failed leave lock and return", async () => {
  history.replaceState(null, "", "/photos/hidden#web_session=synthetic&web_upload_secret=proof");
  storage();
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  let locks = 0;
  let unlocked = true;
  let finish!: () => void;
  stubPhotoFetch(vi.fn(async (url: string) => {
    if (url.endsWith("/photos/hidden/lock")) {
      if (++locks === 1) return Response.json({ detail: "Lock unavailable" }, { status: 503 });
      await new Promise<void>(resolve => finish = resolve);
      unlocked = false;
    }
    if (url.endsWith("/photos/hidden")) return Response.json({ configured: true, ...(unlocked ? { expires_at: "2099-01-01T00:00:00Z", expires_in: 300 } : {}) });
    return Response.json({ items: [photo(1)], total: 1 });
  }));
  render(App);
  await screen.findByRole("button", { name: "Open Photo 1.jpg" });
  const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
  await fireEvent(document, new Event("visibilitychange"));
  await screen.findByText("Hidden did not lock. Lock unavailable");
  visibility.mockReturnValue("visible");
  await fireEvent(document, new Event("visibilitychange"));
  expect(screen.queryByRole("button", { name: "Unlock" })).toBeNull();
  expect(screen.queryByLabelText("Passcode", { exact: true })).toBeNull();
  expect(screen.queryByRole("button", { name: "Open Photo 1.jpg" })).toBeNull();
  expect(screen.queryByRole("main", { name: "Photo library" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Retry lock" }));
  await waitFor(() => expect(finish).toBeTypeOf("function"));
  expect(screen.queryByRole("main", { name: "Photo library" })).toBeNull();
  finish();
  await waitFor(() => expect(screen.queryByText("Hidden did not lock. Lock unavailable")).toBeNull());
  await screen.findByRole("button", { name: "Unlock" });
});

it.each(["hide", "close", "Documents after Photos", "Documents only", "failed lock"])("locks Hidden when leaving %s", async mode => {
  history.replaceState(null, "", `${mode === "Documents only" ? "/" : "/photos"}#web_session=synthetic&web_upload_secret=proof`);
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  let locks = 0;
  const fetcher = vi.fn(async (url: string, init: RequestInit) => {
    if (url.endsWith("/photos/hidden/lock")) {
      locks++;
      if (mode === "failed lock" && locks === 1) return Response.json({ detail: "Lock unavailable" }, { status: 503 });
      return Response.json({ configured: true });
    }
    if (url.endsWith("/photos/hidden")) return Response.json({ configured: true });
    if (url.endsWith("/photos/assets/query")) return Response.json({ items: [photo(1)], total: 1 });
    const root = { id: 1, kind: "dir", name: "", revision: 1, path: "/" };
    if (url.includes("/children")) return Response.json({ directory: root, items: [], total: 0 });
    if (url.includes("/nodes/1") || url.startsWith("/api/v1/path")) return Response.json(root);
    return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
  });
  stubPhotoFetch(fetcher);
  render(App);
  if (mode === "Documents only") await screen.findByRole("button", { name: "Photos" });
  else await screen.findByRole("button", { name: "Select Photo 1.jpg" });
  if (mode === "Documents after Photos" || mode === "failed lock") await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
  const visibility = vi.spyOn(document, "visibilityState", "get");
  if (mode !== "close") {
    visibility.mockReturnValue("hidden");
    await fireEvent(document, new Event("visibilitychange"));
    await fireEvent(document, new Event("visibilitychange"));
  }
  await fireEvent(window, new Event("pagehide"));
  if (mode === "Documents only") { expect(locks).toBe(0); return; }
  await waitFor(() => expect(locks).toBe(1));
  expect(fetcher.mock.calls.find(([url]) => url.endsWith("/photos/hidden/lock"))?.[1]).toMatchObject({ keepalive: true });
  visibility.mockReturnValue("visible");
  await fireEvent(document, new Event("visibilitychange"));
  await fireEvent(window, new Event("pageshow"));
  if (mode === "failed lock") {
    expect(location.pathname).toBe("/");
    expect((await screen.findByText("Hidden did not lock. Lock unavailable")).getAttribute("role")).toBe("alert");
    await fireEvent.click(screen.getByRole("button", { name: "Retry lock" }));
    await waitFor(() => expect(screen.queryByText("Hidden did not lock. Lock unavailable")).toBeNull());
    expect(locks).toBe(2);
  } else {
    await fireEvent(window, new Event("pagehide"));
    await waitFor(() => expect(locks).toBe(2));
  }
});

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
  stubPhotoFetch(vi.fn(async (url: string, init: RequestInit) => {
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

it("reloads Library album counts after adding photos to an album", async () => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  Element.prototype.scrollIntoView = vi.fn();
  const album = photoAlbum();
  let included = false;
  let countRequests = 0;
  vi.stubGlobal("fetch", vi.fn(async (url: string, init: RequestInit) => {
    if (url.endsWith("/photos/albums")) return albumResponse([{ ...album, included_count: included ? 1 : 0 }]);
    if (url.endsWith("/photos/assets/query")) {
      if (!JSON.parse(init.body as string).facets?.length) return albumResponse({ items: [photo(1)], total: 1 });
      countRequests++;
      const values = included ? [{ key: album.id, label: album.name, count: 1, selected: false }] : [];
      return albumResponse({ items: [], total: 1, facets: [{ dimension: "set", available: true, reason: "", total: 1, missing: 0, other: 0, values }] });
    }
    if (url.endsWith(`/photos/albums/${album.id}/members/add`)) { included = true; return albumResponse({ ...album, revision: 2 }); }
    if (url.includes("/nodes/1")) return albumResponse({ id: 1, kind: "dir", name: "", revision: 1, path: "/" });
    return albumResponse({ items: [], nodes: [], tags: [], profiles: [] });
  }));
  render(App);
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  await waitFor(() => expect(countRequests).toBe(1));
  expect(screen.queryByRole("button", { name: `${album.name}, 1 photos` })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: /Add to album/ }));
  await fireEvent.mouseDown(await screen.findByRole("option", { name: album.name }));
  await within(screen.getByRole("group", { name: "Albums facet" })).findByRole("button", { name: `${album.name}, 1 photos` }, { timeout: 5000 });
  expect(countRequests).toBe(2);
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
  stubPhotoFetch(fetcher);
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
  stubPhotoFetch(vi.fn(async (url: string, init?: RequestInit) => {
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
  await waitFor(() => { for (const key of ["albums", "album", "library"] as (keyof typeof reads)[]) expect(reads[key]).toBeGreaterThan(before[key]); });
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
  stubPhotoFetch(vi.fn(async (url: string, options?: RequestInit) => {
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
  stubPhotoFetch(vi.fn(async (url: string, options?: RequestInit) => {
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
  stubPhotoFetch(vi.fn(async (url: string) => {
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
      const request = JSON.parse(String(options?.body));
      if (request.facets?.length) return Response.json({ items: [], total: 0, facets: [] });
      const hidden = request.hidden;
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
  const fetcher = vi.fn(async (url: string, init?: RequestInit) => {
    if (url.endsWith("/photos/albums")) return new Response("[]");
    if (url.endsWith("/photos/hidden")) return Response.json({configured:true,change_id:changed ? "second" : "first"});
    if (url.includes("/photos/assets/query")) {
      if (JSON.parse(init!.body as string).facets?.length) return new Response(JSON.stringify({ items: [], total: 1, facets: [] }));
      return new Response(JSON.stringify({ items: (changed ? [] : [photo(1)]).map(item => ({ ...item, previews: { ...item.previews, grid: { state: "ready", generation_id: "synthetic" } } })), total: changed ? 0 : 1 }));
    }
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
  const listings = () => fetcher.mock.calls.filter(([url, init]) => url.includes("/photos/assets/query") && !JSON.parse(init!.body as string).facets?.length).length;
  const counts = () => fetcher.mock.calls.filter(([url, init]) => url.includes("/photos/assets/query") && JSON.parse(init!.body as string).facets?.length).length;
  const previews = () => fetcher.mock.calls.filter(([url]) => url.includes("/previews/")).length;
  expect(listings()).toBe(1);
  await waitFor(() => expect(counts()).toBe(1));
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
      const request = JSON.parse(String(options?.body));
      if (request.facets?.length) return Response.json({ items: [], total: 0, facets: [] });
      const hidden = request.hidden;
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
    await waitFor(() => expect(location.hash).toBe(""));
    visibility.mockReturnValue("visible");
    await fireEvent(document, new Event("visibilitychange"));
    await screen.findByRole("button", { name: "Open Photo 1.jpg" });
    expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull();
    expect(location.hash).toBe("");
    await fireEvent.click(screen.getByRole("button", { name: "Open Photo 1.jpg" }));
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

it.each(["change", "return", "return before first hide", "interrupted return", "interrupted return before first hide", "direct return", "direct album return", "direct return before first hide", "direct album return before first hide"])("mounts a fresh scoped Map progressively and revalidates it after %s", async cause => {
 const album=photoAlbum();const direct=cause.startsWith("direct");
 history.replaceState(null,"",`${direct?cause.includes("album")?`/photos/map/${album.id}`:"/photos/map":"/photos"}#web_session=synthetic&web_upload_secret=proof`);storage();
 vi.stubGlobal("ResizeObserver",class {observe() {} disconnect() {}});vi.spyOn(HTMLElement.prototype,"clientWidth","get").mockReturnValue(1000);vi.spyOn(HTMLElement.prototype,"clientHeight","get").mockReturnValue(800);
 const meta=document.createElement("meta");meta.name="docbank-map-tiles";meta.content=JSON.stringify({tile_url:"https://tiles.example.org/{z}/{x}/{y}",attribution:"Example",max_zoom:19});document.head.append(meta);
 let finish!:(value:Response)=>void, changed=false, reload=false, pages=0;
 let poll!:()=>void;const interval=globalThis.setInterval;
 vi.spyOn(globalThis,"setInterval").mockImplementation((callback,delay,...args)=>{if(delay === 2000){poll=callback as ()=>void;return 0 as unknown as ReturnType<typeof setInterval>;}return interval(callback,delay,...args);});
 vi.stubGlobal("fetch",vi.fn(async(url:string,init:RequestInit)=>{
  if(url.endsWith("/photos/hidden")) return Response.json({configured:true,change_id:changed?"second":cause.includes("before first hide")?undefined:"first"});
  if(url.endsWith("/photos/albums")) return albumResponse([album]);
  if(url.endsWith("/photos/assets/query")) {
   const request=JSON.parse(String(init.body));
   if(!request.projection) return Response.json({items:[photo(1)],total:1});
   pages++;
   if(reload || request.cursor) return new Promise<Response>(resolve=>finish=resolve);
   return Response.json({items:[],map_items:[mapPhoto(1)],total:2,next_cursor:"second"});
  }
  return Response.json({items:[],nodes:[],tags:[],profiles:[]});
 }));render(App);
 if(!direct) {await screen.findByRole("button",{name:"Open Photo 1.jpg"});await fireEvent.click(screen.getByRole("button",{name:"Map"}));}
 await waitFor(()=>expect(finish).toBeTypeOf("function"));await waitFor(()=>expect(map.props?.items.map(row=>row.asset_id)).toEqual(["photo-1"]));
 finish(Response.json({items:[],map_items:[mapPhoto(2)],total:2}));
 await waitFor(()=>expect(map.props?.items).toHaveLength(2));
 const visibility=vi.spyOn(document,"visibilityState","get").mockReturnValue("visible");
 async function returnToMap() {
  visibility.mockReturnValue("hidden");await fireEvent(document,new Event("visibilitychange"));
  visibility.mockReturnValue("visible");await fireEvent(document,new Event("visibilitychange"));
 }
 const previous=finish;reload=true;
 if(cause.startsWith("return") || direct) {
  const before=pages;
  await returnToMap();
  await screen.findByRole("heading",{name:"Photos in view"});
  expect(pages).toBe(before);
  await returnToMap();
  await screen.findByRole("heading",{name:"Photos in view"});
  expect(pages).toBe(before);
  return;
 }
 if(cause === "change") {changed=true;await returnToMap();}
 else await fireEvent.click(screen.getByRole("button",{name:"Refresh previews"}));
 await waitFor(()=>expect(finish).not.toBe(previous));
 if(cause.startsWith("interrupted return")) {
  const interrupted=finish;visibility.mockReturnValue("hidden");await fireEvent(document,new Event("visibilitychange"));
  await act(()=>interrupted(Response.json({items:[],map_items:[mapPhoto(1)],total:1})));
  visibility.mockReturnValue("visible");await fireEvent(document,new Event("visibilitychange"));await waitFor(()=>{poll();expect(finish).not.toBe(interrupted);});
 }
 expect(screen.queryByRole("heading",{name:"Photos in view"})).toBeNull();finish(Response.json({detail:"Map unavailable"},{status:503}));await screen.findByText("Map unavailable");expect(screen.queryByRole("heading",{name:"Photos in view"})).toBeNull();
 const failed=finish;await fireEvent.click(screen.getByRole("button",{name:"Retry"}));await waitFor(()=>expect(finish).not.toBe(failed));finish(Response.json({items:[],map_items:[mapPhoto(2)],total:1}));
 await waitFor(()=>expect(map.props?.items.map(row=>row.asset_id)).toEqual(["photo-2"]));
});

it.each(["retry", "poll", "changed authority"])("retains staged Map pages after a transient access refresh failure, %s", async recovery => {
 history.replaceState(null,"","/photos/map#web_session=synthetic&web_upload_secret=proof");storage();
 vi.stubGlobal("ResizeObserver",class {observe() {} disconnect() {}});vi.spyOn(HTMLElement.prototype,"clientWidth","get").mockReturnValue(1000);vi.spyOn(HTMLElement.prototype,"clientHeight","get").mockReturnValue(800);
 const meta=document.createElement("meta");meta.name="docbank-map-tiles";meta.content=JSON.stringify({tile_url:"https://tiles.example.org/{z}/{x}/{y}",attribution:"Example",max_zoom:19});document.head.append(meta);
 let changeID="first", failed=false, poll!:()=>void;const cursors:(string|undefined)[]=[];const interval=globalThis.setInterval;
 vi.spyOn(globalThis,"setInterval").mockImplementation((callback,delay,...args)=>{if(delay===2000){poll=callback as ()=>void;return 0 as unknown as ReturnType<typeof setInterval>;}return interval(callback,delay,...args);});
 stubPhotoFetch(vi.fn(async(url:string,init:RequestInit)=>{
  if(url.endsWith("/photos/hidden")) return Response.json({configured:true,change_id:changeID});
  if(url.endsWith("/photos/assets/query")) {
   const request=JSON.parse(String(init.body));if(!request.projection)return Response.json({items:[],total:0});
   cursors.push(request.cursor);
   if(changeID==="first")return Response.json({items:[],map_items:[mapPhoto(1)],total:1});
   if(request.cursor==="second" && !failed){failed=true;return Response.json({detail:"Map unavailable"},{status:503});}
   return Response.json({items:[],map_items:[mapPhoto(request.cursor?3:2)],total:2,next_cursor:request.cursor?undefined:"second"});
  }
  return Response.json({items:[],nodes:[],tags:[],profiles:[]});
 }));render(App);await waitFor(()=>expect(map.props?.items.map(row=>row.asset_id)).toEqual(["photo-1"]));
 changeID="second";await act(()=>poll());await screen.findByText("Map unavailable");
 expect(screen.queryByRole("heading",{name:"Photos in view"})).toBeNull();
 expect(cursors).toEqual([undefined,undefined,"second"]);
 if(recovery==="changed authority")changeID="third";
 if(recovery==="retry")await fireEvent.click(screen.getByRole("button",{name:"Retry"}));else await act(()=>poll());
 await screen.findByRole("heading",{name:"Photos in view"});
 expect(map.props?.items.map(row=>row.asset_id)).toEqual(["photo-2","photo-3"]);
 expect(cursors).toEqual(recovery==="changed authority"?[undefined,undefined,"second",undefined,"second"]:[undefined,undefined,"second","second"]);
});

it("re-pages Library on return with an unchanged hidden change ID", async () => {
 history.replaceState(null,"","/photos#web_session=synthetic&web_upload_secret=proof");storage();
 vi.stubGlobal("ResizeObserver",class {observe() {} disconnect() {}});
 vi.spyOn(HTMLElement.prototype,"clientWidth","get").mockReturnValue(1000);
 vi.spyOn(HTMLElement.prototype,"clientHeight","get").mockReturnValue(800);
 let pages=0, finish!:(value:Response)=>void;
 const interval=globalThis.setInterval;
 vi.spyOn(globalThis,"setInterval").mockImplementation((callback,delay,...args)=>delay === 2000?0 as unknown as ReturnType<typeof setInterval>:interval(callback,delay,...args));
 stubPhotoFetch(vi.fn(async (url:string) => {
  if(url.endsWith("/photos/hidden")) return Response.json({configured:true,change_id:"first"});
  if(url.endsWith("/photos/assets/query")) {
   if(++pages > 1) return new Promise<Response>(resolve=>finish=resolve);
   return Response.json({items:[photo(1)],total:1});
  }
  return Response.json({items:[],nodes:[],tags:[],profiles:[]});
 }));
 render(App);await screen.findByRole("button",{name:"Open Photo 1.jpg"});
 const visibility=vi.spyOn(document,"visibilityState","get");
 for(let expected=2;expected<=3;expected++) {
  visibility.mockReturnValue("hidden");await fireEvent(document,new Event("visibilitychange"));
  visibility.mockReturnValue("visible");await fireEvent(document,new Event("visibilitychange"));
  await waitFor(()=>expect(pages).toBe(expected));
  expect(screen.queryByRole("button",{name:"Open Photo 1.jpg"})).toBeNull();
  finish(Response.json({items:[photo(1)],total:1}));
  await screen.findByRole("button",{name:"Open Photo 1.jpg"});
 }
});

it("shows Album not found for an unknown album Map", async () => {
 const album=photoAlbum();history.replaceState(null,"",`/photos/map/${album.id}#web_session=synthetic&web_upload_secret=proof`);storage();
 const meta=document.createElement("meta");meta.name="docbank-map-tiles";meta.content=JSON.stringify({tile_url:"https://tiles.example.org/{z}/{x}/{y}",attribution:"Example",max_zoom:19});document.head.append(meta);
 stubPhotoFetch(vi.fn(async(url:string)=>{
  if(url.endsWith("/photos/albums")) return albumResponse([]);
  if(url.endsWith("/photos/hidden")) return albumResponse({configured:true});
  return albumResponse({items:[],map_items:[],nodes:[],tags:[],profiles:[],total:0});
 }));render(App);
 await screen.findByText("Album not found");
 expect(screen.queryByText("No located photos")).toBeNull();
 await fireEvent.click(screen.getByRole("button",{name:"Back to albums"}));
 expect(location.pathname).toBe("/photos/albums");
});

it.each(["album", "album year", "direct album", "Hidden", "existing gps_bounds"])("keeps Map scoped when opened from %s", async source => {
  const album = photoAlbum();
  const mapPath = `/photos/map/${album.id}`;
  const libraryScope = source === "Hidden" || source === "existing gps_bounds";
  const gpsBounds = source === "existing gps_bounds" ? { south: "-1", west: "10", north: "1", east: "15" } : { south: "-90", west: "-180", north: "90", east: "180" };
  history.replaceState(null, "", `${source === "direct album" ? mapPath : "/photos"}?retained=library#web_session=synthetic&web_upload_secret=proof`);
  storage();
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const meta = document.createElement("meta");
  meta.name = "docbank-map-tiles";
  meta.content = JSON.stringify({ tile_url: "https://tiles.example.org/{z}/{x}/{y}", attribution: "Example", max_zoom: 19 });
  document.head.append(meta);
  const queries: { projection?: string; hidden: boolean; query: Query }[] = [];
  let library: Photos | undefined;
  let albumPhotos: Photos | undefined;
  const resume = Photos.prototype.resume;
  vi.spyOn(Photos.prototype, "resume").mockImplementation(function(this: Photos, ...args) {
    if (!this.projection && !this.hidden && !this.query.filters?.set_ids) library = this;
    if (!this.projection && this.query.filters?.set_ids) albumPhotos = this;
    return resume.apply(this, args);
  });
  stubPhotoFetch(vi.fn(async (url: string, init: RequestInit) => {
    if (url.endsWith("/photos/albums")) return albumResponse([album]);
    if (url.endsWith("/photos/hidden")) return albumResponse({ configured: true, expires_at: "2099-01-01T00:00:00Z", expires_in: 300 });
    if (url.endsWith("/photos/assets/query")) {
      const request = JSON.parse(String(init.body));
      queries.push(request);
      return albumResponse(request.projection ? { items: [], map_items: [mapPhoto(1)], total: 1 } : { items: [photo(1)], total: 1 });
    }
    return albumResponse({ items: [], nodes: [], tags: [], profiles: [] });
  }));
  render(App);
  if (source !== "direct album") {
    await screen.findByRole("button", { name: "Open Photo 1.jpg" });
    library!.baseQuery = { ...library!.baseQuery, text: "beach", sort: { field: "relevance", direction: "desc" }, filters: { cameras: ["Library camera"], ...(source === "existing gps_bounds" ? { gps_bounds: gpsBounds } : {}) } };
    await library!.selectDate("2024");
    if (source !== "existing gps_bounds") {
      await fireEvent.click(screen.getByRole("button", { name: source.startsWith("album") ? /^Trip/ : "Hidden" }));
      await screen.findByRole("heading", { name: source.startsWith("album") ? "Trip" : "Hidden" });
      if (source === "album year") { await fireEvent.click(screen.getByRole("button", { name: "Timeline" })); await albumPhotos!.selectDate("2023"); }
    }
    if (source === "Hidden") {
      await waitFor(() => expect((screen.getByRole("button", { name: "Search" }) as HTMLButtonElement).disabled).toBe(false));
      await fireEvent.input(await screen.findByRole("searchbox", { name: "Search photos" }), { target: { value: "private" } });
      await fireEvent.click(screen.getByRole("button", { name: "Search" }));
      await waitFor(() => expect(queries.some(request => request.hidden && request.query.text === "private")).toBe(true));
    }
    await fireEvent.click(screen.getByRole("button", { name: "Map" }));
  }
  await waitFor(() => expect(queries.some(request => request.projection === "map")).toBe(true));
  const request = queries.find(request => request.projection === "map")!;
  expect(location.pathname).toBe(libraryScope ? "/photos/map" : mapPath);
  expect(location.search).toBe("?retained=library");
  expect(within(screen.getByRole("navigation", { name: "Docbank navigation" })).getByRole("button", { name: "Map" }).getAttribute("aria-current")).toBe("page");
  expect(request.hidden).toBe(false);
  expect(request.query.sort).toEqual({ field: "capture_time", direction: "desc" });
  expect(request.query.filters.gps_bounds).toEqual(gpsBounds);
  if (libraryScope) {
    expect(request.query).toMatchObject({ text: "beach", filters: { cameras: ["Library camera"], capture_after: "2024-01-01", capture_before: "2025-01-01" } });
    expect(request.query.filters.set_ids).toBeUndefined();
  } else {
    expect(request.query.text).toBe("");
    expect(request.query.filters.set_ids).toEqual([album.id]);
    expect(request.query.filters.cameras).toBeUndefined();
    expect(request.query.filters.capture_after).toBe(source === "album year" ? "2023-01-01" : undefined);
    expect(request.query.filters.capture_before).toBe(source === "album year" ? "2024-01-01" : undefined);
  }
  await waitFor(() => expect(map.props?.items).toHaveLength(1));
  const reads = queries.length;
  await fireEvent.click(within(screen.getByRole("navigation", { name: "Docbank navigation" })).getByRole("button", { name: "Map" }));
  expect(location.pathname).toBe(libraryScope ? "/photos/map" : mapPath);
  expect(queries).toHaveLength(reads);
});

it.each(["Library", "album"])("closes the %s viewer from its active sidebar entry without reloading", async kind => {
  const album = photoAlbum({ id: "22222222-2222-4222-8222-0000000000aa", included_count: 1, member_count: 1 });
  const path=kind === "album" ? `/photos/albums/${album.id}` : "/photos";
  history.replaceState(null, "", `${path}#web_session=synthetic&web_upload_secret=proof`);
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  Element.prototype.scrollIntoView = vi.fn();
  const queries: { filters?: { set_ids?: string[] } }[] = [];
  stubPhotoFetch(vi.fn(async (url: string, init: RequestInit) => {
    if (url.endsWith("/photos/albums")) return new Response(JSON.stringify([album]));
    if (url.endsWith("/photos/assets/query")) { queries.push(JSON.parse(init.body as string).query); return new Response(JSON.stringify({ items: [photo(1)], total: 1 })); }
    if (url.includes("/nodes/1")) return new Response(JSON.stringify({ id: 1, kind: "dir", name: "", revision: 1, path: "/" }));
    return new Response(JSON.stringify({ items: [], nodes: [], tags: [], profiles: [] }));
  }));
  render(App);
  await fireEvent.keyDown(await screen.findByRole("button", { name: "Select Photo 1.jpg" }), { key: "Enter" });
  await screen.findByRole("dialog", { name: /Photo viewer/ });
  expect(location.pathname).toBe(path);
  expect(new URLSearchParams(location.hash.slice(1)).get("photo")).toBe("photo-1");
  expect(screen.getByRole("main", { name: kind === "album" ? "Photo album" : "Photo library" })).toBeTruthy();
  expect(queries[0]?.filters?.set_ids).toEqual(kind === "album" ? [album.id] : undefined);
  const reads=queries.length;
  await fireEvent.click(screen.getByRole("button", { name: kind === "album" ? /^Trip/ : "Library" }));
  await waitFor(() => expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull());
  await waitFor(() => expect(location.hash).not.toContain("photo="));
  expect(queries).toHaveLength(reads);
  expect(location.pathname).toBe(path);
  await fireEvent.keyDown(await screen.findByRole("button", { name: "Select Photo 1.jpg" }), { key: "Enter" });
  await screen.findByRole("dialog", { name: /Photo viewer/ });
  await fireEvent.click(screen.getByRole("button", { name: kind === "Library" ? "Close photo viewer" : "Library" }));
  await waitFor(() => expect(screen.queryByRole("dialog", { name: /Photo viewer/ })).toBeNull());
  expect(location.pathname).toBe("/photos");
  await waitFor(() => expect(location.hash).not.toContain("photo="));
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
  stubPhotoFetch(vi.fn(async (url: string, init: RequestInit) => {
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

it("restores photo scroll before resumed counts finish and keeps subsequent scrolling", async () => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  storage();
  const counts: { signal: AbortSignal; finish: (response: Response) => void }[] = [];
  let listings = 0;
  vi.stubGlobal("fetch", async (url: string, init?: RequestInit) => {
    if (url.includes("/photos/assets/query")) {
      const request = JSON.parse(init!.body as string);
      if (request.facets?.length) return new Promise<Response>(finish => counts.push({ signal: init!.signal!, finish }));
      listings++;
      return Response.json({ items: Array.from({ length: 100 }, (_, index) => photo(index + 1)), total: 100 });
    }
    if (url.endsWith("/photos/hidden")) return Response.json({ configured: true, change_id: "first" });
    if (url.includes("/nodes/1")) return Response.json({ id: 1, kind: "dir", name: "", revision: 1, path: "/" });
    return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
  });
  render(App);
  await waitFor(() => expect(counts).toHaveLength(1));
  await act(async () => {});
  const original = screen.getByTestId("photo-scroll");
  original.scrollTop = 480;
  await fireEvent.scroll(original);
  await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
  expect(counts[0].signal.aborted).toBe(true);
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  await waitFor(() => expect(counts).toHaveLength(2));
  const resumed = screen.getByTestId("photo-scroll");
  await waitFor(() => expect(resumed.scrollTop).toBe(480));
  // Leaving Photos conceals the Library, so returning revalidates it with one refresh.
  expect(listings).toBe(2);
  resumed.scrollTop = 960;
  await fireEvent.scroll(resumed);
  await act(async () => {
    counts[0].finish(Response.json({ items: [], total: 100, facets: [] }));
    counts[1].finish(Response.json({ items: [], total: 100, facets: [] }));
  });
  await waitFor(() => expect(screen.queryByText(/Loading counts/)).toBeNull());
  await act(async () => {});
  expect(resumed.scrollTop).toBe(960);
});
