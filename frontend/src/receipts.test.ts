import { describe, expect, it } from "vite-plus/test";
import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex, utf8ToBytes } from "@noble/hashes/utils.js";
import type { DocumentSearchRequest } from "./generated/docbank.js";
import { validateDocumentSearchReport, validateDocumentSourceFenceResolution } from "./receipts.js";

const vault = "11111111-1111-4111-8111-111111111111";
const versions = ["22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"];
const crossLanguageFingerprint =
  "sha256:3c2a6756783fd03230bb89fe15de79ba10b3a6c1511d56be4042994e66d707cc";
const emptyFenceFingerprint =
  "sha256:460b958d02d96944be00a74a720c0b8af0248239d91c4351141b65d4b9551700";

function fenceFingerprint(vaultID: string, ids: string[]): string {
  const bytes = [...utf8ToBytes("docbank-document-source-fence/v1")];
  const add = (value: number) =>
    bytes.push((value >>> 24) & 255, (value >>> 16) & 255, (value >>> 8) & 255, value & 255);
  add(vaultID.length);
  bytes.push(...utf8ToBytes(vaultID));
  add(ids.length);
  for (const id of [...ids].sort()) {
    add(id.length);
    bytes.push(...utf8ToBytes(id));
  }
  return `sha256:${bytesToHex(sha256(new Uint8Array(bytes)))}`;
}

describe("processing receipts", () => {
  it("accepts sorted IDs and the empty fence", () => {
    expect(
      validateDocumentSourceFenceResolution({
        fence: { vault_uid: vault, content_version_ids: versions },
        observed_scope_count: 2,
        fence_fingerprint: crossLanguageFingerprint,
      }).fence.content_version_ids,
    ).toEqual(versions);
    expect(
      validateDocumentSourceFenceResolution({
        fence: { vault_uid: vault, content_version_ids: [] },
        observed_scope_count: 0,
        fence_fingerprint: emptyFenceFingerprint,
      }).fence.content_version_ids,
    ).toEqual([]);
  });

  it("rejects unsorted IDs and a fingerprint for another version set", () => {
    const valid = {
      fence: { vault_uid: vault, content_version_ids: versions },
      observed_scope_count: 2,
      fence_fingerprint: crossLanguageFingerprint,
    };
    expect(() =>
      validateDocumentSourceFenceResolution({
        ...valid,
        fence: { ...valid.fence, content_version_ids: [...versions].reverse() },
      }),
    ).toThrow(/The daemon returned an invalid source fence/);
    expect(() =>
      validateDocumentSourceFenceResolution({
        ...valid,
        fence_fingerprint: emptyFenceFingerprint,
      }),
    ).toThrow(/The daemon returned an invalid source fence/);
  });

  it("requires a valid reranking receipt only for opted-in requests", () => {
    const request: DocumentSearchRequest = {
      query: "synthetic description",
      mode: "lexical",
      limit: 20,
      profile: "private",
      fence: { vault_uid: vault, content_version_ids: [versions[0]] },
      rerank: true,
    };
    const report = {
      requested_mode: "lexical",
      actual_mode: "lexical",
      coverage: {
        binding_required: false,
        scoped_documents: 1,
        complete_documents: 1,
        state: "complete",
      },
      degradations: [],
      results: [
        {
          vault_uid: vault,
          node_id: 7,
          content_version_id: versions[0],
          rank: 1,
          score: 1,
          path: "/synthetic.txt",
          lexical_rank: 1,
          evidence: [{ kind: "node_name" }],
        },
      ],
      truncated: false,
      trace: [],
      reranking: { outcome: "applied", candidate_count: 1 },
    };
    expect(validateDocumentSearchReport(report, request).reranking?.outcome).toBe("applied");
    expect(() =>
      validateDocumentSearchReport({ ...report, reranking: undefined }, request),
    ).toThrow();
    expect(() => validateDocumentSearchReport(report, { ...request, rerank: false })).toThrow();
  });

  it("allows one selected result per transcript build of a content version", () => {
    const source = {
      source_id: "source",
      source_version_id: "version",
      content_version_id: versions[0],
    };
    const request: DocumentSearchRequest = {
      query: "synthetic phrase",
      mode: "lexical",
      limit: 20,
      profile: "private",
      fence: { vault_uid: vault, content_version_ids: [versions[0]] },
      media_sources: [source],
    };
    const result = (rank: number, buildID: string) => ({
      vault_uid: vault,
      node_id: 7,
      content_version_id: versions[0],
      rank,
      score: 0.5,
      path: "/recording.wav",
      lexical_rank: rank,
      evidence: [
        {
          kind: "rendition_segment",
          build_id: buildID,
          segment_id: "segment-1",
          media_sources: [source],
        },
      ],
    });
    const report = (secondBuild: string) => ({
      requested_mode: "lexical",
      actual_mode: "lexical",
      media_source_selection: true,
      media_selections: [],
      coverage: {
        binding_required: false,
        scoped_documents: 1,
        complete_documents: 1,
        state: "complete",
      },
      degradations: [],
      results: [result(1, "a".repeat(64)), result(2, secondBuild)],
      truncated: false,
      trace: [],
    });
    expect(validateDocumentSearchReport(report("b".repeat(64)), request).results).toHaveLength(2);
    expect(() => validateDocumentSearchReport(report("a".repeat(64)), request)).toThrow();
    const { media_sources: _, ...ordinary } = request;
    expect(() => validateDocumentSearchReport(report("b".repeat(64)), ordinary)).toThrow();
  });

  it("accepts the 4096-ID fence and rejects the 4097-ID boundary", () => {
    const ids = Array.from(
      { length: 4096 },
      (_, index) => `00000000-0000-4000-8000-${index.toString(16).padStart(12, "0")}`,
    );
    const valid = {
      fence: { vault_uid: vault, content_version_ids: ids },
      observed_scope_count: 4096,
      fence_fingerprint: "sha256:cbf7dadb0ffdefd1c5024235dfdfdfe835766bc44fa5bc6654ec561df8b11872",
    };
    expect(validateDocumentSourceFenceResolution(valid).fence.content_version_ids).toHaveLength(
      4096,
    );
    const oversizedIDs = [...ids, "00000000-0000-4000-8000-000000001000"];
    expect(() =>
      validateDocumentSourceFenceResolution({
        ...valid,
        fence: {
          ...valid.fence,
          content_version_ids: oversizedIDs,
        },
        observed_scope_count: 4097,
        fence_fingerprint: fenceFingerprint(vault, oversizedIDs),
      }),
    ).toThrow(/The daemon returned an invalid source fence/);
  });
});

