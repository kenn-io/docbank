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

/** Counts requests that may write, from the call until the caller holds the answer. */
export const mutationsInFlight = () => mutations;

function counted<T>(url: string, method: string | undefined, run: () => Promise<T>) {
  if (!mutates(url, method)) return run();
  mutations++;
  const request = run();
  const release = () => void mutations--;
  request.then(release, release);
  return request;
}

/** Keeps a write counted while its caller applies the answer, so no pass reads in between. */
export function holdMutations<T>(apply: () => Promise<T>) {
  mutations++;
  return apply().finally(() => void mutations--);
}

const mutates = (url: string, method = "GET") =>
  !["GET", "HEAD"].includes(method.toUpperCase()) &&
  !readOnlyPosts.test(url.replace(/^[a-z][a-z\d+.-]*:\/\/[^/]*/i, "").split(/[?#]/, 1)[0]!);

// Orval's transport hook adds the scoped credential and preserves problem details.
export const sessionResponse = <_T>(
  url: string,
  options: SessionOptions,
  allowNotModified = false,
): Promise<Response> => counted(url, options.method, () => send(url, options, allowNotModified));

async function send(
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
  const response = await fetch(url, {
    ...init,
    headers,
    credentials: "same-origin",
    redirect: "error",
  });
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

export function sessionJSON<T>(url: string, options: SessionOptions): Promise<T> {
  return counted(url, options.method, async () => {
    const response = await send(url, options, false);
    if (response.status === 204) return undefined as T;
    return (await response.json()) as T;
  });
}

export async function sessionEmpty<_T>(url: string, options: SessionOptions): Promise<void> {
  await sessionResponse<Response>(url, options);
}
