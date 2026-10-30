import { expect, it } from "vite-plus/test";
import { physicalRectangle } from "./redactionGeometry.js";

it("maps reverse drags using the content rectangle at either zoom and scroll position", () => {
  expect(
    physicalRectangle(
      { x: 210, y: 170 },
      { x: 110, y: 120 },
      { left: 10, top: 20, width: 850, height: 1100 },
      85000,
      110000,
    ),
  ).toEqual({ x0: 10000, y0: 10000, x1: 20000, y1: 15000 });
  expect(
    physicalRectangle(
      { x: 65, y: 40 },
      { x: 115, y: 65 },
      { left: 15, top: -10, width: 425, height: 550 },
      85000,
      110000,
    ),
  ).toEqual({ x0: 10000, y0: 10000, x1: 20000, y1: 15000 });
  expect(
    physicalRectangle(
      { x: 0.1, y: 0.1 },
      { x: 10.9, y: 20.9 },
      { left: 0, top: 0, width: 1100, height: 850 },
      110000,
      85000,
    ),
  ).toEqual({ x0: 10, y0: 10, x1: 1090, y1: 2090 });
});
it("refuses clicks, empty content and non-finite coordinates; clamps only to this page", () => {
  const rect = { left: 0, top: 0, width: 850, height: 1100 };
  expect(physicalRectangle({ x: 1, y: 1 }, { x: 1, y: 1 }, rect, 85000, 110000)).toBeUndefined();
  expect(physicalRectangle({ x: 1, y: 1 }, { x: NaN, y: 4 }, rect, 85000, 110000)).toBeUndefined();
  expect(
    physicalRectangle({ x: 1, y: 1 }, { x: 2, y: 3 }, { ...rect, width: 0 }, 85000, 110000),
  ).toBeUndefined();
  expect(physicalRectangle({ x: -20, y: -20 }, { x: 900, y: 1200 }, rect, 85000, 110000)).toEqual({
    x0: 0,
    y0: 0,
    x1: 85000,
    y1: 110000,
  });
});
