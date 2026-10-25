import { afterEach, beforeEach, expect, it } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import PanelResizer from "./PanelResizer.svelte";
import { readPanelWidth } from "./panelWidth.js";

beforeEach(() => localStorage.clear());
afterEach(cleanup);

// The resizer measures the element after it, so give it a panel of a known width.
function handle(size = 400) {
  const view = render(PanelResizer, { name: "test", label: "Resize panel", min: 300, max: 480 });
  const panel = document.createElement("aside");
  panel.getBoundingClientRect = () => ({ width: size }) as DOMRect;
  view.container.querySelector(".panel-resizer")!.after(panel);
  return screen.getByRole("separator", { name: "Resize panel" });
}

it("widens with ArrowLeft, narrows with ArrowRight, and remembers the width", async () => {
  const separator = handle();
  await fireEvent.keyDown(separator, { key: "ArrowLeft" });
  expect(readPanelWidth("test")).toBe(424);

  await fireEvent.keyDown(separator, { key: "ArrowRight" });
  expect(readPanelWidth("test")).toBe(376);
});

it("stays within the minimum and maximum", async () => {
  const separator = handle(310);
  await fireEvent.keyDown(separator, { key: "ArrowRight" });
  expect(readPanelWidth("test")).toBe(300);

  cleanup();
  const wide = handle(470);
  await fireEvent.keyDown(wide, { key: "ArrowLeft" });
  expect(readPanelWidth("test")).toBe(480);
});

it("resets to the default width on double-click", async () => {
  const separator = handle();
  await fireEvent.keyDown(separator, { key: "ArrowLeft" });
  expect(readPanelWidth("test")).toBe(424);

  await fireEvent.dblClick(separator);
  expect(readPanelWidth("test")).toBeUndefined();
});

it("ignores a saved width that is not a number of pixels", () => {
  localStorage.setItem("docbank.panelWidth.test", "wide");
  expect(readPanelWidth("test")).toBeUndefined();
});
