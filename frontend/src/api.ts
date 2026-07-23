export interface Node {
  id: number;
  parent_id?: number;
  name: string;
  kind: "dir" | "file";
  current_version_id?: string;
  blob_hash?: string;
  size: number;
  mime_type?: string;
  revision: number;
  created_at: string;
  modified_at: string;
  trashed_at?: string;
  path?: string;
}

export interface NodePage {
  directory: Node;
  items: Node[];
  total: number;
  limit: number;
  offset: number;
}

export interface SearchHit {
  node: Node;
  path: string;
  match: "name" | "content";
}

export interface SearchReport {
  hits: SearchHit[];
  limit: number;
  truncated: boolean;
}

export interface AuditScopeStatus {
  id: string;
  target_node_id: number;
  target_path?: string;
  target_trashed: boolean;
  enable_operation_id: string;
  baseline_digest: string;
  member_count: number;
  entry_count: number;
  chain_head: string;
}

export interface AuditMembershipStatus {
  node_id: number;
  path?: string;
  trashed: boolean;
  protected: boolean;
  scope_ids: string[];
  baseline_digests: string[];
}

export interface AuditStatus {
  enabled: boolean;
  enabled_scope_id?: string;
  vault_id: string;
  lineage_id?: string;
  operation_sequence_high_water: number;
  allocation_entry_count: number;
  allocation_head?: string;
  scopes: AuditScopeStatus[];
  membership?: AuditMembershipStatus;
}

export interface AuditPathState {
  path: string;
  state: "live" | "trash";
}

export interface AuditAttachmentIdentity {
  tag_id?: string;
  node_id?: number;
  provenance_id?: string;
}

export interface AuditAttachmentState {
  tag_id?: string;
  node_id?: number;
  tag_name?: string;
  provenance_id?: string;
  ingest_id?: string;
  original_path?: string | null;
  original_mtime?: string | null;
  supersedes?: string | null;
}

export interface AuditAttachmentChange {
  kind: "tag_definition" | "tag_assignment" | "provenance";
  identity: AuditAttachmentIdentity;
  before?: AuditAttachmentState;
  after?: AuditAttachmentState;
}

export interface AuditEvent {
  id: string;
  operation_id: string;
  operation_sequence: number;
  ordinal: number;
  node_id: number;
  kind: string;
  scope_id: string;
  recorded_at: string;
  origin: string;
  agent_label?: string;
  prior_node_revision: number;
  resulting_node_revision: number;
  prior_current_version_id?: string;
  resulting_current_version_id?: string;
  source_version_id?: string;
  target_node_id?: number;
  baseline_digest?: string;
  attachment?: AuditAttachmentChange;
  old_path?: AuditPathState;
  new_path?: AuditPathState;
}

export interface AuditEventPage {
  node: Node;
  path?: string;
  items: AuditEvent[];
  total: number;
  limit: number;
  cursor?: string;
  next_cursor?: string;
}

export interface Job {
  name: string;
  status: "running" | "completed" | "failed" | "cancelled";
  started_at: string;
  finished_at?: string;
  error?: string;
}

export interface JobList {
  items: Job[];
}

export interface Problem {
  status?: number;
  code?: string;
  detail?: string;
  title?: string;
}

export class APIError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code: string,
  ) {
    super(message);
    this.name = "APIError";
  }
}

export function takeFragmentSession(
  location: Location = window.location,
  history: History = window.history,
): string {
  const params = new URLSearchParams(location.hash.replace(/^#/, ""));
  const session = params.get("web_session") ?? "";
  if (session) {
    history.replaceState(null, "", `${location.pathname}${location.search}`);
    return session;
  }
  return "";
}

async function decodeProblem(response: Response): Promise<Problem> {
  try {
    return (await response.json()) as Problem;
  } catch {
    return {};
  }
}

export async function requestJSON<T>(
  path: string,
  session: string,
  init: RequestInit = {},
): Promise<T> {
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/json");
  headers.set("X-Docbank-Web-Session", session);
  const response = await fetch(path, {
    ...init,
    headers,
    credentials: "same-origin",
  });
  if (!response.ok) {
    const problem = await decodeProblem(response);
    const detail = problem.detail || problem.title || `HTTP ${response.status}`;
    throw new APIError(detail, response.status, problem.code ?? "");
  }
  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

export async function revokeSession(session: string): Promise<void> {
  if (!session) return;
  await requestJSON<void>("/api/daemon/web-session", session, { method: "DELETE" });
}

export async function statPath(session: string, path: string): Promise<Node> {
  return requestJSON<Node>(`/api/v1/path?path=${encodeURIComponent(path)}`, session);
}

export async function children(session: string, nodeID: number): Promise<NodePage> {
  return requestJSON<NodePage>(
    `/api/v1/nodes/${nodeID}/children?limit=1000&offset=0`,
    session,
  );
}

export async function search(session: string, query: string): Promise<SearchReport> {
  return requestJSON<SearchReport>(
    `/api/v1/search?q=${encodeURIComponent(query)}&limit=1000`,
    session,
  );
}

export async function auditStatusForNode(
  session: string,
  nodeID: number,
): Promise<AuditStatus> {
  return requestJSON<AuditStatus>(
    `/api/v1/audit/status?node_id=${encodeURIComponent(nodeID)}`,
    session,
  );
}

export async function auditHistory(
  session: string,
  nodeID: number,
  cursor = "",
): Promise<AuditEventPage> {
  const query = new URLSearchParams({
    node_id: String(nodeID),
    limit: "50",
  });
  if (cursor) query.set("cursor", cursor);
  return requestJSON<AuditEventPage>(
    `/api/v1/audit/history?${query.toString()}`,
    session,
  );
}

export async function listJobs(session: string): Promise<Job[]> {
  const result = await requestJSON<JobList>("/api/v1/jobs", session);
  return result.items;
}
