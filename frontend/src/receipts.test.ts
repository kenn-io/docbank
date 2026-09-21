import { describe, expect, it } from "vitest";
import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex, utf8ToBytes } from "@noble/hashes/utils.js";
import type { DocumentSearchRequest } from "./generated/docbank.js";
import { validateDocumentSearchReport, validateDocumentSourceFenceResolution } from "./receipts.js";

const vault = "11111111-1111-4111-8111-111111111111";
const versions = [
  "22222222-2222-4222-8222-222222222222",
  "33333333-3333-4333-8333-333333333333",
];
const crossLanguageFingerprint = "sha256:3c2a6756783fd03230bb89fe15de79ba10b3a6c1511d56be4042994e66d707cc";
const emptyFenceFingerprint = "sha256:460b958d02d96944be00a74a720c0b8af0248239d91c4351141b65d4b9551700";

function fenceFingerprint(vaultID: string, ids: string[]): string {
  const bytes = [...utf8ToBytes("docbank-document-source-fence/v1")];
  const add = (value: number) => bytes.push((value >>> 24) & 255, (value >>> 16) & 255, (value >>> 8) & 255, value & 255);
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
    expect(validateDocumentSourceFenceResolution({
      fence: { vault_uid: vault, content_version_ids: versions },
      observed_scope_count: 2,
      fence_fingerprint: crossLanguageFingerprint,
    }).fence.content_version_ids).toEqual(versions);
    expect(validateDocumentSourceFenceResolution({
      fence: { vault_uid: vault, content_version_ids: [] },
      observed_scope_count: 0,
      fence_fingerprint: emptyFenceFingerprint,
    }).fence.content_version_ids).toEqual([]);
  });

  it("rejects unsorted IDs and a fingerprint for another version set", () => {
    const valid = {
      fence: { vault_uid: vault, content_version_ids: versions },
      observed_scope_count: 2,
      fence_fingerprint: crossLanguageFingerprint,
    };
    expect(() => validateDocumentSourceFenceResolution({
      ...valid,
      fence: { ...valid.fence, content_version_ids: [...versions].reverse() },
    })).toThrow();
    expect(() => validateDocumentSourceFenceResolution({
      ...valid,
      fence_fingerprint: emptyFenceFingerprint,
    })).toThrow();
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
      coverage: { binding_required: false, scoped_documents: 1, complete_documents: 1, state: "complete" },
      degradations: [],
      results: [{ vault_uid: vault, node_id: 7, content_version_id: versions[0], rank: 1, score: 1,
        path: "/synthetic.txt", lexical_rank: 1, evidence: [{ kind: "node_name" }] }],
      truncated: false,
      trace: [],
      reranking: { outcome: "applied", candidate_count: 1 },
    };
    expect(validateDocumentSearchReport(report, request).reranking?.outcome).toBe("applied");
    expect(() => validateDocumentSearchReport({ ...report, reranking: undefined }, request)).toThrow();
    expect(() => validateDocumentSearchReport(report, { ...request, rerank: false })).toThrow();
  });

  it("accepts the 4096-ID fence and rejects the 4097-ID boundary", () => {
    const ids = Array.from({ length: 4096 }, (_, index) =>
      `00000000-0000-4000-8000-${index.toString(16).padStart(12, "0")}`);
    const valid = {
      fence: { vault_uid: vault, content_version_ids: ids },
      observed_scope_count: 4096,
      fence_fingerprint: "sha256:cbf7dadb0ffdefd1c5024235dfdfdfe835766bc44fa5bc6654ec561df8b11872",
    };
    expect(validateDocumentSourceFenceResolution(valid).fence.content_version_ids).toHaveLength(4096);
    expect(() => validateDocumentSourceFenceResolution({
      ...valid,
      fence: { ...valid.fence, content_version_ids: [...ids, "00000000-0000-4000-8000-000000001000"] },
      observed_scope_count: 4097,
    })).toThrow();
  });
});
