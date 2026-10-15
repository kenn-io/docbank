import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import {
  changeNodeTag,
  liveNodeTags,
  createTag,
  deleteTag,
  renameTag,
  restoreNode,
  trashNode,
} from "./receipts.js";
import { takeFragmentSession } from "./browser-session.js";

describe("browser authentication", () => {
  beforeEach(() => {
    history.replaceState(null, "", "/");
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("consumes the browser session without web storage and preserves history.state", () => {
    history.replaceState(
      { fixtureIndex: 4 },
      "",
      "/#web_session=one%20time&web_upload_secret=proof",
    );
    expect(takeFragmentSession()).toEqual({
      token: "one time",
      uploadSecret: "proof",
    });
    expect(location.hash).toBe("");
    expect(sessionStorage.length).toBe(0);
    expect(history.state).toEqual({ fixtureIndex: 4 });
    expect(takeFragmentSession()).toBeNull();
  });

  it("loads every live node tag under one before-and-after revision fence", async () => {
    const live = {
      id: 42,
      parent_id: 1,
      name: "report.txt",
      kind: "file" as const,
      current_version_id: "11111111-1111-4111-8111-111111111111",
      blob_hash: "a".repeat(64),
      size: 5,
      mime_type: "text/plain",
      revision: 8,
      path: "/report.txt",
      created_at: "2026-09-11T12:00:00Z",
      modified_at: "2026-09-11T12:00:00Z",
    };
    const makeTag = (index: number) => ({
      id: `00000000-0000-4000-8000-${String(index).padStart(12, "0")}`,
      name: `tag-${index}`,
      revision: 1,
      assignment_count: 1,
    });
    const requests: string[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      const path = String(input);
      requests.push(path);
      if (path === "/api/v1/nodes/42") {
        return new Response(JSON.stringify(live), {
          headers: { "Content-Type": "application/json" },
        });
      }
      const offset = path.endsWith("offset=1000") ? 1000 : 0;
      const items = Array.from({ length: offset ? 1 : 1000 }, (_, index) =>
        makeTag(offset + index),
      );
      return new Response(JSON.stringify({ items, total: 1001, limit: 1000, offset }), {
        headers: { "Content-Type": "application/json" },
      });
    });

    const result = await liveNodeTags("session", 42);

    expect(result.node.revision).toBe(8);
    expect(result.items).toHaveLength(1001);
    expect(new Set(result.items.map((tag) => tag.id)).size).toBe(1001);
    expect(requests).toEqual([
      "/api/v1/nodes/42",
      "/api/v1/nodes/42/tags?limit=1000&offset=0",
      "/api/v1/nodes/42/tags?limit=1000&offset=1000",
      "/api/v1/nodes/42",
    ]);
  });

  it.each(["revision", "duplicate", "duplicate across pages", "count"])(
    "rejects a live tag listing with inconsistent %s evidence",
    async (problem) => {
      const nodeResponse = (revision: number) =>
        new Response(
          JSON.stringify({
            id: 42,
            parent_id: 1,
            name: "report.txt",
            kind: "file",
            current_version_id: "11111111-1111-4111-8111-111111111111",
            blob_hash: "a".repeat(64),
            size: 5,
            mime_type: "text/plain",
            revision,
            path: "/report.txt",
            created_at: "2026-09-11T12:00:00Z",
            modified_at: "2026-09-11T12:00:00Z",
          }),
          { headers: { "Content-Type": "application/json" } },
        );
      let call = 0;
      vi.spyOn(globalThis, "fetch").mockImplementation(async () => {
        call += 1;
        if (call === 1) return nodeResponse(8);
        if (problem === "duplicate across pages" && (call === 2 || call === 3))
          return Response.json({
            items: Array.from({ length: call === 2 ? 1000 : 1 }, (_, index) => ({
              id: `00000000-0000-4000-8000-${String(index).padStart(12, "0")}`,
              name: `tag-${index}`,
              revision: 1,
              assignment_count: 1,
            })),
            total: 1001,
            limit: 1000,
            offset: call === 2 ? 0 : 1000,
          });
        if (call === 2)
          return new Response(
            JSON.stringify({
              items: Array.from({ length: problem === "duplicate" ? 2 : 1 }, () => ({
                id: "00000000-0000-4000-8000-000000000001",
                name: "one",
                revision: 1,
                assignment_count: 1,
              })),
              total: problem === "count" ? 2 : problem === "duplicate" ? 2 : 1,
              limit: 1000,
              offset: 0,
            }),
            { headers: { "Content-Type": "application/json" } },
          );
        return nodeResponse(problem === "revision" ? 9 : 8);
      });

      await expect(liveNodeTags("session", 42)).rejects.toThrow(
        problem === "revision" ? "changed while tags were loading" : "incomplete or inconsistent",
      );
    },
  );

  it("manages tag definitions under stable revision authority", async () => {
    const tagID = "11111111-1111-4111-8111-111111111111";
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (_path, request) => {
      const method = request?.method;
      if (method === "POST") {
        return new Response(
          JSON.stringify({
            id: tagID,
            name: "tax",
            revision: 1,
            assignment_count: 0,
          }),
          { status: 201, headers: { "Content-Type": "application/json" } },
        );
      }
      if (method === "PATCH") {
        return new Response(
          JSON.stringify({
            id: tagID,
            name: "tax filing",
            revision: 2,
            assignment_count: 3,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        );
      }
      return new Response(
        JSON.stringify({
          tag: {
            id: tagID,
            name: "tax filing",
            revision: 2,
            assignment_count: 3,
          },
          removed_assignments: 3,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    });

    await createTag("session", "tax");
    await renameTag("session", tagID, 1, "tax filing");
    await deleteTag("session", tagID, 2);

    expect(fetchMock.mock.calls.map((call) => call[0])).toEqual([
      "/api/v1/tags",
      `/api/v1/tags/${tagID}`,
      `/api/v1/tags/${tagID}`,
    ]);
    expect(fetchMock.mock.calls.map((call) => call[1]?.method)).toEqual([
      "POST",
      "PATCH",
      "DELETE",
    ]);
    expect(new Headers(fetchMock.mock.calls[1]?.[1]?.headers).get("If-Match")).toBe("1");
    expect(new Headers(fetchMock.mock.calls[2]?.[1]?.headers).get("If-Match")).toBe("2");
  });

  it.each([
    {
      operation: "trash",
      revision: 7,
      method: "POST",
      path: "/api/v1/nodes/42/trash",
      response: {
        id: 42,
        name: "report.txt",
        kind: "file",
        size: 12,
        revision: 8,
        created_at: "2026-07-28T12:00:00Z",
        modified_at: "2026-07-28T12:00:00Z",
        trashed_at: "2026-07-28T12:01:00Z",
        path: "/Reports/report.txt",
      },
    },
    {
      operation: "restore",
      revision: 8,
      method: "POST",
      path: "/api/v1/nodes/42/restore",
      response: {
        id: 42,
        parent_id: 2,
        name: "report.txt",
        kind: "file",
        size: 12,
        revision: 9,
        created_at: "2026-07-28T12:00:00Z",
        modified_at: "2026-07-28T12:02:00Z",
        path: "/Reports/report (2).txt",
      },
    },
    {
      operation: "tag assignment",
      revision: 7,
      method: "PUT",
      path: "/api/v1/nodes/42/tags/11111111-1111-4111-8111-111111111111",
      response: {
        tag: {
          id: "11111111-1111-4111-8111-111111111111",
          name: "reviewed",
          revision: 2,
          assignment_count: 4,
        },
        node: {
          id: 42,
          name: "report.txt",
          kind: "file",
          size: 12,
          revision: 8,
          created_at: "2026-07-28T12:00:00Z",
          modified_at: "2026-07-28T12:02:00Z",
          path: "/Reports/report.txt",
        },
        changed: true,
      },
    },
  ])(
    "applies $operation under the inspected node revision",
    async ({ operation, revision, method, path, response }) => {
      const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(Response.json(response));
      const result = await (operation === "trash"
        ? trashNode("session", 42, revision)
        : operation === "restore"
          ? restoreNode("session", 42, revision)
          : changeNodeTag("session", 42, revision, "11111111-1111-4111-8111-111111111111", true));
      if (operation === "trash") {
        expect(result).toMatchObject({ id: 42, revision: 8, path: "/Reports/report.txt" });
      } else {
        expect(result).toEqual(response);
      }
      const [requestPath, request] = fetchMock.mock.calls[0] ?? [];
      expect(requestPath).toBe(path);
      expect(request?.method).toBe(method);
      expect(new Headers(request?.headers).get("If-Match")).toBe(String(revision));
    },
  );
});
