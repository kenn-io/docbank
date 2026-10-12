import { afterEach, expect, it, vi } from "vitest";
import {
  APIError,
  isCurrentSessionError,
  sessionResponse,
} from "./api-transport.js";

afterEach(() => vi.restoreAllMocks());

it("binds authorization failures to the session that issued the request", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(JSON.stringify({ detail: "session expired" }), {
      status: 401,
      headers: { "Content-Type": "application/problem+json" },
    }),
  );

  const cause = await sessionResponse("/api/v1/tags", { session: "old-tab" })
    .catch((error: unknown) => error);

  expect(cause).toBeInstanceOf(APIError);
  expect(isCurrentSessionError(cause, "old-tab")).toBe(true);
  expect(isCurrentSessionError(cause, "new-tab")).toBe(false);
});

it("keeps manually-created API errors compatible with current-session handling", () => {
  expect(isCurrentSessionError(new APIError("expired", 401, "unauthorized"), "new-tab"))
    .toBe(true);
});
