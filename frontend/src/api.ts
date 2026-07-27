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

export interface ContentVersion {
  id: string;
  node_id: number;
  blob_hash: string;
  size: number;
  mime_type?: string;
  recorded_at: string;
  node_revision: number;
  introduced_operation_id: string;
  transition_kind:
    | "content_create"
    | "content_replace"
    | "content_revert";
  source_version_id?: string;
}

export interface ContentVersionPage {
  items: ContentVersion[];
  total: number;
  limit: number;
  offset: number;
}

export interface ProvenanceFact {
  identity: string;
  node_id: number;
  ingest_id: string;
  ingest_started_at: string;
  source_kind: string;
  source_description: string;
  original_path: string;
  original_mtime?: string;
  supersedes?: string;
  active: boolean;
}

export interface ProvenancePage {
  node: Node;
  items: ProvenanceFact[];
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
  tag_id?: string;
}

export interface Tag {
  id: string;
  name: string;
  revision: number;
  assignment_count: number;
}

export interface TagPage {
  items: Tag[];
  total: number;
  limit: number;
  offset: number;
}

export interface TaggedNode {
  node: Node;
  path?: string;
}

export interface TaggedNodePage {
  items: TaggedNode[];
  total: number;
  limit: number;
  offset: number;
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

export interface StorageStatus {
  loose_blobs: number;
  loose_bytes: number;
  packs: number;
  pack_stored_bytes: number;
  packed_blobs: number;
  packed_raw_bytes: number;
  packed_stored_bytes: number;
  dead_packed_bytes: number;
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

export async function requestResponse(
  path: string,
  session: string,
  init: RequestInit = {},
): Promise<Response> {
  const headers = new Headers(init.headers);
  if (!headers.has("Accept")) headers.set("Accept", "application/json");
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
  return response;
}

export async function requestJSON<T>(
  path: string,
  session: string,
  init: RequestInit = {},
): Promise<T> {
  const response = await requestResponse(path, session, init);
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

export async function contentVersions(
  session: string,
  nodeID: number,
): Promise<ContentVersionPage> {
  return requestJSON<ContentVersionPage>(
    `/api/v1/nodes/${nodeID}/versions?limit=1000&offset=0`,
    session,
  );
}

export async function provenance(
  session: string,
  nodeID: number,
): Promise<ProvenancePage> {
  return requestJSON<ProvenancePage>(
    `/api/v1/nodes/${nodeID}/provenance?limit=1000&offset=0`,
    session,
  );
}

export async function search(
  session: string,
  query: string,
  tagID = "",
): Promise<SearchReport> {
  const params = new URLSearchParams({ q: query, limit: "1000" });
  if (tagID) params.set("tag_id", tagID);
  return requestJSON<SearchReport>(
    `/api/v1/search?${params.toString()}`,
    session,
  );
}

export async function tags(session: string): Promise<TagPage> {
  return requestJSON<TagPage>("/api/v1/tags?limit=1000&offset=0", session);
}

export async function tagByID(session: string, tagID: string): Promise<Tag> {
  return requestJSON<Tag>(`/api/v1/tags/${encodeURIComponent(tagID)}`, session);
}

export async function taggedNodes(
  session: string,
  tagID: string,
): Promise<TaggedNodePage> {
  return requestJSON<TaggedNodePage>(
    `/api/v1/tags/${encodeURIComponent(tagID)}/nodes?limit=1000&offset=0`,
    session,
  );
}

export async function nodeTags(session: string, nodeID: number): Promise<TagPage> {
  return requestJSON<TagPage>(
    `/api/v1/nodes/${nodeID}/tags?limit=1000&offset=0`,
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

export async function storageStatus(session: string): Promise<StorageStatus> {
  return requestJSON<StorageStatus>("/api/v1/storage", session);
}
