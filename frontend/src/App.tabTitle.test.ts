import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import App from "./App.svelte";
import { root } from "./version-test-fixtures.js";
import {
  photo,
  photoAlbum,
  photoFace,
  photoPersonAssertion,
  photoMetadataResponse,
  storage,
} from "./photo-test-fixtures.js";
import { PhotoCull } from "./photoCull.svelte.js";
import { photoCullStorage } from "./photoCullStorage.js";
import { IDBFactory, IDBKeyRange } from "fake-indexeddb";
import * as api from "./generated/docbank.js";

vi.mock("./PhotoMap.svelte", () => ({ default: () => ({}) }));

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

afterEach(() => {
  cleanup();
  localStorage.clear();
  sessionStorage.clear();
  document.querySelectorAll('meta[name="docbank-map-tiles"]').forEach((meta) => meta.remove());
  history.replaceState(null, "", "/");
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it("names a missing linked photo", async () => {
  history.replaceState(
    null,
    "",
    "/photos#web_session=synthetic&web_upload_secret=proof&photo=00000000-0000-4000-8000-000000000099",
  );
  storage();
  vi.stubGlobal("fetch", async (url: string, init: RequestInit) => {
    if (url.endsWith("/photos/hidden")) return Response.json({ configured: true });
    if (url.endsWith("/photos/assets/query")) {
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
});

it("keeps a locked hidden photo out of the tab title", async () => {
  history.replaceState(
    null,
    "",
    "/photos/hidden#web_session=synthetic&web_upload_secret=proof&photo=00000000-0000-4000-8000-000000000001",
  );
  storage();
  vi.stubGlobal("fetch", async (url: string, init: RequestInit) => {
    if (url.endsWith("/photos/hidden")) return Response.json({ configured: true });
    return (
      photoMetadataResponse(url, init) ??
      Response.json({ items: [], nodes: [], tags: [], profiles: [] })
    );
  });
  render(App);
  await screen.findByLabelText("Passcode");
  await waitFor(() => expect(document.title).toBe("Hidden · Docbank"));
});

it("names the current screen in the browser tab", async () => {
  history.replaceState(null, "", "/");
  storage();
  vi.stubGlobal("indexedDB", new IDBFactory());
  vi.stubGlobal("IDBKeyRange", IDBKeyRange);
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const album = photoAlbum();
  const rows = [photo(1), photo(2)].map((row, i) => ({
    ...row,
    asset_id: `00000000-0000-4000-8000-00000000000${i + 1}`,
    content_version_id: `10000000-0000-4000-8000-00000000000${i + 1}`,
  }));
  const personID = "00000000-0000-4000-8000-000000000001";
  vi.spyOn(api, "listPhotoFaces").mockImplementation(async (version) =>
    version === rows[0].content_version_id
      ? {
          faces: [photoFace({ content_version_id: version })],
          assertions: [photoPersonAssertion({ content_version_id: version, person_id: personID })],
          people: [{ person_id: personID, display_name: "Ada", state: "curated" }],
        }
      : { faces: [], assertions: [], people: [] },
  );
  const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  const conceal = async (title: string) => {
    visibility.mockReturnValue("hidden");
    await fireEvent(document, new Event("visibilitychange"));
    await waitFor(() => expect(document.title).toBe(title));
    visibility.mockReturnValue("visible");
    await fireEvent(document, new Event("visibilitychange"));
  };
  vi.spyOn(PhotoCull.prototype, "start").mockImplementation(async function (this: PhotoCull) {
    this.run = {
      ids: rows.map((row) => row.asset_id),
      checkpoint: {
        runID: "run",
        revision: 0,
        pass: rows.map((row) => row.asset_id),
        position: 0,
        visited: [],
        skipped: [],
        unavailable: [],
        scope: "all",
        autoAdvance: true,
      },
    };
    await photoCullStorage("11111111-1111-4111-8111-111111111111").replace(this.run);
    return true;
  });
  const meta = document.createElement("meta");
  meta.name = "docbank-map-tiles";
  meta.content = JSON.stringify({
    tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}",
    host: "tiles.example.org",
    attribution: "Example",
    max_zoom: 19,
  });
  document.head.append(meta);
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
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/daemon/web-auth") return Response.json({ enabled: true });
    if (url === "/api/daemon/web-auth/login")
      return Response.json({
        enabled: true,
        session: { token: "signed-in", upload_secret: "proof" },
      });
    if (url.includes("/audit/status"))
      return Response.json({ vault_id: "11111111-1111-4111-8111-111111111111" });
    if (url.endsWith("/photos/hidden/lock")) return Response.json({ configured: true });
    if (url.endsWith("/photos/hidden"))
      return Response.json({
        configured: true,
        expires_at: "2099-01-01T00:00:00Z",
        expires_in: 300,
      });
    if (url.endsWith("/photos/albums")) return Response.json([album]);
    if (url.endsWith("/photos/assets/query")) {
      const request = JSON.parse(String(init?.body));
      const ids: string[] | undefined = request.query.filters?.asset_ids;
      const items = ids ? rows.filter((row) => ids.includes(row.asset_id)) : rows;
      return Response.json({ items, map_items: [], total: items.length, facets: [] });
    }
    const metadata = photoMetadataResponse(url, init as RequestInit);
    if (metadata) return metadata;
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
    return new Response(null, { status: 404 });
  });
  render(App);
  await waitFor(() => expect(document.title).toBe("Sign in · Docbank"));
  await fireEvent.input(await screen.findByLabelText("API key"), {
    target: { value: "synthetic-key" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
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
  await fireEvent.click(screen.getByRole("button", { name: "System" }));
  await waitFor(() => expect(document.title).toBe("Disk use · Docbank"));
  await fireEvent.click(screen.getByRole("tab", { name: "Backups" }));
  await waitFor(() => expect(document.title).toBe("Backups · Docbank"));
  await fireEvent.click(screen.getByRole("button", { name: "Close System" }));
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Open Photo 1.jpg" }));
  await waitFor(() => expect(document.title).toBe("Photo 1.jpg · Docbank"));
  await fireEvent.click(screen.getByRole("button", { name: "Close photo viewer" }));
  for (const id of [1, 2])
    await fireEvent.click(
      await screen.findByRole("checkbox", { name: `Select photo Photo ${id}.jpg` }),
    );
  await fireEvent.click(screen.getByRole("button", { name: "Compare" }));
  await waitFor(() => expect(document.title).toBe("Compare photos · Docbank"));
  await conceal("Compare photos · Docbank");
  await screen.findByRole("dialog", { name: "Compare photos" });
  await fireEvent.click(screen.getByRole("button", { name: "Exit compare" }));
  await waitFor(() => expect(document.title).toBe("Photos · Docbank"));
  const clear = screen.queryByRole("button", { name: "Clear selection" });
  if (clear) await fireEvent.click(clear);
  const cull = await screen.findByRole("button", { name: "Cull" });
  await waitFor(() => expect((cull as HTMLButtonElement).disabled).toBe(false));
  await fireEvent.click(cull);
  let dialog = await screen.findByRole("dialog", { name: "Cull photos" });
  await within(dialog).findByText("Photo 1.jpg");
  await waitFor(() => expect(document.title).toBe("Photo 1.jpg · Docbank"));
  await conceal("Photo · Docbank");
  dialog = await screen.findByRole("dialog", { name: "Cull photos" });
  await waitFor(() => expect(document.title).toBe("Photo 1.jpg · Docbank"));
  await fireEvent.click(within(dialog).getByRole("button", { name: "Next photo" }));
  await within(dialog).findByText("Photo 2.jpg");
  await fireEvent.click(within(dialog).getByRole("button", { name: "Next photo" }));
  await within(dialog).findByRole("heading", { name: "Pass complete" });
  await waitFor(() => expect(document.title).toBe("Pass complete · Docbank"));
  await conceal("Pass complete · Docbank");
  await screen.findByRole("heading", { name: "Pass complete" });
  dialog = screen.getByRole("dialog", { name: "Cull photos" });
  await fireEvent.click(within(dialog).getByRole("button", { name: "Back to photos" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Open Photo 1.jpg" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Ada" }));
  await screen.findByText("Person: Ada");
  await waitFor(() => expect(document.title).toBe("Ada · Photos · Docbank"));
  await conceal("Person photos · Docbank");
  await screen.findByText("Person: Ada");
  await waitFor(() => expect(document.title).toBe("Ada · Photos · Docbank"));
  await fireEvent.click(screen.getByRole("button", { name: "Albums" }));
  await waitFor(() => expect(document.title).toBe("Albums · Docbank"));
  await fireEvent.click(screen.getByRole("button", { name: "People" }));
  await waitFor(() => expect(document.title).toBe("People · Docbank"));
  await fireEvent.click(screen.getByRole("button", { name: "Library" }));
  await screen.findByText("Person: Ada");
  await waitFor(() => expect(document.title).toBe("Ada · Photos · Docbank"));
  await fireEvent.click(screen.getByRole("button", { name: "Map" }));
  await screen.findByRole("heading", { name: "Photos in view" });
  await waitFor(() => expect(document.title).toBe("Map · Docbank"));
  await fireEvent.click(screen.getByRole("button", { name: "Hidden" }));
  await waitFor(() => expect(document.title).toBe("Hidden · Docbank"));
  await fireEvent.click(await screen.findByRole("button", { name: "Open Photo 1.jpg" }));
  await waitFor(() => expect(document.title).toBe("Hidden · Docbank"));
  await fireEvent.click(await screen.findByRole("button", { name: "Ada" }));
  await screen.findByText("Person: Ada");
  await waitFor(() => expect(document.title).toBe("Hidden · Docbank"));
  await fireEvent.click(screen.getByRole("button", { name: "Library" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Albums" }));
  await waitFor(() => expect(document.title).toBe("Albums · Docbank"));
  await fireEvent.click(await screen.findByRole("link", { name: /Trip/ }));
  await screen.findByRole("heading", { name: "Trip" });
  await waitFor(() => expect(document.title).toBe("Trip · Docbank"));
  await conceal("Album · Docbank");
  await screen.findByRole("heading", { name: "Trip" });
  await waitFor(() => expect(document.title).toBe("Trip · Docbank"));
  await fireEvent.click(screen.getByRole("button", { name: "Map" }));
  await screen.findByRole("heading", { name: "Photos in view" });
  await waitFor(() => expect(document.title).toBe("Map · Trip · Docbank"));
  await conceal("Map · Docbank");
  await screen.findByRole("heading", { name: "Photos in view" });
  await waitFor(() => expect(document.title).toBe("Map · Trip · Docbank"));
});

it("names a saved tag view when its tag was deleted", async () => {
  history.replaceState(null, "", "/#web_session=synthetic&web_upload_secret=proof");
  const tagID = "33333333-3333-4333-8333-333333333333";
  const folder = { ...root, id: 2, parent_id: 1, name: "Archive", path: "/Archive" };
  let catalogReads = 0;
  let assignmentReads = 0;
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
        total: catalogReads === 1 ? 1 : 0,
        limit: 1000,
        offset: 0,
      });
    }
    if (url.includes(`/tags/${tagID}/nodes?`)) {
      assignmentReads++;
      if (assignmentReads > 1) return new Response(null, { status: 404 });
      return Response.json({
        items: [{ node: folder, path: folder.path }],
        total: 1,
        limit: 1000,
        offset: 0,
      });
    }
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
  await waitFor(() => expect(assignmentReads).toBe(2));
  await screen.findByText("HTTP 404");
  await waitFor(() => expect(document.title).toBe("Tag · Docbank"));
  finishCatalog(Response.json({ items: [], total: 0, limit: 1000, offset: 0 }));
  await waitFor(() => expect(document.title).toBe("All files · Docbank"));
});
