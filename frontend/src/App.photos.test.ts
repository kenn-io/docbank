import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { photo, storage } from "./photo-test-fixtures.js";
import App from "./App.svelte";

afterEach(() => { cleanup(); history.replaceState(null, "", "/"); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

it.each(["unhide", "trash"])("refreshes a remounted Hidden grid after a delayed %s", async kind => {
  history.replaceState(null, "", "/photos/hidden#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  storage();
  let changed = false;
  let finish!: (response: Response) => void;
  vi.stubGlobal("fetch", vi.fn(async (url: string, options?: RequestInit) => {
    if (url.endsWith("/photos/hidden")) return Response.json({ configured: true, expires_at: "2099-01-01T00:00:00Z" });
    if (url.includes("/assets/query")) {
      const hidden = JSON.parse(String(options?.body)).hidden;
      const items = hidden && !changed ? [photo(1)] : [];
      return Response.json({ items, total: items.length });
    }
    if (url.endsWith(`/assets/photo-1/${kind}`)) return new Promise<Response>(resolve => finish = resolve);
    return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
  }));
  render(App);
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  if (kind === "unhide") {
    await fireEvent.click(screen.getByRole("button", { name: "Actions for Photo 1.jpg" }));
    await fireEvent.click(await screen.findByRole("menuitem", { name: "Unhide" }));
  } else {
    await fireEvent.click(screen.getByRole("button", { name: "Move to trash" }));
    await fireEvent.click(within(screen.getByRole("dialog", { name: "Move selected photos to trash" })).getByRole("button", { name: "Move to trash" }));
  }
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

it("retains photo state and previews across sidebar switches until lock", async () => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  vi.stubGlobal("URL", class extends URL { static createObjectURL() { return "blob:synthetic"; } static revokeObjectURL() {} });
  const stored = storage();
  let items = [photo(1)];
  const fetcher = vi.fn(async (url: string) => {
    if (url.includes("/photos/assets/query")) return new Response(JSON.stringify({ items: items.map(item => ({ ...item, previews: { ...item.previews, grid: { state: "ready", generation_id: "synthetic" } } })), total: 1 }));
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
  await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
  expect(location.pathname).toBe("/");
  expect(screen.queryByRole("main", { name: "Photo library" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  expect(location.pathname).toBe("/photos");
  await screen.findByText("1 selected photo");
  await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" });
  expect(listings()).toBe(1);
  expect(previews()).toBe(1);
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  items = [photo(2)];
  await fireEvent(document, new Event("visibilitychange"));
  expect(screen.getByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeTruthy();
  expect(fetcher.mock.calls.some(([url]) => url.endsWith("/photos/hidden"))).toBe(false);
  await fireEvent.click(screen.getByRole("button", { name: "Refresh previews" }));
  await screen.findByRole("checkbox", { name: "Select photo Photo 2.jpg" });
  expect(listings()).toBe(2);
  history.replaceState(null, "", "/");
  await fireEvent(window, new PopStateEvent("popstate"));
  await waitFor(() => expect(screen.queryByRole("main", { name: "Photo library" })).toBeNull());
  await fireEvent.click(screen.getByRole("button", { name: /Lock/ }));
  await waitFor(() => expect(stored.data.has(cacheName)).toBe(false));
});

it.each([false, true])("keeps selection and previews after partial visibility writes and failed refresh, hidden=%s", async hidden => {
  history.replaceState(null, "", `${hidden ? "/photos/hidden" : "/photos"}#web_session=synthetic&web_upload_secret=proof`);
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  vi.stubGlobal("URL", class extends URL { static createObjectURL() { return "blob:synthetic"; } static revokeObjectURL() {} });
  storage();
  const items = [photo(1), photo(2), photo(3)].map(item => ({ ...item, previews: { ...item.previews, grid: { state: "ready" as const, generation_id: "synthetic" } } }));
  let refreshFailed = false;
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    if (url.endsWith("/photos/hidden")) return Response.json({ configured: true, expires_at: "2099-01-01T00:00:00Z" });
    if (url.includes("/previews/")) return new Response("synthetic-jpeg", { headers: hidden ? { "Cache-Control": "no-store" } : {} });
    if (url.includes("/assets/query")) {
      if (refreshFailed) throw new Error("Listing unavailable");
      return Response.json({ items, total: 3 });
    }
    if (url.endsWith(`/photo-1/${hidden ? "unhide" : "hide"}`)) { refreshFailed = true; return Response.json({ id: "photo-1", revision: 2 }); }
    if (url.endsWith(`/photo-2/${hidden ? "unhide" : "hide"}`)) return Response.json({ detail: "Photo changed" }, { status: 412 });
    return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
  }));
  render(App);
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select photo Photo 2.jpg" }));
  const grid = screen.getByRole("main", { name: "Photo library" });
  const preview = await screen.findByAltText("Photo 3.jpg");
  await fireEvent.click(screen.getByRole("button", { name: "Actions for Photo 1.jpg" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: hidden ? "Unhide" : "Hide" }));
  await screen.findByText("Listing unavailable");
  expect(screen.queryByRole("checkbox", { name: "Select photo Photo 1.jpg" })).toBeNull();
  expect(screen.getByRole("main", { name: "Photo library" })).toBe(grid);
  expect(screen.getByAltText("Photo 3.jpg")).toBe(preview);
  expect(screen.getByText("1 selected photo")).toBeTruthy();
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
    if (url.endsWith("/photos/hidden")) return Response.json({ configured: true, ...(unlocked ? { expires_at: "2099-01-01T00:00:00Z" } : {}) });
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
  await screen.findByText("No hidden photos");
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
