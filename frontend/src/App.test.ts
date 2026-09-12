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
  let filteredSearches = 0;
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
                {
                  id: "11111111-1111-4111-8111-111111111111",
                  name: "matter/acme/reviewed",
                  revision: 1,
                  assignment_count: 6,
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
        total: tagCatalogReads === 1 ? 2 : 1001,
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
      filteredSearches += 1;
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
  expect(screen.getByText("matter/acme")).toBeTruthy();
  const groupedOption = screen.getByRole("option", {
    name: "matter/acme/reviewed (6)",
  });
  expect(groupedOption.textContent).not.toContain("matter/acme/reviewed");
  expect(
    groupedOption.querySelector<HTMLElement>("[data-tag-swatch]")?.style
      .backgroundColor,
  ).toBe("rgb(188, 76, 0)");
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
  const filteredSearchesBeforeBack = filteredSearches;
  tagResolutionMode = "missing";
  await fireEvent.click(back);
  await waitFor(() =>
    expect(filteredSearches).toBe(filteredSearchesBeforeBack + 1),
  );
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

it.each(["browse", "search"] as const)(
  "reconciles the active tag %s across failed and superseded refreshes",
  async (view) => {
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
    const report = {
      id: 3,
      parent_id: 1,
      name: "quarterly-tax-report.txt",
      kind: "file",
      current_version_id: "11111111-1111-4111-8111-111111111111",
      blob_hash: "a".repeat(64),
      size: 74,
      mime_type: "text/plain",
      revision: 3,
      created_at: "2026-07-28T12:00:00Z",
      modified_at: "2026-07-28T12:00:00Z",
      path: "/quarterly-tax-report.txt",
    };
    const tax = {
      id: "33333333-3333-4333-8333-333333333333",
      name: "tax",
      revision: 1,
      assignment_count: 1,
    };
    const reviewed = {
      id: "44444444-4444-4444-8444-444444444444",
      name: "matter/acme/reviewed",
      revision: 1,
      assignment_count: 1,
    };
    const json = (value: unknown) =>
      new Response(JSON.stringify(value), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    let resolveRemovalSearch!: (response: Response) => void;
    const delayedRemovalSearch = new Promise<Response>((resolve) => {
      resolveRemovalSearch = resolve;
    });
    let resolveAddSearch!: (response: Response) => void;
    const delayedAddSearch = new Promise<Response>((resolve) => {
      resolveAddSearch = resolve;
    });
    let resolvePreMutationRefresh!: (response: Response) => void;
    const delayedPreMutationRefresh = new Promise<Response>((resolve) => {
      resolvePreMutationRefresh = resolve;
    });
    let assigned = true;
    let reviewedAssigned = true;
    let reviewedDeletes = 0;
    let browseReads = 0;
    let searchReads = 0;
    let targetAuditReads = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      const url = String(input);
      if (url === "/api/v1/path?path=%2F") return json(root);
      if (url === "/api/v1/nodes/1/children?limit=1000&offset=0") {
        return json({
          directory: root,
          items: [report],
          total: 1,
          limit: 1000,
          offset: 0,
        });
      }
      if (url === "/api/v1/tags?limit=1000&offset=0") {
        return json({
          items: [
            { ...tax, assignment_count: assigned ? 1 : 0 },
            { ...reviewed, assignment_count: reviewedAssigned ? 1 : 0 },
          ],
          total: 2,
          limit: 1000,
          offset: 0,
        });
      }
      if (
        url ===
        "/api/v1/tags/33333333-3333-4333-8333-333333333333/nodes?limit=1000&offset=0&live_only=true"
      ) {
        browseReads += 1;
        if (view === "browse" && browseReads === 2) {
          return delayedPreMutationRefresh;
        }
        if (view === "browse" && browseReads === 3) {
          return new Response(
            JSON.stringify({
              status: 503,
              detail: "tag view temporarily unavailable",
            }),
            {
              status: 503,
              headers: { "Content-Type": "application/problem+json" },
            },
          );
        }
        if (view === "browse" && browseReads === 4) {
          return new Response(
            JSON.stringify({
              status: 503,
              detail: "re-added tag view temporarily unavailable",
            }),
            {
              status: 503,
              headers: { "Content-Type": "application/problem+json" },
            },
          );
        }
        return json({
          items: assigned ? [{ node: report, path: report.path }] : [],
          total: assigned ? 1 : 0,
          limit: 1000,
          offset: 0,
          omitted_trashed: 0,
        });
      }
      if (
        url ===
        "/api/v1/search?q=quarterly&limit=1000&tag_id=33333333-3333-4333-8333-333333333333"
      ) {
        searchReads += 1;
        if (view === "search" && searchReads === 2) {
          return delayedPreMutationRefresh;
        }
        if (view === "search" && searchReads === 3) {
          return delayedRemovalSearch;
        }
        if (view === "search" && searchReads === 4) {
          return delayedAddSearch;
        }
        return json({
          hits: assigned
            ? [{ node: report, path: report.path, match: "name" }]
            : [],
          limit: 1000,
          truncated: false,
          tag_id: tax.id,
        });
      }
      if (url === "/api/v1/audit/status?node_id=3") {
        targetAuditReads += 1;
        return json({ enabled: false, scopes: [] });
      }
      if (url === "/api/v1/nodes/3/tags?limit=1000&offset=0") {
        const items = [
          ...(assigned ? [tax] : []),
          ...(reviewedAssigned ? [reviewed] : []),
        ];
        return json({
          items,
          total: items.length,
          limit: 1000,
          offset: 0,
        });
      }
      if (
        url ===
          "/api/v1/nodes/3/tags/33333333-3333-4333-8333-333333333333" &&
        init?.method === "DELETE"
      ) {
        assigned = false;
        return json({
          tag: { ...tax, revision: 2, assignment_count: 0 },
          node: {
            ...report,
            revision: 4,
            modified_at: "2026-07-28T12:01:00Z",
          },
          changed: true,
        });
      }
      if (
        url ===
          "/api/v1/nodes/3/tags/44444444-4444-4444-8444-444444444444" &&
        init?.method === "DELETE"
      ) {
        reviewedAssigned = false;
        reviewedDeletes += 1;
        return json({
          tag: { ...reviewed, revision: 2, assignment_count: 0 },
          node: {
            ...report,
            revision: 6,
            modified_at: "2026-07-28T12:03:00Z",
          },
          changed: true,
        });
      }
      if (
        url ===
          "/api/v1/nodes/3/tags/33333333-3333-4333-8333-333333333333" &&
        init?.method === "PUT"
      ) {
        assigned = true;
        return json({
          tag: { ...tax, revision: 3, assignment_count: 1 },
          node: {
            ...report,
            revision: 5,
            modified_at: "2026-07-28T12:02:00Z",
          },
          changed: true,
        });
      }
      throw new Error(`unexpected request: ${url}`);
    });

    render(App);
    await screen.findByRole("cell", { name: "quarterly-tax-report.txt" });
    await fireEvent.click(
      screen.getByRole("combobox", { name: "Browse or filter by tag: All tags" }),
    );
    await fireEvent.click(screen.getByRole("option", { name: "tax (1)" }));
    await screen.findByText("Documents tagged");

    if (view === "search") {
      const input = screen.getByRole("searchbox", { name: "Search documents" });
      await fireEvent.input(input, { target: { value: "quarterly" } });
      await fireEvent.submit(input.closest("form")!);
      await screen.findByText("Search results");
    }

    await screen.findByText("2 assigned");
    const reviewedName = screen.getByText("matter/acme/reviewed");
    const reviewedLabel = reviewedName.parentElement as HTMLElement;
    const reviewedVisual = reviewedLabel.querySelector<HTMLElement>(
      "[aria-hidden='true']",
    );
    expect(reviewedVisual?.textContent).toBe("reviewed");
    expect(
      (reviewedVisual?.firstElementChild as HTMLElement).style.backgroundColor,
    ).toBe("rgb(130, 80, 223)");
    await fireEvent.click(screen.getByRole("button", { name: "Refresh current view" }));
    const manage = screen.getByRole("button", { name: "Manage" });
    expect(manage.hasAttribute("disabled")).toBe(true);
    expect(screen.queryByRole("dialog", { name: /Manage tags/ })).toBeNull();
    resolvePreMutationRefresh(
      view === "search"
        ? json({
            hits: [{ node: report, path: report.path, match: "name" }],
            limit: 1000,
            truncated: false,
            tag_id: tax.id,
          })
        : json({
            items: [{ node: report, path: report.path }],
            total: 1,
            limit: 1000,
            offset: 0,
            omitted_trashed: 0,
          }),
    );
    await waitFor(() => expect(manage.hasAttribute("disabled")).toBe(false));
    await fireEvent.click(screen.getByRole("button", { name: "Manage" }));
    const auditReadsBeforeRemoval = targetAuditReads;
    await fireEvent.click(
      screen.getByRole("button", { name: "Remove tag tax" }),
    );

    await waitFor(() =>
      expect(
        screen.queryByRole("cell", { name: "/quarterly-tax-report.txt" }),
      ).toBeNull(),
    );

    if (view === "browse") {
      expect(await screen.findByText("tag view temporarily unavailable")).toBeTruthy();
      expect(browseReads).toBe(3);
      expect(targetAuditReads).toBe(auditReadsBeforeRemoval);
      const tagToAssign = screen.getByRole("combobox", {
        name: "Tag to assign: Choose a tag…",
      });
      await fireEvent.click(tagToAssign);
      await fireEvent.click(screen.getByRole("option", { name: "tax (0)" }));
      await fireEvent.click(screen.getByRole("button", { name: "Add tag" }));
      expect(
        await screen.findByRole("cell", { name: "/quarterly-tax-report.txt" }),
      ).toBeTruthy();
      expect(await screen.findByText("1 live shown")).toBeTruthy();
      expect(
        await screen.findByText("re-added tag view temporarily unavailable"),
      ).toBeTruthy();
      expect(browseReads).toBe(4);
      return;
    }

    await waitFor(() => expect(searchReads).toBe(3));
    await screen.findByText("Removed tax.");
    resolveRemovalSearch(
      json({
        hits: [],
        limit: 1000,
        truncated: false,
        tag_id: tax.id,
      }),
    );
    const tagToAssign = screen.getByRole("combobox", {
      name: "Tag to assign: Choose a tag…",
    });
    await waitFor(() =>
      expect(tagToAssign.hasAttribute("disabled")).toBe(false),
    );
    await fireEvent.click(
      tagToAssign,
    );
    await fireEvent.click(screen.getByRole("option", { name: "tax (0)" }));
    await fireEvent.click(screen.getByRole("button", { name: "Add tag" }));
    await waitFor(() => expect(searchReads).toBe(4));
    const removeReviewed = screen.getByRole("button", {
      name: "Remove tag matter/acme/reviewed",
    });
    expect(removeReviewed.hasAttribute("disabled")).toBe(true);
    await fireEvent.click(removeReviewed);
    expect(reviewedDeletes).toBe(0);

    resolveAddSearch(
      json({
        hits: [{ node: { ...report, revision: 5 }, path: report.path, match: "name" }],
        limit: 1000,
        truncated: false,
        tag_id: tax.id,
      }),
    );
    await waitFor(() =>
      expect(removeReviewed.hasAttribute("disabled")).toBe(false),
    );
    await fireEvent.click(removeReviewed);
    await waitFor(() => expect(reviewedDeletes).toBe(1));
    expect(
      await screen.findByRole("cell", { name: "/quarterly-tax-report.txt" }),
    ).toBeTruthy();
    await waitFor(() =>
      expect(screen.getByText("6", { selector: "dd" })).toBeTruthy(),
    );
  },
);

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

