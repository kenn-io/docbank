import { APIError, requestResponse } from "./api.js";
import { snapshotMemberHash, type SnapshotMember } from "./snapshots.js";

export const maxExportMembers = 100_000;
const maxRoleBytes = 50 * 2 ** 30;
const maxArchiveBytes = 52 * 2 ** 30;
const maxRoles = 300_000;
const limit = 64 * 1024;
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const digest = /^[0-9a-f]{64}$/;
const roles = ["original", "text", "pages", "email_pdf", "attachment_original", "attachment_pdf"];
const base = "/api/v1/exports";
export interface ExportMember { node_id: number; version_id: string; sha256: string; size: number }
export interface ExportSource { id: string; request_sha256: string; kind: string; state: string; member_hash: string; total: number; source_bytes: number; created_at: string; expires_at: string; collection_id?: string }
export interface RolePolicy { role: string; allow_unavailable?: boolean; profile_fingerprint?: string; recipe_sha256?: string }
export interface ExportOptions { duplicate_policy?: "preserve" | "collapse_exact_content"; volume_limits?: { roles: number; role_bytes: number } }
export interface OutputCounts { messages: number; attachments: number; email_pdfs: number; attachment_pdfs: number; pages: number; collapsed: number; unavailable: number; unavailable_inventories: number }
export interface ExportPlan extends ExportOptions { format: string; id: string; vault_id: string; toolchain: string; source: ExportSource; roles: RolePolicy[]; fingerprint: string; total: number; role_entries: number; role_bytes: number; metadata_bytes: number; created_at: string; expires_at: string; document_rows?: number; volumes?: number; counts?: OutputCounts }
export interface RoleSummary { role: string; available_members: number; unavailable_members: number; files: number; bytes: number; collapsed_files?: number; unavailable_reason?: string }
export interface ExportPreview { plan_id: string; fingerprint: string; member_hash: string; total: number; roles: RoleSummary[] }
export interface EmailPDFRecipeChoice { recipe_sha256: string; paper: string; renderer_version: string; messages: number; ambiguous: number }
export interface OutputProblem { node_id: number; version_id: string; part_path?: string; role: string; reason: string }
export interface OutputProblems { plan_id: string; fingerprint: string; after: number; next: number; total: number; items: OutputProblem[] }
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
  const out = { id: identity(r.id), request_sha256: hash(r.request_sha256), kind: string(r.kind), state: string(r.state), member_hash: hash(r.member_hash), total: integer(r.total, maxExportMembers, 1), source_bytes: integer(r.source_bytes, maxRoleBytes), created_at: date(r.created_at), expires_at: date(r.expires_at), ...(r.collection_id === undefined ? {} : { collection_id: identity(r.collection_id) }) };
  if (!["explicit", "upload", "mailbox_collection"].includes(out.kind) || !["uploading", "sealed"].includes(out.state) || (out.kind === "mailbox_collection") !== !!out.collection_id) fail();
  return out;
}

export function validateRolePolicies(value: unknown): RolePolicy[] {
  if (!Array.isArray(value) || value.length < 1 || value.length > 6) fail();
  const seen = new Set<string>();
  return value.map(raw => {
    const p = object(raw), role = string(p.role);
    if (!roles.includes(role) || seen.has(role) || Object.keys(p).some(k => !["role", "allow_unavailable", "profile_fingerprint", "recipe_sha256"].includes(k))) fail();
    seen.add(role);
    if (p.allow_unavailable !== undefined && typeof p.allow_unavailable !== "boolean") fail();
    const pdf = role === "email_pdf" || role === "attachment_pdf";
    if (p.profile_fingerprint !== undefined && role !== "text" && !pdf || p.recipe_sha256 !== undefined && role !== "pages" && !pdf || pdf && (p.profile_fingerprint === undefined) === (p.recipe_sha256 === undefined)) fail();
    return { role, ...(p.allow_unavailable === true ? { allow_unavailable: true } : {}), ...(p.profile_fingerprint === undefined ? {} : { profile_fingerprint: hash(p.profile_fingerprint) }), ...(p.recipe_sha256 === undefined ? {} : { recipe_sha256: hash(p.recipe_sha256) }) };
  });
}

export function validateExportOptions(value: ExportOptions): ExportOptions {
  const r = object(value), out: ExportOptions = {};
  if (Object.keys(r).some(k => !["duplicate_policy", "volume_limits"].includes(k))) fail();
  if (r.duplicate_policy !== undefined) {
    if (r.duplicate_policy !== "preserve" && r.duplicate_policy !== "collapse_exact_content") fail();
    out.duplicate_policy = r.duplicate_policy;
  }
  if (r.volume_limits !== undefined) {
    const v = object(r.volume_limits);
    if (Object.keys(v).some(k => !["roles", "role_bytes"].includes(k))) fail();
    out.volume_limits = { roles: integer(v.roles, 1000, 1), role_bytes: integer(v.role_bytes, 512 * 2 ** 20, 1) };
  }
  return out;
}

