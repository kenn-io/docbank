import { afterEach, expect, it, vi } from "vite-plus/test";
import { APIError, isCurrentSessionError, sessionResponse } from "./api-transport.js";

afterEach(() => vi.restoreAllMocks());

it("sends only the scoped browser session header and sets redirect behavior", async () => {
  const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(Response.json({ id: 1 }));
  const response = await sessionResponse("/api/v1/tags", { session: "secret" });
  await expect(response.json()).resolves.toEqual({ id: 1 });
  const request = fetchMock.mock.calls[0]?.[1];
  expect(request?.redirect).toBe("error");
  expect(request?.credentials).toBe("same-origin");
  const headers = new Headers(request?.headers);
  expect(headers.get("X-Docbank-Web-Session")).toBe("secret");
  expect(headers.get("X-Api-Key")).toBeNull();
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

it("preserves server field diagnostics verbatim", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    Response.json(
      {
        title: "Unprocessable Entity",
        status: 422,
        detail: "validation failed",
        code: "validation",
        errors: ["expected number <= 5 (body.patch.rating: 6)"],
      },
      { status: 422 },
    ),
  );
  const error = (await sessionResponse("/api/v1/photos/authored/edit", { session: "tab" }).catch(
    (cause: APIError) => cause,
  )) as APIError;
  expect(error.message).toBe("validation failed");
  expect(error).toMatchObject({
    status: 422,
    code: "validation",
    errors: ["expected number <= 5 (body.patch.rating: 6)"],
  });
});

it("preserves an untrusted problem position for the query editor", async () => {
  const position = { offset: 5, end: 8 };
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(
      JSON.stringify({
        status: 422,
        code: "invalid_query",
        detail: "expected an expression",
        position,
      }),
      { status: 422, headers: { "Content-Type": "application/problem+json" } },
    ),
  );

  await expect(
    sessionResponse("/api/v1/query/parse", { session: "session" }),
  ).rejects.toMatchObject({
    message: "expected an expression",
    status: 422,
    code: "invalid_query",
    position,
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
