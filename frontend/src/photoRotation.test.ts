import { expect, it } from "vite-plus/test";
import {
  drawnPhotoFace,
  photoRotationStyle,
  rotatePhotoBox,
  photoFaceCrop,
  photoFaceCropStyle,
} from "./photoRotation.js";

it.each([
  [90, 200, 300],
  [180, 300, 200],
  [270, 200, 300],
])("uses image bounds for a %s degree turn", (rotation, width, height) => {
  expect(photoRotationStyle(rotation, 300, 200)).toContain(`width:${width}px;height:${height}px`);
  expect(photoRotationStyle(rotation, 300, 200)).toContain(`rotate(${rotation}deg)`);
});

const box = { x: 0.1, y: 0.2, width: 0.3, height: 0.4 };
it.each([
  [0, { x: 0.1, y: 0.2, width: 0.3, height: 0.4 }],
  [90, { x: 0.4, y: 0.1, width: 0.4, height: 0.3 }],
  [180, { x: 0.6, y: 0.4, width: 0.3, height: 0.4 }],
  [270, { x: 0.2, y: 0.6, width: 0.4, height: 0.3 }],
])("rotates face corners at %i degrees", (rotation, expected) => {
  const displayed = rotatePhotoBox(box, rotation as number);
  for (const field of ["x", "y", "width", "height"] as const)
    expect(displayed[field]).toBeCloseTo(expected[field]);
  const restored = drawnPhotoFace(
    {
      x: expected.x + expected.width / 2,
      y: expected.y + expected.height / 2,
      width: expected.width,
      height: expected.height,
    },
    rotation as number,
  );
  for (const field of ["x", "y", "width", "height"] as const)
    expect(restored[field]).toBeCloseTo(box[field]);
});

it.each([0, 90, 180, 270])(
  "keeps keyboard face limits inside API bounds at %i degrees",
  (rotation) => {
    for (const center of [0.075, 0.925]) {
      const face = drawnPhotoFace({ x: center, y: center, width: 0.15, height: 0.15 }, rotation);
      for (const value of Object.values(face)) {
        expect(Number.isFinite(value)).toBe(true);
        expect(value).toBeGreaterThanOrEqual(0);
        expect(value).toBeLessThanOrEqual(1);
      }
      expect(face.width).toBeGreaterThan(0);
      expect(face.height).toBeGreaterThan(0);
      expect(face.x + face.width).toBeLessThanOrEqual(1);
      expect(face.y + face.height).toBeLessThanOrEqual(1);
    }
  },
);

it("pads and clamps closeups", () => {
  const crop = photoFaceCrop({ x: 0.98, y: 0.98, width: 0.01, height: 0.01 }, 100, 80);
  expect(crop.side).toBe(24);
  expect(crop.x).toBeGreaterThanOrEqual(0);
  expect(crop.y).toBeGreaterThanOrEqual(0);
  expect(crop.x + crop.side).toBeLessThanOrEqual(crop.width);
  expect(crop.y + crop.side).toBeLessThanOrEqual(crop.height);
  const padded = photoFaceCrop({ x: 0.1, y: 0.2, width: 0.3, height: 0.4 }, 100, 80);
  expect(padded.side).toBeCloseTo(51.2);
});

it.each([
  [0, 350, 280, 172, 112],
  [90, 520, 350, -48, 172],
  [180, 650, 520, -108, -48],
  [270, 280, 650, 112, -108],
])("centers and positions closeups at %i degrees", (rotation, x, y, left, top) => {
  const centered = photoFaceCrop({ x: 0.3, y: 0.3, width: 0.1, height: 0.1 }, 1000, 800, rotation);
  expect(centered.x + centered.side / 2).toBeCloseTo(x!);
  expect(centered.y + centered.side / 2).toBeCloseTo(y!);
  const style = photoFaceCropStyle(
    { x: 0.1, y: 0.2, width: 0.1, height: 0.1 },
    1000,
    800,
    rotation,
    64,
  );
  const image = document.createElement("img");
  image.style.cssText = style;
  expect(parseFloat(image.style.left)).toBeCloseTo(left!);
  expect(parseFloat(image.style.top)).toBeCloseTo(top!);
  expect(style).toContain("width:400px;height:320px");
});
