import { createHash, webcrypto } from "node:crypto";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import type { PreparedAction, PersistedAction } from "./actionJournal.js";
import { batchTagRequestDigest } from "./batch-tags.js";
import { canonicalQuery, type Query } from "./query.js";
import { snapshotMemberHash, type SnapshotPage, type SnapshotRow } from "./snapshots.js";

const journalState = vi.hoisted(() => ({
  action: null as PersistedAction | null,
  pageReadsBeforePrepare: -1,
  pageReads: 0,
}));

const journal = vi.hoisted(() => ({
  async prepare(action: PreparedAction) {
    journalState.pageReadsBeforePrepare = journalState.pageReads;
    journalState.action = {
      ...action,
      state: "prepared",
      checkpoint_verified: false,
      batches: action.batches.map((batch) => ({ ...batch, state: "prepared" })),
    };
  },
  async load() { return journalState.action; },
  async verifyCheckpoint() {},
  async confirmResume() {},
  consumeResumeConfirmation() { return false; },
  async markSending() {},
  async recordReceipt() {},
  async markUncertain() {},
  async markStale() {},
  async pause() {},
  async abandon() { journalState.action = null; },
}));

vi.mock("./actionJournal.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./actionJournal.js")>();
  return { ...actual, ActionJournal: { open: vi.fn(async () => journal) } };
});

import App from "./App.svelte";

const vaultID = "11111111-1111-4111-8111-111111111111";
const tag = { id: "22222222-2222-4222-8222-222222222222", name: "Review", revision: 2, assignment_count: 0 };
const query: Query = {
  v: 1, text: "report", syntax: "simple", mode: "lexical", filters: {},
  sort: { field: "path", direction: "asc" },
};

function row(index: number): SnapshotRow {
  return {
    node_id: index,
    content_version_id: `00000000-0000-4000-8000-${String(index).padStart(12, "0")}`,
    blob_hash: "a".repeat(64),
    size: index,
    revision: 3,
    name: `report-${index}.pdf`,
    path: `/records/report-${index}.pdf`,
    mime_type: "application/pdf",
    media_family: "document",
    modified_at: "2026-09-11T12:00:00Z",
    sort_key: `/records/report-${index}.pdf`,
    tags: [],
    collection_ids: [],
  };
}

async function pages(): Promise<[SnapshotPage, SnapshotPage]> {
  const all = Array.from({ length: 101 }, (_, index) => row(index + 1));
  const memberHash = await snapshotMemberHash(all);
  const authority = {
    query,
    dependencies: [],
    query_fingerprint: `sha256:${createHash("sha256").update(canonicalQuery(query)).digest("hex")}`,
    member_hash: memberHash,
    snapshot_fingerprint: `sha256:${"c".repeat(64)}`,
    generation: { kind: "native" as const },
    coverage: { configuration: "unconfigured" as const },
    observed_at: "2026-09-11T12:34:00Z",
    page_size: 100 as const,
    total: 101,
    total_bytes: all.reduce((sum, item) => sum + item.size, 0),
    facets: ["collections", "tags", "media_family", "extension", "modified", "size", "text_coverage", "duplicates"].map((dimension) => ({
      dimension: dimension as SnapshotPage["facets"][number]["dimension"], available: false,
      reason: "time_budget_exceeded", values: [],
    })),
    snapshot: true as const,
    snapshot_id: "0123456789abcdef0123456789abcdef",
    created_at: "2026-09-11T12:34:00Z",
    expires_at: "2026-09-11T13:04:00Z",
  };
  return [
    { ...authority, rows: all.slice(0, 100), next_cursor: "next-page" },
    { ...authority, rows: all.slice(100), previous_cursor: "previous-page" },
  ];
}

beforeEach(() => {
  journalState.action = null;
  journalState.pageReads = 0;
  journalState.pageReadsBeforePrepare = -1;
  vi.stubGlobal("crypto", webcrypto);
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
});

