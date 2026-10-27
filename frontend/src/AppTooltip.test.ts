import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import IconAction from "./IconAction.svelte";
import { TOOLTIP_DELAY_MS } from "./AppTooltip.svelte";

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

it("shows a tooltip only after the shared delay, and says why a disabled control is off", async () => {
  vi.useFakeTimers();
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  render(IconAction, {
    ariaLabel: "Example action",
    tip: "Example action unavailable",
    disabled: true,
    children: (() => {}) as never,
  });
  const button = screen.getByRole("button", { name: "Example action" });
  expect(button.getAttribute("title")).toBe("");

  await fireEvent.mouseEnter(button.parentElement!);
  vi.advanceTimersByTime(TOOLTIP_DELAY_MS - 1);
  expect(screen.queryByRole("tooltip")).toBeNull();
  vi.advanceTimersByTime(2);
  expect((await screen.findByRole("tooltip")).textContent).toBe("Example action unavailable");
});
