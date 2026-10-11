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


it.each([false, true])("prepares the latest photo selection after releasing a finished job, hidden=%s", async hidden => {
  history.replaceState(null, "", `${hidden ? "/photos/hidden" : "/photos"}#web_session=synthetic&web_upload_secret=proof`);
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  storage();
  const hash = "a".repeat(64), future = "2099-01-01T00:00:00Z";
  const selections: string[][] = [];
  let source: any, plan: any;
  let releases = 0;
  vi.stubGlobal("fetch", vi.fn(async (url: string, options?: RequestInit) => {
    const body = options?.body ? JSON.parse(String(options.body)) : undefined;
    if (url.endsWith("/photos/hidden")) return Response.json({ configured: true, expires_at: future });
    if (url.includes("/assets/query")) return Response.json({ items: [photo(1), photo(2), photo(3)], total: 3 });
    if (url.endsWith("/sources")) {
      selections.push(body.photos.asset_ids);
      source = { id: body.operation_id, request_sha256: hash, kind: "photos", state: "sealed", member_hash: hash, total: 1, source_bytes: 12, created_at: "2026-01-01T00:00:00Z", expires_at: future };
      return Response.json(source);
    }
    if (url.endsWith("/plans")) {
      plan = { format: "docbank-bundle-v1", id: body.operation_id, vault_id: "synthetic", toolchain: "go1.27", source, roles: body.roles, photo_render: body.photo_render, fingerprint: hash, total: 1, role_entries: 1, role_bytes: 12, metadata_bytes: 100, created_at: source.created_at, expires_at: future };
      return Response.json(plan);
    }
    if (url.endsWith("/preview")) return Response.json({ plan_id: plan.id, fingerprint: hash, member_hash: hash, total: 1, roles: [{ role: "photo_rendered", available_members: 1, unavailable_members: 0, files: 1, bytes: 12 }] });
    if (url.endsWith("/jobs")) return Response.json({ id: body.operation_id, plan_id: plan.id, fingerprint: hash, state: "failed", failure: "Synthetic export failure", sequence: 1, completed_roles: 0, completed_bytes: 0, attempt: 1, created_at: source.created_at, deadline: future, expires_at: future });
    if (options?.method === "DELETE") {
      if (++releases === 1) return Response.json({ code: "export_retained", detail: "Download retained" }, { status: 409 });
      return new Response(null, { status: 204 });
    }
    return Response.json({ items: [], nodes: [], tags: [], profiles: [] });
  }));
  render(App);
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  await fireEvent.click(screen.getByRole("button", { name: "Export selection" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Prepare" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Start reviewed export" }));
  await screen.findByText("Synthetic export failure");
  for (const id of [2, 3]) {
    await fireEvent.click(screen.getByRole("button", { name: "Close export" }));
    await fireEvent.click(screen.getByRole("button", { name: "Clear selection" }));
    await fireEvent.click(screen.getByRole("checkbox", { name: `Select photo Photo ${id}.jpg` }));
    await fireEvent.click(screen.getByRole("button", { name: "Export selection" }));
  }
  await fireEvent.click(await screen.findByRole("button", { name: "Prepare another export" }));
  await screen.findByText(/Your browser is still downloading/);
  await fireEvent.click(screen.getByRole("button", { name: "Prepare another export" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Prepare" }));
  await screen.findByRole("button", { name: "Start reviewed export" });
  expect(selections).toEqual([["photo-1"], ["photo-3"]]);
});
