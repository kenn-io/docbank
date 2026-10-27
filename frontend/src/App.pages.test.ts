import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import App from "./App.svelte";
import { productionPackage } from "./package-test-fixtures.js";
import { root } from "./version-test-fixtures.js";
import { queryFingerprint, type Query } from "./query.js";
import { savedSmartSet } from "./photo-test-fixtures.js";
import { snapshotEnvelope } from "./snapshot-test-fixtures.js";

const query: Query = {
  v: 1,
  text: "alpha",
  syntax: "simple",
  mode: "lexical",
  filters: {},
  sort: { field: "name", direction: "asc" },
};

afterEach(() => {
  cleanup();
  localStorage.clear();
  sessionStorage.clear();
  history.replaceState(null, "", "/");
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

async function savedQueriesPage() {
  const fingerprint = await queryFingerprint(query);
  return Response.json({
    items: [
      savedSmartSet(query, fingerprint, {
        name: "Alpha documents",
        description: "Synthetic definition",
        created_at: "2026-09-20T12:00:00Z",
        updated_at: "2026-09-20T12:00:00Z",
      }),
    ],
    total: 1,
    limit: 100,
    offset: 0,
  });
}

function start(path = "/", fragment = "", packages?: Promise<Response>) {
  history.replaceState(
    null,
    "",
    `${path}#web_session=synthetic&web_upload_secret=proof${fragment}`,
  );
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
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url === "/api/v1/processing/profiles")
      return Response.json(
        ["archive", "default"].map((name) => ({
          name,
          fingerprint: "a".repeat(64),
          rendition: true,
          embedding_bindings: [],
          query_embedding_bindings: [],
          reranking_available: false,
        })),
      );
    if (url.includes("/path?")) return Response.json(root);
    if (url.includes("/children?"))
      return Response.json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/collections?limit=100&offset=0")
      return Response.json({
        items: [
          {
            id: "11111111-1111-4111-8111-111111111111",
            source_kind: "cli",
            source_description: "/synthetic/Discovery",
            started_at: "2026-09-20T12:00:00Z",
            file_count: 1,
            total_bytes: 10,
            label: "Discovery",
            label_revision: 1,
            label_updated_at: "2026-09-20T12:00:00Z",
          },
        ],
        total: 1,
        limit: 100,
        offset: 0,
      });
    if (url.startsWith("/api/v1/packages?")) {
      if (packages) return (await packages).clone();
      return Response.json({
        items: [productionPackage()],
        limit: 250,
      });
    }
    return Response.json({
      items: [],
      total: 0,
      limit: url.includes("saved-queries") ? 100 : 1000,
      offset: 0,
    });
  });
  render(App);
  return fetch;
}

it.each([
  ["Searches", "Searches", "/searches"],
  ["Productions", "Productions", "/productions"],
  ["Imports", "Imports", "/imports"],
  ["Recoverable trash", "Trash", "/trash"],
])("navigates to %s and restores it with browser history", async (label, title, path) => {
  start();
  await screen.findByText("Empty folder");
  const button = await screen.findByRole("button", { name: label });
  await fireEvent.click(button);
  const main = await screen.findByRole("main", { name: title });
  expect(screen.getAllByRole("main")).toHaveLength(1);
  expect(within(main).getByRole("heading", { name: title, level: 1 })).toBeTruthy();
  expect(screen.queryByText("Empty folder")).toBeNull();
  expect(button.getAttribute("aria-current")).toBe("page");
  expect(location.pathname).toBe(path);
  expect(screen.queryByRole("button", { name: "Export" })).toBeNull();
  if (title === "Imports")
    expect(await screen.findAllByRole("region", { name: "Imports" })).toHaveLength(1);
  await fireEvent.click(screen.getByRole("button", { name: "Open navigation" }));
  expect(screen.getByRole("button", { name: "Close navigation" })).toBeTruthy();
  await fireEvent.click(button);
  expect(screen.queryByRole("button", { name: "Close navigation" })).toBeNull();
  history.back();
  await waitFor(() => expect(location.pathname).toBe("/"));
  await screen.findByText("Empty folder");
  history.forward();
  await waitFor(() => expect(location.pathname).toBe(path));
  expect(await screen.findByRole("main", { name: title })).toBeTruthy();
});

it("keeps empty Productions open with its sidebar item current", async () => {
  start("/productions", "", Promise.resolve(Response.json({ items: [], limit: 250 })));
  expect(await screen.findByText("No productions yet")).toBeTruthy();
  expect(location.pathname).toBe("/productions");
  expect(screen.getByRole("button", { name: "Productions" }).getAttribute("aria-current")).toBe(
    "page",
  );
});

it.each([
  ["Searches", "/searches", "/api/v1/search-exports?"],
  ["Searches", "/searches", "/api/v1/saved-queries?"],
  ["Productions", "/productions", "/api/v1/bates/exports?"],
  ["Imports", "/imports", "/api/v1/collections?"],
  ["Recoverable trash", "/trash", "/api/v1/trash?"],
])("keeps %s at %s when %s returns 401", async (label, path, endpoint) => {
  const fetch = start();
  await screen.findByText("Empty folder");
  const original = fetch.getMockImplementation()!;
  fetch.mockImplementation(async (input, init) => {
    if (String(input).startsWith(endpoint))
      return Response.json({ detail: "Session expired" }, { status: 401 });
    return original(input, init);
  });
  const entries = history.length;
  await fireEvent.click(await screen.findByRole("button", { name: label }));
  await screen.findByText("Open your Docbank");
  expect(location.pathname).toBe(path);
  expect(history.length).toBe(entries + 1);
});

