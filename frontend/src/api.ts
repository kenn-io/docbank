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

export interface TrashPage {
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

export interface TagAssignmentReceipt {
  tag: Tag;
  node: Node;
  changed: boolean;
}

export interface TagDeletionReceipt {
  tag: Tag;
  removed_assignments: number;
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
  omitted_trashed?: number;
}

export interface BackupRepository {
  id: string;
  path: string;
}

export interface BackupSnapshot {
  id: string;
  parent_id?: string;
  created_at: string;
  tag?: string;
  metadata_format: string;
  nodes: number;
  files: number;
  blobs: number;
  blob_bytes: number;
  packs_added: number;
  bytes_added: number;
  duration_seconds: number;
}

export interface BackupSnapshotList {
  repository: BackupRepository;
  items: BackupSnapshot[];
}

export interface UploadReceipt {
  status: "added" | "skipped";
  node: Node;
  computed_hash: string;
  computed_size: number;
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

export interface BrowserSession {
  token: string;
  uploadSecret: string;
}

export function takeFragmentSession(
  location: Location = window.location,
  history: History = window.history,
): BrowserSession | null {
  const params = new URLSearchParams(location.hash.replace(/^#/, ""));
  const token = params.get("web_session") ?? "";
  const uploadSecret = params.get("web_upload_secret") ?? "";
  if (token || uploadSecret) {
    history.replaceState(null, "", `${location.pathname}${location.search}`);
  }
  return token && uploadSecret ? { token, uploadSecret } : null;
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

export async function createTag(session: string, name: string): Promise<Tag> {
  const normalized = name.normalize("NFC");
  const tag = await requestJSON<Tag>("/api/v1/tags", session, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name }),
  });
  if (
    !tag.id ||
    tag.name !== normalized ||
    tag.revision !== 1 ||
    tag.assignment_count !== 0
  ) {
    throw new Error("The daemon returned an invalid created-tag receipt.");
  }
  return tag;
}

export async function renameTag(
  session: string,
  tagID: string,
  revision: number,
  name: string,
): Promise<Tag> {
  const normalized = name.normalize("NFC");
  const tag = await requestJSON<Tag>(
    `/api/v1/tags/${encodeURIComponent(tagID)}`,
    session,
    {
      method: "PATCH",
      headers: {
        "Content-Type": "application/json",
        "If-Match": String(revision),
      },
      body: JSON.stringify({ name }),
    },
  );
  if (tag.id !== tagID || tag.name !== normalized || tag.revision < revision) {
    throw new Error("The daemon returned an invalid renamed-tag receipt.");
  }
  return tag;
}

export async function deleteTag(
  session: string,
  tagID: string,
  revision: number,
): Promise<TagDeletionReceipt> {
  const receipt = await requestJSON<TagDeletionReceipt>(
    `/api/v1/tags/${encodeURIComponent(tagID)}`,
    session,
    {
      method: "DELETE",
      headers: { "If-Match": String(revision) },
    },
  );
  if (
    receipt.tag.id !== tagID ||
    receipt.tag.revision !== revision ||
    receipt.removed_assignments !== receipt.tag.assignment_count
  ) {
    throw new Error("The daemon returned an invalid deleted-tag receipt.");
  }
  return receipt;
}

export async function taggedNodes(
  session: string,
  tagID: string,
  offset = 0,
): Promise<TaggedNodePage> {
  return requestJSON<TaggedNodePage>(
    `/api/v1/tags/${encodeURIComponent(tagID)}/nodes?limit=1000&offset=${offset}`,
    session,
  );
}

export async function liveTaggedNodes(
  session: string,
  tagID: string,
): Promise<TaggedNodePage> {
  return requestJSON<TaggedNodePage>(
    `/api/v1/tags/${encodeURIComponent(tagID)}/nodes?limit=1000&offset=0&live_only=true`,
    session,
  );
}

export async function nodeTags(session: string, nodeID: number): Promise<TagPage> {
  return requestJSON<TagPage>(
    `/api/v1/nodes/${nodeID}/tags?limit=1000&offset=0`,
    session,
  );
}

export async function changeNodeTag(
  session: string,
  nodeID: number,
  revision: number,
  tagID: string,
  assign: boolean,
): Promise<TagAssignmentReceipt> {
  const receipt = await requestJSON<TagAssignmentReceipt>(
    `/api/v1/nodes/${nodeID}/tags/${encodeURIComponent(tagID)}`,
    session,
    {
      method: assign ? "PUT" : "DELETE",
      headers: { "If-Match": String(revision) },
    },
  );
  if (
    receipt.node.id !== nodeID ||
    receipt.node.revision < revision ||
    receipt.node.trashed_at ||
    !receipt.node.path?.startsWith("/") ||
    receipt.tag.id !== tagID
  ) {
    throw new Error("The daemon returned an invalid tag-assignment receipt.");
  }
  return receipt;
}

export async function trashNode(
  session: string,
  nodeID: number,
  revision: number,
): Promise<Node> {
  const node = await requestJSON<Node>(
    `/api/v1/nodes/${nodeID}/trash`,
    session,
    {
      method: "POST",
      headers: { "If-Match": String(revision) },
    },
  );
  if (
    node.id !== nodeID ||
    node.revision <= revision ||
    !node.trashed_at ||
    !node.path?.startsWith("/")
  ) {
    throw new Error("The daemon returned an invalid trash receipt.");
  }
  return node;
}

export async function trashRoots(session: string): Promise<TrashPage> {
  return requestJSON<TrashPage>("/api/v1/trash?limit=1000&offset=0", session);
}

export async function restoreNode(
  session: string,
  nodeID: number,
  revision: number,
): Promise<Node> {
  const node = await requestJSON<Node>(
    `/api/v1/nodes/${nodeID}/restore`,
    session,
    {
      method: "POST",
      headers: { "If-Match": String(revision) },
    },
  );
  if (
    node.id !== nodeID ||
    node.revision <= revision ||
    node.trashed_at ||
    !node.path?.startsWith("/")
  ) {
    throw new Error("The daemon returned an invalid restore receipt.");
  }
  return node;
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

export async function backupSnapshots(
  session: string,
): Promise<BackupSnapshotList> {
  return requestJSON<BackupSnapshotList>("/api/v1/backup/snapshots", session);
}
