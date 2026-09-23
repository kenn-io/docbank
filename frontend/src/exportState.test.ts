import { afterEach, expect, it, vi } from "vitest";
import { ExportSession, type ExportState } from "./exportState.js";
import { exportMemberHash, type ExportJob } from "./exports.js";
import { parseQuery } from "./query.js";
import type { SnapshotPage } from "./snapshots.js";

const id = "11111111-1111-4111-8111-111111111111", hash = "a".repeat(64), future = "2099-01-01T00:00:00Z";
const members = [{ node_id: 1, version_id: id, sha256: hash, size: 12 }];
const response = (value: unknown) => new Response(JSON.stringify(value), { headers: { "Content-Type": "application/json" } });
afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers(); });

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
  session.choose({ label: "Selected documents", members }, [{ role: "original" }]);
  return { session, fetcher, state: () => state, starts: () => starts, startIDs };
}

it.each(["source", "roles"])("invalidates a reviewed plan when %s changes and fences late preview responses", async (changed) => {
  const h = await harness();
  await h.session.preview();
  expect(h.state().status).toBe("ready");
  expect(h.state().reviewed).toBeTruthy();
  h.session.choose({ label: "Selected documents", members: changed === "source" ? [{ ...members[0]!, version_id: "99999999-9999-4999-8999-999999999999" }] : members }, [{ role: changed === "roles" ? "text" : "original" }]);
  expect(h.state().reviewed).toBeUndefined();
  let respond!: (value: Response) => void, markRequested!: () => void;
  const responsePending = new Promise<Response>(resolve => respond = resolve);
  const requested = new Promise<void>(resolve => markRequested = resolve);
  h.fetcher.mockImplementationOnce(() => { markRequested(); return responsePending; });
  const pending = h.session.preview();
  await requested;
  h.session.close();
  respond(response({}));
  await pending;
  expect(h.state().reviewed).toBeUndefined();
  h.session.dispose();
});

it("keeps sealed membership when only policies change", async () => {
  const h = await harness();
  await h.session.preview();
  const frozen = h.state().reviewed?.plan.source;
  h.session.choose({ label: "Selected documents", members }, [{ role: "original" }]);
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
  h.session.choose({ label: "Selected documents", members }, [{ role: "original" }]);
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
  h.session.choose({ label: "Changed selection", members }, [{ role: "text" }]);
  expect(h.state().active?.plan).toEqual(admitted?.plan);
  expect(h.state().active?.label).toBe("Selected documents");
  h.session.close();
  await h.session.reconnect();
  expect(h.starts()).toBe(2);
  expect(h.state().status).toBe("disconnected");
  await h.session.cancel();
  expect(h.state().status).toBe("canceled");
  expect(h.state().active?.job?.state).toBe("canceled");
  h.session.dispose();
});

it.each(["start", "reconnect"] as const)("releases a refused admission during %s so the source can be previewed again", async (action) => {
  for (const status of [409, 410]) {
    const h = await harness();
    await h.session.preview();
    const original = h.fetcher.getMockImplementation()!;
    let refuse = action === "start";
    h.fetcher.mockImplementation(async (url, init) => {
      if (String(url).endsWith("/jobs")) return new Response(JSON.stringify({ detail: "Admission refused" }), { status: refuse ? status : 503 });
      if (String(url).includes("/jobs/")) return new Response(null, { status: 404 });
      return original(url, init);
    });
    await h.session.start();
    if (action === "reconnect") {
      expect(h.state().active).toBeDefined();
      expect(h.state().active?.job).toBeUndefined();
      refuse = true;
      await h.session.reconnect();
    }
    expect(h.state().active).toBeUndefined();
    expect(h.state().reviewed).toBeUndefined();
    expect(h.state().status).toBe(status === 410 ? "expired" : "error");
    expect(h.state().error?.message).toBe("Admission refused");
    h.fetcher.mockImplementation(original);
    await h.session.preview();
    expect(h.state().status).toBe("ready");
    h.session.dispose();
    h.fetcher.mockRestore();
  }
});

it("does not release an ambiguous admission when a stale create response refuses it", async () => {
  const h = await harness();
  await h.session.preview();
  let refuse!: (value: Response) => void;
  const refused = new Promise<Response>(resolve => refuse = resolve);
  h.fetcher.mockImplementationOnce(() => refused);
  const starting = h.session.start();
  h.session.close();
  const retained = h.state();
  refuse(new Response(null, { status: 409 }));
  await starting;
  expect(h.state()).toBe(retained);
  expect(h.state().active).toBeDefined();
  h.session.dispose();
});

