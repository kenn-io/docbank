import * as actions from "./action-events.js";
import { createHash } from "node:crypto";
import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import App from "./App.svelte";
import { canonicalQuery, highlightSetFingerprint, type HighlightSet, type Query } from "./query.js";
import { batchTagRequestDigest } from "./batch-tags.js";
import type { SnapshotPage, SnapshotRow } from "./snapshots.js";

afterEach(() => {
  cleanup();
  history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

const initialQuery: Query = {
  v: 1,
  text: "report AND NOT tag:obsolete",
  syntax: "advanced",
  mode: "lexical",
  filters: { exclude_tag_ids: ["11111111-1111-4111-8111-111111111111"], paths: ["/records"] },
  sort: { field: "path", direction: "asc" },
};

function row(index: number, name = `report-${index}.pdf`): SnapshotRow {
  return {
    node_id: index,
    content_version_id: `00000000-0000-4000-8000-${String(index).padStart(12, "0")}`,
    blob_hash: "a".repeat(64),
    size: 1000 + index,
    revision: 3,
    name,
    path: `/records/${name}`,
    mime_type: "application/pdf",
    media_family: "document",
    modified_at: "2026-09-11T12:00:00Z",
    sort_key: name,
    tags: [{ id: "22222222-2222-4222-8222-222222222222", name: "frozen", revision: 2 }],
    collection_ids: ["33333333-3333-4333-8333-333333333333"],
  };
}

function snapshot(
  query: Query,
  rows: SnapshotRow[],
  cursors: { previous_cursor?: string; next_cursor?: string },
  total = 101,
): SnapshotPage {
  return {
    query,
    dependencies: [],
    query_fingerprint: `sha256:${createHash("sha256").update(canonicalQuery(query)).digest("hex")}`,
    member_hash: "b".repeat(64),
    snapshot_fingerprint: `sha256:${"c".repeat(64)}`,
    generation: { kind: "native" },
    coverage: { configuration: "unconfigured" },
    observed_at: "2026-09-11T12:34:00Z",
    page_size: 100,
    total,
    total_bytes: total * 2006,
    rows,
    facets: [
      "collections",
      "tags",
      "media_family",
      "extension",
      "modified",
      "size",
      "text_coverage",
      "duplicates",
    ].map((dimension) =>
      dimension === "extension"
        ? {
            dimension,
            available: true,
            total,
            missing: 0,
            other: 0,
            values: [
              {
                key: "pdf",
                label: "pdf",
                count: total,
                selected: query.filters.extensions?.includes("pdf") ?? false,
              },
            ],
          }
        : {
            dimension: dimension as SnapshotPage["facets"][number]["dimension"],
            available: false,
            reason: "time_budget_exceeded",
            values: [],
          },
    ),
    snapshot: true,
    snapshot_id: "0123456789abcdef0123456789abcdef",
    created_at: "2026-09-11T12:34:00Z",
    expires_at: "2026-09-11T13:04:00Z",
    ...cursors,
  };
}

it("navigates frozen documents across pages and stops at the boundary or expiry", async () => {
  const action = vi.spyOn(actions, "reportAction").mockImplementation(() => {});
  const searchAction = vi.spyOn(actions, "reportSearch").mockImplementation(() => {});
  const failure = vi.spyOn(actions, "reportFailure").mockImplementation(() => {});
  history.replaceState(
    null,
    "",
    `/#web_session=synthetic&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(initialQuery))}`,
  );
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  const first = snapshot(
    initialQuery,
    Array.from({ length: 100 }, (_, index) => row(index + 1)),
    { next_cursor: "next" },
  );
  const last = snapshot(initialQuery, [row(101)], { previous_cursor: "previous" });
  const root = {
    id: 1000,
    name: "",
    kind: "dir",
    path: "/",
    revision: 1,
    size: 0,
    created_at: "2026-09-11T00:00:00Z",
    modified_at: "2026-09-11T00:00:00Z",
  };
  const json = (value: unknown, status = 200) =>
    new Response(JSON.stringify(value), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  const cursors: string[] = [];
  let highlightRequests = 0;
  let highlightSetReads = 0;
  const payload: HighlightSet = { v: 1, terms: [{ text: "evidence", color: "#ffcc00" }] };
  const highlightSet = {
    id: "44444444-4444-4444-8444-444444444444",
    name: "Review terms",
    description: "Synthetic highlights",
    kind: "highlight_set",
    payload,
    fingerprint: await highlightSetFingerprint(payload),
    revision: 1,
    created_at: first.created_at,
    updated_at: first.created_at,
  };
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1000/children?limit=1000&offset=0")
      return json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/tags?limit=1000&offset=0") {
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    }
    if (url === "/api/v1/saved-queries?limit=1000&offset=0&kind=highlight_set") {
      highlightSetReads++;
      return json({ items: [highlightSet], total: 1, limit: 1000, offset: 0 });
    }
    if (url === "/api/v1/queries/parse")
      return json({
        query: initialQuery,
        query_fingerprint: first.query_fingerprint,
        dependencies: [],
      });
    if (url === "/api/v1/queries/highlights") {
      highlightRequests++;
      return json({
        query_fingerprint: first.query_fingerprint,
        dependencies: [],
        terms: ["report"],
      });
    }
    if (url === "/api/v1/workspace/queries") {
      const { query } = JSON.parse(String(init?.body));
      return json({
        ...snapshot(query, first.rows, { next_cursor: "next" }),
        snapshot_id: first.snapshot_id,
      });
    }
    if (url === "/api/v1/renditions/text") {
      const request = JSON.parse(String(init?.body));
      return json({
        state: "unconfigured",
        source: {
          node_id: request.node_id,
          revision: request.revision,
          version_id: request.version_id,
          blob_hash: request.blob_hash,
          size: request.size,
          media_type: "application/pdf",
        },
        profile: { name: "", configuration: "unconfigured", fingerprint: "" },
        generation_id: "",
        attachment_id: "",
        build_id: "",
      });
    }
    if (url === `/api/v1/workspace/queries/${first.snapshot_id}/pages`) {
      const { cursor } = JSON.parse(String(init?.body));
      cursors.push(cursor);
      return cursor === "next"
        ? json(last)
        : json({ code: "snapshot_gone", detail: "Snapshot expired" }, 410);
    }
    const node = /^\/api\/v1\/nodes\/(\d+)$/.exec(url);
    if (node) {
      const selected = row(Number(node[1]));
      return json({
        ...root,
        id: selected.node_id,
        parent_id: root.id,
        kind: "file",
        name: selected.name,
        path: selected.path,
        revision: selected.revision,
        size: selected.size,
        current_version_id: selected.content_version_id,
        blob_hash: selected.blob_hash,
        mime_type: selected.mime_type,
      });
    }
    if (/^\/api\/v1\/nodes\/\d+\/tags\?/.test(url))
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    if (url.startsWith("/api/v1/audit/status?node_id="))
      return json({ enabled: false, scopes: [] });
    throw new Error(`unexpected request: ${url}`);
  });

  render(App);
  await fireEvent.click(await screen.findByRole("button", { name: "Search" }));
  await screen.findByText("Document 1 of 101");
  expect(searchAction).toHaveBeenCalledExactlyOnceWith("synthetic", "text", false);
  searchAction.mockClear();
  await waitFor(() => expect(highlightRequests).toBe(1));
  const opened = screen.getByRole("cell", { name: "/records/report-100.pdf" }).closest("tr")!;
  await fireEvent.click(opened, { detail: 1 });
  await fireEvent.click(opened, { detail: 2 });
  await fireEvent.dblClick(opened, { detail: 2 });
  await screen.findByText("Document 100 of 101");
  expect(action).toHaveBeenCalledExactlyOnceWith("synthetic", "document_opened");
  expect(opened.getAttribute("aria-selected")).toBe("true");
  action.mockClear();
  await fireEvent.click(
    within(screen.getByRole("navigation", { name: "Document navigation" })).getByRole("button", {
      name: "Next",
    }),
  );
  await screen.findByText("Document 101 of 101");
  expect(cursors).toEqual(["next"]);
  expect(screen.getByRole("region", { name: "Verified content of report-101.pdf" })).toBeTruthy();
  const lastNavigation = within(screen.getByRole("navigation", { name: "Document navigation" }));
  expect((lastNavigation.getByRole("button", { name: "Next" }) as HTMLButtonElement).disabled).toBe(
    true,
  );
  await fireEvent.click(lastNavigation.getByRole("button", { name: "Previous" }));
  await screen.findByText("These results expired. Run the query again to continue.");
  // A page that fails while paging is not a submitted search.
  expect(failure).not.toHaveBeenCalled();
  const expiredNavigation = within(screen.getByRole("navigation", { name: "Document navigation" }));
  for (const name of ["Previous", "Next"]) {
    expect((expiredNavigation.getByRole("button", { name }) as HTMLButtonElement).disabled).toBe(
      true,
    );
  }
  expect(cursors).toEqual(["next", "previous"]);
  expect(action).not.toHaveBeenCalled();
  expect(highlightRequests).toBe(1);
  await fireEvent.click(screen.getByRole("tab", { name: "Text" }));
  // Highlight choices appear only beside readable text; VerifiedPreview covers that menu.
  await screen.findByText("Text extraction isn't set up.");
  expect(highlightSetReads).toBeGreaterThan(0);

  first.snapshot_id = "1123456789abcdef0123456789abcdef";
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await screen.findByText("Document 1 of 101");
  await waitFor(() => expect(highlightRequests).toBe(2));
  expect(searchAction).toHaveBeenCalledExactlyOnceWith("synthetic", "text", false);
  searchAction.mockClear();
  const results = screen.getByRole("region", { name: "Frozen query results" });
  await fireEvent.click(within(results).getByRole("button", { name: "Document" }));
  await screen.findByText("Document 1 of 101");
  expect(action).not.toHaveBeenCalled();
});

