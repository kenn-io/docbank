import { afterEach, describe, expect, it, vi } from "vitest";
import { prepareCurrentDownload, prepareVersionDownload } from "./download.js";
import type { ContentVersion, Node } from "./api.js";

const node: Node = {
  id: 7,
  parent_id: 2,
  name: "quarterly-report.txt",
  kind: "file",
  current_version_id: "12345678-1234-4123-8123-123456789abc",
  blob_hash: "a".repeat(64),
  size: 25,
  mime_type: "text/plain",
  revision: 3,
  created_at: "2026-07-26T12:00:00Z",
  modified_at: "2026-07-26T12:00:00Z",
};

const retainedVersion: ContentVersion = {
  id: "abcdefab-cdef-4abc-8def-abcdefabcdef",
  node_id: node.id,
  blob_hash: "b".repeat(64),
  size: 12,
  mime_type: "text/plain",
  recorded_at: "2026-07-20T12:00:00Z",
  node_revision: 1,
  introduced_operation_id: "87654321-4321-4321-8321-cba987654321",
  transition_kind: "content_create",
};

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("prepareCurrentDownload", () => {
  it("accepts monotonic progress and exact terminal authority", async () => {
    const events = [
      JSON.stringify({ phase: "progress", total: 25 }),
      JSON.stringify({ phase: "progress", received: 12, total: 25 }),
      JSON.stringify({
        phase: "ready",
        received: 25,
        total: 25,
        url: "/api/daemon/web-download/file?ticket=one-use",
        name: node.name,
        version_id: node.current_version_id,
        blob_hash: node.blob_hash,
      }),
    ].join("\n");
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(events + "\n", {
          status: 200,
          headers: { "Content-Type": "application/x-ndjson" },
        }),
      ),
    );
    const progress: number[] = [];

    const prepared = await prepareCurrentDownload(
      "browser-session",
      node,
      new AbortController().signal,
      (value) => progress.push(value.received),
    );

    expect(prepared).toEqual({
      url: "/api/daemon/web-download/file?ticket=one-use",
      name: node.name,
      versionID: node.current_version_id,
      blobHash: node.blob_hash,
      size: node.size,
    });
    expect(progress).toEqual([0, 12, 25]);
    const [path, init] = vi.mocked(fetch).mock.calls[0];
    expect(path).toBe("/api/daemon/web-download");
    expect(init?.method).toBe("POST");
    expect(new Headers(init?.headers).get("X-Docbank-Web-Session")).toBe(
      "browser-session",
    );
  });

  it("rejects a ready event that names different content", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          `${JSON.stringify({
            phase: "ready",
            received: 25,
            total: 25,
            url: "/api/daemon/web-download/file?ticket=one-use",
            name: node.name,
            version_id: node.current_version_id,
            blob_hash: "b".repeat(64),
          })}\n`,
          { status: 200 },
        ),
      ),
    );

    await expect(
      prepareCurrentDownload(
        "browser-session",
        node,
        new AbortController().signal,
        () => undefined,
      ),
    ).rejects.toThrow("disagreed with the selected document");
  });

  it("binds a retained-version request and receipt to immutable authority", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          [
            JSON.stringify({ phase: "progress", total: retainedVersion.size }),
            JSON.stringify({
              phase: "ready",
              received: retainedVersion.size,
              total: retainedVersion.size,
              url: "/api/daemon/web-download/file?ticket=historical",
              name: node.name,
              version_id: retainedVersion.id,
              blob_hash: retainedVersion.blob_hash,
            }),
            "",
          ].join("\n"),
          { status: 200 },
        ),
      ),
    );

    const prepared = await prepareVersionDownload(
      "browser-session",
      node,
      retainedVersion,
      new AbortController().signal,
      () => undefined,
    );

    expect(prepared.versionID).toBe(retainedVersion.id);
    expect(prepared.blobHash).toBe(retainedVersion.blob_hash);
    const [, init] = vi.mocked(fetch).mock.calls[0];
    expect(JSON.parse(String(init?.body))).toEqual({
      node_id: node.id,
      revision: node.revision,
      version_id: retainedVersion.id,
      blob_hash: retainedVersion.blob_hash,
      size: retainedVersion.size,
    });
  });
});
