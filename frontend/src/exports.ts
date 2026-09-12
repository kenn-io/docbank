import { APIError, requestResponse } from "./api.js";
import { snapshotMemberHash, type SnapshotMember } from "./snapshots.js";

export const maxExportMembers = 100_000;
const maxRoleBytes = 50 * 2 ** 30;
const maxArchiveBytes = 52 * 2 ** 30;
const maxRoles = 300_000;
const limit = 64 * 1024;
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const digest = /^[0-9a-f]{64}$/;
const roles = ["original", "text", "pages"];
const base = "/api/v1/exports";
export interface ExportMember { node_id: number; version_id: string; sha256: string; size: number }
export interface ExportSource { id: string; request_sha256: string; kind: string; state: string; member_hash: string; total: number; source_bytes: number; created_at: string; expires_at: string }
export interface RolePolicy { role: string; allow_unavailable?: boolean; profile_fingerprint?: string; recipe_sha256?: string }
export interface ExportPlan { format: string; id: string; vault_id: string; toolchain: string; source: ExportSource; roles: RolePolicy[]; fingerprint: string; total: number; role_entries: number; role_bytes: number; metadata_bytes: number; created_at: string; expires_at: string }
export interface RoleSummary { role: string; available_members: number; unavailable_members: number; files: number; bytes: number; unavailable_reason?: string }
export interface ExportPreview { plan_id: string; fingerprint: string; member_hash: string; total: number; roles: RoleSummary[] }
export interface ExportReceipt { format: string; plan_fingerprint: string; sha256: string; size: number; entries: number }
export interface ExportJob { id: string; plan_id: string; fingerprint: string; state: string; sequence: number; completed_roles: number; completed_bytes: number; attempt: number; failure?: string; created_at: string; deadline: string; expires_at: string; receipt?: ExportReceipt }
export interface ExportTicket { url: string; receipt: ExportReceipt }

function fail(): never { throw new Error("The export response disagreed with its captured authority."); }
function object(value: unknown): Record<string, unknown> { if (!value || typeof value !== "object" || Array.isArray(value)) fail(); return value as Record<string, unknown>; }
function string(value: unknown, max = 1024): string { if (typeof value !== "string" || value.length > max) fail(); return value; }
function integer(value: unknown, max: number, min = 0): number { if (typeof value !== "number" || !Number.isSafeInteger(value) || value < min || value > max) fail(); return value; }
function identity(value: unknown): string { const s = string(value); if (!uuid.test(s)) fail(); return s; }
function hash(value: unknown): string { const s = string(value); if (!digest.test(s)) fail(); return s; }
function date(value: unknown): string { const s = string(value, 40); if (!/^\d{4}-\d\d-\d\dT/.test(s) || !Number.isFinite(Date.parse(s))) fail(); return s; }
export function exportExpired(expiry: string): boolean { return Date.parse(expiry) <= Date.now(); }

export function copyExportMembers(members: readonly Pick<SnapshotMember, "node_id" | "content_version_id" | "blob_hash" | "size">[]): ExportMember[] {
  if (members.length < 1 || members.length > maxExportMembers) throw new Error("Choose between 1 and 100,000 exact documents.");
  const seen = new Set<string>();
  let bytes = 0;
  return members.map(m => {
    const member = { node_id: integer(m.node_id, Number.MAX_SAFE_INTEGER, 1), version_id: identity(m.content_version_id), sha256: hash(m.blob_hash), size: integer(m.size, maxRoleBytes) };
    const key = `${member.node_id}:${member.version_id}`;
    if (seen.has(key) || member.size > maxRoleBytes - bytes) fail();
    seen.add(key); bytes += member.size;
    return member;
  });
}

export function exportMemberHash(members: readonly ExportMember[]): Promise<string> {
  return snapshotMemberHash(members.map(m => ({ node_id: m.node_id, content_version_id: m.version_id })));
}

function parseSource(value: unknown): ExportSource {
  const r = object(value);
  const out = { id: identity(r.id), request_sha256: hash(r.request_sha256), kind: string(r.kind), state: string(r.state), member_hash: hash(r.member_hash), total: integer(r.total, maxExportMembers, 1), source_bytes: integer(r.source_bytes, maxRoleBytes), created_at: date(r.created_at), expires_at: date(r.expires_at) };
  if (!["explicit", "upload"].includes(out.kind) || !["uploading", "sealed"].includes(out.state)) fail();
  return out;
}

