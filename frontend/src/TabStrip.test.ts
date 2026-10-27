import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import TabStrip from "./TabStrip.svelte";

afterEach(cleanup);

const tabs = [
  { value: "a", label: "Alpha" },
  { value: "b", label: "Beta" },
  { value: "c", label: "Gamma" },
];

it("marks the selected tab and reports a click", async () => {
  const onchange = vi.fn();
  render(TabStrip, { tabs, value: "b", ariaLabel: "Sections", onchange });
  expect(screen.getByRole("tab", { name: "Beta", selected: true })).toBeTruthy();
  await fireEvent.click(screen.getByRole("tab", { name: "Gamma" }));
  expect(onchange).toHaveBeenCalledWith("c");
});

it("moves between tabs with the arrow keys and wraps", async () => {
  const onchange = vi.fn();
  render(TabStrip, { tabs, value: "c", ariaLabel: "Sections", onchange });
  await fireEvent.keyDown(screen.getByRole("tab", { name: "Gamma" }), { key: "ArrowRight" });
  expect(onchange).toHaveBeenLastCalledWith("a");
  await fireEvent.keyDown(screen.getByRole("tab", { name: "Gamma" }), { key: "ArrowLeft" });
  expect(onchange).toHaveBeenLastCalledWith("b");
});

it("keeps one tab stop whichever tab is selected", () => {
  render(TabStrip, { tabs, value: "a", ariaLabel: "Sections", onchange: vi.fn() });
  expect(screen.getAllByRole("tab").map((tab) => tab.getAttribute("tabindex"))).toEqual([
    "0",
    "-1",
    "-1",
  ]);
});
