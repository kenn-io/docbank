import type { PhotoBrowseRow } from "./generated/docbank.js";

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

export const HEADER_HEIGHT = 44;
export const ROW_HEIGHTS = { compact: 140, comfortable: 200, large: 280 };
export type Density = keyof typeof ROW_HEIGHTS;
export type PhotoGroup = { key: string; label: string; year: string; items: PhotoBrowseRow[] };

export function photoAspect(photo: PhotoBrowseRow): number {
  const preview = photo.previews.grid;
  const width = preview.width ?? photo.width_px;
  const height = preview.height ?? photo.height_px;
  return width && height && width > 0 && height > 0 ? width / height : 1;
}

function monthLabel(key: string): string {
  return new Intl.DateTimeFormat(undefined, { month: "long", year: "numeric", timeZone: "UTC" }).format(new Date(`${key}-01T00:00:00Z`));
}

// Floating EXIF dates retain their recorded clock time regardless of the browser's zone.
function captureClock(value: string): number {
  const match = /^(\d{4}-\d{2}-\d{2})(?:T(\d{1,2})(?::(\d{2}))?(?::(\d{2})([.,]\d+)?)?)?(Z|[+-]\d{2}:\d{2})?$/.exec(value);
  if (!match) return NaN;
  const [, date, hour = "00", minute = "00", second = "00", fraction = "", zone = "Z"] = match;
  return Date.parse(`${date}T${hour.padStart(2, "0")}:${minute}:${second}${fraction.replace(",", ".")}${zone}`);
}

export function groupPhotos(items: PhotoBrowseRow[], grouping: "months" | "sessions"): PhotoGroup[] {
  const undated = items.filter(item => !item.capture_time || !Number.isFinite(captureClock(item.capture_time)));
  const dated = items.filter(item => item.capture_time && Number.isFinite(captureClock(item.capture_time)));
  const groups: PhotoGroup[] = [];
  if (grouping === "months") {
    const byMonth = new Map<string, PhotoGroup>();
    for (const item of dated) {
      const key = item.capture_time!.slice(0, 7);
      let group = byMonth.get(key);
      if (!group) {
        group = { key, label: monthLabel(key), year: key.slice(0, 4), items: [] };
        byMonth.set(key, group);
        groups.push(group);
      }
      group.items.push(item);
    }
  } else {
    const sorted = [...dated].sort((a, b) => captureClock(a.capture_time!) - captureClock(b.capture_time!));
    let previous = -Infinity;
    for (const item of sorted) {
      const clock = captureClock(item.capture_time!);
      if (clock - previous > 4 * 3600 * 1000) {
        groups.push({ key: item.asset_id, label: `${item.capture_time!.slice(0, 10)} · Capture session`, year: item.capture_time!.slice(0, 4), items: [] });
      }
      groups[groups.length - 1].items.push(item);
      previous = clock;
    }
    groups.reverse();
  }
  if (undated.length) groups.push({ key: "undated", label: "Undated", year: "", items: undated });
  return groups;
}

export function computeMonthLayout(items: PhotoBrowseRow[], containerWidth: number, targetRowHeight: number) {
  const layout = computeJustified(items.map(item => ({ aspect: photoAspect(item) })), { containerWidth, targetRowHeight });
  return { ...layout, intrinsicHeight: items.length ? layout.totalHeight + HEADER_HEIGHT : 0 };
}

export function visibleRows(rows: Row[], top: number, bottom: number): Row[] {
  let start = 0;
  let end = rows.length;
  while (start < end) {
    const mid = (start + end) >>> 1;
    if (rows[mid].y + rows[mid].height < top) start = mid + 1;
    else end = mid;
  }
  const first = start;
  while (start < rows.length && rows[start].y <= bottom) start++;
  return rows.slice(first, start);
}
