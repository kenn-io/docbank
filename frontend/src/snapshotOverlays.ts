import type { BatchTagReceipt } from "./batch-tags.js";
import type { SnapshotRow } from "./snapshots.js";

export type SnapshotReceiptOverlay = {
  expectedRevision: number;
  revision: number;
  assignments: Record<string, { assign: boolean; label: string; completedAt: string }>;
};

export type SnapshotReceiptOverlays = Record<number, SnapshotReceiptOverlay>;

export function applySnapshotReceiptOverlay(
  overlays: Readonly<SnapshotReceiptOverlays>,
  receipt: Readonly<BatchTagReceipt>,
  tagLabel: string,
): SnapshotReceiptOverlays {
  const next = { ...overlays };
  for (const node of receipt.nodes) {
    const current = next[node.node_id];
    if (current && node.revision < current.revision) continue;
    const priorAssignment = current?.assignments[receipt.tag_id];
    if (current && node.revision === current.revision && priorAssignment &&
        priorAssignment.completedAt > receipt.completed_at) continue;
    next[node.node_id] = {
      expectedRevision: current && (node.expected_revision === current.revision || node.revision === current.revision)
        ? current.expectedRevision : node.expected_revision,
      revision: Math.max(current?.revision ?? 0, node.revision),
      assignments: {
        ...(current?.assignments ?? {}),
        [receipt.tag_id]: {
          assign: receipt.assign,
          label: tagLabel,
          completedAt: receipt.completed_at,
        },
      },
    };
  }
  return next;
}

export function snapshotTargetRevision(
  row: Readonly<Pick<SnapshotRow, "node_id" | "revision">>,
  overlays: Readonly<Record<number, Pick<SnapshotReceiptOverlay, "revision" | "expectedRevision">>>,
): number {
  const overlay = overlays[row.node_id];
  return overlay && overlay.expectedRevision <= row.revision && row.revision <= overlay.revision
    ? overlay.revision : row.revision;
}

export function visibleSnapshotOverlay(
  row: Readonly<Pick<SnapshotRow, "node_id" | "revision">>,
  overlays: Readonly<SnapshotReceiptOverlays>,
): SnapshotReceiptOverlay | undefined {
  const overlay = overlays[row.node_id];
  return overlay && overlay.revision >= row.revision ? overlay : undefined;
}
