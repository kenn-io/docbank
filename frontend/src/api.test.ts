import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  APIError,
  auditHistory,
  auditStatusForNode,
  backupSnapshots,
  contentVersions,
  listJobs,
  liveTaggedNodes,
  nodeTags,
  requestJSON,
  revokeSession,
  search,
  storageStatus,
  tagByID,
  taggedNodes,
  tags,
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
    history.replaceState(
      null,
      "",
      "/#web_session=one%20time&web_upload_secret=proof",
    );
    expect(takeFragmentSession()).toEqual({
      token: "one time",
      uploadSecret: "proof",
    });
    expect(location.hash).toBe("");
    expect(sessionStorage.length).toBe(0);
    expect(takeFragmentSession()).toBeNull();
  });

  it("sends only the scoped browser session header", async () => {
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

  it("reads daemon-owned background jobs through the browser session", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ items: [] }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );

    await expect(listJobs("session")).resolves.toEqual([]);
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/jobs");
  });

  it("reads physical storage authority through the browser session", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ loose_blobs: 0, packs: 0 }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );

    await storageStatus("session");
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/storage");
  });

  it("reads configured backup snapshots through the browser session", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(
        JSON.stringify({ repository: { id: "repo", path: "/backups" }, items: [] }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );

    await backupSnapshots("session");
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/backup/snapshots");
  });

  it("reads immutable versions for one stable node", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ items: [], total: 0, limit: 1000, offset: 0 }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );

    await contentVersions("session", 42);
    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/api/v1/nodes/42/versions?limit=1000&offset=0",
    );
  });

  it("addresses tag authority and tag-filtered search", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async () =>
      new Response(
        JSON.stringify({ items: [], total: 0, limit: 1000, offset: 0, hits: [] }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );

    await tags("session");
    await tagByID("session", "11111111-1111-4111-8111-111111111111");
    await taggedNodes("session", "11111111-1111-4111-8111-111111111111");
    await liveTaggedNodes("session", "11111111-1111-4111-8111-111111111111");
    await nodeTags("session", 42);
    await search("session", "quarterly report", "11111111-1111-4111-8111-111111111111");

    expect(fetchMock.mock.calls.map((call) => call[0])).toEqual([
      "/api/v1/tags?limit=1000&offset=0",
      "/api/v1/tags/11111111-1111-4111-8111-111111111111",
      "/api/v1/tags/11111111-1111-4111-8111-111111111111/nodes?limit=1000&offset=0",
      "/api/v1/tags/11111111-1111-4111-8111-111111111111/nodes?limit=1000&offset=0&live_only=true",
      "/api/v1/nodes/42/tags?limit=1000&offset=0",
      "/api/v1/search?q=quarterly+report&limit=1000&tag_id=11111111-1111-4111-8111-111111111111",
    ]);
  });
});
