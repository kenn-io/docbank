import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/svelte";
import FacetSidebar from "./FacetSidebar.svelte";
import type { Query } from "./query.js";
import type { SnapshotPage } from "./snapshots.js";

afterEach(cleanup);

it("selects unrated photos and empty flags and labels", async () => {
  const onchange = vi.fn();
  render(FacetSidebar, {
    unit: "photos",
    query: originalQuery,
    onchange,
    facets: [
      facet("rating", {
        values: [0, 1, 5].map((rating) => ({
          key: String(rating),
          label: String(rating),
          count: 2,
          selected: false,
        })),
      }),
      facet("flag", { values: [{ key: "none", label: "None", count: 2, selected: false }] }),
      facet("label", { values: [{ key: "none", label: "None", count: 2, selected: false }] }),
    ],
  });
  expect(screen.getByRole("button", { name: "1 star, 2 photos" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "5 stars, 2 photos" })).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Unrated, 2 photos" }));
  expect(onchange.mock.lastCall?.[0].filters).toMatchObject({ rating_min: 0, rating_max: 0 });
  await fireEvent.click(
    within(screen.getByRole("group", { name: "Flag facet" })).getByRole("button"),
  );
  expect(onchange.mock.lastCall?.[0].filters.flags).toEqual([""]);
  await fireEvent.click(
    within(screen.getByRole("group", { name: "Color label facet" })).getByRole("button"),
  );
  expect(onchange.mock.lastCall?.[0].filters.labels).toEqual([""]);
});

const tagID = "11111111-1111-4111-8111-111111111111";
const excludedTagID = "22222222-2222-4222-8222-222222222222";
const originalQuery: Query = {
  v: 1,
  text: "report AND NOT tag:obsolete",
  syntax: "advanced",
  mode: "lexical",
  filters: { exclude_tag_ids: [excludedTagID], paths: ["/records"] },
  sort: { field: "path", direction: "asc" },
};

function facet(
  dimension: SnapshotPage["facets"][number]["dimension"],
  overrides: Partial<SnapshotPage["facets"][number]> = {},
): SnapshotPage["facets"][number] {
  return { dimension, available: true, total: 9, missing: 1, other: 2, values: [], ...overrides };
}

const facets: SnapshotPage["facets"] = [
  facet("collections", {
    values: [
      { key: "33333333-3333-4333-8333-333333333333", label: "Intake", count: 2, selected: false },
    ],
  }),
  facet("tags", { values: [{ key: tagID, label: "reviewed", count: 0, selected: true }] }),
  facet("media_family", {
    values: [{ key: "document", label: "document", count: 3, selected: false }],
  }),
  facet("extension", { values: [{ key: "pdf", label: "pdf", count: 4, selected: false }] }),
  facet("modified", { values: [{ key: "2026-02", label: "2026-02", count: 5, selected: false }] }),
  facet("size", {
    values: [{ key: "1_mib_to_10_mib", label: "1_mib_to_10_mib", count: 6, selected: false }],
  }),
  facet("text_coverage", {
    available: false,
    reason: "coverage_unconfigured",
    total: undefined,
    missing: undefined,
    other: undefined,
  }),
  facet("duplicates", {
    values: [
      { key: "duplicate", label: "duplicate", count: 2, selected: false },
      { key: "unique", label: "unique", count: 7, selected: false },
    ],
  }),
];

it("shows the facets that can narrow the results and leaves out the rest", () => {
  render(FacetSidebar, { facets, query: originalQuery, disabled: false, onchange: vi.fn() });
  expect(screen.getAllByRole("heading", { level: 3 })).toHaveLength(7);
  expect(
    screen
      .getByRole("button", { name: "reviewed, 0 documents, selected" })
      .getAttribute("aria-pressed"),
  ).toBe("true");
  expect(screen.queryByRole("group", { name: "Text coverage facet" })).toBeNull();
  expect(screen.queryByText(/coverage|informational|unavailable/i)).toBeNull();
  expect(screen.queryByText(/unique/i)).toBeNull();
});

