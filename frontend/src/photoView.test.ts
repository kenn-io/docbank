import { expect, it } from "vite-plus/test";
import { photoViewGeometry, toPanView, fromPanView } from "./photoView.js";
it("keeps the chosen image point through resize, density changes and a quarter turn", () => {
  const view = { scale: 2, x: 0.75, y: 0.25, actual: true };
  const first = photoViewGeometry(1200, 800, 600, 400, 1, 0);
  expect(toPanView(view, first)).toEqual({ scale: 2, offsetX: -300, offsetY: 200 });
  const resized = photoViewGeometry(1200, 800, 300, 200, 2, 0);
  expect(toPanView(view, resized)).toEqual({ scale: 2, offsetX: -150, offsetY: 100 });
  const rotated = photoViewGeometry(1200, 800, 400, 600, 2, 90);
  expect(toPanView(view, rotated)).toEqual({ scale: 1, offsetX: -100, offsetY: 150 });
  expect(fromPanView({ scale: 2, offsetX: -300, offsetY: 200 }, first)).toEqual({
    scale: 2,
    x: 0.75,
    y: 0.25,
    actual: false,
  });
});
