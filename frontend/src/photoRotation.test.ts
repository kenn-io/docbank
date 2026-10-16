import { expect, it } from "vite-plus/test";
import {
  photoRotationStyle,
  drawnPhotoFace,
  rotatePhotoBox,
  photoFaceCrop,
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
])("rotates face corners and inverts a drawn region at %i degrees", (rotation, expected) => {
  const displayed = rotatePhotoBox(box, rotation as number);
  for (const field of ["x", "y", "width", "height"] as const)
    expect(displayed[field]).toBeCloseTo(expected[field]);
  const stored = drawnPhotoFace(
    { ...displayed, x: displayed.x + displayed.width / 2, y: displayed.y + displayed.height / 2 },
    rotation as number,
  );
  for (const field of ["x", "y", "width", "height"] as const)
    expect(stored[field]).toBeCloseTo(box[field]);
});

it.each([
  [0, 350, 280],
  [90, 520, 350],
  [180, 650, 520],
  [270, 280, 650],
])("pads, clamps and centers closeups at %i degrees", (rotation, x, y) => {
  const crop = photoFaceCrop({ x: 0.98, y: 0.98, width: 0.01, height: 0.01 }, 100, 80, rotation);
  expect(crop.side).toBe(24);
  expect(crop.x).toBeGreaterThanOrEqual(0);
  expect(crop.y).toBeGreaterThanOrEqual(0);
  expect(crop.x + crop.side).toBeLessThanOrEqual(crop.width);
  expect(crop.y + crop.side).toBeLessThanOrEqual(crop.height);
  const padded = photoFaceCrop({ x: 0.1, y: 0.2, width: 0.3, height: 0.4 }, 100, 80, rotation);
  expect(padded.side).toBeCloseTo(51.2);
  const centered = photoFaceCrop({ x: 0.3, y: 0.3, width: 0.1, height: 0.1 }, 1000, 800, rotation);
  expect(centered.x + centered.side / 2).toBeCloseTo(x!);
  expect(centered.y + centered.side / 2).toBeCloseTo(y!);
});
