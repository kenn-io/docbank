import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import App from "./App.svelte";
import { root } from "./version-test-fixtures.js";
import { photo, photoMetadataResponse, storage } from "./photo-test-fixtures.js";
import { PhotoCull } from "./photoCull.svelte.js";
import { IDBFactory, IDBKeyRange } from "fake-indexeddb";

beforeEach(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
});

it("names the photo shown in Cull and restores the library title", async () => {
  history.replaceState(null, "", "/photos#web_session=synthetic&web_upload_secret=proof");
  storage();
  vi.stubGlobal("indexedDB", new IDBFactory());
  vi.stubGlobal("IDBKeyRange", IDBKeyRange);
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  vi.spyOn(PhotoCull.prototype, "restore").mockResolvedValue(false);
  vi.spyOn(PhotoCull.prototype, "start").mockImplementation(async function (this: PhotoCull) {
    this.run = {
      ids: ["photo-1", "photo-2"],
      checkpoint: {
        runID: "run",
        revision: 0,
        pass: ["photo-1", "photo-2"],
        position: 0,
        visited: [],
        skipped: [],
        unavailable: [],
        scope: "all",
        autoAdvance: true,
      },
    };
    return true;
  });
  const rows = [photo(1), photo(2)];
  vi.stubGlobal("fetch", async (url: string, init: RequestInit) => {
    if (url.endsWith("/photos/hidden")) return Response.json({ configured: true });
    if (url.includes("/audit/status"))
      return Response.json({ vault_id: "11111111-1111-4111-8111-111111111111" });
    if (url.endsWith("/photos/assets/query")) {
      const ids: string[] | undefined = JSON.parse(String(init.body)).query.filters?.asset_ids;
      return Response.json({
        items: ids ? rows.filter((row) => ids.includes(row.asset_id)) : rows,
        total: rows.length,
      });
    }
    return (
      photoMetadataResponse(url, init) ??
      Response.json({ items: [], nodes: [], tags: [], profiles: [] })
    );
  });
  render(App);
  const cull = await screen.findByRole("button", { name: "Cull" });
  await waitFor(() => expect((cull as HTMLButtonElement).disabled).toBe(false));
  await fireEvent.click(cull);
  const dialog = await screen.findByRole("dialog", { name: "Cull photos" });
  await within(dialog).findByText("Photo 1.jpg");
  await waitFor(() => expect(document.title).toBe("Photo 1.jpg · Docbank"));
  await fireEvent.click(within(dialog).getByRole("button", { name: "Next photo" }));
  await within(dialog).findByText("Photo 2.jpg");
  await waitFor(() => expect(document.title).toBe("Photo 2.jpg · Docbank"));
  await fireEvent.click(within(dialog).getByRole("button", { name: "Exit cull" }));
  await waitFor(() => expect(document.title).toBe("Photos · Docbank"));
});

it("names a missing or loading linked photo and then its resolved name", async () => {
  history.replaceState(
    null,
    "",
    "/photos#web_session=synthetic&web_upload_secret=proof&photo=photo-1",
  );
  storage();
  let resolvePhoto!: (response: Response) => void;
  vi.stubGlobal("fetch", async (url: string, init: RequestInit) => {
    if (url.endsWith("/photos/hidden")) return Response.json({ configured: true });
    if (url.endsWith("/photos/assets/query")) {
      const ids: string[] | undefined = JSON.parse(String(init.body)).query.filters?.asset_ids;
      if (ids?.includes("photo-2"))
        return new Promise<Response>((resolve) => (resolvePhoto = resolve));
      return Response.json({ items: [], total: 0 });
    }
    return (
      photoMetadataResponse(url, init) ??
      Response.json({ items: [], nodes: [], tags: [], profiles: [] })
    );
  });
  render(App);
  await screen.findByText("This photo is no longer in this view.");
  await waitFor(() => expect(document.title).toBe("Photo · Docbank"));
  history.pushState(null, "", "/photos#photo=photo-2");
  window.dispatchEvent(new HashChangeEvent("hashchange"));
  await waitFor(() => expect(resolvePhoto).toBeTypeOf("function"));
  expect(document.title).toBe("Photo · Docbank");
  resolvePhoto(Response.json({ items: [photo(2)], total: 1 }));
  await waitFor(() => expect(document.title).toBe("Photo 2.jpg · Docbank"));
  await fireEvent.click(screen.getByRole("button", { name: "Close photo viewer" }));
  await waitFor(() => expect(document.title).toBe("Photos · Docbank"));
});