it("retains unfinished Advanced input and execution settings across Imports navigation", async () => {
  start();
  await screen.findByText("Empty folder");
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  await fireEvent.click(
    await screen.findByRole("combobox", { name: "Processing profile: Default" }),
  );
  await fireEvent.click(screen.getByRole("option", { name: "archive" }));
  await fireEvent.click(screen.getByRole("combobox", { name: "Documents per page: 100" }));
  await fireEvent.click(screen.getByRole("option", { name: "250" }));
  await fireEvent.input(screen.getByLabelText("Filters JSON"), {
    target: { value: '{"size_min":' },
  });
  await fireEvent.keyDown(window, { key: "Escape" });
  await fireEvent.click(screen.getByRole("button", { name: "Imports" }));
  await screen.findByRole("main", { name: "Imports" });
  expect(screen.queryByRole("dialog", { name: "Filters and sort" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "All files" }));
  await screen.findByText("Empty folder");
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  expect((screen.getByLabelText("Filters JSON") as HTMLTextAreaElement).value).toBe('{"size_min":');
  expect(screen.getByRole("combobox", { name: "Processing profile: archive" })).toBeTruthy();
  expect(screen.getByRole("combobox", { name: "Documents per page: 250" })).toBeTruthy();
});

it("keeps search and help shortcuts on Imports after running a saved search", async () => {
  const fetch = start();
  const original = fetch.getMockImplementation()!;
  fetch.mockImplementation(async (input, init) => {
    if (String(input) === "/api/v1/saved-queries?limit=100&offset=0") return savedQueriesPage();
    if (String(input) === "/api/v1/workspace/queries" && init?.method === "POST")
      return Response.json(await snapshotEnvelope(query, []));
    return original(input, init);
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Searches" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Run Alpha documents" }));
  await waitFor(() => expect(location.pathname).toBe("/"));
  await screen.findByText("0 results");
  await fireEvent.click(await screen.findByRole("button", { name: "Imports" }));
  await screen.findByRole("main", { name: "Imports" });
  const search = screen.getByRole("searchbox", { name: "Search documents" });
  await fireEvent.keyDown(window, { key: "/" });
  expect(document.activeElement).toBe(search);
  await fireEvent.keyDown(search, { key: "?", shiftKey: true });
  expect(screen.queryByRole("dialog", { name: "Keyboard shortcuts" })).toBeNull();
});

it.each([
  ["empty", "   ", "/imports"],
  ["non-empty", "alpha", "/"],
])("handles a %s search submitted from Imports", async (_kind, text, path) => {
  const fetch = start("/imports");
  await screen.findByRole("main", { name: "Imports" });
  const search = screen.getByRole("searchbox", { name: "Search documents" });
  await fireEvent.input(search, { target: { value: text } });
  const push = vi.spyOn(history, "pushState");
  fetch.mockClear();
  await fireEvent.submit(search.closest("form")!);
  await waitFor(() => expect(location.pathname).toBe(path));
  if (path === "/imports") {
    expect(push).not.toHaveBeenCalled();
    expect(fetch.mock.calls.some(([input]) => String(input).includes("/api/v1/search?"))).toBe(
      false,
    );
  } else {
    expect(screen.queryByRole("main", { name: "Imports" })).toBeNull();
  }
});

it("opens Advanced on the first click after loading Imports with a query", async () => {
  start("/imports", `&query=${encodeURIComponent(JSON.stringify(query))}`);
  await screen.findByRole("main", { name: "Imports" });
  expect(screen.queryByRole("dialog", { name: "Filters and sort" })).toBeNull();
  expect(screen.getByRole("button", { name: "Advanced" }).getAttribute("aria-expanded")).toBe(
    "false",
  );
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  expect(await screen.findByRole("dialog", { name: "Filters and sort" })).toBeTruthy();
  expect(location.pathname).toBe("/");
  history.back();
  await screen.findByRole("main", { name: "Imports" });
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  expect(await screen.findByRole("dialog", { name: "Filters and sort" })).toBeTruthy();
});

it("keeps Imports through a Documents and Photos round trip", async () => {
  start("/imports");
  await screen.findByRole("main", { name: "Imports" });
  await fireEvent.click(screen.getByRole("button", { name: "Photos" }));
  expect(location.pathname).toBe("/photos");
  await fireEvent.click(screen.getByRole("button", { name: "Documents" }));
  expect(await screen.findByRole("main", { name: "Imports" })).toBeTruthy();
  expect(location.pathname).toBe("/imports");
});

it("seeds Run report from the current search", async () => {
  start("/searches");
  const main = await screen.findByRole("main", { name: "Searches" });
  expect(within(main).getByRole("heading", { name: "Reports", level: 2 })).toBeTruthy();
  const search = screen.getByRole("searchbox", { name: "Search documents" });
  await fireEvent.input(search, { target: { value: "alpha" } });
  await fireEvent.click(screen.getByRole("button", { name: "Run report" }));
  expect(screen.getByRole("textbox", { name: "Expression for term 1" })).toHaveProperty(
    "value",
    "alpha",
  );
});
