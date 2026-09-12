import { createHash } from "node:crypto";
import { afterEach, expect, it, vi } from "vitest";
import { readVerifiedRenditionText, resolveRenditionText, type ReadyRenditionText, type RenditionObservation } from "./renditionText.js";
import type { SelectedSource } from "./selectedSource.js";

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

const source: SelectedSource = {
  kind: "snapshot", key: "snapshot:7:version", nodeID: 7, mutationRevision: 3,
  versionID: "11111111-1111-4111-8111-111111111111", blobHash: "a".repeat(64), size: 25,
  name: "evidence.pdf", path: "/evidence.pdf", mimeType: "application/pdf",
  modifiedAt: "2026-09-11T12:00:00Z", observedAt: "2026-09-11T12:00:00Z",
  originalTags: [], collectionIDs: [],
};
const observation: RenditionObservation = {
  configuration: "configured", profileFingerprint: "b".repeat(64),
  generationID: "c".repeat(64), coverageState: "complete",
  attachmentID: "d".repeat(64), buildID: "e".repeat(64),
};
const artifact = { id: `artifact_${"f".repeat(64)}`, sha256: "0".repeat(64), size: 0,
  media_type: "text/markdown; charset=utf-8" };

function receipt(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    $schema: "https://example.test/schemas/RenditionTextReceipt.json",
    state: "ready",
    source: { node_id: 7, revision: 9, version_id: source.versionID, blob_hash: source.blobHash,
      size: source.size, media_type: source.mimeType },
    profile: { name: "archive", configuration: "configured", fingerprint: observation.profileFingerprint },
    generation_id: observation.generationID, attachment_id: observation.attachmentID,
    build_id: observation.buildID, artifact, ...overrides,
  };
}

it("resolves an exact snapshot rendition and submits every observed authority", async () => {
  const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify(receipt()), {
    headers: { "Content-Type": "application/json" },
  }));
  const resolved = await resolveRenditionText("session", source, 9, "archive", observation,
    new AbortController().signal);
  expect(resolved.state).toBe("ready");
  const [, init] = fetchMock.mock.calls[0] ?? [];
  expect(JSON.parse(String(init?.body))).toEqual({
    node_id: 7, revision: 9, version_id: source.versionID, blob_hash: source.blobHash,
    size: source.size, profile: "archive", observed: {
      configuration: "configured", profile_fingerprint: observation.profileFingerprint,
      generation_id: observation.generationID, coverage_state: "complete",
      attachment_id: observation.attachmentID, build_id: observation.buildID,
    },
  });
});

it("accepts the worker's bounded digest-form artifact identity", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify(receipt({
    artifact: { ...artifact, id: "f".repeat(64) },
  })), { headers: { "Content-Type": "application/json" } }));
  await expect(resolveRenditionText("session", source, 9, "archive", observation,
    new AbortController().signal)).resolves.toMatchObject({ state: "ready", artifact: { id: "f".repeat(64) } });
});

it.each([
  ["source", receipt({ source: { ...receipt().source as object, blob_hash: "1".repeat(64) } })],
  ["generation", receipt({ generation_id: "1".repeat(64) })],
  ["artifact fields", receipt({ artifact: { ...artifact, extra: true } })],
  ["unknown receipt field", receipt({ extra: true })],
  ["invalid schema link", receipt({ $schema: "" })],
  ["unbounded artifact identity", receipt({ artifact: { ...artifact, id: "x".repeat(129) } })],
])("rejects a mismatched or widened %s receipt", async (_name, value) => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify(value), {
    headers: { "Content-Type": "application/json" },
  }));
  await expect(resolveRenditionText("session", source, 9, "archive", observation,
    new AbortController().signal)).rejects.toThrow(/Malformed rendition text receipt/);
});

it("verifies rendition headers, length, SHA-256 and UTF-8 before returning inert text", async () => {
  const bytes = new TextEncoder().encode("Verified <script>alert('blocked')</script>");
  const digest = createHash("sha256").update(bytes).digest("hex");
  const ready = await resolveReady(bytes.length, digest);
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(bytes, { headers: renditionHeaders(ready, digest, bytes) }));
  await expect(readVerifiedRenditionText("session", source, 9, ready,
    new AbortController().signal)).resolves.toBe("Verified <script>alert('blocked')</script>");
});

it("rejects a wrong body digest and wrong generation header", async () => {
  const expected = new TextEncoder().encode("expected text");
  const wrong = new TextEncoder().encode("different txt");
  const digest = createHash("sha256").update(expected).digest("hex");
  const ready = await resolveReady(expected.length, digest);
  const headers = renditionHeaders(ready, digest, wrong);
  headers.set("X-Docbank-Rendition-Generation", "9".repeat(64));
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(wrong, { headers }));
  await expect(readVerifiedRenditionText("session", source, 9, ready,
    new AbortController().signal)).rejects.toThrow(/disagreed with the selected rendition/);
});

async function resolveReady(size: number, digest: string) {
  vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(new Response(JSON.stringify(receipt({ artifact: {
    ...artifact, sha256: digest, size,
  } })), { headers: { "Content-Type": "application/json" } }));
  const resolved = await resolveRenditionText("session", source, 9, "archive", observation, new AbortController().signal);
  if (resolved.state !== "ready") throw new Error("ready fixture expected");
  return resolved;
}

function renditionHeaders(ready: ReadyRenditionText, digest: string, bytes: Uint8Array): Headers {
  return new Headers({
    "Content-Type": "text/markdown; charset=utf-8", "Content-Length": String(bytes.length),
    "Content-Digest": `sha-256=:${createHash("sha256").update(bytes).digest("base64")}:`,
    "X-Docbank-Rendition-SHA256": digest, "X-Docbank-Rendition-Size": String(bytes.length),
    "X-Docbank-Rendition-Profile": ready.profile.fingerprint,
    "X-Docbank-Rendition-Generation": ready.generationID,
    "X-Docbank-Rendition-Attachment": ready.attachmentID,
    "X-Docbank-Rendition-Build": ready.buildID,
    "X-Docbank-Rendition-Artifact": ready.artifact.id,
  });
}
