import { createHash } from "node:crypto";
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import App from "./App.svelte";

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
  const length = (value: number) => append(Uint8Array.from([(value >>> 24) & 0xff, (value >>> 16) & 0xff, (value >>> 8) & 0xff, value & 0xff]));
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
    coverage: { binding_required: mode === "hybrid", scoped_documents: 2, complete_documents: 2, state: "complete" },
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
  files: ReturnType<typeof node>[];
  baseReport: unknown;
  baseSearchStatus?: number;
  legacySearchStatus?: number;
  legacyFailureAfter?: number;
  legacyFailurePending?: boolean;
  basePending?: boolean;
  legacyPending?: boolean;
  rerankReport?: unknown;
  rerankPending?: boolean;
  rerankNode?: ReturnType<typeof node>;
  emptyFence?: boolean;
  nodeFailure?: { id: number; status: number };
  tagSearch?: unknown;
}

function installHarness(options: HarnessOptions) {
  history.replaceState(null, "", "/#web_session=short-lived&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
  const root = { id: 1, name: "", kind: "dir", size: 0, revision: 1, created_at: "2026-09-21T00:00:00Z", modified_at: "2026-09-21T00:00:00Z", path: "/" };
  const postBodies: Record<string, unknown>[] = [];
  const fenceBodies: Record<string, unknown>[] = [];
  let rerankResolve: ((response: Response) => void) | undefined;
  let baseResolve: ((response: Response) => void) | undefined;
  let profilesResolve: ((response: Response) => void) | undefined;
  let legacyResolve: ((response: Response) => void) | undefined;
  let legacyFailureResolve: ((response: Response) => void) | undefined;
  const basePending = new Promise<Response>((resolve) => { baseResolve = resolve; });
  const profilesPending = new Promise<Response>((resolve) => { profilesResolve = resolve; });
  const legacyPending = new Promise<Response>((resolve) => { legacyResolve = resolve; });
  const legacyFailurePending = new Promise<Response>((resolve) => { legacyFailureResolve = resolve; });
  let baseCalls = 0;
  let legacyCalls = 0;
  const nodeCalls = new Map<number, number>();
  const rerankPending = new Promise<Response>((resolve) => { rerankResolve = resolve; });
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    const method = init?.method ?? "GET";
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1/children?limit=1000&offset=0") {
      return json({ directory: root, items: options.files, total: options.files.length, limit: 1000, offset: 0 });
    }
    if (url === "/api/v1/tags?limit=1000&offset=0") {
      return json({ items: [{ id: tagID, name: "reviewed", revision: 1, assignment_count: 1 }], total: 1, limit: 1000, offset: 0 });
    }
    if (url === `/api/v1/tags/${tagID}/nodes?limit=1000&offset=0&live_only=true`) {
      return json({ items: options.files.map((file) => ({ node: file, path: file.path })), total: options.files.length, limit: 1000, offset: 0, omitted_trashed: 0 });
    }
    if (url === "/api/v1/processing/profiles") {
      if (options.profilesPending) return profilesPending;
      return options.profilesStatus ? new Response("profile failure", { status: options.profilesStatus }) : json(options.profiles);
    }
    const nodeMatch = url.match(/^\/api\/v1\/nodes\/(\d+)$/);
    if (nodeMatch) {
      const id = Number(nodeMatch[1]);
      if (id === options.nodeFailure?.id) return new Response("node failure", { status: options.nodeFailure.status });
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
      if (options.baseSearchStatus) return new Response("search failure", { status: options.baseSearchStatus });
      if (body.rerank) {
        if (options.rerankPending) return rerankPending;
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
      if (options.legacyPending) return legacyPending;
      if (options.legacySearchStatus) return new Response("legacy search failure", { status: options.legacySearchStatus });
      return json(options.tagSearch ?? { hits: [], limit: 1000, truncated: false });
    }
    if (url.startsWith("/api/v1/audit/status?")) return json({ enabled: false, scopes: [] });
    if (url.startsWith("/api/v1/nodes/") && url.includes("/tags?")) return json({ items: [], total: 0, limit: 1000, offset: 0 });
    throw new Error(`unexpected request: ${method} ${url}`);
  });
  return {
    fetchMock,
    postBodies,
    fenceBodies,
    resolveRerank: (response: Response) => rerankResolve?.(response),
    resolveBase: (response: Response) => baseResolve?.(response),
    resolveProfiles: (response: Response) => profilesResolve?.(response),
    resolveLegacy: (response: Response) => legacyResolve?.(response),
    resolveLegacyFailure: (response: Response) => legacyFailureResolve?.(response),
    legacyCalls: () => legacyCalls,
  };
}

