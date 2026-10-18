import { queryFingerprint, type Query } from "./query.js";
import { snapshotMemberHash, type SnapshotRow } from "./snapshots.js";

export async function snapshotEnvelope<T extends SnapshotRow>(
  query: Query,
  rows: T[],
  overrides: Record<string, unknown> = {},
) {
  return {
    $schema: "http://localhost/schemas/WorkspaceQueryResponse.json",
    query,
    dependencies: [],
    query_fingerprint: await queryFingerprint(query),
    member_hash:
      "member_hash" in overrides ? overrides.member_hash : await snapshotMemberHash(rows),
    snapshot_fingerprint: `sha256:${"b".repeat(64)}`,
    generation: { kind: "native" },
    coverage: { configuration: "unconfigured" },
    observed_at: "2026-10-01T00:00:00Z",
    created_at: "2026-10-01T00:00:00Z",
    expires_at: "2026-10-01T00:20:00Z",
    snapshot: true,
    snapshot_id: "c".repeat(32),
    total: rows.length,
    total_bytes: rows.reduce((sum, row) => sum + row.size, 0),
    page_size: 250,
    facets: [],
    rows,
    ...overrides,
  };
}
