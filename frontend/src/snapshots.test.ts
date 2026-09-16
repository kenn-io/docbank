// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { parseQuery, queryFingerprint, type Query } from "./query.js";
import {
  captureSnapshotTargets,
  createSnapshot,
  readSnapshotPage,
  snapshotMemberHash,
  type SnapshotPage,
  type SnapshotRow,
} from "./snapshots.js";

const version2 = "22222222-2222-4222-8222-222222222222";
const version10 = "10101010-1010-4010-8010-101010101010";
const snapshotID = "0123456789abcdef0123456789abcdef";
const blob2 = "2".repeat(64);
const blob10 = "a".repeat(64);
const tagID = "11111111-1111-4111-8111-111111111111";
const query = parseQuery("{}");

async function sha256OfUTF8(text: string): Promise<string> {
  const bytes = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text));
  return Array.from(new Uint8Array(bytes), (byte) => byte.toString(16).padStart(2, "0")).join("");
}

function row(nodeID: number, version: string, blob: string, size: number): SnapshotRow {
  return {
    node_id: nodeID,
    content_version_id: version,
    blob_hash: blob,
    size,
    revision: nodeID,
    name: `synthetic-${nodeID}.txt`,
    path: `/synthetic-${nodeID}.txt`,
    mime_type: "text/plain",
    media_family: "text",
    modified_at: "2026-09-11T00:00:00Z",
    sort_key: `synthetic-${nodeID}.txt`,
    tags: [{ id: tagID, name: "Synthetic", revision: 2 }],
    collection_ids: [tagID],
    display_collection_id: tagID,
    display_collection_label: null,
    coverage_state: "complete",
    coverage_build_id: "build-synthetic",
    coverage_attachment_id: "attachment-synthetic",
    excerpt: "Synthetic excerpt",
  };
}

async function page(overrides: Record<string, unknown> = {}): Promise<Record<string, unknown>> {
  const rows = (overrides.rows as SnapshotRow[] | undefined) ?? [row(2, version2, blob2, 20)];
  return {
    $schema: "http://localhost/schemas/WorkspaceQueryResponse.json",
    query,
    dependencies: [{ kind: "tag", id: tagID, revision: 2 }],
    query_fingerprint: await queryFingerprint(query),
    member_hash: await sha256OfUTF8("2:" + version2 + "\n"),
    snapshot_fingerprint: `sha256:${"f".repeat(64)}`,
    generation: { kind: "rendition", generation_id: "generation-synthetic" },
    coverage: { configuration: "configured", profile_fingerprint: "c".repeat(64) },
    observed_at: "2026-09-11T00:00:00Z",
    page_size: 50,
    total: rows.length,
    total_bytes: rows.reduce((total, item) => total + item.size, 0),
    rows,
    facets: [{
      dimension: "tags", available: true, values: [
        { key: tagID, label: "Synthetic", count: 1, selected: false },
      ], total: 1, missing: 0, other: 0,
    }],
    snapshot: true,
    snapshot_id: snapshotID,
    created_at: "2026-09-11T00:00:00Z",
    expires_at: "2026-09-11T00:15:00Z",
    ...overrides,
  };
}