async function submitSearch(value: string): Promise<void> {
  const input = await screen.findByRole("searchbox", { name: "Search documents" });
  await fireEvent.input(input, { target: { value } });
  await fireEvent.submit(input.closest("form")!);
}

it("keeps Names and text on GET with the tag and 1000 result limit", async () => {
  const file = node(2, "existing.txt", baseVersion);
  const { fetchMock } = installHarness({
    profiles: [{ name: "private", fingerprint: "a".repeat(64), rendition: true, embedding_bindings: [], reranking_available: true }],
    files: [file],
    baseReport: report("lexical", [result(file, 1, "unused")]),
    tagSearch: { hits: [{ node: file, path: file.path, match: "content" }], limit: 1000, truncated: false, tag_id: tagID },
  });
  render(App);
  await screen.findAllByText("existing.txt");
  await screen.findByRole("button", { name: "Process and retrieve" });
  await fireEvent.click(await screen.findByRole("combobox", { name: "Browse or filter by tag: All tags" }));
  await fireEvent.click(screen.getByRole("option", { name: "reviewed (1)" }));
  await submitSearch("existing");
  await screen.findByText("/existing.txt");
  expect(fetchMock.mock.calls.some(([input, init]) => String(input).startsWith("/api/v1/search?") && (init?.method ?? "GET") === "GET")).toBe(true);
  const searchURL = fetchMock.mock.calls.map(([input]) => String(input)).find((input) => input.startsWith("/api/v1/search?"));
  expect(searchURL).toContain("limit=1000");
  expect(searchURL).toContain(`tag_id=${tagID}`);
  expect(fetchMock.mock.calls.some(([input, init]) => String(input) === "/api/v1/search" && init?.method === "POST")).toBe(false);
  expect(screen.queryByText("Reranking results… Base results are shown.")).toBeNull();
});

it("shows base rows while reranking and applies the validated order", async () => {
  const baseFile = node(2, "base.txt", baseVersion);
  const rerankedFile = node(3, "reranked.txt", rerankedVersion);
  const harness = installHarness({
    profiles: [{ name: "private", fingerprint: "a".repeat(64), rendition: true, embedding_bindings: ["embed"], reranking_available: true }],
    files: [baseFile, rerankedFile],
    baseReport: report("hybrid", [result(baseFile, 1, "base excerpt")]),
    rerankReport: report("hybrid", [result(rerankedFile, 1, "reranked excerpt")], { outcome: "applied", candidate_count: 1 }),
    rerankPending: true,
  });
  render(App);
  await screen.findAllByText("base.txt");
  await screen.findByRole("button", { name: "Process and retrieve" });
  await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
  await submitSearch("annual report");
  expect(await screen.findByText("base excerpt")).toBeTruthy();
  expect(await screen.findByText("Reranking results… Base results are shown.")).toBeTruthy();
  expect(screen.queryByText("reranked excerpt")).toBeNull();
  harness.resolveRerank(json({
    requested_mode: "hybrid",
    actual_mode: "hybrid",
    coverage: { binding_required: true, scoped_documents: 2, complete_documents: 2, state: "complete" },
    degradations: [],
    results: [result(rerankedFile, 1, "reranked excerpt")],
    truncated: false,
    trace: [],
    reranking: { outcome: "applied", candidate_count: 1 },
  }));
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
    profiles: [{ name: "private", fingerprint: "a".repeat(64), rendition: true, embedding_bindings: ["embed"], reranking_available: true }],
    files: [file],
    rerankNode: renamed,
    baseReport: report("hybrid", [result(file, 1, "base excerpt")]),
    rerankReport: report("hybrid", [result(file, 1, "reranked excerpt")], { outcome: "applied", candidate_count: 1 }),
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
    profiles: [{ name: "private", fingerprint: "a".repeat(64), rendition: true, embedding_bindings: ["embed"], reranking_available: true }],
    files: [baseFile, rerankedFile],
    baseReport: report("hybrid", [result(baseFile, 1, "base excerpt")]),
    rerankPending: true,
    tagSearch: { hits: [{ node: baseFile, path: baseFile.path, match: "name" }], limit: 1000, truncated: false },
  });
  render(App);
  await screen.findAllByText("base.txt");
  await screen.findByRole("button", { name: "Process and retrieve" });
  await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
  await submitSearch("annual report");
  await screen.findByText("Reranking results… Base results are shown.");
  await fireEvent.click(screen.getByRole("combobox", { name: "Search mode: Auto" }));
  await fireEvent.click(screen.getByRole("option", { name: "Names and text" }));
  harness.resolveRerank(json({
    requested_mode: "hybrid",
    actual_mode: "hybrid",
    coverage: { binding_required: true, scoped_documents: 2, complete_documents: 2, state: "complete" },
    degradations: [],
    results: [result(rerankedFile, 1, "reranked excerpt")],
    truncated: false,
    trace: [],
    reranking: { outcome: "applied", candidate_count: 1 },
  }));
  await waitFor(() => expect(screen.queryByText("Reranking results… Base results are shown.")).toBeNull());
  await waitFor(() => expect(screen.queryByText("reranked excerpt")).toBeNull());
  expect(screen.getByText("/base.txt")).toBeTruthy();
});

