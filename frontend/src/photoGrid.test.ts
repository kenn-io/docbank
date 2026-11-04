import { expect, it } from "vite-plus/test";
import {
  computeJustified,
  createPhotoGrouper,
  createPhotoChunks,
  createPhotoSeek,
  computeMonthLayout,
  groupPhotos,
  monthAt,
  photoAspect,
  visibleRows,
  type PhotoGridRow,
} from "./photoGrid.js";
import { arrayView } from "./photoRows.js";
import { photo } from "./photo-test-fixtures.js";

const seekOffset = createPhotoSeek();

it("seeks an expanded 500-frame stack without walking member pairs", () => {
  let visits = 0;
  const assetIDs = Array.from({ length: 500 }, (_, id) => `photo-${id}`);
  for (let i = 0; i < assetIDs.length; i++) {
    const id = assetIDs[i];
    Object.defineProperty(assetIDs, i, { get: () => (visits++, id) });
  }
  const stack = { asset_ids: assetIDs, representative_id: "photo-0" };
  const items = Array.from({ length: 500 }, (_, id) => ({
    ...photo(id, "2025-06-01T12:00:00Z"),
    stack,
  }));
  const chunks = createPhotoChunks()(groupPhotos(items, "months"), 1000, 200);
  expect(createPhotoSeek()(chunks, "2025-06", false, items)).toBe(0);
  expect(visits).toBeLessThan(1000);
});

it("keeps bridge and long-burst representatives in capture order", () => {
  for (const [times, expected] of [
    [
      ["09:00:00", "01:00:00", "03:00:00", "07:00:00"],
      [["photo-0", "photo-3", "photo-2", "photo-1"]],
    ],
    [
      ["09:00:00", "09:00:01", "13:00:02", "13:00:01"],
      [["photo-2", "photo-3", "photo-1", "photo-0"]],
    ],
  ] as const) {
    const rows: PhotoGridRow[] = [];
    const grouper = createPhotoGrouper(),
      expanded = new Set<string>();
    for (const [i, time] of times.entries()) {
      rows.push(photo(i, `2025-06-01T${time}Z`));
      const groups = grouper.sync(arrayView(rows), "sessions", expanded);
      if (rows.length === times.length)
        expect(groups.map((group) => group.items.map((row) => row.asset_id))).toEqual(expected);
    }
  }
});

it("matches full grouping and exact layouts after shuffled pages and stack changes", () => {
  for (const grouping of ["months", "sessions", "flat", "relevance"] as const) {
    for (let seed = 1; seed <= 12; seed++) {
      let state = seed;
      const random = () => (state = (Math.imul(state, 1664525) + 1013904223) >>> 0) / 2 ** 32;
      const grouper = createPhotoGrouper(),
        build = createPhotoChunks();
      const rows: PhotoGridRow[] = [];
      let expanded = new Set<string>();
      for (let page = 0; page < 8; page++) {
        for (let i = 0; i < 17; i++) {
          const id = rows.length;
          const dates = [
            null,
            "invalid",
            "2025-01-01T00:30:00+14:00",
            "2024-12-31T23:30:00-12:00",
            `2025-06-01T${String(Math.floor(random() * 24)).padStart(2, "0")}:00:00Z`,
          ];
          const row = photo(id, dates[Math.floor(random() * dates.length)]);
          if (id % 7 === 0) row.file_modified_time = "2025-05-01T09:00:00Z";
          if (id < 2)
            row.stack = { asset_ids: ["photo-0", "photo-1"], representative_id: "photo-0" };
          rows.push(row);
        }
        if (page === 3) expanded = new Set(["photo-0"]);
        if (page === 6) expanded = new Set();
        const groups = grouper.sync(arrayView(rows), grouping, expanded);
        const expected = groupPhotos(
          rows.filter((row) => !row.stack || expanded.has("photo-0") || row.asset_id === "photo-0"),
          grouping,
        );
        expect([...groups]).toEqual(expected);
        const width = page < 4 ? 997 : 816;
        const chunks = build(groups, width, 173);
        let offset = 0;
        expect(chunks).toHaveLength(expected.length);
        for (const [i, group] of expected.entries()) {
          const layout = computeMonthLayout(
            group.items,
            width,
            173,
            grouping === "relevance" ? 12 : undefined,
          );
          expect(chunks[i].layout).toEqual(layout);
          expect(chunks[i].offset).toBe(offset);
          offset += layout.intrinsicHeight;
        }
      }
      // An independent replacement buffer takes the rebuild path.
      const replaced = rows.slice().reverse();
      expect([...grouper.sync(replaced, grouping, new Set(["photo-0"]))]).toEqual(
        groupPhotos(replaced, grouping),
      );
    }
  }
});

