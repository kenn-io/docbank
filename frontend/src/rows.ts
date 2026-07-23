import type { Node } from "./api.js";

export type SortDirection = "asc" | "desc";
export type SortField = "relevance" | "name" | "size" | "modified";
export type SortableRow = { node: Node; path: string };

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