it("keeps rapid runs bound to the accepted frozen snapshot while live observations load separately", async () => {
  history.replaceState(
    null,
    "",
    `/#web_session=synthetic&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(initialQuery))}`,
  );
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  const root = {
    id: 1000,
    name: "",
    kind: "dir",
    path: "/",
    revision: 1,
    size: 0,
    created_at: "2026-09-11T00:00:00Z",
    modified_at: "2026-09-11T00:00:00Z",
  };
  const json = (value: unknown, status = 200) =>
    new Response(JSON.stringify(value), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  let resolveOld!: (response: Response) => void;
  const oldRun = new Promise<Response>((resolve) => {
    resolveOld = resolve;
  });
  const requests: string[] = [];
  let creates = 0;
  const workspaceBodies: { query: Query; page_size: number; facets: string[] }[] = [];

  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    requests.push(url);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1000/children?limit=1000&offset=0")
      return json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/tags?limit=1000&offset=0")
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/queries/parse") {
      const query = JSON.parse(String(init?.body)) as Query;
      return json({
        query,
        query_fingerprint: `sha256:${createHash("sha256").update(canonicalQuery(query)).digest("hex")}`,
        dependencies: [],
      });
    }
    if (url === "/api/v1/workspace/queries") {
      creates += 1;
      const body = JSON.parse(String(init?.body)) as {
        query: Query;
        page_size: number;
        facets: string[];
      };
      workspaceBodies.push(body);
      const query = body.query;
      if (creates === 1) return oldRun;
      return json(snapshot(query, [row(1, "new-result.pdf")], {}, 1));
    }
    if (url === "/api/v1/nodes/1") {
      return json({
        id: 1,
        parent_id: 1000,
        name: "new-result.pdf",
        kind: "file",
        current_version_id: "99999999-9999-4999-8999-999999999999",
        blob_hash: "d".repeat(64),
        size: 2000,
        mime_type: "application/pdf",
        revision: 9,
        created_at: "2026-09-11T12:00:00Z",
        modified_at: "2026-09-11T12:40:00Z",
        path: "/records/new-result.pdf",
      });
    }
    if (url === "/api/v1/nodes/1/tags?limit=1000&offset=0") {
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    }
    if (url === "/api/v1/audit/status?node_id=1") {
      return json({ enabled: false, scopes: [] });
    }
    throw new Error(`unexpected request: ${url}`);
  });

  render(App);
  await screen.findByRole("dialog", { name: "Filters and sort" });
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await waitFor(() => expect(creates).toBe(1));
  await fireEvent.input(screen.getByLabelText("Query syntax"), {
    target: { value: "new query" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await screen.findByRole("cell", { name: "/records/new-result.pdf" });
  resolveOld(json(snapshot(initialQuery, [row(1, "old-result.pdf")], {}, 1)));
  await Promise.resolve();
  expect(screen.queryByRole("cell", { name: "/records/old-result.pdf" })).toBeNull();
  expect(screen.getByText("1 result")).toBeTruthy();
  expect(workspaceBodies.at(-1)?.query).toMatchObject({
    text: "new query",
    syntax: "advanced",
    filters: {
      paths: ["/records"],
      exclude_tag_ids: ["11111111-1111-4111-8111-111111111111"],
    },
  });
  expect(workspaceBodies.at(-1)).toMatchObject({
    page_size: 100,
    facets: [
      "collections",
      "tags",
      "media_family",
      "extension",
      "modified",
      "size",
      "text_coverage",
      "duplicates",
    ],
  });

  expect(creates).toBe(2);
  await fireEvent.click(screen.getByRole("cell", { name: "/records/new-result.pdf" }));
  await waitFor(() => expect(requests.some((url) => url.includes("/nodes/1/tags"))).toBe(true));
  expect(requests.some((url) => url.includes("/nodes/1/tags"))).toBe(true);
  expect(requests.some((url) => url.includes("audit/status?node_id=1"))).toBe(true);
  expect(requests.some((url) => url === "/api/daemon/web-download")).toBe(false);
  expect(screen.getByText("a".repeat(64))).toBeTruthy();
  await fireEvent.click(screen.getAllByRole("button", { name: "Clear search" }).at(-1)!);
  await waitFor(() =>
    expect(screen.queryByRole("region", { name: "Frozen query results" })).toBeNull(),
  );
  expect(new URLSearchParams(location.hash.slice(1)).has("query")).toBe(false);
});

it("reruns supported facets and sorting from the complete accepted query", async () => {
  history.replaceState(
    null,
    "",
    `/#web_session=synthetic&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(initialQuery))}`,
  );
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  const root = {
    id: 1000,
    name: "",
    kind: "dir",
    path: "/",
    revision: 1,
    size: 0,
    created_at: "2026-09-11T00:00:00Z",
    modified_at: "2026-09-11T00:00:00Z",
  };
  const json = (value: unknown) =>
    new Response(JSON.stringify(value), { headers: { "Content-Type": "application/json" } });
  const bodies: { query: Query; page_size: number; facets: string[] }[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1000/children?limit=1000&offset=0")
      return json({
        directory: root,
        items: [{ ...root, id: 2000, name: "live-folder", path: "/live-folder" }],
        total: 1,
        limit: 1000,
        offset: 0,
      });
    if (url === "/api/v1/tags?limit=1000&offset=0")
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/queries/parse") {
      const query = JSON.parse(String(init?.body)) as Query;
      return json({
        query,
        query_fingerprint: `sha256:${createHash("sha256").update(canonicalQuery(query)).digest("hex")}`,
        dependencies: [],
      });
    }
    if (url === "/api/v1/workspace/queries") {
      const body = JSON.parse(String(init?.body)) as {
        query: Query;
        page_size: number;
        facets: string[];
      };
      bodies.push(body);
      const rows = body.query.filters.extensions?.includes("pdf")
        ? [row(1)]
        : [row(1), { ...row(2, "report-2.txt"), mime_type: "text/plain" }];
      const page = snapshot(body.query, rows, {}, rows.length);
      if (rows.length === 2) {
        page.facets.find((facet) => facet.dimension === "extension")!.values = [
          { key: "pdf", label: "pdf", count: 1, selected: false },
          { key: "txt", label: "txt", count: 1, selected: false },
        ];
      }
      return json(page);
    }
    throw new Error(`unexpected request: ${url}`);
  });

  render(App);
  await screen.findByRole("dialog", { name: "Filters and sort" });
  await fireEvent.click(await screen.findByRole("cell", { name: "live-folder" }));
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await screen.findByRole("button", { name: "pdf, 1 documents" });
  await fireEvent.click(screen.getByRole("button", { name: "pdf, 1 documents" }));
  await screen.findByRole("button", { name: "pdf, 1 documents, selected" });
  expect(bodies.at(-1)?.query).toMatchObject({
    text: initialQuery.text,
    syntax: "advanced",
    filters: {
      paths: ["/records"],
      exclude_tag_ids: ["11111111-1111-4111-8111-111111111111"],
      extensions: ["pdf"],
    },
  });
  const results = screen.getByRole("region", { name: "Frozen query results" });
  await fireEvent.click(within(results).getByRole("button", { name: "Document" }));
  await waitFor(() => expect(bodies).toHaveLength(3));
  await screen.findByRole("region", { name: "Frozen query results" });
  await fireEvent.keyDown(window, { key: "Enter" });
  expect(screen.getByRole("region", { name: "Frozen query results" })).toBeTruthy();
  expect(bodies.at(-1)?.query.sort).toEqual({ field: "name", direction: "asc" });
  expect(bodies.at(-1)).toMatchObject({
    page_size: 100,
    facets: [
      "collections",
      "tags",
      "media_family",
      "extension",
      "modified",
      "size",
      "text_coverage",
      "duplicates",
    ],
  });

  await waitFor(() =>
    expect((screen.getByRole("button", { name: "Tag all…" }) as HTMLButtonElement).disabled).toBe(
      false,
    ),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  expect(
    (screen.getByRole("searchbox", { name: "Search documents" }) as HTMLInputElement).value,
  ).toBe(initialQuery.text);
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await waitFor(() => expect(bodies).toHaveLength(4));
  expect(bodies[3].query).toEqual(bodies[2].query);
});

