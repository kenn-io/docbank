import * as actions from "./action-events.js";
import { createHash } from "node:crypto";
import { afterEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import App from "./App.svelte";
import { snapshotEnvelope } from "./snapshot-test-fixtures.js";
import { parseQuery } from "./query.js";
import type { SnapshotRow } from "./snapshots.js";
import type { Node } from "./generated/docbank.js";

const vaultID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const baseVersion = "11111111-1111-4111-8111-111111111111";
const rerankedVersion = "22222222-2222-4222-8222-222222222222";
const tagID = "33333333-3333-4333-8333-333333333333";

afterEach(() => {
  cleanup();
  history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

function json(value: unknown): Response {
  return new Response(JSON.stringify(value), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

function fenceFingerprint(vault: string, versions: string[]): string {
  const bytes: number[] = [];
  const append = (value: Uint8Array) => bytes.push(...value);
  const length = (value: number) =>
    append(
      Uint8Array.from([
        (value >>> 24) & 0xff,
        (value >>> 16) & 0xff,
        (value >>> 8) & 0xff,
        value & 0xff,
      ]),
    );
  const encoder = new TextEncoder();
  append(encoder.encode("docbank-document-source-fence/v1"));
  const vaultBytes = encoder.encode(vault);
  length(vaultBytes.length);
  append(vaultBytes);
  length(versions.length);
  for (const version of [...versions].sort()) {
    const versionBytes = encoder.encode(version);
    length(versionBytes.length);
    append(versionBytes);
  }
  return `sha256:${createHash("sha256").update(Uint8Array.from(bytes)).digest("hex")}`;
}

function node(id: number, name: string, version: string) {
  return {
    id,
    parent_id: 1,
    name,
    kind: "file",
    current_version_id: version,
    blob_hash: String.fromCharCode(96 + id).repeat(64),
    size: 32,
    mime_type: "text/plain",
    revision: 1,
    created_at: "2026-09-21T00:00:00Z",
    modified_at: "2026-09-21T00:00:00Z",
    path: `/${name}`,
  };
}

function result(file: ReturnType<typeof node>, rank: number, excerpt: string) {
  return {
    vault_uid: vaultID,
    node_id: file.id,
    content_version_id: file.current_version_id,
    rank,
    score: 1 / rank,
    path: file.path,
    excerpt,
    lexical_rank: rank,
    evidence: [{ kind: "content_blob" }],
  };
}

function report(
  mode: "lexical" | "hybrid",
  results: unknown[],
  reranking?: unknown,
  degradations: string[] = [],
) {
  return {
    requested_mode: mode,
    actual_mode: mode,
    coverage: {
      binding_required: mode === "hybrid",
      scoped_documents: 2,
      complete_documents: 2,
      state: "complete",
    },
    degradations,
    results,
    truncated: false,
    trace: [],
    ...(reranking === undefined ? {} : { reranking }),
  };
}

interface HarnessOptions {
  profiles: unknown[];
  profilesStatus?: number;
  profilesPending?: boolean;
  files: (ReturnType<typeof node> | Node)[];
  baseReport: unknown;
  baseSearchStatus?: number;
  legacySearchStatus?: number;
  legacyFailureAfter?: number;
  legacyFailurePending?: boolean;
  basePending?: boolean;
  legacyPending?: boolean;
  childrenPending?: boolean;
  rerankReport?: unknown;
  rerankPending?: boolean;
  // A pending rerank rejects on abort, as fetch does, instead of resolving later.
  rerankAbortable?: boolean;
  rerankNode?: ReturnType<typeof node>;
  emptyFence?: boolean;
  nodeFailure?: { id: number; status: number };
  tagSearch?: unknown;
  snapshotRows?: SnapshotRow[];
}

function installHarness(options: HarnessOptions) {
  history.replaceState(null, "", "/#web_session=short-lived&web_upload_secret=proof");
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
    created_at: "2026-09-21T00:00:00Z",
    modified_at: "2026-09-21T00:00:00Z",
    path: "/",
  };
  const postBodies: Record<string, unknown>[] = [];
  const fenceBodies: Record<string, unknown>[] = [];
  const misses: Record<string, unknown>[] = [];
  let rerankResolve: ((response: Response) => void) | undefined;
  let baseResolve: ((response: Response) => void) | undefined;
  let profilesResolve: ((response: Response) => void) | undefined;
  let legacyResolve: ((response: Response) => void) | undefined;
  let legacyFailureResolve: ((response: Response) => void) | undefined;
  let childrenResolve: ((response: Response) => void) | undefined;
  const basePending = new Promise<Response>((resolve) => {
    baseResolve = resolve;
  });
  const profilesPending = new Promise<Response>((resolve) => {
    profilesResolve = resolve;
  });
  const legacyPending = new Promise<Response>((resolve) => {
    legacyResolve = resolve;
  });
  const legacyFailurePending = new Promise<Response>((resolve) => {
    legacyFailureResolve = resolve;
  });
  const childrenPending = new Promise<Response>((resolve) => {
    childrenResolve = resolve;
  });
  let baseCalls = 0;
  let legacyCalls = 0;
  const nodeCalls = new Map<number, number>();
  const rerankPending = new Promise<Response>((resolve) => {
    rerankResolve = resolve;
  });
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    const method = init?.method ?? "GET";
    if (url === "/api/v1/miss-log" && method === "POST") {
      misses.push(JSON.parse(String(init?.body)));
      return new Response(null, { status: 204 });
    }
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1/children?limit=1000&offset=0") {
      return json({
        directory: root,
        items: options.files,
        total: options.files.length,
        limit: 1000,
        offset: 0,
      });
    }
    if (url === "/api/v1/nodes/3/children?limit=1000&offset=0") {
      if (options.childrenPending) return childrenPending;
      return json({
        directory: options.files.find((file) => file.id === 3) ?? root,
        items: [],
        total: 0,
        limit: 1000,
        offset: 0,
      });
    }
    if (url === "/api/v1/tags?limit=1000&offset=0") {
      return json({
        items: [{ id: tagID, name: "reviewed", revision: 1, assignment_count: 1 }],
        total: 1,
        limit: 1000,
        offset: 0,
      });
    }
    if (url === `/api/v1/tags/${tagID}/nodes?limit=1000&offset=0&live_only=true`) {
      return json({
        items: options.files.map((file) => ({ node: file, path: file.path })),
        total: options.files.length,
        limit: 1000,
        offset: 0,
        omitted_trashed: 0,
      });
    }
    if (url === "/api/v1/workspace/queries") {
      const body = JSON.parse(String(init?.body));
      return json(
        await snapshotEnvelope(parseQuery(JSON.stringify(body.query)), options.snapshotRows ?? [], {
          page_size: body.page_size,
          facets: (body.facets ?? []).map((dimension: string) => ({
            dimension,
            available: true,
            total: options.snapshotRows?.length ?? 0,
            missing: 0,
            other: 0,
            values: [],
          })),
        }),
      );
    }
    if (url === "/api/v1/queries/parse")
      return json({
        query: JSON.parse(String(init?.body)),
        query_fingerprint: `sha256:${createHash("sha256").update(String(init?.body)).digest("hex")}`,
        dependencies: [],
      });
    if (url === "/api/v1/processing/profiles") {
      if (options.profilesPending) return profilesPending;
      return options.profilesStatus
        ? new Response("profile failure", { status: options.profilesStatus })
        : json(options.profiles);
    }
    const nodeMatch = url.match(/^\/api\/v1\/nodes\/(\d+)$/);
    if (nodeMatch) {
      const id = Number(nodeMatch[1]);
      if (id === options.nodeFailure?.id)
        return new Response("node failure", { status: options.nodeFailure.status });
      const calls = nodeCalls.get(id) ?? 0;
      nodeCalls.set(id, calls + 1);
      const file = options.files.find((item) => item.id === id);
      const current = options.rerankNode?.id === id && calls > 0 ? options.rerankNode : file;
      return current ? json(current) : new Response("", { status: 404 });
    }
    if (url === "/api/v1/processing/source-fences/resolve" && method === "POST") {
      fenceBodies.push(JSON.parse(String(init?.body ?? "{}")) as Record<string, unknown>);
      const ids = options.emptyFence ? [] : [baseVersion, rerankedVersion];
      return json({
        fence: { vault_uid: vaultID, content_version_ids: ids },
        observed_scope_count: ids.length,
        fence_fingerprint: fenceFingerprint(vaultID, ids),
      });
    }
    if (url === "/api/v1/search" && method === "POST") {
      const body = JSON.parse(String(init?.body ?? "{}")) as Record<string, unknown>;
      postBodies.push(body);
      if (options.baseSearchStatus)
        return new Response("search failure", { status: options.baseSearchStatus });
      if (body.rerank) {
        if (options.rerankPending && options.rerankAbortable)
          return new Promise<Response>((_, reject) =>
            init?.signal?.addEventListener("abort", () =>
              reject(new DOMException("Aborted", "AbortError")),
            ),
          );
        if (options.rerankPending) return rerankPending.then((response) => response.clone());
        return json(options.rerankReport ?? options.baseReport);
      }
      baseCalls += 1;
      if (options.basePending && baseCalls === 1) return basePending;
      return json(options.baseReport);
    }
    if (url.startsWith("/api/v1/search?") && method === "GET") {
      legacyCalls += 1;
      if (options.legacyFailureAfter !== undefined && legacyCalls > options.legacyFailureAfter) {
        if (options.legacyFailurePending) return legacyFailurePending;
        return new Response("legacy search failure", { status: 500 });
      }
      if (options.legacyPending && legacyCalls === 1) return legacyPending;
      if (options.legacySearchStatus)
        return new Response("legacy search failure", { status: options.legacySearchStatus });
      return json(
        options.tagSearch ?? {
          hits: [],
          limit: 1000,
          truncated: false,
          tag_id: new URL(url, "https://example.com").searchParams.get("tag_id") ?? undefined,
        },
      );
    }
    if (url.startsWith("/api/v1/audit/status?")) return json({ enabled: false, scopes: [] });
    if (url.startsWith("/api/v1/nodes/") && url.includes("/tags?"))
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    throw new Error(`unexpected request: ${method} ${url}`);
  });
  return {
    fetchMock,
    misses,
    postBodies,
    fenceBodies,
    resolveRerank: (response: Response) => rerankResolve?.(response),
    resolveBase: (response: Response) => baseResolve?.(response),
    resolveProfiles: (response: Response) => profilesResolve?.(response),
    resolveLegacy: (response: Response) => legacyResolve?.(response),
    resolveLegacyFailure: (response: Response) => legacyFailureResolve?.(response),
    resolveChildren: (response: Response) => childrenResolve?.(response),
    legacyCalls: () => legacyCalls,
  };
}

it("switches the top bar between plain words and query syntax", async () => {
  installHarness({ profiles: [], files: [], baseReport: report("lexical", []) });
  render(App);
  const input = await screen.findByRole("searchbox", { name: "Search documents" });
  expect(input.getAttribute("placeholder")).toBe("Search names and text");
  await fireEvent.click(screen.getByRole("combobox", { name: "Search syntax: Plain words" }));
  await fireEvent.click(screen.getByRole("option", { name: "Query syntax" }));
  expect(input.getAttribute("placeholder")).toBe("name:invoice AND tag:urgent");
});

async function submitSearch(value: string): Promise<void> {
  const input = await screen.findByRole("searchbox", { name: "Search documents" });
  await fireEvent.input(input, { target: { value } });
  await fireEvent.submit(input.closest("form")!);
}

it.each([
  [true, false],
  [true, true],
  [false, false],
  [false, true],
])(
  "keeps folder name matches with embeddings=%s and live files=%s",
  async (embeddings, liveFiles) => {
    const folder: Node = {
      id: 3,
      parent_id: 1,
      name: "invoices",
      kind: "dir",
      size: 0,
      revision: 1,
      created_at: "2026-09-21T00:00:00Z",
      modified_at: "2026-09-21T00:00:00Z",
      path: "/invoices",
    };
    const file = node(2, "invoices.txt", baseVersion);
    const harness = installHarness({
      profiles: [
        {
          name: "private",
          fingerprint: "a".repeat(64),
          rendition: true,
          embedding_bindings: embeddings ? ["embed"] : [],
          reranking_available: true,
        },
      ],
      files: liveFiles ? [folder, file] : [folder],
      emptyFence: !liveFiles,
      baseReport: report(
        embeddings ? "hybrid" : "lexical",
        liveFiles ? [result(file, 1, "invoice excerpt")] : [],
      ),
      rerankReport: report("hybrid", [result(file, 1, "invoice excerpt")], {
        outcome: "applied",
        candidate_count: 1,
      }),
      tagSearch: {
        hits: [
          { node: folder, path: folder.path, match: "name" },
          ...(liveFiles ? [{ node: file, path: file.path, match: "name" }] : []),
        ],
        limit: 1000,
        truncated: false,
      },
    });
    render(App);
    await screen.findByRole("button", { name: "Open" });
    await submitSearch("invoices");
    expect(await screen.findByRole("cell", { name: "/invoices" })).toBeTruthy();
    if (embeddings && liveFiles) {
      await screen.findByText("invoice excerpt");
      await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
      await screen.findByText("Reranked results applied.");
      expect(screen.getByRole("cell", { name: "/invoices" })).toBeTruthy();
      expect(
        screen.getAllByRole("row").filter((row) => row.textContent?.includes("invoices.txt")),
      ).toHaveLength(1);
      expect(harness.postBodies.at(-1)).toMatchObject({ mode: "hybrid", rerank: true });
    } else {
      expect(screen.queryByText("No documents match the current filter.")).toBeNull();
      expect(harness.postBodies).toHaveLength(0);
      if (!embeddings) expect(harness.legacyCalls()).toBe(1);
    }
    expect(harness.misses).toHaveLength(0);
  },
);

it("keeps names and text on GET without a processing profile", async () => {
  const file = node(2, "existing.txt", baseVersion);
  const { fetchMock } = installHarness({
    profiles: [],
    files: [file],
    baseReport: report("lexical", [result(file, 1, "unused")]),
    tagSearch: {
      hits: [{ node: file, path: file.path, match: "content" }],
      limit: 1000,
      truncated: false,
      tag_id: tagID,
    },
  });
  render(App);
  await screen.findAllByText("existing.txt");
  await screen.findByRole("button", { name: "Move to trash" });
  await fireEvent.click(
    await screen.findByRole("combobox", { name: "Browse or filter by tag: All tags" }),
  );
  await fireEvent.click(screen.getByRole("option", { name: "reviewed (1)" }));
  await submitSearch("existing");
  await screen.findByText("/existing.txt");
  expect(
    fetchMock.mock.calls.some(
      ([input, init]) =>
        String(input).startsWith("/api/v1/search?") && (init?.method ?? "GET") === "GET",
    ),
  ).toBe(true);
  const searchURL = fetchMock.mock.calls
    .map(([input]) => String(input))
    .find((input) => input.startsWith("/api/v1/search?"));
  expect(searchURL).toContain("limit=1000");
  expect(searchURL).toContain(`tag_id=${tagID}`);
  expect(
    fetchMock.mock.calls.some(
      ([input, init]) => String(input) === "/api/v1/search" && init?.method === "POST",
    ),
  ).toBe(false);
  expect(screen.queryByText("Reranking results… Base results are shown.")).toBeNull();
});

it("shows base rows while reranking and applies the validated order", async () => {
  const baseFile = node(2, "base.txt", baseVersion);
  const rerankedFile = node(3, "reranked.txt", rerankedVersion);
  const harness = installHarness({
    profiles: [
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: true,
      },
    ],
    files: [baseFile, rerankedFile],
    baseReport: report("hybrid", [result(baseFile, 1, "base excerpt")]),
    rerankReport: report("hybrid", [result(rerankedFile, 1, "reranked excerpt")], {
      outcome: "applied",
      candidate_count: 1,
    }),
    rerankPending: true,
  });
  render(App);
  await screen.findAllByText("base.txt");
  await screen.findByRole("button", { name: "Move to trash" });
  await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
  await submitSearch("annual report");
  expect(await screen.findByText("base excerpt")).toBeTruthy();
  expect(await screen.findByText("Reranking results… Base results are shown.")).toBeTruthy();
  expect(screen.queryByText("reranked excerpt")).toBeNull();
  harness.resolveRerank(
    json({
      requested_mode: "hybrid",
      actual_mode: "hybrid",
      coverage: {
        binding_required: true,
        scoped_documents: 2,
        complete_documents: 2,
        state: "complete",
      },
      degradations: [],
      results: [result(rerankedFile, 1, "reranked excerpt")],
      truncated: false,
      trace: [],
      reranking: { outcome: "applied", candidate_count: 1 },
    }),
  );
  await screen.findByText("reranked excerpt");
  expect(screen.queryByText("base excerpt")).toBeNull();
  expect(harness.postBodies).toHaveLength(2);
  expect(harness.postBodies[0]?.rerank).toBeUndefined();
  expect(harness.postBodies[1]?.rerank).toBe(true);
});

