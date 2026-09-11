import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/svelte";
import FacetSidebar from "./FacetSidebar.svelte";
import type { Query } from "./query.js";
import type { SnapshotPage } from "./snapshots.js";

afterEach(cleanup);

const tagID = "11111111-1111-4111-8111-111111111111";
const excludedTagID = "22222222-2222-4222-8222-222222222222";
const originalQuery: Query = {
  v: 1, text: "report AND NOT tag:obsolete", syntax: "advanced", mode: "lexical",
  filters: { exclude_tag_ids: [excludedTagID], paths: ["/records"] },
  sort: { field: "path", direction: "asc" },
};

function facet(dimension: SnapshotPage["facets"][number]["dimension"], overrides: Partial<SnapshotPage["facets"][number]> = {}): SnapshotPage["facets"][number] {
  return { dimension, available: true, total: 9, missing: 1, other: 2, values: [], ...overrides };
}

const facets: SnapshotPage["facets"] = [
  facet("collections", { values: [{ key: "33333333-3333-4333-8333-333333333333", label: "Intake", count: 2, selected: false }] }),
  facet("tags", { values: [{ key: tagID, label: "reviewed", count: 0, selected: true }] }),
  facet("media_family", { values: [{ key: "document", label: "document", count: 3, selected: false }] }),
  facet("extension", { values: [{ key: "pdf", label: "pdf", count: 4, selected: false }] }),
  facet("modified", { values: [{ key: "2026-02", label: "2026-02", count: 5, selected: false }] }),
  facet("size", { values: [{ key: "1_mib_to_10_mib", label: "1_mib_to_10_mib", count: 6, selected: false }] }),
  facet("text_coverage", { available: false, reason: "coverage_unconfigured", total: undefined, missing: undefined, other: undefined }),
  facet("duplicates", { values: [{ key: "duplicate", label: "duplicate", count: 2, selected: false }, { key: "unique", label: "unique", count: 7, selected: false }] }),
];

it("shows every requested facet with selected-zero, missing, other, and unavailable evidence", () => {
  render(FacetSidebar, { facets, query: originalQuery, disabled: false, onchange: vi.fn() });
  expect(screen.getAllByRole("heading", { level: 3 })).toHaveLength(8);
  expect(screen.getByRole("button", { name: "reviewed, 0 documents, selected" }).getAttribute("aria-pressed")).toBe("true");
  expect(screen.getAllByText("Missing 1")).toHaveLength(7);
  expect(screen.getAllByText("Other 2")).toHaveLength(7);
  expect(within(screen.getByRole("group", { name: "Text coverage facet" })).getByText(/coverage unconfigured/i)).toBeTruthy();
  expect(screen.queryByRole("button", { name: /unique/i })).toBeNull();
  expect(screen.getByText("unique · 7 · informational")).toBeTruthy();
});

it("preserves expression, syntax, exclusions, and unrelated filters when applying supported buckets", async () => {
  const onchange = vi.fn();
  render(FacetSidebar, { facets, query: originalQuery, disabled: false, onchange });
  await fireEvent.click(screen.getByRole("button", { name: "pdf, 4 documents" }));
  const sentQuery = onchange.mock.calls.at(-1)?.[0] as Query;
  expect(sentQuery.text).toBe(originalQuery.text);
  expect(sentQuery.syntax).toBe(originalQuery.syntax);
  expect(sentQuery.filters.exclude_tag_ids).toEqual(originalQuery.filters.exclude_tag_ids);
  expect(sentQuery.filters.paths).toEqual(originalQuery.filters.paths);
  expect(sentQuery.filters.extensions).toEqual(["pdf"]);

  await fireEvent.click(screen.getByRole("button", { name: "1 MiB to 10 MiB, 6 documents" }));
  expect(onchange.mock.calls.at(-1)?.[0].filters).toMatchObject({ size_min: 1_048_576, size_max: 10_485_759 });
  await fireEvent.click(screen.getByRole("button", { name: "February 2026, 5 documents" }));
  expect(onchange.mock.calls.at(-1)?.[0].filters).toMatchObject({
    modified_after: "2026-02-01T00:00:00Z", modified_before: "2026-03-01T00:00:00Z",
  });
});

it("maps only tags missing and duplicate documents to supported QueryV1 filters", async () => {
  const onchange = vi.fn();
  render(FacetSidebar, { facets, query: originalQuery, disabled: false, onchange });
  const tags = screen.getByRole("group", { name: "Tags facet" });
  await fireEvent.click(within(tags).getByRole("button", { name: "Missing 1" }));
  expect(onchange.mock.calls.at(-1)?.[0].filters.no_tags).toBe(true);
  await fireEvent.click(screen.getByRole("button", { name: "duplicate, 2 documents" }));
  expect(onchange.mock.calls.at(-1)?.[0].filters.has_duplicates).toBe(true);
  expect(screen.getByText("unique · 7 · informational")).toBeTruthy();
});