it("keeps the current selection when reranking replaces the rows", async () => {
  const baseFile = node(2, "base.txt", baseVersion);
  const rerankedFile = node(3, "reranked.txt", rerankedVersion);
  const harness = installHarness({
    profiles: [{ name: "private", fingerprint: "a".repeat(64), rendition: true, embedding_bindings: ["embed"], reranking_available: true }],
    files: [baseFile, rerankedFile],
    baseReport: report("hybrid", [result(baseFile, 1, "base excerpt"), result(rerankedFile, 2, "second excerpt")]),
    rerankReport: report("hybrid", [result(rerankedFile, 1, "reranked excerpt"), result(baseFile, 2, "base excerpt")], { outcome: "applied", candidate_count: 2 }),
    rerankPending: true,
  });
  render(App);
  await screen.findAllByText("base.txt");
  await screen.findByRole("button", { name: "Process and retrieve" });
  await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
  await submitSearch("annual report");
  await screen.findByText("second excerpt");
  await fireEvent.keyDown(window, { key: "j" });
  await fireEvent.keyDown(window, { key: "/" });
  expect(document.activeElement).toBe(screen.getByRole("searchbox", { name: "Search documents" }));
  expect(document.querySelector('tr[data-node-id="3"]')?.getAttribute("aria-selected")).toBe("true");
  harness.resolveRerank(json({
    requested_mode: "hybrid",
    actual_mode: "hybrid",
    coverage: { binding_required: true, scoped_documents: 2, complete_documents: 2, state: "complete" },
    degradations: [],
    results: [result(rerankedFile, 1, "reranked excerpt"), result(baseFile, 2, "base excerpt")],
    truncated: false,
    trace: [],
    reranking: { outcome: "applied", candidate_count: 2 },
  }));
  await screen.findByText("reranked excerpt");
  expect(harness.fetchMock.mock.calls.filter(([input, init]) => init?.signal && /^\/api\/v1\/nodes\/\d+$/.test(String(input)))).toHaveLength(4);
  expect(document.querySelector('tr[data-node-id="3"]')?.getAttribute("aria-selected")).toBe("true");
});

it("keeps base rows with a named degradation note", async () => {
  const file = node(2, "timeout.txt", baseVersion);
  const harness = installHarness({
    profiles: [{ name: "private", fingerprint: "a".repeat(64), rendition: true, embedding_bindings: ["embed"], reranking_available: true }],
    files: [file],
    baseReport: report("hybrid", [result(file, 1, "kept excerpt")]),
    rerankReport: report("hybrid", [result(file, 1, "kept excerpt")], { outcome: "degraded", cause: "timed_out", candidate_count: 1 }, ["reranking_degraded"]),
  });
  render(App);
  await screen.findAllByText("timeout.txt");
  await screen.findByRole("button", { name: "Process and retrieve" });
  await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
  await submitSearch("slow provider");
  await screen.findByText("kept excerpt");
  await screen.findByText(/Reranking unavailable \(timed_out\)/);
  expect(harness.postBodies).toHaveLength(2);
});

