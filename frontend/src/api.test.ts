import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  APIError,
  auditHistory,
  auditStatusForNode,
  requestJSON,
  revokeSession,
  takeFragmentSession,
} from "./api.js";

describe("browser authentication", () => {
  beforeEach(() => {
    history.replaceState(null, "", "/");
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("consumes the browser session without retaining it in web storage", () => {
    history.replaceState(null, "", "/#web_session=one%20time");
    expect(takeFragmentSession()).toBe("one time");
    expect(location.hash).toBe("");
    expect(sessionStorage.length).toBe(0);
    expect(takeFragmentSession()).toBe("");
  });

  it("sends only the read-only browser session header", async () => {
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
    const headers = new Headers(request?.headers);
    expect(headers.get("X-Docbank-Web-Session")).toBe("secret");
    expect(headers.get("X-Api-Key")).toBeNull();
  });

  it("revokes the session when the interface locks", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(null, { status: 204 }),
    );
    await revokeSession("short-lived");
    const [path, request] = fetchMock.mock.calls[0] ?? [];
    expect(path).toBe("/api/daemon/web-session");
    expect(request?.method).toBe("DELETE");
    expect(new Headers(request?.headers).get("X-Docbank-Web-Session")).toBe(
      "short-lived",
    );
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

  it("addresses audit status and cursor-stable history by node ID", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async () =>
      new Response(JSON.stringify({ enabled: true, scopes: [], items: [] }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
    await auditStatusForNode("session", 42);
    await auditHistory("session", 42, "cursor +/=");

    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/audit/status?node_id=42");
    expect(fetchMock.mock.calls[1]?.[0]).toBe(
      "/api/v1/audit/history?node_id=42&limit=50&cursor=cursor+%2B%2F%3D",
    );
  });
});
