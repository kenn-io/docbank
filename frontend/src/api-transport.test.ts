import { afterEach, expect, it, vi } from "vitest";
import { APIError, isCurrentSessionError, sessionResponse } from "./api-transport.js";

afterEach(() => vi.restoreAllMocks());

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