it("allows lexical reranking when the profile has no embedding binding", async () => {
  const file = node(2, "lexical.txt", baseVersion);
  const harness = installHarness({
    profiles: [{ name: "private", fingerprint: "a".repeat(64), rendition: true, embedding_bindings: [], reranking_available: true }],
    files: [file],
    baseReport: report("lexical", [result(file, 1, "lexical excerpt")]),
    rerankReport: report("lexical", [result(file, 1, "lexical excerpt")], { outcome: "applied", candidate_count: 1 }),
  });
  render(App);
  await screen.findAllByText("lexical.txt");
  await screen.findByRole("button", { name: "Process and retrieve" });
  await fireEvent.click(screen.getByRole("combobox", { name: "Search mode: Names and text" }));
  await fireEvent.click(screen.getByRole("option", { name: "Lexical" }));
  await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
  await submitSearch("lexical phrase");
  await screen.findByText("lexical excerpt");
  await waitFor(() => expect(harness.postBodies).toHaveLength(2));
  expect(harness.postBodies[0]?.mode).toBe("lexical");
  expect(harness.postBodies[0]?.binding_id).toBeUndefined();
  expect(harness.postBodies[1]?.rerank).toBe(true);
});

it("keeps a legacy profile-load fallback on GET when profiles are empty", async () => {
  const file = node(2, "legacy.txt", baseVersion);
  const { fetchMock } = installHarness({
    profiles: [],
    files: [file],
    baseReport: report("lexical", []),
    tagSearch: { hits: [{ node: file, path: file.path, match: "name" }], limit: 1000, truncated: false },
  });
  render(App);
  await screen.findAllByText("legacy.txt");
  await submitSearch("legacy");
  await screen.findByText("/legacy.txt");
  expect(fetchMock.mock.calls.filter(([input]) => String(input).startsWith("/api/v1/search?")).length).toBe(1);
  expect(fetchMock.mock.calls.some(([input, init]) => String(input) === "/api/v1/search" && init?.method === "POST")).toBe(false);
});

it("reruns the accepted query when profiles arrive after legacy results", async () => {
  const file = node(2, "legacy.txt", baseVersion);
  const harness = installHarness({
    profiles: [],
    profilesPending: true,
    files: [file],
    baseReport: report("hybrid", [result(file, 1, "natural excerpt")]),
    tagSearch: { hits: [{ node: file, path: file.path, match: "name" }], limit: 1000, truncated: false },
  });
  render(App);
  await submitSearch("annual report");
  await screen.findAllByText("/legacy.txt");

  harness.resolveProfiles(json([{
    name: "private",
    fingerprint: "a".repeat(64),
    rendition: true,
    embedding_bindings: ["embed"],
    reranking_available: false,
  }]));
  await screen.findByText("natural excerpt");
  expect(screen.getByRole("combobox", { name: "Search mode: Auto" })).toBeTruthy();
  expect(harness.postBodies[0]?.mode).toBe("hybrid");
});

it("defers the profile default until a pending legacy search is accepted", async () => {
  const file = node(2, "pending.txt", baseVersion);
  const harness = installHarness({
    profiles: [],
    profilesPending: true,
    files: [file],
    baseReport: report("hybrid", [result(file, 1, "natural excerpt")]),
    legacyPending: true,
    tagSearch: { hits: [{ node: file, path: file.path, match: "name" }], limit: 1000, truncated: false },
  });
  render(App);
  await submitSearch("annual report");
  await waitFor(() => expect(harness.fetchMock.mock.calls.some(([input]) => String(input).startsWith("/api/v1/search?"))).toBe(true));
  harness.resolveProfiles(json([{
    name: "private",
    fingerprint: "a".repeat(64),
    rendition: true,
    embedding_bindings: ["embed"],
    reranking_available: false,
  }]));
  harness.resolveLegacy(json({ hits: [{ node: file, path: file.path, match: "name" }], limit: 1000, truncated: false }));
  await screen.findByText("natural excerpt");
  expect(screen.getByRole("combobox", { name: "Search mode: Auto" })).toBeTruthy();
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
    tagSearch: { hits: [{ node: file, path: file.path, match: "name" }], limit: 1000, truncated: false },
  });
  render(App);
  await submitSearch("first query");
  await screen.findAllByText("/first.txt");
  await submitSearch("second query");
  await waitFor(() => expect(harness.legacyCalls()).toBe(2));

  harness.resolveProfiles(json([{
    name: "private",
    fingerprint: "a".repeat(64),
    rendition: true,
    embedding_bindings: ["embed"],
    reranking_available: false,
  }]));
  harness.resolveLegacyFailure(new Response("legacy search failure", { status: 500 }));
  await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("HTTP 500"));
  await new Promise((resolve) => setTimeout(resolve, 20));
  expect(harness.legacyCalls()).toBe(2);
  expect(screen.getByRole("combobox", { name: "Search mode: Names and text" })).toBeTruthy();
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
  expect(fetchMock.mock.calls.filter(([input]) => String(input).startsWith("/api/v1/search?")).length).toBe(1);
});

