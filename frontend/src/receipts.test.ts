import { describe, expect, it } from "vite-plus/test";
import type { DocumentSearchReport, DocumentSearchRequest } from "./generated/docbank.js";
import { validateDocumentSearchReport } from "./receipts.js";

const vault = "11111111-1111-4111-8111-111111111111";
const versions = ["22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"];
describe("processing receipts", () => {
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
  const valid = (): DocumentSearchReport => ({
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
  it("accepts scoped hits and coverage beyond the version-list bound", () => {
    const scoped = { ...request, fence: undefined, scope: { kind: "all_live" as const } };
    const report = valid();
    report.results[0]!.content_version_id = "44444444-4444-4444-8444-444444444444";
    report.results[0]!.vault_uid = "66666666-6666-4666-8666-666666666666";
    report.coverage.scoped_documents = 5000;
    report.coverage.complete_documents = 5000;
    expect(validateDocumentSearchReport(report, scoped).results).toHaveLength(1);
    report.results[0]!.vault_uid = "invalid";
    expect(() => validateDocumentSearchReport(report, scoped)).toThrow();
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

  it("accepts image evidence while rejecting rendition fields", () => {
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
  });

  it("accepts rendition-chunk embedding evidence with a build and time span", () => {
    const report = valid();
    const mediaIdentity = { build_id: "f".repeat(64), time_span: { start_ms: 1000, end_ms: 2000 } };
    Object.assign(report.results[0]!.evidence[0]!, mediaIdentity);
    expect(validateDocumentSearchReport(report, request)).toMatchObject({
      results: [{ evidence: [mediaIdentity] }],
    });
  });

  it("accepts lexical fallback for a hybrid request with an image-only profile", () => {
    const hybridRequest = { ...request, mode: "hybrid" as const, profile: "images" };
    const report = valid();
    report.requested_mode = "hybrid";
    report.actual_mode = "lexical";
    report.results[0]!.semantic_rank = 0;
    report.results[0]!.lexical_rank = 1;
    report.results[0]!.evidence = [{ kind: "node_name" }];
    expect(validateDocumentSearchReport(report, hybridRequest)).toMatchObject({
      requested_mode: "hybrid",
      actual_mode: "lexical",
      results: [{ lexical_rank: 1, path: "/visible.pdf" }],
    });
  });
});
