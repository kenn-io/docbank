import type { CaptureDayFacet } from "./snapshots.js";

export type { CaptureDayFacet } from "./snapshots.js";
export interface TimelineMonth {
  key: string;
  count: number;
}
export interface TimelineYear {
  key: string;
  count: number;
  months: TimelineMonth[];
}

export function timelineYears(facet: CaptureDayFacet): TimelineYear[] {
  const years = new Map<string, TimelineYear>();
  for (const day of facet.values) {
    const yearKey = day.key.slice(0, 4);
    let year = years.get(yearKey);
    if (!year) {
      year = { key: yearKey, count: 0, months: [] };
      years.set(yearKey, year);
    }
    const monthKey = day.key.slice(0, 7);
    let month = year.months.at(-1);
    if (month?.key !== monthKey) {
      month = { key: monthKey, count: 0 };
      year.months.push(month);
    }
    year.count += day.count;
    month.count += day.count;
  }
  return [...years.values()];
}

// timelineProblem says why counts could not be shown, from an unavailable facet's reason or a
// refusal's error code.
export function timelineProblem(reason?: string): string {
  if (reason === "snapshot_busy" || reason === "snapshot_capacity")
    return "Timeline is busy. Retry in a moment.";
  if (reason === "time_budget_exceeded") return "Timeline took too long. Try again.";
  if (reason === "member_budget_exceeded") return "Too many photos for timeline counts.";
  if (reason === "snapshot_too_large") return "Timeline exceeds its limits.";
  return "Timeline counts are unavailable.";
}