it.each(["months", "flat", "relevance"] as const)(
  "resumes %s without traversing old photos or finished layout rows",
  (grouping) => {
    const rows = Array.from({ length: 20_000 }, (_, id) => photo(id));
    const grouper = createPhotoGrouper(),
      build = createPhotoChunks(),
      expanded = new Set<string>();
    const first = build(grouper.sync(arrayView(rows), grouping, expanded), 1000, 200);
    let dates = 0,
      sizes = 0,
      finishedRows = 0;
    for (const row of rows) {
      const capture = row.capture_time,
        width = row.width_px;
      Object.defineProperty(row, "capture_time", {
        get: () => {
          dates++;
          return capture;
        },
      });
      Object.defineProperty(row, "width_px", {
        get: () => {
          sizes++;
          return width;
        },
      });
    }
    const packed = first[0].layout.rows;
    for (let i = 0; i < packed.length - 1; i++) {
      const row = packed[i];
      Object.defineProperty(packed, i, {
        configurable: true,
        get: () => {
          finishedRows++;
          return row;
        },
      });
    }
    rows.push(...Array.from({ length: 250 }, (_, i) => photo(20_000 + i)));
    const next = build(grouper.sync(arrayView(rows), grouping, expanded), 1000, 200);
    expect(dates).toBe(0);
    expect(sizes).toBeLessThanOrEqual(4);
    expect(finishedRows).toBe(0);
    expect(next[0].group.items).toHaveLength(20_250);
  },
);

it("touches only nearby sessions when one page spans years", () => {
  const rows = Array.from({ length: 20_000 }, (_, id) =>
    photo(id, new Date(Date.UTC(2025, 0, 1) - id * 24 * 3600_000).toISOString()),
  );
  const grouper = createPhotoGrouper(),
    build = createPhotoChunks(),
    expanded = new Set<string>();
  const first = grouper.sync(arrayView(rows), "sessions", expanded);
  build(first, 1000, 200);
  let reads = 0;
  for (const row of rows) {
    const date = row.capture_time,
      width = row.width_px;
    Object.defineProperty(row, "capture_time", {
      get: () => {
        reads++;
        return date;
      },
    });
    Object.defineProperty(row, "width_px", {
      get: () => {
        reads++;
        return width;
      },
    });
  }
  rows.push(photo(20_000, "2025-01-01T01:00:00Z"), photo(20_001, "1970-04-01T01:00:00Z"));
  const next = grouper.sync(arrayView(rows), "sessions", expanded);
  build(next, 1000, 200);
  expect(reads).toBeLessThan(25);
  expect(next[1]).toBe(first[1]);
});

it("appends older sessions without traversing the retained group prefix", () => {
  const rows = Array.from({ length: 20_000 }, (_, id) =>
    photo(id, new Date(Date.UTC(2025, 0, 1) - id * 5 * 3600_000).toISOString()),
  );
  const grouper = createPhotoGrouper(),
    build = createPhotoChunks(),
    expanded = new Set<string>();
  const groups = grouper.sync(arrayView(rows), "sessions", expanded);
  const chunks = build(groups, 1000, 200);
  let reads = 0;
  for (const group of groups) {
    const key = group.key;
    Object.defineProperty(group, "key", {
      get: () => {
        reads++;
        return key;
      },
    });
  }
  rows.push(photo(20_000, "2000-01-01T00:00:00Z"));
  const next = build(grouper.sync(arrayView(rows), "sessions", expanded), 1000, 200);
  expect(reads).toBeLessThan(10);
  expect(next[19_999]).toBe(chunks[19_999]);
  expect(next).toHaveLength(20_001);
});

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