function selectionNode(
  id: number,
  name: string,
  kind: "dir" | "file",
  parentID: number | undefined,
  revision = 1,
) {
  return {
    id,
    parent_id: parentID,
    name,
    kind,
    size: kind === "file" ? id * 10 : 0,
    mime_type: kind === "file" ? "text/plain" : undefined,
    current_version_id:
      kind === "file" ? `${String(id).padStart(8, "0")}-1111-4111-8111-111111111111` : undefined,
    blob_hash: kind === "file" ? id.toString(16).repeat(64).slice(0, 64) : undefined,
    revision,
    created_at: "2026-09-09T00:00:00Z",
    modified_at: "2026-09-09T00:00:00Z",
    path: kind === "dir" ? (id === 1 ? "/" : `/${name}`) : undefined,
  };
}

function installSelectionBackend(rootHasFiles = true) {
  const root = selectionNode(1, "", "dir", undefined);
  const reports = selectionNode(2, "Reports", "dir", 1);
  const rootFile = selectionNode(3, "readme.txt", "file", 1);
  const alpha = selectionNode(10, "alpha.txt", "file", 2);
  const beta = selectionNode(11, "beta.txt", "file", 2);
  const gamma = selectionNode(12, "gamma.txt", "file", 2);
  const tax = {
    id: "33333333-3333-4333-8333-333333333333",
    name: "tax",
    revision: 1,
    assignment_count: 3,
  };
  let reportsReads = 0;
  const json = (value: unknown, status = 200) =>
    new Response(JSON.stringify(value), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(
    async (input, init) => {
      const url = String(input);
      if (url === "/api/v1/path?path=%2F") return json(root);
      if (url === "/api/v1/nodes/1/children?limit=1000&offset=0") {
        return json({
          directory: root,
          items: rootHasFiles ? [reports, rootFile] : [reports],
          total: rootHasFiles ? 2 : 1,
          limit: 1000,
          offset: 0,
        });
      }
      if (url === "/api/v1/nodes/2/children?limit=1000&offset=0") {
        reportsReads += 1;
        return json({
          directory: reports,
          items:
            reportsReads === 1
              ? [alpha, beta, gamma]
              : [{ ...alpha, revision: 7 }, gamma],
          total: reportsReads === 1 ? 3 : 2,
          limit: 1000,
          offset: 0,
        });
      }
      if (url === "/api/v1/tags?limit=1000&offset=0") {
        return json({ items: [tax], total: 1, limit: 1000, offset: 0 });
      }
      if (url.startsWith("/api/v1/audit/status?node_id=")) {
        return json({ enabled: false, scopes: [] });
      }
      if (/^\/api\/v1\/nodes\/\d+\/tags\?limit=1000&offset=0$/.test(url)) {
        return json({ items: [], total: 0, limit: 1000, offset: 0 });
      }
      if (url === `/api/v1/tags/${tax.id}/nodes?limit=1000&offset=0&live_only=true`) {
        return json({
          items: [reports, alpha, rootFile].map((node) => ({
            node,
            path: node.path ?? (node.parent_id === 1 ? `/${node.name}` : `/Reports/${node.name}`),
          })),
          total: 3,
          limit: 1000,
          offset: 0,
        });
      }
      if (url.startsWith("/api/v1/search?")) {
        const requestedTag = new URL(`https://docbank.local${url}`).searchParams.get(
          "tag_id",
        );
        return json({
          hits: [
            {
              node: { ...alpha, revision: requestedTag ? 9 : 8 },
              path: "/Reports/alpha.txt",
              match: "name",
            },
          ],
          limit: 1000,
          truncated: false,
          tag_id: requestedTag ?? undefined,
        });
      }
      if (url === "/api/daemon/web-session" && init?.method === "DELETE") {
        return new Response(null, { status: 204 });
      }
      throw new Error(`unexpected request: ${url}`);
    },
  );
  return { fetchMock, getReportsReads: () => reportsReads };
}

function prepareSelectionApp(): void {
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
}

it("opens bounded tag assignment for the exact page selection", async () => {
  prepareSelectionApp();
  installSelectionBackend();
  render(App);
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select readme.txt" }));
  await fireEvent.click(screen.getByRole("button", { name: "Edit tags" }));
  const dialog = await screen.findByRole("dialog", { name: "Tag selected documents" });
  expect(within(dialog).getByText(/1 selected document\./)).toBeTruthy();
  expect((within(dialog).getByRole("button", { name: "Add to all" }) as HTMLButtonElement).disabled).toBe(true);
  await fireEvent.click(within(dialog).getByRole("button", { name: "Done" }));
  expect(screen.queryByRole("dialog", { name: "Tag selected documents" })).toBeNull();
});

it("selects displayed files without requests or changing the inspector, then reconciles a refresh", async () => {
  prepareSelectionApp();
  const { fetchMock, getReportsReads } = installSelectionBackend();
  render(App);

  await fireEvent.dblClick(await screen.findByRole("cell", { name: "Reports" }));
  await screen.findByRole("checkbox", { name: "Select alpha.txt" });
  await waitFor(() => expect(getReportsReads()).toBe(1));
  const requestsBeforeSelection = fetchMock.mock.calls.length;

  const selectVisible = screen.getByRole("checkbox", {
    name: "Select visible documents",
  });
  await fireEvent.click(selectVisible);
  expect(screen.getByText("3 selected on this page")).toBeTruthy();
  await fireEvent.click(selectVisible);
  expect(screen.queryByText(/selected on this page/)).toBeNull();
  expect(fetchMock.mock.calls).toHaveLength(requestsBeforeSelection);

  const alpha = screen.getByRole("checkbox", { name: "Select alpha.txt" });
  const beta = screen.getByRole("checkbox", { name: "Select beta.txt" });
  await fireEvent.click(alpha);
  beta.focus();
  await fireEvent.click(beta, { shiftKey: true });

  expect(fetchMock.mock.calls).toHaveLength(requestsBeforeSelection);
  expect(screen.getByLabelText("Document authority for alpha.txt")).toBeTruthy();
  expect(screen.getByText("2 selected on this page")).toBeTruthy();
  expect(document.activeElement).toBe(beta);

  await fireEvent.click(screen.getByRole("button", { name: "Refresh current view" }));
  await waitFor(() => expect(getReportsReads()).toBe(2));
  expect(await screen.findByText("1 selected on this page")).toBeTruthy();
  expect(screen.queryByRole("checkbox", { name: "Select beta.txt" })).toBeNull();

  const requestsAfterRefresh = fetchMock.mock.calls.length;
  const gamma = screen.getByRole("checkbox", { name: "Select gamma.txt" });
  gamma.focus();
  await fireEvent.click(gamma, { shiftKey: true });
  expect(screen.getByText("2 selected on this page")).toBeTruthy();
  expect(document.activeElement).toBe(gamma);
  expect(fetchMock.mock.calls).toHaveLength(requestsAfterRefresh);
  await fireEvent.click(gamma);

  expect(screen.getByText("1 selected on this page")).toBeTruthy();
});

it("clears page selection across folder, Back, query, tag-filter, and session transitions", async () => {
  prepareSelectionApp();
  installSelectionBackend();
  render(App);

  await screen.findByRole("checkbox", { name: "Select readme.txt" });
  await fireEvent.click(
    screen.getByRole("checkbox", { name: "Select readme.txt" }),
  );
  expect(screen.getByText("1 selected on this page")).toBeTruthy();

  await fireEvent.dblClick(screen.getByRole("cell", { name: "Reports" }));
  await screen.findByRole("checkbox", { name: "Select alpha.txt" });
  expect(screen.queryByText(/selected on this page/)).toBeNull();

  await fireEvent.click(
    screen.getByRole("checkbox", { name: "Select alpha.txt" }),
  );
  await fireEvent.click(
    screen.getByRole("button", { name: "Back to previous directory" }),
  );
  await screen.findByRole("checkbox", { name: "Select readme.txt" });
  expect(screen.queryByText(/selected on this page/)).toBeNull();

  await fireEvent.click(
    screen.getByRole("checkbox", { name: "Select readme.txt" }),
  );
  const search = screen.getByRole("searchbox", { name: "Search documents" });
  await fireEvent.input(search, { target: { value: "alpha" } });
  await fireEvent.submit(search.closest("form")!);
  await screen.findByRole("cell", { name: "/Reports/alpha.txt" });
  expect(screen.queryByText(/selected on this page/)).toBeNull();

  await fireEvent.click(
    await screen.findByRole("checkbox", { name: "Select /Reports/alpha.txt" }),
  );
  await fireEvent.click(
    screen.getByRole("combobox", { name: "Browse or filter by tag: All tags" }),
  );
  await fireEvent.click(screen.getByRole("option", { name: "tax (3)" }));
  await waitFor(() => expect(screen.queryByText(/selected on this page/)).toBeNull());

  await fireEvent.click(
    await screen.findByRole("checkbox", { name: "Select /Reports/alpha.txt" }),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Lock web session" }));
  expect(await screen.findByText("Open your Docbank")).toBeTruthy();
  expect(screen.queryByText(/selected on this page/)).toBeNull();
});

it("restores the tag view sort when returning from a tagged folder", async () => {
  prepareSelectionApp();
  installSelectionBackend();
  render(App);
  await screen.findByRole("cell", { name: "Reports" });
  await fireEvent.click(screen.getByRole("combobox", { name: "Browse or filter by tag: All tags" }));
  await fireEvent.click(screen.getByRole("option", { name: "tax (3)" }));
  await screen.findByRole("cell", { name: "/Reports/alpha.txt" });
  await fireEvent.click(within(screen.getByRole("columnheader", { name: "Size" })).getByRole("button"));
  expect(screen.getByRole("columnheader", { name: /Size/ }).getAttribute("aria-sort")).toBe("descending");
  await fireEvent.dblClick(screen.getByRole("cell", { name: "/Reports" }));
  await screen.findByRole("cell", { name: "alpha.txt" });
  await fireEvent.click(screen.getByRole("button", { name: "Back to previous directory" }));
  await screen.findByRole("cell", { name: "/Reports/alpha.txt" });
  expect(screen.getByRole("columnheader", { name: /Size/ }).getAttribute("aria-sort")).toBe("descending");
});

it("disables select-all in a folder containing only folders", async () => {
  prepareSelectionApp();
  installSelectionBackend(false);
  render(App);
  await screen.findByRole("cell", { name: "Reports" });
  const checkbox = screen.getByRole("checkbox", { name: "Select visible documents" }) as HTMLInputElement;
  checkbox.click();
  expect(checkbox.checked).toBe(false);
  expect(checkbox.disabled).toBe(true);
});

it.each(["folder", "query", "tag"])("keeps selection when loading a new %s fails", async (destination) => {
  prepareSelectionApp();
  const { fetchMock } = installSelectionBackend();
  render(App);
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select readme.txt" }));
  const backend = fetchMock.getMockImplementation()!;
  fetchMock.mockImplementation(async (input, init) => {
    const url = String(input);
    if (url.includes("/nodes/2/children?") || url.startsWith("/api/v1/search?") || url.endsWith("&live_only=true")) {
      return new Response(JSON.stringify({ detail: "temporarily unavailable" }), { status: 503 });
    }
    return backend(input, init);
  });
  if (destination === "folder") {
    await fireEvent.dblClick(screen.getByRole("cell", { name: "Reports" }));
  } else if (destination === "query") {
    const search = screen.getByRole("searchbox", { name: "Search documents" });
    await fireEvent.input(search, { target: { value: "alpha" } });
    await fireEvent.submit(search.closest("form")!);
  } else {
    await fireEvent.click(screen.getByRole("combobox", { name: "Browse or filter by tag: All tags" }));
    await fireEvent.click(screen.getByRole("option", { name: "tax (3)" }));
  }
  await screen.findByText("temporarily unavailable");
  expect((screen.getByRole("checkbox", { name: "Select readme.txt" }) as HTMLInputElement).checked).toBe(true);
  expect(screen.getByText("1 selected on this page")).toBeTruthy();
});

it("does not carry Shift from an abandoned gesture into a later toggle", async () => {
  prepareSelectionApp();
  installSelectionBackend();
  render(App);
  await fireEvent.dblClick(await screen.findByRole("cell", { name: "Reports" }));
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select alpha.txt" }));
  const gamma = screen.getByRole("checkbox", { name: "Select gamma.txt" });
  await fireEvent.pointerDown(gamma, { shiftKey: true });
  await fireEvent.pointerUp(gamma.closest("td")!);
  await fireEvent.click(gamma);
  expect((screen.getByRole("checkbox", { name: "Select beta.txt" }) as HTMLInputElement).checked).toBe(false);
  expect(screen.getByText("2 selected on this page")).toBeTruthy();
});

it("retains tag selection on refresh and clears it when the tag disappears", async () => {
  prepareSelectionApp();
  const { fetchMock } = installSelectionBackend();
  render(App);
  await screen.findByRole("cell", { name: "Reports" });
  await fireEvent.click(screen.getByRole("combobox", { name: "Browse or filter by tag: All tags" }));
  await fireEvent.click(screen.getByRole("option", { name: "tax (3)" }));
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select /readme.txt" }));
  await fireEvent.click(screen.getByRole("button", { name: "Refresh current view" }));
  await screen.findByRole("checkbox", { name: "Select /readme.txt" });
  expect(screen.getByText("1 selected on this page")).toBeTruthy();

  const backend = fetchMock.getMockImplementation()!;
  fetchMock.mockImplementation(async (input, init) => {
    if (String(input) === "/api/v1/tags?limit=1000&offset=0") {
      return new Response(JSON.stringify({ items: [], total: 0, limit: 1000, offset: 0 }));
    }
    return backend(input, init);
  });
  await fireEvent.click(screen.getByRole("button", { name: "Refresh current view" }));
  await screen.findByRole("cell", { name: "readme.txt" });
  expect(screen.queryByText(/selected on this page/)).toBeNull();
});
