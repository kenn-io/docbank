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
  files: ReturnType<typeof node>[];
  baseReport: unknown;
  rerankReport?: unknown;
  rerankPending?: boolean;
  tagSearch?: unknown;
}

function installHarness(options: HarnessOptions) {
  history.replaceState(null, "", "/#web_session=short-lived&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
  const root = { id: 1, name: "", kind: "dir", size: 0, revision: 1, created_at: "2026-09-21T00:00:00Z", modified_at: "2026-09-21T00:00:00Z", path: "/" };
  const postBodies: Record<string, unknown>[] = [];
  let rerankResolve: ((response: Response) => void) | undefined;
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
    if (url === "/api/v1/processing/profiles") return json(options.profiles);
    const nodeMatch = url.match(/^\/api\/v1\/nodes\/(\d+)$/);
    if (nodeMatch) {
      const file = options.files.find((item) => item.id === Number(nodeMatch[1]));
      return file ? json(file) : new Response("", { status: 404 });
    }
    if (url === "/api/v1/processing/source-fences/resolve" && method === "POST") {
      return json({
        fence: { vault_uid: vaultID, content_version_ids: [baseVersion, rerankedVersion] },
        observed_scope_count: 2,
        fence_fingerprint: fenceFingerprint(vaultID, [baseVersion, rerankedVersion]),
      });
    }
    if (url === "/api/v1/search" && method === "POST") {
      const body = JSON.parse(String(init?.body ?? "{}")) as Record<string, unknown>;
      postBodies.push(body);
      if (body.rerank) {
        if (options.rerankPending) return rerankPending;
        return json(options.rerankReport ?? options.baseReport);
      }
      return json(options.baseReport);
    }
    if (url.startsWith("/api/v1/search?") && method === "GET") return json(options.tagSearch ?? { hits: [], limit: 1000, truncated: false });
    if (url.startsWith("/api/v1/audit/status?")) return json({ enabled: false, scopes: [] });
    if (url.startsWith("/api/v1/nodes/") && url.includes("/tags?")) return json({ items: [], total: 0, limit: 1000, offset: 0 });
    throw new Error(`unexpected request: ${method} ${url}`);
  });
  return { fetchMock, postBodies, resolveRerank: (response: Response) => rerankResolve?.(response) };
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
