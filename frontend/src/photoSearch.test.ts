import { expect, it } from "vite-plus/test";
import { photoSearchSort, activeFilterCount } from "./photoSearch.js";

it("chooses photo search order", () => {
  for (const [syntax, text, field] of [
    ["advanced", "rating:5 pick", "capture_time"],
    ["simple", "pick", "relevance"],
    ["simple", "  ", "capture_time"],
  ] as const) {
    expect(photoSearchSort({ syntax, text }).field).toBe(field);
  }
});

it("counts only filters editable in the Photos panel", () => {
  expect(
    activeFilterCount({
      kinds: ["photo"],
      asset_ids: ["00000000-0000-4000-8000-000000000001"],
      gps_bounds: { west: "-1", south: "-1", east: "1", north: "1" },
      iso_min: 100,
    }),
  ).toBe(0);
  expect(
    activeFilterCount({
      cameras: ["Example camera"],
      lenses: ["Example lens"],
      locations: ["Example location"],
      set_ids: ["00000000-0000-4000-8000-000000000002"],
      rating_min: 1,
      rating_max: 5,
      flags: ["pick"],
      labels: ["red"],
      capture_after: "2024-01-01",
      capture_before: "2025-01-01",
    }),
  ).toBe(8);
  expect(activeFilterCount({ cameras: [], capture_after: "" })).toBe(0);
});
