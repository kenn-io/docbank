import { facetFilterFields, type FacetDimension } from "./facetFilters.js";
import type { Query, QuerySort } from "./query.js";

export function photoSearchSort(query: Pick<Query, "text" | "syntax">): QuerySort {
  return {
    field: query.syntax === "simple" && query.text.trim() ? "relevance" : "capture_time",
    direction: "desc",
  };
}

export function isFilterSet(value: unknown): boolean {
  return Array.isArray(value)
    ? value.length > 0
    : value !== undefined && value !== false && value !== "";
}

// activeFilterCount counts the facet groups with an applied filter, so both ends of a range count once.
export function activeFilterCount(
  filters: Query["filters"],
  dimensions: readonly FacetDimension[],
): number {
  return dimensions.filter((dimension) =>
    facetFilterFields[dimension].some((field) => isFilterSet(filters?.[field])),
  ).length;
}
