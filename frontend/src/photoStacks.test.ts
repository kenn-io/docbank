import { expect, it } from "vite-plus/test";
import fixtures from "../../internal/store/testdata/photo-bursts.json";
import { photo } from "./photo-test-fixtures.js";
import { groupBursts } from "./photoStacks.js";

it.each(fixtures)("groups $name", (fixture) => {
  const rows = fixture.rows.map((row, i) => ({
    ...photo(i + 1, row.time),
    source_folder_key: row.folder,
    capture_time_precision: row.precision,
  }));
  expect(groupBursts(rows, fixture.gap).map((group) => group.asset_ids.length)).toEqual(
    fixture.groups,
  );
});
it("caps a burst at 500 frames and keeps the tail", () => {
  const rows = Array.from({ length: 501 }, (_, i) => ({
    ...photo(i + 1, "2024-01-01T12:00:00Z"),
    source_folder_key: "folder",
  }));
  expect(groupBursts(rows, 2).map((group) => group.asset_ids.length)).toEqual([500, 1]);
});
it("represents a burst with its only picked asset, or the first of several picks", () => {
  const rows = [photo(1), photo(2), photo(3)].map((row) => ({
    ...row,
    source_folder_key: "folder",
  }));
  rows[1]!.picked = true;
  expect(groupBursts(rows, 2)[0]?.representative_id).toBe("photo-2");
  rows[2]!.picked = true;
  expect(groupBursts(rows, 2)[0]?.representative_id).toBe("photo-1");
});