afterEach(() => {
  cleanup();
  localStorage.clear();
  sessionStorage.clear();
  history.replaceState(null, "", "/");
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it("names the current screen in the browser tab", async () => {
  history.replaceState(null, "", "/#web_session=synthetic&web_upload_secret=proof");
  const folder = { ...root, id: 2, parent_id: 1, name: "Taxes", path: "/Taxes" };
  const archive = { ...root, id: 4, parent_id: 1, name: "Archive", path: "/Archive" };
  const nested = { ...root, id: 7, parent_id: 4, name: "Taxes", path: "/Archive/Taxes" };
  const file = {
    ...root,
    id: 3,
    parent_id: 2,
    name: "report.pdf",
    path: "/Taxes/report.pdf",
    kind: "file",
    size: 100,
    mime_type: "application/pdf",
    current_version_id: "00000000-0000-4000-8000-000000000001",
    blob_hash: "a".repeat(64),
  };
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.startsWith("/api/v1/search?"))
      return Response.json({ hits: [], limit: 1000, truncated: false });
    if (url === "/api/v1/path?path=%2F") return Response.json(root);
    if (url === "/api/v1/nodes/1/children?limit=1000&offset=0")
      return Response.json({
        directory: root,
        items: [folder, archive, { ...file, id: 6, parent_id: 1, name: "Sample.pdf" }],
        total: 3,
        limit: 1000,
        offset: 0,
      });
    if (url === "/api/v1/nodes/2") return Response.json(folder);
    if (url === "/api/v1/nodes/2/children?limit=1000&offset=0")
      return Response.json({ directory: folder, items: [file], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/nodes/3") return Response.json(file);
    if (url === "/api/v1/nodes/4") return Response.json(archive);
    if (url === "/api/v1/nodes/4/children?limit=1000&offset=0")
      return Response.json({
        directory: archive,
        items: [{ ...file, id: 5, parent_id: 4 }, nested],
        total: 2,
        limit: 1000,
        offset: 0,
      });
    if (url === "/api/v1/nodes/7") return Response.json(nested);
    if (url === "/api/v1/nodes/7/children?limit=1000&offset=0")
      return Response.json({ directory: nested, items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/nodes/5")
      return Response.json({ ...file, id: 5, parent_id: 4, path: "/Archive/report.pdf" });
    if (url === "/api/v1/nodes/6")
      return Response.json({
        ...file,
        id: 6,
        parent_id: 1,
        name: "Sample.pdf",
        path: "/Sample.pdf",
      });
    return Response.json({ items: [], total: 0, limit: 1000, offset: 0 });
  });
  render(App);
  await screen.findByRole("cell", { name: "Taxes" });
  await waitFor(() => expect(document.title).toBe("All files · Docbank"));
  await fireEvent.doubleClick(screen.getByRole("cell", { name: "Taxes" }));
  await screen.findByRole("cell", { name: "report.pdf" });
  await waitFor(() => expect(document.title).toBe("Taxes · Docbank"));
  await fireEvent.click(screen.getByRole("cell", { name: "report.pdf" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Open" }));
  await waitFor(() => expect(document.title).toBe("report.pdf · /Taxes · Docbank"));
  await fireEvent.click(
    within(screen.getByRole("dialog", { name: "Read report.pdf" })).getByRole("button", {
      name: "Close",
    }),
  );
  await waitFor(() => expect(document.title).toBe("Taxes · Docbank"));
  await fireEvent.click(
    within(screen.getByRole("navigation", { name: "Docbank navigation" })).getByRole("button", {
      name: "All files",
    }),
  );
  await fireEvent.doubleClick(await screen.findByRole("cell", { name: "Archive" }));
  await fireEvent.click(await screen.findByRole("cell", { name: "report.pdf" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Open" }));
  await waitFor(() => expect(document.title).toBe("report.pdf · /Archive · Docbank"));
  await fireEvent.click(
    within(screen.getByRole("dialog", { name: "Read report.pdf" })).getByRole("button", {
      name: "Close",
    }),
  );
  await fireEvent.doubleClick(await screen.findByRole("cell", { name: "Taxes" }));
  await waitFor(() => expect(document.title).toBe("Taxes · /Archive · Docbank"));
  await fireEvent.click(
    within(screen.getByRole("navigation", { name: "Docbank navigation" })).getByRole("button", {
      name: "All files",
    }),
  );
  await fireEvent.click(await screen.findByRole("cell", { name: "Sample.pdf" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Open" }));
  await waitFor(() => expect(document.title).toBe("Sample.pdf · Docbank"));
  await fireEvent.click(
    within(screen.getByRole("dialog", { name: "Read Sample.pdf" })).getByRole("button", {
      name: "Close",
    }),
  );
  const search = screen.getByRole("searchbox", { name: "Search documents" });
  await fireEvent.input(search, { target: { value: "alpha" } });
  await fireEvent.submit(search.closest("form")!);
  await waitFor(() => expect(document.title).toBe("Search · Docbank"));
  await fireEvent.click(screen.getByRole("button", { name: "Searches" }));
  await waitFor(() => expect(document.title).toBe("Searches · Docbank"));
  history.back();
  await waitFor(() => expect(document.title).toBe("Search · Docbank"));
  history.forward();
  await waitFor(() => expect(document.title).toBe("Searches · Docbank"));
  await fireEvent.click(screen.getByRole("button", { name: "System" }));
  await waitFor(() => expect(document.title).toBe("Disk use · Docbank"));
  await fireEvent.click(screen.getByRole("tab", { name: "Backups" }));
  await waitFor(() => expect(document.title).toBe("Backups · Docbank"));
  await fireEvent.click(screen.getByRole("tab", { name: "Running now" }));
  await waitFor(() => expect(document.title).toBe("Running now · Docbank"));
  await fireEvent.click(screen.getByRole("button", { name: "Close System" }));
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Albums" }));
  await waitFor(() => expect(document.title).toBe("Albums · Docbank"));
});

it("names a saved tag view when its tag was deleted", async () => {
  history.replaceState(null, "", "/#web_session=synthetic&web_upload_secret=proof");
  const tagID = "33333333-3333-4333-8333-333333333333";
  const folder = { ...root, id: 2, parent_id: 1, name: "Archive", path: "/Archive" };
  let catalogReads = 0;
  let finishCatalog!: (response: Response) => void;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url === "/api/v1/path?path=%2F") return Response.json(root);
    if (url.includes("/nodes/1/children?"))
      return Response.json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
    if (url.includes("/nodes/2/children?"))
      return Response.json({ directory: folder, items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/nodes/2") return Response.json(folder);
    if (url === "/api/v1/tags?limit=1000&offset=0") {
      catalogReads++;
      if (catalogReads === 3) return new Promise<Response>((resolve) => (finishCatalog = resolve));
      return Response.json({
        items:
          catalogReads === 1 ? [{ id: tagID, name: "Tax", revision: 1, assignment_count: 1 }] : [],
        total: 0,
        limit: 1000,
        offset: 0,
      });
    }
    if (url.includes(`/tags/${tagID}/nodes?`))
      return Response.json({
        items: [{ node: folder, path: folder.path }],
        total: 1,
        limit: 1000,
        offset: 0,
      });
    return Response.json({ items: [], total: 0, limit: 1000, offset: 0 });
  });
  render(App);
  await screen.findByText("No documents yet");
  await fireEvent.click(
    screen.getByRole("combobox", { name: "Browse or filter by tag: All tags" }),
  );
  await fireEvent.click(await screen.findByRole("option", { name: "Tax (1)" }));
  await waitFor(() => expect(document.title).toBe("Tax · Docbank"));
  await fireEvent.doubleClick(await screen.findByRole("cell", { name: "/Archive" }));
  await screen.findByText("Empty folder");
  await fireEvent.click(screen.getByRole("combobox", { name: "Browse or filter by tag: Tax" }));
  await fireEvent.click(screen.getByRole("option", { name: "All tags" }));
  await fireEvent.click(screen.getByRole("button", { name: "Refresh current view" }));
  await waitFor(() => expect(catalogReads).toBe(2));
  await fireEvent.click(
    screen.getByRole("combobox", { name: "Browse or filter by tag: All tags" }),
  );
  await waitFor(() => expect(screen.queryByRole("option", { name: "Tax (1)" })).toBeNull());
  await fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
  await fireEvent.click(screen.getByRole("button", { name: "Back to previous directory" }));
  await screen.findByText("Documents tagged");
  await waitFor(() => expect(document.title).toBe("Tag · Docbank"));
  finishCatalog(Response.json({ items: [], total: 0, limit: 1000, offset: 0 }));
  await waitFor(() => expect(document.title).toBe("All files · Docbank"));
});
