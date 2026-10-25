import { afterEach, expect, it, vi } from "vite-plus/test";
import {
  APIError,
  isCurrentSessionError,
  mutationsInFlight,
  sessionJSON,
  sessionResponse,
} from "./api-transport.js";

afterEach(() => vi.restoreAllMocks());

it("sends the scoped browser session header and sets redirect behavior", async () => {
  const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(Response.json({ id: 1 }));
  const response = await sessionResponse("/api/v1/tags", { session: "secret" });
  await expect(response.json()).resolves.toEqual({ id: 1 });
  const request = fetchMock.mock.calls[0]?.[1];
  expect(request?.redirect).toBe("error");
  expect(request?.credentials).toBe("same-origin");
  const headers = new Headers(request?.headers);
  expect(headers.get("X-Docbank-Web-Session")).toBe("secret");
});

it("counts writes from the call until their response headers, and ignores reads", async () => {
  let answer!: (response: Response) => void;
  vi.spyOn(globalThis, "fetch").mockImplementation(
    () => new Promise<Response>((resolve) => (answer = resolve)),
  );
  for (const [url, method] of [
    ["/api/v1/photos/assets/query", "POST"],
    ["/api/v1/photos/assets/photo-1/preview", "POST"],
    ["/api/v1/queries/parse", "POST"],
    ["/api/v1/photos/events", "GET"],
  ] as const) {
    const reading = sessionResponse(url, { session: "tab", method });
    expect(mutationsInFlight()).toBe(0);
    answer(Response.json({}));
    await reading;
  }
  const writing = sessionResponse("/api/v1/photos/authored/edit?trace=1", {
    session: "tab",
    method: "POST",
  });
  expect(mutationsInFlight()).toBe(1);
  answer(Response.json({}, { status: 503 }));
  await expect(writing).rejects.toBeInstanceOf(APIError);
  expect(mutationsInFlight()).toBe(0);
  vi.mocked(globalThis.fetch).mockRejectedValueOnce(new TypeError("offline"));
  await expect(
    sessionResponse("/api/v1/photos/authored/edit", { session: "tab", method: "PUT" }),
  ).rejects.toThrow("offline");
  expect(mutationsInFlight()).toBe(0);
});

it("keeps a write counted until its answer body is read", async () => {
  let body!: ReadableStreamDefaultController<Uint8Array>;
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(new ReadableStream<Uint8Array>({ start: (controller) => (body = controller) }), {
      headers: { "Content-Type": "application/json" },
    }),
  );
  const writing = sessionJSON("/api/v1/photos/authored/edit", { session: "tab", method: "POST" });
  await new Promise((resolve) => setTimeout(resolve, 0));
  // The headers are here, but the receipt is not, so a pass must still wait.
  expect(mutationsInFlight()).toBe(1);
  body.enqueue(new TextEncoder().encode("{}"));
  body.close();
  await writing;
  expect(mutationsInFlight()).toBe(0);
});

it("binds authorization failures to the session that issued the request", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(JSON.stringify({ detail: "session expired" }), {
      status: 401,
      headers: { "Content-Type": "application/problem+json" },
    }),
  );

  const cause = await sessionResponse("/api/v1/tags", { session: "old-tab" }).catch(
    (error: unknown) => error,
  );

  expect(cause).toBeInstanceOf(APIError);
  expect(isCurrentSessionError(cause, "old-tab")).toBe(true);
  expect(isCurrentSessionError(cause, "new-tab")).toBe(false);
});

it("keeps manually-created API errors compatible with current-session handling", () => {
  expect(isCurrentSessionError(new APIError("expired", 401, "unauthorized"), "new-tab")).toBe(true);
});

it.each([
  {
    name: "field errors",
    problem: {
      code: "validation",
      detail: "validation failed",
      errors: ["expected number <= 5 (body.patch.rating: 6)"],
    },
    expected: {
      code: "validation",
      message: "validation failed",
      errors: ["expected number <= 5 (body.patch.rating: 6)"],
    },
  },
  {
    name: "untrusted query position",
    problem: {
      code: "invalid_query",
      detail: "expected an expression",
      position: { offset: 5, end: 8 },
    },
    expected: {
      code: "invalid_query",
      message: "expected an expression",
      position: { offset: 5, end: 8 },
    },
  },
])("preserves server diagnostics verbatim: $name", async ({ problem, expected }) => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    Response.json(
      {
        title: "Unprocessable Entity",
        status: 422,
        ...problem,
      },
      { status: 422 },
    ),
  );
  const error = (await sessionResponse("/api/v1/photos/authored/edit", { session: "tab" }).catch(
    (cause: APIError) => cause,
  )) as APIError;
  expect(error).toMatchObject({
    status: 422,
    ...expected,
  });
});

it.each([
  {
    problem: { detail: "ordinary failure", errors: [{ message: "missing location" }] },
    expected: "ordinary failure",
  },
  { problem: { title: "Unavailable", errors: {} }, expected: "Unavailable" },
])("falls back when diagnostics are unusable: $expected", async ({ problem, expected }) => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(Response.json(problem, { status: 503 }));
  const error = (await sessionResponse("/api/v1/photos", {}).catch(
    (cause: APIError) => cause,
  )) as APIError;
  expect(error.message).toBe(expected);
  expect(error.errors).toEqual([]);
});