export function parseExportPlan(value: unknown, expected: ExportSource, policies: RolePolicy[], id: string, options: ExportOptions = {}): ExportPlan {
  const r = object(value);
  const p: ExportPlan = { format: string(r.format), id: identity(r.id), vault_id: string(r.vault_id), toolchain: string(r.toolchain), source: parseSource(r.source), roles: validateRolePolicies(r.roles), fingerprint: hash(r.fingerprint), total: integer(r.total, maxExportMembers, 1), role_entries: integer(r.role_entries, maxRoles), role_bytes: integer(r.role_bytes, maxRoleBytes), metadata_bytes: integer(r.metadata_bytes, 512 * 2 ** 20), created_at: date(r.created_at), expires_at: date(r.expires_at) };
  if (p.format !== "docbank-bundle-v1" || !p.vault_id || !p.toolchain || p.id !== id || JSON.stringify(p.source) !== JSON.stringify(parseSource(expected)) || p.source.state !== "sealed" || p.total !== expected.total || JSON.stringify(p.roles) !== JSON.stringify(validateRolePolicies(policies))) fail();
  const actualOptions = validateExportOptions({ ...(r.duplicate_policy === undefined ? {} : { duplicate_policy: r.duplicate_policy as ExportOptions["duplicate_policy"] }), ...(r.volume_limits === undefined ? {} : { volume_limits: r.volume_limits as ExportOptions["volume_limits"] }) });
  if (JSON.stringify(actualOptions) !== JSON.stringify(validateExportOptions(options))) fail();
  Object.assign(p, actualOptions);
  if (r.document_rows !== undefined) p.document_rows = integer(r.document_rows, maxExportMembers + maxRoles, p.total);
  if (r.volumes !== undefined) p.volumes = integer(r.volumes, p.role_entries);
  if ((p.volumes ?? 0) > 0 && !p.volume_limits || p.volume_limits && ((p.role_entries > 0) !== ((p.volumes ?? 0) > 0))) fail();
  if (r.counts !== undefined) {
    const c = object(r.counts);
    p.counts = { messages: integer(c.messages, p.total), attachments: integer(c.attachments, maxRoles), email_pdfs: integer(c.email_pdfs, p.role_entries), attachment_pdfs: integer(c.attachment_pdfs, p.role_entries), pages: integer(c.pages, maxRoles * 1000), collapsed: integer(c.collapsed, 2_400_000), unavailable: integer(c.unavailable, 2_400_000), unavailable_inventories: integer(c.unavailable_inventories, p.total) };
    if (p.counts.email_pdfs + p.counts.attachment_pdfs > p.role_entries || p.counts.attachments + p.total !== (p.document_rows ?? p.total) || p.counts.collapsed > 0 && p.duplicate_policy !== "collapse_exact_content" || p.counts.pages < p.counts.email_pdfs + p.counts.attachment_pdfs) fail();
  }
  if (exportExpired(p.expires_at)) throw new APIError("The export plan expired. Preview again.", 410, "export_expired");
  return p;
}

export function parseExportPreview(value: unknown, plan: ExportPlan): ExportPreview {
  const r = object(value);
  if (r.plan_id !== plan.id || r.fingerprint !== plan.fingerprint || r.member_hash !== plan.source.member_hash || r.total !== plan.total || !Array.isArray(r.roles) || r.roles.length !== plan.roles.length) fail();
  let files = 0, bytes = 0, collapsed = 0;
  const summaries = r.roles.map((raw, i) => {
    const s = object(raw), policy = plan.roles[i]!;
    const summary: RoleSummary = { role: string(s.role), available_members: integer(s.available_members, plan.total), unavailable_members: integer(s.unavailable_members, plan.total), files: integer(s.files, maxRoles), bytes: integer(s.bytes, maxRoleBytes), ...(s.collapsed_files === undefined ? {} : { collapsed_files: integer(s.collapsed_files, 2_400_000) }), ...(s.unavailable_reason === undefined ? {} : { unavailable_reason: string(s.unavailable_reason) }) };
    const attachment = summary.role === "attachment_original" || summary.role === "attachment_pdf", outputs = summary.files + (summary.collapsed_files ?? 0);
    if (summary.role !== policy.role || (attachment ? summary.available_members + summary.unavailable_members < plan.total : summary.available_members + summary.unavailable_members !== plan.total || outputs < summary.available_members || summary.role !== "pages" && outputs !== summary.available_members) || (summary.unavailable_members > 0 && (!policy.allow_unavailable || !summary.unavailable_reason)) || (summary.files === 0 && summary.bytes !== 0) || (summary.collapsed_files ?? 0) > 0 && plan.duplicate_policy !== "collapse_exact_content") fail();
    files += summary.files; bytes += summary.bytes; collapsed += summary.collapsed_files ?? 0;
    return summary;
  });
  if (files !== plan.role_entries || bytes !== plan.role_bytes || plan.counts && collapsed !== plan.counts.collapsed) fail();
  return { plan_id: plan.id, fingerprint: plan.fingerprint, member_hash: plan.source.member_hash, total: plan.total, roles: summaries };
}

