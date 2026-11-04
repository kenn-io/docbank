import type { PhotoPreviewSlot } from "./generated/docbank.js";
import type { PhotoBrowseRow } from "./photoRows.js";

export type PhotoGridRow = Pick<
  PhotoBrowseRow,
  | "matched_ai_suggestion"
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
  return justify(items.length, (i) => items[i]!.aspect, opts);
}

function justify(
  count: number,
  aspectAt: (index: number) => number,
  opts: LayoutOptions,
  prev?: Layout,
): Layout {
  const gap = opts.gap ?? 4;
  const maxH = opts.maxRowHeight ?? Math.ceil(opts.targetRowHeight * 1.6);
  const rows = prev?.rows ?? [];
  const last = rows.pop();
  const start = last?.items[0]?.index ?? 0;
  let y = last?.y ?? 0;

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

  for (let i = start; i < count; i++) {
    const aspect = Math.max(aspectAt(i), 0.05);
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

const clocks = new WeakMap<PhotoGridRow, number>();
function clockOf(row: PhotoGridRow): number {
  if (!clocks.has(row)) {
    const value = photoDate(row);
    clocks.set(row, value ? captureClock(value) : NaN);
  }
  return clocks.get(row)!;
}

export function stackHidden(
  row: { asset_id: string; stack?: PhotoBrowseRow["stack"] },
  expanded: ReadonlySet<string>,
): boolean {
  return (
    !!row.stack &&
    !expanded.has(row.stack.asset_ids[0]!) &&
    row.asset_id !== row.stack.representative_id
  );
}

export function groupPhotos(
  items: PhotoGridRow[],
  grouping: "months" | "sessions" | "flat" | "relevance",
): PhotoGroup[] {
  if (grouping === "flat")
    return items.length ? [{ key: "all", label: "Photos", year: "", items }] : [];
  if (grouping === "relevance")
    return items.length ? [{ key: grouping, label: "Search results", year: "", items }] : [];
  const undated = items.filter((item) => !Number.isFinite(clockOf(item)));
  const dated = items.filter((item) => Number.isFinite(clockOf(item)));
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
    const sorted = [...dated].sort((a, b) => clockOf(a) - clockOf(b));
    let previous = -Infinity;
    for (const item of sorted) {
      const clock = clockOf(item);
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
      group.items.sort((a, b) => clockOf(b) - clockOf(a));
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

type Grouping = "months" | "sessions" | "flat" | "relevance";

// Returns the first index where before no longer holds.
function boundary(count: number, before: (index: number) => boolean): number {
  let lo = 0,
    hi = count;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (before(mid)) lo = mid + 1;
    else hi = mid;
  }
  return lo;
}

export function createPhotoGrouper() {
  let source: PhotoGridRow[] | undefined,
    consumed = 0;
  let mode: Grouping | undefined, expanded: ReadonlySet<string> | undefined;
  let groups: PhotoGroup[] = [];
  let byKey = new Map<string, PhotoGroup>();
  return {
    sync(
      rows: PhotoGridRow[],
      grouping: Grouping,
      nextExpanded: ReadonlySet<string>,
      appendedFrom?: PhotoGridRow[],
    ): { groups: PhotoGroup[]; changedFrom: number } {
      const append =
        source !== undefined &&
        consumed <= rows.length &&
        mode === grouping &&
        expanded === nextExpanded &&
        source === appendedFrom;
      let changedFrom = append ? groups.length : 0;
      const added = (append ? rows.slice(consumed) : rows).filter(
        (row) => !stackHidden(row, nextExpanded),
      );
      source = rows;
      consumed = rows.length;
      mode = grouping;
      expanded = nextExpanded;
      if (!append) {
        groups = groupPhotos(added, grouping);
        byKey = new Map();
        for (const group of groups)
          if (grouping !== "sessions" || group.key === "undated") byKey.set(group.key, group);
      } else {
        const merge = (page: PhotoGroup[]) => {
          for (const part of page) {
            const old = byKey.get(part.key);
            const index = boundary(
              groups.length,
              (i) =>
                groups[i].key !== "undated" &&
                (part.key === "undated" || groups[i].key.localeCompare(part.key) > 0),
            );
            changedFrom = Math.min(changedFrom, index);
            if (old) {
              for (const item of part.items) old.items.push(item);
              const next = { ...old };
              groups[index] = next;
              byKey.set(part.key, next);
            } else {
              groups.splice(index, 0, part);
              byKey.set(part.key, part);
            }
          }
        };
        if (grouping !== "sessions") merge(groupPhotos(added, grouping));
        else {
          const page = groupPhotos(added, "sessions");
          // Each page session is a separate clock cluster. A wide page touches only
          // nearby retained sessions, even when Imported order spans many years.
          for (const part of page) {
            if (part.key === "undated") {
              merge([part]);
              continue;
            }
            const hi = clockOf(part.items[0]),
              lo = clockOf(part.items.at(-1)!);
            const gap = 4 * 3600 * 1000;
            const endOfDated = groups.length - (groups.at(-1)?.key === "undated" ? 1 : 0);
            const start = boundary(endOfDated, (i) => clockOf(groups[i].items.at(-1)!) > hi + gap);
            const end = boundary(endOfDated, (i) => clockOf(groups[i].items[0]) >= lo - gap);
            const retained = groups.slice(start, end).flatMap((group) => group.items);
            const regrouped = groupPhotos([...retained, ...part.items], "sessions");
            for (let i = 0; i < regrouped.length; i++) {
              const next = regrouped[i],
                old = groups[start + i];
              if (
                old &&
                old.items.length <= next.items.length &&
                old.items.every((item, index) => item === next.items[index])
              ) {
                for (let j = old.items.length; j < next.items.length; j++)
                  old.items.push(next.items[j]);
                next.items = old.items;
              }
            }
            changedFrom = Math.min(changedFrom, start);
            groups = groups.slice(0, start).concat(regrouped, groups.slice(end));
          }
        }
      }
      return { groups: groups.slice(), changedFrom };
    },
  };
}

export function computeMonthLayout(
  items: PhotoGridRow[],
  containerWidth: number,
  targetRowHeight: number,
  contentOffset = HEADER_HEIGHT,
  prev?: { layout: Layout; count: number },
) {
  const layout = justify(
    items.length,
    (i) => photoAspect(items[i]),
    { containerWidth, targetRowHeight },
    prev && prev.count < items.length ? prev.layout : undefined,
  );
  return {
    ...layout,
    contentOffset,
    intrinsicHeight: items.length ? layout.totalHeight + contentOffset : 0,
  };
}

function rowBoundary(rows: Row[], top: number, after = false): number {
  return boundary(rows.length, (i) => {
    const bottom = rows[i].y + rows[i].height;
    return after ? bottom <= top : bottom < top;
  });
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

export function createPhotoChunks() {
  let chunks: PhotoChunk[] = [];
  let width = 0,
    height = 0;
  let cache = new WeakMap<
    PhotoGridRow[],
    { count: number; layout: ReturnType<typeof computeMonthLayout> }
  >();
  return (
    groups: PhotoGroup[],
    containerWidth: number,
    targetRowHeight: number,
    changedFrom = 0,
  ): { chunks: PhotoChunk[]; changedFrom: number } => {
    const reset = width !== containerWidth || height !== targetRowHeight;
    if (reset) cache = new WeakMap();
    const start = reset ? 0 : changedFrom;
    let offset = start ? chunks[start - 1].offset + chunks[start - 1].layout.intrinsicHeight : 0;
    chunks.length = start;
    for (let i = start; i < groups.length; i++) {
      const group = groups[i];
      const previous = cache.get(group.items);
      const contentOffset = group.key === "relevance" ? 12 : HEADER_HEIGHT;
      const layout =
        previous?.count === group.items.length
          ? previous.layout
          : computeMonthLayout(
              group.items,
              containerWidth,
              targetRowHeight,
              contentOffset,
              previous,
            );
      cache.set(group.items, { count: group.items.length, layout });
      chunks.push({ group, layout, offset });
      offset += layout.intrinsicHeight;
    }
    width = containerWidth;
    height = targetRowHeight;
    return { chunks: chunks.slice(), changedFrom: start };
  };
}

export function chunkAt(chunks: PhotoChunk[], top: number): number {
  return boundary(chunks.length, (i) => chunks[i].offset + chunks[i].layout.intrinsicHeight < top);
}

const photoMonth = (item?: PhotoGridRow) => (item && photoDate(item)?.slice(0, 7)) ?? "";

// layoutRow is the chunk's layout row holding the photo at index, if any.
export function layoutRow(chunk: PhotoChunk, index: number): Row | undefined {
  if (index < 0) return;
  const rows = chunk.layout.rows;
  const found = boundary(rows.length, (i) => rows[i].items.at(-1)!.index < index);
  return rows[found];
}

export function rowTop(chunk: PhotoChunk, row: Row): number {
  return chunk.offset + chunk.layout.contentOffset + row.y;
}

// Keep a pending jump's scan position across appended pages. Replacements start a new scan.
export function createPhotoSeek() {
  let source: PhotoChunk[] | undefined, loadedSource: PhotoGridRow[] | undefined;
  let target = "",
    consumed = 0;
  type Scan = { chunk: PhotoChunk; count: number };
  let scanned = new WeakMap<PhotoGridRow[], Scan>();
  type Location = { scan: Scan; index: number };
  let locations = new Map<string, Location>();
  let representatives = new Set<string>();
  let own: string | undefined, stack: string | undefined, older: string | undefined;
  let olderMonth = "";
  return (
    chunks: PhotoChunk[],
    month: string,
    ended = false,
    loadedItems: PhotoGridRow[] = [],
    appendedFrom?: PhotoGridRow[],
    start = 0,
  ) => {
    const reset =
      !loadedSource ||
      (loadedSource !== loadedItems && loadedSource !== appendedFrom) ||
      (source && start === 0 && source[0]?.group.items !== chunks[0]?.group.items) ||
      (consumed === loadedItems.length && start < (source?.length ?? 0)) ||
      consumed > loadedItems.length ||
      target !== month;
    if (reset) {
      scanned = new WeakMap();
      locations = new Map();
      representatives = new Set();
      own = stack = older = undefined;
      olderMonth = "";
      consumed = 0;
    }
    source = chunks;
    loadedSource = loadedItems;
    target = month;
    const earlier = (a: string | undefined, b: string): string => {
      if (!a) return b;
      const left = locations.get(a)!,
        right = locations.get(b)!;
      return right.scan.chunk.offset < left.scan.chunk.offset ||
        (right.scan.chunk.offset === left.scan.chunk.offset && right.index < left.index)
        ? b
        : a;
    };
    if (reset) start = 0;
    for (let c = start; c < chunks.length; c++) {
      const chunk = chunks[c],
        items = chunk.group.items;
      if (!chunk.group.year) continue;
      let scan = scanned.get(items);
      if (!scan) scanned.set(items, (scan = { chunk, count: 0 }));
      scan.chunk = chunk;
      // Regrouping can move an earlier candidate. Update positions before comparing them.
      for (let i = scan.count; i < items.length; i++)
        locations.set(items[i].asset_id, { scan, index: i });
    }
    for (let i = consumed; i < loadedItems.length; i++) {
      const row = loadedItems[i];
      if (!row.stack || photoMonth(row) !== month) continue;
      const id = row.stack.representative_id;
      representatives.add(id);
      if (locations.has(id)) stack = earlier(stack, id);
    }
    consumed = loadedItems.length;
    for (let c = start; c < chunks.length; c++) {
      const chunk = chunks[c],
        items = chunk.group.items;
      if (!chunk.group.year) continue;
      const scan = scanned.get(items)!;
      const count = scan.count;
      for (let i = count; i < items.length; i++) {
        const row = items[i],
          found = photoMonth(row),
          id = row.asset_id;
        if (found === month) own = earlier(own, id);
        if (found && found <= month && found >= olderMonth) {
          older = found > olderMonth ? id : earlier(older, id);
          olderMonth = found;
        }
        if (representatives.has(id)) stack = earlier(stack, id);
      }
      scan.count = items.length;
    }
    const id = own ?? stack ?? (ended ? older : undefined);
    if (!id) return;
    const { scan, index } = locations.get(id)!;
    const chunk = scan.chunk;
    const row = layoutRow(chunk, index);
    if (row)
      return row === chunk.layout.rows[0]
        ? chunk.offset
        : rowTop(chunk, row) - STICKY_HEADER_HEIGHT;
  };
}

// topPhoto is the first photo in the chunk's top visible row.
function topPhoto({ layout, group, offset }: PhotoChunk, top: number) {
  const row = layout.rows[rowBoundary(layout.rows, top - offset - layout.contentOffset, true)];
  return row && group.items[row.items[0]!.index];
}

// monthAt names the first photo visible below the sticky section heading in any order.
export function monthAt(chunks: PhotoChunk[], top: number): string {
  for (let i = chunkAt(chunks, top); i < chunks.length; i++) {
    const chunk = chunks[i];
    if (chunk.offset + chunk.layout.intrinsicHeight <= top) continue;
    const photo = topPhoto(
      chunk,
      top + (chunk.group.key === "relevance" ? 0 : STICKY_HEADER_HEIGHT),
    );
    if (photo) return photoMonth(photo);
  }
  return "";
}
