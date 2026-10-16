import { expect, it } from "vite-plus/test";
import fixtures from "../../internal/store/testdata/photo-bursts.json";
import { photo } from "./photo-test-fixtures.js";
import { groupBursts } from "./photoStacks.js";

it.each(fixtures)("groups $name", (fixture) => {
  let id = 0;
  const rows = fixture.rows.flatMap((row) =>
    Array.from({ length: "repeat" in row ? row.repeat : 1 }, () => ({
      ...photo(++id, row.time),
      source_folder_key: row.folder,
      capture_time_precision: row.precision,
    })),
  );
  expect(groupBursts(rows, fixture.gap).map((group) => group.asset_ids.length)).toEqual(
    fixture.groups,
  );
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
