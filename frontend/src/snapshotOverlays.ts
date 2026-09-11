import type { BatchTagReceipt } from "./batch-tags.js";
import type { SnapshotRow } from "./snapshots.js";

export type SnapshotReceiptOverlay = {
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

export function visibleSnapshotOverlay(
  row: Readonly<Pick<SnapshotRow, "node_id" | "revision">>,
  overlays: Readonly<SnapshotReceiptOverlays>,
): SnapshotReceiptOverlay | undefined {
  const overlay = overlays[row.node_id];
  return overlay && overlay.revision >= row.revision ? overlay : undefined;
}
