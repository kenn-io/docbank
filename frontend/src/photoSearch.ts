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

export function activeFilterCount(filters: Query["filters"]): number {
  const fields = new Set([
    "cameras",
    "lenses",
    "locations",
    "set_ids",
    "rating_min",
    "rating_max",
    "flags",
    "labels",
    "capture_after",
    "capture_before",
  ]);
  return new Set(
    Object.entries(filters ?? {})
      .filter(([key, value]) => fields.has(key) && isFilterSet(value))
      .map(([key]) => key.replace(/_(min|max|after|before)$/, "")),
  ).size;
}
