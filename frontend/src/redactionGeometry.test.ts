import { expect, it } from "vite-plus/test";
import { physicalRectangle } from "./redactionGeometry.js";

const bounds = { left: 0, top: 0, width: 85000, height: 110000 };
it("normalizes reverse drags in physical page coordinates", () => {
  expect(
    physicalRectangle({ x: 20000, y: 15000 }, { x: 10000, y: 10000 }, bounds, 85000, 110000),
  ).toEqual({ x0: 10000, y0: 10000, x1: 20000, y1: 15000 });
});
it("refuses a physical selection with no width or height", () => {
  expect(
    physicalRectangle({ x: 10000, y: 10000 }, { x: 10000, y: 15000 }, bounds, 85000, 110000),
  ).toBeUndefined();
  expect(
    physicalRectangle({ x: 10000, y: 10000 }, { x: 20000, y: 10000 }, bounds, 85000, 110000),
  ).toBeUndefined();
});
