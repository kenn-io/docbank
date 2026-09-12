import { afterEach, expect, it, vi } from "vitest";
import { copyExportMembers, sealExportSource, parseExportJob, parseExportPlan, parseExportPreview, readExportEvents, exportTicket, safeExportBasename } from "./exports.js";
import { snapshotMemberHash } from "./snapshots.js";

const id = "11111111-1111-4111-8111-111111111111";
const planID = "22222222-2222-4222-8222-222222222222";
const jobID = "33333333-3333-4333-8333-333333333333";
const hash = "a".repeat(64);
const future = "2099-01-01T00:00:00Z";
const source = { id, request_sha256: hash, kind: "explicit", state: "sealed", member_hash: hash, total: 1, source_bytes: 12, created_at: "2026-01-01T00:00:00Z", expires_at: future };
const plan = { format: "docbank-bundle-v1", id: planID, vault_id: id, toolchain: "go1.27", source, roles: [{ role: "original" }], fingerprint: hash, total: 1, role_entries: 1, role_bytes: 12, metadata_bytes: 100, created_at: source.created_at, expires_at: future };
const queued = { id: jobID, plan_id: planID, fingerprint: hash, state: "queued", sequence: 1, completed_roles: 0, completed_bytes: 0, attempt: 0, created_at: source.created_at, deadline: future, expires_at: future };
const complete = { ...queued, state: "completed", sequence: 8, completed_roles: 1, completed_bytes: 12, receipt: { format: "docbank-bundle-v1", plan_fingerprint: hash, sha256: "b".repeat(64), size: 512, entries: 4 } };
function response(value: unknown) { return new Response(JSON.stringify(value), { headers: { "Content-Type": "application/json" } }); }
function events(lines: string[]) { return new Response(lines.join("\n") + "\n", { headers: { "Content-Type": "application/x-ndjson" } }); }
afterEach(() => vi.restoreAllMocks());

it("copies exact content members, drops observed mutation revisions, and rejects duplicates and unsafe sizes", () => {
  const members = [{ node_id: 1, content_version_id: id, blob_hash: hash, size: 12, revision: 99 }];
  expect(copyExportMembers(members)).toEqual([{ node_id: 1, version_id: id, sha256: hash, size: 12 }]);
  expect(() => copyExportMembers([...members, ...members])).toThrow();
  expect(() => copyExportMembers([{ ...members[0]!, size: Number.MAX_SAFE_INTEGER + 1 }])).toThrow();
  expect(() => copyExportMembers([])).toThrow();
});

it("retries the same 1001-member source and chunks after response loss and verifies the seal", async () => {
  const members = Array.from({ length: 1001 }, (_, i) => ({ node_id: i + 1, version_id: id, sha256: hash, size: 1 }));
  const memberHash = await snapshotMemberHash(members.map(m => ({ node_id: m.node_id, content_version_id: m.version_id })));
  let sourceCalls = 0;
  const chunks: number[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
    const path = String(url);
    const body = JSON.parse(String(init?.body));
    if (path.endsWith("/sources")) {
      expect(body).toEqual({ operation_id: id, kind: "upload", total: 1001, member_hash: memberHash });
      if (++sourceCalls === 1) throw new TypeError("response lost");
      return response({ ...source, kind: "upload", state: "uploading", total: 1001, source_bytes: 0, member_hash: memberHash });
    }
    if (path.includes("/chunks/")) {
      chunks.push(body.members.length);
      if (chunks.length === 1) throw new TypeError("chunk response lost");
      return new Response(null, { status: 204 });
    }
    return response({ ...source, kind: "upload", total: 1001, source_bytes: 1001, member_hash: memberHash });
  });
  const sealed = await sealExportSource("session", members, id, new AbortController().signal);
  expect(sealed.total).toBe(1001);
  expect(chunks).toEqual([1000, 1000, 1]);
});

it("rejects plan/source disagreement, unsafe progress, missing completion receipt and wrong receipt identity", () => {
  expect(parseExportPlan(plan, source, plan.roles, planID).fingerprint).toBe(hash);
  expect(() => parseExportPlan({ ...plan, total: 2 }, source, plan.roles, planID)).toThrow();
  expect(() => parseExportJob({ ...queued, sequence: 1.5 }, plan, jobID)).toThrow();
  expect(() => parseExportJob({ ...complete, receipt: undefined }, plan, jobID)).toThrow();
  expect(() => parseExportJob({ ...complete, receipt: { ...complete.receipt, entries: 3 } }, plan, jobID)).toThrow();
  expect(() => parseExportJob({ ...queued, completed_bytes: 13 }, plan, jobID)).toThrow();
  expect(() => parseExportJob({ ...queued, plan_id: id }, plan, jobID)).toThrow();
});

it("reconciles frozen availability against role files and total members", () => {
  const preview = { plan_id: planID, fingerprint: hash, member_hash: hash, total: 1, roles: [{ role: "original", available_members: 1, unavailable_members: 0, files: 1, bytes: 12 }] };
  expect(parseExportPreview(preview, plan).total).toBe(1);
  expect(() => parseExportPreview({ ...preview, roles: [{ ...preview.roles[0], unavailable_members: 1 }] }, plan)).toThrow();
  expect(() => parseExportPreview({ ...preview, roles: [{ ...preview.roles[0], bytes: 11 }] }, plan)).toThrow();
});

