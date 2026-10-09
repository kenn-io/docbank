import { normalizeCaptureDate, type Query } from "./query.js";
import type { CaptureDayFacet } from "./snapshots.js";

export type { CaptureDayFacet } from "./snapshots.js";
export interface TimelineMonth { key: string; count: number; days: { key: string; count: number }[] }
export interface TimelineYear { key: string; count: number; months: TimelineMonth[] }

export function timelineYears(facet: CaptureDayFacet): TimelineYear[] {
  const years = new Map<string, TimelineYear>();
  for (const day of facet.values) {
    const yearKey = day.key.slice(0, 4);
    let year = years.get(yearKey);
    if (!year) { year = { key: yearKey, count: 0, months: [] }; years.set(yearKey, year); }
    const monthKey = day.key.slice(0, 7);
    let month = year.months.at(-1);
    if (month?.key !== monthKey) { month = { key: monthKey, count: 0, days: [] }; year.months.push(month); }
    year.count += day.count;
    month.count += day.count;
    month.days.push({ key: day.key, count: day.count });
  }
  return [...years.values()];
}

export function captureDateQuery(base: Query, date?: string): Query {
  if (!date) return { ...base, filters: {} };
  const after = normalizeCaptureDate(date.length === 4 ? `${date}-01-01` : date.length === 7 ? `${date}-01` : date)!;
  const end = new Date(`${after}T00:00:00Z`);
  if (date.length === 4) end.setUTCFullYear(end.getUTCFullYear() + 1);
  else if (date.length === 7) end.setUTCMonth(end.getUTCMonth() + 1);
  else end.setUTCDate(end.getUTCDate() + 1);
  const before = end.getUTCFullYear() <= 9999 ? end.toISOString().slice(0, 10) : undefined;
  return { ...base, filters: { capture_after: after, ...(before ? { capture_before: before } : {}) } };
}
