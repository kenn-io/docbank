import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { afterEach, expect, it, vi } from "vite-plus/test";
import {
  readVerifiedRenditionText,
  resolveRenditionText,
  type ReadyRenditionText,
  type RenditionObservation,
} from "./renditionText.js";
import type { SelectedSource } from "./selectedSource.js";

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

const source: SelectedSource = {
  kind: "snapshot",
  key: "snapshot:7:version",
  nodeID: 7,
  mutationRevision: 3,
  versionID: "11111111-1111-4111-8111-111111111111",
  blobHash: "a".repeat(64),
  size: 25,
  name: "evidence.txt",
  path: "/evidence.txt",
  mimeType: "text/plain; charset=utf-8",
  modifiedAt: "2026-09-11T12:00:00Z",
  observedAt: "2026-09-11T12:00:00Z",
  originalTags: [],
  collectionIDs: [],
};
const observation: RenditionObservation = {
  configuration: "configured",
  profileFingerprint: "b".repeat(64),
  generationID: "c".repeat(64),
  coverageState: "complete",
  attachmentID: "d".repeat(64),
  buildID: "e".repeat(64),
};
const artifact = {
  id: `artifact_${"f".repeat(64)}`,
  sha256: "0".repeat(64),
  size: 0,
  media_type: "text/markdown; charset=utf-8",
};

function receipt(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    $schema: "https://example.test/schemas/RenditionTextReceipt.json",
    state: "ready",
    source: {
      node_id: 7,
      revision: 9,
      version_id: source.versionID,
      blob_hash: source.blobHash,
      size: source.size,
      media_type: source.mimeType,
    },
    profile: {
      name: "archive",
      configuration: "configured",
      fingerprint: observation.profileFingerprint,
    },
    generation_id: observation.generationID,
    attachment_id: observation.attachmentID,
    build_id: observation.buildID,
    artifact,
    ...overrides,
  };
}

it("resolves an exact snapshot rendition and submits every observed authority", async () => {
  const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(JSON.stringify(receipt()), {
      headers: { "Content-Type": "application/json" },
    }),
  );
  const resolved = await resolveRenditionText(
    "session",
    source,
    9,
    "archive",
    observation,
    new AbortController().signal,
  );
  expect(resolved.state).toBe("ready");
  const [, init] = fetchMock.mock.calls[0] ?? [];
  expect(JSON.parse(String(init?.body))).toEqual({
    node_id: 7,
    revision: 9,
    version_id: source.versionID,
    blob_hash: source.blobHash,
    size: source.size,
    profile: "archive",
    observed: {
      configuration: "configured",
      profile_fingerprint: observation.profileFingerprint,
      generation_id: observation.generationID,
      coverage_state: "complete",
      attachment_id: observation.attachmentID,
      build_id: observation.buildID,
    },
  });
});

it("accepts the worker's bounded digest-form artifact identity", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(
      JSON.stringify(
        receipt({
          artifact: { ...artifact, id: "f".repeat(64) },
        }),
      ),
      { headers: { "Content-Type": "application/json" } },
    ),
  );
  await expect(
    resolveRenditionText(
      "session",
      source,
      9,
      "archive",
      observation,
      new AbortController().signal,
    ),
  ).resolves.toMatchObject({ state: "ready", artifact: { id: "f".repeat(64) } });
});

it("validates page fields", async () => {
  const fetchMock = vi.spyOn(globalThis, "fetch");
  const signal = new AbortController().signal;
  const resolve = (fields: Record<string, unknown>) => {
    fetchMock.mockResolvedValueOnce(new Response(JSON.stringify(receipt(fields))));
    return resolveRenditionText("session", source, 9, "archive", observation, signal);
  };
  for (const [fields, expected] of [
    [{ pages_without_text: [2, 3] }, { pagesWithoutText: [2, 3] }],
    [{ page_coverage: "unavailable" }, { pageCoverage: "unavailable" }],
    [
      { state: "unprocessed", artifact: undefined, pages_without_text: [2] },
      { state: "unprocessed", pagesWithoutText: [2] },
    ],
    [
      { state: "unprocessed", artifact: undefined, page_coverage: "unavailable" },
      { state: "unprocessed", pageCoverage: "unavailable" },
    ],
  ]) {
    await expect(resolve(fields)).resolves.toMatchObject(expected);
  }
  for (const fields of [{ pages_without_text: [0] }, { page_coverage: "invalid" }]) {
    await expect(resolve(fields)).rejects.toThrow(/Malformed rendition text receipt/);
  }
});