export function validateRolePolicies(value: unknown): RolePolicy[] {
  if (!Array.isArray(value) || value.length < 1 || value.length > 3) fail();
  const seen = new Set<string>();
  return value.map(raw => {
    const p = object(raw), role = string(p.role);
    if (!roles.includes(role) || seen.has(role) || Object.keys(p).some(k => !["role", "allow_unavailable", "profile_fingerprint", "recipe_sha256"].includes(k))) fail();
    seen.add(role);
    if (p.allow_unavailable !== undefined && typeof p.allow_unavailable !== "boolean") fail();
    if (p.profile_fingerprint !== undefined && role !== "text" || p.recipe_sha256 !== undefined && role !== "pages") fail();
    return { role, ...(p.allow_unavailable === true ? { allow_unavailable: true } : {}), ...(p.profile_fingerprint === undefined ? {} : { profile_fingerprint: hash(p.profile_fingerprint) }), ...(p.recipe_sha256 === undefined ? {} : { recipe_sha256: hash(p.recipe_sha256) }) };
  });
}

export function parseExportPlan(value: unknown, expected: ExportSource, policies: RolePolicy[], id: string): ExportPlan {
  const r = object(value);
  const p: ExportPlan = { format: string(r.format), id: identity(r.id), vault_id: string(r.vault_id), toolchain: string(r.toolchain), source: parseSource(r.source), roles: validateRolePolicies(r.roles), fingerprint: hash(r.fingerprint), total: integer(r.total, maxExportMembers, 1), role_entries: integer(r.role_entries, maxRoles), role_bytes: integer(r.role_bytes, maxRoleBytes), metadata_bytes: integer(r.metadata_bytes, 512 * 2 ** 20), created_at: date(r.created_at), expires_at: date(r.expires_at) };
  if (p.format !== "docbank-bundle-v1" || !p.vault_id || !p.toolchain || p.id !== id || JSON.stringify(p.source) !== JSON.stringify(parseSource(expected)) || p.source.state !== "sealed" || p.total !== expected.total || JSON.stringify(p.roles) !== JSON.stringify(validateRolePolicies(policies))) fail();
  if (exportExpired(p.expires_at)) throw new APIError("The export plan expired. Preview again.", 410, "export_expired");
  return p;
}

export function parseExportPreview(value: unknown, plan: ExportPlan): ExportPreview {
  const r = object(value);
  if (r.plan_id !== plan.id || r.fingerprint !== plan.fingerprint || r.member_hash !== plan.source.member_hash || r.total !== plan.total || !Array.isArray(r.roles) || r.roles.length !== plan.roles.length) fail();
  let files = 0, bytes = 0;
  const summaries = r.roles.map((raw, i) => {
    const s = object(raw), policy = plan.roles[i]!;
    const summary: RoleSummary = { role: string(s.role), available_members: integer(s.available_members, plan.total), unavailable_members: integer(s.unavailable_members, plan.total), files: integer(s.files, maxRoles), bytes: integer(s.bytes, maxRoleBytes), ...(s.unavailable_reason === undefined ? {} : { unavailable_reason: string(s.unavailable_reason) }) };
    if (summary.role !== policy.role || summary.available_members + summary.unavailable_members !== plan.total || summary.files < summary.available_members || (summary.role !== "pages" && summary.files !== summary.available_members) || (summary.unavailable_members > 0 && (!policy.allow_unavailable || !summary.unavailable_reason)) || (summary.files === 0 && summary.bytes !== 0)) fail();
    files += summary.files; bytes += summary.bytes;
    return summary;
  });
  if (files !== plan.role_entries || bytes !== plan.role_bytes) fail();
  return { plan_id: plan.id, fingerprint: plan.fingerprint, member_hash: plan.source.member_hash, total: plan.total, roles: summaries };
}

function parseReceipt(value: unknown, plan: ExportPlan): ExportReceipt {
  const r = object(value);
  const receipt = { format: string(r.format), plan_fingerprint: hash(r.plan_fingerprint), sha256: hash(r.sha256), size: integer(r.size, maxArchiveBytes, 1), entries: integer(r.entries, maxRoles + 3, 3) };
  if (receipt.format !== plan.format || receipt.plan_fingerprint !== plan.fingerprint || receipt.entries !== plan.role_entries + 3) fail();
  return receipt;
}

export function parseExportJob(value: unknown, plan: ExportPlan, id: string): ExportJob {
  const r = object(value);
  const job: ExportJob = { id: identity(r.id), plan_id: identity(r.plan_id), fingerprint: hash(r.fingerprint), state: string(r.state), sequence: integer(r.sequence, Number.MAX_SAFE_INTEGER, 1), completed_roles: integer(r.completed_roles, plan.role_entries), completed_bytes: integer(r.completed_bytes, plan.role_bytes), attempt: integer(r.attempt, Number.MAX_SAFE_INTEGER), created_at: date(r.created_at), deadline: date(r.deadline), expires_at: date(r.expires_at), ...(r.failure === undefined ? {} : { failure: string(r.failure, 8192) }), ...(r.receipt === undefined ? {} : { receipt: parseReceipt(r.receipt, plan) }) };
  if (job.id !== id || job.plan_id !== plan.id || job.fingerprint !== plan.fingerprint || !["queued", "running", "completed", "canceled", "failed"].includes(job.state)) fail();
  if (job.state === "completed" ? !job.receipt || job.completed_roles !== plan.role_entries || job.completed_bytes !== plan.role_bytes : job.receipt !== undefined) fail();
  return job;
}

