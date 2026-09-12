import { describe, expect, it } from "vitest";
import type { Node } from "./api.js";
import {
  clearSelection,
  reconcileSelection,
  selectVisibleDocuments,
  selectedTargets,
  toggleDocumentSelection,
  type SelectableRow,
} from "./selection.js";

function node(id: number, kind: Node["kind"] = "file", revision = id): Node {
  return {
    id,
    name: `node-${id}`,
    kind,
    size: kind === "file" ? id * 10 : 0,
    revision,
    created_at: "2026-09-09T00:00:00Z",
    modified_at: "2026-09-09T00:00:00Z",
  };
}

function row(id: number, kind: Node["kind"] = "file", revision = id): SelectableRow {
  return { node: node(id, kind, revision), path: `/node-${id}` };
}

describe("document selection", () => {
  it("toggles one document and moves the range anchor", () => {
    const rows = [row(1), row(2), row(3)];
    const checked = toggleDocumentSelection(
      { selectedIDs: new Set<number>(), anchorID: undefined },
      rows,
      2,
      true,
      false,
    );
    expect([...checked.selectedIDs]).toEqual([2]);
    expect(checked.anchorID).toBe(2);

    const unchecked = toggleDocumentSelection(checked, rows, 2, false, false);
    expect([...unchecked.selectedIDs]).toEqual([]);
    expect(unchecked.anchorID).toBe(2);
  });

  it("checks an inclusive Shift range in displayed eligible order", () => {
    const displayed = [row(4), row(3, "dir"), row(2), row(1)];
    const result = toggleDocumentSelection(
      { selectedIDs: new Set([4]), anchorID: 4 },
      displayed,
      1,
      true,
      true,
    );
    expect([...result.selectedIDs].sort()).toEqual([1, 2, 4]);
    expect(result.anchorID).toBe(4);
  });

  it("unchecks an inclusive Shift range", () => {
    const displayed = [row(1), row(2), row(3), row(4)];
    const result = toggleDocumentSelection(
      { selectedIDs: new Set([1, 2, 3, 4]), anchorID: 2 },
      displayed,
      4,
      false,
      true,
    );
    expect([...result.selectedIDs]).toEqual([1]);
    expect(result.anchorID).toBe(2);
  });

  it("uses the current displayed sort order for the next range", () => {
    const sorted = [row(3), row(1), row(4), row(2)];
    const result = toggleDocumentSelection(
      { selectedIDs: new Set([1]), anchorID: 1 },
      sorted,
      2,
      true,
      true,
    );
    expect([...result.selectedIDs].sort()).toEqual([1, 2, 4]);
  });

  it("falls back to a normal toggle when the anchor is no longer eligible", () => {
    const result = toggleDocumentSelection(
      { selectedIDs: new Set([9]), anchorID: 9 },
      [row(1), row(2)],
      2,
      true,
      true,
    );
    expect([...result.selectedIDs].sort()).toEqual([2, 9]);
    expect(result.anchorID).toBe(2);
  });

  it("selects visible files while excluding directories", () => {
    const result = selectVisibleDocuments([row(1), row(2, "dir"), row(3)]);
    expect([...result.selectedIDs]).toEqual([1, 3]);
    expect(result.anchorID).toBeUndefined();
  });

  it("clears IDs and the anchor", () => {
    expect(clearSelection()).toEqual({
      selectedIDs: new Set<number>(),
      anchorID: undefined,
    });
  });

  it("reconciles removed IDs and derives targets from current revisions", () => {
    const visible = [row(2, "file", 20), row(3, "file", 30), row(4, "dir", 40)];
    const state = reconcileSelection(
      { selectedIDs: new Set([1, 2, 4]), anchorID: 1 },
      visible,
    );
    expect([...state.selectedIDs]).toEqual([2]);
    expect(state.anchorID).toBeUndefined();
    expect(selectedTargets(visible, state.selectedIDs)).toEqual([
      { node_id: 2, revision: 20 },
    ]);
  });
});