it("refreshes live nodes before replacing rows with reranked results", async () => {
  const file = node(2, "base.txt", baseVersion);
  const renamed = { ...file, name: "renamed.txt", path: "/renamed.txt" };
  const harness = installHarness({
    profiles: [
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: true,
      },
    ],
    files: [file],
    rerankNode: renamed,
    baseReport: report("hybrid", [result(file, 1, "base excerpt")]),
    rerankReport: report("hybrid", [result(file, 1, "reranked excerpt")], {
      outcome: "applied",
      candidate_count: 1,
    }),
  });
  render(App);
  await screen.findAllByText("base.txt");
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Rerank results" }));
  await submitSearch("annual report");
  await screen.findByText("reranked excerpt");
  expect(screen.getAllByText("/renamed.txt").length).toBeGreaterThan(0);
  expect(harness.postBodies[1]?.rerank).toBe(true);
});

it("ignores a rerank response after switching to Names and text", async () => {
  const baseFile = node(2, "base.txt", baseVersion);
  const rerankedFile = node(3, "reranked.txt", rerankedVersion);
  const harness = installHarness({
    profiles: [
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: true,
      },
    ],
    files: [baseFile, rerankedFile],
    baseReport: report("hybrid", [result(baseFile, 1, "base excerpt")]),
    rerankPending: true,
    tagSearch: {
      hits: [{ node: baseFile, path: baseFile.path, match: "name" }],
      limit: 1000,
      truncated: false,
    },
  });
  render(App);
  await screen.findAllByText("base.txt");
  await screen.findByRole("button", { name: "Move to trash" });
  await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
  await submitSearch("annual report");
  await screen.findByText("Reranking results… Base results are shown.");
  await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
  harness.resolveRerank(
    json({
      requested_mode: "hybrid",
      actual_mode: "hybrid",
      coverage: {
        binding_required: true,
        scoped_documents: 2,
        complete_documents: 2,
        state: "complete",
      },
      degradations: [],
      results: [result(rerankedFile, 1, "reranked excerpt")],
      truncated: false,
      trace: [],
      reranking: { outcome: "applied", candidate_count: 1 },
    }),
  );
  await waitFor(() =>
    expect(screen.queryByText("Reranking results… Base results are shown.")).toBeNull(),
  );
  await waitFor(() => expect(screen.queryByText("reranked excerpt")).toBeNull());
  expect(screen.getByText("/base.txt")).toBeTruthy();
});