it.each([
  ["source", receipt({ source: { ...(receipt().source as object), blob_hash: "1".repeat(64) } })],
  ["generation", receipt({ generation_id: "1".repeat(64) })],
  ["artifact fields", receipt({ artifact: { ...artifact, extra: true } })],
  ["unknown receipt field", receipt({ extra: true })],
  ["invalid schema link", receipt({ $schema: "" })],
  ["unbounded artifact identity", receipt({ artifact: { ...artifact, id: "x".repeat(129) } })],
])("rejects a mismatched or widened %s receipt", async (_name, value) => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(JSON.stringify(value), {
      headers: { "Content-Type": "application/json" },
    }),
  );
  await expect(
    resolveRenditionText(
      "session",
      source,
      9,
      "archive",
      observation,
      new AbortController().signal,
    ),
  ).rejects.toThrow(/Malformed rendition text receipt/);
});

function renditionArtifact(markdown: string, completeness = "complete", image = false) {
  const entries = image
    ? [
        ["    ALPHA", "Visible text (best-effort transcription)"],
        ["next\\", "Literal description"],
        ["flames.", "Possible meaning (interpretation)"],
        ["\n\n  indented", "Uncertainty and missing context"],
      ].map(([text, title], index) => {
        const offset = markdown.indexOf(text);
        const prefix = markdown.slice(0, offset);
        return {
          key: `unit_${createHash("sha256").update(String(index)).digest("hex")}`,
          kind: "generic",
          title,
          line: prefix.split("\n").length,
          byte: Buffer.byteLength(prefix),
        };
      })
    : [{ key: "page:1", kind: "page", line: 1, byte: 0 }];
  const envelope = {
    docbank: {
      contract: "docbank-sanitized-markdown/v1",
      source: {
        sha256: source.blobHash,
        format: image ? "png" : "txt",
        media_type: image ? "image/png" : "text/plain",
      },
      rendition: {
        build_id: "e".repeat(64),
        rendition_request_fingerprint: "a".repeat(64),
        evidence_lexical_fingerprint: "b".repeat(64),
        normalized_evidence_contract: "normalized-evidence/v1",
        body_sha256: createHash("sha256").update(markdown).digest("hex"),
        completeness,
        truncated: false,
      },
      document: { unit_kind: image ? "generic" : "page", unit_count: image ? 4 : 1 },
      navigation: {
        offset_base: "body",
        complete: true,
        entries,
      },
    },
  };
  return new TextEncoder().encode(`---\n${JSON.stringify(envelope)}\n---\n${markdown}`);
}

async function readArtifact(
  bytes: Uint8Array<ArrayBuffer>,
  providerID?: string,
  selected = source,
): Promise<string> {
  const digest = createHash("sha256").update(bytes).digest("hex");
  const ready = await resolveReady(bytes.length, digest, selected);
  const headers = renditionHeaders(ready, digest, bytes);
  if (providerID !== undefined) headers.set("X-Docbank-Rendition-Provider-ID", providerID);
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(bytes, { headers }));
  return readVerifiedRenditionText("session", selected, 9, ready, new AbortController().signal);
}

it("returns verified inert rendition text", async () => {
  await expect(
    readArtifact(renditionArtifact("Verified <script>alert('blocked')</script>")),
  ).resolves.toBe("Verified <script>alert('blocked')</script>");
});

