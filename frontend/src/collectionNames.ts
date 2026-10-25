import * as generated from "./generated/docbank.js";
import type { Collection } from "./generated/docbank.js";

// Import sources are a JSON array of paths; older imports may have plain text.
export function sourcePaths(collection: Pick<Collection, "source_description">): string[] {
  try {
    const value: unknown = JSON.parse(collection.source_description);
    if (Array.isArray(value) && value.every((path) => typeof path === "string")) return value;
  } catch {
    /* Older imports may have a plain-text source. */
  }
  return [collection.source_description];
}

// What the UI calls an import: its label, else the folder it came from.
export function displayName(
  collection: Pick<Collection, "id" | "label" | "source_description">,
): string {
  return (
    collection.label ??
    sourcePaths(collection)[0]?.replace(/\\/g, "/").replace(/\/$/, "").split("/").pop() ??
    `Import ${collection.id.slice(0, 8)}`
  );
}

export async function loadCollectionNames(
  session: string,
  signal?: AbortSignal,
): Promise<Record<string, string>> {
  const names: Record<string, string> = {};
  const limit = 100;
  for (let offset = 0; ; offset += limit) {
    const page = await generated.listCollections({ limit, offset }, { session, signal });
    for (const collection of page.items) names[collection.id] = displayName(collection);
    if (offset + limit >= page.total) return names;
  }
}
