export interface Problem {
  status?: number;
  code?: string;
  detail?: string;
  title?: string;
  position?: unknown;
  errors?: unknown;
}

export class APIError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code: string,
    readonly position?: unknown,
    readonly errors: string[] = [],
  ) {
    super(message);
    this.name = "APIError";
  }
}

const requestSessionByError = new WeakMap<APIError, string>();

// API failures from browser requests are tied to the tab credential that
// issued them, so a delayed response cannot revoke a newer sign-in.
export function isCurrentSessionError(cause: unknown, activeSession: string): boolean {
  if (!(cause instanceof APIError) || !requestSessionByError.has(cause)) return true;
  return requestSessionByError.get(cause) === activeSession;
}

export interface SessionOptions extends RequestInit {
  session?: string;
}

// Reads that use POST for their request body, so scrolling or typing a search never defers a pass.
// A missed entry only delays one refresh pass.
const readOnlyPosts =
  /^\/api\/v1\/(photos\/(assets\/(query|timeline|flags|[^/]+\/preview)|authored\/prepare|rejects\/preflight|stacks\/group|checkouts\/estimate)|queries\/parse)$/;

let mutations = 0;

/** Counts requests that may write, from the call until their response headers arrive. */
export const mutationsInFlight = () => mutations;

const mutates = (url: string, method = "GET") =>
  !["GET", "HEAD"].includes(method.toUpperCase()) &&
  !readOnlyPosts.test(url.replace(/^[a-z][a-z\d+.-]*:\/\/[^/]*/i, "").split(/[?#]/, 1)[0]!);

// Orval's transport hook adds the scoped credential and preserves problem details.
export async function sessionResponse<_T>(
  url: string,
  { session = "", ...init }: SessionOptions,
  allowNotModified = false,
): Promise<Response> {
  const headers = new Headers(init.headers);
  if (init.body instanceof Blob && init.body.type.startsWith("multipart/form-data;")) {
    headers.set("Content-Type", init.body.type);
  }
  if (!headers.has("Accept")) headers.set("Accept", "application/json");
  headers.set("X-Docbank-Web-Session", session);
  const writing = mutates(url, init.method);
  if (writing) mutations++;
  let response: Response;
  try {
    response = await fetch(url, {
      ...init,
      headers,
      credentials: "same-origin",
      redirect: "error",
    });
  } finally {
    if (writing) mutations--;
  }
  if (!response.ok && !(allowNotModified && response.status === 304)) {
    let problem: Problem = {};
    try {
      const body: unknown = await response.json();
      if (body && typeof body === "object") problem = body as Problem;
    } catch {
      /* An empty error still carries its HTTP status. */
    }
    const errors = Array.isArray(problem.errors)
      ? problem.errors.filter((entry): entry is string => typeof entry === "string")
      : [];
    const error = new APIError(
      problem.detail || problem.title || `HTTP ${response.status}`,
      response.status,
      problem.code ?? "",
      problem.position,
      errors,
    );
    requestSessionByError.set(error, session);
    throw error;
  }
  return response;
}

export async function sessionPhotoPreview<_T>(
  url: string,
  options: SessionOptions,
): Promise<Response> {
  return sessionResponse<Response>(url, options, true);
}

export async function sessionJSON<T>(url: string, options: SessionOptions): Promise<T> {
  const response = await sessionResponse<Response>(url, options);
  if (response.status === 204) return undefined as T;
  return response.json() as Promise<T>;
}

export async function sessionEmpty<_T>(url: string, options: SessionOptions): Promise<void> {
  await sessionResponse<Response>(url, options);
}
