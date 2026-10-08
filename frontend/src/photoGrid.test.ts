import { expect, it } from "vitest";
import { computeJustified, computeMonthLayout, groupPhotos, photoAspect, visibleRows } from "./photoGrid.js";
import { photo } from "./photo-test-fixtures.js";

it("fits panoramas and full justified rows within the available width", () => {
  const layout = computeJustified([{ aspect: 20 }, ...Array.from({ length: 20 }, () => ({ aspect: 1.5 }))], { containerWidth: 800, targetRowHeight: 200 });
  for (const row of layout.rows) {
    expect(row.items.at(-1)!.x + row.items.at(-1)!.width).toBeLessThanOrEqual(800.001);
  }
  expect(layout.rows[0].height).toBe(40);
});

it("bounds the mounted rows in one month with 10,000 photos", () => {
  expect(computeMonthLayout([], 800, 200).intrinsicHeight).toBe(0);
  const items = Array.from({ length: 10_000 }, (_, index) => photo(index));
  const layout = computeMonthLayout(items, 1200, 200);
  expect(layout.intrinsicHeight).toBe(layout.totalHeight + 56);
  for (const top of [0, 10_000, 100_000, layout.totalHeight - 1000]) {
    const rows = visibleRows(layout.rows, top, top + 2000);
    expect(rows.flatMap(row => row.items).length).toBeLessThan(100);
    expect(rows.every(row => row.y + row.height >= top && row.y <= top + 2000)).toBe(true);
  }
});

it("uses preview dimensions before technical dimensions and falls back to a square", () => {
  const item = photo(1);
  expect(photoAspect(item)).toBe(1.5);
  item.previews.grid = { state: "ready", width: 400, height: 600 };
  expect(photoAspect(item)).toBe(2 / 3);
  item.previews.grid = { state: "missing" };
  item.height_px = null;
  expect(photoAspect(item)).toBe(1);
});

it("groups recorded months and capture sessions across appended pages", () => {
  const months = groupPhotos([photo(3, "2024-12-31T23:30:00-12:00"), photo(1, "2025-01-01T00:30:00+14:00")], "months");
  expect(months.map(group => group.key)).toEqual(["2025-01", "2024-12"]);
  const firstPage = [photo(1, "2025-06-01T17:00:00"), photo(2, "2025-06-01T10:00:00")];
  const secondPage = [photo(3, "2025-06-01T09:00:00")];
  const groups = groupPhotos([...firstPage, ...secondPage], "sessions");
  expect(groups.map(group => group.items.map(item => item.asset_id))).toEqual([["photo-1"], ["photo-2", "photo-3"]]);
  const latestKey = groups[0].key;
  expect(groupPhotos([...firstPage, ...secondPage, photo(4, "2025-06-01T17:00:00"), photo(5, "2025-06-01T16:00:00")], "sessions")[0].key).toBe(latestKey);
  expect(groupPhotos([photo(1, "2025-06-01"), photo(2, "2025-06-01T01")], "sessions")).toHaveLength(1);
  const parsed = groupPhotos([
    photo(1, "2025-06-01T9"), photo(2, "2025-06-01T9:30Z"),
    photo(3, "2025-06-01T9:45+00:00"), photo(4, "2025-06-01T10:00:00,123456789Z"),
    photo(5, "2025-06-01T10:00:00.123456789Z"),
  ], "sessions");
  expect(parsed[0].items).toHaveLength(5);
  const invalid = [null, "invalid", "0000-01-02", "10000-01-02", "0001-01-01T00:00:00+01:00", "9999-12-31T23:00:00-02:00"];
  const items = [...invalid.map((capture, index) => photo(index, capture)), photo(6, "0001-01-01T01:00:00+01:00"), photo(7, "9999-12-31T22:00:00-01:00")];
  for (const grouping of ["months", "sessions"] as const) {
    const groups = groupPhotos(items, grouping);
    expect(groups.at(-1)).toMatchObject({ key: "undated", year: "", items: items.slice(0, invalid.length) });
    expect(groups.slice(0, -1).flatMap(group => group.items)).toHaveLength(2);
  }
});
