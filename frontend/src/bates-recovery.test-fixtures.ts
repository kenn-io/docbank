import type { BatesCandidate, BatesExport, BatesNamespace } from "./generated/docbank.js";

export const recoveryNamespace: BatesNamespace = {
  namespace_id: "11111111-1111-4111-8111-111111111111",
  prefix: "QUAL",
  suffix: "",
  padding: 6,
  created_at: "2026-10-01T12:00:00Z",
};
export const recoveryCandidate: BatesCandidate = {
  artifact_id: "22222222-2222-4222-8222-222222222222",
  allocation_id: "33333333-3333-4333-8333-333333333333",
  snapshot_id: "44444444-4444-4444-8444-444444444444",
  blob_sha256: "a".repeat(64),
  manifest_sha256: "b".repeat(64),
  size: 12345,
  page_count: 2,
  media_type: "application/pdf",
  state: "verified",
  created_at: "2026-10-01T12:00:00Z",
  evidence_truncated: false,
  evidence: [{ kind: "label", label: "QUAL000002", output_page: 2, occurrence_id: "c".repeat(32) }],
};
export function recoveryReceipt(contract = "native-pdf-numbered/v1"): BatesExport {
  const pages = [1, 2].map((page) => ({
    ordinal: page,
    occurrence_id: "c".repeat(32),
    source_page: page,
    output_page: page,
    label: `QUAL00000${page}`,
    source_blob_sha256: "d".repeat(64),
  }));
  return {
    ...recoveryCandidate,
    recipe_sha256: "e".repeat(64),
    pages,
    recipe: { contract, renderer: { writer_version: "recorded-writer" } },
    namespace: recoveryNamespace,
    allocation: {
      allocation_id: recoveryCandidate.allocation_id,
      snapshot_id: recoveryCandidate.snapshot_id,
      namespace_id: recoveryNamespace.namespace_id,
      recipe_sha256: "e".repeat(64),
      request_sha256: "f".repeat(64),
      state: "committed",
      start_sequence: 1,
      end_sequence: 2,
      created_at: recoveryCandidate.created_at,
      labels: pages,
    },
  };
}
export const recoveryJSON = (body: unknown) =>
  new Response(JSON.stringify(body), {
    headers: { "Content-Type": "application/json" },
  });
