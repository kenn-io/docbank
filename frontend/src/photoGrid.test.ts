import { expect, it } from "vite-plus/test";
import {
  computeJustified,
  computeMonthLayout,
  groupPhotos,
  monthAt,
  photoAspect,
  seekOffset,
  visibleRows,
  type PhotoGridRow,
} from "./photoGrid.js";
import { photo } from "./photo-test-fixtures.js";

// Lays out groups at 144px wide, so every 3:2 photo gets its own 96px row.
function chunks(groups: { key: string; year: string; items: PhotoGridRow[] }[]) {
  let offset = 0;
  return groups.map((group) => {
    const layout = computeMonthLayout(
      group.items,
      144,
      96,
      group.key === "relevance" ? 12 : undefined,
    );
    const chunk = { group: { label: group.key, ...group }, layout, offset };
    offset += layout.intrinsicHeight;
    return chunk;
  });
}

it("seeks a month's offset across month, session, and stacked layouts", () => {
  const months = chunks([
    { key: "2025-06", year: "2025", items: [1, 2, 3].map((id) => photo(id, "2025-06-01")) },
    { key: "2024-01", year: "2024", items: [photo(4, "2024-01-01")] },
    { key: "undated", year: "", items: [photo(5, null)] },
  ]);
  expect(seekOffset(months, "2025-06")).toBe(0);
  expect(seekOffset(months, "2024-01")).toBe(months[1].offset);
  // A month absent from the loaded rows may still be on a later page.
  expect(seekOffset(months, "2025-03")).toBeUndefined();
  // Once the list has ended, the first section whose newest photo is older stands in.
  expect(seekOffset(months, "2025-03", true)).toBe(months[1].offset);
  // Session order follows UTC, while the nearest older month follows the recorded date.
  const sessions = chunks(
    groupPhotos(
      [
        photo(6, "2025-04-01T00:00:00Z"),
        photo(7, "2025-01-31T23:30:00-05:00"),
        photo(8, "2025-02-01T00:30:00+01:00"),
        photo(9, "2024-12-01T12:00:00Z"),
      ],
      "sessions",
    ),
  );
  // April and January each occupy a 56px heading and one 96px row.
  expect(seekOffset(sessions, "2025-03", true)).toBe(304);
  // With nothing older, the caller scrolls to the end.
  expect(seekOffset(months, "2023-05", true)).toBeUndefined();

  {
    const session = chunks(
      groupPhotos(
        [
          photo(1, "2025-01-01T04:00:00"),
          photo(2, "2025-01-01T03:00:00"),
          photo(3, "2025-01-01T02:00:00"),
          photo(4, "2025-01-01T01:00:00"),
          photo(5, "2025-01-01T00:00:00"),
          photo(6, "2024-12-31T23:00:00"),
        ],
        "sessions",
      ),
    );
    expect(session).toHaveLength(1);
    // The December row starts at 556px and must sit below the 44px sticky heading.
    const target = seekOffset(session, "2024-12")!;
    expect(target).toBe(512);
    expect(monthAt(session, target)).toBe("2024-12");
    expect(seekOffset(session, "2025-01")).toBe(0);
  }

  {
    const items = Array.from({ length: 100 }, (_, index) => photo(index, "2025-01-01"));
    const sections = chunks(groupPhotos(items, "months"));
    let reads = 0;
    for (const item of items) {
      const capture = item.capture_time;
      Object.defineProperty(item, "capture_time", {
        get() {
          reads++;
          return capture;
        },
      });
    }
    expect(seekOffset(sections, "2024-12", false, items)).toBeUndefined();
    expect(reads).toBeLessThanOrEqual(100);
  }

  {
    const stack = { asset_ids: ["photo-1", "photo-2"], representative_id: "photo-1" };
    const items = [
      photo(3, "2025-02-01"),
      { ...photo(1, "2025-01-01T00:00:00"), stack },
      { ...photo(2, "2024-12-31T23:59:59"), stack },
      photo(4, "2024-11-01"),
    ];
    const sections = chunks(
      groupPhotos(
        items.filter((item) => item.asset_id !== "photo-2"),
        "months",
      ),
    );
    expect(seekOffset(sections, "2024-12", true, items)).toBe(152);
    expect(seekOffset(sections, "2024-12", false, items)).toBe(152);
    const visible = chunks(
      groupPhotos(
        [...items.filter((item) => item.asset_id !== "photo-2"), photo(5, "2024-12-01")],
        "months",
      ),
    );
    expect(seekOffset(visible, "2024-12", false, items)).toBe(304);
    const expanded = chunks(groupPhotos(items, "months"));
    expect(seekOffset(expanded, "2024-12", true, items)).toBe(304);
    const sessionItems = [photo(3, "2025-01-01T00:01:00"), ...items.slice(1)];
    const sessions = chunks(
      groupPhotos(
        sessionItems.filter((item) => item.asset_id !== "photo-2"),
        "sessions",
      ),
    );
    expect(seekOffset(sessions, "2024-12", true, sessionItems)).toBe(112);
  }
});

