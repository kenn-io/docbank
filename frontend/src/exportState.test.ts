import { afterEach, expect, it, vi } from "vitest";
import { ExportSession, type ExportState } from "./exportState.js";
import { exportMemberHash } from "./exports.js";
import { parseQuery } from "./query.js";
import type { SnapshotPage } from "./snapshots.js";

const id = "11111111-1111-4111-8111-111111111111", hash = "a".repeat(64), future = "2099-01-01T00:00:00Z";
const members = [{ node_id: 1, version_id: id, sha256: hash, size: 12 }];
const response = (value: unknown) => new Response(JSON.stringify(value), { headers: { "Content-Type": "application/json" } });
afterEach(() => vi.restoreAllMocks());

async function harness() {
  const memberHash = await exportMemberHash(members);
  let state: Readonly<ExportState> = { status: "idle" };
  let plan: any, source: any, job: any;
  let starts = 0;
  const startIDs: string[] = [];
  const fetcher = vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
    const path = String(url), body = init?.body ? JSON.parse(String(init.body)) : undefined;
    if (path.endsWith("/sources")) { source = { id: body.operation_id, request_sha256: hash, kind: "explicit", state: "sealed", member_hash: memberHash, total: 1, source_bytes: 12, created_at: "2026-01-01T00:00:00Z", expires_at: future }; return response(source); }
    if (path.endsWith("/plans")) { plan = { format: "docbank-bundle-v1", id: body.operation_id, vault_id: id, toolchain: "go1.27", source, roles: body.roles, fingerprint: hash, total: 1, role_entries: 1, role_bytes: 12, metadata_bytes: 100, created_at: source.created_at, expires_at: future }; return response(plan); }
    if (path.endsWith("/preview")) return response({ plan_id: plan.id, fingerprint: hash, member_hash: memberHash, total: 1, roles: [{ role: "original", available_members: 1, unavailable_members: 0, files: 1, bytes: 12 }] });
    if (path.endsWith("/jobs")) { starts++; startIDs.push(body.operation_id); job = { id: body.operation_id, plan_id: plan.id, fingerprint: hash, state: "queued", sequence: 1, completed_roles: 0, completed_bytes: 0, attempt: 0, created_at: source.created_at, deadline: future, expires_at: future }; throw new TypeError("start response lost"); }
    if (path.includes("/events")) return new Response("", { headers: { "Content-Type": "application/x-ndjson" } });
    if (path.endsWith("/cancel")) { job = { ...job, state: "canceled", sequence: 2 }; return new Response(null, { status: 204 }); }
    return response(job);
  });
  const session = new ExportSession("s", next => state = next);
  session.choose({ label: "Selected documents", members }, [{ role: "original" }], "docbank-bundle.zip");
  return { session, fetcher, state: () => state, starts: () => starts, startIDs };
}

it("invalidates a reviewed plan when naming changes and fences late preview responses", async () => {
  const h = await harness();
  await h.session.preview();
  expect(h.state().status).toBe("ready");
  expect(h.state().reviewed).toBeTruthy();
  h.session.choose({ label: "Selected documents", members }, [{ role: "original" }], "Review.zip");
  expect(h.state().reviewed).toBeUndefined();
  let resolve: (value: Response) => void = () => {};
  h.fetcher.mockImplementationOnce(() => new Promise(r => resolve = r));
  const pending = h.session.preview();
  await new Promise(r => setTimeout(r, 0));
  h.session.close();
  resolve(response({}));
  await pending;
  expect(h.state().reviewed).toBeUndefined();
  h.session.dispose();
});

it("keeps sealed membership when only policies or naming change", async () => {
  const h = await harness();
  await h.session.preview();
  const frozen = h.state().reviewed?.plan.source;
  h.session.choose({ label: "Selected documents", members }, [{ role: "original" }], "Review.zip");
  await h.session.preview();
  expect(h.state().status).toBe("ready");
  expect(h.state().reviewed?.plan.source).toEqual(frozen);
  expect(h.fetcher.mock.calls.filter(([url]) => String(url).endsWith("/sources"))).toHaveLength(1);
  h.session.dispose();
});

