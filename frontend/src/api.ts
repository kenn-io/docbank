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
