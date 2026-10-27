import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import App from "./App.svelte";
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

function start(path = "/", fragment = "") {
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
    if (url.startsWith("/api/v1/packages?"))
      return Response.json({
        items: [
          {
            package_id: "review",
            package_name: "Review set",
            direction: "received",
            state: "complete",
            page_count: 0,
            member_count: 1,
          },
        ],
        limit: 250,
      });
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
  await fireEvent.keyDown(window, { key: "/" });
  expect(document.activeElement).toBe(screen.getByRole("searchbox", { name: "Search documents" }));
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

it("opens Trash directly", async () => {
  start("/trash");
  expect(await screen.findByRole("main", { name: "Trash" })).toBeTruthy();
  expect(
    screen.getByRole("button", { name: "Recoverable trash" }).getAttribute("aria-current"),
  ).toBe("page");
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

it("shows saved searches, report creation, and recent reports together", async () => {
  const fetch = start();
  const original = fetch.getMockImplementation()!;
  fetch.mockImplementation(async (input, init) => {
    if (String(input) === "/api/v1/saved-queries?limit=100&offset=0") return savedQueriesPage();
    if (String(input).startsWith("/api/v1/search-exports?"))
      return Response.json({
        items: [
          {
            summary: { id: "report", observed_at: "2026-09-20T12:00:00Z", state: "complete" },
            request: {
              version: 1,
              all_documents: true,
              timezone: "UTC",
              coverage_mode: "strict",
              terms: [
                {
                  number: 1,
                  expression: "alpha",
                  syntax: "simple",
                  dates: { start: "2000-01-01", end: "2026-09-20" },
                },
              ],
            },
          },
        ],
        total: 1,
      });
    return original(input, init);
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Searches" }));
  await screen.findByText("Alpha documents");
  expect(await screen.findByRole("heading", { name: "Recent reports" })).toBeTruthy();
  expect(screen.queryByRole("textbox", { name: "Expression for term 1" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Run report" }));
  expect(screen.getByRole("button", { name: "Create report" })).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  expect(screen.queryByRole("textbox", { name: "Expression for term 1" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Use as draft" }));
  expect(
    (screen.getByRole("textbox", { name: "Expression for term 1" }) as HTMLInputElement).value,
  ).toBe("alpha");
});

it("stays on Trash when restoring reruns an active saved query", async () => {
  const fetch = start();
  const original = fetch.getMockImplementation()!;
  const trashed = {
    ...root,
    id: 42,
    name: "Reports",
    path: "/Reports",
    trashed_at: "2026-09-20T12:00:00Z",
  };
  let restored = false;
  let queries = 0;
  fetch.mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/v1/saved-queries?limit=100&offset=0") return savedQueriesPage();
    if (url === "/api/v1/workspace/queries" && init?.method === "POST") {
      queries++;
      return Response.json(await snapshotEnvelope(query, []));
    }
    if (url.startsWith("/api/v1/trash?"))
      return Response.json({
        items: restored ? [] : [trashed],
        total: restored ? 0 : 1,
        limit: 1000,
        offset: 0,
      });
    if (url === "/api/v1/nodes/42/restore") {
      restored = true;
      return Response.json({ ...trashed, trashed_at: undefined, revision: 2 });
    }
    return original(input, init);
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Searches" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Run Alpha documents" }));
  await waitFor(() => expect(queries).toBe(1));
  await fireEvent.click(screen.getByRole("button", { name: "Recoverable trash" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Restore" }));
  await waitFor(() => expect(queries).toBe(2));
  expect(location.pathname).toBe("/trash");
  expect(screen.getByRole("main", { name: "Trash" })).toBeTruthy();
  await waitFor(() => expect(screen.queryByRole("button", { name: "Restore" })).toBeNull());
});