it("sends the selected tag filter in the resolver body", async () => {
  const file = node(2, "tagged.txt", baseVersion);
  const harness = installHarness({
    profiles: [{ name: "private", fingerprint: "a".repeat(64), rendition: true, embedding_bindings: ["embed"], reranking_available: false }],
    files: [file],
    baseReport: report("hybrid", [result(file, 1, "tagged excerpt")]),
  });
  render(App);
  await screen.findAllByText("tagged.txt");
  await fireEvent.click(await screen.findByRole("combobox", { name: "Browse or filter by tag: All tags" }));
  await fireEvent.click(screen.getByRole("option", { name: "reviewed (1)" }));
  await submitSearch("tagged");
  await screen.findByText("tagged excerpt");
  expect(harness.fenceBodies[0]?.filters).toEqual({ tag_id: tagID });
});

it("shows the empty-fence note without posting a search", async () => {
  const file = node(2, "empty.txt", baseVersion);
  const harness = installHarness({
    profiles: [{ name: "private", fingerprint: "a".repeat(64), rendition: true, embedding_bindings: ["embed"], reranking_available: false }],
    files: [file],
    baseReport: report("hybrid", []),
    emptyFence: true,
  });
  render(App);
  await screen.findAllByText("empty.txt");
  await submitSearch("nothing");
  await screen.findByText("No live documents match the current filter.");
  expect(harness.postBodies).toHaveLength(0);
});

it("keeps a non-401 processing failure on the legacy GET path", async () => {
  const file = node(2, "fallback.txt", baseVersion);
  const harness = installHarness({
    profiles: [{ name: "private", fingerprint: "a".repeat(64), rendition: true, embedding_bindings: ["embed"], reranking_available: true }],
    files: [file],
    baseReport: report("hybrid", []),
    baseSearchStatus: 500,
    tagSearch: { hits: [{ node: file, path: file.path, match: "name" }], limit: 1000, truncated: false },
  });
  render(App);
  await screen.findAllByText("fallback.txt");
  await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
  await submitSearch("fallback");
  await screen.findByText("/fallback.txt");
  await screen.findByText(/Natural-language search unavailable/);
  expect(screen.getByRole("combobox", { name: "Search mode: Names and text" })).toBeTruthy();
  expect(screen.queryByRole("checkbox", { name: "Rerank results" })).toBeNull();
  expect(harness.postBodies).toHaveLength(1);
  expect(harness.fetchMock.mock.calls.filter(([input]) => String(input).startsWith("/api/v1/search?")).length).toBe(1);
});

it("does not fall back to GET after a processing 401", async () => {
  const file = node(2, "unauthorized.txt", baseVersion);
  const harness = installHarness({
    profiles: [{ name: "private", fingerprint: "a".repeat(64), rendition: true, embedding_bindings: ["embed"], reranking_available: false }],
    files: [file],
    baseReport: report("hybrid", []),
    baseSearchStatus: 401,
  });
  render(App);
  await screen.findAllByText("unauthorized.txt");
  await submitSearch("unauthorized");
  await screen.findByText("Open your Docbank");
  expect(harness.fetchMock.mock.calls.filter(([input]) => String(input).startsWith("/api/v1/search?")).length).toBe(0);
});

