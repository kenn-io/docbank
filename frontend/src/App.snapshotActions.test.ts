import { createHash, webcrypto } from "node:crypto";
import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import type { PreparedAction } from "./actionRecovery.js";
import type { PersistedAction } from "./actionJournal.js";
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
  async load() {
    return journalState.action;
  },
  async loadBatch(index: number) {
    const action = journalState.action!;
    return {
      action_id: action.action_id,
      state: action.state,
      checkpoint_verified: action.checkpoint_verified,
      batch: action.batches[index],
    };
  },
  async verifyCheckpoint() {},
  async confirmResume() {},
  consumeResumeConfirmation() {
    return false;
  },
  async markSending() {},
  async recordReceipt() {},
  async markUncertain() {},
  async markStale() {},
  async pause() {},
  async discardUnsent(actionID: string) {
    if (
      journalState.action?.action_id === actionID &&
      journalState.action.state === "prepared" &&
      journalState.action.batches.every((batch) => batch.state === "prepared")
    )
      journalState.action = null;
  },
  async abandon() {
    journalState.action = null;
  },
}));

vi.mock("./actionJournal.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./actionJournal.js")>();
  return {
    ...actual,
    ActionJournal: {
      open: vi.fn(async () => journal),
      peek: vi.fn(async () => journalState.action),
    },
  };
});

import App from "./App.svelte";

const vaultID = "11111111-1111-4111-8111-111111111111";
const tag = {
  id: "22222222-2222-4222-8222-222222222222",
  name: "Review",
  revision: 2,
  assignment_count: 0,
};
const query: Query = {
  v: 1,
  text: "report",
  syntax: "simple",
  mode: "lexical",
  filters: {},
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
      reason: "time_budget_exceeded",
      values: [],
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
});

