import type { PhotoPreviewSlot } from "./generated/docbank.js";
import type { PhotoBrowseRow } from "./photoRows.js";

export type PhotoGridRow = Pick<
  PhotoBrowseRow,
  | "asset_id"
  | "name"
  | "capture_time"
  | "file_modified_time"
  | "width_px"
  | "height_px"
  | "rotation"
> & {
  stack?: PhotoBrowseRow["stack"];
  previews: { grid: PhotoPreviewSlot };
};
import { rotatedDimensions } from "./photoRotation.js";

export type LayoutItem = { aspect: number };

export type LayoutOptions = {
  containerWidth: number;
  targetRowHeight: number;
  gap?: number;
  maxRowHeight?: number;
};

export type Row = {
  y: number;
  height: number;
  items: { index: number; x: number; width: number }[];
};

export type Layout = { rows: Row[]; totalHeight: number };

export function computeJustified(items: LayoutItem[], opts: LayoutOptions): Layout {
  const gap = opts.gap ?? 4;
  const maxH = opts.maxRowHeight ?? Math.ceil(opts.targetRowHeight * 1.6);
  const rows: Row[] = [];
  let y = 0;

  let pending: { index: number; aspect: number }[] = [];
  let pendingAspectSum = 0;

  const flush = (forceRow: boolean) => {
    if (pending.length === 0) return;
    const budget = opts.containerWidth - gap * (pending.length - 1);
    let height = budget / pendingAspectSum;
    if (forceRow) {
      height = Math.min(height, opts.targetRowHeight);
    } else {
      // Raising a panorama's row height would overflow the container.
      height = Math.min(maxH, height);
    }
    let x = 0;
    const rowItems: Row["items"] = [];
    for (const p of pending) {
      const w = p.aspect * height;
      rowItems.push({ index: p.index, x, width: w });
      x += w + gap;
    }
    rows.push({ y, height, items: rowItems });
    y += height + gap;
    pending = [];
    pendingAspectSum = 0;
  };

  for (let i = 0; i < items.length; i++) {
    const aspect = Math.max(items[i]!.aspect, 0.05);
    pending.push({ index: i, aspect });
    pendingAspectSum += aspect;
    const widthIfPacked = pendingAspectSum * opts.targetRowHeight + gap * (pending.length - 1);
    if (widthIfPacked >= opts.containerWidth) {
      flush(false);
    }
  }
  flush(true);

  return { rows, totalHeight: rows.length === 0 ? 0 : y - gap };
}

export const HEADER_HEIGHT = 56;
export const STICKY_HEADER_HEIGHT = 44;
export const ROW_HEIGHTS = { compact: 140, comfortable: 200, large: 280 };
export type Density = keyof typeof ROW_HEIGHTS;
export type PhotoGroup = { key: string; label: string; year: string; items: PhotoGridRow[] };

export function photoAspect(photo: PhotoGridRow): number {
  const preview = photo.previews.grid;
  const width = preview.width ?? photo.width_px;
  const height = preview.height ?? photo.height_px;
  if (!width || !height || width <= 0 || height <= 0) return 1;
  const size = rotatedDimensions(width, height, photo.rotation);
  return size.width / size.height;
}

// The file's modified time places photos that carry no capture date.
export function photoDate(
  item: Pick<PhotoGridRow, "capture_time" | "file_modified_time">,
): string | null {
  return item.file_modified_time ?? item.capture_time ?? null;
}

export function monthLabel(
  key: string,
  format: Intl.DateTimeFormatOptions = { month: "long", year: "numeric" },
): string {
  return new Intl.DateTimeFormat(undefined, { ...format, timeZone: "UTC" }).format(
    new Date(`${key}-01T00:00:00Z`),
  );
}

// Floating EXIF dates retain their recorded clock time regardless of the browser's zone.
function captureClock(value: string): number {
  const match =
    /^(\d{4}-\d{2}-\d{2})(?:T(\d{1,2})(?::(\d{2}))?(?::(\d{2})([.,]\d+)?)?)?(Z|[+-]\d{2}:\d{2})?$/.exec(
      value,
    );
  if (!match) return NaN;
  const [, date, hour = "00", minute = "00", second = "00", fraction = "", zone = "Z"] = match;
  if (Number(date.slice(0, 4)) < 1) return NaN;
  const clock = Date.parse(
    `${date}T${hour.padStart(2, "0")}:${minute}:${second}${fraction.replace(",", ".")}${zone}`,
  );
  const utcYear = new Date(clock).getUTCFullYear();
  return utcYear >= 1 && utcYear <= 9999 ? clock : NaN;
}

