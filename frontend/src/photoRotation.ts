import type { FaceBox } from "./generated/docbank.js";

export function rotatedDimensions(width: number, height: number, rotation = 0) {
  return rotation === 90 || rotation === 270 ? { width: height, height: width } : { width, height };
}

export function photoRotationTransform(rotation: number) {
  return `translate(-50%,-50%) rotate(${rotation}deg)`;
}

export function photoRotationStyle(rotation: number, width: number, height: number) {
  const size = rotatedDimensions(width, height, rotation);
  return `width:${size.width}px;height:${size.height}px;transform:${photoRotationTransform(rotation)}`;
}

export function observePhotoSize(
  element: HTMLElement,
  resized: (width: number, height: number) => void,
) {
  const update = () => resized(element.clientWidth, element.clientHeight);
  const observer = new ResizeObserver(update);
  observer.observe(element);
  update();
  return () => observer.disconnect();
}

export type NormalizedBox = FaceBox;
export function rotatePhotoBox(box: NormalizedBox, rotation: number): NormalizedBox {
  switch ((rotation + 360) % 360) {
    case 90:
      return { x: 1 - box.y - box.height, y: box.x, width: box.height, height: box.width };
    case 180:
      return { ...box, x: 1 - box.x - box.width, y: 1 - box.y - box.height };
    case 270:
      return { x: box.y, y: 1 - box.x - box.width, width: box.height, height: box.width };
    default:
      return box;
  }
}

// Region callbacks use a center; persisted face boxes use the top left corner.
export function drawnPhotoFace(region: NormalizedBox, rotation: number): NormalizedBox {
  const width = Math.min(region.width, 1),
    height = Math.min(region.height, 1);
  return rotatePhotoBox(
    {
      x: Math.max(0, Math.min(1 - width, region.x - width / 2)),
      y: Math.max(0, Math.min(1 - height, region.y - height / 2)),
      width,
      height,
    },
    360 - rotation,
  );
}

// Square closeups use displayed coordinates after rotation.
export function photoFaceCrop(box: NormalizedBox, width: number, height: number, rotation = 0) {
  const displayed = rotatePhotoBox(box, rotation);
  const size = rotatedDimensions(width, height, rotation);
  const side = Math.min(
    Math.min(size.width, size.height),
    Math.max(24, 1.6 * Math.max(displayed.width * size.width, displayed.height * size.height)),
  );
  const x = Math.max(
    0,
    Math.min(size.width - side, (displayed.x + displayed.width / 2) * size.width - side / 2),
  );
  const y = Math.max(
    0,
    Math.min(size.height - side, (displayed.y + displayed.height / 2) * size.height - side / 2),
  );
  return { x, y, side, ...size };
}