it.each([false, true])(
  "reports a search whose rerank is superseded after base results, abort rejects=%s",
  async (rerankAbortable) => {
    const action = vi.spyOn(actions, "reportSearch").mockImplementation(() => {});
    const file = node(2, "base.txt", baseVersion);
    const harness = installHarness({
      profiles: [
        {
          name: "private",
          fingerprint: "a".repeat(64),
          rendition: true,
          embedding_bindings: ["embed"],
          reranking_available: true,
        },
      ],
      files: [file],
      baseReport: report("hybrid", [result(file, 1, "base excerpt")]),
      rerankPending: true,
      rerankAbortable,
      tagSearch: {
        hits: [{ node: file, path: file.path, match: "name" }],
        limit: 1000,
        truncated: false,
      },
    });
    render(App);
    await screen.findAllByText("base.txt");
    await fireEvent.click(await screen.findByRole("checkbox", { name: "Rerank results" }));
    await submitSearch("annual report");
    await screen.findByText("Reranking results… Base results are shown.");
    await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
    if (!rerankAbortable)
      harness.resolveRerank(
        json(
          report("hybrid", [result(file, 1, "reranked excerpt")], {
            outcome: "applied",
            candidate_count: 1,
          }),
        ),
      );
    await waitFor(() => expect(action.mock.calls).toEqual([["short-lived", "hybrid", false]]));
  },
);

it("reports completed searches and a failed submission once, not reruns", async () => {
  const searchAction = vi.spyOn(actions, "reportSearch").mockImplementation(() => {});
  const failure = vi.spyOn(actions, "reportFailure").mockImplementation(() => {});
  const file = node(2, "base.txt", baseVersion);
  const options: HarnessOptions = {
    profiles: [
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: true,
      },
    ],
    files: [file],
    baseReport: report("hybrid", [result(file, 1, "base excerpt")]),
    rerankPending: true,
  };
  const harness = installHarness(options);
  render(App);
  await screen.findAllByText("base.txt");
  const toggle = await screen.findByRole("checkbox", { name: "Rerank results" });
  await fireEvent.click(toggle);
  await submitSearch("annual report");
  await screen.findByText("Reranking results… Base results are shown.");
  expect(screen.getByText("base excerpt")).toBeTruthy();
  expect(searchAction.mock.calls).toEqual([]);
  harness.resolveRerank(
    json(
      report("hybrid", [result(file, 1, "reranked excerpt")], {
        outcome: "applied",
        candidate_count: 1,
      }),
    ),
  );
  await waitFor(() =>
    expect(screen.queryByText("Reranking results… Base results are shown.")).toBeNull(),
  );
  await screen.findByText("reranked excerpt");
  await waitFor(() => expect(searchAction.mock.calls).toEqual([["short-lived", "hybrid", false]]));
  await fireEvent.click(toggle);
  await screen.findByText("base excerpt");
  // Refresh and settings changes rerun the accepted search without a new action.
  await fireEvent.click(screen.getByRole("button", { name: "Refresh current view" }));
  await waitFor(() => expect(harness.postBodies.filter((body) => !body.rerank)).toHaveLength(2));
  await screen.findByText("base excerpt");
  expect(searchAction.mock.calls).toHaveLength(1);
  await submitSearch("new query");
  await waitFor(() => expect(searchAction.mock.calls).toHaveLength(2));
  options.baseSearchStatus = 500;
  options.legacySearchStatus = 500;
  searchAction.mockClear();
  await submitSearch("failed query");
  await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("HTTP 500"));
  expect(searchAction).not.toHaveBeenCalled();
  expect(failure).toHaveBeenCalledExactlyOnceWith("short-lived", "search", expect.any(Error));
  // A refresh that fails again reports nothing.
  const calls = harness.legacyCalls();
  await fireEvent.click(screen.getByRole("button", { name: "Refresh current view" }));
  await waitFor(() => expect(harness.legacyCalls()).toBe(calls + 1));
  expect(failure).toHaveBeenCalledOnce();
});

it("keeps the current selection when reranking replaces the rows", async () => {
  const baseFile = node(2, "base.txt", baseVersion);
  const rerankedFile = node(3, "reranked.txt", rerankedVersion);
  const harness = installHarness({
    profiles: [
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: true,
      },
    ],
    files: [baseFile, rerankedFile],
    baseReport: report("hybrid", [
      result(baseFile, 1, "base excerpt"),
      result(rerankedFile, 2, "second excerpt"),
    ]),
    rerankReport: report(
      "hybrid",
      [result(rerankedFile, 1, "reranked excerpt"), result(baseFile, 2, "base excerpt")],
      { outcome: "applied", candidate_count: 2 },
    ),
    rerankPending: true,
  });
  render(App);
  await screen.findAllByText("base.txt");
  await screen.findByRole("button", { name: "Move to trash" });
  await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
  await submitSearch("annual report");
  await screen.findByText("second excerpt");
  await fireEvent.keyDown(window, { key: "j" });
  await fireEvent.keyDown(window, { key: "/" });
  expect(document.activeElement).toBe(screen.getByRole("searchbox", { name: "Search documents" }));
  expect(document.querySelector('tr[data-node-id="3"]')?.getAttribute("aria-selected")).toBe(
    "true",
  );
  harness.resolveRerank(
    json({
      requested_mode: "hybrid",
      actual_mode: "hybrid",
      coverage: {
        binding_required: true,
        scoped_documents: 2,
        complete_documents: 2,
        state: "complete",
      },
      degradations: [],
      results: [result(rerankedFile, 1, "reranked excerpt"), result(baseFile, 2, "base excerpt")],
      truncated: false,
      trace: [],
      reranking: { outcome: "applied", candidate_count: 2 },
    }),
  );
  await screen.findByText("reranked excerpt");
  expect(
    harness.fetchMock.mock.calls.filter(
      ([input, init]) => init?.signal && /^\/api\/v1\/nodes\/\d+$/.test(String(input)),
    ),
  ).toHaveLength(4);
  expect(document.querySelector('tr[data-node-id="3"]')?.getAttribute("aria-selected")).toBe(
    "true",
  );
});