async function boundedJSON(response: Response): Promise<unknown> {
  if (!response.body) fail();
  const reader = response.body.getReader(), decoder = new TextDecoder("utf-8", { fatal: true });
  let raw = "", bytes = 0;
  try {
    for (;;) {
      const next = await reader.read();
      if (next.done) break;
      bytes += next.value.byteLength;
      if (bytes > limit) fail();
      raw += decoder.decode(next.value, { stream: true });
    }
    raw += decoder.decode();
    return JSON.parse(raw) as unknown;
  } finally { await reader.cancel().catch(() => undefined); reader.releaseLock(); }
}

async function request(session: string, path: string, signal: AbortSignal, body?: unknown, method = body === undefined ? "GET" : "POST"): Promise<unknown> {
  const response = await requestResponse(base + path, session, { method, signal, ...(body === undefined ? {} : { headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) }) });
  return response.status === 204 ? undefined : boundedJSON(response);
}

// Only ambiguous transport/server failures are retried, with identical request
// identity. Validation/authentication/conflicts are never a new operation.
export async function retryExportRequest<T>(fn: () => Promise<T>, signal: AbortSignal): Promise<T> {
  try { return await fn(); } catch (error) {
    signal.throwIfAborted();
    if (!(error instanceof TypeError) && !(error instanceof APIError && error.status >= 500)) throw error;
    return fn();
  }
}

export async function sealExportSource(session: string, members: readonly ExportMember[], operationID: string, signal: AbortSignal): Promise<ExportSource> {
  identity(operationID);
  const copied = copyExportMembers(members.map(m => ({ node_id: m.node_id, content_version_id: m.version_id, blob_hash: m.sha256, size: m.size })));
  const memberHash = await exportMemberHash(copied);
  const kind = copied.length > 1000 ? "upload" : "explicit";
  const body = kind === "upload" ? { operation_id: operationID, kind, total: copied.length, member_hash: memberHash } : { operation_id: operationID, kind, members: copied };
  let source = parseSource(await retryExportRequest(() => request(session, "/sources", signal, body), signal));
  if (source.id !== operationID || source.kind !== kind || source.total !== copied.length || source.member_hash !== memberHash) fail();
  if (kind === "upload" && source.state !== "sealed") {
    for (let offset = 0; offset < copied.length; offset += 1000) {
      await retryExportRequest(() => request(session, `/sources/${operationID}/chunks/${offset / 1000}`, signal, { members: copied.slice(offset, offset + 1000) }, "PUT"), signal);
    }
    source = parseSource(await retryExportRequest(() => request(session, `/sources/${operationID}/seal`, signal, {}), signal));
  }
  if (source.id !== operationID || source.kind !== kind || source.state !== "sealed" || source.total !== copied.length || source.member_hash !== memberHash || source.source_bytes !== copied.reduce((sum, m) => sum + m.size, 0)) fail();
  return source;
}

export async function createExportPlan(session: string, source: ExportSource, roles: RolePolicy[], id: string, signal: AbortSignal): Promise<{ plan: ExportPlan; preview: ExportPreview }> {
  const plan = parseExportPlan(await retryExportRequest(() => request(session, "/plans", signal, { operation_id: id, source_id: source.id, member_hash: source.member_hash, roles }), signal), source, roles, id);
  const preview = parseExportPreview(await request(session, `/plans/${id}/preview`, signal), plan);
  return { plan, preview };
}

export async function startExportJob(session: string, plan: ExportPlan, id: string, signal: AbortSignal): Promise<ExportJob> {
  identity(id);
  return parseExportJob(await retryExportRequest(() => request(session, "/jobs", signal, { operation_id: id, plan_id: plan.id, fingerprint: plan.fingerprint }), signal), plan, id);
}
export async function getExportJob(session: string, plan: ExportPlan, id: string, signal: AbortSignal): Promise<ExportJob> {
  identity(id);
  return parseExportJob(await request(session, `/jobs/${id}`, signal), plan, id);
}
export async function cancelExportJob(session: string, id: string, signal: AbortSignal): Promise<void> { identity(id); await request(session, `/jobs/${id}/cancel`, signal, {}); }

