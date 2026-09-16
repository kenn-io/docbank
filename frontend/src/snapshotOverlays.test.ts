import { expect, it } from "vitest";
import type { BatchTagReceipt } from "./batch-tags.js";
import { applySnapshotReceiptOverlay, snapshotTargetRevision, visibleSnapshotOverlay } from "./snapshotOverlays.js";
import type { SnapshotPage, SnapshotRow } from "./snapshots.js";

const tagID = "22222222-2222-4222-8222-222222222222";

it("advances targets only through a continuous tag-receipt chain from the frozen revision", () => {
  const row = { node_id: 7, revision: 3 };
  const first = applySnapshotReceiptOverlay({}, receipt(4, true, "2026-09-11T12:00:00Z"), "Review");
  const secondReceipt = receipt(5, false, "2026-09-11T12:01:00Z");
  const second = applySnapshotReceiptOverlay(first, secondReceipt, "Review");
  const replay = applySnapshotReceiptOverlay(second, secondReceipt, "Review");
  expect(snapshotTargetRevision(row, first)).toBe(4);
  expect(snapshotTargetRevision(row, replay)).toBe(5);
  expect(snapshotTargetRevision({ ...row, revision: 4 }, replay)).toBe(5);
  const gap = applySnapshotReceiptOverlay(second, receipt(7, true, "2026-09-11T12:02:00Z"), "Review");
  expect(snapshotTargetRevision(row, gap)).toBe(3);
  expect(snapshotTargetRevision(row, applySnapshotReceiptOverlay({}, secondReceipt, "Review"))).toBe(3);
});

function receipt(revision: number, assign: boolean, completedAt: string): BatchTagReceipt {
  return {
    version: 1,
    operation_id: "11111111-1111-4111-8111-111111111111",
    request_digest: "a".repeat(64),
    tag_id: tagID,
    assign,
    tag_revision: 9,
    assignment_count: assign ? 1 : 0,
    completed_at: completedAt,
    nodes: [{ node_id: 7, expected_revision: revision - 1, revision, changed: true }],
  };
}

it("layers receipts without mutating frozen membership, totals, order, or hash", () => {
  const rows = [{ node_id: 7, revision: 3 }, { node_id: 9, revision: 4 }] as SnapshotRow[];
  const before = { member_hash: "b".repeat(64), total: 2, rows } as SnapshotPage;
  const frozen = JSON.stringify(before);

  const overlays = applySnapshotReceiptOverlay({}, receipt(4, true, "2026-09-11T12:00:00.000000000Z"), "Review");
  const after = before;

  expect(after.member_hash).toBe(before.member_hash);
  expect(after.total).toBe(before.total);
  expect(after.rows.map((row) => row.node_id)).toEqual(before.rows.map((row) => row.node_id));
  expect(JSON.stringify(after)).toBe(frozen);
  expect(visibleSnapshotOverlay(rows[0], overlays)?.assignments[tagID]).toMatchObject({ assign: true, label: "Review" });
});

it("never replaces a newer receipt observation with an older one", () => {
  const newer = applySnapshotReceiptOverlay({}, receipt(6, true, "2026-09-11T12:02:00.000000000Z"), "Review");
  const lowerRevision = applySnapshotReceiptOverlay(newer, receipt(5, false, "2026-09-11T12:03:00.000000000Z"), "Review");
  const earlierAtSameRevision = applySnapshotReceiptOverlay(lowerRevision, receipt(6, false, "2026-09-11T12:01:00.000000000Z"), "Review");

  expect(earlierAtSameRevision[7].revision).toBe(6);
  expect(earlierAtSameRevision[7].assignments[tagID].assign).toBe(true);
  expect(earlierAtSameRevision[7].assignments[tagID].completedAt).toBe("2026-09-11T12:02:00.000000000Z");
});

it("keeps out-of-order cross-tag observations and joins their revision evidence", () => {
  const otherTag = "33333333-3333-4333-8333-333333333333";
  const row = { node_id: 7, revision: 3 };
  const earlier = receipt(4, true, "2026-09-11T12:00:00Z");
  const later = { ...receipt(5, true, "2026-09-11T12:01:00Z"), tag_id: otherTag };
  let overlays = applySnapshotReceiptOverlay({}, later, "Archive");
  expect(snapshotTargetRevision(row, overlays)).toBe(3);
  overlays = applySnapshotReceiptOverlay(overlays, earlier, "Review");
  expect(overlays[7].assignments[tagID]).toMatchObject({ assign: true, label: "Review" });
  expect(overlays[7].assignments[otherTag]).toMatchObject({ assign: true, label: "Archive" });
  expect(snapshotTargetRevision(row, overlays)).toBe(5);

  // Keep separated evidence until a later receipt actually fills the gap.
  overlays = applySnapshotReceiptOverlay(overlays, receipt(7, false, "2026-09-11T12:03:00Z"), "Review");
  expect(snapshotTargetRevision(row, overlays)).toBe(3);
  overlays = applySnapshotReceiptOverlay(overlays, { ...receipt(6, false, "2026-09-11T12:02:00Z"), tag_id: otherTag }, "Archive");
  overlays = applySnapshotReceiptOverlay(overlays, earlier, "Review");
  expect(snapshotTargetRevision(row, overlays)).toBe(7);
  expect(overlays[7].assignments[tagID].assign).toBe(false);
  expect(overlays[7].assignments[otherTag].assign).toBe(false);
});
