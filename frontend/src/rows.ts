import type { Node } from "./api.js";

export type SortDirection = "asc" | "desc";
export type SortField = "relevance" | "name" | "size" | "modified";
export type SortableRow = { node: Node; path: string };
export type SearchView = {
  sortField: SortField;
  sortDirection: SortDirection;
  selectedID: number | undefined;
};

export function orderRows<Row extends SortableRow>(
  rows: readonly Row[],
  field: SortField,
  direction: SortDirection,
  searchResults: boolean,
): Row[] {
  const ordered = [...rows];
  if (field === "relevance") return ordered;

  ordered.sort((left, right) => {
    if (left.node.kind !== right.node.kind) return left.node.kind === "dir" ? -1 : 1;

    let result = 0;
    if (field === "size") result = left.node.size - right.node.size;
    else if (field === "modified") {
      result = left.node.modified_at.localeCompare(right.node.modified_at);
    } else {
      const leftName = searchResults ? left.path : left.node.name;
      const rightName = searchResults ? right.path : right.node.name;
      result = leftName.localeCompare(rightName);
    }
    if (result === 0) result = left.node.id - right.node.id;
    return direction === "asc" ? result : -result;
  });
  return ordered;
}

export function reconcileSearchView<Row extends SortableRow>(
  rows: readonly Row[],
  query: string,
  previousQuery: string,
  previousSortField: SortField,
  previousSortDirection: SortDirection,
  previousSelectedID: number | undefined,
): SearchView {
  const refreshing = query === previousQuery;
  const sortField = refreshing ? previousSortField : "relevance";
  const sortDirection = refreshing ? previousSortDirection : "asc";
  const ordered = orderRows(rows, sortField, sortDirection, true);
  const selectedID =
    refreshing && rows.some((row) => row.node.id === previousSelectedID)
      ? previousSelectedID
      : ordered[0]?.node.id;
  return { sortField, sortDirection, selectedID };
}