it("publishes bounded array views with normal iteration, slicing, and concatenation", () => {
  const buffer = [1, 2],
    first = arrayView(buffer);
  buffer.push(3);
  const second = arrayView(buffer);
  expect(Array.isArray(first)).toBe(true);
  expect([...first]).toEqual([1, 2]);
  expect(first.slice()).toEqual([1, 2]);
  expect(first.map((n) => n * 2)).toEqual([2, 4]);
  expect(first.concat([4])).toEqual([1, 2, 4]);
  expect(JSON.stringify(first)).toBe("[1,2]");
  expect([...second]).toEqual([1, 2, 3]);
  expect(() => first.push(4)).toThrow();
});

it("resumes a dense session layout when an older page extends its item prefix", () => {
  const rows = Array.from({ length: 20_000 }, (_, id) => photo(id, "2025-06-01T12:00:00Z"));
  const grouper = createPhotoGrouper(),
    build = createPhotoChunks(),
    expanded = new Set<string>();
  build(grouper.sync(arrayView(rows), "sessions", expanded), 1000, 200);
  let sizes = 0;
  for (const row of rows) {
    const width = row.width_px;
    Object.defineProperty(row, "width_px", {
      get: () => {
        sizes++;
        return width;
      },
    });
  }
  rows.push(...Array.from({ length: 250 }, (_, i) => photo(20_000 + i, "2025-06-01T11:59:59Z")));
  const result = build(grouper.sync(arrayView(rows), "sessions", expanded), 1000, 200);
  expect(sizes).toBeLessThanOrEqual(4);
  expect(result[0].layout).toEqual(computeMonthLayout(rows, 1000, 200));
});

it("keeps pending date jumps equal to a fresh scan after regrouping and stack arrivals", () => {
  for (const mode of ["months", "sessions"] as const) {
    const rows: PhotoGridRow[] = [];
    const grouper = createPhotoGrouper(),
      build = createPhotoChunks(),
      expanded = new Set<string>();
    const seeks = Array.from({ length: 4 }, () => createPhotoSeek());
    const dates = [
      "2025-02-01T01:00:00Z",
      "2025-01-31T23:00:00Z",
      "2024-12-31T22:00:00Z",
      "2025-02-01T02:00:00Z",
    ];
    for (let page = 0; page < 12; page++) {
      const row = photo(page, dates[page % dates.length]);
      if (page < 2) row.stack = { asset_ids: ["photo-0", "photo-1"], representative_id: "photo-0" };
      rows.push(row);
      const chunks = build(grouper.sync(arrayView(rows), mode, expanded), 1000, 200);
      for (const [i, month] of ["2025-02", "2025-01", "2024-12", "2025-03"].entries()) {
        expect(seeks[i](chunks, month, page === 11, arrayView(rows))).toBe(
          createPhotoSeek()(chunks, month, page === 11, arrayView(rows)),
        );
      }
    }
  }
});

it("keeps the first matching seek row when a later page reorders a session", () => {
  const rows = Array.from({ length: 20 }, (_, id) => photo(id, "2025-01-01T00:00:00Z"));
  const grouper = createPhotoGrouper(),
    build = createPhotoChunks(),
    seek = createPhotoSeek(),
    expanded = new Set<string>();
  expect(
    seek(
      build(grouper.sync(arrayView(rows), "sessions", expanded), 1000, 200),
      "2025-01",
      false,
      arrayView(rows),
    ),
  ).toBe(0);
  rows.push(...Array.from({ length: 20 }, (_, i) => photo(20 + i, "2025-01-01T01:00:00Z")));
  expect(
    seek(
      build(grouper.sync(arrayView(rows), "sessions", expanded), 1000, 200),
      "2025-01",
      false,
      arrayView(rows),
    ),
  ).toBe(0);
});
