import type { Query, QuerySort } from "./query.js";

export function hasPersonFilter(query: Pick<Query, "filters" | "text" | "syntax">): boolean {
  if (query.filters.person_ids?.length) return true;
  if (query.syntax !== "advanced") return false;
  // Field names belong to advanced text; quoted phrases remain literal terms.
  const tokens = query.text.match(/"(?:\\.|[^"\\])*"|(?:\\.|[^\s():"\\])+|[:()]/gu) ?? [];
  return tokens.some(
    (token, index) =>
      ["person", "saved"].includes(token.replace(/\\(.)/gu, "$1")) && tokens[index + 1] === ":",
  );
}

export function photoSearchSort(query: Pick<Query, "text" | "syntax">): QuerySort {
  return {
    field: query.syntax === "simple" && query.text.trim() ? "relevance" : "capture_time",
    direction: "desc",
  };
}