it("accepts qualified PDF policies, mixed attachment availability and explicit collapsed volume counts", () => {
  const policies = [{ role: "email_pdf", recipe_sha256: hash }, { role: "attachment_pdf", recipe_sha256: hash, allow_unavailable: true }];
  const batch = { ...plan, roles: policies, document_rows: 3, duplicate_policy: "collapse_exact_content", volume_limits: { roles: 1, role_bytes: 1024 }, volumes: 1,
    counts: { messages: 1, attachments: 2, email_pdfs: 1, attachment_pdfs: 0, pages: 2, collapsed: 1, unavailable: 1, unavailable_inventories: 0 } };
  const parsed = parseExportPlan(batch, source, policies, planID, { duplicate_policy: "collapse_exact_content", volume_limits: batch.volume_limits });
  expect(parsed.counts?.pages).toBe(2);
  const preview = { plan_id: planID, fingerprint: hash, member_hash: hash, total: 1, roles: [
    { role: "email_pdf", available_members: 1, unavailable_members: 0, files: 1, bytes: 12 },
    { role: "attachment_pdf", available_members: 1, unavailable_members: 1, files: 0, collapsed_files: 1, bytes: 0, unavailable_reason: "Unsupported child." },
  ] };
  expect(parseExportPreview(preview, parsed).roles[1]?.collapsed_files).toBe(1);
  expect(() => parseExportPlan({ ...batch, duplicate_policy: "preserve" }, source, policies, planID, { duplicate_policy: "collapse_exact_content", volume_limits: batch.volume_limits })).toThrow();
  expect(() => parseExportPlan({ ...batch, counts: { ...batch.counts, email_pdfs: 2 } }, source, policies, planID, { duplicate_policy: "collapse_exact_content", volume_limits: batch.volume_limits })).toThrow();
  expect(parseExportJob(complete, parsed, jobID).receipt?.entries).toBe(4);
});

it("accepts current-state gaps and byte-identical duplicates, never treats EOF as completion", async () => {
  const first = JSON.stringify({ delivery: "current_state", requested_after: 0, job: queued });
  const last = JSON.stringify({ delivery: "current_state", requested_after: 0, job: complete });
  vi.spyOn(globalThis, "fetch").mockResolvedValue(events([first, first, last]));
  const seen: number[] = [];
  const gaps: boolean[] = [];
  await readExportEvents("session", plan, jobID, undefined, new AbortController().signal, (job, gap) => { seen.push(job.sequence); gaps.push(gap); });
  expect(seen).toEqual([1, 8]);
  expect(gaps).toEqual([false, true]);
  vi.mocked(fetch).mockResolvedValue(events([first]));
  await expect(readExportEvents("session", plan, jobID, undefined, new AbortController().signal, () => {})).rejects.toThrow(/closed/);
});

it("rejects conflicting duplicates, backwards, truncated and oversized progress", async () => {
  const line = (job: unknown) => JSON.stringify({ delivery: "current_state", requested_after: 0, job });
  for (const content of [events([line(queued), line({ ...queued, state: "running" })]), events([line({ ...queued, sequence: 3 }), line(queued)]), new Response(line(complete)), new Response("a".repeat(65537))]) {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(content);
    await expect(readExportEvents("s", plan, jobID, undefined, new AbortController().signal, () => {})).rejects.toThrow();
  }
});

it("withholds completion until the terminal stream closes cleanly and ignores identical terminal duplicates", async () => {
  const last = JSON.stringify({ delivery: "current_state", requested_after: 0, job: complete });
  vi.spyOn(globalThis, "fetch").mockResolvedValue(events([last, "malformed"]));
  const published: string[] = [];
  await expect(readExportEvents("s", plan, jobID, undefined, new AbortController().signal, job => published.push(job.state))).rejects.toThrow();
  expect(published).toEqual([]);
  vi.mocked(fetch).mockResolvedValue(events([last, last]));
  await readExportEvents("s", plan, jobID, undefined, new AbortController().signal, job => published.push(job.state));
  expect(published).toEqual(["completed"]);
});

it("requires exact receipt and native same-origin ticket and rejects unsafe basenames", async () => {
  for (const name of ["../x.zip", "CON.zip", "x\n.zip", "a/b.zip", "foo", ".zip", "x.zip "]) expect(() => safeExportBasename(name)).toThrow();
  expect(safeExportBasename("Review.zip")).toBe("Review.zip");
  const url = `/api/daemon/web-download/file?ticket=${"a".repeat(43)}`;
  vi.spyOn(globalThis, "fetch").mockResolvedValue(response({ url, receipt: complete.receipt }));
  expect((await exportTicket("s", plan, complete, "Review.zip", new AbortController().signal)).url).toBe(url);
  for (const bad of ["https://elsewhere.test/file", `${url}&other=1`, "//elsewhere.test/file", `${url}#fragment`]) {
    vi.mocked(fetch).mockResolvedValue(response({ url: bad, receipt: complete.receipt }));
    await expect(exportTicket("s", plan, complete, "Review.zip", new AbortController().signal)).rejects.toThrow();
  }
  vi.mocked(fetch).mockResolvedValue(response({ url, receipt: { ...complete.receipt, size: 999 } }));
  await expect(exportTicket("s", plan, complete, "Review.zip", new AbortController().signal)).rejects.toThrow();
});
