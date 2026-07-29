import { afterEach, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/svelte";
import App from "./App.svelte";

afterEach(() => {
  cleanup();
  history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
  vi.restoreAllMocks();
});

it("supersedes an in-flight search when its tag filter changes", async () => {
  history.replaceState(
    null,
    "",
    "/#web_session=short-lived&web_upload_secret=proof",
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
  let resolveUnfiltered!: (response: Response) => void;
  const unfiltered = new Promise<Response>((resolve) => {
    resolveUnfiltered = resolve;
  });
  let resolveRoot!: (response: Response) => void;
  const initialRoot = new Promise<Response>((resolve) => {
    resolveRoot = resolve;
  });
  let resolveInitialTagBrowse!: (response: Response) => void;
  const initialTagBrowse = new Promise<Response>((resolve) => {
    resolveInitialTagBrowse = resolve;
  });
  const root = {
    id: 1,
    name: "",
    kind: "dir",
    size: 0,
    revision: 1,
    created_at: "2026-07-27T12:00:00Z",
    modified_at: "2026-07-27T12:00:00Z",
    path: "/",
  };
  const taxReport = {
    id: 3,
    parent_id: 2,
    name: "quarterly-tax-report.txt",
    kind: "file",
    current_version_id: "11111111-1111-4111-8111-111111111111",
    blob_hash: "a".repeat(64),
    size: 74,
    mime_type: "text/plain",
    revision: 3,
    created_at: "2026-07-27T12:00:00Z",
    modified_at: "2026-07-27T12:00:00Z",
  };
  const productReport = {
    ...taxReport,
    id: 4,
    name: "quarterly-product-report.txt",
    current_version_id: "22222222-2222-4222-8222-222222222222",
    blob_hash: "b".repeat(64),
  };
  const archiveDirectory = {
    id: 5,
    parent_id: 1,
    name: "Archive",
    kind: "dir",
    size: 0,
    revision: 1,
    created_at: "2026-07-27T12:00:00Z",
    modified_at: "2026-07-27T12:00:00Z",
    path: "/Archive",
  };
  const json = (value: unknown) =>
    new Response(JSON.stringify(value), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  let tagCatalogReads = 0;
  let tagResolutionMode: "browse" | "renamed" | "missing" = "browse";
  let failNextTagBrowse = false;
  let renamedTagResolved = false;
  let missingTagResolved = false;
  let tagBrowseReads = 0;
  let unfilteredSearches = 0;
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url === "/api/v1/path?path=%2F") return initialRoot;
    if (url === "/api/v1/nodes/1/children?limit=1000&offset=0") {
      return json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
    }
    if (url === "/api/v1/nodes/5/children?limit=1000&offset=0") {
      return json({
        directory: archiveDirectory,
        items: [],
        total: 0,
        limit: 1000,
        offset: 0,
      });
    }
    if (url === "/api/v1/tags?limit=1000&offset=0") {
      tagCatalogReads += 1;
      return json({
        items:
          tagCatalogReads === 1
            ? [
                {
                  id: "33333333-3333-4333-8333-333333333333",
                  name: "tax",
                  revision: 1,
                  assignment_count: 3,
                },
              ]
            : [
                {
                  id: "44444444-4444-4444-8444-444444444444",
                  name: "reviewed",
                  revision: 1,
                  assignment_count: 7,
                },
              ],
        total: tagCatalogReads === 1 ? 1 : 1001,
        limit: 1000,
        offset: 0,
      });
    }
    if (url === "/api/v1/tags/33333333-3333-4333-8333-333333333333") {
      if (tagResolutionMode === "missing") {
        missingTagResolved = true;
        return new Response(
          JSON.stringify({ status: 404, detail: "tag not found" }),
          {
            status: 404,
            headers: { "Content-Type": "application/json" },
          },
        );
      }
      if (tagResolutionMode === "renamed") {
        renamedTagResolved = true;
        return json({
          id: "33333333-3333-4333-8333-333333333333",
          name: "tax records",
          revision: 3,
          assignment_count: 3,
        });
      }
      return json({
        id: "33333333-3333-4333-8333-333333333333",
        name: "tax",
        revision: 1,
        assignment_count: 3,
      });
    }
    if (
      url ===
      "/api/v1/tags/33333333-3333-4333-8333-333333333333/nodes?limit=1000&offset=0&live_only=true"
    ) {
      tagBrowseReads += 1;
      if (tagBrowseReads === 1) return initialTagBrowse;
      if (failNextTagBrowse) {
        failNextTagBrowse = false;
        return new Response(
          JSON.stringify({ status: 503, detail: "tag catalog temporarily unavailable" }),
          {
            status: 503,
            headers: { "Content-Type": "application/json" },
          },
        );
      }
      return json({
        items: [{ node: taxReport, path: "/Reports/quarterly-tax-report.txt" }],
        total: 1,
        limit: 1000,
        offset: 0,
        omitted_trashed: 2,
      });
    }
    if (url === "/api/v1/search?q=quarterly&limit=1000") {
      unfilteredSearches += 1;
      if (unfilteredSearches === 1) return unfiltered;
      return json({
        hits: [
          {
            node: productReport,
            path: "/Reports/quarterly-product-report.txt",
            match: "name",
          },
        ],
        limit: 1000,
        truncated: false,
      });
    }
    if (
      url ===
      "/api/v1/search?q=quarterly&limit=1000&tag_id=33333333-3333-4333-8333-333333333333"
    ) {
      return json({
        hits: [
          { node: taxReport, path: "/Reports/quarterly-tax-report.txt", match: "name" },
          { node: archiveDirectory, path: "/Archive", match: "name" },
        ],
        limit: 1000,
        truncated: false,
        tag_id: "33333333-3333-4333-8333-333333333333",
      });
    }
    if (url === "/api/v1/audit/status?node_id=3" || url === "/api/v1/audit/status?node_id=5") {
      return json({ enabled: false, scopes: [] });
    }
    if (
      url === "/api/v1/nodes/3/tags?limit=1000&offset=0" ||
      url === "/api/v1/nodes/5/tags?limit=1000&offset=0"
    ) {
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    }
    throw new Error(`unexpected request: ${url}`);
  });

  render(App);
  const input = await screen.findByRole("searchbox", { name: "Search documents" });
  const tagSelector = await screen.findByRole("combobox", {
    name: "Browse or filter by tag: All tags",
  });
  expect(tagSelector.hasAttribute("disabled")).toBe(true);
  resolveRoot(json(root));
  await screen.findByText("This folder is empty");
  expect(tagSelector.hasAttribute("disabled")).toBe(false);

  await fireEvent.click(
    screen.getByRole("combobox", { name: "Browse or filter by tag: All tags" }),
  );
  await fireEvent.click(screen.getByRole("option", { name: "tax (3)" }));
  await fireEvent.click(
    screen.getByRole("combobox", { name: "Browse or filter by tag: tax" }),
  );
  await fireEvent.click(screen.getByRole("option", { name: "All tags" }));
  await screen.findByText("This folder is empty");
  resolveInitialTagBrowse(
    json({
      items: [{ node: taxReport, path: "/Reports/quarterly-tax-report.txt" }],
      total: 1,
      limit: 1000,
      offset: 0,
      omitted_trashed: 2,
    }),
  );
  await waitFor(() => expect(screen.queryByText("Documents tagged")).toBeNull());

  failNextTagBrowse = true;
  await fireEvent.click(
    screen.getByRole("combobox", { name: "Browse or filter by tag: All tags" }),
  );
  await fireEvent.click(screen.getByRole("option", { name: "tax (3)" }));
  expect(await screen.findByText("tag catalog temporarily unavailable")).toBeTruthy();
  expect(
    screen.getByRole("combobox", { name: "Browse or filter by tag: All tags" }),
  ).toBeTruthy();
  expect(screen.getByText("This folder is empty")).toBeTruthy();

  await fireEvent.click(
    screen.getByRole("combobox", { name: "Browse or filter by tag: All tags" }),
  );
  await fireEvent.click(screen.getByRole("option", { name: "tax (3)" }));
  expect(await screen.findByText("Documents tagged")).toBeTruthy();
  expect(screen.getByText("tax", { selector: "strong" })).toBeTruthy();
  expect(
    screen.getByRole("cell", { name: "/Reports/quarterly-tax-report.txt" }),
  ).toBeTruthy();
  expect(screen.queryByText("superseded-tax-report.txt")).toBeNull();
  expect(screen.getByText(/1 live shown · 2 trashed omitted/)).toBeTruthy();

  await fireEvent.click(
    screen.getByRole("combobox", { name: "Browse or filter by tag: tax" }),
  );
  await fireEvent.click(screen.getByRole("option", { name: "All tags" }));
  await screen.findByText("This folder is empty");

  await fireEvent.input(input, { target: { value: "quarterly" } });
  await fireEvent.submit(input.closest("form")!);
  await waitFor(() =>
    expect(fetchMock.mock.calls.some(([url]) => String(url).includes("q=quarterly"))).toBe(true),
  );

  await fireEvent.click(
    screen.getByRole("combobox", { name: "Browse or filter by tag: All tags" }),
  );
  await fireEvent.click(screen.getByRole("option", { name: "tax (3)" }));
  expect(
    await screen.findByRole("cell", { name: "/Reports/quarterly-tax-report.txt" }),
  ).toBeTruthy();

  resolveUnfiltered(
    json({
      hits: [
        {
          node: productReport,
          path: "/Reports/quarterly-product-report.txt",
          match: "name",
        },
      ],
      limit: 1000,
      truncated: false,
    }),
  );
  await waitFor(() =>
    expect(
      screen.queryByRole("cell", { name: "/Reports/quarterly-product-report.txt" }),
    ).toBeNull(),
  );

  tagResolutionMode = "renamed";
  await fireEvent.click(screen.getByRole("button", { name: "Refresh current view" }));
  await waitFor(() =>
    expect(fetchMock.mock.calls.map(([url]) => String(url))).toContain(
      "/api/v1/tags/33333333-3333-4333-8333-333333333333",
    ),
  );
  await waitFor(() =>
    expect(screen.getByRole("combobox").getAttribute("aria-label")).toBe(
      "Browse or filter: showing 1 of 1001 tags: tax records",
    ),
  );
  expect(renamedTagResolved).toBe(true);
  expect(screen.getByText("“quarterly” · tax records")).toBeTruthy();

  await fireEvent.dblClick(screen.getByRole("cell", { name: "/Archive" }));
  expect(await screen.findByText("This folder is empty")).toBeTruthy();
  const searchCalls = fetchMock.mock.calls.filter(([url]) =>
    String(url).startsWith("/api/v1/search?"),
  ).length;
  await fireEvent.click(
    screen.getByRole("combobox", {
      name: "Browse or filter: showing 1 of 1001 tags: tax records",
    }),
  );
  await fireEvent.click(screen.getByRole("option", { name: "All tags" }));
  expect(screen.getByText("Current folder")).toBeTruthy();
  expect(
    fetchMock.mock.calls.filter(([url]) => String(url).startsWith("/api/v1/search?")),
  ).toHaveLength(searchCalls);

  const back = screen.getByRole("button", { name: "Back to previous directory" });
  expect(back.hasAttribute("disabled")).toBe(false);
  tagResolutionMode = "missing";
  await fireEvent.click(back);
  await waitFor(() => expect(tagCatalogReads).toBe(3));
  expect(screen.getByRole("combobox").getAttribute("aria-label")).toContain("tax records");
  await waitFor(() => expect(missingTagResolved).toBe(true));
  await waitFor(() => expect(unfilteredSearches).toBe(2));
  expect(
    await screen.findByRole("cell", { name: "/Reports/quarterly-product-report.txt" }),
  ).toBeTruthy();
  expect(screen.getByText("“quarterly”")).toBeTruthy();
  expect(screen.queryByText("“quarterly” · tax records")).toBeNull();
  expect(
    screen.getByRole("combobox", {
      name: "Browse or filter: showing 1 of 1001 tags: All tags",
    }),
  ).toBeTruthy();
});