it("ignores an aborted natural search completion", async () => {
  const file = node(2, "stale.txt", baseVersion);
  const harness = installHarness({
    profiles: [{ name: "private", fingerprint: "a".repeat(64), rendition: true, embedding_bindings: ["embed"], reranking_available: false }],
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
  expect(harness.fetchMock.mock.calls.filter(([input]) => String(input).startsWith("/api/v1/search?")).length).toBe(0);
});

it("shows why natural search controls could not load", async () => {
  installHarness({ profiles: [], profilesStatus: 503, files: [], baseReport: report("lexical", []) });
  render(App);
  expect(await screen.findByText(/Natural-language search unavailable.*503/)).toBeTruthy();
});

it.each([500, 401])("handles node lookup failures without a Names and text fallback (%i)", async (status) => {
  const good = node(2, "available.txt", baseVersion);
  const bad = node(3, "unavailable.txt", rerankedVersion);
  const harness = installHarness({
    profiles: [{ name: "private", fingerprint: "a".repeat(64), rendition: true, embedding_bindings: ["embed"], reranking_available: true }],
    files: [good, bad], nodeFailure: { id: bad.id, status },
    baseReport: report("hybrid", [result(good, 1, "available excerpt"), result(bad, 2, "unavailable excerpt")]),
  });
  render(App);
  await screen.findByRole("combobox", { name: "Search mode: Auto" });
  await submitSearch("annual report");
  if (status === 401) {
    await screen.findByText(/browser session expired or was rejected/);
  } else {
    await screen.findByText("available excerpt");
    expect(screen.queryByText("unavailable excerpt")).toBeNull();
    expect(screen.getByRole("combobox", { name: "Search mode: Auto" })).toBeTruthy();
  }
  expect(harness.fetchMock.mock.calls.some(([input]) => String(input).startsWith("/api/v1/search?"))).toBe(false);
});

it("reruns the active query when the search mode or rerank toggle changes", async () => {
  const file = node(2, "report.txt", baseVersion);
  const options: HarnessOptions = {
    profiles: [{ name: "private", fingerprint: "a".repeat(64), rendition: true, embedding_bindings: ["embed"], reranking_available: true }],
    files: [file],
    baseReport: report("hybrid", [result(file, 1, "auto excerpt")]),
  };
  const harness = installHarness(options);
  render(App);
  await screen.findByRole("combobox", { name: "Search mode: Auto" });
  await submitSearch("annual report");
  await screen.findByText("auto excerpt");
  options.baseReport = report("lexical", [result(file, 1, "lexical excerpt")]);
  await fireEvent.click(screen.getByRole("combobox", { name: "Search mode: Auto" }));
  await fireEvent.click(screen.getByRole("option", { name: "Lexical" }));
  await screen.findByText("lexical excerpt");
  expect(harness.postBodies.at(-1)?.mode).toBe("lexical");
  options.rerankReport = report("lexical", [result(file, 1, "reranked excerpt")], { outcome: "applied", candidate_count: 1 });
  await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
  await screen.findByText("reranked excerpt");
  expect(harness.postBodies.at(-1)?.rerank).toBe(true);
  await fireEvent.click(screen.getByRole("checkbox", { name: "Rerank results" }));
  await screen.findByText("lexical excerpt");
  expect(harness.postBodies.at(-1)?.rerank).toBeUndefined();
});

it("preserves the chosen sort on a natural search refresh", async () => {
  const first = node(2, "zebra.txt", baseVersion);
  const second = node(3, "alpha.txt", rerankedVersion);
  const harness = installHarness({
    profiles: [{ name: "private", fingerprint: "a".repeat(64), rendition: true, embedding_bindings: ["embed"], reranking_available: false }],
    files: [first, second],
    baseReport: report("hybrid", [result(first, 1, "first excerpt"), result(second, 2, "second excerpt")]),
  });
  render(App);
  await screen.findByRole("combobox", { name: "Search mode: Auto" });
  await submitSearch("annual report");
  await screen.findByText("first excerpt");
  await fireEvent.click(screen.getByRole("button", { name: "Document" }));
  expect(screen.getByRole("columnheader", { name: "Document" }).getAttribute("aria-sort")).toBe("ascending");
  await fireEvent.click(screen.getByRole("button", { name: "Refresh current view" }));
  await waitFor(() => expect(harness.postBodies).toHaveLength(2));
  await waitFor(() => expect(screen.getByRole("button", { name: "Process and retrieve" })).toBeTruthy());
  expect(screen.getByRole("columnheader", { name: "Document" }).getAttribute("aria-sort")).toBe("ascending");
});
