import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/svelte";
import PhotoTimeline from "./PhotoTimeline.svelte";
import { photoTimelineCounts } from "./photo-test-fixtures.js";

// The compact selector measures and scrolls its option list.
beforeEach(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

const facet = (missing = 4) =>
  photoTimelineCounts(
    [
      ["2025-06-02", 10],
      ["2025-06-01", 2],
      ["2025-01-05", 3],
      ["2024-02-29", 7],
    ],
    { missing },
  ).facets[0];

const props = (overrides: Record<string, unknown> = {}) => ({
  facet: facet(),
  loading: false,
  error: "",
  onselect: vi.fn(),
  onretry: vi.fn(),
  ...overrides,
});

it("lists years with full counts and opens the chosen and current years", () => {
  render(PhotoTimeline, props({ chosen: "2024", current: "2025-01" }));
  const rail = screen.getByRole("navigation", { name: "Timeline" });
  expect(within(rail).getByRole("button", { name: "2025, 15 photos" })).toBeTruthy();
  expect(within(rail).getByRole("button", { name: "2024, 7 photos" })).toBeTruthy();
  // Both years are open, so each lists its months.
  expect(within(rail).getByRole("button", { name: "Jun 2025, 12 photos" })).toBeTruthy();
  expect(within(rail).getByRole("button", { name: "Feb 2024, 7 photos" })).toBeTruthy();
  // Only the top photo's month is marked.
  const marked = rail.querySelectorAll("[aria-current]");
  expect([...marked].map((row) => row.getAttribute("aria-label"))).toEqual(["Jan 2025, 3 photos"]);
});

it("resolves a year click to its newest counted month", async () => {
  const onselect = vi.fn();
  render(PhotoTimeline, props({ onselect }));
  await fireEvent.click(screen.getByRole("button", { name: "2025, 15 photos" }));
  expect(onselect).toHaveBeenCalledWith("2025-06");
  // Closed years list no months.
  expect(screen.queryByRole("button", { name: /^Jun 2025/ })).toBeNull();
});

it("shows the undated foot for a scope with only undated photos", () => {
  render(PhotoTimeline, props({ facet: photoTimelineCounts([], { missing: 5 }).facets[0] }));
  expect(screen.queryByRole("button")).toBeNull();
  expect(screen.getByText("5 undated")).toBeTruthy();
});

it("keeps the counts beside an error and retries", async () => {
  const onretry = vi.fn();
  render(PhotoTimeline, props({ error: "Timeline is busy. Retry in a moment.", onretry }));
  expect(screen.getByRole("alert").textContent).toContain("Timeline is busy. Retry in a moment.");
  expect(screen.getByRole("button", { name: "2025, 15 photos" })).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Retry timeline" }));
  expect(onretry).toHaveBeenCalledOnce();
});

it("disables every entry", () => {
  render(PhotoTimeline, props({ disabled: true, current: "2025-06" }));
  const buttons = screen.getAllByRole("button");
  expect(buttons.length).toBeGreaterThan(1);
  expect(buttons.every((button) => (button as HTMLButtonElement).disabled)).toBe(true);
});

it("offers years, months, and the undated count with the current month or a prompt at the top", async () => {
  const view = render(PhotoTimeline, props({ compact: true, current: "2025-01" }));
  expect(screen.getByRole("combobox", { name: "Jump to date: Jan 2025" })).toBeTruthy();
  await view.rerender(props({ compact: true, current: undefined }));
  const select = screen.getByRole("combobox", { name: "Jump to date: Jump to date" });
  await fireEvent.click(select);
  const options = screen.getAllByRole("option");
  expect(options.map((option) => option.textContent?.trim())).toEqual([
    "Jump to date",
    "2025 · 15 photos",
    "Jun 2025 · 12 photos",
    "Jan 2025 · 3 photos",
    "2024 · 7 photos",
    "Feb 2024 · 7 photos",
    "4 undated",
  ]);
  expect(options[0]!.hasAttribute("disabled")).toBe(true);
  expect(options.at(-1)!.hasAttribute("disabled")).toBe(true);
});