it.each(["job", "events"])("retains admission when the %s read expires", async (read) => {
  const h = await harness();
  await h.session.preview();
  const original = h.fetcher.getMockImplementation()!;
  h.fetcher.mockImplementation(async (url, init) => {
    const path = String(url);
    if (read === "events" ? path.includes("/events") : path.includes("/jobs/")) return new Response(null, { status: 410 });
    return original(url, init);
  });
  await h.session.start();
  expect(h.state().status).toBe("expired");
  expect(h.state().active).toBeDefined();
  expect(!!h.state().active?.job).toBe(read === "events");
  h.session.dispose();
});

it("validates the download filename without changing the completed export and allows ticket retries", async () => {
  const h = await harness();
  const original = h.fetcher.getMockImplementation()!;
  let completed: ExportJob;
  let ticketStatus = 503;
  const requestedNames: string[] = [], offeredNames: string[] = [];
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) { offeredNames.push(this.download); });
  h.fetcher.mockImplementation(async (url, init) => {
    const path = String(url);
    if (path.endsWith("/jobs")) {
      const body = JSON.parse(String(init?.body));
      completed = { id: body.operation_id, plan_id: body.plan_id, fingerprint: body.fingerprint, state: "completed", sequence: 2, completed_roles: 1, completed_bytes: 12, attempt: 1, created_at: "2026-01-01T00:00:00Z", deadline: future, expires_at: future, receipt: { format: "docbank-bundle-v1", plan_fingerprint: body.fingerprint, sha256: hash, size: 512, entries: 4 } };
      return response(completed);
    }
    if (path.endsWith("/download")) {
      requestedNames.push(JSON.parse(String(init?.body)).basename);
      if (ticketStatus) return new Response(null, { status: ticketStatus });
      return response({ url: `/api/daemon/web-download/file?ticket=${"a".repeat(43)}`, receipt: completed.receipt });
    }
    return original(url, init);
  });
  await h.session.preview();
  const reviewed = h.state().reviewed;
  await h.session.start();
  expect(h.state().status).toBe("completed");
  await h.session.download("../invalid.zip");
  expect(requestedNames).toEqual([]);
  expect(h.state().error?.message).toMatch(/ZIP basename/);
  expect(h.state().status).toBe("completed");
  await h.session.download("Chosen.zip");
  expect(h.state().error?.message).toBe("HTTP 503");
  expect(h.state().status).toBe("completed");
  ticketStatus = 0;
  await h.session.download("Retried.zip");
  expect(requestedNames).toEqual(["Chosen.zip", "Retried.zip"]);
  expect(offeredNames).toEqual(["Retried.zip"]);
  expect(h.state().downloadOffered).toBe(true);
  expect(h.state().error).toBeUndefined();
  expect(h.state().reviewed).toBe(reviewed);
  expect(h.state().active?.plan).toBe(reviewed?.plan);
  ticketStatus = 410;
  await h.session.download("Expired.zip");
  expect(h.state().status).toBe("expired");
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
  h.session.choose({ label: "Frozen query", snapshot: new Proxy(snapshot, {}) }, [{ role: "original" }]);
  snapshot.rows[0]!.content_version_id = "99999999-9999-4999-8999-999999999999";
  await h.session.preview();
  expect(h.state().status).toBe("ready");
  expect(h.state().reviewed?.plan.source.member_hash).toBe(await exportMemberHash(members));
  h.session.dispose();
});

it("starts over after a failed source without changing ordinary retry identity or an admitted job", async () => {
  const h = await harness();
  const original = h.fetcher.getMockImplementation()!;
  const sourceIDs: string[] = [];
  let failedID = "";
  h.fetcher.mockImplementation(async (url, init) => {
    if (String(url).endsWith("/sources")) {
      const operationID = JSON.parse(String(init?.body)).operation_id;
      sourceIDs.push(operationID);
      failedID ||= operationID;
      if (operationID === failedID) return new Response(JSON.stringify({ detail: "Source preparation failed", code: "export_conflict" }), { status: 409 });
    }
    return original(url, init);
  });
  await h.session.discoverRecipes();
  h.session.choose({ label: "Selected documents", members }, [{ role: "original" }]);
  await h.session.preview();
  expect(h.state().status).toBe("error");
  expect(sourceIDs).toEqual([failedID, failedID]);
  h.session.resetPreparation();
  await h.session.preview();
  expect(h.state().status).toBe("ready");
  expect(sourceIDs[2]).not.toBe(failedID);
  await h.session.start();
  const admitted = h.state();
  h.session.resetPreparation();
  expect(h.state()).toBe(admitted);
  h.session.dispose();
});

