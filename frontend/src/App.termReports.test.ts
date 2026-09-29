import { createHash } from "node:crypto";
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import App from "./App.svelte";
import { canonicalQuery, type Query } from "./query.js";
import type { Request } from "./generated/docbank.js";

const root = { id: 1000, name: "", kind: "dir", path: "/", revision: 1, size: 0,
  created_at: "2026-09-20T12:00:00Z", modified_at: "2026-09-20T12:00:00Z" };
const first = { ...root, id: 1, parent_id: root.id, kind: "file", name: "alpha.txt", path: "/alpha.txt",
  current_version_id: "20000000-0000-4000-8000-000000000001", blob_hash: "a".repeat(64), size: 10, mime_type: "text/plain" };
const second = { ...first, id: 2, name: "other-alpha.txt", path: "/other-alpha.txt",
  current_version_id: "20000000-0000-4000-8000-000000000002", blob_hash: "b".repeat(64) };
const query: Query = { v: 1, text: "alpha", syntax: "simple", mode: "lexical", filters: {}, sort: { field: "path", direction: "asc" } };
const json = (value: unknown) => new Response(JSON.stringify(value), { headers: { "Content-Type": "application/json" } });

afterEach(() => { cleanup(); history.replaceState(null, "", "/"); vi.unstubAllGlobals(); vi.restoreAllMocks(); Reflect.deleteProperty(Element.prototype, "scrollIntoView"); });

it.each(["live", "query page", "missing identity"])("reports the displayed selection from %s without expanding scope", async mode => {
  history.replaceState(null, "", `/#web_session=synthetic&web_upload_secret=proof${mode === "query page" ? `&query=${encodeURIComponent(JSON.stringify(query))}` : ""}`);
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
  let current = { ...first };
  const other = { ...second, current_version_id: mode === "missing identity" ? "" : second.current_version_id };
  const submitted: Request[] = [];
  let listings = 0;
  const fingerprint = `sha256:${createHash("sha256").update(canonicalQuery(query)).digest("hex")}`;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1000/children?limit=1000&offset=0") {
      listings++; return json({ directory: root, items: [current, other], total: 2, limit: 1000, offset: 0 });
    }
    if (url.startsWith("/api/v1/tags?") || url.startsWith("/api/v1/collections?") || url.startsWith("/api/v1/search-exports?") || url.startsWith("/api/v1/saved-queries?") || /^\/api\/v1\/nodes\/\d+\/tags\?/.test(url)) return json({ items: [], total: 0, limit: 1000, offset: 0 });
    if (url.startsWith("/api/v1/audit/status?")) return json({ enabled: false, scopes: [] });
    if (url === "/api/v1/queries/parse") return json({ query, query_fingerprint: fingerprint, dependencies: [] });
    if (url === "/api/v1/queries/highlights") return json({ query_fingerprint: fingerprint, dependencies: [], terms: ["alpha"] });
    if (url === "/api/v1/nodes/1") return json(current);
    if (url === "/api/v1/renditions/text") return json({ state: "unconfigured", source: { node_id: 1, revision: 1, version_id: first.current_version_id, blob_hash: first.blob_hash, size: 10, media_type: "text/plain" }, profile: { name: "", configuration: "unconfigured", fingerprint: "" }, generation_id: "", attachment_id: "", build_id: "" });
    if (url === "/api/v1/workspace/queries") return json({ query, dependencies: [], query_fingerprint: fingerprint,
      member_hash: "b".repeat(64), snapshot_fingerprint: `sha256:${"c".repeat(64)}`, generation: { kind: "native" }, coverage: { configuration: "unconfigured" },
      observed_at: root.created_at, page_size: 100, total: 2, total_bytes: 20, facets: ["collections", "tags", "media_family", "extension", "modified", "size", "text_coverage", "duplicates"].map(dimension => ({ dimension, available: false, reason: "time_budget_exceeded", values: [] })), snapshot: true,
      snapshot_id: "0123456789abcdef0123456789abcdef", created_at: root.created_at, expires_at: "2026-09-20T12:30:00Z",
      rows: [first, second].map(node => ({ node_id: node.id, content_version_id: node.current_version_id, blob_hash: node.blob_hash,
        size: node.size, revision: node.revision, name: node.name, path: node.path, mime_type: node.mime_type, media_family: "text",
        modified_at: node.modified_at, sort_key: node.path, tags: [], collection_ids: [] })) });
    if (url === "/api/v1/search-exports" && init?.method === "POST") {
      const request = JSON.parse(String(init.body)) as Request; submitted.push(request);
      return json({ id: "a".repeat(48), state: "complete", observed_at: root.created_at, expires_at: "2026-09-20T12:30:00Z",
        terms: request.terms, counts: [{ hits: 1, hits_plus_family: 1, unique_hits: 1, unique_families: 1, unique_hits_plus_family: 1 }],
        coverage: { scoped: 1, searchable: 1, missing_text: 0, incomplete_families: 0, fallback_dates: 0 }, unresolved_dates: 0 });
    }
    throw new Error(`unexpected request: ${url}`);
  });
  render(App);
  if (mode === "query page") await fireEvent.click(await screen.findByRole("button", { name: "Run query" }));
  const checkbox = await screen.findByRole("checkbox", { name: mode === "query page" ? "Select /alpha.txt" : mode === "missing identity" ? "Select other-alpha.txt" : "Select alpha.txt" });
  expect(screen.queryByRole("button", { name: "Report selected documents" })).toBeNull();
  await fireEvent.click(checkbox);
  await fireEvent.click(screen.getByRole("button", { name: "Report selected documents" }));
  if (mode === "missing identity") {
    expect(screen.queryByRole("dialog", { name: "Search exports" })).toBeNull();
    expect(await screen.findByText(/Select current documents with complete version identities/)).toBeTruthy();
    expect(submitted).toHaveLength(0);
    return;
  }
  const drawer = await screen.findByRole("dialog", { name: "Search exports" });
  expect(within(drawer).getByText("Selected documents (1)")).toBeTruthy();
  if (mode === "live") {
    const before = listings;
    current = { ...first, current_version_id: second.current_version_id, blob_hash: second.blob_hash, revision: 2 };
    await fireEvent.click(screen.getByRole("button", { name: "Refresh current view" }));
    await waitFor(() => expect(listings).toBeGreaterThan(before));
  }
  await fireEvent.input(within(drawer).getByRole("textbox", { name: "Expression for term 1" }), { target: { value: "alpha" } });
  await fireEvent.click(within(drawer).getByRole("button", { name: "Create export" }));
  await waitFor(() => expect(submitted).toHaveLength(1));
  expect(submitted[0].selected_documents?.documents).toEqual([{ node_id: 1, version_id: "20000000-0000-4000-8000-000000000001", sha256: "a".repeat(64) }]);
  expect(submitted[0].all_documents).toBe(false);
  expect(submitted[0].collection_ids ?? []).toEqual([]);
});
