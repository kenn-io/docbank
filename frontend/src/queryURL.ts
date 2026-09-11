import { canonicalQuery, parseQuery, type Query } from "./query.js";

// Query state stays in the fragment (never sent to the daemon). Authentication
// is consumed independently; only the validated query is written back.
export function queryFromFragment(hash: string): Query | null {
  if (hash.length > 256 * 1024) throw new Error("Query URL exceeds its size limit.");
  const values = new URLSearchParams(hash.replace(/^#/, "")).getAll("query");
  if (values.length > 1) throw new Error("Query URL contains multiple definitions.");
  return values.length ? parseQuery(values[0]) : null;
}

export function replaceQueryURL(query: Query | null): void {
  const fragment = query ? `#query=${encodeURIComponent(canonicalQuery(query))}` : "";
  history.replaceState(null, "", `${location.pathname}${location.search}${fragment}`);
}
