import { afterEach, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
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
  history.replaceState(null, "", "/#web_session=short-lived");
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
  const json = (value: unknown) =>
    new Response(JSON.stringify(value), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  let tagCatalogReads = 0;
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1/children?limit=1000&offset=0") {
      return json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
    }
    if (url === "/api/v1/tags?limit=1000&offset=0") {
      tagCatalogReads += 1;
      return json({
        items: [
          {
            id: "33333333-3333-4333-8333-333333333333",
            name: tagCatalogReads === 1 ? "tax" : "tax records",
            revision: 1,
            assignment_count: tagCatalogReads === 1 ? 1 : 2,
          },
        ],
        total: 1,
        limit: 1000,
        offset: 0,
      });
    }
    if (url === "/api/v1/search?q=quarterly&limit=1000") return unfiltered;
    if (
      url ===
      "/api/v1/search?q=quarterly&limit=1000&tag_id=33333333-3333-4333-8333-333333333333"
    ) {
      return json({
        hits: [{ node: taxReport, path: "/Reports/quarterly-tax-report.txt", match: "name" }],
        limit: 1000,
        truncated: false,
        tag_id: "33333333-3333-4333-8333-333333333333",
      });
    }
    if (url === "/api/v1/audit/status?node_id=3") {
      return json({ enabled: false, scopes: [] });
    }
    if (url === "/api/v1/nodes/3/tags?limit=1000&offset=0") {
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    }
    throw new Error(`unexpected request: ${url}`);
  });

  render(App);
  const input = await screen.findByRole("searchbox", { name: "Search documents" });
  await screen.findByRole("combobox", { name: "Filter search by tag: All tags" });
  await fireEvent.input(input, { target: { value: "quarterly" } });
  await fireEvent.submit(input.closest("form")!);
  await waitFor(() =>
    expect(fetchMock.mock.calls.some(([url]) => String(url).includes("q=quarterly"))).toBe(true),
  );

  await fireEvent.click(
    screen.getByRole("combobox", { name: "Filter search by tag: All tags" }),
  );
  await fireEvent.click(screen.getByRole("option", { name: "tax (1)" }));
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

  await fireEvent.click(screen.getByRole("button", { name: "Refresh current view" }));
  expect(
    await screen.findByRole("combobox", { name: "Filter search by tag: tax records" }),
  ).toBeTruthy();
  expect(screen.getByText("“quarterly” · tax records")).toBeTruthy();
});
