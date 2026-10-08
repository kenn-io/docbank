import type { Query } from "./query.js";
import type { WorkspaceQueryResponse } from "./snapshots.js";

export type CaptureDayFacet = WorkspaceQueryResponse["facets"][number];
export interface TimelineMonth { key: string; count: number; days: { key: string; count: number }[] }
export interface TimelineYear { key: string; count: number; months: TimelineMonth[] }

export function timelineYears(facet: CaptureDayFacet): TimelineYear[] {
  const years = new Map<string, TimelineYear>();
  for (const day of [...facet.values].sort((a, b) => b.key.localeCompare(a.key))) {
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

export function nextCaptureDay(day: string): string | undefined {
  const date = new Date(`${day}T00:00:00Z`);
  if (day === "9999-12-31") return undefined;
  date.setUTCDate(date.getUTCDate() + 1);
  return date.toISOString().slice(0, 10);
}

export function captureDayQuery(base: Query, day?: string): Query {
  if (!day) return { ...base, filters: { ...base.filters } };
  const before = nextCaptureDay(day);
  return { ...base, filters: {
    ...base.filters,
    capture_after: base.filters.capture_after && base.filters.capture_after > day ? base.filters.capture_after : day,
    ...(before ? { capture_before: base.filters.capture_before && base.filters.capture_before < before ? base.filters.capture_before : before } : {}),
  } };
}