export function assertExportAdvance(previous: ExportJob, next: ExportJob): void {
  if (next.sequence < previous.sequence || next.attempt < previous.attempt || (next.attempt === previous.attempt && (next.completed_roles < previous.completed_roles || next.completed_bytes < previous.completed_bytes)) || (next.sequence === previous.sequence && JSON.stringify(next) !== JSON.stringify(previous)) || (["completed", "canceled", "failed"].includes(previous.state) && JSON.stringify(next) !== JSON.stringify(previous))) fail();
}

export async function readExportEvents(session: string, plan: ExportPlan, id: string, previous: ExportJob | undefined, signal: AbortSignal, publish: (job: ExportJob, gap: boolean) => void): Promise<void> {
  identity(id);
  const after = previous?.sequence ?? 0;
  const response = await requestResponse(`${base}/jobs/${id}/events?after=${after}`, session, { signal, headers: { Accept: "application/x-ndjson" } });
  if (!response.body || response.headers.get("Content-Type")?.split(";", 1)[0] !== "application/x-ndjson") fail();
  const reader = response.body.getReader(), decoder = new TextDecoder("utf-8", { fatal: true });
  let buffered = "", lastLine: string | undefined, terminal = false;
  let finalSnapshot: { job: ExportJob; gap: boolean } | undefined;
  try {
    for (;;) {
      const next = await reader.read();
      if (next.done) { buffered += decoder.decode(); break; }
      // Decode incrementally so a single network chunk cannot allocate an
      // unbounded event buffer, while allowing several bounded lines per read.
      for (let offset = 0; offset < next.value.length; offset += 4096) {
        buffered += decoder.decode(next.value.subarray(offset, offset + 4096), { stream: true });
        let newline: number;
        while ((newline = buffered.indexOf("\n")) >= 0) {
          const line = buffered.slice(0, newline); buffered = buffered.slice(newline + 1);
          if (line.length > limit || !line) fail();
          const envelope = object(JSON.parse(line));
          if (envelope.delivery !== "current_state" || envelope.requested_after !== after) fail();
          const job = parseExportJob(envelope.job, plan, id);
          if (previous) {
            assertExportAdvance(previous, job);
            if (job.sequence === previous.sequence) {
              if (lastLine !== undefined && line !== lastLine) fail();
              lastLine = line; continue;
            }
          }
          if (terminal) fail();
          const gap = job.sequence > (previous?.sequence ?? 0) + 1;
          previous = job; lastLine = line;
          terminal = !["queued", "running"].includes(job.state);
          if (terminal) finalSnapshot = { job, gap };
          else publish(job, gap);
        }
        if (buffered.length > limit) fail();
      }
    }
    if (buffered.length) throw new Error("The export progress stream was truncated.");
    if (!terminal) throw new Error("The export progress stream closed. Reconnect to read the same job.");
    if (finalSnapshot) publish(finalSnapshot.job, finalSnapshot.gap);
  } finally { await reader.cancel().catch(() => undefined); reader.releaseLock(); }
}

export function safeExportBasename(name: string): string {
  const stem = name.split(".", 1)[0]!.replace(/[ .]+$/, "");
  if (new TextEncoder().encode(name).length > 180 || name.length <= 4 || !name.endsWith(".zip") || name.trim() !== name || /[\p{Cc}\/\\:<>"|?*]/u.test(name) || !stem || /^(CON|PRN|AUX|NUL|CONIN\$|CONOUT\$|COM[1-9¹²³]|LPT[1-9¹²³])$/i.test(stem)) throw new Error("Use a ZIP basename of at most 180 bytes without folders, control characters, or reserved names.");
  return name;
}

export async function exportTicket(session: string, plan: ExportPlan, job: ExportJob, basename: string, signal: AbortSignal): Promise<ExportTicket> {
  const checked = parseExportJob(job, plan, job.id);
  if (checked.state !== "completed" || !checked.receipt || exportExpired(checked.expires_at)) fail();
  const r = object(await request(session, `/jobs/${job.id}/download`, signal, { basename: safeExportBasename(basename) }));
  const receipt = parseReceipt(r.receipt, plan), url = string(r.url);
  if (JSON.stringify(receipt) !== JSON.stringify(checked.receipt) || !/^\/api\/daemon\/web-download\/file\?ticket=[A-Za-z0-9_-]{43}$/.test(url)) fail();
  return { url, receipt };
}

export function offerExportDownload(ticket: ExportTicket, basename: string): void {
  if (!/^\/api\/daemon\/web-download\/file\?ticket=[A-Za-z0-9_-]{43}$/.test(ticket.url)) fail();
  const link = document.createElement("a");
  link.href = ticket.url; link.download = safeExportBasename(basename); link.rel = "noreferrer"; link.hidden = true;
  document.body.append(link); link.click(); link.remove();
}