it("keeps base rows with a named degradation note", async () => {
  const file = node(2, "timeout.txt", baseVersion);
  const harness = installHarness({
    profiles: [
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: true,
      },
    ],
    files: [file],
    baseReport: report("hybrid", [result(file, 1, "kept excerpt")]),
    rerankReport: report(
      "hybrid",
      [result(file, 1, "kept excerpt")],
      { outcome: "degraded", cause: "timed_out", candidate_count: 1 },
      ["reranking_degraded"],
    ),
  });
  render(App);
  await screen.findAllByText("timeout.txt");
  await screen.findByRole("button", { name: "Move to trash" });
  await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
  await submitSearch("slow provider");
  await screen.findByText("kept excerpt");
  await screen.findByText(/Reranking unavailable \(timed_out\)/);
  expect(harness.postBodies).toHaveLength(2);
});

it("reruns the accepted query when profiles arrive after legacy results", async () => {
  const file = node(2, "legacy.txt", baseVersion);
  const harness = installHarness({
    profiles: [],
    profilesPending: true,
    files: [file],
    baseReport: report("hybrid", [result(file, 1, "natural excerpt")]),
    tagSearch: {
      hits: [{ node: file, path: file.path, match: "name" }],
      limit: 1000,
      truncated: false,
    },
  });
  render(App);
  await submitSearch("annual report");
  await screen.findAllByText("/legacy.txt");

  harness.resolveProfiles(
    json([
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: false,
      },
    ]),
  );
  await screen.findByText("natural excerpt");
  expect(screen.getByRole("combobox", { name: "Search syntax: Plain words" })).toBeTruthy();
  expect(harness.postBodies[0]?.mode).toBe("hybrid");
});

it("restarts search settings with the accepted query instead of the draft", async () => {
  const file = node(2, "accepted.txt", baseVersion);
  const harness = installHarness({
    profiles: [
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: true,
      },
    ],
    files: [file],
    baseReport: report("hybrid", [result(file, 1, "accepted excerpt")]),
  });
  render(App);
  await screen.findByRole("combobox", { name: "Search syntax: Plain words" });
  await submitSearch("accepted query");
  await screen.findByText("accepted excerpt");

  await fireEvent.input(screen.getByRole("searchbox", { name: "Search documents" }), {
    target: { value: "draft query" },
  });
  await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
  await waitFor(() => expect(harness.postBodies.length).toBeGreaterThanOrEqual(2));
  expect(harness.postBodies[1]?.query).toBe("accepted query");
});

it("defers the profile default until a pending legacy search is accepted", async () => {
  const file = node(2, "pending.txt", baseVersion);
  const harness = installHarness({
    profiles: [],
    profilesPending: true,
    files: [file],
    baseReport: report("hybrid", [result(file, 1, "natural excerpt")]),
    legacyPending: true,
    tagSearch: {
      hits: [{ node: file, path: file.path, match: "name" }],
      limit: 1000,
      truncated: false,
    },
  });
  render(App);
  await submitSearch("annual report");
  await waitFor(() =>
    expect(
      harness.fetchMock.mock.calls.some(([input]) => String(input).startsWith("/api/v1/search?")),
    ).toBe(true),
  );
  harness.resolveProfiles(
    json([
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: false,
      },
    ]),
  );
  harness.resolveLegacy(
    json({ hits: [{ node: file, path: file.path, match: "name" }], limit: 1000, truncated: false }),
  );
  await screen.findByText("natural excerpt");
  expect(screen.getByRole("combobox", { name: "Search syntax: Plain words" })).toBeTruthy();
  expect(harness.postBodies[0]?.mode).toBe("hybrid");
});

it("does not rerun the accepted query after a pending legacy search fails", async () => {
  const file = node(2, "first.txt", baseVersion);
  const harness = installHarness({
    profiles: [],
    profilesPending: true,
    files: [file],
    baseReport: report("hybrid", []),
    legacyFailureAfter: 1,
    legacyFailurePending: true,
    tagSearch: {
      hits: [{ node: file, path: file.path, match: "name" }],
      limit: 1000,
      truncated: false,
    },
  });
  render(App);
  await submitSearch("first query");
  await screen.findAllByText("/first.txt");
  await submitSearch("second query");
  await waitFor(() => expect(harness.legacyCalls()).toBe(2));

  harness.resolveProfiles(
    json([
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: false,
      },
    ]),
  );
  harness.resolveLegacyFailure(new Response("legacy search failure", { status: 500 }));
  await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("HTTP 500"));
  await new Promise((resolve) => setTimeout(resolve, 20));
  expect(harness.legacyCalls()).toBe(2);
  expect(screen.getByRole("combobox", { name: "Search syntax: Plain words" })).toBeTruthy();
});

it("does not rerun an old query while folder navigation is pending", async () => {
  const folder = { ...node(3, "folder", baseVersion), kind: "dir", mime_type: "", path: "/folder" };
  const file = node(2, "old-result.txt", baseVersion);
  const harness = installHarness({
    profiles: [],
    profilesPending: true,
    files: [folder, file],
    childrenPending: true,
    baseReport: report("hybrid", [result(file, 1, "old natural result")]),
    tagSearch: {
      hits: [{ node: folder, path: folder.path, match: "name" }],
      limit: 1000,
      truncated: false,
    },
  });
  render(App);
  await screen.findAllByText("folder");
  await submitSearch("old query");
  await screen.findAllByText("/folder");
  await waitFor(() => expect(document.querySelector('tr[data-node-id="3"]')).not.toBeNull());
  await fireEvent.dblClick(document.querySelector('tr[data-node-id="3"]')!);
  harness.resolveProfiles(
    json([
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: false,
      },
    ]),
  );
  await new Promise((resolve) => setTimeout(resolve, 20));
  expect(harness.postBodies).toHaveLength(0);
  harness.resolveChildren(json({ directory: folder, items: [], total: 0, limit: 1000, offset: 0 }));
  await screen.findByText("/folder");
  expect(screen.getByRole("combobox", { name: "Search syntax: Plain words" })).toBeTruthy();
});

it("reports a Names and text GET failure without retrying it as a fallback", async () => {
  const { fetchMock } = installHarness({
    profiles: [],
    files: [node(2, "legacy.txt", baseVersion)],
    baseReport: report("lexical", []),
    legacySearchStatus: 500,
  });
  render(App);
  await screen.findAllByText("legacy.txt");
  await submitSearch("legacy");
  await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("HTTP 500"));
  expect(
    fetchMock.mock.calls.filter(([input]) => String(input).startsWith("/api/v1/search?")).length,
  ).toBe(1);
});

it("sends the selected tag filter in the resolver body", async () => {
  const file = node(2, "tagged.txt", baseVersion);
  const harness = installHarness({
    profiles: [
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: false,
      },
    ],
    files: [file],
    baseReport: report("hybrid", [result(file, 1, "tagged excerpt")]),
  });
  render(App);
  await screen.findAllByText("tagged.txt");
  await fireEvent.click(
    await screen.findByRole("combobox", { name: "Browse or filter by tag: All tags" }),
  );
  await fireEvent.click(screen.getByRole("option", { name: "reviewed (1)" }));
  await submitSearch("tagged");
  await screen.findByText("tagged excerpt");
  expect(harness.fenceBodies[0]?.filters).toEqual({ tag_id: tagID });
});

