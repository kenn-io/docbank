import { actualPixelSize, type PanZoomView } from "@kenn-io/kit-ui";
import { rotatedDimensions } from "./photoRotation.js";

export interface PhotoView {
  scale: number;
  x: number;
  y: number;
  actual: boolean;
}
export const fittedPhotoView: PhotoView = { scale: 1, x: 0.5, y: 0.5, actual: false };
export function toggleActualPhotoView(view: PhotoView): PhotoView {
  return view.actual ? { ...fittedPhotoView } : { ...view, actual: true };
}
export function photoViewGeometry(
  width: number,
  height: number,
  boxWidth: number,
  boxHeight: number,
  dpr: number,
  rotation: number,
) {
  const size = rotatedDimensions(width, height, rotation);
  const actual = actualPixelSize(
    { width, height },
    { width: boxWidth, height: boxHeight },
    dpr,
    rotation,
  );
  return {
    width: actual.fittedWidth,
    height: (actual.fittedWidth * size.height) / size.width,
    actualScale: actual.scale,
  };
}
export function toPanView(
  view: PhotoView,
  geometry: ReturnType<typeof photoViewGeometry>,
): PanZoomView {
  const scale = view.actual ? geometry.actualScale : view.scale;
  return {
    scale,
    offsetX: (0.5 - view.x) * geometry.width * scale,
    offsetY: (0.5 - view.y) * geometry.height * scale,
  };
}
export function fromPanView(
  view: PanZoomView,
  geometry: ReturnType<typeof photoViewGeometry>,
): PhotoView {
  return {
    scale: view.scale,
    x: 0.5 - view.offsetX / (geometry.width * view.scale),
    y: 0.5 - view.offsetY / (geometry.height * view.scale),
    actual: false,
  };
}
