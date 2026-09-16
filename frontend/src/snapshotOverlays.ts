import type { BatchTagReceipt } from "./batch-tags.js";
import type { SnapshotRow } from "./snapshots.js";

export type SnapshotReceiptOverlay = {
  expectedRevision: number;
  revision: number;
  ranges: { from: number; to: number }[];
  assignments: Record<string, { revision: number; assign: boolean; label: string; completedAt: string }>;
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
    const priorAssignment = current?.assignments[receipt.tag_id];
    const newerAssignment = !priorAssignment || node.revision > priorAssignment.revision ||
      (node.revision === priorAssignment.revision && receipt.completed_at >= priorAssignment.completedAt);

    // Keep disconnected evidence until later receipts bridge the gap. Ordering
    // a tag observation must not discard evidence from another tag or replay.
    const ranges: SnapshotReceiptOverlay["ranges"] = [];
    for (const range of [...(current?.ranges ?? []), { from: node.expected_revision, to: node.revision }]
      .sort((left, right) => left.from - right.from)) {
      const previous = ranges.at(-1);
      if (previous && range.from <= previous.to) previous.to = Math.max(previous.to, range.to);
      else ranges.push({ ...range });
    }
    const latest = ranges[ranges.length - 1];
    next[node.node_id] = {
      expectedRevision: latest.from,
      revision: latest.to,
      ranges,
      assignments: newerAssignment ? {
        ...(current?.assignments ?? {}),
        [receipt.tag_id]: {
          revision: node.revision,
          assign: receipt.assign,
          label: tagLabel,
          completedAt: receipt.completed_at,
        },
      } : current!.assignments,
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
