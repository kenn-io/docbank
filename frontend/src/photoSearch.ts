import type { Query, QuerySort } from "./query.js";

export function photoSearchSort(query: Pick<Query, "text" | "syntax">): QuerySort {
  return {
    field: query.syntax === "simple" && query.text.trim() ? "relevance" : "capture_time",
    direction: "desc",
  };
}
