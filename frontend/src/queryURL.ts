import { canonicalQuery, parseQuery, type Query } from "./query.js";

// Query state stays in the fragment (never sent to the daemon). Authentication
// is consumed independently; query writes preserve other fragment state.
export function queryFromFragment(hash: string): Query | null {
  if (hash.length > 256 * 1024) throw new Error("Query URL exceeds its size limit.");
  const values = new URLSearchParams(hash.replace(/^#/, "")).getAll("query");
  if (values.length > 1) throw new Error("Query URL contains multiple definitions.");
  return values.length ? parseQuery(values[0]) : null;
}

export function replaceQueryURL(query: Query | null): void {
  const url = new URL(location.href);
  const params = new URLSearchParams(url.hash.slice(1));
  if (query) params.set("query", canonicalQuery(query));
  else params.delete("query");
  url.hash = params.toString();
  history.replaceState(history.state, "", url.href);
}