afterEach(() => {
  cleanup();
  history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

it("captures the complete first-page population before preparing a whole-query action", async () => {
  history.replaceState(null, "", `/#web_session=fresh-session&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(query))}`);
  const [first, second] = await pages();
  const requests: string[] = [];
  const root = { id: 1000, name: "", kind: "dir", path: "/", revision: 1, size: 0, created_at: "2026-09-11T00:00:00Z", modified_at: "2026-09-11T00:00:00Z" };
  const json = (value: unknown) => new Response(JSON.stringify(value), { headers: { "Content-Type": "application/json" } });
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    requests.push(url);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1000/children?limit=1000&offset=0") return json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/tags?limit=1000&offset=0") return json({ items: [tag], total: 1, limit: 1000, offset: 0 });
    if (url === `/api/v1/tags/${tag.id}`) return json(tag);
    if (url === "/api/v1/queries/parse") return json({ query, query_fingerprint: first.query_fingerprint, dependencies: [] });
    if (url === "/api/v1/workspace/queries") return json(first);
    if (url.includes("/pages")) { journalState.pageReads++; return json(second); }
    if (url === "/api/v1/audit/status") return json({ vault_id: vaultID });
    throw new Error(`unexpected request: ${url} ${String(init?.method)}`);
  });

  render(App);
  await screen.findByRole("region", { name: "Query editor" });
  await fireEvent.click(screen.getByRole("button", { name: "Run query" }));
  await screen.findByRole("cell", { name: "/records/report-1.pdf" });
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select /records/report-1.pdf" }));
  expect(screen.getByText("1 selected on this frozen page")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Tag whole query" }));
  await fireEvent.click(screen.getByRole("combobox", { name: /Tag for snapshot action/ }));
  await fireEvent.click(screen.getByRole("option", { name: "Review" }));
  await fireEvent.click(screen.getByRole("button", { name: "Add tag to whole query" }));

  await screen.findByRole("dialog", { name: "Recoverable snapshot action" });
  expect(journalState.pageReadsBeforePrepare).toBe(1);
  expect(journalState.action?.total).toBe(101);
  expect(journalState.action?.source.member_hash).toBe(first.member_hash);
  expect(journalState.action?.batches.flatMap((batch) => batch.members).map((item) => item.node_id))
    .toEqual(Array.from({ length: 101 }, (_, index) => index + 1));
  expect(requests.filter((url) => url === "/api/v1/batch/tags")).toHaveLength(0);
});

it("shows validated receipt overlays without refreshing frozen membership, counts, order, or hash", async () => {
  history.replaceState(null, "", `/#web_session=fresh-session&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(query))}`);
  const [first] = await pages();
  const before = {
    member_hash: first.member_hash,
    total: first.total,
    rows: first.rows.map((item) => item.node_id),
  };
  const requests: string[] = [];
  let changed = false;
  const root = { id: 1000, name: "", kind: "dir", path: "/", revision: 1, size: 0, created_at: "2026-09-11T00:00:00Z", modified_at: "2026-09-11T00:00:00Z" };
  const json = (value: unknown) => new Response(JSON.stringify(value), { headers: { "Content-Type": "application/json" } });
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    requests.push(url);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1000/children?limit=1000&offset=0") return json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/tags?limit=1000&offset=0") return json({ items: [tag], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/queries/parse") return json({ query, query_fingerprint: first.query_fingerprint, dependencies: [] });
    if (url === "/api/v1/workspace/queries") return json(first);
    if (url === "/api/v1/batch/tags/preview") return json({ tag_id: tag.id, tag_revision: changed ? 3 : 2,
      nodes: [{ node_id: 1, revision: changed ? 4 : 3, assigned: changed }] });
    if (url === "/api/v1/batch/tags") {
      const request = JSON.parse(String(init?.body));
      changed = true;
      return json({ version: 1, operation_id: request.operation_id,
        request_digest: await batchTagRequestDigest(request), tag_id: tag.id, assign: true,
        tag_revision: 3, assignment_count: 1, completed_at: "2026-09-11T13:00:00.000000000Z",
        nodes: [{ node_id: 1, expected_revision: 3, revision: 4, changed: true }] });
    }
    throw new Error(`unexpected request: ${url}`);
  });

  render(App);
  await screen.findByRole("region", { name: "Query editor" });
  await fireEvent.click(screen.getByRole("button", { name: "Run query" }));
  await screen.findByRole("cell", { name: "/records/report-1.pdf" });
  const table = screen.getByRole("table", { name: "Snapshot documents" });
  const beforeRows = within(table).getAllByRole("row").slice(1).map((tableRow) => tableRow.textContent);
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select /records/report-1.pdf" }));
  await fireEvent.click(screen.getByRole("button", { name: "Tag visible selection" }));
  await fireEvent.click(screen.getByRole("combobox", { name: /Tag for selected documents/ }));
  await fireEvent.click(screen.getByRole("option", { name: "Review" }));
  await screen.findByText("0 of 1 selected documents have this tag.");
  await fireEvent.click(screen.getByRole("button", { name: "Add to all" }));

  await screen.findByText("Added: Review");
  const afterRows = within(table).getAllByRole("row").slice(1).map((tableRow) => tableRow.textContent);
  const after = { member_hash: first.member_hash, total: first.total, rows: first.rows.map((item) => item.node_id) };
  expect(after.member_hash).toBe(before.member_hash);
  expect(after.total).toBe(before.total);
  expect(after.rows).toEqual(before.rows);
  expect(afterRows).toEqual(beforeRows);
  expect(requests.filter((url) => url === "/api/v1/workspace/queries")).toHaveLength(1);
  expect(requests.some((url) => url.startsWith("/api/v1/search"))).toBe(false);
});
