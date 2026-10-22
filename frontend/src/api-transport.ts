export interface Problem {
  status?: number;
  code?: string;
  detail?: string;
  title?: string;
  position?: unknown;
  errors?: unknown;
  face_ids?: unknown;
  face_version_ids?: unknown;
}

// A person merge conflict names each visible face with its exact version.
export type ProblemFace = { face_id: string; version: string };
const strings = (value: unknown) =>
  Array.isArray(value) ? value.filter((entry): entry is string => typeof entry === "string") : [];

export class APIError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code: string,
    readonly position?: unknown,
    readonly errors: string[] = [],
    readonly faces: ProblemFace[] = [],
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
  onResponseHeaders?: (headers: Headers) => void;
}

// Orval's transport hook adds the scoped credential and preserves problem details.
export async function sessionResponse<_T>(
  url: string,
  { session = "", onResponseHeaders, ...init }: SessionOptions,
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
    const errors = strings(problem.errors);
    const versions = strings(problem.face_version_ids);
    const faces = strings(problem.face_ids)
      .map((face_id, index) => ({ face_id, version: versions[index] ?? "" }))
      .filter((face) => face.version);
    const error = new APIError(
      problem.detail || problem.title || `HTTP ${response.status}`,
      response.status,
      problem.code ?? "",
      problem.position,
      errors,
      faces,
    );
    requestSessionByError.set(error, session);
    throw error;
  }
  onResponseHeaders?.(response.headers);
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
