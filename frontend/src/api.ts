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

const keyStorageName = "docbank-api-key";

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

export function takeFragmentKey(
  location: Location = window.location,
  history: History = window.history,
): string {
  const params = new URLSearchParams(location.hash.replace(/^#/, ""));
  const fragmentKey = params.get("api_key") ?? "";
  if (fragmentKey) {
    sessionStorage.setItem(keyStorageName, fragmentKey);
    history.replaceState(null, "", `${location.pathname}${location.search}`);
    return fragmentKey;
  }
  return sessionStorage.getItem(keyStorageName) ?? "";
}

export function rememberKey(key: string): string {
  if (key) {
    sessionStorage.setItem(keyStorageName, key);
  } else {
    sessionStorage.removeItem(keyStorageName);
  }
  return key;
}

export function forgetKey(): void {
  sessionStorage.removeItem(keyStorageName);
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
  key: string,
  init: RequestInit = {},
): Promise<T> {
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/json");
  headers.set("X-Api-Key", key);
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
  return (await response.json()) as T;
}

export async function statPath(key: string, path: string): Promise<Node> {
  return requestJSON<Node>(`/api/v1/path?path=${encodeURIComponent(path)}`, key);
}

export async function children(key: string, nodeID: number): Promise<NodePage> {
  return requestJSON<NodePage>(
    `/api/v1/nodes/${nodeID}/children?limit=1000&offset=0`,
    key,
  );
}

export async function search(key: string, query: string): Promise<SearchReport> {
  return requestJSON<SearchReport>(
    `/api/v1/search?q=${encodeURIComponent(query)}&limit=1000`,
    key,
  );
}