it.each([false, true])("keeps plan expiry and readiness through problem paging (page fails: %s)", async (fails) => {
  vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout"] });
  vi.setSystemTime(Date.parse(future) - 1000);
  const h = await harness();
  const original = h.fetcher.getMockImplementation()!;
  let pageFails = fails;
  h.fetcher.mockImplementation(async (url, init) => {
    if (String(url).includes("/problems")) {
      if (pageFails) return new Response(null, { status: 503 });
      return response({ plan_id: h.state().reviewed!.plan.id, fingerprint: hash, after: 0, next: 0, total: 0, items: [] });
    }
    return original(url, init);
  });
  await h.session.preview();
  const reviewed = h.state().reviewed;
  await h.session.problemPage(0);
  expect(h.state().status).toBe("ready");
  expect(h.state().reviewed).toBe(reviewed);
  expect(h.state().problemsError?.message).toBe(fails ? "HTTP 503" : undefined);
  expect(h.state().error).toBeUndefined();
  pageFails = false;
  await h.session.problemPage(0);
  expect(h.state().status).toBe("ready");
  expect(h.state().problemsError).toBeUndefined();
  await vi.advanceTimersByTimeAsync(1001);
  expect(h.state().status).toBe("expired");
  expect(h.state().reviewed).toBeUndefined();
  h.session.dispose();
});

it("cancels pending problem pages when the drawer closes without accepting their late result", async () => {
  const h = await harness();
  await h.session.preview();
  let respond!: (response: Response) => void;
  let pageSignal: AbortSignal | undefined;
  h.fetcher.mockImplementationOnce((_url, init) => {
    pageSignal = init?.signal as AbortSignal;
    return new Promise<Response>(resolve => respond = resolve);
  });
  const paging = h.session.problemPage(0);
  expect(h.state().problemsLoading).toBe(true);
  h.session.close();
  expect(pageSignal?.aborted).toBe(true);
  expect(h.state().problemsLoading).toBe(false);
  respond(new Response(null, { status: 503 }));
  await paging;
  expect(h.state().problemsError).toBeUndefined();
  expect(h.state().status).toBe("ready");
  h.session.dispose();
});

it("asks for an attachment set and keeps the explicit choice across option changes", async () => {
  const h = await harness(), memberHash = await exportMemberHash(members);
  const underlying = h.fetcher.getMockImplementation()!;
  h.fetcher.mockImplementation((url, init) => {
    if (String(url).includes("/attachment-publications")) return Promise.resolve(response({ source_id: String(url).split("/").at(-2), member_hash: memberHash, after: 0, next: 0, total: 2, items: ["first", "second"].map(operation_id => ({ node_id: 1, version_id: id, name: "empty.eml", operation_id, generation_id: hash, created_at: "2026-01-01T00:00:00Z", state: "complete", attachments: 0 })) }));
    return underlying(url, init);
  });
  h.session.choose({ label: "Selected documents", members }, [{ role: "attachment_original" }]);
  await h.session.preview();
  expect(h.state().status).toBe("idle");
  expect(h.state().publications?.items).toHaveLength(2);
  expect(h.fetcher.mock.calls.filter(([url]) => String(url).endsWith("/plans"))).toHaveLength(0);
  h.session.selectPublication(id, "second");
  h.session.choose({ label: "Selected documents", members }, [{ role: "attachment_original", allow_unavailable: true }]);
  await h.session.preview();
  const request = h.fetcher.mock.calls.find(([url]) => String(url).endsWith("/plans"));
  expect(JSON.parse(String(request?.[1]?.body)).publications).toEqual([{ version_id: id, operation_id: "second" }]);
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2100-01-01T00:00:00Z"));
  await h.session.publicationPage(0);
  expect(h.state().publicationSelections).toBeUndefined();
  h.session.resetPreparation();
  expect(h.state().publications).toBeUndefined();
  expect(h.state().publicationSelections).toBeUndefined();
  h.session.dispose();
});