it("returns to root when a nested child is trashed and refresh fails", async () => {
  history.replaceState(
    null,
    "",
    "/#web_session=short-lived&web_upload_secret=proof",
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

  const root = {
    id: 1,
    name: "",
    kind: "dir",
    size: 0,
    revision: 1,
    created_at: "2026-07-28T12:00:00Z",
    modified_at: "2026-07-28T12:00:00Z",
    path: "/",
  };
  const reports = {
    id: 2,
    parent_id: 1,
    name: "Reports",
    kind: "dir",
    size: 0,
    revision: 1,
    created_at: "2026-07-28T12:00:00Z",
    modified_at: "2026-07-28T12:00:00Z",
    path: "/Reports",
  };
  const quarterlyReport = {
    id: 3,
    parent_id: 2,
    name: "quarterly-report.txt",
    kind: "file",
    current_version_id: "11111111-1111-4111-8111-111111111111",
    blob_hash: "a".repeat(64),
    size: 74,
    mime_type: "text/plain",
    revision: 1,
    created_at: "2026-07-28T12:00:00Z",
    modified_at: "2026-07-28T12:00:00Z",
    path: "/Reports/quarterly-report.txt",
  };
  const json = (value: unknown) =>
    new Response(JSON.stringify(value), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  let trashed = false;
  let failRootRefresh = true;
  let rootReads = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1/children?limit=1000&offset=0") {
      rootReads += 1;
      if (trashed && failRootRefresh) {
        failRootRefresh = false;
        return new Response(
          JSON.stringify({
            status: 503,
            detail: "root temporarily unavailable",
          }),
          {
            status: 503,
            headers: { "Content-Type": "application/problem+json" },
          },
        );
      }
      return json({
        directory: root,
        items: [reports],
        total: 1,
        limit: 1000,
        offset: 0,
      });
    }
    if (url === "/api/v1/nodes/2/children?limit=1000&offset=0") {
      return json({
        directory: reports,
        items: trashed ? [] : [quarterlyReport],
        total: trashed ? 0 : 1,
        limit: 1000,
        offset: 0,
      });
    }
    if (url === "/api/v1/tags?limit=1000&offset=0") {
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    }
    if (url === "/api/v1/audit/status?node_id=3") {
      return json({ enabled: false, scopes: [] });
    }
    if (url === "/api/v1/nodes/3/tags?limit=1000&offset=0") {
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    }
    if (url === "/api/v1/nodes/3/trash" && init?.method === "POST") {
      trashed = true;
      return json({
        ...quarterlyReport,
        revision: 2,
        trashed_at: "2026-07-28T12:01:00Z",
      });
    }
    throw new Error(`unexpected request: ${url}`);
  });

  render(App);
  await fireEvent.dblClick(await screen.findByRole("cell", { name: "Reports" }));
  await screen.findByRole("cell", { name: "quarterly-report.txt" });

  await fireEvent.click(screen.getByRole("button", { name: "Move to trash" }));
  const confirmation = screen.getByRole("dialog", {
    name: "Move quarterly-report.txt to trash",
  });
  await fireEvent.click(
    within(confirmation).getByRole("button", { name: "Move to trash" }),
  );

  await waitFor(() => expect(rootReads).toBe(2));
  expect(await screen.findByText("root temporarily unavailable")).toBeTruthy();
  expect(screen.queryByRole("cell", { name: "quarterly-report.txt" })).toBeNull();
  expect(
    screen.getByRole("button", { name: "Back to previous directory" }).hasAttribute(
      "disabled",
    ),
  ).toBe(true);

  await fireEvent.click(screen.getByRole("button", { name: "Refresh current view" }));
  await waitFor(() => expect(rootReads).toBe(3));
  expect(await screen.findByRole("cell", { name: "Reports" })).toBeTruthy();
  expect(screen.queryByRole("cell", { name: "quarterly-report.txt" })).toBeNull();
});
