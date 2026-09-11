import { createHash } from "node:crypto";
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import App from "./App.svelte";
import { canonicalQuery, type Query } from "./query.js";
import type { SnapshotPage, SnapshotRow } from "./snapshots.js";

afterEach(() => { cleanup(); history.replaceState(null, "", "/"); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

const initialQuery: Query = {
  v: 1, text: "report AND NOT tag:obsolete", syntax: "advanced", mode: "lexical",
  filters: { exclude_tag_ids: ["11111111-1111-4111-8111-111111111111"], paths: ["/records"] },
  sort: { field: "path", direction: "asc" },
};

function row(index: number, name = `report-${index}.pdf`): SnapshotRow {
  return {
    node_id: index, content_version_id: `00000000-0000-4000-8000-${String(index).padStart(12, "0")}`,
    blob_hash: "a".repeat(64), size: 1000 + index, revision: 3, name, path: `/records/${name}`,
    mime_type: "application/pdf", media_family: "document", modified_at: "2026-09-11T12:00:00Z",
    sort_key: name, tags: [{ id: "22222222-2222-4222-8222-222222222222", name: "frozen", revision: 2 }],
    collection_ids: ["33333333-3333-4333-8333-333333333333"],
  };
}

function snapshot(query: Query, rows: SnapshotRow[], cursors: { previous_cursor?: string; next_cursor?: string }, total = 101): SnapshotPage {
  return {
    query, dependencies: [], query_fingerprint: `sha256:${createHash("sha256").update(canonicalQuery(query)).digest("hex")}`,
    member_hash: "b".repeat(64), snapshot_fingerprint: `sha256:${"c".repeat(64)}`,
    generation: { kind: "native" }, coverage: { configuration: "unconfigured" }, observed_at: "2026-09-11T12:34:00Z",
    page_size: 100, total, total_bytes: total * 2006, rows,
    facets: ["collections", "tags", "media_family", "extension", "modified", "size", "text_coverage", "duplicates"].map((dimension) =>
      dimension === "extension"
        ? { dimension, available: true, total, missing: 0, other: 0,
            values: [{ key: "pdf", label: "pdf", count: total, selected: query.filters.extensions?.includes("pdf") ?? false }] }
        : { dimension: dimension as SnapshotPage["facets"][number]["dimension"], available: false,
            reason: "time_budget_exceeded", values: [] }),
    snapshot: true, snapshot_id: "0123456789abcdef0123456789abcdef",
    created_at: "2026-09-11T12:34:00Z", expires_at: "2026-09-11T13:04:00Z", ...cursors,
  };
}

it("keeps rapid runs bound to the accepted frozen snapshot while live observations load separately", async () => {
  history.replaceState(null, "", `/#web_session=synthetic&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(initialQuery))}`);
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  const root = { id: 1000, name: "", kind: "dir", path: "/", revision: 1, size: 0, created_at: "2026-09-11T00:00:00Z", modified_at: "2026-09-11T00:00:00Z" };
  const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } });
  let resolveOld!: (response: Response) => void;
  const oldRun = new Promise<Response>((resolve) => { resolveOld = resolve; });
  const requests: string[] = [];
  let creates = 0;
  const workspaceBodies: { query: Query; page_size: number; facets: string[] }[] = [];

  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    requests.push(url);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1000/children?limit=1000&offset=0") return json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/tags?limit=1000&offset=0") return json({ items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/queries/parse") {
      const query = JSON.parse(String(init?.body)) as Query;
      return json({ query, query_fingerprint: `sha256:${createHash("sha256").update(canonicalQuery(query)).digest("hex")}`, dependencies: [] });
    }
    if (url === "/api/v1/workspace/queries") {
      creates += 1;
      const body = JSON.parse(String(init?.body)) as { query: Query; page_size: number; facets: string[] };
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
  await screen.findByRole("region", { name: "Query editor" });
  await fireEvent.click(screen.getByRole("button", { name: "Run query" }));
  await waitFor(() => expect(creates).toBe(1));
  await fireEvent.input(screen.getByLabelText("Query expression"), { target: { value: "new query" } });
  await fireEvent.click(screen.getByRole("button", { name: "Run query" }));
  await screen.findByRole("cell", { name: "/records/new-result.pdf" });
  resolveOld(json(snapshot(initialQuery, [row(1, "old-result.pdf")], {}, 1)));
  await Promise.resolve();
  expect(screen.queryByRole("cell", { name: "/records/old-result.pdf" })).toBeNull();
  expect(screen.getByText("1–1 of 1", { exact: false })).toBeTruthy();
  expect(workspaceBodies.at(-1)?.query).toMatchObject({
    text: "new query", syntax: "advanced", filters: {
      paths: ["/records"], exclude_tag_ids: ["11111111-1111-4111-8111-111111111111"],
    },
  });
  expect(workspaceBodies.at(-1)).toMatchObject({ page_size: 100, facets: [
    "collections", "tags", "media_family", "extension", "modified", "size", "text_coverage", "duplicates",
  ] });

  expect(creates).toBe(2);
  await fireEvent.click(screen.getByRole("cell", { name: "/records/new-result.pdf" }));
  await waitFor(() => expect(requests.some((url) => url.includes("/nodes/1/tags"))).toBe(true));
  expect(requests.some((url) => url.includes("/nodes/1/tags"))).toBe(true);
  expect(requests.some((url) => url.includes("audit/status?node_id=1"))).toBe(true);
  expect(requests.some((url) => url === "/api/daemon/web-download")).toBe(false);
  expect(screen.getByText("a".repeat(64))).toBeTruthy();
});

