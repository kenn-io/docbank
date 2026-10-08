import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import PhotoTimeline from "./PhotoTimeline.svelte";
import { parseQuery } from "./query.js";
import { captureDayQuery, nextCaptureDay, timelineYears } from "./photoTimeline.js";

afterEach(cleanup);

it("uses recorded days and full counts for years and months", () => {
  const years = timelineYears({ dimension: "capture_day", available: true, total: 904, missing: 4, other: 0, values: [
    { key: "2024-02-29", label: "2024-02-29", count: 300, selected: false },
    { key: "2025-01-01", label: "2025-01-01", count: 500, selected: false },
    { key: "2024-02-28", label: "2024-02-28", count: 100, selected: false },
  ] });
  expect(years.map(year => [year.key, year.count])).toEqual([["2025", 500], ["2024", 400]]);
  expect(years[1].months[0].days.map(day => day.key)).toEqual(["2024-02-29", "2024-02-28"]);
  expect(years[1].months[0].count).toBe(400);
});

it("bounds leap days and the final supported day without a timezone shift", () => {
  expect(nextCaptureDay("2024-02-29")).toBe("2024-03-01");
  expect(nextCaptureDay("2025-12-31")).toBe("2026-01-01");
  expect(nextCaptureDay("0000-02-29")).toBe("0000-03-01");
  expect(nextCaptureDay("9999-12-31")).toBeUndefined();
  const base = parseQuery("{}");
  expect(captureDayQuery(base, "2024-02-29").filters).toEqual({ capture_after: "2024-02-29", capture_before: "2024-03-01" });
  expect(captureDayQuery(base).filters).toEqual(base.filters);
  expect(captureDayQuery(parseQuery("{}"), "9999-12-31").filters).toEqual({ capture_after: "9999-12-31" });
});

it("shows full-scope year density and only the focused month's day rows", async () => {
  const onselect = vi.fn();
  render(PhotoTimeline, { loading: false, error: "", onselect, onretry: vi.fn(), facet: {
    dimension: "capture_day", available: true, total: 905, missing: 5, other: 0, values: [
      { key: "2025-01-01", label: "2025-01-01", count: 500, selected: false },
      { key: "2024-02-29", label: "2024-02-29", count: 400, selected: false },
    ],
  } });
  expect(screen.getByText("905 photos in scope · 5 undated")).toBeTruthy();
  const years = screen.getByRole("navigation", { name: "Timeline years" });
  await fireEvent.click([...years.querySelectorAll("button")][1]);
  const day = screen.getByRole("button", { name: "2024-02-29 · 400 photos" });
  expect(screen.queryByRole("button", { name: "2025-01-01 · 500 photos" })).toBeNull();
  await fireEvent.click(day);
  expect(onselect).toHaveBeenCalledWith("2024-02-29");
  expect(screen.getByRole("navigation", { name: "Timeline month scrubber" })).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Choose capture day 2024-02-29" }));
  expect(onselect).toHaveBeenCalledTimes(2);
});

it("shows undated and unavailable scopes honestly", async () => {
  const view = render(PhotoTimeline, { loading: false, error: "", onselect: vi.fn(), onretry: vi.fn(), facet: { dimension: "capture_day", available: true, total: 10, missing: 10, other: 0, values: [] } });
  expect(screen.getByText("These photos have no recorded capture dates.")).toBeTruthy();
  await view.rerender({ loading: false, error: "", onselect: vi.fn(), onretry: vi.fn(), facet: { dimension: "capture_day", available: false, reason: "time_budget_exceeded", values: [] } });
  expect(screen.getByText(/Timeline exceeds its limits.*time budget exceeded/)).toBeTruthy();
  expect(screen.queryByRole("navigation", { name: "Timeline years" })).toBeNull();
});
it.each([ ["snapshot_busy", /Timeline is busy.*snapshot busy/], ["snapshot_capacity", /Timeline is busy.*snapshot capacity/], ["snapshot_too_large", /Timeline exceeds its limits.*snapshot too large/] ])("shows %s with its retry reason", async (reason, message) => {
  const retry = vi.fn();
  render(PhotoTimeline, { loading: false, error: "", onselect: vi.fn(), onretry: retry, facet: { dimension: "capture_day", available: false, reason: String(reason), values: [] } });
  expect(screen.getByText(message)).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Retry timeline" })); expect(retry).toHaveBeenCalledTimes(1);
});
