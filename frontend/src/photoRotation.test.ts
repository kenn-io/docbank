import { expect, it } from "vite-plus/test";
import { photoRotationStyle } from "./photoRotation.js";

it.each([
  [90, 200, 300],
  [180, 300, 200],
  [270, 200, 300],
])("uses image bounds for a %s degree turn", (rotation, width, height) => {
  expect(photoRotationStyle(rotation, 300, 200)).toContain(`width:${width}px;height:${height}px`);
  expect(photoRotationStyle(rotation, 300, 200)).toContain(`rotate(${rotation}deg)`);
});
