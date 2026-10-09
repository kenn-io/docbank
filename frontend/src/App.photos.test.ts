import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { photo, photoAlbum, albumResponse, storage } from "./photo-test-fixtures.js";
import App from "./App.svelte";

afterEach(() => { cleanup(); history.replaceState(null, "", "/"); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

it("opens an album from the index, preserves its sort scope, and deletes only the album", async () => {
  history.replaceState(null, "", "/photos/albums#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  Element.prototype.scrollIntoView = vi.fn();
  const album = photoAlbum({ starred: true, member_count: 1, included_count: 1 });
  let deleted = false;
  const queries: unknown[] = [];
  const fetcher = vi.fn(async (url: string, init: RequestInit) => {
    if (url.endsWith("/photos/albums")) return new Response(JSON.stringify(deleted ? [] : [album]));
    if (url.endsWith(`/photos/albums/${album.id}`) && init.method === "DELETE") { deleted = true; return albumResponse({ ...album, revision: 2, deleted_at: "2025-01-01" }); }
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
  expect(screen.getByRole("combobox", { name: "Sort photos: Imported" })).toBeTruthy();
  await screen.findByText("1 selected photo");
  await waitFor(() => expect(screen.getByRole("button", { name: /Trip/ }).textContent).toContain("7"));
  await fireEvent.click(screen.getByRole("button", { name: "Album actions" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: "Delete album…" }));
  await fireEvent.click(screen.getByRole("button", { name: "Delete album" }));
  await waitFor(() => expect(location.pathname).toBe("/photos/albums"));

});

it("retains photo state and previews across sidebar switches until lock", async () => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  vi.stubGlobal("URL", class extends URL { static createObjectURL() { return "blob:synthetic"; } static revokeObjectURL() {} });
  const stored = storage();
  const fetcher = vi.fn(async (url: string) => {
    if (url.endsWith("/photos/albums")) return new Response("[]");
    if (url.includes("/photos/assets/query")) return new Response(JSON.stringify({ items: [{ ...photo(1), previews: { ...photo(1).previews, grid: { state: "ready", generation_id: "synthetic" } } }], total: 1 }));
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
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  expect(location.pathname).toBe("/photos");
  await screen.findByText("1 selected photo");
  await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
  expect(listings()).toBe(1);
  expect(previews()).toBe(1);
  history.replaceState(null, "", "/");
  await fireEvent(window, new PopStateEvent("popstate"));
  await waitFor(() => expect(screen.queryByRole("main", { name: "Photo library" })).toBeNull());
  await fireEvent.click(screen.getByRole("button", { name: /Lock/ }));
  await waitFor(() => expect(stored.data.has(cacheName)).toBe(false));
});

it.each(["add", "remove"] as const)("refreshes an album opened while its %s is pending", async operation => {
  history.replaceState(null, "", `${operation === "add" ? "/photos" : "/photos/albums/22222222-2222-4222-8222-00000000006a"}#web_session=synthetic&web_upload_secret=proof`);
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  Element.prototype.scrollIntoView = vi.fn();
  const album = photoAlbum({ id: "22222222-2222-4222-8222-00000000006a", included_count: operation === "add" ? 0 : 1 });
  let included = operation === "remove";
  let finish!: () => void;
  let albumReads = 0;
  const fetcher = vi.fn(async (url: string, init: RequestInit) => {
    if (url.endsWith(`/members/${operation}`)) return new Promise<Response>(resolve => finish = () => {
      included = operation === "add";
      album.included_count = included ? 1 : 0;
      album.revision++;
      resolve(albumResponse(album));
    });
    if (url.endsWith("/photos/albums")) return new Response(JSON.stringify([album]));
    if (url.endsWith("/photos/assets/query")) {
      const scoped = JSON.parse(init.body as string).query.filters?.set_ids?.includes(album.id);
      if (scoped) albumReads++;
      const items = !scoped || included ? [photo(1)] : [];
      return new Response(JSON.stringify({ items, total: items.length }));
    }
    if (url.includes("/nodes/1")) return new Response(JSON.stringify({ id: 1, kind: "dir", name: "", revision: 1, path: "/" }));
    return new Response(JSON.stringify({ items: [], nodes: [], tags: [], profiles: [] }));
  });
  vi.stubGlobal("fetch", fetcher);
  render(App);
  await fireEvent.click(await screen.findByRole("button", { name: "Select Photo 1.jpg" }));
  if (operation === "add") {
    await fireEvent.click(screen.getByRole("button", { name: /Add to album/ }));
    await fireEvent.mouseDown(await screen.findByRole("option", { name: "Trip" }));
  } else {
    await fireEvent.click(screen.getByRole("button", { name: "Remove from album" }));
  }
  await waitFor(() => expect(finish).toBeTypeOf("function"));
  if (operation === "remove") await fireEvent.click(screen.getByRole("button", { name: "Library" }));
  await fireEvent.click(screen.getByRole("button", { name: /^Trip/ }));
  await screen.findByText(operation === "add" ? "0 photos · 0 loaded" : "1 photos · 1 loaded");
  const before = albumReads;
  finish();
  await screen.findByText(operation === "add" ? "1 photos · 1 loaded" : "0 photos · 0 loaded");
  expect(albumReads).toBeGreaterThan(before);
  await waitFor(() => expect(!!screen.queryByRole("button", { name: "Select Photo 1.jpg" })).toBe(operation === "add"));
});
