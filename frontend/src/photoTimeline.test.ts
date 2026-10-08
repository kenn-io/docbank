import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import PhotoTimeline from "./PhotoTimeline.svelte";
import { parseQuery } from "./query.js";
import { captureDateQuery, timelineYears } from "./photoTimeline.js";

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
  const base = parseQuery("{}");
  expect(captureDateQuery(base, "2025").filters).toEqual({ capture_after: "2025-01-01", capture_before: "2026-01-01" });
  expect(captureDateQuery(base, "2025-12").filters).toEqual({ capture_after: "2025-12-01", capture_before: "2026-01-01" });
  expect(captureDateQuery(base, "0001").filters).toEqual({ capture_after: "0001-01-01", capture_before: "0002-01-01" });
  expect(captureDateQuery(base, "9999").filters).toEqual({ capture_after: "9999-01-01" });
  expect(captureDateQuery(base, "9999-12").filters).toEqual({ capture_after: "9999-12-01" });
  expect(captureDateQuery(base, "2024-02-29").filters).toEqual({ capture_after: "2024-02-29", capture_before: "2024-03-01" });
  expect(captureDateQuery(base).filters).toEqual(base.filters);
  expect(captureDateQuery(parseQuery("{}"), "9999-12-31").filters).toEqual({ capture_after: "9999-12-31" });
});

it("shows full-scope year density and only the focused month's day rows", async () => {
  const onselect = vi.fn();
  const view = render(PhotoTimeline, { loading: false, error: "", onselect, onretry: vi.fn(), facet: {
    dimension: "capture_day", available: true, total: 905, missing: 5, other: 0, values: [
      { key: "2025-01-01", label: "2025-01-01", count: 500, selected: false },
      { key: "2024-02-29", label: "2024-02-29", count: 400, selected: false },
    ],
  } });
  expect(screen.getByText("905 photos in scope · 5 undated")).toBeTruthy();
  const years = screen.getByRole("navigation", { name: "Timeline years" });
  await fireEvent.click([...years.querySelectorAll("button")][1]);
  expect(onselect).toHaveBeenLastCalledWith("2024");
  await view.rerender({ selected: "2024" });
  expect(years.querySelector('[aria-pressed="true"]')?.textContent).toContain("2024");
  expect(screen.getByRole("button", { name: "February 2024 · 400", pressed: false })).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "February 2024 · 400" }));
  expect(onselect).toHaveBeenLastCalledWith("2024-02");
  await view.rerender({ selected: "2024-02" });
  expect(screen.getByRole("button", { name: "February 2024 · 400", pressed: true })).toBeTruthy();
  const day = screen.getByRole("button", { name: "2024-02-29 · 400 photos" });
  expect(screen.queryByRole("button", { name: "2025-01-01 · 500 photos" })).toBeNull();
  await fireEvent.click(day);
  expect(onselect).toHaveBeenCalledWith("2024-02-29");
  await view.rerender({ selected: "2024-02-29" });
  expect(screen.getByRole("button", { name: "2024-02-29 · 400 photos", pressed: true })).toBeTruthy();
  expect(screen.getByRole("navigation", { name: "Timeline month scrubber" })).toBeTruthy();
});

it("shows undated and unavailable scopes honestly", async () => {
  const view = render(PhotoTimeline, { loading: false, error: "", onselect: vi.fn(), onretry: vi.fn(), facet: { dimension: "capture_day", available: true, total: 10, missing: 10, other: 0, values: [] } });
  expect(screen.getByText("These photos have no recorded capture dates.")).toBeTruthy();
  await view.rerender({ loading: false, error: "", onselect: vi.fn(), onretry: vi.fn(), facet: { dimension: "capture_day", available: false, reason: "time_budget_exceeded", values: [] } });
  expect(screen.queryByRole("navigation", { name: "Timeline years" })).toBeNull();
});
it.each([ ["time_budget_exceeded", /Timeline took too long/], ["snapshot_busy", /Timeline is busy/], ["snapshot_capacity", /Timeline is busy/], ["snapshot_too_large", /Timeline exceeds its limits/], ["member_budget_exceeded", /Too many photos/] ])("shows %s with its retry message", async (reason, message) => {
  const retry = vi.fn();
  render(PhotoTimeline, { loading: false, error: "", onselect: vi.fn(), onretry: retry, facet: { dimension: "capture_day", available: false, reason: String(reason), values: [] } });
  expect(screen.getByText(message)).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Retry timeline" })); expect(retry).toHaveBeenCalledTimes(1);
});
