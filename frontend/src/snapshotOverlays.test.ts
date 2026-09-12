import { expect, it } from "vitest";
import type { BatchTagReceipt } from "./batch-tags.js";
import { applySnapshotReceiptOverlay, visibleSnapshotOverlay } from "./snapshotOverlays.js";
import type { SnapshotPage, SnapshotRow } from "./snapshots.js";

const tagID = "22222222-2222-4222-8222-222222222222";

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
