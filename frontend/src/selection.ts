import type { Node } from "./api.js";

export type SelectableRow = { node: Node; path: string };

export type SelectionState = {
  selectedIDs: Set<number>;
  anchorID: number | undefined;
};

export type SelectionTarget = {
  node_id: number;
  revision: number;
};

function eligibleIDs(rows: readonly SelectableRow[]): number[] {
  return rows.filter((row) => row.node.kind === "file").map((row) => row.node.id);
}

export function clearSelection(): SelectionState {
  return { selectedIDs: new Set<number>(), anchorID: undefined };
}

export function selectVisibleDocuments(rows: readonly SelectableRow[]): SelectionState {
  return { selectedIDs: new Set(eligibleIDs(rows)), anchorID: undefined };
}

export function toggleDocumentSelection(
  state: SelectionState,
  displayedRows: readonly SelectableRow[],
  targetID: number,
  checked: boolean,
  range: boolean,
): SelectionState {
  const displayedIDs = eligibleIDs(displayedRows);
  const targetIndex = displayedIDs.indexOf(targetID);
  const anchorIndex =
    state.anchorID === undefined ? -1 : displayedIDs.indexOf(state.anchorID);
  const selectedIDs = new Set(state.selectedIDs);

  if (targetIndex < 0) return { selectedIDs, anchorID: state.anchorID };
  if (!range || anchorIndex < 0) {
    if (checked) selectedIDs.add(targetID);
    else selectedIDs.delete(targetID);
    return { selectedIDs, anchorID: targetID };
  }

  const start = Math.min(anchorIndex, targetIndex);
  const end = Math.max(anchorIndex, targetIndex);
  for (const id of displayedIDs.slice(start, end + 1)) {
    if (checked) selectedIDs.add(id);
    else selectedIDs.delete(id);
  }
  return { selectedIDs, anchorID: state.anchorID };
}

export function reconcileSelection(
  state: SelectionState,
  rows: readonly SelectableRow[],
): SelectionState {
  const eligible = new Set(eligibleIDs(rows));
  const selectedIDs = new Set(
    [...state.selectedIDs].filter((id) => eligible.has(id)),
  );
  return {
    selectedIDs,
    anchorID:
      state.anchorID !== undefined && eligible.has(state.anchorID)
        ? state.anchorID
        : undefined,
  };
}
export function selectedTargets(
  rows: readonly SelectableRow[],
  selectedIDs: ReadonlySet<number>,
): SelectionTarget[] {
  return rows
    .filter((row) => row.node.kind === "file" && selectedIDs.has(row.node.id))
    .map((row) => ({ node_id: row.node.id, revision: row.node.revision }));
}
