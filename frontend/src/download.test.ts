import { afterEach, describe, expect, it, vi } from "vitest";
import { prepareCurrentDownload } from "./download.js";
import type { Node } from "./api.js";

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
});
