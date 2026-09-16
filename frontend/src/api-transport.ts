export interface Problem {
  status?: number;
  code?: string;
  detail?: string;
  title?: string;
  position?: unknown;
}

export class APIError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code: string,
    readonly position?: unknown,
  ) {
    super(message);
    this.name = "APIError";
  }
}

export interface SessionOptions extends RequestInit {
  session?: string;
}

// Orval's transport hook adds the scoped credential and preserves problem details.
export async function sessionResponse<_T>(url: string, { session = "", ...init }: SessionOptions): Promise<Response> {
  const headers = new Headers(init.headers);
  if (!headers.has("Accept")) headers.set("Accept", "application/json");
  headers.set("X-Docbank-Web-Session", session);
  const response = await fetch(url, { ...init, headers, credentials: "same-origin" });
  if (!response.ok) {
    let problem: Problem = {};
    try { problem = await response.json() as Problem; } catch { /* An empty error still carries its HTTP status. */ }
    throw new APIError(problem.detail || problem.title || `HTTP ${response.status}`,
      response.status, problem.code ?? "", problem.position);
  }
  return response;
}

export async function sessionJSON<T>(url: string, options: SessionOptions): Promise<T> {
  const response = await sessionResponse<Response>(url, options);
  if (response.status === 204) return undefined as T;
  return response.json() as Promise<T>;
}