it("discovers recipes against the same sealed source later used by preview", async () => {
  const h = await harness();
  const underlying = h.fetcher.getMockImplementation()!;
  const memberHash = await exportMemberHash(members);
  h.fetcher.mockImplementation((url, init) => {
    if (String(url).endsWith("/email-pdf-recipes")) return Promise.resolve(response({ source_id: String(url).split("/").at(-2), member_hash: memberHash, total: 1, recipes: [{ recipe_sha256: hash, paper: "A4", renderer_version: "151.0.7922.34", messages: 1, ambiguous: 0 }] }));
    return underlying(url, init);
  });
  await h.session.discoverRecipes();
  expect(h.state().recipes?.[0]?.paper).toBe("A4");
  h.session.choose({ label: "Selected documents", members }, [{ role: "original" }], "Review.zip");
  expect(h.state().recipes).toHaveLength(1);
  await h.session.preview();
  expect(h.state().status).toBe("ready");
  expect(h.fetcher.mock.calls.filter(([url]) => String(url).endsWith("/sources"))).toHaveLength(1);
  h.session.dispose();
});

it("recovers a lost start with the same UUID, retains the handle on close, and confirms cancellation", async () => {
  const h = await harness();
  await h.session.preview();
  await h.session.start();
  expect(h.starts()).toBe(2);
  expect(new Set(h.startIDs).size).toBe(1);
  expect(h.state().status).toBe("disconnected");
  expect(h.state().active?.job?.state).toBe("queued");
  const admitted = h.state().active;
  h.session.choose({ label: "Changed selection", members }, [{ role: "text" }], "Changed.zip");
  expect(h.state().active?.plan).toEqual(admitted?.plan);
  expect(h.state().active?.basename).toBe("docbank-bundle.zip");
  h.session.close();
  await h.session.reconnect();
  expect(h.starts()).toBe(2);
  expect(h.state().status).toBe("disconnected");
  await h.session.cancel();
  expect(h.state().status).toBe("canceled");
  expect(h.state().active?.job?.state).toBe("canceled");
  h.session.dispose();
});

it("cannot start an expired reviewed plan", async () => {
  const h = await harness();
  await h.session.preview();
  vi.spyOn(Date, "now").mockReturnValue(Date.parse(future) + 1);
  await h.session.start();
  expect(h.state().status).toBe("expired");
  expect(h.starts()).toBe(0);
  h.session.dispose();
});

it("copies reactive snapshot inputs before caller changes and never promotes observed revisions to preconditions", async () => {
  const h = await harness();
  const snapshot: SnapshotPage = {
    query: parseQuery("{}"), dependencies: [], query_fingerprint: `sha256:${hash}`,
    member_hash: await exportMemberHash(members), snapshot_fingerprint: `sha256:${hash}`,
    generation: { kind: "native" }, coverage: { configuration: "unconfigured" },
    observed_at: "2026-01-01T00:00:00Z", page_size: 100, total: 1, total_bytes: 12,
    rows: [{ node_id: 1, content_version_id: id, blob_hash: hash, size: 12, revision: 99, name: "synthetic.txt", path: "/synthetic.txt", mime_type: "text/plain", media_family: "text", modified_at: "2026-01-01T00:00:00Z", sort_key: "synthetic.txt", tags: [], collection_ids: [] }],
    facets: [], snapshot: true, snapshot_id: "a".repeat(32), created_at: "2026-01-01T00:00:00Z", expires_at: future,
  };
  h.session.choose({ label: "Frozen query", snapshot: new Proxy(snapshot, {}) }, [{ role: "original" }], "docbank-bundle.zip");
  snapshot.rows[0]!.content_version_id = "99999999-9999-4999-8999-999999999999";
  await h.session.preview();
  expect(h.state().status).toBe("ready");
  expect(h.state().reviewed?.plan.source.member_hash).toBe(await exportMemberHash(members));
  h.session.dispose();
});
