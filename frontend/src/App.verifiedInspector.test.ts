import { createHash } from "node:crypto";
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import App from "./App.svelte";
import { canonicalQuery, parseQuery } from "./query.js";
import type { SnapshotPage, SnapshotRow } from "./snapshots.js";

afterEach(() => {
  cleanup();
  history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it("inspects and downloads a frozen snapshot version under separately labeled live authority", async () => {
  const bytes = new TextEncoder().encode("Pinned historical evidence\n");
  const selectedHash = createHash("sha256").update(bytes).digest("hex");
  const selectedVersion = "11111111-1111-4111-8111-111111111111";
  const query = parseQuery("{}");
  const row: SnapshotRow = {
    node_id: 7,
    content_version_id: selectedVersion,
    blob_hash: selectedHash,
    size: bytes.length,
    revision: 3,
    name: "historical.txt",
    path: "/records/historical.txt",
    mime_type: "text/plain; charset=utf-8",
    media_family: "text",
    modified_at: "2026-09-01T12:00:00Z",
    sort_key: "historical.txt",
    tags: [
      {
        id: "22222222-2222-4222-8222-222222222222",
        name: "original",
        revision: 2,
      },
    ],
    collection_ids: ["33333333-3333-4333-8333-333333333333"],
    display_collection_id: "33333333-3333-4333-8333-333333333333",
    display_collection_label: "September import",
  };
  const page: SnapshotPage = {
    query,
    dependencies: [],
    query_fingerprint: `sha256:${createHash("sha256").update(canonicalQuery(query)).digest("hex")}`,
    member_hash: selectedHash,
    snapshot_fingerprint: `sha256:${"c".repeat(64)}`,
    generation: { kind: "native" },
    coverage: { configuration: "unconfigured" },
    observed_at: "2026-09-11T12:34:00Z",
    page_size: 100,
    total: 1,
    total_bytes: bytes.length,
    rows: [row],
    facets: [
      "collections",
      "tags",
      "media_family",
      "extension",
      "modified",
      "size",
      "text_coverage",
      "duplicates",
    ].map((dimension) => ({
      dimension: dimension as SnapshotPage["facets"][number]["dimension"],
      available: false,
      reason: "not_requested",
      values: [],
    })),
    snapshot: true,
    snapshot_id: "0123456789abcdef0123456789abcdef",
    created_at: "2026-09-11T12:34:00Z",
    expires_at: "2026-09-11T13:04:00Z",
  };
  const root = {
    id: 1,
    name: "",
    kind: "dir",
    size: 0,
    revision: 1,
    path: "/",
    created_at: "2026-09-11T00:00:00Z",
    modified_at: "2026-09-11T00:00:00Z",
  };
  const liveNode = {
    id: 7,
    parent_id: 2,
    name: "renamed-current.txt",
    path: "/live/renamed-current.txt",
    kind: "file",
    current_version_id: "99999999-9999-4999-8999-999999999999",
    blob_hash: "d".repeat(64),
    size: 9,
    mime_type: "text/plain",
    revision: 9,
    created_at: "2026-09-01T12:00:00Z",
    modified_at: "2026-09-11T12:30:00Z",
  };
  const json = (value: unknown) =>
    new Response(JSON.stringify(value), {
      headers: { "Content-Type": "application/json" },
    });
  const preparedBodies: unknown[] = [];
  history.replaceState(
    null,
    "",
    `/#web_session=synthetic&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(query))}`,
  );
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1/children?limit=1000&offset=0")
      return json({
        directory: root,
        items: [],
        total: 0,
        limit: 1000,
        offset: 0,
      });
    if (url === "/api/v1/tags?limit=1000&offset=0")
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/queries/parse")
      return json({
        query,
        query_fingerprint: page.query_fingerprint,
        dependencies: [],
      });
    if (url === "/api/v1/workspace/queries") return json(page);
    if (url === "/api/v1/nodes/7") return json(liveNode);
    if (url === "/api/v1/nodes/7/tags?limit=1000&offset=0")
      return json({
        items: [
          {
            id: "44444444-4444-4444-8444-444444444444",
            name: "live now",
            revision: 4,
            assignment_count: 1,
          },
        ],
        total: 1,
        limit: 1000,
        offset: 0,
      });
    if (url === "/api/v1/audit/status?node_id=7")
      return json({
        enabled: false,
        vault_id: "vault",
        operation_sequence_high_water: 0,
        allocation_entry_count: 0,
        scopes: [],
        membership: {
          node_id: 7,
          path: liveNode.path,
          trashed: false,
          protected: false,
          scope_ids: [],
          baseline_digests: [],
        },
      });
    if (url === "/api/daemon/web-download" && init?.method === "POST") {
      preparedBodies.push(JSON.parse(String(init.body)));
      return new Response(
        `${JSON.stringify({
          phase: "ready",
          received: bytes.length,
          total: bytes.length,
          url: "/api/daemon/web-download/file?ticket=preview",
          name: row.name,
          version_id: selectedVersion,
          blob_hash: selectedHash,
        })}\n`,
      );
    }
    if (url === "/api/daemon/web-download/file?ticket=preview")
      return new Response(bytes, {
        headers: {
          "Content-Type": row.mime_type,
          "Content-Length": String(bytes.length),
          "Content-Digest": `sha-256=:${createHash("sha256").update(bytes).digest("base64")}:`,
          "X-Docbank-Content-Version": selectedVersion,
          "X-Docbank-Blob-Hash": selectedHash,
          "X-Docbank-Blob-Size": String(bytes.length),
        },
      });
    if (
      url.startsWith("/api/daemon/web-download?ticket=") &&
      init?.method === "DELETE"
    )
      return new Response(null, { status: 204 });
    throw new Error(`unexpected request: ${url}`);
  });

  render(App);
  await screen.findByRole("region", { name: "Query editor" });
  await fireEvent.click(screen.getByRole("button", { name: "Run query" }));

  expect(await screen.findByText("Pinned historical evidence")).toBeTruthy();
  expect(screen.getByText("Original snapshot facts")).toBeTruthy();
  expect(screen.getByText("Current live observations")).toBeTruthy();
  expect(screen.getByText("September import")).toBeTruthy();
  expect(screen.getAllByText("live now").length).toBeGreaterThan(0);
  expect(screen.getByText("Revision 9")).toBeTruthy();
  expect(preparedBodies[0]).toEqual({
    node_id: 7,
    revision: 9,
    version_id: selectedVersion,
    blob_hash: selectedHash,
    size: bytes.length,
    purpose: "preview",
  });
});
