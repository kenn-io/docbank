import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import App from "./App.svelte";
import { root } from "./version-test-fixtures.js";

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
  const folder = { ...root, id: 2, parent_id: 1, name: "Taxes", path: "/Taxes" };
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
      return Response.json({ directory: root, items: [folder], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/nodes/2") return Response.json(folder);
    if (url === "/api/v1/nodes/2/children?limit=1000&offset=0")
      return Response.json({ directory: folder, items: [file], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/nodes/3") return Response.json(file);
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
  await waitFor(() => expect(document.title).toBe("report.pdf · Docbank"));
  await fireEvent.click(
    within(screen.getByRole("dialog", { name: "Read report.pdf" })).getByRole("button", {
      name: "Close",
    }),
  );
  await waitFor(() => expect(document.title).toBe("Taxes · Docbank"));
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