it("shows the empty-fence note without posting a search and reports it once, not for reruns", async () => {
  const searchRun = vi.spyOn(actions, "reportSearch").mockImplementation(() => {});
  const action = vi.spyOn(actions, "reportAction").mockImplementation(() => {});
  const failure = vi.spyOn(actions, "reportFailure").mockImplementation(() => {});
  const file = node(2, "empty.txt", baseVersion);
  const harness = installHarness({
    profiles: [
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: false,
      },
    ],
    files: [file],
    baseReport: report("hybrid", []),
    emptyFence: true,
    tagSearch: { hits: [], limit: 1000, truncated: false, tag_id: tagID },
  });
  render(App);
  await screen.findAllByText("empty.txt");
  await fireEvent.click(
    await screen.findByRole("combobox", { name: "Browse or filter by tag: All tags" }),
  );
  await fireEvent.click(screen.getByRole("option", { name: "reviewed (1)" }));
  await submitSearch("nothing");
  await screen.findByText("No documents match the current filter.");
  expect(harness.postBodies).toHaveLength(0);
  expect(action).toHaveBeenCalledExactlyOnceWith("short-lived", "search_empty", "hybrid");
  expect(harness.misses).toEqual([
    expect.objectContaining({
      query: "nothing",
      filters: { tag_id: tagID },
      document_count: 0,
      fence_fingerprint: expect.stringMatching(/^sha256:/),
    }),
  ]);
  expect(searchRun).not.toHaveBeenCalled();
  // A refresh reruns the same search without a new submission.
  const fences = harness.fenceBodies.length;
  await fireEvent.click(screen.getByRole("button", { name: "Refresh current view" }));
  await waitFor(() => expect(harness.fenceBodies).toHaveLength(fences + 1));
  expect(action).toHaveBeenCalledOnce();
  action.mockClear();
  await submitSearch("q".repeat(8193));
  await waitFor(() => expect(harness.fenceBodies).toHaveLength(fences + 2));
  await screen.findByText("No documents match the current filter.");
  expect(action).not.toHaveBeenCalled();
  expect(searchRun).not.toHaveBeenCalled();
  expect(failure).not.toHaveBeenCalled();
  expect(harness.postBodies).toHaveLength(0);
});

it("keeps a non-401 processing failure on the legacy GET path", async () => {
  const failure = vi.spyOn(actions, "reportFailure").mockImplementation(() => {});
  const file = node(2, "fallback.txt", baseVersion);
  const harness = installHarness({
    profiles: [
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: true,
      },
    ],
    files: [file],
    baseReport: report("hybrid", []),
    baseSearchStatus: 500,
    legacySearchStatus: undefined,
    tagSearch: {
      hits: [{ node: file, path: file.path, match: "name" }],
      limit: 1000,
      truncated: false,
    },
  });
  render(App);
  await screen.findAllByText("fallback.txt");
  await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
  await submitSearch("fallback");
  await screen.findByText("/fallback.txt");
  await screen.findByText(/Natural-language search unavailable/);
  expect(screen.getByRole("combobox", { name: "Search syntax: Plain words" })).toBeTruthy();
  expect(screen.queryByRole("checkbox", { name: "Rerank results" })).toBeNull();
  expect(harness.postBodies).toHaveLength(1);
  expect(
    harness.fetchMock.mock.calls.filter(([input]) => String(input).startsWith("/api/v1/search?"))
      .length,
  ).toBe(1);
  expect(failure).not.toHaveBeenCalled();
});

it("reports a failed legacy GET fallback after a processing failure", async () => {
  const failure = vi.spyOn(actions, "reportFailure").mockImplementation(() => {});
  const file = node(2, "fallback.txt", baseVersion);
  const harness = installHarness({
    profiles: [
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: true,
      },
    ],
    files: [file],
    baseReport: report("hybrid", []),
    baseSearchStatus: 500,
    legacySearchStatus: 500,
    tagSearch: {
      hits: [{ node: file, path: file.path, match: "name" }],
      limit: 1000,
      truncated: false,
    },
  });
  render(App);
  await screen.findAllByText("fallback.txt");
  await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
  await submitSearch("fallback");
  await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("HTTP 500"));
  expect(harness.postBodies).toHaveLength(1);
  expect(
    harness.fetchMock.mock.calls.filter(([input]) => String(input).startsWith("/api/v1/search?"))
      .length,
  ).toBe(1);
  expect(failure).toHaveBeenCalledExactlyOnceWith("short-lived", "search", expect.any(Error));
});

it("does not fall back to GET after a processing 401", async () => {
  const file = node(2, "unauthorized.txt", baseVersion);
  const harness = installHarness({
    profiles: [
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: false,
      },
    ],
    files: [file],
    baseReport: report("hybrid", []),
    baseSearchStatus: 401,
  });
  render(App);
  await screen.findAllByText("unauthorized.txt");
  await submitSearch("unauthorized");
  await screen.findByText("Open your Docbank");
  expect(
    harness.fetchMock.mock.calls.filter(([input]) => String(input).startsWith("/api/v1/search?"))
      .length,
  ).toBe(0);
});

it("ignores an aborted natural search completion", async () => {
  const file = node(2, "stale.txt", baseVersion);
  const harness = installHarness({
    profiles: [
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: false,
      },
    ],
    files: [file],
    baseReport: report("hybrid", [result(file, 1, "current excerpt")]),
    basePending: true,
  });
  render(App);
  await screen.findAllByText("stale.txt");
  await submitSearch("old query");
  await waitFor(() => expect(harness.postBodies).toHaveLength(1));
  await submitSearch("new query");
  await screen.findByText("current excerpt");
  harness.resolveBase(json(report("hybrid", [result(file, 1, "stale excerpt")])));
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(screen.queryByText("stale excerpt")).toBeNull();
  const nameRequests = harness.fetchMock.mock.calls.filter(([input]) =>
    String(input).startsWith("/api/v1/search?"),
  );
  expect(nameRequests).toHaveLength(1);
  expect(new URL(String(nameRequests[0][0]), "https://example.com").searchParams.get("q")).toBe(
    "new query",
  );
});

it("shows why natural search controls could not load", async () => {
  installHarness({
    profiles: [],
    profilesStatus: 503,
    files: [],
    baseReport: report("lexical", []),
  });
  render(App);
  expect(await screen.findByText(/Natural-language search unavailable.*503/)).toBeTruthy();
});

it.each([500, 401])(
  "handles node lookup failures without a Names and text fallback (%i)",
  async (status) => {
    const good = node(2, "available.txt", baseVersion);
    const bad = node(3, "unavailable.txt", rerankedVersion);
    const harness = installHarness({
      profiles: [
        {
          name: "private",
          fingerprint: "a".repeat(64),
          rendition: true,
          embedding_bindings: ["embed"],
          reranking_available: true,
        },
      ],
      files: [good, bad],
      nodeFailure: { id: bad.id, status },
      baseReport: report("hybrid", [
        result(good, 1, "available excerpt"),
        result(bad, 2, "unavailable excerpt"),
      ]),
    });
    render(App);
    await screen.findByRole("checkbox", { name: "Rerank results" });
    await submitSearch("annual report");
    if (status === 401) {
      await screen.findByText(/browser session expired or was rejected/);
    } else {
      await screen.findByText("available excerpt");
      expect(screen.queryByText("unavailable excerpt")).toBeNull();
      expect(screen.getByRole("combobox", { name: "Search syntax: Plain words" })).toBeTruthy();
    }
    expect(harness.legacyCalls()).toBe(status === 401 ? 0 : 1);
    expect(screen.queryByText(/Natural-language search unavailable/)).toBeNull();
  },
);

it("reruns automatic search when reranking changes", async () => {
  const file = node(2, "report.txt", baseVersion);
  const options: HarnessOptions = {
    profiles: [
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: true,
      },
    ],
    files: [file],
    baseReport: report("hybrid", [result(file, 1, "auto excerpt")]),
  };
  const harness = installHarness(options);
  render(App);
  await screen.findByRole("combobox", { name: "Search syntax: Plain words" });
  await submitSearch("annual report");
  await screen.findByText("auto excerpt");
  expect(harness.postBodies.at(-1)?.mode).toBe("hybrid");
  options.rerankReport = report("hybrid", [result(file, 1, "reranked excerpt")], {
    outcome: "applied",
    candidate_count: 1,
  });
  await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
  await screen.findByText("reranked excerpt");
  expect(harness.postBodies.at(-1)?.rerank).toBe(true);
  await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
  await screen.findByText("auto excerpt");
  expect(harness.postBodies.at(-1)?.rerank).toBeUndefined();
});