it("names the top visible photo's month across orders with bounded row reads", () => {
  const sections = chunks(
    groupPhotos(
      [
        photo(1, "2025-01-01T01:00:00"),
        photo(2, "2024-12-31T23:00:00"),
        photo(3, "2024-06-01"),
        photo(4, null),
      ],
      "sessions",
    ),
  );
  expect(monthAt(sections, 0)).toBe("2025-01");
  expect(monthAt(sections, 156)).toBe("2024-12");
  expect(monthAt(sections, 107)).toBe("2025-01");
  expect(monthAt(sections, 108)).toBe("2024-12");
  expect(monthAt(sections, sections[1].offset - 1)).toBe("2024-06");
  expect(monthAt(sections, sections[1].offset)).toBe("2024-06");
  expect(monthAt(sections, sections[2].offset)).toBe("");
  for (const [grouping, second, third] of [
    ["flat", 156, 256],
    ["relevance", 112, 212],
  ] as const) {
    const rows = chunks(
      groupPhotos([photo(1, "2025-06-01"), photo(2, "2024-03-01"), photo(3, null)], grouping),
    );
    expect(monthAt(rows, 0)).toBe("2025-06");
    expect(monthAt(rows, second)).toBe("2024-03");
    expect(monthAt(rows, third)).toBe("");
  }

  const items = Array.from({ length: 10_000 }, (_, index) =>
    photo(
      index,
      `${2025 - Math.floor(index / 1200)}-${String(12 - (Math.floor(index / 100) % 12)).padStart(2, "0")}-01`,
    ),
  );
  const [large] = chunks([{ key: "all", year: "", items }]);
  let reads = 0;
  large.layout.rows = large.layout.rows.map((row) => ({
    ...row,
    get y() {
      reads++;
      return row.y;
    },
  }));
  // Row 9,000 starts at 900,056px, below the 44px sticky heading.
  expect(monthAt([large], 900_012)).toBe("2018-06");
  expect(reads).toBeLessThan(30);
});

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

it.each([
  ["default", undefined, 56],
  ["explicit", 12, 12],
] as const)(
  "bounds mounted rows with the %s content offset and 10,000 photos",
  (_label, contentOffset, expectedOffset) => {
    expect(computeMonthLayout([], 800, 200).intrinsicHeight).toBe(0);
    const items = Array.from({ length: 10_000 }, (_, index) => photo(index));
    const layout = computeMonthLayout(items, 1200, 200, contentOffset);
    expect(layout.contentOffset).toBe(expectedOffset);
    expect(layout.intrinsicHeight).toBe(layout.totalHeight + expectedOffset);
    for (const top of [0, 10_000, 100_000, layout.totalHeight - 1000]) {
      const rows = visibleRows(layout.rows, top, top + 2000);
      expect(rows.flatMap((row) => row.items).length).toBeLessThan(100);
      expect(rows.every((row) => row.y + row.height >= top && row.y <= top + 2000)).toBe(true);
    }
  },
);

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
