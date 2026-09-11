// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { APIError } from "./api.js";
import { parseQuery, queryFingerprint } from "./query.js";
import { SnapshotSession, type SnapshotState } from "./snapshotState.js";

const query = parseQuery("{}");
const otherQuery = parseQuery('{"text":"new"}');
const version = "22222222-2222-4222-8222-222222222222";

function snapshotRow(nodeID: number): Record<string, unknown> {
  return {
    node_id: nodeID,
    content_version_id: `00000000-0000-4000-8000-${String(nodeID).padStart(12, "0")}`,
    blob_hash: nodeID.toString(16).padStart(64, "0"),
    size: nodeID,
    revision: nodeID,
    name: `synthetic-${nodeID}.txt`,
    path: `/synthetic-${nodeID}.txt`,
    mime_type: "text/plain",
    media_family: "text",
    modified_at: "2026-09-11T00:00:00Z",
    sort_key: `synthetic-${nodeID}.txt`,
    tags: [],
    collection_ids: [],
  };
}

async function receipt(overrides: Record<string, unknown> = {}): Promise<Record<string, unknown>> {
  const selectedQuery = (overrides.query as typeof query | undefined) ?? query;
  return {
    query: selectedQuery,
    dependencies: [],
    query_fingerprint: await queryFingerprint(selectedQuery),
    member_hash: "a".repeat(64),
    snapshot_fingerprint: `sha256:${"b".repeat(64)}`,
    generation: { kind: "native" },
    coverage: { configuration: "unconfigured" },
    observed_at: "2026-09-11T00:00:00Z",
    page_size: 50,
    total: 1,
    total_bytes: 10,
    rows: [{ ...snapshotRow(2), content_version_id: version, blob_hash: "c".repeat(64), size: 10 }],
    facets: [], snapshot: true, snapshot_id: "0123456789abcdef0123456789abcdef",
    created_at: "2026-09-11T00:00:00Z", expires_at: "2026-09-11T00:15:00Z",
    ...overrides,
  };
}

function response(value: unknown): Response {
  return new Response(JSON.stringify(value), { status: 200, headers: { "Content-Type": "application/json" } });
}