function jsonResponse(value: unknown): Response {
  return new Response(JSON.stringify(value), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

afterEach(() => vi.restoreAllMocks());

describe("snapshot transport and receipt validation", () => {
  it("posts the complete canonical create request and maps the real wire shape", async () => {
    const controller = new AbortController();
    const receipt = await page();
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(jsonResponse(receipt));

    const { $schema: _schema, ...expected } = receipt;
    await expect(createSnapshot("session", query, {
      profile: "archive", page_size: 50, facets: ["tags"],
    }, controller.signal)).resolves.toEqual(expected);

    const [url, init] = fetchMock.mock.calls[0] ?? [];
    expect(url).toBe("/api/v1/workspace/queries");
    expect(init?.method).toBe("POST");
    expect(init?.signal).toBe(controller.signal);
    expect(new Headers(init?.headers).get("Content-Type")).toBe("application/json");
    expect(JSON.parse(String(init?.body))).toEqual({
      query, profile: "archive", page_size: 50, facets: ["tags"],
    });
  });

  it.each([
    ["an unknown envelope field", async () => ({ ...await page(), future: true })],
    ["a missing receipt field", async () => {
      const value = await page();
      delete value.member_hash;
      return value;
    }],
    ["an unsafe total", async () => page({ total: Number.MAX_SAFE_INTEGER + 1 })],
    ["an unsafe row identity", async () => page({
      rows: [row(Number.MAX_SAFE_INTEGER + 1, version2, blob2, 20)], total: 1, total_bytes: 20,
    })],
    ["an oversized serialized row", async () => page({
      rows: [{ ...row(2, version2, blob2, 20), excerpt: "x".repeat(65_536) }],
      total: 1, total_bytes: 20,
    })],
    ["invented unavailable counts", async () => page({
      facets: [{ dimension: "tags", available: false, reason: "time_budget_exceeded", values: [], total: 0 }],
    })],
    ["missing unavailable reason", async () => page({
      facets: [{ dimension: "tags", available: false, values: [] }],
    })],
    ["unknown nested row field", async () => page({
      rows: [{ ...row(2, version2, blob2, 20), future: true }], total: 1, total_bytes: 20,
    })],
  ])("rejects %s", async (_name, makeReceipt) => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(jsonResponse(await makeReceipt()));
    await expect(createSnapshot("session", query, {}, new AbortController().signal)).rejects.toThrow(
      /malformed snapshot receipt/i,
    );
  });

  it("rejects a receipt for a different query", async () => {
    const other = parseQuery('{"text":"different"}');
    vi.spyOn(globalThis, "fetch").mockResolvedValue(jsonResponse(await page({
      query: other, query_fingerprint: await queryFingerprint(other),
    })));
    await expect(createSnapshot("session", query, {}, new AbortController().signal)).rejects.toThrow(
      /does not match the request/i,
    );
  });

  it("bounds chunked successful response decoding to 32 MiB", async () => {
    const chunk = new Uint8Array(8 * 1024 * 1024);
    const body = new ReadableStream<Uint8Array>({
      start(stream) {
        for (let index = 0; index < 4; index++) stream.enqueue(chunk);
        stream.enqueue(new Uint8Array([1]));
        stream.close();
      },
    });
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(body, { status: 200 }));
    await expect(createSnapshot("session", query, {}, new AbortController().signal)).rejects.toThrow(
      /32 MiB/i,
    );
  });

  it("binds a page to the requested snapshot and cursor direction", async () => {
    const first = await page({ total: 2, total_bytes: 30, next_cursor: "next-1" }) as unknown as SnapshotPage;
    vi.spyOn(globalThis, "fetch").mockResolvedValue(jsonResponse(await page({
      total: 2, total_bytes: 30, rows: [row(10, version10, blob10, 10)],
      previous_cursor: "previous-1", expires_at: "2026-09-11T00:16:00Z",
    })));

    await expect(readSnapshotPage("session", first, "next-1", new AbortController().signal))
      .resolves.toMatchObject({ rows: [{ node_id: 10 }], previous_cursor: "previous-1" });
  });

  it.each([
    ["wrong snapshot ID", { snapshot_id: "fedcba9876543210fedcba9876543210", previous_cursor: "previous-1" }],
    ["wrong member identity", { member_hash: "0".repeat(64), previous_cursor: "previous-1" }],
    ["wrong snapshot identity", { snapshot_fingerprint: `sha256:${"0".repeat(64)}`, previous_cursor: "previous-1" }],
    ["missing reverse cursor", {}],
  ])("rejects a page with %s", async (_name, overrides) => {
    const first = await page({ total: 2, total_bytes: 30, next_cursor: "next-1" }) as unknown as SnapshotPage;
    vi.spyOn(globalThis, "fetch").mockResolvedValue(jsonResponse(await page({
      total: 2, total_bytes: 30, rows: [row(10, version10, blob10, 10)], ...overrides,
    })));
    await expect(readSnapshotPage("session", first, "next-1", new AbortController().signal)).rejects.toThrow();
  });

  it("requires the requested cursor to belong to the supplied page", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch");
    await expect(readSnapshotPage(
      "session", await page({ next_cursor: "next-1" }) as unknown as SnapshotPage,
      "unrelated", new AbortController().signal,
    )).rejects.toThrow(/cursor/i);
    expect(fetchMock).not.toHaveBeenCalled();
  });

});

