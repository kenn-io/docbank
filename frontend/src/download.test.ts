import { createHash } from "node:crypto";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  cancelPreparedDownload,
  prepareCurrentDownload,
  prepareExactDownload,
  prepareVersionDownload,
  readVerifiedPreview,
} from "./download.js";
import type { ContentVersion, Node } from "./api.js";
import type { SelectedSource } from "./selectedSource.js";

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

describe("exact selected-source preview", () => {
  const content = new TextEncoder().encode("historical selected bytes\n");
  const hash = createHash("sha256").update(content).digest("hex");
  const digest = createHash("sha256").update(content).digest("base64");
  const source: SelectedSource = {
    kind: "snapshot",
    key: `snapshot:7:${retainedVersion.id}:${hash}:${content.length}`,
    nodeID: 7,
    mutationRevision: 3,
    versionID: retainedVersion.id,
    blobHash: hash,
    size: content.length,
    name: "historical.txt",
    path: "/records/historical.txt",
    mimeType: "text/plain; charset=utf-8",
    modifiedAt: "2026-07-20T12:00:00Z",
    observedAt: "2026-07-21T12:00:00Z",
    originalTags: [],
    collectionIDs: [],
  };

  function ready(ticket = "preview", name = source.name): Response {
    return new Response(
      `${JSON.stringify({
        phase: "ready",
        received: source.size,
        total: source.size,
        url: `/api/daemon/web-download/file?ticket=${ticket}`,
        name,
        version_id: source.versionID,
        blob_hash: source.blobHash,
      })}\n`,
      { status: 200 },
    );
  }

  function body(
    overrides: Record<string, string> = {},
    bytes = content,
  ): Response {
    return new Response(bytes, {
      status: 200,
      headers: {
        "Content-Type": source.mimeType,
        "Content-Length": String(bytes.length),
        "Content-Digest": `sha-256=:${digest}:`,
        "X-Docbank-Content-Version": source.versionID,
        "X-Docbank-Blob-Hash": source.blobHash,
        "X-Docbank-Blob-Size": String(source.size),
        ...overrides,
      },
    });
  }

  it("uses a live revision only as authorization for the pinned historical bytes", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(ready()));

    const prepared = await prepareExactDownload(
      "browser-session",
      source,
      11,
      "preview",
      new AbortController().signal,
      () => undefined,
    );

    expect(prepared.versionID).toBe(source.versionID);
    const [, init] = vi.mocked(fetch).mock.calls[0];
    expect(JSON.parse(String(init?.body))).toEqual({
      node_id: 7,
      revision: 11,
      version_id: source.versionID,
      blob_hash: source.blobHash,
      size: source.size,
      purpose: "preview",
    });
  });

  it("accepts the daemon's current safe filename without changing pinned bytes", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(ready("renamed", "renamed-live.txt")));

    const prepared = await prepareExactDownload(
      "browser-session", source, 11, "native", new AbortController().signal, () => undefined,
    );

    expect(prepared.name).toBe("renamed-live.txt");
    expect(prepared.versionID).toBe(source.versionID);
    expect(prepared.blobHash).toBe(source.blobHash);
  });

  it("cancels a ready ticket when cancellation or stream failure wins before return", async () => {
    const controller = new AbortController();
    vi.stubGlobal(
      "fetch",
      vi.fn()
        .mockResolvedValueOnce(ready("resolved-before-cancel"))
        .mockResolvedValueOnce(new Response(null, { status: 204 })),
    );

    await expect(prepareExactDownload(
      "browser-session", source, 11, "native", controller.signal,
      () => controller.abort(),
    )).rejects.toMatchObject({ name: "AbortError" });
    expect(vi.mocked(fetch).mock.calls[1]?.[0]).toBe(
      "/api/daemon/web-download?ticket=resolved-before-cancel",
    );

    const continued = new Response(
      `${await ready("resolved-before-failure").text()}${JSON.stringify({ phase: "unknown" })}\n`,
    );
    vi.stubGlobal(
      "fetch",
      vi.fn()
        .mockResolvedValueOnce(continued)
        .mockResolvedValueOnce(new Response(null, { status: 204 })),
    );
    await expect(prepareExactDownload(
      "browser-session", source, 11, "native", new AbortController().signal, () => undefined,
    )).rejects.toThrow("continued after publication");
    expect(vi.mocked(fetch).mock.calls[1]?.[0]).toBe(
      "/api/daemon/web-download?ticket=resolved-before-failure",
    );
  });

  it.each([
    [
      "version",
      { "X-Docbank-Content-Version": node.current_version_id ?? "" },
      content,
    ],
    [
      "declared size",
      { "X-Docbank-Blob-Size": String(source.size + 1) },
      content,
    ],
    [
      "received size",
      { "Content-Length": String(source.size - 1) },
      content.slice(0, -1),
    ],
    [
      "digest",
      {
        "Content-Digest": `sha-256=:${createHash("sha256").update("wrong").digest("base64")}:`,
      },
      content,
    ],
  ])(
    "rejects a wrong received %s before decoding",
    async (_case, headers, bytes) => {
      vi.stubGlobal(
        "fetch",
        vi
          .fn()
          .mockResolvedValueOnce(ready())
          .mockResolvedValueOnce(body(headers, bytes))
          .mockResolvedValueOnce(new Response(null, { status: 204 })),
      );

      await expect(
        readVerifiedPreview(
          "browser-session",
          source,
          11,
          new AbortController().signal,
          () => undefined,
        ),
      ).rejects.toThrow("disagreed with the selected document");
    },
  );

  it("verifies the complete digest before decoding text", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValueOnce(ready()).mockResolvedValueOnce(body()),
    );

    await expect(
      readVerifiedPreview(
        "browser-session",
        source,
        11,
        new AbortController().signal,
        () => undefined,
      ),
    ).resolves.toEqual({
      kind: "text",
      text: "historical selected bytes\n",
      mediaType: "text/plain",
    });
  });

  it("cancels a ready ticket when the selected source is aborted before body receipt", async () => {
    const controller = new AbortController();
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValueOnce(ready("cancel-me"))
        .mockImplementationOnce(
          async (_path, init) =>
            new Promise<Response>((_resolve, reject) => {
              init?.signal?.addEventListener("abort", () =>
                reject(new DOMException("cancelled", "AbortError")),
              );
              controller.abort();
            }),
        )
        .mockResolvedValueOnce(new Response(null, { status: 204 })),
    );

    await expect(
      readVerifiedPreview(
        "browser-session",
        source,
        11,
        controller.signal,
        () => undefined,
      ),
    ).rejects.toMatchObject({ name: "AbortError" });
    expect(vi.mocked(fetch).mock.calls[2]?.[0]).toBe(
      "/api/daemon/web-download?ticket=cancel-me",
    );
    expect(vi.mocked(fetch).mock.calls[2]?.[1]?.method).toBe("DELETE");
  });

  it("uses authenticated owner cancellation for a known ready ticket", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(new Response(null, { status: 204 })),
    );

    await cancelPreparedDownload("browser-session", {
      url: "/api/daemon/web-download/file?ticket=known",
      name: source.name,
      versionID: source.versionID,
      blobHash: source.blobHash,
      size: source.size,
    });

    const [path, init] = vi.mocked(fetch).mock.calls[0];
    expect(path).toBe("/api/daemon/web-download?ticket=known");
    expect(init?.method).toBe("DELETE");
    expect(new Headers(init?.headers).get("X-Docbank-Web-Session")).toBe(
      "browser-session",
    );
  });
});