it("preserves the chosen sort on a natural search refresh", async () => {
  const first = node(2, "zebra.txt", baseVersion);
  const second = node(3, "alpha.txt", rerankedVersion);
  const harness = installHarness({
    profiles: [
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: false,
      },
    ],
    files: [first, second],
    baseReport: report("hybrid", [
      result(first, 1, "first excerpt"),
      result(second, 2, "second excerpt"),
    ]),
  });
  render(App);
  await screen.findByRole("combobox", { name: "Search syntax: Plain words" });
  await submitSearch("annual report");
  await screen.findByText("first excerpt");
  await fireEvent.click(screen.getByRole("button", { name: "Document" }));
  expect(screen.getByRole("columnheader", { name: "Document" }).getAttribute("aria-sort")).toBe(
    "ascending",
  );
  await fireEvent.click(screen.getByRole("button", { name: "Refresh current view" }));
  await waitFor(() => expect(harness.postBodies).toHaveLength(2));
  await waitFor(() => expect(screen.getByRole("button", { name: "Move to trash" })).toBeTruthy());
  expect(
    (await screen.findByRole("columnheader", { name: "Document" })).getAttribute("aria-sort"),
  ).toBe("ascending");
});

it("reports explicit file opens and leaves navigation silent", async () => {
  const action = vi.spyOn(actions, "reportAction").mockImplementation(() => {});
  const file = node(2, "report.txt", baseVersion);
  const folder = { ...node(3, "folder", rerankedVersion), kind: "dir" };
  installHarness({
    profiles: [],
    files: [file, node(4, "second.txt", rerankedVersion), folder],
    baseReport: report("lexical", []),
  });
  render(App);
  await screen.findAllByText("report.txt");
  expect(action).not.toHaveBeenCalled();
  await fireEvent.click(document.querySelector('tr[data-node-id="4"]')!);
  expect(action).toHaveBeenCalledOnce();
  expect(action).toHaveBeenCalledWith("short-lived", "document_opened");
  action.mockClear();
  await fireEvent.keyDown(window, { key: "k" });
  await waitFor(() =>
    expect(document.querySelector('tr[data-node-id="2"]')?.getAttribute("aria-selected")).toBe(
      "true",
    ),
  );
  expect(action).not.toHaveBeenCalled();
  await fireEvent.keyDown(document.querySelector('tr[data-node-id="4"]')!, { key: "Enter" });
  expect(action).toHaveBeenCalledOnce();
  expect(action).toHaveBeenCalledWith("short-lived", "document_opened");
  action.mockClear();
  const row = document.querySelector('tr[data-node-id="4"]')!;
  await fireEvent.click(row, { detail: 1 });
  await fireEvent.click(row, { detail: 2 });
  await fireEvent.dblClick(row, { detail: 2 });
  expect(action).toHaveBeenCalledOnce();
  expect(action).toHaveBeenCalledWith("short-lived", "document_opened");
  action.mockClear();
  await fireEvent.dblClick(document.querySelector('tr[data-node-id="3"]')!);
  await waitFor(() => expect(document.querySelector('tr[data-node-id="2"]')).toBeNull());
  expect(action).not.toHaveBeenCalled();
});

it("returns to automatic meaning search with the typed draft when plain words is selected", async () => {
  const file = node(2, "report.txt", baseVersion);
  const harness = installHarness({
    profiles: [
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: true,
      },
    ],
    files: [file],
    baseReport: report("hybrid", [result(file, 1, "report excerpt")]),
  });
  render(App);
  await screen.findByRole("checkbox", { name: "Rerank results" });
  await submitSearch("annual report");
  await screen.findByText("report excerpt");
  await fireEvent.click(screen.getByRole("combobox", { name: "Search syntax: Plain words" }));
  await fireEvent.click(screen.getByRole("option", { name: "Query syntax" }));
  await screen.findByRole("region", { name: "Frozen query results" });
  await fireEvent.input(screen.getByRole("searchbox", { name: "Search documents" }), {
    target: { value: "revised report" },
  });
  await fireEvent.click(screen.getByRole("combobox", { name: "Search syntax: Query syntax" }));
  await fireEvent.click(screen.getByRole("option", { name: "Plain words" }));
  await waitFor(() =>
    expect(harness.postBodies.at(-1)).toMatchObject({ mode: "hybrid", query: "revised report" }),
  );
});

it.each(["previously displayed", "same"] as const)(
  "sorts the %s query from Advanced and supersedes any pending search",
  async (which) => {
    const small = { ...node(2, "small.txt", baseVersion), size: 10 };
    const large = { ...node(3, "large.txt", rerankedVersion), size: 500 };
    const harness = installHarness({
      profiles: [
        {
          name: "private",
          fingerprint: "a".repeat(64),
          rendition: true,
          embedding_bindings: ["embed"],
          reranking_available: false,
        },
      ],
      files: [small, large],
      baseReport: report("hybrid", [
        result(small, 1, "small excerpt"),
        result(large, 2, "large excerpt"),
      ]),
    });
    let resolvePending!: (response: Response) => void;
    const pending = new Promise<Response>((resolve) => {
      resolvePending = resolve;
    });
    const fetch = harness.fetchMock.getMockImplementation()!;
    render(App);
    if (which === "previously displayed") {
      await submitSearch("query A");
      await screen.findByText("small excerpt");
    }
    const pendingSearch = vi.fn(() => {
      harness.fetchMock.mockImplementation(fetch);
      return pending;
    });
    harness.fetchMock.mockImplementation((input, init) => {
      if (String(input) === "/api/v1/search") return pendingSearch();
      return fetch(input, init);
    });
    await submitSearch("query B");
    await waitFor(() => expect(pendingSearch).toHaveBeenCalledOnce());
    const query = which === "previously displayed" ? "query A" : "query B";
    await fireEvent.input(screen.getByRole("searchbox", { name: "Search documents" }), {
      target: { value: query },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
    await fireEvent.click(screen.getByRole("combobox", { name: /^Sort by:/ }));
    await fireEvent.click(screen.getByRole("option", { name: "Size" }));
    await fireEvent.click(screen.getByRole("combobox", { name: "Direction: Ascending" }));
    await fireEvent.click(screen.getByRole("option", { name: "Descending" }));
    await fireEvent.click(screen.getByRole("button", { name: "Search" }));
    await act(async () => {
      resolvePending(json(report("hybrid", [result(small, 1, "stale excerpt")])));
      await pending;
    });
    await waitFor(() => expect(harness.postBodies.at(-1)).toMatchObject({ mode: "hybrid", query }));
    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: "Filters and sort" })).toBeNull(),
    );
    expect(screen.queryByRole("region", { name: "Frozen query results" })).toBeNull();
    expect(
      harness.fetchMock.mock.calls.some(([url]) => String(url) === "/api/v1/workspace/queries"),
    ).toBe(false);
    await waitFor(() => {
      expect(screen.getByText("small excerpt")).toBeTruthy();
      expect(screen.queryByText("stale excerpt")).toBeNull();
      expect(
        (screen.getByRole("searchbox", { name: "Search documents" }) as HTMLInputElement).value,
      ).toBe(query);
      expect(screen.getByRole("columnheader", { name: "Size" }).getAttribute("aria-sort")).toBe(
        "descending",
      );
      const rows = screen.getAllByRole("row").filter((row) => row.textContent?.includes("txt"));
      expect(rows[0].textContent).toContain("large.txt");
    });
  },
);

it.each(["unrun edit", "failed search"])(
  "Cancel restores the applied meaning search after an Advanced %s",
  async (state) => {
    const file = node(2, "report.txt", baseVersion);
    const harness = installHarness({
      profiles: [
        {
          name: "private",
          fingerprint: "a".repeat(64),
          rendition: true,
          embedding_bindings: ["embed"],
          reranking_available: false,
        },
      ],
      files: [file],
      baseReport: report("hybrid", [result(file, 1, "applied meaning result")]),
    });
    const fetch = harness.fetchMock.getMockImplementation()!;
    harness.fetchMock.mockImplementation((input, init) => {
      if (String(input) === "/api/v1/workspace/queries")
        return Promise.resolve(
          new Response(
            JSON.stringify({
              code: "snapshot_unavailable",
              detail: "query snapshot did not finish within its resource budget",
            }),
            { status: 503, headers: { "Content-Type": "application/json" } },
          ),
        );
      return fetch(input, init);
    });
    render(App);
    await submitSearch("annual report");
    await screen.findByText("applied meaning result");
    await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
    await fireEvent.input(screen.getByLabelText("Filters JSON"), {
      target: { value: '{"size_min":10}' },
    });
    await fireEvent.input(screen.getByRole("searchbox", { name: "Search documents" }), {
      target: { value: "unfinished draft" },
    });
    if (state === "failed search") {
      await fireEvent.click(screen.getByRole("button", { name: "Search" }));
      await screen.findAllByText("query snapshot did not finish within its resource budget");
    }
    await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await screen.findByText("applied meaning result");
    expect(
      (screen.getByRole("searchbox", { name: "Search documents" }) as HTMLInputElement).value,
    ).toBe("annual report");
    expect(harness.postBodies).toHaveLength(1);
    expect(location.hash).toBe("");
    await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
    expect((screen.getByLabelText("Filters JSON") as HTMLTextAreaElement).value).toBe("{}");
    expect(
      screen.queryByText("query snapshot did not finish within its resource budget"),
    ).toBeNull();
  },
);