describe("exact snapshot target capture", () => {
  const captureRows = Array.from({ length: 51 }, (_, index) => {
    const nodeID = index + 1;
    const suffix = String(nodeID).padStart(12, "0");
    return row(nodeID, `00000000-0000-4000-8000-${suffix}`, nodeID.toString(16).padStart(64, "0"), nodeID);
  });

  async function captureHash(): Promise<string> {
    return sha256OfUTF8(captureRows.map((item) => `${item.node_id}:${item.content_version_id}\n`).join(""));
  }

  async function twoPageFirst(overrides: Record<string, unknown> = {}): Promise<SnapshotPage> {
    return await page({
      rows: captureRows.slice(0, 50), total: 51, total_bytes: 1326,
      member_hash: await captureHash(),
      next_cursor: "next-1", ...overrides,
    }) as unknown as SnapshotPage;
  }

  async function second(overrides: Record<string, unknown> = {}): Promise<Record<string, unknown>> {
    return page({
      rows: captureRows.slice(50), total: 51, total_bytes: 1326,
      member_hash: await captureHash(),
      previous_cursor: "previous-1", ...overrides,
    });
  }

  it("hashes numeric node/version identities independent of input order", async () => {
    const expected = await sha256OfUTF8("2:v2\n10:v10\n");
    await expect(snapshotMemberHash([
      { node_id: 10, content_version_id: "v10" },
      { node_id: 2, content_version_id: "v2" },
    ])).resolves.toBe(expected);
  });

  it("copies and verifies every member before returning mutation-ready targets", async () => {
    const first = await twoPageFirst();
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(jsonResponse(await second()));
    const result = await captureSnapshotTargets("session", first, new AbortController().signal, { 1: { expectedRevision: 1, revision: 4 }, 51: { expectedRevision: 48, revision: 49 } });

    expect(result.snapshot).toBe(first);
    expect(result.members).toHaveLength(51);
    expect(result.members[0]).toEqual({
      node_id: 1, content_version_id: captureRows[0].content_version_id,
      blob_hash: captureRows[0].blob_hash, size: 1, revision: 4,
    });
    expect(result.members[50]).toEqual({
      node_id: 51, content_version_id: captureRows[50].content_version_id,
      blob_hash: captureRows[50].blob_hash, size: 51, revision: 51,
    });
    expect(first.rows[0].revision).toBe(1);
    expect(await snapshotMemberHash(result.members)).toBe(first.member_hash);
    expect(fetchMock.mock.calls.every(([url]) => String(url).includes("/workspace/queries/"))).toBe(true);
    expect(fetchMock.mock.calls.filter(([url]) => String(url).endsWith("/batch/tags"))).toHaveLength(0);
  });

  it.each([
    ["duplicate members", async () => second({ rows: [captureRows[0]] })],
    ["wrong count", async () => second({ rows: [], previous_cursor: "previous-1" })],
    ["wrong bytes", async () => second({ rows: [{ ...captureRows[50], size: 52 }] })],
    ["wrong member hash", async () => second({ member_hash: "0".repeat(64) })],
    ["premature next cursor", async () => second({ next_cursor: "next-2" })],
  ])("rejects %s without sending a mutation", async (_name, makeSecond) => {
    const first = await twoPageFirst();
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(jsonResponse(await makeSecond()));
    await expect(captureSnapshotTargets("session", first, new AbortController().signal)).rejects.toThrow();
    expect(fetchMock.mock.calls.filter(([url]) => String(url).endsWith("/batch/tags"))).toHaveLength(0);
  });

  it("rejects capture from a later page so earlier members cannot be omitted", async () => {
    const later = await second() as unknown as SnapshotPage;
    const fetchMock = vi.spyOn(globalThis, "fetch");
    await expect(captureSnapshotTargets("session", later, new AbortController().signal)).rejects.toThrow(
      /first page/i,
    );
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("refuses populations above 250000 before paging", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch");
    await expect(captureSnapshotTargets(
      "session", await page({ total: 250_001 }) as unknown as SnapshotPage,
      new AbortController().signal,
    )).rejects.toThrow(/250,000/i);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