describe("search result authority", () => {
  const vaultID = "11111111-1111-4111-8111-111111111111";
  const versionID = "22222222-2222-4222-8222-222222222222";
  const otherVersionID = "55555555-5555-4555-8555-555555555555";
  const request = {
    query: "visible",
    mode: "semantic" as const,
    limit: 20,
    profile: "private",
    fence: { vault_uid: vaultID, content_version_ids: [versionID, otherVersionID] },
  };
  const valid = () => ({
    requested_mode: "semantic",
    actual_mode: "semantic",
    coverage: {
      binding_required: false,
      scoped_documents: 1,
      complete_documents: 1,
      state: "complete",
    },
    degradations: [],
    truncated: false,
    trace: [],
    results: [
      {
        vault_uid: vaultID,
        node_id: 7,
        content_version_id: versionID,
        rank: 1,
        semantic_rank: 1,
        score: 0.9,
        path: "/visible.pdf",
        excerpt: "Synthetic match",
        evidence: [
          {
            kind: "embedding",
            build_id: "f".repeat(64),
            vector_space_id: "a".repeat(64),
            embedding_set_id: "b".repeat(64),
            input_generation_id: "c".repeat(64),
            input_id: "chunk-000000-aaaaaaaaaaaa",
            input_kind: "rendition_chunk",
            source_manifest_checksum: "d".repeat(64),
          },
        ],
      },
    ],
  });
  it.each([
    [
      "foreign vault",
      (report: ReturnType<typeof valid>) => {
        report.results[0]!.vault_uid = "33333333-3333-4333-8333-333333333333";
      },
    ],
    [
      "foreign version",
      (report: ReturnType<typeof valid>) => {
        report.results[0]!.content_version_id = "44444444-4444-4444-8444-444444444444";
      },
    ],
    [
      "duplicate result identity",
      (report: ReturnType<typeof valid>) => {
        report.results.push({ ...report.results[0]!, rank: 2, semantic_rank: 2 });
      },
    ],
    [
      "nonconsecutive rank",
      (report: ReturnType<typeof valid>) => {
        report.results[0]!.rank = 2;
      },
    ],
    [
      "duplicate semantic rank",
      (report: ReturnType<typeof valid>) => {
        report.results.push({
          ...report.results[0]!,
          node_id: 8,
          content_version_id: otherVersionID,
          rank: 2,
        });
      },
    ],
    [
      "duplicate evidence",
      (report: ReturnType<typeof valid>) => {
        report.results[0]!.evidence.push({ ...report.results[0]!.evidence[0]! });
      },
    ],
    [
      "missing input generation",
      (report: ReturnType<typeof valid>) => {
        report.results[0]!.evidence[0]!.input_generation_id = "";
      },
    ],
    [
      "relative path",
      (report: ReturnType<typeof valid>) => {
        report.results[0]!.path = "visible.pdf";
      },
    ],
    [
      "oversized excerpt",
      (report: ReturnType<typeof valid>) => {
        report.results[0]!.excerpt = "e".repeat(513);
      },
    ],
  ] as const)("rejects %s", (_name, mutate) => {
    const report = valid();
    mutate(report);
    expect(() => validateDocumentSearchReport(report, request)).toThrow(
      "The daemon returned an invalid search response.",
    );
  });

  it("accepts long absolute paths", () => {
    const report = valid();
    report.results[0]!.path = `/${"p".repeat(17000)}`;
    expect(validateDocumentSearchReport(report, request)).toMatchObject({
      results: [{ content_version_id: versionID }],
    });
  });

  it("accepts image evidence while rejecting rendition fields and incomplete identity", () => {
    const report = valid();
    const evidence = report.results[0]!.evidence[0]!;
    evidence.input_kind = "image";
    evidence.build_id = "";
    expect(validateDocumentSearchReport(report, request).results[0]!.evidence[0]!.input_kind).toBe(
      "image",
    );
    evidence.build_id = "f".repeat(64);
    expect(() => validateDocumentSearchReport(report, request)).toThrow(/invalid search response/);
    evidence.build_id = "";
    Object.assign(evidence, { time_span: { start_ms: 0, end_ms: 1000 } });
    expect(() => validateDocumentSearchReport(report, request)).toThrow(/invalid search response/);
    Reflect.deleteProperty(evidence, "time_span");
    evidence.input_generation_id = "";
    expect(() => validateDocumentSearchReport(report, request)).toThrow(/invalid search response/);
  });

  it("accepts rendition-chunk embedding evidence with a build and time span", () => {
    const report = valid();
    const mediaIdentity = { build_id: "f".repeat(64), time_span: { start_ms: 1000, end_ms: 2000 } };
    Object.assign(report.results[0]!.evidence[0]!, mediaIdentity);
    expect(validateDocumentSearchReport(report, request)).toMatchObject({
      results: [{ evidence: [mediaIdentity] }],
    });
  });
});