it("names imports and falls back to a short ID for an unnamed one", () => {
  const named = "33333333-3333-4333-8333-333333333333";
  const unnamed = "55555555-5555-4555-8555-555555555555";
  render(FacetSidebar, {
    facets: [
      facet("collections", {
        values: [
          { key: named, label: named, count: 3, selected: false },
          { key: unnamed, label: unnamed, count: 2, selected: false },
        ],
      }),
    ],
    query: originalQuery,
    collectionNames: { [named]: "Intake box" },
    onchange: vi.fn(),
  });
  expect(screen.getByRole("button", { name: "Intake box, 3 documents" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Import 55555555, 2 documents" })).toBeTruthy();
});

it("hides zero counts, single-value groups that cover everything, and unclickable values", async () => {
  const view = render(FacetSidebar, {
    facets: [
      facet("extension", {
        total: 9,
        values: [
          { key: "pdf", label: "pdf", count: 9, selected: false },
          { key: "txt", label: "txt", count: 0, selected: false },
        ],
      }),
      facet("media_family", {
        total: 9,
        values: [
          { key: "text", label: "text", count: 7, selected: false },
          { key: "image", label: "image", count: 2, selected: false },
          { key: "audio_video", label: "audio video", count: 0, selected: false },
        ],
      }),
    ],
    query: originalQuery,
    onchange: vi.fn(),
  });
  expect(screen.queryByRole("group", { name: "Extension facet" })).toBeNull();
  const family = within(screen.getByRole("group", { name: "Media family facet" }));
  expect(family.getAllByRole("button").map((button) => button.textContent?.trim())).toEqual([
    "text7",
    "image2",
  ]);
  expect(view.container.textContent).not.toContain("audio video");
  expect(screen.getByRole("heading", { level: 2, name: "Filters" })).toBeTruthy();
});

it.each([
  ["camera", "with no camera", "uses of unlisted camera names", "use of unlisted camera names"],
  ["lens", "with no lens", "uses of unlisted lens names", "use of unlisted lens names"],
  ["year", "with no capture date", "in unlisted years", "in unlisted year"],
  ["location", "with no location", "in unlisted locations", "in unlisted location"],
  ["set", "not in an album", "memberships in unlisted albums", "membership in unlisted albums"],
  [
    "collections",
    "not in a collection",
    "memberships in unlisted collections",
    "membership in unlisted collections",
  ],
  ["tags", "with no tags", "uses of unlisted tags", "use of unlisted tags"],
  ["extension", "with no extension", "in unlisted extensions", "in unlisted extension"],
  ["modified", "with no modified date", "in unlisted months", "in unlisted month"],
] as const)(
  "shows positive %s counts in plain words and hides zeros",
  async (dimension, missing, other, singular) => {
    // The server lists the 50 largest buckets before counting the rest as other.
    const key = (i: number) =>
      dimension === "modified"
        ? `${2000 + Math.floor(i / 12)}-${String((i % 12) + 1).padStart(2, "0")}`
        : ["set", "collections", "tags"].includes(dimension)
          ? `00000000-0000-4000-8000-${String(i).padStart(12, "0")}`
          : `v${i}`;
    const listed = (count: number) =>
      Array.from({ length: count }, (_, i) => ({
        key: key(i),
        label: key(i),
        count: 1,
        selected: false,
      }));
    const view = render(FacetSidebar, {
      facets: [facet(dimension, { total: 62, missing: 6, other: 6, values: listed(50) })],
      query: originalQuery,
      onchange: vi.fn(),
    });
    expect(screen.getByText(`6 ${missing}`)).toBeTruthy();
    expect(screen.getByText(`6 ${other}`)).toBeTruthy();
    await view.rerender({
      facets: [facet(dimension, { total: 52, missing: 1, other: 1, values: listed(50) })],
    });
    expect(screen.getByText(`1 ${missing}`)).toBeTruthy();
    expect(screen.getByText(`1 ${singular}`)).toBeTruthy();
    await view.rerender({
      facets: [facet(dimension, { total: 56, missing: 0, other: 6, values: listed(50) })],
    });
    expect(screen.queryByText(`0 ${missing}`)).toBeNull();
    expect(screen.getByText(`6 ${other}`)).toBeTruthy();
    await view.rerender({
      facets: [facet(dimension, { total: 9, missing: 6, other: 0, values: listed(3) })],
    });
    expect(screen.getByText(`6 ${missing}`)).toBeTruthy();
    expect(view.container.querySelectorAll(".summary span")).toHaveLength(
      dimension === "tags" ? 0 : 1,
    );
    await view.rerender({
      facets: [facet(dimension, { total: 9, missing: 0, other: 0, values: listed(9) })],
    });
    expect(view.container.querySelector(".summary")).toBeNull();
  },
);

it("hides a group with no values or an empty scope, and keeps one with some missing or a selected value", async () => {
  const selectedTag = { key: tagID, label: "reviewed", count: 0, selected: true };
  const view = render(FacetSidebar, {
    facets: [facet("tags", { total: 9, missing: 9, other: 0 })],
    query: originalQuery,
    onchange: vi.fn(),
  });
  expect(screen.queryByRole("group", { name: "Tags facet" })).toBeNull();
  await view.rerender({
    facets: [
      facet("tags", {
        total: 9,
        missing: 4,
        other: 0,
        values: [{ ...selectedTag, count: 5, selected: false }],
      }),
    ],
  });
  expect(screen.getByRole("group", { name: "Tags facet" })).toBeTruthy();
  await view.rerender({
    facets: [facet("tags", { total: 9, missing: 9, other: 0, values: [selectedTag] })],
  });
  expect(screen.getByRole("group", { name: "Tags facet" })).toBeTruthy();
  await view.rerender({ facets: [facet("tags", { total: 0, missing: 0, other: 0 })] });
  expect(screen.queryByRole("group", { name: "Tags facet" })).toBeNull();
});

it("keeps an active modified month in an empty scope so it can be cleared", async () => {
  const onchange = vi.fn();
  const month = { key: "2025-07", label: "2025-07", count: 0, selected: true };
  const filters = {
    ...originalQuery.filters,
    modified_after: "2025-07-01T00:00:00Z",
    modified_before: "2025-08-01T00:00:00Z",
  };
  render(FacetSidebar, {
    facets: [facet("modified", { total: 0, missing: 0, other: 0, values: [month] })],
    query: { ...originalQuery, filters },
    onchange,
  });
  await fireEvent.click(
    within(screen.getByRole("group", { name: "Modified facet" })).getByRole("button", {
      name: /July 2025/,
    }),
  );
  expect(onchange.mock.calls.at(-1)?.[0].filters.modified_after).toBeUndefined();
});

it("keeps the No tags button reachable to clear while active and to apply after a tag is selected", async () => {
  const onchange = vi.fn();
  const view = render(FacetSidebar, {
    facets: [facet("tags", { total: 9, missing: 9, other: 0 })],
    query: { ...originalQuery, filters: { ...originalQuery.filters, no_tags: true } },
    onchange,
  });
  const button = screen.getByRole("button", { name: "9 with no tags" });
  expect(button.getAttribute("aria-pressed")).toBe("true");
  await fireEvent.click(button);
  expect(onchange.mock.calls.at(-1)?.[0].filters.no_tags).toBeUndefined();
  await view.rerender({
    facets: [
      facet("tags", {
        total: 9,
        missing: 0,
        other: 0,
        values: [{ key: tagID, label: "reviewed", count: 9, selected: true }],
      }),
    ],
    query: { ...originalQuery, filters: { ...originalQuery.filters, tag_ids: [tagID] } },
  });
  expect(screen.getByRole("button", { name: "No tags" })).toBeTruthy();
});

it("preserves expression, syntax, exclusions, and unrelated filters when applying supported buckets", async () => {
  const onchange = vi.fn();
  const view = render(FacetSidebar, { facets, query: originalQuery, disabled: false, onchange });
  await fireEvent.click(screen.getByRole("button", { name: "pdf, 4 documents" }));
  const sentQuery = onchange.mock.calls.at(-1)?.[0] as Query;
  expect(sentQuery.text).toBe(originalQuery.text);
  expect(sentQuery.syntax).toBe(originalQuery.syntax);
  expect(sentQuery.filters.exclude_tag_ids).toEqual(originalQuery.filters.exclude_tag_ids);
  expect(sentQuery.filters.paths).toEqual(originalQuery.filters.paths);
  expect(sentQuery.filters.extensions).toEqual(["pdf"]);

  await fireEvent.click(screen.getByRole("button", { name: "1 MiB to 10 MiB, 6 documents" }));
  expect(onchange.mock.calls.at(-1)?.[0].filters).toMatchObject({
    size_min: 1_048_576,
    size_max: 10_485_759,
  });
  await fireEvent.click(screen.getByRole("button", { name: "February 2026, 5 documents" }));
  expect(onchange.mock.calls.at(-1)?.[0].filters).toMatchObject({
    modified_after: "2026-02-01T00:00:00Z",
    modified_before: "2026-03-01T00:00:00Z",
  });
  await view.rerender({ query: onchange.mock.calls.at(-1)?.[0] });
  await fireEvent.click(
    screen.getByRole("button", { name: "February 2026, 5 documents, selected" }),
  );
  expect(onchange.mock.calls.at(-1)?.[0].filters.modified_after).toBeUndefined();
  expect(onchange.mock.calls.at(-1)?.[0].filters.modified_before).toBeUndefined();
});

it("maps only tags missing and duplicate documents to supported QueryV1 filters", async () => {
  const onchange = vi.fn();
  render(FacetSidebar, { facets, query: originalQuery, disabled: false, onchange });
  const tags = screen.getByRole("group", { name: "Tags facet" });
  await fireEvent.click(within(tags).getByRole("button", { name: "1 with no tags" }));
  expect(onchange.mock.calls.at(-1)?.[0].filters.no_tags).toBe(true);
  await fireEvent.click(screen.getByRole("button", { name: "duplicate, 2 documents" }));
  expect(onchange.mock.calls.at(-1)?.[0].filters.has_duplicates).toBe(true);
  expect(screen.queryByText(/unique/)).toBeNull();
});

it("submits original expanding names and leaves out invalid or over-cap operands", async () => {
  const raw = "ß".repeat(200),
    oversized = "x".repeat(300),
    onchange = vi.fn();
  const values = [
    { key: raw, label: raw, count: 2, selected: false },
    { key: oversized, label: oversized, count: 1, selected: false },
  ];
  const names = [
    { dimension: "camera", field: "cameras", key: "Camera_One", label: "Camera_One" },
    { dimension: "lens", field: "lenses", key: "NIKKOR_Z 35", label: "NIKKOR_Z 35" },
    { dimension: "location", field: "locations", key: "Paris_Walk", label: "Paris_Walk" },
    {
      dimension: "set",
      field: "set_ids",
      key: "44444444-4444-4444-8444-444444444444",
      label: "Trip_2024",
    },
  ] as const;
  const view = render(FacetSidebar, {
    facets: [facet("camera", { values }), facet("location", { values: [values[1]] })],
    query: originalQuery,
    disabled: false,
    onchange,
  });
  await fireEvent.click(screen.getByRole("button", { name: `${raw}, 2 documents` }));
  expect(onchange.mock.calls.at(-1)?.[0].filters.cameras).toEqual([raw]);
  expect(screen.queryByText(oversized)).toBeNull();
  const selected = Array.from({ length: 64 }, (_, i) => `Camera ${i}`);
  await view.rerender({
    query: { ...originalQuery, filters: { locations: selected } },
    facets: [
      facet("location", { values: [{ key: "Other", label: "Other", count: 1, selected: false }] }),
    ],
  });
  expect(screen.queryByText("Other")).toBeNull();
  await view.rerender({
    query: originalQuery,
    facets: names.map((value) =>
      facet(value.dimension, {
        values: [{ key: value.key, label: value.label, count: 1, selected: false }],
      }),
    ),
  });
  for (const value of names) {
    const button = screen.getByRole("button", { name: `${value.label}, 1 documents` });
    expect(within(button).getByText(value.label, { exact: true })).toBeTruthy();
    await fireEvent.click(button);
    expect(onchange.mock.calls.at(-1)?.[0].filters[value.field]).toEqual([value.key]);
  }
  await view.rerender({
    facets: [
      facet("camera", {
        values: [
          { key: "sony", label: "SONY", count: 2, selected: true },
          { key: "strasse", label: "Straße", count: 1, selected: true },
        ],
      }),
    ],
    query: { ...originalQuery, filters: { cameras: ["SoNy", "STRASSE"] } },
  });
  await fireEvent.click(screen.getByRole("button", { name: "SONY, 2 documents, selected" }));
  expect(onchange.mock.calls.at(-1)?.[0].filters.cameras).toEqual(["strasse"]);
});

it("omits the whole Filters panel when no group can narrow one result", () => {
  render(FacetSidebar, {
    query: originalQuery,
    onchange: vi.fn(),
    facets: [
      facet("extension", {
        total: 1,
        missing: 0,
        other: 0,
        values: [{ key: "pdf", label: "pdf", count: 1, selected: false }],
      }),
    ],
  });
  expect(screen.queryByRole("complementary", { name: "Snapshot facets" })).toBeNull();
  expect(screen.queryByRole("heading", { name: "Filters" })).toBeNull();
});