afterEach(() => {
  cleanup();
  history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

it.each([false, true])(
  "captures all targets and keeps recovery reachable when the tag was deleted: %s",
  async (deleted) => {
    history.replaceState(
      null,
      "",
      `/#web_session=fresh-session&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(query))}`,
    );
    const [first, second] = await pages();
    const requests: string[] = [];
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
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      const url = String(input);
      requests.push(url);
      if (url === "/api/v1/path?path=%2F") return json(root);
      if (url === "/api/v1/nodes/1000/children?limit=1000&offset=0")
        return json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
      if (url === "/api/v1/tags?limit=1000&offset=0")
        return json({ items: [tag], total: 1, limit: 1000, offset: 0 });
      if (url === `/api/v1/tags/${tag.id}`)
        return deleted
          ? new Response(JSON.stringify({ code: "not_found" }), { status: 404 })
          : json(tag);
      if (url === "/api/v1/queries/parse")
        return json({ query, query_fingerprint: first.query_fingerprint, dependencies: [] });
      if (url === "/api/v1/workspace/queries") return json(first);
      if (url.includes("/pages")) {
        journalState.pageReads++;
        return json(second);
      }
      if (url === "/api/v1/audit/status") return json({ vault_id: vaultID });
      throw new Error(`unexpected request: ${url} ${String(init?.method)}`);
    });

    render(App);
    await screen.findByRole("dialog", { name: "Filters and sort" });
    await fireEvent.click(screen.getByRole("button", { name: "Search" }));
    await screen.findByRole("cell", { name: "/records/report-1.pdf" });
    await fireEvent.click(screen.getByRole("checkbox", { name: "Select /records/report-1.pdf" }));
    expect(screen.getByText("1 selected")).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Tag all…" }));
    await fireEvent.input(screen.getByRole("combobox", { name: "Tag" }), {
      target: { value: "Rev" },
    });
    await fireEvent.click(screen.getByRole("option", { name: "Review (0)" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Continue" }));

    await screen.findByRole("button", { name: "Apply" }, { timeout: 10_000 });
    expect(journalState.pageReadsBeforePrepare).toBe(1);
    expect(journalState.action?.total).toBe(101);
    expect(journalState.action?.source.member_hash).toBe(first.member_hash);
    expect(
      journalState.action?.batches.flatMap((batch) => batch.members).map((item) => item.node_id),
    ).toEqual(Array.from({ length: 101 }, (_, index) => index + 1));
    expect(requests.filter((url) => url === "/api/v1/batch/tags")).toHaveLength(0);
    if (deleted) {
      await screen.findByText("This tag no longer exists.");
      await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
      await waitFor(() => expect(journalState.action).toBeNull());
    }
  },
);

it("shows validated receipt overlays without refreshing frozen membership, counts, order, or hash", async () => {
  history.replaceState(
    null,
    "",
    `/#web_session=fresh-session&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(query))}`,
  );
  const [first, second] = await pages();
  const before = {
    member_hash: first.member_hash,
    total: first.total,
    rows: first.rows.map((item) => item.node_id),
  };
  const requests: string[] = [];
  let changed = false;
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
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    requests.push(url);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1000/children?limit=1000&offset=0")
      return json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/tags?limit=1000&offset=0")
      return json({ items: [tag], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/queries/parse")
      return json({ query, query_fingerprint: first.query_fingerprint, dependencies: [] });
    if (url === "/api/v1/workspace/queries") return json(first);
    if (url.includes("/pages")) return json(second);
    if (url === "/api/v1/audit/status") return json({ vault_id: vaultID });
    if (url === `/api/v1/tags/${tag.id}`) return json(tag);
    if (url === "/api/v1/batch/tags/preview")
      return json({
        tag_id: tag.id,
        tag_revision: changed ? 3 : 2,
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
        tag_revision: 3,
        assignment_count: 1,
        completed_at: "2026-09-11T13:00:00.000000000Z",
        nodes: [{ node_id: 1, expected_revision: 3, revision: 4, changed: true }],
      });
    }
    throw new Error(`unexpected request: ${url}`);
  });

  render(App);
  await screen.findByRole("dialog", { name: "Filters and sort" });
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await screen.findByRole("cell", { name: "/records/report-1.pdf" });
  const table = screen.getByRole("table", { name: "Snapshot documents" });
  const beforeRows = within(table)
    .getAllByRole("row")
    .slice(1)
    .map((tableRow) => tableRow.textContent);
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select /records/report-1.pdf" }));
  await fireEvent.click(screen.getByRole("button", { name: "Tag selected…" }));
  await fireEvent.input(screen.getByRole("combobox", { name: "Tag" }), {
    target: { value: "Rev" },
  });
  await fireEvent.click(screen.getByRole("option", { name: "Review (0)" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Apply" }));

  await screen.findByText("Added: Review", {}, { timeout: 10_000 });
  const afterRows = within(table)
    .getAllByRole("row")
    .slice(1)
    .map((tableRow) => tableRow.textContent);
  const after = {
    member_hash: first.member_hash,
    total: first.total,
    rows: first.rows.map((item) => item.node_id),
  };
  expect(after.member_hash).toBe(before.member_hash);
  expect(after.total).toBe(before.total);
  expect(after.rows).toEqual(before.rows);
  expect(afterRows).toEqual(beforeRows);
  expect(requests.filter((url) => url === "/api/v1/workspace/queries")).toHaveLength(1);
  expect(requests.some((url) => url.startsWith("/api/v1/search"))).toBe(false);
  await screen.findByText("Added “Review” to 1 document.");
  await fireEvent.click(screen.getByRole("button", { name: "Done" }));
  await fireEvent.click(screen.getByRole("button", { name: "Tag all…" }));
  await fireEvent.input(screen.getByRole("combobox", { name: "Tag" }), {
    target: { value: "Rev" },
  });
  await fireEvent.click(screen.getByRole("option", { name: "Review (0)" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Continue" }));
  await screen.findByRole("button", { name: "Apply" }, { timeout: 10_000 });
  expect(journalState.action?.batches[0].request.nodes[0]).toEqual({ node_id: 1, revision: 4 });
  expect(first.rows[0].revision).toBe(3);
});

it.each([
  { pending: "pages", discard: "Clear search" },
  { pending: "audit", discard: "Search again" },
  { pending: "tag", discard: "Lock web session" },
  { pending: "pages", discard: "Cancel" },
])("discards a capture waiting for $pending after $discard", async ({ pending, discard }) => {
  history.replaceState(
    null,
    "",
    `/#web_session=fresh-session&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(query))}`,
  );
  const [first, second] = await pages();
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
  let release!: (response: Response) => void;
  const delayed = new Promise<Response>((resolve) => {
    release = resolve;
  });
  let waiting = false;
  let captureSignal: AbortSignal | undefined;
  // Set once Continue is pressed, so the searches before it are not held.
  let armed = false;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url.includes("/children?"))
      return json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/tags?limit=1000&offset=0")
      return json({ items: [tag], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/queries/parse")
      return json({ query, query_fingerprint: first.query_fingerprint, dependencies: [] });
    if (url === "/api/v1/workspace/queries") return json(first);
    const stage = url.includes("/pages")
      ? "pages"
      : url === "/api/v1/audit/status"
        ? "audit"
        : url === `/api/v1/tags/${tag.id}`
          ? "tag"
          : undefined;
    if (armed && (stage === "pages" || stage === "audit"))
      captureSignal = init?.signal ?? undefined;
    if (armed && stage === pending) {
      waiting = true;
      return delayed;
    }
    if (stage)
      return json(stage === "pages" ? second : stage === "audit" ? { vault_id: vaultID } : tag);
    return json({});
  });
  render(App);
  await screen.findByRole("dialog", { name: "Filters and sort" });
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await screen.findByRole("cell", { name: "/records/report-1.pdf" });
  await fireEvent.click(screen.getByRole("button", { name: "Tag all…" }));
  await fireEvent.input(screen.getByRole("combobox", { name: "Tag" }), {
    target: { value: "Rev" },
  });
  await fireEvent.click(screen.getByRole("option", { name: "Review (0)" }));
  const next = await screen.findByRole("button", { name: "Continue" });
  armed = true;
  await fireEvent.click(next);
  await waitFor(() => expect(waiting).toBe(true));
  try {
    if (discard === "Search again") {
      await fireEvent.click(screen.getByRole("button", { name: "Advanced", hidden: true }));
      discard = "Search";
    }
    await fireEvent.click(screen.getAllByRole("button", { name: discard, hidden: true }).at(-1)!);
    expect(captureSignal?.aborted).toBe(true);
  } finally {
    // Deliberately deliver the old response even after abort to exercise the
    // operation guard as well as cancellation of the transport.
    release(json(pending === "pages" ? second : pending === "audit" ? { vault_id: vaultID } : tag));
  }
  await delayed;
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(screen.queryByRole("button", { name: "Apply" })).toBeNull();
  if (pending !== "tag") expect(journalState.action).toBeNull();
});

it("disables tagging during reruns and ignores the old selection's late receipt", async () => {
  history.replaceState(
    null,
    "",
    `/#web_session=fresh-session&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(query))}`,
  );
  const [first] = await pages();
  const replacement = { ...first, snapshot_id: "1123456789abcdef0123456789abcdef" };
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
  let finishRun!: (response: Response) => void;
  const rerun = new Promise<Response>((resolve) => {
    finishRun = resolve;
  });
  let finishMutation!: (response: Response) => void;
  const mutation = new Promise<Response>((resolve) => {
    finishMutation = resolve;
  });
  let creates = 0;
  let confirmedReceipt: unknown;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url.includes("/children?"))
      return json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
    if (url === "/api/v1/tags?limit=1000&offset=0")
      return json({ items: [tag], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/queries/parse")
      return json({ query, query_fingerprint: first.query_fingerprint, dependencies: [] });
    if (url === "/api/v1/workspace/queries") return ++creates === 1 ? json(first) : rerun;
    if (url === "/api/v1/audit/status") return json({ vault_id: vaultID });
    if (url === "/api/v1/batch/tags/preview")
      return json({
        tag_id: tag.id,
        tag_revision: 2,
        nodes: [{ node_id: 1, revision: 3, assigned: false }],
      });
    if (url === "/api/v1/batch/tags") {
      const request = JSON.parse(String(init?.body));
      confirmedReceipt = {
        version: 1,
        operation_id: request.operation_id,
        request_digest: await batchTagRequestDigest(request),
        tag_id: tag.id,
        assign: true,
        tag_revision: 3,
        assignment_count: 1,
        completed_at: "2026-09-11T13:00:00.000000000Z",
        nodes: [{ node_id: 1, expected_revision: 3, revision: 4, changed: true }],
      };
      return mutation;
    }
    throw new Error(`unexpected request: ${url}`);
  });
  render(App);
  await screen.findByRole("dialog", { name: "Filters and sort" });
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await screen.findByRole("cell", { name: "/records/report-1.pdf" });
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select /records/report-1.pdf" }));
  await fireEvent.click(screen.getByRole("button", { name: "Tag selected…" }));
  await fireEvent.input(screen.getByRole("combobox", { name: "Tag" }), {
    target: { value: "Rev" },
  });
  await fireEvent.click(screen.getByRole("option", { name: "Review (0)" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Apply" }));
  await waitFor(() => expect(confirmedReceipt).toBeDefined());
  await fireEvent.click(screen.getByRole("button", { name: "Advanced", hidden: true }));
  await fireEvent.click(screen.getByRole("button", { name: "Search", hidden: true }));
  await waitFor(() => expect(creates).toBe(2));
  try {
    expect(
      (screen.getByRole("button", { name: "Tag selected…" }) as HTMLButtonElement).disabled,
    ).toBe(true);
    expect((screen.getByRole("button", { name: "Tag all…" }) as HTMLButtonElement).disabled).toBe(
      true,
    );
    expect(screen.queryByRole("dialog", { name: "Tag selected documents" })).toBeNull();
  } finally {
    finishRun(json(replacement));
    await screen.findByText(replacement.snapshot_id);
    finishMutation(json(confirmedReceipt));
  }
  await mutation;
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(screen.queryByText(/^Added “Review” to \d+ documents?\.$/)).toBeNull();
});

it.each(["warning", "draft"])(
  "preserves the %s after an uncertain selection tag refresh",
  async (check) => {
    history.replaceState(
      null,
      "",
      `/#web_session=fresh-session&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(query))}`,
    );
    const [first] = await pages();
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
    const searches: Record<string, unknown>[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      const url = String(input);
      if (url === "/api/v1/path?path=%2F") return json(root);
      if (url.includes("/children?"))
        return json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
      if (url === "/api/v1/tags?limit=1000&offset=0")
        return json({ items: [tag], total: 1, limit: 1000, offset: 0 });
      if (url === "/api/v1/queries/parse")
        return json({ query, query_fingerprint: first.query_fingerprint, dependencies: [] });
      if (url === "/api/v1/workspace/queries") {
        searches.push(JSON.parse(String(init?.body)));
        return json(first);
      }
      if (url === "/api/v1/batch/tags/preview")
        return json({
          tag_id: tag.id,
          tag_revision: 2,
          nodes: [{ node_id: 1, revision: 3, assigned: false }],
        });
      // A server error leaves the client unable to confirm whether the change committed.
      if (url === "/api/v1/batch/tags") return new Response("unavailable", { status: 503 });
      throw new Error(`unexpected request: ${url}`);
    });
    render(App);
    await fireEvent.click(await screen.findByRole("button", { name: "Search" }));
    await screen.findByRole("cell", { name: "/records/report-1.pdf" });
    await fireEvent.input(screen.getByRole("searchbox", { name: "Search documents" }), {
      target: { value: "unfinished draft" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
    await fireEvent.input(screen.getByLabelText("Filters JSON"), {
      target: { value: '{"size_min":' },
    });
    await fireEvent.click(screen.getByRole("combobox", { name: "Documents per page: 100" }));
    await fireEvent.click(screen.getByRole("option", { name: "250" }));
    await fireEvent.keyDown(window, { key: "Escape" });
    await fireEvent.click(screen.getByRole("checkbox", { name: "Select /records/report-1.pdf" }));
    await fireEvent.click(screen.getByRole("button", { name: "Tag selected…" }));
    await fireEvent.input(screen.getByRole("combobox", { name: "Tag" }), {
      target: { value: "Rev" },
    });
    await fireEvent.click(screen.getByRole("option", { name: "Review (0)" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Apply" }));
    await waitFor(() => expect(searches).toHaveLength(2));
    await screen.findByRole("cell", { name: "/records/report-1.pdf" });
    expect(searches[1]).toEqual(searches[0]);
    if (check === "warning") {
      const warning = await screen.findByText(
        "The result is unknown. Check these documents' tags before trying again.",
      );
      expect(warning.closest('[role="alert"]')).toBeTruthy();
      await fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
      expect(
        screen.queryByText(
          "The result is unknown. Check these documents' tags before trying again.",
        ),
      ).toBeNull();
    } else {
      const dismiss = screen.queryByRole("button", { name: "Dismiss" });
      if (dismiss) await fireEvent.click(dismiss);
      expect(
        (screen.getByRole("searchbox", { name: "Search documents" }) as HTMLInputElement).value,
      ).toBe("unfinished draft");
      await fireEvent.focusIn(screen.getByRole("button", { name: "Advanced" }));
      expect((await screen.findByRole("tooltip")).textContent).toBe("Unapplied changes");
      await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
      expect((screen.getByLabelText("Filters JSON") as HTMLTextAreaElement).value).toBe(
        '{"size_min":',
      );
      expect(screen.getByRole("combobox", { name: "Documents per page: 250" })).toBeTruthy();
    }
  },
);