export function groupPhotos(
  items: PhotoGridRow[],
  grouping: "months" | "sessions" | "flat" | "relevance",
): PhotoGroup[] {
  if (grouping === "flat")
    return items.length ? [{ key: "all", label: "Photos", year: "", items }] : [];
  if (grouping === "relevance")
    return items.length ? [{ key: grouping, label: "Search results", year: "", items }] : [];
  const clocks = new Map(
    items.map((item) => [item, photoDate(item) ? captureClock(photoDate(item)!) : NaN]),
  );
  const undated = items.filter((item) => !Number.isFinite(clocks.get(item)));
  const dated = items.filter((item) => Number.isFinite(clocks.get(item)));
  const groups: PhotoGroup[] = [];
  if (grouping === "months") {
    const byMonth = new Map<string, PhotoGroup>();
    for (const item of dated) {
      const key = photoMonth(item);
      let group = byMonth.get(key);
      if (!group) {
        group = { key, label: monthLabel(key), year: key.slice(0, 4), items: [] };
        byMonth.set(key, group);
        groups.push(group);
      }
      group.items.push(item);
    }
    groups.sort((a, b) => b.key.localeCompare(a.key));
  } else {
    const sorted = [...dated].sort((a, b) => clocks.get(a)! - clocks.get(b)!);
    let previous = -Infinity;
    for (const item of sorted) {
      const clock = clocks.get(item)!;
      if (clock - previous > 4 * 3600 * 1000) {
        groups.push({
          key: item.asset_id,
          label: "",
          year: photoDate(item)!.slice(0, 4),
          items: [],
        });
      }
      const group = groups[groups.length - 1];
      if (clock > previous) group.key = item.asset_id;
      group.items.push(item);
      previous = clock;
    }
    for (const group of groups) {
      // The seed item (still items[0] before the sort below reorders them) carries the date.
      const date = photoDate(group.items[0])!.slice(0, 10);
      group.items.sort((a, b) => clocks.get(b)! - clocks.get(a)!);
      // A group with any genuinely captured photo is a real capture session, even if an
      // mtime-placed photo lands in the same window; only an all-mtime group is file-modified.
      const label = group.items.every((item) => item.file_modified_time)
        ? "File modified"
        : "Capture session";
      group.label = `${date} · ${label}`;
    }
    groups.reverse();
  }
  if (undated.length) groups.push({ key: "undated", label: "Undated", year: "", items: undated });
  return groups;
}

export function computeMonthLayout(
  items: PhotoGridRow[],
  containerWidth: number,
  targetRowHeight: number,
  contentOffset = HEADER_HEIGHT,
) {
  const layout = computeJustified(
    items.map((item) => ({ aspect: photoAspect(item) })),
    { containerWidth, targetRowHeight },
  );
  return {
    ...layout,
    contentOffset,
    intrinsicHeight: items.length ? layout.totalHeight + contentOffset : 0,
  };
}

function rowBoundary(rows: Row[], top: number, after = false): number {
  let start = 0;
  let end = rows.length;
  while (start < end) {
    const mid = (start + end) >>> 1;
    const bottom = rows[mid].y + rows[mid].height;
    if (after ? bottom <= top : bottom < top) start = mid + 1;
    else end = mid;
  }
  return start;
}

export function visibleRows(rows: Row[], top: number, bottom: number): Row[] {
  const first = rowBoundary(rows, top);
  let start = first;
  while (start < rows.length && rows[start].y <= bottom) start++;
  return rows.slice(first, start);
}

export type PhotoChunk = {
  group: PhotoGroup;
  layout: ReturnType<typeof computeMonthLayout>;
  offset: number;
};

const photoMonth = (item?: PhotoGridRow) => (item && photoDate(item)?.slice(0, 7)) ?? "";

// layoutRow is the chunk's layout row holding the photo at index, if any.
export function layoutRow(chunk: PhotoChunk, index: number): Row | undefined {
  return chunk.layout.rows.find((row) => row.items.some((item) => item.index === index));
}

export function rowTop(chunk: PhotoChunk, row: Row): number {
  return chunk.offset + chunk.layout.contentOffset + row.y;
}

// firstRow locates the first matching visible photo. A section's first row starts at its heading.
function firstRow(chunks: PhotoChunk[], matches: (item: PhotoGridRow) => boolean) {
  for (const chunk of chunks.filter((chunk) => chunk.group.year)) {
    const row = layoutRow(chunk, chunk.group.items.findIndex(matches));
    if (row)
      return row === chunk.layout.rows[0]
        ? chunk.offset
        : rowTop(chunk, row) - STICKY_HEADER_HEIGHT;
  }
}

// seekOffset returns where the month's first photo or its collapsed stack sits. Photos page in UTC
// order but are filed by recorded date, so only a finished list can say it has none;
// then the first row of the nearest older calendar month stands in. Undefined means the
// caller should keep paging, or scroll to the end once the list has ended.
export function seekOffset(
  chunks: PhotoChunk[],
  month: string,
  ended = false,
  loadedItems: PhotoGridRow[] = [],
) {
  const own = firstRow(chunks, (item) => photoMonth(item) === month);
  if (own !== undefined) return own;
  const members = new Set(
    loadedItems.flatMap((row) => (row.stack && photoMonth(row) === month ? [row.asset_id] : [])),
  );
  const stack = firstRow(chunks, (item) => !!item.stack?.asset_ids.some((id) => members.has(id)));
  if (stack !== undefined) return stack;
  if (!ended) return;
  const months = chunks.flatMap(({ group }) => (group.year ? group.items.map(photoMonth) : []));
  const target = months.sort().findLast((found) => found <= month);
  return firstRow(chunks, (item) => photoMonth(item) === target);
}

// topPhoto is the first photo in the chunk's top visible row.
function topPhoto({ layout, group, offset }: PhotoChunk, top: number) {
  const row = layout.rows[rowBoundary(layout.rows, top - offset - layout.contentOffset, true)];
  return row && group.items[row.items[0]!.index];
}

// monthAt names the first photo visible below the sticky section heading in any order.
export function monthAt(chunks: PhotoChunk[], top: number): string {
  for (const chunk of chunks) {
    if (chunk.offset + chunk.layout.intrinsicHeight <= top) continue;
    const photo = topPhoto(
      chunk,
      top + (chunk.group.key === "relevance" ? 0 : STICKY_HEADER_HEIGHT),
    );
    if (photo) return photoMonth(photo);
  }
  return "";
}
