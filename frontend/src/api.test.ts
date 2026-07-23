import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  APIError,
  forgetKey,
  rememberKey,
  requestJSON,
  takeFragmentKey,
} from "./api.js";

describe("browser authentication", () => {
  beforeEach(() => {
    sessionStorage.clear();
    history.replaceState(null, "", "/");
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("consumes the fragment key without sending it in the visible URL", () => {
    history.replaceState(null, "", "/#api_key=one%20time");
    expect(takeFragmentKey()).toBe("one time");
    expect(location.hash).toBe("");
    expect(sessionStorage.getItem("docbank-api-key")).toBe("one time");
  });

  it("keeps the exact manually supplied key only for the browser session", () => {
    expect(rememberKey("  configured  ")).toBe("  configured  ");
    expect(takeFragmentKey()).toBe("  configured  ");
    forgetKey();
    expect(takeFragmentKey()).toBe("");
  });

  it("sends the key in the authenticated header", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ id: 1 }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
    await expect(requestJSON<{ id: number }>("/api/v1/path", "secret")).resolves.toEqual({
      id: 1,
    });
    const request = fetchMock.mock.calls[0]?.[1];
    expect(new Headers(request?.headers).get("X-Api-Key")).toBe("secret");
  });

  it("preserves structured daemon failures", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(
        JSON.stringify({
          status: 401,
          code: "unauthorized",
          detail: "missing or invalid API key",
        }),
        { status: 401, headers: { "Content-Type": "application/problem+json" } },
      ),
    );
    await expect(requestJSON("/api/v1/path", "bad")).rejects.toEqual(
      new APIError("missing or invalid API key", 401, "unauthorized"),
    );
  });
});