function deferred<T>(): { promise: Promise<T>; resolve(value: T): void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

afterEach(() => vi.restoreAllMocks());

describe("snapshot request-epoch state", () => {
  it("publishes immutable loading and ready states with the accepted original first page", async () => {
    const states: Readonly<SnapshotState>[] = [];
    vi.spyOn(globalThis, "fetch").mockResolvedValue(response(await receipt({ facets: [{
      dimension: "tags", available: true, values: [], total: 1, missing: 1, other: 0,
    }] })));
    const session = new SnapshotSession("session", (state) => states.push(state));

    await session.run(query, { page_size: 50, facets: ["tags"] });

    expect(states.map((state) => state.status)).toEqual(["idle", "loading", "ready"]);
    const ready = states.at(-1)!;
    expect(Object.isFrozen(ready)).toBe(true);
    expect(ready.firstPage).toBe(ready.page);
    expect(ready.query).toEqual(query);
    expect(ready.options).toEqual({ page_size: 50, facets: ["tags"] });
    expect(ready.offset).toBe(0);
  });

  it("pages in both directions while retaining the accepted first page", async () => {
    const states: Readonly<SnapshotState>[] = [];
    const firstRows = Array.from({ length: 50 }, (_, index) => snapshotRow(index + 1));
    const first = await receipt({ rows: firstRows, total: 51, total_bytes: 1326, next_cursor: "next-1" });
    const last = await receipt({ rows: [snapshotRow(51)], total: 51, total_bytes: 1326, previous_cursor: "previous-1" });
    const firstAgain = await receipt({ rows: firstRows, total: 51, total_bytes: 1326, next_cursor: "next-2" });
    vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(response(first))
      .mockResolvedValueOnce(response(last))
      .mockResolvedValueOnce(response(firstAgain));
    const session = new SnapshotSession("session", (state) => states.push(state));

    await session.run(query, { page_size: 50 });
    const acceptedFirst = states.at(-1)!.firstPage;
    await session.page("next");
    expect(states.at(-1)).toMatchObject({ status: "ready", offset: 50 });
    expect(states.at(-1)!.firstPage).toBe(acceptedFirst);
    await session.page("previous");
    expect(states.at(-1)).toMatchObject({ status: "ready", offset: 0 });
    expect(states.at(-1)!.firstPage).toBe(acceptedFirst);
  });

  it("keeps 410 expired until an explicit run", async () => {
    const states: Readonly<SnapshotState>[] = [];
    const firstRows = Array.from({ length: 50 }, (_, index) => snapshotRow(index + 1));
    vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(response(await receipt({
        rows: firstRows, total: 51, total_bytes: 1326, next_cursor: "next-1",
      })))
      .mockRejectedValueOnce(new APIError("gone", 410, "snapshot_gone"))
      .mockResolvedValueOnce(response(await receipt({ query: otherQuery })));
    const session = new SnapshotSession("session", (state) => states.push(state));

    await session.run(query, { page_size: 50 });
    await session.page("next");
    expect(states.at(-1)?.status).toBe("expired");
    const count = states.length;
    await session.page("next");
    expect(states).toHaveLength(count);
    await session.run(otherQuery, { page_size: 50 });
    expect(states.at(-1)).toMatchObject({ status: "ready", query: otherQuery });
  });

  it("does not publish a delayed decoded response after a newer run", async () => {
    const states: Readonly<SnapshotState>[] = [];
    const stale = deferred<Response>();
    vi.spyOn(globalThis, "fetch")
      .mockImplementationOnce(() => stale.promise)
      .mockResolvedValueOnce(response(await receipt({ query: otherQuery })));
    const session = new SnapshotSession("session", (state) => states.push(state));

    const oldRun = session.run(query, { page_size: 50 });
    await session.run(otherQuery, { page_size: 50 });
    stale.resolve(response(await receipt()));
    await oldRun;

    expect(states.at(-1)).toMatchObject({ status: "ready", query: otherQuery });
    expect(states.filter((state) => state.status === "ready")).toHaveLength(1);
  });

  it("checks the epoch after delayed digest work", async () => {
    const states: Readonly<SnapshotState>[] = [];
    const staleDigest = deferred<ArrayBuffer>();
    const realDigest = crypto.subtle.digest.bind(crypto.subtle);
    const digestSpy = vi.spyOn(crypto.subtle, "digest")
      .mockImplementationOnce(() => staleDigest.promise)
      .mockImplementation((algorithm, data) => realDigest(algorithm, data));
    vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(response(await receipt()))
      .mockResolvedValueOnce(response(await receipt({ query: otherQuery })));
    const session = new SnapshotSession("session", (state) => states.push(state));

    const oldRun = session.run(query, { page_size: 50 });
    while (digestSpy.mock.calls.length === 0) await Promise.resolve();
    await session.run(otherQuery, { page_size: 50 });
    staleDigest.resolve(await realDigest("SHA-256", new TextEncoder().encode(JSON.stringify(query))));
    await oldRun;

    expect(states.at(-1)).toMatchObject({ status: "ready", query: otherQuery });
    expect(states.filter((state) => state.status === "ready")).toHaveLength(1);
  });

  it("suppresses delayed work after dispose", async () => {
    const states: Readonly<SnapshotState>[] = [];
    const pending = deferred<Response>();
    vi.spyOn(globalThis, "fetch").mockImplementation(() => pending.promise);
    const session = new SnapshotSession("session", (state) => states.push(state));
    const run = session.run(query, {});

    session.dispose();
    pending.resolve(response(await receipt()));
    await run;

    expect(states.map((state) => state.status)).toEqual(["idle", "loading"]);
  });

  it("runSaved applies the same query/options state and revision-fenced transport", async () => {
    const states: Readonly<SnapshotState>[] = [];
    const snapshot = await receipt({ page_size: 100 });
    vi.spyOn(globalThis, "fetch").mockResolvedValue(response({
      run: {
        run_id: "44444444-4444-4444-8444-444444444444",
        saved_query_id: "33333333-3333-4333-8333-333333333333",
        saved_query_revision: 3,
        query_fingerprint: snapshot.query_fingerprint,
        snapshot_id: snapshot.snapshot_id,
        member_hash: snapshot.member_hash,
        total: snapshot.total,
        total_bytes: snapshot.total_bytes,
        ran_at: "2026-09-11T00:00:00Z",
        expires_at: "2026-09-11T00:15:00Z",
        comparison: { hash_changed: false, total_delta: 0, definition_changed: false },
      }, snapshot,
    }));
    const session = new SnapshotSession("session", (state) => states.push(state));

    await session.runSaved("33333333-3333-4333-8333-333333333333", 3, query, { profile: "archive" });

    expect(states.at(-1)).toMatchObject({ status: "ready", firstPage: snapshot, query, options: { profile: "archive" } });
  });
});
