import { expect, it } from "vite-plus/test";
import {
  computeJustified,
  computeMonthLayout,
  groupPhotos,
  photoAspect,
  visibleRows,
} from "./photoGrid.js";
import { photo } from "./photo-test-fixtures.js";

it("fits panoramas and full justified rows within the available width", () => {
  const layout = computeJustified(
    [{ aspect: 20 }, ...Array.from({ length: 20 }, () => ({ aspect: 1.5 }))],
    { containerWidth: 800, targetRowHeight: 200 },
  );
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
    expect(rows.flatMap((row) => row.items).length).toBeLessThan(100);
    expect(rows.every((row) => row.y + row.height >= top && row.y <= top + 2000)).toBe(true);
  }
});

it("applies rotation to technical dimensions, prefers preview dimensions and falls back to a square", () => {
  const item = photo(1);
  expect(photoAspect(item)).toBe(1.5);
  item.rotation = 90;
  expect(photoAspect(item)).toBe(2 / 3);
  item.rotation = 0;
  item.previews.grid = { state: "ready", width: 400, height: 600 };
  expect(photoAspect(item)).toBe(2 / 3);
  item.previews.grid = { state: "missing" };
  item.height_px = null;
  expect(photoAspect(item)).toBe(1);
});

it("groups recorded months and capture sessions across appended pages", () => {
  const months = groupPhotos(
    [photo(3, "2024-12-31T23:30:00-12:00"), photo(1, "2025-01-01T00:30:00+14:00")],
    "months",
  );
  expect(months.map((group) => group.key)).toEqual(["2025-01", "2024-12"]);
  const firstPage = [photo(1, "2025-06-01T17:00:00"), photo(2, "2025-06-01T10:00:00")];
  const secondPage = [photo(3, "2025-06-01T09:00:00")];
  const groups = groupPhotos([...firstPage, ...secondPage], "sessions");
  expect(groups.map((group) => group.items.map((item) => item.asset_id))).toEqual([
    ["photo-1"],
    ["photo-2", "photo-3"],
  ]);
  const tied = [
    photo(4, "2025-06-01T17:00:00"),
    photo(1, "2025-06-01T17:00:00"),
    photo(2, "2025-06-01T10:00:00"),
    photo(3, "2025-06-01T10:00:00"),
  ];
  expect(groupPhotos(tied, "sessions").flatMap((group) => group.items)).toEqual(tied);
  const latestKey = groups[0].key;
  expect(
    groupPhotos(
      [
        ...firstPage,
        ...secondPage,
        photo(4, "2025-06-01T17:00:00"),
        photo(5, "2025-06-01T16:00:00"),
      ],
      "sessions",
    )[0].key,
  ).toBe(latestKey);
  expect(groupPhotos([photo(1, "2025-06-01"), photo(2, "2025-06-01T01")], "sessions")).toHaveLength(
    1,
  );
  const parsed = groupPhotos(
    [
      photo(1, "2025-06-01T9"),
      photo(2, "2025-06-01T9:30Z"),
      photo(3, "2025-06-01T9:45+00:00"),
      photo(4, "2025-06-01T10:00:00,123456789Z"),
      photo(5, "2025-06-01T10:00:00.123456789Z"),
    ],
    "sessions",
  );
  expect(parsed[0].items).toHaveLength(5);
  const invalid = [
    null,
    "invalid",
    "0000-01-02",
    "10000-01-02",
    "0001-01-01T00:00:00+01:00",
    "9999-12-31T23:00:00-02:00",
  ];
  const items = [
    ...invalid.map((capture, index) => photo(index, capture)),
    photo(6, "0001-01-01T01:00:00+01:00"),
    photo(7, "9999-12-31T22:00:00-01:00"),
  ];
  for (const grouping of ["months", "sessions"] as const) {
    const groups = groupPhotos(items, grouping);
    expect(groups.at(-1)).toMatchObject({
      key: "undated",
      year: "",
      items: items.slice(0, invalid.length),
    });
    expect(groups.slice(0, -1).flatMap((group) => group.items)).toHaveLength(2);
  }
});

it("keeps relevance order across months and undated photos", () => {
  const items = [photo(3), photo(1), photo(2)];
  items[0]!.capture_time = null;
  items[1]!.capture_time = "2020-01-01T12:00:00";
  items[2]!.capture_time = "2026-01-01T12:00:00";
  expect(groupPhotos(items, "relevance").flatMap((group) => group.items)).toEqual(items);
  expect(groupPhotos(items, "relevance")[0]!.year).toBe("");
});

it("labels a session by file modified time across the whole group, not just the seed item", () => {
  const modified = [photo(1), photo(2)];
  modified[0]!.capture_time = null;
  modified[0]!.file_modified_time = "2023-11-04T18:00:00Z";
  modified[1]!.capture_time = null;
  modified[1]!.file_modified_time = "2023-11-04T19:00:00Z";
  const modifiedGroups = groupPhotos(modified, "sessions");
  expect(modifiedGroups).toHaveLength(1);
  expect(modifiedGroups[0]!.label).toBe("2023-11-04 · File modified");

  const captured = groupPhotos(
    [photo(3, "2025-06-01T09:00:00"), photo(4, "2025-06-01T10:00:00")],
    "sessions",
  );
  expect(captured).toHaveLength(1);
  expect(captured[0]!.label).toBe("2025-06-01 · Capture session");

  // A raw capture_time can still hold a truthy-but-invalid placeholder even when
  // file_modified_time is what actually placed the photo; the label must key off
  // file_modified_time, not capture_time truthiness.
  const invalidCapture = [photo(5), photo(6)];
  invalidCapture[0]!.capture_time = "0000-01-01T00:00:00";
  invalidCapture[0]!.file_modified_time = "2024-02-02T08:00:00Z";
  invalidCapture[1]!.capture_time = "0000-01-01T00:00:00";
  invalidCapture[1]!.file_modified_time = "2024-02-02T09:00:00Z";
  const invalidGroups = groupPhotos(invalidCapture, "sessions");
  expect(invalidGroups).toHaveLength(1);
  expect(invalidGroups[0]!.label).toBe("2024-02-02 · File modified");

  // A window mixing a real capture with an mtime-placed photo is still a real session:
  // any genuinely captured photo makes the whole group a capture session.
  const mixed = [photo(7, "2025-06-01T09:00:00")];
  mixed[0]!.file_modified_time = null;
  const mtimeOnly = photo(8);
  mtimeOnly.capture_time = null;
  mtimeOnly.file_modified_time = "2025-06-01T10:00:00Z";
  const mixedGroups = groupPhotos([...mixed, mtimeOnly], "sessions");
  expect(mixedGroups).toHaveLength(1);
  expect(mixedGroups[0]!.items.map((item) => item.asset_id)).toEqual(["photo-8", "photo-7"]);
  expect(mixedGroups[0]!.label).toBe("2025-06-01 · Capture session");
});

it("places a row with only a file modified time in its month and keeps rows with neither undated", () => {
  const items = [photo(1), photo(2), photo(3)];
  items[0]!.capture_time = null;
  items[0]!.file_modified_time = "2023-11-04T18:22:03.5Z";
  items[1]!.capture_time = "2020-01-01T12:00:00";
  items[2]!.capture_time = null;
  const groups = groupPhotos(items, "months");
  expect(groups.map((group) => group.key)).toEqual(["2023-11", "2020-01", "undated"]);
  expect(groups[0]!.items).toEqual([items[0]]);
  expect(groups.at(-1)!.items).toEqual([items[2]]);
});
