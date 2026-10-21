import type { ContentVersion, Node } from "./generated/docbank.js";

export const node: Node = {
  id: 42,
  name: "quarterly-report.txt",
  kind: "file",
  current_version_id: "33333333-3333-4333-8333-333333333333",
  blob_hash: "c".repeat(64),
  size: 384,
  mime_type: "text/plain; charset=utf-8",
  revision: 3,
  created_at: "2026-01-02T03:04:05Z",
  modified_at: "2026-07-24T12:00:00Z",
};

export function version(
  id: string,
  revision: number,
  transition: ContentVersion["transition_kind"],
  hash: string,
  sourceVersionID?: string,
): ContentVersion {
  return {
    id,
    node_id: node.id,
    blob_hash: hash.repeat(64),
    size: revision * 128,
    mime_type: "text/plain; charset=utf-8",
    recorded_at: new Date(Date.UTC(2026, 6, 21 + revision, 12)).toISOString(),
    node_revision: revision,
    introduced_operation_id: `${revision.toString(16).padStart(8, "0")}-1111-4111-8111-111111111111`,
    transition_kind: transition,
    source_version_id: sourceVersionID,
  };
}