it("reruns supported facets and sorting from the complete accepted query", async () => {
  history.replaceState(null, "", `/#web_session=synthetic&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(initialQuery))}`);
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  const root = { id: 1000, name: "", kind: "dir", path: "/", revision: 1, size: 0, created_at: "2026-09-11T00:00:00Z", modified_at: "2026-09-11T00:00:00Z" };
  const json = (value: unknown) => new Response(JSON.stringify(value), { headers: { "Content-Type": "application/json" } });
  const bodies: { query: Query; page_size: number; facets: string[] }[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1000/children?limit=1000&offset=0") return json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/tags?limit=1000&offset=0") return json({ items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/queries/parse") {
      const query = JSON.parse(String(init?.body)) as Query;
      return json({ query, query_fingerprint: `sha256:${createHash("sha256").update(canonicalQuery(query)).digest("hex")}`, dependencies: [] });
    }
    if (url === "/api/v1/workspace/queries") {
      const body = JSON.parse(String(init?.body)) as { query: Query; page_size: number; facets: string[] };
      bodies.push(body);
      return json(snapshot(body.query, [row(1)], {}, 1));
    }
    throw new Error(`unexpected request: ${url}`);
  });

  render(App);
  await screen.findByRole("region", { name: "Query editor" });
  await fireEvent.click(screen.getByRole("button", { name: "Run query" }));
  await screen.findByRole("button", { name: "pdf, 1 documents" });
  await fireEvent.click(screen.getByRole("button", { name: "pdf, 1 documents" }));
  await screen.findByRole("button", { name: "pdf, 1 documents, selected" });
  expect(bodies.at(-1)?.query).toMatchObject({
    text: initialQuery.text, syntax: "advanced", filters: {
      paths: ["/records"], exclude_tag_ids: ["11111111-1111-4111-8111-111111111111"], extensions: ["pdf"],
    },
  });
  const results = screen.getByRole("region", { name: "Frozen query results" });
  await fireEvent.click(within(results).getByRole("button", { name: "Document" }));
  await waitFor(() => expect(bodies).toHaveLength(3));
  expect(bodies.at(-1)?.query.sort).toEqual({ field: "name", direction: "asc" });
  expect(bodies.at(-1)).toMatchObject({ page_size: 100, facets: [
    "collections", "tags", "media_family", "extension", "modified", "size", "text_coverage", "duplicates",
  ] });
});