it("filtered search walkthrough reaches accepted results", async () => {
  const file = node(2, "report.txt", baseVersion);
  const options: HarnessOptions = {
    profiles: [
      {
        name: "private",
        fingerprint: "a".repeat(64),
        rendition: true,
        embedding_bindings: ["embed"],
        reranking_available: false,
      },
    ],
    files: [file],
    snapshotRows: [
      {
        node_id: file.id,
        content_version_id: baseVersion,
        blob_hash: file.blob_hash,
        size: 32,
        revision: 1,
        name: "report.txt",
        path: "/report.txt",
        mime_type: "text/plain",
        media_family: "text",
        modified_at: "2026-09-21T00:00:00Z",
        sort_key: "/report.txt",
        tags: [],
        collection_ids: [],
      },
    ],
    baseReport: report("hybrid", [result(file, 1, "report excerpt")]),
  };
  const harness = installHarness(options);
  render(App);
  await screen.findAllByText("report.txt");
  await submitSearch("annual report");
  await screen.findByText("report excerpt");
  expect(harness.misses).toHaveLength(0);
  expect(screen.queryByText("Filters search names and text only.")).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  await fireEvent.input(screen.getByLabelText("Filters JSON"), {
    target: { value: '{"size_min":10}' },
  });
  await screen.findByRole("status", { name: "Query valid" });
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await waitFor(() =>
    expect(screen.queryByRole("dialog", { name: "Filters and sort" })).toBeNull(),
  );
  const results = await screen.findByRole("region", { name: "Frozen query results" });
  expect(within(results).getByText("report.txt")).toBeTruthy();
  expect(await screen.findByText("Filters search names and text only.")).toBeTruthy();
  expect(
    harness.fetchMock.mock.calls.some(([url]) => String(url) === "/api/v1/workspace/queries"),
  ).toBe(true);
  await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
  await fireEvent.input(screen.getByLabelText("Filters JSON"), {
    target: { value: "{}" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await waitFor(() => expect(harness.postBodies).toHaveLength(2));
  expect(harness.postBodies[1]).toMatchObject({ mode: "hybrid", query: "annual report" });
  await screen.findByText("report excerpt");
  expect(screen.queryByRole("region", { name: "Frozen query results" })).toBeNull();
  expect(screen.queryByRole("dialog", { name: "Filters and sort" })).toBeNull();
  expect(screen.queryByText("Filters search names and text only.")).toBeNull();
});

it.each([
  ["Advanced", false],
  ["top bar", false],
  ["Advanced", true],
  ["top bar", true],
])(
  "keeps table sorting after %s submits a sorted search, superseded=%s",
  async (submission, superseded) => {
    const small = { ...node(2, "small.txt", baseVersion), size: 10 };
    const large = { ...node(3, "large.txt", rerankedVersion), size: 500 };
    const harness = installHarness({
      profiles: [
        {
          name: "private",
          fingerprint: "a".repeat(64),
          rendition: true,
          embedding_bindings: ["embed"],
          reranking_available: true,
        },
      ],
      files: [small, large],
      baseReport: report("hybrid", [
        result(small, 1, "small excerpt"),
        result(large, 2, "large excerpt"),
      ]),
      rerankPending: true,
    });
    render(App);
    await submitSearch("first search");
    await screen.findByText("small excerpt");
    await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
    await fireEvent.input(screen.getByRole("searchbox", { name: "Search documents" }), {
      target: { value: "second search" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
    await fireEvent.click(screen.getByRole("combobox", { name: /^Sort by:/ }));
    await fireEvent.click(screen.getByRole("option", { name: "Size" }));
    await fireEvent.click(screen.getByRole("combobox", { name: "Direction: Ascending" }));
    await fireEvent.click(screen.getByRole("option", { name: "Descending" }));
    if (submission === "Advanced")
      await fireEvent.click(screen.getByRole("button", { name: "Search" }));
    else {
      await fireEvent.keyDown(window, { key: "Escape" });
      await fireEvent.submit(
        screen.getByRole("searchbox", { name: "Search documents" }).closest("form")!,
      );
    }
    await waitFor(() =>
      expect(harness.postBodies.at(-1)).toMatchObject({ query: "second search", rerank: true }),
    );
    expect(screen.getByRole("columnheader", { name: "Size" }).getAttribute("aria-sort")).toBe(
      "descending",
    );
    if (submission === "Advanced" && !superseded) {
      const rows = screen.getAllByRole("row").filter((row) => row.textContent?.includes("txt"));
      expect(rows[0].textContent).toContain("large.txt");
      expect(
        harness.fetchMock.mock.calls.some(([url]) => String(url) === "/api/v1/workspace/queries"),
      ).toBe(false);
    }
    if (superseded) {
      await submitSearch("newer search");
      await waitFor(() =>
        expect(harness.postBodies.at(-1)).toMatchObject({ query: "newer search", rerank: true }),
      );
    }
    await fireEvent.click(screen.getByRole("button", { name: "Document" }));
    harness.resolveRerank(
      json(
        report(
          "hybrid",
          [result(small, 1, "reranked excerpt"), result(large, 2, "large excerpt")],
          { outcome: "applied", candidate_count: 2 },
        ),
      ),
    );
    await screen.findByText("reranked excerpt");
    expect(screen.getByRole("columnheader", { name: "Document" }).getAttribute("aria-sort")).toBe(
      "ascending",
    );
  },
);

it.each(["top bar", "Advanced"])(
  "retains the sort draft after a failed search from %s",
  async (entry) => {
    const options = {
      profiles: [
        {
          name: "private",
          fingerprint: "a".repeat(64),
          rendition: true,
          embedding_bindings: ["embed"],
          reranking_available: false,
        },
      ],
      files: [node(2, "report.txt", baseVersion)],
      baseReport: report("hybrid", [
        result(node(2, "report.txt", baseVersion), 1, "report excerpt"),
      ]),
      baseSearchStatus: 0,
      legacySearchStatus: 0,
    };
    installHarness(options);
    render(App);
    await submitSearch("first search");
    await screen.findByText("report excerpt");
    options.baseSearchStatus = 500;
    options.legacySearchStatus = 500;
    await fireEvent.input(screen.getByRole("searchbox", { name: "Search documents" }), {
      target: { value: "annual report" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
    await fireEvent.click(screen.getByRole("combobox", { name: "Sort by: Path" }));
    await fireEvent.click(screen.getByRole("option", { name: "Size" }));
    if (entry === "top bar") {
      await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
      await submitSearch("annual report");
    } else {
      await fireEvent.click(screen.getByRole("button", { name: "Search" }));
    }
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("HTTP 500"));
    if (!screen.queryByRole("dialog", { name: "Filters and sort" }))
      await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
    expect(screen.getByRole("combobox", { name: "Sort by: Size" })).toBeTruthy();
  },
);

it("keeps the applied query and unapplied draft when the inspector removes the active tag", async () => {
  const file = node(2, "report.txt", baseVersion);
  const tag = { id: tagID, name: "reviewed", revision: 1, assignment_count: 1 };
  const tagSearch = {
    hits: [{ node: file, path: file.path }],
    limit: 1000,
    truncated: false,
    tag_id: tagID,
  };
  const harness = installHarness({
    profiles: [],
    files: [file],
    baseReport: report("lexical", []),
    tagSearch,
  });
  const fetch = harness.fetchMock.getMockImplementation()!;
  let removed = false;
  harness.fetchMock.mockImplementation((input, init) => {
    const url = String(input);
    if (url === `/api/v1/nodes/2/tags/${tagID}` && init?.method === "DELETE") {
      removed = true;
      tagSearch.hits = [];
      return Promise.resolve(
        json({
          node: { ...file, revision: 2 },
          tag: { ...tag, revision: 2, assignment_count: 0 },
          changed: true,
        }),
      );
    }
    if (url.startsWith("/api/v1/nodes/2/tags?"))
      return Promise.resolve(
        json({ items: removed ? [] : [tag], total: removed ? 0 : 1, limit: 1000, offset: 0 }),
      );
    return fetch(input, init);
  });
  render(App);
  await screen.findAllByText("report.txt");
  await fireEvent.click(
    screen.getByRole("combobox", { name: "Browse or filter by tag: All tags" }),
  );
  await fireEvent.click(await screen.findByRole("option", { name: "reviewed (1)" }));
  await submitSearch("annual report");
  await screen.findByText("Search results");
  await screen.findByRole("button", { name: "Manage" });
  const search = screen.getByRole("searchbox", { name: "Search documents" });
  await fireEvent.input(search, { target: { value: "unfinished draft" } });
  await fireEvent.click(screen.getByRole("button", { name: "Manage" }));
  const dialog = await screen.findByRole("dialog", { name: "Manage tags for report.txt" });
  await fireEvent.click(await within(dialog).findByRole("button", { name: "Remove tag reviewed" }));
  await waitFor(() => expect(harness.legacyCalls()).toBe(2));
  const requests = harness.fetchMock.mock.calls.filter(([url]) =>
    String(url).startsWith("/api/v1/search?"),
  );
  const request = new URL(String(requests.at(-1)![0]), "https://example.com");
  expect(request.searchParams.get("q")).toBe("annual report");
  expect(request.searchParams.get("tag_id")).toBe(tagID);
  expect((search as HTMLInputElement).value).toBe("unfinished draft");
});

it.each(["names", "meaning", "frozen"])(
  "keeps unapplied drafts while trash and Undo refresh %s results",
  async (mode) => {
    const file = node(2, "report.txt", baseVersion);
    const harness = installHarness({
      profiles:
        mode === "meaning"
          ? [
              {
                name: "private",
                fingerprint: "a".repeat(64),
                rendition: true,
                embedding_bindings: ["embed"],
                reranking_available: false,
              },
            ]
          : [],
      files: [file],
      baseReport: report("hybrid", [result(file, 1, "report excerpt")]),
      tagSearch: { hits: [{ node: file, path: file.path }], limit: 1000, truncated: false },
      snapshotRows: [
        {
          node_id: file.id,
          content_version_id: baseVersion,
          blob_hash: file.blob_hash,
          size: file.size,
          revision: file.revision,
          name: file.name,
          path: file.path,
          mime_type: file.mime_type,
          media_family: "text",
          modified_at: file.modified_at,
          sort_key: file.path,
          tags: [],
          collection_ids: [],
        },
      ],
    });
    const fetch = harness.fetchMock.getMockImplementation()!;
    harness.fetchMock.mockImplementation((input, init) => {
      const url = String(input);
      if (url === "/api/v1/nodes/2/trash")
        return Promise.resolve(json({ ...file, revision: 2, trashed_at: "2026-09-21T01:00:00Z" }));
      if (url === "/api/v1/nodes/2/restore") return Promise.resolve(json({ ...file, revision: 3 }));
      return fetch(input, init);
    });
    render(App);
    await screen.findByRole("combobox", { name: "Search syntax: Plain words" });
    await submitSearch("annual report");
    if (mode === "frozen") {
      await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
      await fireEvent.input(screen.getByLabelText("Filters JSON"), {
        target: { value: '{"size_min":10}' },
      });
      await screen.findByRole("status", { name: "Query valid" });
      await fireEvent.click(screen.getByRole("button", { name: "Search" }));
      await screen.findByRole("region", { name: "Frozen query results" });
    }
    await screen.findByRole("button", { name: "Move to trash" });
    const search = screen.getByRole("searchbox", { name: "Search documents" });
    await fireEvent.input(search, { target: { value: "unfinished draft" } });
    await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
    await fireEvent.input(screen.getByLabelText("Filters JSON"), {
      target: { value: '{"size_min":' },
    });
    await fireEvent.keyDown(window, { key: "Escape" });
    for (const [action, count] of [
      ["Move to trash", 2],
      ["Undo", 3],
    ] as const) {
      await fireEvent.click(screen.getByRole("button", { name: action }));
      const frozenRequests = () =>
        harness.fetchMock.mock.calls.filter(([url]) => String(url) === "/api/v1/workspace/queries");
      await waitFor(() =>
        expect(
          mode === "frozen"
            ? frozenRequests().length
            : mode === "meaning"
              ? harness.postBodies.length
              : harness.legacyCalls(),
        ).toBe(count),
      );
      if (mode === "frozen") {
        const queries = frozenRequests().map(([, init]) => JSON.parse(String(init?.body)).query);
        expect(queries.at(-1)).toEqual(queries[0]);
        expect(queries.at(-1)).toMatchObject({ text: "annual report", filters: { size_min: 10 } });
        expect(screen.getByRole("region", { name: "Frozen query results" })).toBeTruthy();
      } else if (mode === "meaning") expect(harness.postBodies.at(-1)?.query).toBe("annual report");
      else {
        const requests = harness.fetchMock.mock.calls.filter(([url]) =>
          String(url).startsWith("/api/v1/search?"),
        );
        expect(
          new URL(String(requests.at(-1)![0]), "https://example.com").searchParams.get("q"),
        ).toBe("annual report");
      }
      expect((search as HTMLInputElement).value).toBe("unfinished draft");
      await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
      expect((screen.getByLabelText("Filters JSON") as HTMLTextAreaElement).value).toBe(
        '{"size_min":',
      );
      await fireEvent.keyDown(window, { key: "Escape" });
    }
  },
);

it.each(["plain", "rerank", "fallback", "filtered"])(
  "records an empty %s submission once",
  async (kind) => {
    const rerank = kind === "rerank";
    const harness = installHarness({
      profiles: [
        {
          name: "private",
          fingerprint: "a".repeat(64),
          rendition: true,
          embedding_bindings: ["embed"],
          reranking_available: rerank,
        },
      ],
      files: [node(2, "existing.txt", baseVersion)],
      baseReport: report("hybrid", []),
      ...(kind === "fallback"
        ? { baseSearchStatus: 500, tagSearch: { hits: [], limit: 1000, truncated: false } }
        : {}),
      rerankReport: report("hybrid", [], { outcome: "skipped", candidate_count: 0 }),
    });
    render(App);
    await screen.findAllByText("existing.txt");
    if (rerank)
      await fireEvent.click(await screen.findByRole("checkbox", { name: "Rerank results" }));
    if (kind === "filtered") {
      await fireEvent.input(screen.getByRole("searchbox", { name: "Search documents" }), {
        target: { value: "missing" },
      });
      await fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
      await fireEvent.input(screen.getByLabelText("Filters JSON"), {
        target: { value: '{"size_min":20}' },
      });
      await screen.findByRole("status", { name: "Query valid" });
      await fireEvent.click(screen.getByRole("button", { name: "Search" }));
    } else {
      await submitSearch("missing");
    }
    await waitFor(() => expect(harness.misses).toHaveLength(1));
    expect(harness.misses[0]).toMatchObject({
      query: "missing",
      ...(kind === "fallback"
        ? {
            requested_mode: "hybrid",
            actual_mode: "lexical",
            profile: "private",
            binding_id: "embed",
            degradations: [screen.getByText(/Natural-language search unavailable/).textContent],
          }
        : kind === "filtered"
          ? {
              filters: { filters: { size_min: 20 } },
              actual_mode: "lexical",
              coverage: { configuration: "unconfigured" },
            }
          : { binding_id: "embed", actual_mode: "hybrid", document_count: 2 }),
    });
    if (kind === "fallback" || kind === "filtered") return;
    await fireEvent.click(screen.getByRole("button", { name: "Refresh current view" }));
    await waitFor(() => expect(harness.postBodies.length).toBeGreaterThan(rerank ? 2 : 1));
    expect(harness.misses).toHaveLength(1);
  },
);