it("hides the live detail card when a snapshot has no selected row", async () => {
  history.replaceState(null, "", "/#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  const root = {
    id: 1000,
    name: "",
    kind: "dir",
    path: "/",
    revision: 1,
    size: 0,
    created_at: "2026-09-11T00:00:00Z",
    modified_at: "2026-09-11T00:00:00Z",
  };
  const json = (value: unknown) => new Response(JSON.stringify(value));
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url.includes("/children?"))
      return json({
        directory: root,
        items: [{ ...root, id: 2000, name: "live-folder", path: "/live-folder" }],
        total: 1,
        limit: 1000,
        offset: 0,
      });
    if (url === "/api/v1/tags?limit=1000&offset=0")
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/workspace/queries")
      return json(snapshot(JSON.parse(String(init?.body)).query, [], {}, 0));
    throw new Error(`unexpected request: ${url}`);
  });
  render(App);
  await fireEvent.click(await screen.findByRole("cell", { name: "live-folder" }));
  expect(screen.getByLabelText("Folder details for live-folder")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await screen.findByRole("region", { name: "Frozen query results" });
  expect(screen.queryByLabelText("Folder details for live-folder")).toBeNull();
});

it("refreshes live metadata and download authority after tagging an inspected frozen row", async () => {
  history.replaceState(
    null,
    "",
    `/#web_session=synthetic&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(initialQuery))}`,
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
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => undefined);
  const frozen = row(1);
  const root = {
    id: 1000,
    name: "",
    kind: "dir",
    path: "/",
    revision: 1,
    size: 0,
    created_at: "2026-09-11T00:00:00Z",
    modified_at: "2026-09-11T00:00:00Z",
  };
  const tag = {
    id: "55555555-5555-4555-8555-555555555555",
    name: "Reviewed",
    revision: 1,
    assignment_count: 0,
  };
  let changed = false;
  const prepared: Record<string, unknown>[] = [];
  const json = (value: unknown, status = 200) =>
    new Response(JSON.stringify(value), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url.includes("/children?"))
      return json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/tags?limit=1000&offset=0")
      return json({ items: [tag], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/workspace/queries") return json(snapshot(initialQuery, [frozen], {}, 1));
    if (url === "/api/v1/nodes/1")
      return json({
        ...root,
        id: 1,
        parent_id: 1000,
        kind: "file",
        name: frozen.name,
        path: frozen.path,
        revision: changed ? 4 : 3,
        current_version_id: frozen.content_version_id,
        blob_hash: frozen.blob_hash,
        size: frozen.size,
        mime_type: frozen.mime_type,
      });
    if (url === "/api/v1/nodes/1/tags?limit=1000&offset=0") {
      return json({
        items: [...frozen.tags, ...(changed ? [tag] : [])],
        total: changed ? 2 : 1,
        limit: 1000,
        offset: 0,
      });
    }
    if (url === "/api/v1/audit/status?node_id=1") return json({ enabled: false, scopes: [] });
    if (url === `/api/v1/tags/${tag.id}`) return json(tag);
    if (url === "/api/v1/batch/tags/preview")
      return json({
        tag_id: tag.id,
        tag_revision: changed ? 2 : 1,
        nodes: [{ node_id: 1, revision: changed ? 4 : 3, assigned: changed }],
      });
    if (url === "/api/v1/batch/tags") {
      const request = JSON.parse(String(init?.body));
      changed = true;
      return json({
        version: 1,
        operation_id: request.operation_id,
        request_digest: await batchTagRequestDigest(request),
        tag_id: tag.id,
        assign: true,
        tag_revision: 2,
        assignment_count: 1,
        completed_at: "2026-09-11T13:00:00.000000000Z",
        nodes: [{ node_id: 1, expected_revision: 3, revision: 4, changed: true }],
      });
    }
    if (url === "/api/daemon/web-download") {
      const request = JSON.parse(String(init?.body));
      prepared.push(request);
      if (request.revision !== (changed ? 4 : 3))
        return json({ detail: "Stale download revision" }, 409);
      return new Response(
        `${JSON.stringify({
          phase: "ready",
          received: frozen.size,
          total: frozen.size,
          url: "/api/daemon/web-download/file?ticket=synthetic",
          name: frozen.name,
          version_id: frozen.content_version_id,
          blob_hash: frozen.blob_hash,
        })}\n`,
      );
    }
    throw new Error(`unexpected request: ${url}`);
  });
  render(App);
  await fireEvent.click(await screen.findByRole("button", { name: "Search" }));
  await screen.findByRole("button", { name: "Download" });
  await fireEvent.click(screen.getByRole("checkbox", { name: `Select ${frozen.path}` }));
  await fireEvent.click(screen.getByRole("button", { name: "Tag selected…" }));
  await fireEvent.input(screen.getByRole("combobox", { name: "Tag" }), {
    target: { value: "Rev" },
  });
  await fireEvent.click(screen.getByRole("option", { name: "Reviewed (0)" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Apply" }));
  await screen.findByText("Added “Reviewed” to 1 document.");
  await fireEvent.click(screen.getByRole("button", { name: "Done" }));
  const current = screen.getByRole("region", { name: "Current details" });
  await waitFor(() =>
    expect(within(current).getByRole("group", { name: "Current tags" }).textContent).toContain(
      "Reviewed",
    ),
  );
  expect(screen.getByRole("region", { name: "Saved tags" })).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Download" }));
  await waitFor(() =>
    expect(prepared.at(-1)).toMatchObject({
      revision: 4,
      version_id: frozen.content_version_id,
      blob_hash: frozen.blob_hash,
    }),
  );
});

it("keeps the filtered result in view after trashing an inspected frozen row", async () => {
  history.replaceState(
    null,
    "",
    `/#web_session=synthetic&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(initialQuery))}`,
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
  const frozen = row(1);
  const root = {
    id: 1000,
    name: "",
    kind: "dir",
    path: "/",
    revision: 1,
    size: 0,
    created_at: "2026-09-11T00:00:00Z",
    modified_at: "2026-09-11T00:00:00Z",
  };
  const live = {
    ...root,
    id: 1,
    parent_id: 1000,
    kind: "file",
    name: frozen.name,
    path: frozen.path,
    revision: 3,
    current_version_id: frozen.content_version_id,
    blob_hash: frozen.blob_hash,
    size: frozen.size,
    mime_type: frozen.mime_type,
  };
  const json = (value: unknown, status = 200) =>
    new Response(JSON.stringify(value), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  const searches: Query[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url.includes("/children?"))
      return json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/tags?limit=1000&offset=0")
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/workspace/queries") {
      searches.push(JSON.parse(String(init?.body)).query);
      return json(snapshot(initialQuery, [frozen], {}, 1));
    }
    if (url === "/api/v1/nodes/1") return json(live);
    if (url === "/api/v1/nodes/1/tags?limit=1000&offset=0")
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/audit/status?node_id=1") return json({ enabled: false, scopes: [] });
    if (url === "/api/v1/nodes/1/trash" && init?.method === "POST")
      return json({ ...live, revision: 4, trashed_at: "2026-09-11T13:00:00Z" });
    throw new Error(`unexpected request: ${url}`);
  });
  render(App);
  await fireEvent.click(await screen.findByRole("button", { name: "Search" }));
  await screen.findByRole("region", { name: "Frozen query results" });
  await fireEvent.click(await screen.findByRole("button", { name: "Move to trash" }));
  await screen.findByText(`Moved “${frozen.name}” to trash`);
  await waitFor(() => expect(searches).toHaveLength(2));
  expect(searches[1]).toMatchObject({ text: initialQuery.text, filters: initialQuery.filters });
  expect(screen.getByRole("region", { name: "Frozen query results" })).toBeTruthy();
});

it("reports a submitted frozen run that finds nothing or fails", async () => {
  const searchAction = vi.spyOn(actions, "reportSearch").mockImplementation(() => {});
  const failure = vi.spyOn(actions, "reportFailure").mockImplementation(() => {});
  history.replaceState(
    null,
    "",
    `/#web_session=synthetic&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(initialQuery))}`,
  );
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  const root = { id: 1000, name: "", kind: "dir", path: "/", revision: 1, size: 0 };
  const json = (value: unknown, status = 200) =>
    new Response(JSON.stringify(value), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  const empty = snapshot(initialQuery, [], {}, 0);
  let runs = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url.startsWith("/api/v1/nodes/1000/children"))
      return json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
    if (url.startsWith("/api/v1/tags?") || url.startsWith("/api/v1/saved-queries?"))
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/queries/parse")
      return json({
        query: initialQuery,
        query_fingerprint: empty.query_fingerprint,
        dependencies: [],
      });
    if (url === "/api/v1/queries/highlights")
      return json({ query_fingerprint: empty.query_fingerprint, dependencies: [], terms: [] });
    if (url === "/api/v1/workspace/queries")
      return ++runs === 1
        ? json(empty)
        : json({ code: "internal", detail: "synthetic failure" }, runs === 2 ? 500 : 401);
    throw new Error(`unexpected request: ${url}`);
  });

  render(App);
  await fireEvent.click(await screen.findByRole("button", { name: "Search" }));
  await waitFor(() =>
    expect(searchAction).toHaveBeenCalledExactlyOnceWith("synthetic", "text", true),
  );
  expect(failure).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await waitFor(() =>
    expect(failure).toHaveBeenCalledExactlyOnceWith("synthetic", "search", expect.any(Error)),
  );
  expect(searchAction).toHaveBeenCalledOnce();
  // A rejected session sends the person to sign in rather than counting a failed search.
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await waitFor(() => expect(runs).toBe(3));
  await screen.findByText(/session expired or was rejected/);
  expect(failure).toHaveBeenCalledOnce();
});