function parseReceipt(value: unknown, plan: ExportPlan): ExportReceipt {
  const r = object(value);
  const receipt = { format: string(r.format), plan_fingerprint: hash(r.plan_fingerprint), sha256: hash(r.sha256), size: integer(r.size, maxArchiveBytes, 1), entries: integer(r.entries, maxRoles + 3, 3) };
  if (receipt.format !== plan.format || receipt.plan_fingerprint !== plan.fingerprint || receipt.entries !== (plan.volume_limits ? plan.volumes ?? 0 : plan.role_entries) + 3) fail();
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

export async function createExportPlan(session: string, source: ExportSource, roles: RolePolicy[], id: string, signal: AbortSignal, options: ExportOptions = {}): Promise<{ plan: ExportPlan; preview: ExportPreview }> {
  const checked = validateExportOptions(options);
  const plan = parseExportPlan(await retryExportRequest(() => request(session, "/plans", signal, { operation_id: id, source_id: source.id, member_hash: source.member_hash, roles, ...checked }), signal), source, roles, id, checked);
  const preview = parseExportPreview(await request(session, `/plans/${id}/preview`, signal), plan);
  return { plan, preview };
}

export async function sealMailboxExportSource(session: string, collectionID: string, total: number, operationID: string, signal: AbortSignal): Promise<ExportSource> {
  identity(collectionID); identity(operationID); integer(total, maxExportMembers, 1);
  const source = parseSource(await retryExportRequest(() => request(session, "/sources", signal, { operation_id: operationID, kind: "mailbox_collection", collection_id: collectionID }), signal));
  if (source.id !== operationID || source.kind !== "mailbox_collection" || source.collection_id !== collectionID || source.total !== total || source.state !== "sealed") fail();
  return source;
}

export async function exportEmailPDFRecipes(session: string, source: ExportSource, signal: AbortSignal): Promise<EmailPDFRecipeChoice[]> {
  const r = object(await request(session, `/sources/${identity(source.id)}/email-pdf-recipes`, signal));
  if (r.source_id !== source.id || r.member_hash !== source.member_hash || r.total !== source.total || !Array.isArray(r.recipes) || r.recipes.length > 50) fail();
  let previous = "";
  return r.recipes.map(raw => {
    const c = object(raw), choice = { recipe_sha256: hash(c.recipe_sha256), paper: string(c.paper), renderer_version: string(c.renderer_version, 128), messages: integer(c.messages, source.total), ambiguous: integer(c.ambiguous, source.total) };
    if (!["A4", "Letter"].includes(choice.paper) || !choice.renderer_version || choice.recipe_sha256 <= previous || choice.messages + choice.ambiguous > source.total) fail();
    previous = choice.recipe_sha256;
    return choice;
  });
}

export async function exportOutputProblems(session: string, plan: ExportPlan, after: number, signal: AbortSignal): Promise<OutputProblems> {
  integer(after, 2_600_000);
  const r = object(await request(session, `/plans/${identity(plan.id)}/problems?after=${after}`, signal));
  if (r.plan_id !== plan.id || r.fingerprint !== plan.fingerprint || r.after !== after || !Array.isArray(r.items) || r.items.length > 50) fail();
  const total = integer(r.total, 2_600_000, after), next = integer(r.next, total);
  const items = r.items.map(raw => {
    const p = object(raw), role = string(p.role), reason = string(p.reason, 512);
    if (!plan.roles.some(policy => policy.role === role && policy.allow_unavailable) || !reason) fail();
    return { node_id: integer(p.node_id, Number.MAX_SAFE_INTEGER, 1), version_id: identity(p.version_id), role, reason, ...(p.part_path === undefined ? {} : { part_path: string(p.part_path, 159) }) };
  });
  if (after + items.length > total || (after + items.length < total ? next !== after + items.length || !items.length : next !== 0)) fail();
  return { plan_id: plan.id, fingerprint: plan.fingerprint, after, next, total, items };
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
