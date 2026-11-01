import { render, fireEvent, screen, cleanup } from "@testing-library/svelte";
import { afterEach, expect, it, vi } from "vite-plus/test";
import RedactionCanvas from "./RedactionCanvas.svelte";

afterEach(cleanup);
it("commits a keyboard rectangle in physical coordinates and cancels Escape locally", async () => {
  const oncommit = vi.fn(),
    ondrag = vi.fn();
  render(RedactionCanvas, {
    page: {
      number: 1,
      frame_sha256: "a".repeat(64),
      width: 85000,
      height: 110000,
      span: { start: 0, end: 18 },
    },
    masks: [],
    gaps: [],
    drawing: true,
    oncommit,
    ondrag,
  });
  const canvas = screen.getByRole("application", { name: "Mask canvas" });
  await fireEvent.keyDown(canvas, { key: "ArrowRight" });
  await fireEvent.keyDown(canvas, { key: "ArrowDown" });
  await fireEvent.keyDown(canvas, { key: "Enter" });
  await fireEvent.keyDown(canvas, { key: "ArrowRight" });
  await fireEvent.keyDown(canvas, { key: "ArrowDown", shiftKey: true });
  await fireEvent.keyDown(canvas, { key: "Enter" });
  expect(oncommit).toHaveBeenCalledWith({
    page: 1,
    frame_sha256: "a".repeat(64),
    x0: 100,
    y0: 100,
    x1: 200,
    y1: 1100,
  });
  await fireEvent.keyDown(canvas, { key: "Enter" });
  const escape = new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true });
  canvas.dispatchEvent(escape);
  expect(escape.defaultPrevented).toBe(true);
  expect(oncommit).toHaveBeenCalledTimes(1);
  expect(ondrag).toHaveBeenLastCalledWith(false);
});