it("keeps each frozen run's empty outcome after a newer run supersedes it", async () => {
  const { SnapshotWorkspace } = await import("./snapshotWorkspace.svelte.js");
  const workspace = new SnapshotWorkspace(() => "synthetic", vi.fn(), vi.fn());
  const options = {
    facets: snapshot(initialQuery, [], {}, 0).facets.map((facet) => facet.dimension),
  };
  let resolveOld!: (value: Response) => void;
  let runs = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(async () => {
    if (++runs === 1)
      return new Promise<Response>((resolve) => {
        resolveOld = resolve;
      });
    return Response.json(
      snapshot(initialQuery, runs === 2 ? [] : [row(2)], {}, runs === 2 ? 0 : 1),
    );
  });
  const old = workspace.run(initialQuery, options);
  const accepted = await workspace.run(initialQuery, options);
  const newer = workspace.run(initialQuery, options);
  resolveOld(Response.json(snapshot(initialQuery, [row(1)], {}, 1)));
  expect(await old).toBeUndefined();
  expect(accepted).toEqual({ type: "text", empty: true });
  expect(await newer).toEqual({ type: "text", empty: false });
  workspace.reset();
});

it("retains edits made while a submitted search is still running", async () => {
  history.replaceState(
    null,
    "",
    `/#web_session=synthetic&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(initialQuery))}`,
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
    id: 1000,
    name: "",
    kind: "dir",
    path: "/",
    revision: 1,
    size: 0,
    created_at: "2026-09-11T00:00:00Z",
    modified_at: "2026-09-11T00:00:00Z",
  };
  let complete!: (response: Response) => void;
  const pending = new Promise<Response>((resolve) => {
    complete = resolve;
  });
  let running = false;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    const json = (value: unknown) => new Response(JSON.stringify(value));
    if (url.includes("/path?")) return json(root);
    if (url.includes("/children?"))
      return json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/queries/parse") {
      const query = JSON.parse(String(init?.body));
      return json({
        query,
        query_fingerprint: `sha256:${createHash("sha256").update(canonicalQuery(query)).digest("hex")}`,
        dependencies: [],
      });
    }
    if (url === "/api/v1/workspace/queries") {
      running = true;
      return pending;
    }
    return json({ items: [], total: 0, limit: 1000, offset: 0 });
  });
  render(App);
  await screen.findByRole("dialog", { name: "Filters and sort" });
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await waitFor(() => expect(running).toBe(true));
  await fireEvent.input(screen.getByLabelText("Filters JSON"), {
    target: { value: '{"size_min":' },
  });
  await fireEvent.click(screen.getByRole("combobox", { name: "Documents per page: 100" }));
  await fireEvent.click(screen.getByRole("option", { name: "50" }));
  complete(new Response(JSON.stringify(snapshot(initialQuery, [], {}, 0))));
  await screen.findByRole("region", { name: "Frozen query results" });
  expect((screen.getByLabelText("Filters JSON") as HTMLTextAreaElement).value).toBe('{"size_min":');
  await fireEvent.keyDown(window, { key: "Escape" });
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  expect(screen.getByRole("combobox", { name: "Documents per page: 50" })).toBeTruthy();
  expect((screen.getByLabelText("Filters JSON") as HTMLTextAreaElement).value).toBe('{"size_min":');
});

