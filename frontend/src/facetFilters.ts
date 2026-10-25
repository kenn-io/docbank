import type { QueryFilters } from "./query.js";
import type { SnapshotPage } from "./snapshots.js";

export type FacetDimension = SnapshotPage["facets"][number]["dimension"];

// The query filters each facet group sets. FacetSidebar writes them, and the Photos Filters
// badge counts them, so a new facet must declare its filters here before either can use it.
export const facetFilterFields = {
  collections: ["collection_ids"],
  tags: ["tag_ids"],
  media_family: ["media_families"],
  extension: ["extensions"],
  modified: ["modified_after", "modified_before"],
  size: ["size_min", "size_max"],
  text_coverage: ["text_coverage"],
  duplicates: ["has_duplicates"],
  camera: ["cameras"],
  lens: ["lenses"],
  year: ["capture_after", "capture_before"],
  location: ["locations"],
  set: ["set_ids"],
  rating: ["rating_min", "rating_max"],
  flag: ["flags"],
  label: ["labels"],
} as const satisfies Record<FacetDimension, readonly (keyof QueryFilters)[]>;