const escapedImageText =
  "\n\t  # Machine\\-generated image understanding\n\n" +
  "flames\\. C:\\\\dir \\a\n\t\tline \\(1\\)  \n\n";

it("shows the producer's image understanding text exactly as transcribed", async () => {
  const markdown = readFileSync(
    "../document/testdata/image-understanding-reader.golden.md",
    "utf8",
  );
  const expected =
    "# Machine-generated image understanding\n\n" +
    "This is a model interpretation of an image, not verified source testimony. Possible meaning is uncertain without conversation context and does not establish anyone's beliefs, endorsement, or intent.\n\n" +
    "Configured model: synthetic<&>" +
    "m".repeat(243) +
    "\n\n" +
    "## Visible text (best-effort transcription)\n\n" +
    "    ALPHA\n    BETA\n  literal \\# text\n```\ninside\n````\n\n\n\n\n---\n\n" +
    "## Literal description\n\nnext\\\nline\n~~~\ntext\t\n\n\n---\n\n" +
    "## Possible meaning (interpretation)\n\nflames. C:\\dir \\a\n\t\tline (1)  \n\n---\n\n" +
    "## Uncertainty and missing context\n\n\n\n  indented\t\nends in a backslash\\\nand two\\\\\n``````\n\\# not a heading \\* \\` \\\\\n";
  await expect(
    readArtifact(
      renditionArtifact(markdown, "degraded_provenance", true),
      "compatible-vision.understand-v1",
      { ...source, name: "evidence.png", path: "/evidence.png", mimeType: "image/png" },
    ),
  ).resolves.toBe(expected);
});

it("keeps text from another provider byte-for-byte", async () => {
  await expect(
    readArtifact(renditionArtifact(escapedImageText), "compatible-ocr.extract-v1"),
  ).resolves.toBe(escapedImageText);
});

it.each(["body digest", "generation header"])("rejects a wrong %s", async (problem) => {
  const expected = new TextEncoder().encode("expected text");
  const digest = createHash("sha256").update(expected).digest("hex");
  const ready = await resolveReady(expected.length, digest);
  const headers = renditionHeaders(ready, digest, expected);
  const bytes = problem === "body digest" ? new TextEncoder().encode("different txt") : expected;
  if (problem === "generation header")
    headers.set("X-Docbank-Rendition-Generation", "9".repeat(64));
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(bytes, { headers }));
  await expect(
    readVerifiedRenditionText("session", source, 9, ready, new AbortController().signal),
  ).rejects.toThrow(/disagreed with the selected rendition/);
});

async function resolveReady(size: number, digest: string, selected = source) {
  vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
    new Response(
      JSON.stringify(
        receipt({
          source: { ...(receipt().source as object), media_type: selected.mimeType },
          artifact: {
            ...artifact,
            sha256: digest,
            size,
          },
        }),
      ),
      { headers: { "Content-Type": "application/json" } },
    ),
  );
  const resolved = await resolveRenditionText(
    "session",
    selected,
    9,
    "archive",
    observation,
    new AbortController().signal,
  );
  if (resolved.state !== "ready") throw new Error("ready fixture expected");
  return resolved;
}

function renditionHeaders(ready: ReadyRenditionText, digest: string, bytes: Uint8Array): Headers {
  return new Headers({
    "Content-Type": "text/markdown; charset=utf-8",
    "Content-Length": String(bytes.length),
    "Content-Digest": `sha-256=:${createHash("sha256").update(bytes).digest("base64")}:`,
    "X-Docbank-Rendition-SHA256": digest,
    "X-Docbank-Rendition-Size": String(bytes.length),
    "X-Docbank-Rendition-Profile": ready.profile.fingerprint,
    "X-Docbank-Rendition-Generation": ready.generationID,
    "X-Docbank-Rendition-Attachment": ready.attachmentID,
    "X-Docbank-Rendition-Build": ready.buildID,
    "X-Docbank-Rendition-Artifact": ready.artifact.id,
  });
}