it("resolves referenced import names when paging interrupts a pending read", async () => {
  history.replaceState(
    null,
    "",
    `/#web_session=synthetic&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(initialQuery))}`,
  );
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  const collectionID = "33333333-3333-4333-8333-333333333333";
  const collection = {
    id: collectionID,
    source_kind: "cli",
    source_description: "/synthetic/import",
    started_at: "2026-09-11T12:00:00Z",
    file_count: 2,
    total_bytes: 2000,
    label: "Synthetic import",
    label_revision: 1,
    label_updated_at: "2026-09-11T12:00:00Z",
  };
  let release!: () => void;
  const pending = new Promise<void>((resolve) => {
    release = resolve;
  });
  let first = snapshot(
    initialQuery,
    Array.from({ length: 100 }, (_, index) => ({
      ...row(index + 1),
      display_collection_id: collectionID,
    })),
    { next_cursor: "next" },
  );
  let last = snapshot(initialQuery, [{ ...row(101), display_collection_id: collectionID }], {
    previous_cursor: "previous",
  });
  const json = (value: unknown) => new Response(JSON.stringify(value));
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === `/api/v1/collections/${collectionID}`) {
      await pending;
      return json(collection);
    }
    if (url.startsWith("/api/v1/collections?")) {
      await pending;
      return json({ items: [collection], total: 1, limit: 100, offset: 0 });
    }
    if (url === "/api/v1/workspace/queries") {
      const { query } = JSON.parse(String(init?.body));
      first = {
        ...first,
        query,
        query_fingerprint: `sha256:${createHash("sha256").update(canonicalQuery(query)).digest("hex")}`,
      };
      last = { ...last, query, query_fingerprint: first.query_fingerprint };
      return json(first);
    }
    if (url === `/api/v1/workspace/queries/${first.snapshot_id}/pages`) return json(last);
    if (url === "/api/v1/queries/parse") {
      const query = JSON.parse(String(init?.body));
      return json({
        query,
        query_fingerprint: `sha256:${createHash("sha256").update(canonicalQuery(query)).digest("hex")}`,
        dependencies: [],
      });
    }
    if (url.includes("/path?"))
      return json({
        id: 1000,
        name: "",
        kind: "dir",
        path: "/",
        revision: 1,
        size: 0,
        created_at: first.created_at,
        modified_at: first.created_at,
      });
    if (url.includes("/children?")) return json({ items: [], total: 0, limit: 1000, offset: 0 });
    return json({ items: [], total: 0, limit: 1000, offset: 0 });
  });
  render(App);
  await fireEvent.click(await screen.findByRole("button", { name: "Search" }));
  await screen.findByRole("cell", { name: "/records/report-1.pdf" });
  await waitFor(() =>
    expect(fetch.mock.calls.some(([url]) => String(url).includes("/collections"))).toBe(true),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Next page" }));
  await screen.findByRole("cell", { name: "/records/report-101.pdf" });
  release();
  expect(await screen.findByText("Synthetic import")).toBeTruthy();
  expect(
    fetch.mock.calls.filter(([url]) => String(url).startsWith("/api/v1/collections?")),
  ).toHaveLength(0);
});
